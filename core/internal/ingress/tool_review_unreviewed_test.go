package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

// Observe every upsert, not just the final record: an empty pending summary
// during preparation is also a spurious review, even if finish removes it.
type reviewMetadataObserver struct {
	*reviewRecords
	metadataMu  sync.Mutex
	wroteReview bool
}

func (store *reviewMetadataObserver) UpsertRequestRecord(ctx context.Context, record contract.RequestRecord) error {
	store.metadataMu.Lock()
	store.wroteReview = store.wroteReview || record.ToolReview != nil
	store.metadataMu.Unlock()
	return store.reviewRecords.UpsertRequestRecord(ctx, record)
}

func (store *reviewMetadataObserver) assertNoReview(t *testing.T) {
	t.Helper()
	store.metadataMu.Lock()
	defer store.metadataMu.Unlock()
	if store.wroteReview || store.latest(t).ToolReview != nil {
		t.Fatal("unreviewed request wrote tool review metadata")
	}
	select {
	case <-store.updated:
		t.Fatal("unreviewed request started a detached review")
	default:
	}
}

func TestToolReviewUnreviewedRequestsKeepOnlyOrdinaryRecords(t *testing.T) {
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range []struct {
				name, output        string
				httpError, disabled bool
			}{
				{name: "empty", output: `[]`},
				{name: "text", output: `[{"type":"message","id":"msg_text","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}]`},
				{name: "hosted-only", output: `[{"type":"web_search_call","id":"web_1","status":"completed"},{"type":"mcp_call","name":"remote","arguments":"{}"}]`},
				{name: "http400", httpError: true},
				{name: "disabled", disabled: true},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", mode, streaming, tc.name), func(t *testing.T) {
					body := `{"id":"resp_no_review","status":"completed","output":` + tc.output + `}`
					if tc.disabled {
						body = reviewDangerousResponse
					}
					if streaming {
						body = reviewSSE(`{"type":"response.completed","response":` + body + `}`)
					}
					status := http.StatusOK
					wantState := contract.RequestStatusSucceeded
					if tc.httpError {
						status, wantState = http.StatusBadRequest, contract.RequestStatusFailed
						body = `{"error":{"message":"bad request","type":"invalid_request_error"}}`
					}
					handler, records := reviewTestHandler(t, mode, func(http.ResponseWriter, *http.Request) {
						t.Error("unreviewed response reached judge")
					}, func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if streaming && !tc.httpError {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						w.WriteHeader(status)
						_, _ = io.WriteString(w, body)
					}, streaming)
					if tc.disabled {
						source := handler.toolReviewSettings.(reviewSettingsSource)
						source.settings.Enabled = false
						handler.toolReviewSettings = source
					}
					observed := &reviewMetadataObserver{reviewRecords: records}
					handler.requestRecords = observed
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5","input":"Hello","stream":%t}`, streaming))))
					if response.Code != status || response.Body.String() != body {
						t.Fatalf("delivery changed: %d %s", response.Code, response.Body.String())
					}
					if record := records.latest(t); record.ID == "" || record.Status != wantState {
						t.Fatalf("ordinary record lost or changed: %+v", record)
					}
					observed.assertNoReview(t)
					if !tc.httpError {
						responseID := "resp_no_review"
						if tc.disabled {
							responseID = "resp_review"
						}
						candidates := handler.resolver.(candidateResolver).candidates
						if _, err := handler.bindResponseAffinity(context.Background(), Request{Protocol: contract.ProtocolOpenAIResponses, Model: "gpt-5", PreviousResponseID: responseID}, candidates); err != nil {
							t.Fatalf("successful no-review response lost affinity: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestToolReviewUnstartedResponseHasNoSummary(t *testing.T) {
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
		t.Run(string(mode), func(t *testing.T) {
			handler, records := reviewTestHandler(t, mode, func(http.ResponseWriter, *http.Request) {
				t.Error("unstarted response reached judge")
			}, func(http.ResponseWriter, *http.Request) {}, false)
			observed := &reviewMetadataObserver{reviewRecords: records}
			handler.requestRecords = observed
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Hello"}`))
			session := handler.startRecordSession(request, Request{Protocol: contract.ProtocolOpenAIResponses})
			session.persistPending(request.Context(), observed, nil)
			request = request.WithContext(withRecordSession(request.Context(), session))
			writer, _, err := handler.prepareToolReview(httptest.NewRecorder(), request, session.classified)
			if err != nil || writer == nil {
				t.Fatalf("prepare = %v %v", writer, err)
			}
			defer writer.cancel(nil)
			handler.finishToolReview(writer, session)
			if session.toolReview != nil || writer.async != nil {
				t.Fatalf("unstarted response created review: %+v", session.toolReview)
			}
			observed.assertNoReview(t)
		})
	}
}

type reviewCancelRead struct {
	io.Reader
	cancel context.CancelFunc
}

func (reader reviewCancelRead) Read(p []byte) (int, error) {
	n, err := reader.Reader.Read(p)
	if err == io.EOF {
		reader.cancel()
		return n, context.Canceled
	}
	return n, err
}

func TestToolReviewCancellationBeforeAndAfterClientTool(t *testing.T) {
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
		for _, hasTool := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tool=%t", mode, hasTool), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				prefix := reviewSSE(`{"type":"response.output_text.delta","item_id":"msg_text","output_index":0,"content_index":0,"delta":"Hello"}`)
				if hasTool {
					prefix = reviewSSE(reviewResponseEvents[0])
				}
				handler, records := reviewTestHandler(t, mode, func(http.ResponseWriter, *http.Request) {
					t.Error("incomplete response reached judge")
				}, func(http.ResponseWriter, *http.Request) {}, true)
				observed := &reviewMetadataObserver{reviewRecords: records}
				handler.requestRecords = observed
				handler.forwarder = transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(reviewCancelRead{strings.NewReader(prefix), cancel})}, nil
				}))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Hello","stream":true}`)).WithContext(ctx))
				record := records.latest(t)
				if record.Status != contract.RequestStatusCancelled {
					t.Fatalf("cancellation lost: %+v", record)
				}
				if hasTool {
					if record.ToolReview == nil || record.ToolReview.Status != "cancelled" || record.ToolReview.RejectThreshold == nil {
						t.Fatalf("observed tool lost cancellation metadata: %+v", record.ToolReview)
					}
				} else {
					observed.assertNoReview(t)
				}
			})
		}
	}
}

func TestToolReviewBufferedCancellationKeepsOnlyActualReviewEvidence(t *testing.T) {
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
		for _, tc := range []struct {
			name, body string
			wantReview bool
		}{
			{"text", `{"id":"resp_text","status":"completed","output":[]}`, false},
			{"tool", reviewDangerousResponse, true},
			{"malformed", `{`, true},
		} {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				deliveredTool := mode == contract.ToolReviewModeMark && tc.name == "tool"
				handler, records := reviewTestHandler(t, mode, func(w http.ResponseWriter, _ *http.Request) {
					if !deliveredTool {
						t.Error("unreviewable buffered response reached judge")
					}
					sendReviewChoice(w, "pass")
				}, func(http.ResponseWriter, *http.Request) {}, false)
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Inspect only"}`))
				session := handler.startRecordSession(request, Request{Protocol: contract.ProtocolOpenAIResponses})
				request = request.WithContext(withRecordSession(request.Context(), session))
				writer, _, err := handler.prepareToolReview(httptest.NewRecorder(), request, session.classified)
				if err != nil || writer == nil {
					t.Fatalf("prepare = %v %v", writer, err)
				}
				defer writer.cancel(nil)
				if _, err := io.WriteString(writer, tc.body); err != nil {
					t.Fatal(err)
				}
				// The body was fully read, but the client cancels before finish.
				session.toolReviewEOF = true
				writer.cancel(context.Canceled)
				handler.finishToolReview(writer, session)
				if (session.toolReview != nil) != tc.wantReview || (writer.async != nil) != deliveredTool {
					t.Fatalf("cancelled response review = %+v", session.toolReview)
				}
				if deliveredTool {
					// Mark already delivered a complete tool; late cancellation must
					// not discard its detached review or label delivery withheld.
					session.persistPending(context.Background(), records, nil)
					writer.async()
					if review := records.latest(t).ToolReview; review == nil || review.Status != "passed" || len(review.Calls) != 1 || review.Calls[0].Action != "delivered" {
						t.Fatalf("delivered tool lost review after cancellation: %+v", review)
					}
					return
				}
				if tc.wantReview && (session.toolReview.Status != "cancelled" || session.toolReview.RejectThreshold == nil) {
					t.Fatalf("cancellation lost review evidence: %+v", session.toolReview)
				}
			})
		}
	}
}

func TestToolReviewCollectionErrorsWithoutSafeCallsRemainVisible(t *testing.T) {
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
		for _, streaming := range []bool{false, true} {
			for _, payload := range []string{`{`, `{"id":"resp_unknown","status":"completed","output":[{"type":"unknown_tool_call","name":"unsafe"}]}`} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", mode, streaming, payload), func(t *testing.T) {
					body := payload
					if streaming {
						if json.Valid([]byte(payload)) {
							body = `{"type":"response.completed","response":` + payload + `}`
						}
						body = reviewSSE(body)
					}
					handler, records := reviewTestHandler(t, mode, func(http.ResponseWriter, *http.Request) {
						t.Error("invalid response reached judge")
					}, func(w http.ResponseWriter, _ *http.Request) {
						if streaming {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						_, _ = io.WriteString(w, body)
					}, streaming)
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5","input":"Hello","stream":%t}`, streaming))))
					review := records.latest(t).ToolReview
					if review == nil || review.Status != "failed" || review.ErrorCode == "" || review.RejectThreshold == nil {
						t.Fatalf("collection failure silently discarded: %+v", review)
					}
					if mode == contract.ToolReviewModeReject {
						if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "unsafe") || !strings.Contains(response.Body.String(), "tool_review_unavailable") {
							t.Fatalf("collection failure did not fail closed: %d %s", response.Code, response.Body.String())
						}
					} else if response.Body.String() != body {
						t.Fatal("mark collection failure changed response")
					}
				})
			}
		}
	}
}
