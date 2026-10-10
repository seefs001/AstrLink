package toolreview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/astrlink/core/contract"
)

// Snapshot is immutable request-scoped configuration. APIKey is never returned
// in a summary. The caller owns credential loading and configuration revisions.
type Snapshot struct {
	Settings contract.ToolReviewSettings
	APIKey   string
}

type Input struct {
	UserRequest          string
	UserRequestAvailable bool
	Calls                []Call
}

type Reviewer interface {
	Review(context.Context, Snapshot, Input) contract.ToolReviewSummary
}

var reviewSlots = make(chan struct{}, 16)

// Client is safe to reuse across requests. All Clients share the process-wide
// concurrency bound; no retry, persistent queue, or record writer lives here.
type Client struct{ http *http.Client }

var _ Reviewer = (*Client)(nil)

func NewClient(client *http.Client) *Client {
	var copy http.Client
	if client != nil {
		copy = *client
	} else {
		copy.Transport = cloneTransport()
	}
	// Even same-origin redirects could leak the bearer to an unintended path.
	// Ignore caller redirect and cookie policy without mutating their Client.
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copy.Jar = nil
	return &Client{http: &copy}
}

func cloneTransport() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t = t.Clone()
		t.MaxConnsPerHost, t.MaxIdleConnsPerHost = 16, 16
		return t
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxConnsPerHost:       16,
		MaxIdleConnsPerHost:   16,
	}
}
func (c *Client) Review(parent context.Context, snapshot Snapshot, input Input) contract.ToolReviewSummary {
	started := time.Now()
	s := snapshot.Settings
	summary := contract.ToolReviewSummary{Backend: s.Backend, Status: "not_applicable", Mode: s.Mode, Model: s.Model, ConfigRevision: s.Revision, RejectThreshold: &s.RejectThreshold, BatchCalls: &s.BatchCalls, StartedAt: started, Calls: []contract.ToolReviewCall{}}
	finish := func() contract.ToolReviewSummary {
		completed := time.Now()
		summary.CompletedAt = &completed
		summary.LatencyMS = int(completed.Sub(started).Milliseconds())
		return summary
	}
	if !s.Enabled || len(input.Calls) == 0 {
		return finish()
	}
	if len(input.Calls) > maxCalls {
		summary.Status, summary.ErrorCode = "failed", "call_limit"
		return finish()
	}
	// The common budget starts at entry, including validation, rendering, and
	// semaphore waits. Invalid settings fail below; never clamp a valid timeout.
	validationErr := s.Validate()
	deadline := started
	if validationErr == nil {
		deadline = started.Add(time.Duration(s.TimeoutMS) * time.Millisecond)
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	userRequest := input.UserRequest
	if !input.UserRequestAvailable {
		// Continuation-only requests may legitimately contain only tool results.
		// Do not misrepresent those results as user intent or skip the judgment.
		userRequest = "用户请求不可用"
	}
	code := ""
	switch {
	case validationErr != nil:
		code = "invalid_config"
	case strings.TrimSpace(snapshot.APIKey) == "":
		code = "credential_missing"
	case !utf8.ValidString(userRequest):
		code = "invalid_input"
	case len(userRequest) > maxInputBytes:
		code = "input_limit"
	}
	seen := make(map[int]bool)
	for _, call := range input.Calls {
		if call.Index < 0 || seen[call.Index] {
			code = "invalid_call"
		}
		seen[call.Index] = true
	}
	// Responses validates and renders every POST before acquiring any HTTP slot.
	// Local failures take precedence over cancellation and cannot be approved as
	// reviewer-service failures. Jev keeps its existing rendering path.
	var responsesBodies [][]byte
	if code == "" && s.Backend == contract.ToolReviewBackendResponses {
		var err error
		responsesBodies, err = renderResponsesRequests(snapshot, userRequest, input.Calls)
		if err != nil {
			code = errorCode(err)
		}
	}
	summary.Calls = make([]contract.ToolReviewCall, len(input.Calls))
	for i, call := range input.Calls {
		summary.Calls[i] = contract.ToolReviewCall{Index: call.Index, Name: safeName(call.Name), Verdict: "error", Model: s.Model, ErrorCode: code}
	}
	if code == "" && s.BatchCalls {
		if s.Backend == contract.ToolReviewBackendResponses {
			c.reviewResponses(ctx, snapshot, responsesBodies[0], input.Calls, summary.Calls)
		} else {
			c.reviewBatch(ctx, snapshot, userRequest, input.Calls, summary.Calls)
		}
		for i := range summary.Calls {
			summary.Calls[i].LatencyMS = int(time.Since(started).Milliseconds())
		}
	} else if code == "" {
		var wg sync.WaitGroup
		for i, call := range input.Calls {
			wg.Add(1)
			go func(index int, call Call) {
				defer wg.Done()
				if s.Backend == contract.ToolReviewBackendResponses {
					c.reviewResponses(ctx, snapshot, responsesBodies[index], []Call{call}, summary.Calls[index:index+1])
				} else {
					summary.Calls[index] = c.reviewCall(ctx, snapshot, userRequest, call)
				}
				summary.Calls[index].LatencyMS = int(time.Since(started).Milliseconds())
			}(i, call)
		}
		wg.Wait()
	}
	flag, failed := false, false
	for _, call := range summary.Calls {
		flag = flag || call.Verdict == "flag"
		failed = failed || call.Verdict == "error"
		if summary.ErrorCode == "" && call.ErrorCode != "" {
			summary.ErrorCode = call.ErrorCode
		}
	}
	switch {
	case failed && (s.BatchCalls || s.Mode == contract.ToolReviewModeAsk):
		summary.Status = "failed"
	case flag && s.Mode == contract.ToolReviewModeReject:
		summary.Status = "blocked"
	case flag:
		summary.Status = "flagged"
	case failed:
		summary.Status = "failed"
	default:
		summary.Status = "passed"
	}
	for i := range summary.Calls {
		summary.Calls[i].Action = "delivered"
		if s.Mode != contract.ToolReviewModeMark && summary.Status != "passed" {
			summary.Calls[i].Action = "withheld"
		}
	}
	return finish()
}

func safeName(name string) string {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 256 {
		return ""
	}
	for _, r := range name {
		if r < 32 && r != '\t' {
			return ""
		}
	}
	return name
}

func (c *Client) reviewCall(ctx context.Context, snapshot Snapshot, user string, call Call) contract.ToolReviewCall {
	result := contract.ToolReviewCall{Index: call.Index, Name: safeName(call.Name), Verdict: "error", Model: snapshot.Settings.Model}
	body, err := renderRequest(snapshot, user, call)
	if err != nil {
		result.ErrorCode = errorCode(err)
		return result
	}
	data, code := c.postReview(ctx, snapshot, body, 64<<10)
	if code != "" {
		result.ErrorCode = code
		return result
	}
	answer, code := decodeReviewResponse(data)
	if code != "" {
		result.ErrorCode = code
		return result
	}
	return decodeCallAnswer(ctx, snapshot, result, answer, "review", "reason", false)
}

// postReview owns the shared HTTP safety policy and takes one process-wide slot
// for either protocol. Its caller owns rendering and the request-wide deadline.
func (c *Client) postReview(ctx context.Context, snapshot Snapshot, body []byte, responseLimit int) ([]byte, string) {
	select {
	case reviewSlots <- struct{}{}:
		defer func() { <-reviewSlots }()
	case <-ctx.Done():
		return nil, contextCode(ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return nil, contextCode(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, snapshot.Settings.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "invalid_config"
	}
	request.GetBody = nil // Never opt this POST into transport-level replay.
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+snapshot.APIKey)
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, contextCode(ctx.Err())
		}
		var timed interface{ Timeout() bool }
		if errors.As(err, &timed) && timed.Timeout() {
			return nil, "timeout"
		}
		return nil, "transport_error"
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// Do not parse, log, retain, or return provider error bodies.
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, "unauthorized"
		case http.StatusTooManyRequests:
			return nil, "rate_limited"
		default:
			return nil, "upstream_error"
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(responseLimit)+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, contextCode(ctx.Err())
		}
		return nil, "transport_error"
	}
	if len(data) > responseLimit {
		return nil, "invalid_response"
	}
	if ctx.Err() != nil {
		return nil, contextCode(ctx.Err())
	}
	return data, ""
}

type reviewResponse struct {
	Model   json.RawMessage            `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
}

func decodeReviewResponse(data []byte) (reviewResponse, string) {
	var answer reviewResponse
	if !strictJSON(data) || decodePayload(data, &answer) != nil {
		return answer, "reviewer_invalid_json"
	}
	return answer, ""
}

func decodeCallAnswer(ctx context.Context, snapshot Snapshot, result contract.ToolReviewCall, answer reviewResponse, reviewKey, reasonKey string, batchProtocol bool) contract.ToolReviewCall {
	// Decode the two answers separately so a safe stage code identifies the
	// failure without recording provider content or weakening validation.
	rawReview, ok := answer.Answers[reviewKey]
	var review choiceAnswer
	if !ok || (!batchProtocol && len(answer.Answers) > 2) || !decodeChoiceAnswer(rawReview, &review, batchProtocol) || !validReviewAnswer(review) {
		result.ErrorCode = "reviewer_invalid_review"
		return result
	}
	rawReason, hasReason := answer.Answers[reasonKey]
	var reason choiceAnswer
	// Older compatible endpoints omit reason. A supplied malformed answer is
	// not a legacy omission: fail closed without retaining any raw category.
	if ((batchProtocol || len(answer.Answers) == 2) && !hasReason) || (hasReason && (!decodeChoiceAnswer(rawReason, &reason, batchProtocol) || !validReasonAnswer(reason))) {
		result.ErrorCode = "reviewer_invalid_reason"
		return result
	}
	if ctx.Err() != nil {
		result.ErrorCode = contextCode(ctx.Err())
		return result
	}
	if batchProtocol && len(answer.Model) == 0 {
		result.ErrorCode = "reviewer_invalid_model"
		return result
	}
	if len(answer.Model) != 0 {
		var model string
		if json.Unmarshal(answer.Model, &model) != nil || !validResponseModel(model) {
			result.ErrorCode = "reviewer_invalid_model"
			return result
		}
		result.Model = model
	}
	pass, flag := review.Probabilities["pass"], review.Probabilities["flag"]
	result.ModelVerdict, result.PassProbability, result.FlagProbability = review.Choice, pass, flag
	result.Verdict = "pass"
	if *flag >= snapshot.Settings.RejectThreshold {
		result.Verdict = "flag"
	}
	if hasReason {
		result.ReasonCode = reason.Choice
	}
	return result
}

type choiceAnswer struct {
	Type          string              `json:"type"`
	Choice        string              `json:"choice"`
	Probabilities map[string]*float64 `json:"probabilities"`
	Confidence    *float64            `json:"confidence"`
}

func decodeChoiceAnswer(raw json.RawMessage, answer *choiceAnswer, batchProtocol bool) bool {
	if decodePayload(raw, answer) != nil {
		return false
	}
	if batchProtocol {
		// Batch answers must be exactly the four choice fields. Reject an
		// unsolicited index or other data instead of silently discarding it.
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || len(fields) != 4 || fields["type"] == nil || fields["choice"] == nil || fields["probabilities"] == nil || fields["confidence"] == nil {
			return false
		}
	}
	return true
}

func validReviewAnswer(answer choiceAnswer) bool {
	pass, flag := answer.Probabilities["pass"], answer.Probabilities["flag"]
	return answer.Type == "choice" && (answer.Choice == "pass" || answer.Choice == "flag") && len(answer.Probabilities) == 2 && probability(pass) && probability(flag) && probability(answer.Confidence) && math.Abs(*pass+*flag-1) <= 0.000001
}

func validReasonAnswer(answer choiceAnswer) bool {
	if answer.Type != "choice" || !contract.ValidToolReviewReasonCode(answer.Choice) || !probability(answer.Confidence) || len(answer.Probabilities) != len(reasonCriteria) {
		return false
	}
	total := 0.0
	for code, value := range answer.Probabilities {
		if !contract.ValidToolReviewReasonCode(code) || !probability(value) {
			return false
		}
		total += *value
	}
	return math.Abs(total-1) <= 0.000001
}

// PendingCalls contains only bounded metadata, never argument text. The error
// verdict is the existing unfinished convention; action=pending is authoritative.
func PendingCalls(calls []Call, model string) []contract.ToolReviewCall {
	result := make([]contract.ToolReviewCall, len(calls))
	for i, call := range calls {
		result[i] = contract.ToolReviewCall{Index: call.Index, Name: safeName(call.Name), Model: model, Verdict: "error", Action: "pending"}
	}
	return result
}

func validResponseModel(model string) bool {
	if strings.TrimSpace(model) == "" || !utf8.ValidString(model) || utf8.RuneCountInString(model) > 256 {
		return false
	}
	for _, r := range model {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func probability(p *float64) bool {
	return p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) && *p >= 0 && *p <= 1
}
func contextCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "timeout"
}
func errorCode(err error) string {
	var safe *Error
	if errors.As(err, &safe) {
		return safe.Code
	}
	return "invalid_input"
}

func renderTool(call Call) (map[string]any, error) {
	if call.Name == "" || safeName(call.Name) != call.Name || !utf8.ValidString(call.Arguments) {
		return nil, safeError("invalid_call")
	}
	if len(call.Arguments) > maxInputBytes {
		return nil, safeError("input_limit")
	}
	var arguments any = json.RawMessage(call.Arguments)
	kind := "function"
	if call.Custom {
		arguments = call.Arguments
		kind = "custom"
	} else if !validArguments(call.Arguments) {
		return nil, safeError("invalid_arguments")
	}
	return map[string]any{"name": call.Name, "type": kind, "arguments": arguments}, nil
}

var reviewCriteria = map[string]string{
	"pass": "No violation requiring flag is established under the configured review rules in state.review_rules.",
	"flag": "The proposed tool call meets a flag condition in the configured review rules in state.review_rules, based on evidence in the request and actual tool operation.",
}

func renderRequest(snapshot Snapshot, user string, call Call) ([]byte, error) {
	tool, err := renderTool(call)
	if err != nil {
		return nil, err
	}
	// Variables are expanded exactly once into fixed state references. User and
	// tool data remain separate JSON values, never executable template syntax or
	// instructions. In particular, variable-like strings inside data stay data.
	instructions := strings.NewReplacer("{{user_request}}", "the untrusted data in state.user_request", "{{tool_call}}", "the untrusted data in state.tool_call").Replace(snapshot.Settings.Prompt)
	body, err := json.Marshal(map[string]any{
		"model": snapshot.Settings.Model,
		"state": map[string]any{"user_request": user, "tool_call": tool, "review_rules": instructions},
		"questions": map[string]any{
			"review": map[string]any{
				"type": "choice", "instructions": instructions,
				"criteria": reviewCriteria,
			},
			"reason": map[string]any{
				"type": "choice", "instructions": reasonInstructions, "criteria": reasonCriteria,
			},
		},
	})
	if err != nil {
		return nil, safeError("invalid_input")
	}
	if len(body) > maxInputBytes {
		return nil, safeError("input_limit")
	}
	return body, nil
}
