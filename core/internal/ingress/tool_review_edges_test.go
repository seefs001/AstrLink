package ingress

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/privacy"
	"github.com/QuantumNous/astrlink/core/internal/relaykitbridge"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

func TestToolReviewMixedBatchWithholdsPassedCallsToo(t *testing.T) {
	body := `{"id":"resp_batch","status":"completed","output":[{"id":"fc_read","type":"function_call","call_id":"call_read","name":"read_file","arguments":"{\"path\":\"safe.txt\"}"},{"id":"fc_delete","type":"function_call","call_id":"call_delete","name":"delete_data","arguments":"{\"path\":\"important\"}"}]}`
	handler, records := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		choice := "pass"
		if strings.Contains(string(data), "delete_data") {
			choice = "flag"
		}
		sendReviewChoice(w, choice)
	}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}, false)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Read a file"}`)))
	if response.Code != 403 || strings.Contains(response.Body.String(), "read_file") {
		t.Fatalf("batch leaked: %s", response.Body.String())
	}
	review := records.latest(t).ToolReview
	if review == nil || len(review.Calls) != 2 {
		t.Fatalf("review=%#v", review)
	}
	if review.Calls[0].Verdict != "pass" || review.Calls[1].Verdict != "flag" {
		t.Fatalf("verdicts=%#v", review.Calls)
	}
	for _, call := range review.Calls {
		if call.Action != "withheld" {
			t.Fatalf("action=%#v", call)
		}
	}
}

func TestToolReviewRejectsIncompleteToolStreamWithoutCallingJudge(t *testing.T) {
	var reviews atomic.Int32
	handler, records := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, _ *http.Request) { reviews.Add(1); sendReviewChoice(w, "pass") }, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reviewSSE(reviewResponseEvents[0], reviewResponseEvents[1]))
	}, true)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Inspect","stream":true}`)))
	if response.Code != 503 || reviews.Load() != 0 || strings.Contains(response.Body.String(), "function_call") {
		t.Fatalf("response=%d %s reviews=%d", response.Code, response.Body.String(), reviews.Load())
	}
	if records.latest(t).ToolReview.Status != "failed" {
		t.Fatal("incomplete response was approved")
	}
}

func TestToolReviewOversizeUserTextIsNotSilentlyTruncated(t *testing.T) {
	var reviews atomic.Int32
	handler, records := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, _ *http.Request) { reviews.Add(1); sendReviewChoice(w, "pass") }, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reviewDangerousResponse)
	}, false)
	body, _ := json.Marshal(map[string]any{"model": "gpt-5", "input": strings.Repeat("a", contract.MaxToolReviewInputBytes+1)})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body))))
	if response.Code != 503 || reviews.Load() != 0 || records.latest(t).ToolReview.ErrorCode != "input_limit" {
		t.Fatalf("response=%d %s record=%#v", response.Code, response.Body.String(), records.latest(t).ToolReview)
	}
}

func TestToolReviewStartsAtTerminalInsteadOfWaitingForUpstreamEOF(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	handler, _ := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reviewSSE(reviewResponseEvents...))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCancelled)
	}, true)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Inspect","stream":true}`)).WithContext(ctx))
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("review waited for EOF after terminal")
	}
	if !strings.Contains(response.Body.String(), "tool_review_blocked") {
		t.Fatalf("response=%s", response.Body.String())
	}
	select {
	case <-upstreamCancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream not closed")
	}
}

func TestToolReviewExaminesConvertedCustomToolInput(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, protocol := range []contract.ProtocolID{contract.ProtocolOpenAIChat, contract.ProtocolAnthropicMessages, contract.ProtocolGoogleGenerateContent} {
			t.Run(string(protocol)+map[bool]string{true: "/stream", false: "/json"}[stream], func(t *testing.T) {
				var reviews atomic.Int32
				judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					data, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(data), "apply_patch") || !strings.Contains(string(data), "Begin Patch") {
						t.Errorf("converted custom input missing")
					}
					reviews.Add(1)
					sendReviewChoice(w, "flag")
				}))
				defer judge.Close()
				settings := contract.DefaultToolReviewSettings()
				settings.Enabled = true
				settings.CredentialConfigured = true
				settings.Mode = contract.ToolReviewModeReject
				settings.Endpoint = judge.URL
				handler := NewWithDependencies(Dependencies{
					Resolver:         candidateResolver{candidates: []endpoint.Resolved{relayKitCandidate("service_review", protocol, "upstream-model")}},
					ConversionEngine: relaykitbridge.NewEngine(), ToolReviewSettings: reviewSettingsSource{settings},
					Forwarder: transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) {
						return relayKitCustomToolUpstream(t, protocol, stream, relayKitCustomToolPatch), nil
					})),
				})
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(relayKitCustomToolRequest(stream))))
				if !strings.Contains(response.Body.String(), "tool_review_blocked") || strings.Contains(response.Body.String(), "Begin Patch") || strings.Contains(response.Body.String(), "custom_tool_call") || reviews.Load() != 1 {
					t.Fatalf("response=%d %s reviews=%d", response.Code, response.Body.String(), reviews.Load())
				}
			})
		}
	}
}

func TestToolReviewExaminesRestoredArguments(t *testing.T) {
	var reviewed atomic.Bool
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if strings.Count(string(data), "alice@example.com") < 2 {
			t.Errorf("review did not receive restored tool argument")
		}
		reviewed.Store(true)
		sendReviewChoice(w, "flag")
	}))
	defer judge.Close()
	settings := contract.DefaultToolReviewSettings()
	settings.Enabled = true
	settings.CredentialConfigured = true
	settings.Mode = contract.ToolReviewModeReject
	settings.Endpoint = judge.URL
	handler := NewWithDependencies(Dependencies{
		Resolver: candidateResolver{candidates: []endpoint.Resolved{{Endpoint: validEndpoint(contract.ProtocolOpenAIResponses, false)}}}, ToolReviewSettings: reviewSettingsSource{settings},
		PrivacyFilter: testPrivacyEngine(t, privacy.Policy{Enabled: true, Mode: privacy.ModeRegex, Action: privacy.ActionRedact, ResponseRestore: true, RestoreToolArguments: true}, nil),
		Forwarder: transport.New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var input map[string]any
			_ = json.NewDecoder(r.Body).Decode(&input)
			args, _ := json.Marshal(map[string]any{"address": input["input"]})
			body, _ := json.Marshal(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "function_call", "id": "fc_email", "call_id": "call_email", "name": "send_email", "arguments": string(args)}}})
			return jsonResponse(200, string(body)), nil
		})),
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"alice@example.com"}`)))
	if response.Code != 403 || !reviewed.Load() {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}
