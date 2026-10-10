package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

// ToolReviewSettingsProvider returns one atomic configuration/credential snapshot.
// It is deliberately separate from the request-side privacy policy.
type ToolReviewSettingsProvider interface {
	LoadToolReviewSettings(context.Context) (contract.ToolReviewSettings, []byte, error)
}

var (
	errToolReviewStopped           = errors.New("tool review stopped delivery")
	errToolReviewComplete          = errors.New("tool review received protocol terminal")
	errToolReviewCollectionTimeout = errors.New("tool review collection timed out")
)

const toolReviewCollectionTimeout = 120 * time.Second

func supportsToolReview(protocol contract.ProtocolID) bool {
	switch protocol {
	case contract.ProtocolOpenAIChat, contract.ProtocolOpenAIResponses, contract.ProtocolAnthropicMessages, contract.ProtocolGoogleGenerateContent:
		return true
	default:
		return false
	}
}

// prepareToolReview is outside routing attempts and inside the client record
// boundary. It sees converted/restored client bytes, including builtin emitters,
// but never reviews the gateway's own internal model calls or hosted execution.
func (handler *Handler) prepareToolReview(writer http.ResponseWriter, request *http.Request, classified Request) (*toolReviewWriter, *http.Request, error) {
	if handler.toolReviewSettings == nil || !supportsToolReview(classified.Protocol) || builtinInternalFrom(request.Context()) != nil {
		return nil, request, nil
	}
	settings, key, err := handler.toolReviewSettings.LoadToolReviewSettings(request.Context())
	defer clear(key)
	credentialUnavailable := err != nil && settings.Enabled &&
		(settings.Mode == contract.ToolReviewModeMark || settings.Mode == contract.ToolReviewModeReject)
	if err != nil && !credentialUnavailable {
		return nil, request, errors.New("tool review configuration unavailable")
	}
	if !settings.Enabled {
		return nil, request, nil
	}
	ctx, cancel := context.WithCancelCause(request.Context())
	output := &toolReviewWriter{
		ResponseWriter: writer, ctx: ctx, cancel: cancel,
		protocol: classified.Protocol, streaming: classified.Streaming,
		snapshot:  toolreview.Snapshot{Settings: settings, APIKey: string(key)},
		collector: toolreview.NewCollector(classified.Protocol, classified.Streaming),
		startedAt: time.Now().UTC(),
	}
	if credentialUnavailable {
		output.inputError = "credential_unavailable"
	}
	if session := recordSessionFromContext(request.Context()); session != nil {
		pending := output.summary("pending", "")
		pending.CompletedAt = nil
		session.toolReview = &pending
		session.persistPending(request.Context(), handler.requestRecords, handler.recordLogger)
	}
	// Parsing this bounded copy never rewrites what the upstream receives.
	// Convo's usual 4096-byte preview must not become the review's source text.
	limit := handler.maxRequestBodyBytes
	if limit == 0 {
		limit = 128 << 20
	}
	body, readErr := bufferRequestBody(request, limit)
	if readErr != nil {
		output.inputError = "user_request_unavailable"
	} else {
		text, available, count, inspectErr := extractToolReviewInput(classified.Protocol, body)
		if inspectErr != nil {
			output.inputError = "user_request_unavailable"
		} else if len(text) > contract.MaxToolReviewInputBytes {
			output.inputError = "input_limit"
		} else {
			output.input.UserRequest = text
			output.input.UserRequestAvailable = available
		}
		if err := output.collector.SetExpectedCandidates(count); err != nil {
			output.inputError = reviewErrorCode(err)
		}
	}
	request.Header.Del("Accept-Encoding")
	if turn := responsesWSTurnFromContext(request.Context()); turn != nil {
		turn.session.mu.Lock()
		turn.cancelReview = func() { cancel(context.Canceled) }
		if turn.cancelled {
			cancel(context.Canceled)
		}
		turn.session.mu.Unlock()
	}
	return output, request.WithContext(ctx), nil
}

type toolReviewWriter struct {
	http.ResponseWriter
	ctx               context.Context
	cancel            context.CancelCauseFunc
	protocol          contract.ProtocolID
	streaming         bool
	snapshot          toolreview.Snapshot
	input             toolreview.Input
	inputError        string
	collector         *toolreview.Collector
	startedAt         time.Time
	status            int
	committed         bool
	passthrough       bool
	held              bool
	terminal          bool
	fault             string
	carry             []byte
	buffer            bytes.Buffer
	mode              responseRestoreMode
	jsonKind          jsonRestoreStreamKind
	jsonPrefix        []byte
	jsonNeedComma     bool
	jsonValues        int
	timer             *time.Timer
	collectionStarted time.Time
	readyAt           time.Time
	async             func()
	deliveryBytes     int
}

func (writer *toolReviewWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }
func (writer *toolReviewWriter) rejecting() bool {
	return writer.snapshot.Settings.Mode == contract.ToolReviewModeReject
}

func (writer *toolReviewWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	if status >= 400 {
		writer.passthrough = true
		writer.commit()
		return
	}
	if encoding := strings.TrimSpace(writer.Header().Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		writer.fault = "encoded_response"
	}
	if writer.streaming {
		writer.mode = streamingRestoreMode(writer.Header().Get("Content-Type"))
		if writer.mode == restoreModeText {
			writer.fault = "unsupported_response_format"
		}
	}
	if !writer.rejecting() {
		writer.commit()
	}
}

func (writer *toolReviewWriter) commit() {
	if writer.committed {
		return
	}
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	if writer.rejecting() {
		writer.Header().Del("Content-Length")
	}
	writer.ResponseWriter.WriteHeader(writer.status)
	writer.committed = true
}

func (writer *toolReviewWriter) Flush() { _ = writer.FlushError() }

func (writer *toolReviewWriter) FlushError() error {
	// An upstream header flush is not permission to release an incomplete
	// tool. Ordinary content is flushed immediately by emit once inspected.
	if writer.passthrough || (!writer.rejecting() && writer.committed) {
		err := http.NewResponseController(writer.ResponseWriter).Flush()
		if err != nil && !errors.Is(err, http.ErrNotSupported) {
			writer.fault = "client_delivery_failed"
			return err
		}
	}
	return nil
}

func (writer *toolReviewWriter) Write(chunk []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	if writer.passthrough {
		return writer.ResponseWriter.Write(chunk)
	}
	if !writer.rejecting() {
		n, err := writer.ResponseWriter.Write(chunk)
		if err == nil && n != len(chunk) {
			err = io.ErrShortWrite
		}
		if err != nil {
			writer.fault = "client_delivery_failed"
			return n, err
		}
	}
	// Mark mode keeps relaying until upstream EOF; nothing after a legal
	// terminal is collected. Reject mode stops reading at the terminal.
	if writer.terminal {
		return len(chunk), nil
	}
	if !writer.collectionStarted.IsZero() && time.Since(writer.collectionStarted) > toolReviewCollectionTimeout {
		writer.fault = "collection_timeout"
	}
	if writer.fault != "" {
		if writer.rejecting() {
			return 0, errToolReviewStopped
		}
		return len(chunk), nil
	}
	if len(chunk) > contract.MaxToolReviewBufferBytes-len(writer.carry)-writer.buffer.Len()-len(writer.jsonPrefix) {
		return writer.failWrite("buffer_limit", len(chunk))
	}
	writer.carry = append(writer.carry, chunk...)
	var err error
	if writer.streaming {
		if writer.mode == restoreModeSSE {
			err = writer.processSSE()
		} else {
			err = writer.processJSON()
		}
	}
	if errors.Is(err, errToolReviewComplete) {
		writer.readyAt = time.Now()
		writer.terminal = true
		writer.carry = nil
		if !writer.rejecting() {
			return len(chunk), writer.FlushError()
		}
		return len(chunk), err
	}
	if err != nil {
		return writer.failWrite(reviewErrorCode(err), len(chunk))
	}
	return len(chunk), nil
}

func reviewErrorCode(err error) string {
	var failure *toolreview.Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return "invalid_response"
}

func (writer *toolReviewWriter) failWrite(code string, n int) (int, error) {
	writer.fault = code
	writer.carry = nil
	writer.jsonPrefix = nil
	writer.buffer.Reset()
	if writer.rejecting() {
		return 0, errToolReviewStopped
	}
	return n, nil
}

func (writer *toolReviewWriter) processSSE() error {
	for {
		end := nextSSEFrameEnd(writer.carry)
		if end == 0 {
			return nil
		}
		frame := writer.carry[:end]
		payload := parseSSEData(frame)
		if err := writer.frame(frame, payload); err != nil {
			return err
		}
		writer.carry = writer.carry[end:]
		if writer.protocolTerminal(payload) {
			return errToolReviewComplete
		}
	}
}

func (writer *toolReviewWriter) protocolTerminal(payload []byte) bool {
	if !writer.collector.Complete() {
		return false
	}
	if writer.protocol == contract.ProtocolOpenAIChat {
		return bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]"))
	}
	return true
}

// processJSON preserves Gemini's JSON-array/JSON-sequence envelope and emits
// each complete value before the first tool, never a partially parsed value.
func (writer *toolReviewWriter) processJSON() error {
	for {
		start := firstNonSpace(writer.carry)
		if start < 0 {
			return nil
		}
		if writer.jsonKind == jsonRestoreDone {
			return errors.New("trailing JSON")
		}
		if writer.jsonKind == jsonRestoreUnknown {
			switch writer.carry[start] {
			case '[':
				writer.jsonKind = jsonRestoreArray
				writer.jsonPrefix = append(writer.jsonPrefix, writer.carry[:start+1]...)
				writer.carry = writer.carry[start+1:]
				continue
			case '{':
				writer.jsonKind = jsonRestoreSequence
			default:
				return errors.New("invalid JSON stream")
			}
		}
		if writer.jsonKind == jsonRestoreArray {
			if writer.carry[start] == ']' {
				if !writer.jsonNeedComma && writer.jsonValues > 0 {
					return errors.New("trailing comma")
				}
				frame := append(writer.jsonPrefix, writer.carry[:start+1]...)
				writer.jsonPrefix = nil
				if err := writer.frame(frame, nil); err != nil {
					return err
				}
				writer.carry = writer.carry[start+1:]
				writer.jsonKind = jsonRestoreDone
				if writer.collector.Complete() {
					return errToolReviewComplete
				}
				continue
			}
			if writer.jsonNeedComma {
				if writer.carry[start] != ',' {
					return errors.New("missing comma")
				}
				writer.jsonPrefix = append(writer.jsonPrefix, writer.carry[:start+1]...)
				writer.carry = writer.carry[start+1:]
				writer.jsonNeedComma = false
				continue
			}
		}
		end, complete, invalid := completeJSONValueEnd(writer.carry, start)
		if invalid {
			return errors.New("invalid JSON value")
		}
		if !complete {
			return nil
		}
		frame := append(writer.jsonPrefix, writer.carry[:end]...)
		writer.jsonPrefix = nil
		if err := writer.frame(frame, writer.carry[start:end]); err != nil {
			return err
		}
		writer.carry = writer.carry[end:]
		writer.jsonValues++
		writer.jsonNeedComma = writer.jsonKind == jsonRestoreArray
		if writer.jsonKind == jsonRestoreSequence && writer.collector.Complete() {
			return errToolReviewComplete
		}
	}
}

func (writer *toolReviewWriter) frame(raw, payload []byte) error {
	if len(payload) > 0 {
		hasTool, err := writer.collector.Observe(payload)
		if err != nil {
			return err
		}
		if hasTool && !writer.held {
			writer.held = true
			writer.collectionStarted = time.Now()
			if writer.rejecting() {
				writer.timer = time.AfterFunc(toolReviewCollectionTimeout, func() { writer.cancel(errToolReviewCollectionTimeout) })
			}
		}
		if writer.collector.SawTool() && writer.inputError != "" {
			return &toolreview.Error{Code: writer.inputError}
		}
	}
	if !writer.rejecting() {
		return nil
	}
	if writer.held {
		if len(raw) > contract.MaxToolReviewBufferBytes-writer.buffer.Len() {
			return &toolreview.Error{Code: "buffer_limit"}
		}
		_, _ = writer.buffer.Write(raw)
		return nil
	}
	return writer.emit(raw)
}

func (writer *toolReviewWriter) emit(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	writer.commit()
	n, err := writer.ResponseWriter.Write(raw)
	writer.deliveryBytes = n
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	if writer.streaming {
		if err := http.NewResponseController(writer.ResponseWriter).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
	}
	return nil
}

func (writer *toolReviewWriter) summary(status, code string) contract.ToolReviewSummary {
	now := time.Now().UTC()
	return contract.ToolReviewSummary{Status: status, Mode: writer.snapshot.Settings.Mode,
		Model: writer.snapshot.Settings.Model, ConfigRevision: writer.snapshot.Settings.Revision,
		StartedAt: writer.startedAt, CompletedAt: &now, LatencyMS: int(now.Sub(writer.startedAt).Milliseconds()),
		Calls: []contract.ToolReviewCall{}, ErrorCode: code}
}

// finish runs before the final request record is saved. A mark-mode job is
// returned and started only after that save, so it cannot overwrite an absent
// record or race the foreground recordSession's mutable fields.
func (handler *Handler) finishToolReview(writer *toolReviewWriter, session *recordSession) {
	if writer == nil {
		return
	}
	if writer.timer != nil {
		writer.timer.Stop()
	}
	if !writer.collectionStarted.IsZero() && time.Since(writer.collectionStarted) > toolReviewCollectionTimeout {
		writer.fault = "collection_timeout"
	}
	defer func() { writer.collector = nil; writer.carry = nil; writer.jsonPrefix = nil; writer.buffer.Reset() }()
	if writer.passthrough || writer.status == 0 {
		result := writer.summary("not_applicable", "")
		session.toolReview = &result
		return
	}
	cause := context.Cause(writer.ctx)
	if cause != nil && writer.rejecting() && !writer.held && writer.streaming && writer.collector.Complete() && writer.fault == "" {
		// With no held tools, HTTP may already have delivered its terminal.
		// A later disconnect cannot undo that success. A WS terminal retained
		// by its adapter still has to pass the actual publication check.
		if err := writer.confirmDelivery(); err == nil {
			cause = nil
		}
	}
	if errors.Is(cause, errToolReviewCollectionTimeout) {
		writer.fault = "collection_timeout"
	} else if cause != nil && writer.rejecting() {
		result := writer.summary("cancelled", "cancelled")
		session.toolReview = &result
		// A generated response buffered by the gate was not delivered.
		session.status = contract.RequestStatusCancelled
		return
	}
	if !writer.streaming && !session.toolReviewEOF {
		writer.fault = "incomplete_response"
	}
	if writer.fault == "" {
		if !writer.streaming {
			_, err := writer.collector.Observe(writer.carry)
			if err != nil {
				writer.fault = reviewErrorCode(err)
			}
			if writer.rejecting() {
				_, _ = writer.buffer.Write(writer.carry)
			}
			writer.carry = nil
		} else if len(bytes.TrimSpace(writer.carry)) != 0 || len(writer.jsonPrefix) != 0 || writer.jsonKind == jsonRestoreArray {
			writer.fault = "incomplete_response"
		}
	}
	if writer.fault == "" && !writer.collector.Complete() && session.toolReviewEOF && cause == nil {
		if err := writer.collector.Finish(); err != nil {
			writer.fault = reviewErrorCode(err)
		}
	}
	if writer.fault == "" {
		calls, err := writer.collector.Calls()
		if err != nil {
			writer.fault = reviewErrorCode(err)
		} else {
			writer.input.Calls = calls
		}
		if len(writer.input.Calls) > 0 && writer.inputError != "" {
			writer.fault = writer.inputError
		}
	}
	if !writer.rejecting() && writer.streaming {
		if err := writer.confirmDelivery(); err != nil {
			writer.fault = "client_delivery_failed"
			writer.deliveryFailed(session)
		}
	}
	if cause != nil && !writer.rejecting() && !writer.collector.Complete() {
		result := writer.summary("cancelled", "cancelled")
		session.toolReview = &result
		return
	}
	if writer.fault != "" {
		result := writer.summary("failed", writer.fault)
		session.toolReview = &result
		if writer.rejecting() {
			writer.refuse(session, false)
		}
		return
	}
	if len(writer.input.Calls) == 0 {
		result := writer.summary("not_applicable", "")
		session.toolReview = &result
		if writer.rejecting() {
			if err := writer.deliverReviewed(); err != nil {
				writer.deliveryFailed(session)
			}
		}
		return
	}
	if writer.readyAt.IsZero() {
		writer.readyAt = time.Now()
	}
	deadline := writer.readyAt.Add(time.Duration(writer.snapshot.Settings.TimeoutMS) * time.Millisecond)
	if !writer.rejecting() {
		// Admission covers the entire detached job, including metadata I/O.
		// Overflow is saved with the foreground record, never another goroutine.
		select {
		case handler.toolReviewSlots <- struct{}{}:
		default:
			result := writer.summary("failed", "queue_full")
			session.toolReview = &result
			return
		}
		pending := writer.summary("pending", "")
		pending.CompletedAt = nil
		session.toolReview = &pending
		snapshot, input, id := writer.snapshot, writer.input, session.id
		ctx, stop := context.WithDeadline(context.WithoutCancel(writer.ctx), deadline)
		writer.async = func() {
			defer stop()
			defer func() { <-handler.toolReviewSlots }()
			handler.reviewDelivered(ctx, snapshot, input, id)
		}
		return
	}
	ctx, stop := context.WithDeadline(writer.ctx, deadline)
	defer stop()
	result := handler.toolReviewer.Review(ctx, writer.snapshot, writer.input)
	session.toolReview = &result
	if writer.ctx.Err() != nil {
		session.status = contract.RequestStatusCancelled
		result.Status, result.ErrorCode = "cancelled", "cancelled"
		for i := range result.Calls {
			result.Calls[i].Action = "withheld"
		}
		return
	}
	if result.Status == "passed" {
		if err := writer.deliverReviewed(); err != nil {
			writer.deliveryFailed(session)
		}
	} else {
		writer.refuse(session, result.Status == "blocked")
	}
}

func (handler *Handler) reviewDelivered(ctx context.Context, snapshot toolreview.Snapshot, input toolreview.Input, id contract.RequestID) {
	result := handler.toolReviewer.Review(ctx, snapshot, input)
	if store, ok := handler.requestRecords.(storage.ToolReviewRecordStore); ok {
		persistCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := store.UpdateRequestToolReview(persistCtx, id, result); err != nil {
			logRequestRecordFailure(handler.recordLogger, "tool_review_update", errors.New("review metadata could not be saved"))
		}
	}
}

func (writer *toolReviewWriter) deliverReviewed() error {
	if err := writer.emit(writer.buffer.Bytes()); err != nil {
		return err
	}
	return writer.confirmDelivery()
}

func (writer *toolReviewWriter) confirmDelivery() error {
	// WebSocket adapters may retain a success terminal for admission ordering.
	// Confirm its publication too before declaring the reviewed batch delivered.
	for downstream := writer.ResponseWriter; downstream != nil; {
		if delivery, ok := downstream.(interface{ ConfirmToolReviewDelivery() error }); ok {
			return delivery.ConfirmToolReviewDelivery()
		}
		wrapper, ok := downstream.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		downstream = wrapper.Unwrap()
	}
	return nil
}

func (writer *toolReviewWriter) deliveryFailed(session *recordSession) {
	// Passing a review is not proof of delivery or execution. A partial write
	// cannot identify which complete tool events reached the client.
	if session.toolReview != nil {
		for i := range session.toolReview.Calls {
			session.toolReview.Calls[i].Action = "withheld"
			if writer.deliveryBytes > 0 {
				session.toolReview.Calls[i].Action = "pending"
			}
		}
	}
	session.status = contract.RequestStatusFailed
	if writer.ctx.Err() != nil {
		session.status = contract.RequestStatusCancelled
		if session.toolReview != nil {
			session.toolReview.Status, session.toolReview.ErrorCode = "cancelled", "cancelled"
		}
	}
	summary := errorSummaryFromInference("client_delivery_failed", "reviewed response could not be delivered", false)
	session.errorSummary = &summary
}

func (writer *toolReviewWriter) refuse(session *recordSession, blocked bool) {
	code, message, status := "tool_review_unavailable", "tool call review could not be completed", http.StatusServiceUnavailable
	if blocked {
		code, message, status = "tool_review_blocked", "tool call delivery was blocked by review policy", http.StatusForbidden
	}
	summary := errorSummaryFromInference(code, message, false)
	session.errorSummary = &summary
	session.status = contract.RequestStatusFailed
	if blocked {
		session.status = contract.RequestStatusBlocked
	}
	if writer.ctx.Err() != nil && !errors.Is(context.Cause(writer.ctx), errToolReviewCollectionTimeout) {
		return
	}
	writer.Header().Del("Content-Length")
	writer.Header().Del("Content-Encoding")
	if !writer.committed && writer.protocol != contract.ProtocolGoogleGenerateContent {
		writeInferenceError(writer.ResponseWriter, status, code, message, false, nil)
		writer.committed = true
		return
	}
	problem := map[string]any{"code": code, "message": message, "retryable": false}
	event := map[string]any{"error": problem}
	switch writer.protocol {
	case contract.ProtocolOpenAIResponses:
		event["type"], event["code"], event["message"] = "error", code, message
	case contract.ProtocolAnthropicMessages:
		event["type"] = "error"
		problem["type"] = anthropicErrorType(status)
	case contract.ProtocolGoogleGenerateContent:
		problem["code"], problem["status"] = status, googleErrorStatus(status)
		problem["details"] = []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": code, "domain": "gateway"}}
	}
	if !writer.committed {
		event["request_id"] = session.id
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.ResponseWriter.WriteHeader(status)
		writer.committed = true
		_ = json.NewEncoder(writer.ResponseWriter).Encode(event)
		return
	}
	body, _ := json.Marshal(event)
	if writer.mode == restoreModeSSE {
		prefix := "data: "
		if writer.protocol == contract.ProtocolAnthropicMessages || writer.protocol == contract.ProtocolOpenAIResponses {
			prefix = "event: error\ndata: "
		}
		_, _ = io.WriteString(writer.ResponseWriter, prefix+string(body)+"\n\n")
	} else if writer.jsonKind == jsonRestoreArray || writer.jsonKind == jsonRestoreDone {
		// The held suffix includes its leading comma and closing bracket.
		_, _ = io.WriteString(writer.ResponseWriter, ","+string(body)+"]")
	} else {
		_, _ = writer.ResponseWriter.Write(body)
	}
	_ = http.NewResponseController(writer.ResponseWriter).Flush()
}
