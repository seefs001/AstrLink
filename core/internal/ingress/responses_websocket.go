package ingress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/transport"
	"github.com/gorilla/websocket"
)

type responsesWSTurnKey struct{}
type responsesWSTurn struct {
	session  *responsesWSSession
	eventID  string
	streamID string
	controls chan []byte
	// Guarded by session.mu; cancellation also reaches post-generation review.
	cancelReview context.CancelFunc
	cancelled    bool
	ctx          context.Context
	// routingModel is this turn's model after redirects; forward pins it on
	// the socket together with the client model.
	routingModel string
}

func responsesWSTurnFromContext(ctx context.Context) *responsesWSTurn {
	turn, _ := ctx.Value(responsesWSTurnKey{}).(*responsesWSTurn)
	return turn
}
func isResponsesWebSocket(request *http.Request) bool {
	return request.Method == http.MethodGet && request.URL.Path == "/v1/responses" && websocket.IsWebSocketUpgrade(request)
}

type responsesWSClient interface {
	SetWriteDeadline(time.Time) error
	WriteMessage(int, []byte) error
}

type responsesWSSession struct {
	client   responsesWSClient
	upstream transport.ResponsesSocket
	// mu serializes admission and client writes, including terminal publication.
	mu            sync.Mutex
	active        *responsesWSTurn
	serviceID     contract.ServiceID
	model         string
	upstreamModel string
	// routingModel is the redirect result pinned when the socket bound.
	routingModel string
}

func (handler *Handler) serveResponsesWebSocket(writer http.ResponseWriter, request *http.Request) {
	// The normal local boundary has already checked Origin, Host and credentials.
	upgrader := websocket.Upgrader{HandshakeTimeout: 10 * time.Second}
	client, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(request.Context())
	session := &responsesWSSession{client: client}
	limit := handler.maxRequestBodyBytes
	if limit == 0 {
		limit = 128 << 20
	}
	client.SetReadLimit(limit)
	var workers sync.WaitGroup
	defer func() { cancel(); session.upstream.Close(); _ = client.Close(); workers.Wait() }()
	for {
		kind, data, err := client.ReadMessage()
		if err != nil {
			return
		}
		var envelope struct {
			Type       string `json:"type"`
			EventID    string `json:"event_id"`
			StreamID   string `json:"stream_id"`
			ResponseID string `json:"response_id"`
		}
		if kind != websocket.TextMessage || json.Unmarshal(data, &envelope) != nil {
			session.sendError("", http.StatusBadRequest, "invalid_request", "expected a JSON text event")
			continue
		}
		session.mu.Lock()
		if envelope.Type == "response.cancel" && session.active != nil {
			session.active.cancelled = true
			if session.active.cancelReview != nil {
				session.active.cancelReview()
			}
			// Only forward the cancellation envelope, never arbitrary body fields.
			select {
			case session.active.controls <- responsesWSCancel(envelope.StreamID, envelope.ResponseID):
			default:
			}
			session.mu.Unlock()
			continue
		}
		if envelope.Type != "response.create" {
			session.mu.Unlock()
			session.sendError(envelope.EventID, http.StatusBadRequest, "invalid_request", "unsupported Responses WebSocket event")
			continue
		}
		if session.active != nil {
			session.mu.Unlock()
			session.sendError(envelope.EventID, http.StatusConflict, "response_in_progress", "another response.create is already in progress")
			continue
		}
		turn := &responsesWSTurn{session: session, eventID: envelope.EventID, streamID: envelope.StreamID, controls: make(chan []byte, 1), ctx: ctx}
		session.active = turn
		session.mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			output := &responsesWSWriter{turn: turn, header: make(http.Header)}
			defer output.finish()
			body, err := normalizeResponsesWSCreate(data)
			if err != nil {
				writeInferenceError(output, 400, "invalid_request", err.Error(), false, nil)
				return
			}
			call := request.Clone(context.WithValue(ctx, responsesWSTurnKey{}, turn))
			call.Method = http.MethodPost
			call.Body = io.NopCloser(bytes.NewReader(body))
			call.ContentLength = int64(len(body))
			call.Header.Set("Content-Type", "application/json")
			for name := range call.Header {
				if strings.HasPrefix(strings.ToLower(name), "sec-websocket-") {
					call.Header.Del(name)
				}
			}
			for _, name := range []string{"Connection", "Upgrade", "Content-Encoding", "Content-Length"} {
				call.Header.Del(name)
			}
			// Re-authenticate each turn, then use the usual routing/privacy/audit path.
			handler.ServeHTTP(output, call)
		}()
	}
}

func normalizeResponsesWSCreate(data []byte) ([]byte, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(data, &body); err != nil || body == nil {
		return nil, fmt.Errorf("invalid response.create")
	}
	// Accept the legacy wrapped form as well as the current flat Responses API.
	if wrapped, ok := body["response"]; ok {
		generate, streamID := body["generate"], body["stream_id"]
		body = nil
		if err := json.Unmarshal(wrapped, &body); err != nil || body == nil {
			return nil, fmt.Errorf("response must be an object")
		}
		if generate != nil {
			body["generate"] = generate
		}
		if streamID != nil {
			body["stream_id"] = streamID
		}
	}
	for _, name := range []string{"type", "event_id", "response", "stream_options", "background"} {
		delete(body, name)
	}
	if generate, ok := body["generate"]; ok && string(generate) != "true" && string(generate) != "false" {
		return nil, fmt.Errorf("generate must be a boolean")
	}
	if value, ok := body["stream_id"]; ok {
		var id string
		if json.Unmarshal(value, &id) != nil || len(id) == 0 || len(id) > 256 {
			return nil, fmt.Errorf("stream_id must contain 1-256 ASCII letters, digits, underscores, hyphens or periods")
		}
		for _, char := range id {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.') {
				return nil, fmt.Errorf("invalid stream_id")
			}
		}
	}
	var model string
	if json.Unmarshal(body["model"], &model) != nil || strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("model is required")
	}
	body["stream"] = json.RawMessage("true")
	return json.Marshal(body)
}

// filterCandidates keeps native Responses WebSocket candidates. model is the
// client's model, which the bound socket must match; routingModel stands in
// for a candidate without an explicit upstream model.
func (session *responsesWSSession) filterCandidates(model, routingModel string, candidates []endpoint.Resolved) []endpoint.Resolved {
	result := make([]endpoint.Resolved, 0, len(candidates))
	for _, candidate := range candidates {
		if websocketSkip(candidate, routingModel) != "" {
			continue
		}
		upstreamModel := candidate.UpstreamModel
		if upstreamModel == "" {
			upstreamModel = routingModel
		}
		if session.serviceID != "" && (session.serviceID != candidate.CanonicalService().ID || session.model != model || session.upstreamModel != upstreamModel) {
			continue
		}
		result = append(result, candidate)
	}
	return result
}

// websocketSkip says why candidate cannot serve a native Responses WebSocket
// turn, or is empty when it can.
func websocketSkip(candidate endpoint.Resolved, routingModel string) contract.RoutingSkipReason {
	service := candidate.CanonicalService()
	if !service.Enabled {
		return contract.RoutingSkipDisabled
	}
	if !service.ResponsesWebSocket() {
		return contract.RoutingSkipWebSocketDisabled
	}
	if candidate.PlanType == contract.PlanTypeRelayKit {
		return contract.RoutingSkipWebSocketUnsupported
	}
	if candidate.UpstreamProtocol != "" && candidate.UpstreamProtocol != contract.ProtocolOpenAIResponses {
		return contract.RoutingSkipWebSocketUnsupported
	}
	upstreamModel := candidate.UpstreamModel
	if upstreamModel == "" {
		upstreamModel = routingModel
	}
	if native := service.Kind.ModelNativeProtocol(upstreamModel); native != "" && native != contract.ProtocolOpenAIResponses {
		return contract.RoutingSkipWebSocketUnsupported
	}
	for _, capability := range service.Capabilities {
		if capability.Protocol == contract.ProtocolOpenAIResponses && capability.Streaming && capability.ConvertTo == "" {
			return ""
		}
	}
	return contract.RoutingSkipWebSocketUnsupported
}
func (turn *responsesWSTurn) forward(writer http.ResponseWriter, request *http.Request, target transport.Target, candidate endpoint.Resolved, upstreamModel string) error {
	// Hash credentials instead of retaining their plaintext as connection identity.
	headers, _ := json.Marshal(target.RequestHeaders)
	binding := fmt.Sprintf("%s/%s/%s/%x", candidate.CanonicalService().ID, upstreamModel, target.BaseURL, sha256.Sum256(headers))
	err := turn.session.upstream.Forward(writer, request, target, binding, turn.controls)
	if turn.session.upstream.Connected() {
		turn.session.serviceID = candidate.CanonicalService().ID
		turn.session.model = candidate.RequestedModel
		turn.session.upstreamModel = upstreamModel
		turn.session.routingModel = turn.routingModel
		if turn.session.routingModel == "" {
			turn.session.routingModel = candidate.RequestedModel
		}
	}
	return err
}
func (session *responsesWSSession) writeLocked(data []byte) error {
	_ = session.client.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return session.client.WriteMessage(websocket.TextMessage, data)
}
func (session *responsesWSSession) sendError(eventID string, status int, code, message string) {
	payload, _ := json.Marshal(map[string]any{"type": "error", "status": status, "event_id": eventID, "error": map[string]any{"type": "invalid_request_error", "code": code, "message": message}})
	session.mu.Lock()
	defer session.mu.Unlock()
	_ = session.writeLocked(payload)
}

// responsesWSWriter unwraps the internal SSE envelope after all response
// transformations. It holds the terminal event until recording is finished and
// admission is released, so a client can immediately start the next turn.
type responsesWSWriter struct {
	turn     *responsesWSTurn
	header   http.Header
	status   int
	buffer   []byte
	terminal []byte
	// A reviewed success terminal is published before success is recorded.
	// Retain admission until finish so an immediate next turn cannot race
	// persistence and overwrite the connection binding.
	recordingLocked bool
}

func (writer *responsesWSWriter) Header() http.Header { return writer.header }
func (writer *responsesWSWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
}
func (writer *responsesWSWriter) Flush() {}
func (writer *responsesWSWriter) Write(data []byte) (int, error) {
	if writer.status == 0 {
		writer.status = 200
	}
	previous := len(writer.buffer)
	consumed := 0
	accepted := func() int { return max(0, min(len(data), consumed-previous)) }
	writer.buffer = append(writer.buffer, data...)
	if len(writer.buffer) > 128<<20 {
		return 0, fmt.Errorf("Responses WebSocket event exceeds size limit")
	}
	if !strings.Contains(writer.header.Get("Content-Type"), "text/event-stream") {
		return len(data), nil
	}
	for {
		end := bytes.Index(writer.buffer, []byte("\n\n"))
		if end < 0 {
			break
		}
		frame := writer.buffer[:end]
		writer.buffer = writer.buffer[end+2:]
		var payload []byte
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				payload = append(payload, bytes.TrimSpace(line[5:])...)
				payload = append(payload, '\n')
			}
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			consumed += end + 2
			continue
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &event) != nil {
			return accepted(), fmt.Errorf("invalid Responses event")
		}
		session := writer.turn.session
		session.mu.Lock()
		// Only the review gate's cancellation stops local replay. Ordinary
		// upstream cancellation still publishes its response.cancelled terminal.
		cancelled := writer.turn.cancelReview != nil && writer.turn.cancelled
		cancelled = cancelled || (writer.turn.ctx != nil && writer.turn.ctx.Err() != nil)
		if cancelled {
			session.mu.Unlock()
			return accepted(), context.Canceled
		}
		if transport.ResponsesTerminalEvent(event.Type) {
			writer.terminal = bytes.Clone(payload)
			session.mu.Unlock()
			consumed += end + 2
			continue
		}
		err := session.writeLocked(payload)
		session.mu.Unlock()
		if err != nil {
			return accepted(), err
		}
		consumed += end + 2
	}
	return len(data), nil
}

// ConfirmToolReviewDelivery is called after a reject batch passes, or before
// detaching a mark review. It never waits for the mark reviewer. The
// outer WebSocket handler always calls finish, which releases this lock after
// the request record and successful routing side effects have been finalized.
func (writer *responsesWSWriter) ConfirmToolReviewDelivery() error {
	if writer.recordingLocked {
		return nil
	}
	session := writer.turn.session
	session.mu.Lock()
	if writer.turn.cancelled || (writer.turn.ctx != nil && writer.turn.ctx.Err() != nil) {
		session.mu.Unlock()
		return context.Canceled
	}
	terminal := writer.terminal
	writer.terminal = nil
	if len(terminal) == 0 {
		session.mu.Unlock()
		return fmt.Errorf("reviewed Responses terminal missing")
	}
	if err := session.writeLocked(terminal); err != nil {
		session.mu.Unlock()
		return err
	}
	writer.recordingLocked = true
	return nil
}

func (writer *responsesWSWriter) finish() {
	session := writer.turn.session
	if writer.recordingLocked {
		session.active = nil
		writer.recordingLocked = false
		session.mu.Unlock()
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	defer func() { session.active = nil }()
	cancelled := writer.turn.cancelReview != nil && writer.turn.cancelled
	cancelled = cancelled || (writer.turn.ctx != nil && writer.turn.ctx.Err() != nil)
	if writer.terminal != nil && !cancelled {
		_ = session.writeLocked(writer.terminal)
		return
	}
	// Preserve local and handshake errors as Responses error events.
	var object map[string]json.RawMessage
	_ = json.Unmarshal(writer.buffer, &object)
	problem := object["error"]
	if len(problem) == 0 {
		problem = json.RawMessage(`{"type":"server_error","code":"upstream_interrupted","message":"upstream WebSocket response ended before a terminal event"}`)
	}
	status := writer.status
	if status < 400 {
		status = 502
	}
	payload, _ := json.Marshal(map[string]any{"type": "error", "status": status, "event_id": writer.turn.eventID, "stream_id": writer.turn.streamID, "error": problem})
	_ = session.writeLocked(payload)
}

func responsesWSCancel(streamID, responseID string) []byte {
	event := map[string]string{"type": "response.cancel"}
	if streamID != "" {
		event["stream_id"] = streamID
	}
	if responseID != "" {
		event["response_id"] = responseID
	}
	body, _ := json.Marshal(event)
	return body
}
