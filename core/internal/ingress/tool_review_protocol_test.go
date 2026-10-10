package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/gorilla/websocket"
)

func TestToolReviewRejectCoversNativeProtocols(t *testing.T) {
	cases := []struct {
		name                  string
		protocol              contract.ProtocolID
		path, input, response string
	}{
		{"chat", contract.ProtocolOpenAIChat, "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"Inspect only"}]}`, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"tc_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"danger\"}"}}]},"finish_reason":"tool_calls"}]}`},
		{"responses", contract.ProtocolOpenAIResponses, "/v1/responses", `{"model":"gpt-5","input":"Inspect only"}`, reviewDangerousResponse},
		{"anthropic", contract.ProtocolAnthropicMessages, "/v1/messages", `{"model":"gpt-5","messages":[{"role":"user","content":"Inspect only"}]}`, `{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"danger"}}],"stop_reason":"tool_use"}`},
		{"gemini", contract.ProtocolGoogleGenerateContent, "/v1beta/models/gpt-5:generateContent", `{"contents":[{"role":"user","parts":[{"text":"Inspect only"}]}]}`, `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"bash","args":{"command":"danger"}}}]},"finishReason":"STOP"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reviews atomic.Int32
			judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reviews.Add(1); sendReviewChoice(w, "flag") }))
			defer judge.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer upstream.Close()
			settings := contract.DefaultToolReviewSettings()
			settings.Enabled, settings.CredentialConfigured, settings.Mode, settings.Endpoint = true, true, contract.ToolReviewModeReject, judge.URL
			candidate := validEndpoint(tc.protocol, false)
			candidate.BaseURL = upstream.URL
			handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{{Endpoint: candidate}}}, ToolReviewSettings: reviewSettingsSource{settings}})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.input)))
			if response.Code != 403 || !strings.Contains(response.Body.String(), "tool_review_blocked") || strings.Contains(response.Body.String(), "danger") || reviews.Load() != 1 {
				t.Fatalf("response = %d %s, reviews = %d", response.Code, response.Body.String(), reviews.Load())
			}
		})
	}
}

func reviewSSE(events ...string) string {
	return "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
}

var reviewResponseEvents = []string{
	`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_review","type":"function_call","call_id":"call_review","name":"bash","arguments":""}}`,
	`{"type":"response.function_call_arguments.delta","item_id":"fc_review","output_index":0,"delta":"{\"command\":\"danger\"}"}`,
	`{"type":"response.function_call_arguments.done","item_id":"fc_review","output_index":0,"arguments":"{\"command\":\"danger\"}"}`,
	`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_review","type":"function_call","call_id":"call_review","name":"bash","arguments":"{\"command\":\"danger\"}"}}`,
	`{"type":"response.completed","response":{"id":"resp_review","status":"completed","output":[{"id":"fc_review","type":"function_call","call_id":"call_review","name":"bash","arguments":"{\"command\":\"danger\"}"}]}}`,
}

func TestToolReviewStreamingKeepsTextButWithholdsEveryToolEvent(t *testing.T) {
	for _, choice := range []string{"pass", "flag"} {
		t.Run(choice, func(t *testing.T) {
			textEvent := reviewSSE(`{"type":"response.output_text.delta","item_id":"msg_text","output_index":1,"content_index":0,"delta":"Inspecting."}`)
			toolEvents := reviewSSE(reviewResponseEvents...)
			handler, records := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, choice) }, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, textEvent)
				w.(http.Flusher).Flush()
				// Exercise SSE and UTF-8 independent transport fragmentation.
				for _, part := range strings.Split(toolEvents, "") {
					_, _ = io.WriteString(w, part)
				}
			}, true)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Inspect only","stream":true}`)))
			body := response.Body.String()
			if !strings.HasPrefix(body, textEvent) {
				t.Fatalf("ordinary text lost: %s", body)
			}
			if choice == "pass" {
				if body != textEvent+toolEvents {
					t.Fatalf("passed stream changed: %s", body)
				}
			} else {
				if strings.Contains(body, "function_call") || strings.Contains(body, "danger") || strings.Contains(body, "response.completed") || !strings.Contains(body, "tool_review_blocked") {
					t.Fatalf("unsafe blocked stream: %s", body)
				}
				if records.latest(t).Status != contract.RequestStatusBlocked {
					t.Fatal("stream block not recorded")
				}
			}
		})
	}
}

func TestToolReviewFailureIsNotRiskAndDoesNotLeakJudgeBody(t *testing.T) {
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
		t.Run(string(mode), func(t *testing.T) {
			handler, records := reviewTestHandler(t, mode, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(401)
				_, _ = io.WriteString(w, "private judge diagnostic")
			}, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, reviewDangerousResponse)
			}, false)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Inspect only"}`)))
			var record contract.RequestRecord
			if mode == contract.ToolReviewModeMark {
				if response.Body.String() != reviewDangerousResponse {
					t.Fatal("mark failure changed response")
				}
				select {
				case record = <-records.updated:
				case <-time.After(2 * time.Second):
					t.Fatal("no review result")
				}
			} else {
				if response.Code != 503 || strings.Contains(response.Body.String(), "private judge") {
					t.Fatalf("response = %d %s", response.Code, response.Body.String())
				}
				record = records.latest(t)
			}
			encoded, _ := json.Marshal(record.ToolReview)
			if record.ToolReview == nil || record.ToolReview.Status != "failed" || strings.Contains(string(encoded), "private judge") {
				t.Fatalf("review = %s", encoded)
			}
		})
	}
}

func TestToolReviewUsesFullUserTextInsteadOfPreview(t *testing.T) {
	userText := strings.Repeat("a", 6000) + " final-user-instruction"
	seen := make(chan string, 1)
	handler, _ := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- string(body)
		sendReviewChoice(w, "pass")
	}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reviewDangerousResponse)
	}, false)
	body, _ := json.Marshal(map[string]any{"model": "gpt-5", "input": userText})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body))))
	if response.Code != 200 {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	select {
	case data := <-seen:
		if !strings.Contains(data, userText) {
			t.Fatal("review silently truncated user text")
		}
	default:
		t.Fatal("no review")
	}
}

func TestToolReviewResponsesWebSocketDoesNotPublishBlockedTool(t *testing.T) {
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }))
	defer judge.Close()
	provider := wsUpstream(t, func(conn *websocket.Conn, _ *http.Request) {
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Error(err)
			return
		}
		for _, payload := range reviewResponseEvents {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				return
			}
		}
	})
	settings := contract.DefaultToolReviewSettings()
	settings.Enabled, settings.CredentialConfigured, settings.Mode, settings.Endpoint = true, true, contract.ToolReviewModeReject, judge.URL
	handler := NewWithDependencies(Dependencies{
		Resolver:                 candidateResolver{candidates: []endpoint.Resolved{wsCandidate(provider.URL)}},
		AccessTokenAuthenticator: AccessTokenAuthenticatorFunc(func(context.Context, string) (contract.AccessTokenID, error) { return "token_review", nil }),
		ToolReviewSettings:       reviewSettingsSource{settings},
	})
	client := dialResponses(t, handler, http.Header{"Authorization": {"Bearer local"}})
	sendWS(t, client, `{"type":"response.create","model":"gpt-5","input":"Inspect only"}`)
	result := readWS(t, client)
	if result["type"] != "error" || !strings.Contains(fmt.Sprint(result), "tool_review_blocked") {
		t.Fatalf("websocket output = %#v", result)
	}
}
