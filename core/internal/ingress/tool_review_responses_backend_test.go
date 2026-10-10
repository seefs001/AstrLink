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

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func sendStructuredReviewFlag(w http.ResponseWriter) {
	text, _ := json.Marshal(map[string]any{"results": []any{map[string]any{
		"index": 0, "verdict": "flag", "reason_code": "destructive_action",
		"pass_probability": 0.01, "flag_probability": 0.99,
	}}})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "resp_structured_review", "object": "response", "status": "completed", "model": "structured-returned-version",
		"error": nil, "incomplete_details": nil,
		"output": []any{map[string]any{
			"id": "msg_structured_review", "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": string(text), "annotations": []any{}}},
		}},
	})
}

func TestToolReviewResponsesBackendApprovalGate(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, batch := range []bool{false, true} {
			for _, decision := range []string{"allow", "deny"} {
				t.Run(fmt.Sprintf("stream=%t/batch=%t/%s", streaming, batch, decision), func(t *testing.T) {
					var calls atomic.Int32
					h, _ := reviewTestHandler(t, contract.ToolReviewModeAsk, func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test-review-key" {
							t.Error("Responses review did not use the configured endpoint/credential")
						}
						var body struct {
							Model string `json:"model"`
						}
						if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "structured-review-model" {
							t.Error("Responses review did not use the configured model")
						}
						sendStructuredReviewFlag(w)
					}, func(w http.ResponseWriter, _ *http.Request) {
						if streaming {
							w.Header().Set("Content-Type", "text/event-stream")
							for _, event := range reviewResponseEvents {
								_, _ = io.WriteString(w, "data: "+event+"\n\n")
							}
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, reviewDangerousResponse)
						}
					}, streaming)
					broker := toolreview.NewBroker(context.Background())
					t.Cleanup(broker.Close)
					h.toolReviewApprovals = broker
					source := h.toolReviewSettings.(reviewSettingsSource)
					source.settings.Backend, source.settings.BatchCalls = contract.ToolReviewBackendResponses, batch
					source.settings.Endpoint += "/v1/responses"
					source.settings.Model = "structured-review-model"
					h.toolReviewSettings = source
					records := &approvalRecords{reviewRecords: newReviewRecords()}
					h.requestRecords = records
					response := newAskRecorder()
					done := startAskRequest(t, h, response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"m","input":"inspect synthetic data","stream":%t}`, streaming))))
					item := waitIngressApproval(t, broker, done)
					if item.Summary.Backend != contract.ToolReviewBackendResponses || item.Summary.BatchCalls == nil || *item.Summary.BatchCalls != batch || item.Summary.Approval.ReviewStatus != "flagged" {
						t.Fatalf("pending metadata lost backend/strategy: %+v", item.Summary)
					}
					if strings.Contains(response.body(), "function_call") {
						t.Fatal("client tool escaped before permission")
					}
					// A later backend selection must not mutate the in-flight snapshot.
					next := source
					next.settings.Backend, next.settings.Revision = contract.ToolReviewBackendJev, "next-backend"
					h.toolReviewSettings = next
					if _, err := broker.Decide(item.ID, decision); err != nil {
						t.Fatal(err)
					}
					waitIngressDone(t, done)
					record := records.latest(t)
					summary := record.ToolReview
					if summary == nil || summary.Backend != contract.ToolReviewBackendResponses || summary.Status != "flagged" || summary.ConfigRevision != "test-v1" || summary.Approval == nil || summary.Calls[0].Model != "structured-returned-version" {
						t.Fatalf("final metadata changed review identity: %+v", summary)
					}
					if decision == "allow" {
						if summary.Approval.State != "approved" || summary.Calls[0].Action != "delivered" || record.Status != contract.RequestStatusSucceeded || !strings.Contains(response.body(), "function_call") {
							t.Fatalf("approval failed to release: %+v", summary)
						}
					} else if summary.Approval.State != "denied" || summary.Calls[0].Action != "withheld" || record.Status == contract.RequestStatusSucceeded || strings.Contains(response.body(), "function_call") {
						t.Fatalf("denial released: %+v", summary)
					}
					if calls.Load() != 1 || len(broker.List()) != 0 {
						t.Fatal("review retried or approval leaked")
					}
				})
			}
		}
	}
}

func TestToolReviewResponsesBackendReviewFailureRequiresOptIn(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(fmt.Sprintf("ask_on_error=%t", optIn), func(t *testing.T) {
			h, _ := reviewTestHandler(t, contract.ToolReviewModeAsk, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "synthetic-reviewer-private-error", http.StatusTooManyRequests)
			}, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, reviewDangerousResponse)
			}, false)
			broker := toolreview.NewBroker(context.Background())
			t.Cleanup(broker.Close)
			h.toolReviewApprovals = broker
			source := h.toolReviewSettings.(reviewSettingsSource)
			source.settings.Backend, source.settings.BatchCalls, source.settings.AskOnReviewError = contract.ToolReviewBackendResponses, true, optIn
			source.settings.Endpoint += "/v1/responses"
			h.toolReviewSettings = source
			records := &approvalRecords{reviewRecords: newReviewRecords()}
			h.requestRecords = records
			response := newAskRecorder()
			done := startAskRequest(t, h, response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)))
			if optIn {
				item := waitIngressApproval(t, broker, done)
				if item.Summary.Approval.ReviewStatus != "failed" {
					t.Fatal("failed review masqueraded as a flag")
				}
				if _, err := broker.Decide(item.ID, "allow"); err != nil {
					t.Fatal(err)
				}
			}
			waitIngressDone(t, done)
			summary := records.latest(t).ToolReview
			if summary == nil || summary.Backend != contract.ToolReviewBackendResponses || summary.Status != "failed" || summary.Calls[0].ErrorCode != "rate_limited" {
				t.Fatalf("failure summary=%+v", summary)
			}
			if optIn {
				if summary.Approval == nil || summary.Approval.State != "approved" || summary.Calls[0].Action != "delivered" {
					t.Fatal("explicit failure approval did not deliver")
				}
			} else if summary.Approval != nil || strings.Contains(response.body(), "function_call") || len(broker.List()) != 0 {
				t.Fatal("review error bypassed opt-in")
			}
			data, _ := json.Marshal(summary)
			if strings.Contains(string(data), "synthetic-reviewer-private-error") {
				t.Fatal("raw reviewer error persisted")
			}
		})
	}
}
