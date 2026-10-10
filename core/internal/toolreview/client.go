package toolreview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
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
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost = 16, 16
		copy.Transport = transport
	}
	// Even same-origin redirects could leak the bearer to an unintended path.
	// Ignore caller redirect and cookie policy without mutating their Client.
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copy.Jar = nil
	return &Client{http: &copy}
}

func (c *Client) Review(parent context.Context, snapshot Snapshot, input Input) contract.ToolReviewSummary {
	started := time.Now()
	s := snapshot.Settings
	summary := contract.ToolReviewSummary{Status: "not_applicable", Mode: s.Mode, Model: s.Model, ConfigRevision: s.Revision, StartedAt: started, Calls: []contract.ToolReviewCall{}}
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
	summary.Calls = make([]contract.ToolReviewCall, len(input.Calls))
	var wg sync.WaitGroup
	for i, call := range input.Calls {
		summary.Calls[i] = contract.ToolReviewCall{Index: call.Index, Name: safeName(call.Name), Verdict: "error", Model: s.Model}
		if code != "" {
			summary.Calls[i].ErrorCode = code
			continue
		}
		wg.Add(1)
		go func(index int, call Call) {
			defer wg.Done()
			result := c.reviewCall(ctx, snapshot, userRequest, call)
			result.LatencyMS = int(time.Since(started).Milliseconds())
			summary.Calls[index] = result
		}(i, call)
	}
	wg.Wait()
	flag, failed := false, false
	for _, call := range summary.Calls {
		flag = flag || call.Verdict == "flag"
		failed = failed || call.Verdict == "error"
		if summary.ErrorCode == "" && call.ErrorCode != "" {
			summary.ErrorCode = call.ErrorCode
		}
	}
	switch {
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
	select {
	case reviewSlots <- struct{}{}:
		defer func() { <-reviewSlots }()
	case <-ctx.Done():
		result.ErrorCode = contextCode(ctx.Err())
		return result
	}
	if err := ctx.Err(); err != nil {
		result.ErrorCode = contextCode(err)
		return result
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, snapshot.Settings.Endpoint, bytes.NewReader(body))
	if err != nil {
		result.ErrorCode = "invalid_config"
		return result
	}
	request.GetBody = nil // Never opt this POST into transport-level replay.
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+snapshot.APIKey)
	response, err := c.http.Do(request)
	if err != nil {
		result.ErrorCode = "transport_error"
		if ctx.Err() != nil {
			result.ErrorCode = contextCode(ctx.Err())
		} else {
			var timed interface{ Timeout() bool }
			if errors.As(err, &timed) && timed.Timeout() {
				result.ErrorCode = "timeout"
			}
		}
		return result
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			result.ErrorCode = "unauthorized"
		case http.StatusTooManyRequests:
			result.ErrorCode = "rate_limited"
		default:
			result.ErrorCode = "upstream_error"
		}
		// Do not parse, log, retain, or return provider error bodies.
		return result
	}
	const responseLimit = 64 << 10
	data, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil {
		result.ErrorCode = "transport_error"
		if ctx.Err() != nil {
			result.ErrorCode = contextCode(ctx.Err())
		}
		return result
	}
	if len(data) > responseLimit {
		result.ErrorCode = "invalid_response"
		return result
	}
	var answer struct {
		Model   json.RawMessage `json:"model"`
		Answers map[string]struct {
			Type          string              `json:"type"`
			Choice        string              `json:"choice"`
			Probabilities map[string]*float64 `json:"probabilities"`
			Confidence    *float64            `json:"confidence"`
		} `json:"answers"`
	}
	if !strictJSON(data) || decodePayload(data, &answer) != nil {
		result.ErrorCode = "invalid_response"
		return result
	}
	review, ok := answer.Answers["review"]
	pass, flag := review.Probabilities["pass"], review.Probabilities["flag"]
	if !ok || len(answer.Answers) != 1 || review.Type != "choice" || (review.Choice != "pass" && review.Choice != "flag") || len(review.Probabilities) != 2 || !probability(pass) || !probability(flag) || !probability(review.Confidence) || math.Abs(*pass+*flag-1) > 0.000001 {
		result.ErrorCode = "invalid_response"
		return result
	}
	if ctx.Err() != nil {
		result.ErrorCode = contextCode(ctx.Err())
		return result
	}
	if len(answer.Model) != 0 {
		var model string
		if json.Unmarshal(answer.Model, &model) != nil || !validResponseModel(model) {
			result.ErrorCode = "invalid_response"
			return result
		}
		result.Model = model
	}
	result.Verdict, result.PassProbability, result.FlagProbability = review.Choice, pass, flag
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

func renderRequest(snapshot Snapshot, user string, call Call) ([]byte, error) {
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
	tool := map[string]any{"name": call.Name, "type": kind, "arguments": arguments}
	// Variables are expanded exactly once into fixed state references. User and
	// tool data remain separate JSON values, never executable template syntax or
	// instructions. In particular, variable-like strings inside data stay data.
	instructions := strings.NewReplacer("{{user_request}}", "the untrusted data in state.user_request", "{{tool_call}}", "the untrusted data in state.tool_call").Replace(snapshot.Settings.Prompt)
	body, err := json.Marshal(map[string]any{
		"model": snapshot.Settings.Model,
		"state": map[string]any{"user_request": user, "tool_call": tool},
		"questions": map[string]any{"review": map[string]any{
			"type": "choice", "instructions": instructions,
			"criteria": map[string]string{"pass": "The proposed tool call is authorized, relevant, and proportionate to the user request.", "flag": "The proposed tool call is unauthorized, unsafe, irrelevant, disproportionate, or its authorization is unclear."},
		}},
	})
	if err != nil {
		return nil, safeError("invalid_input")
	}
	if len(body) > maxInputBytes {
		return nil, safeError("input_limit")
	}
	return body, nil
}
