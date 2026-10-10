package ingress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

func TestToolReviewSSEWithoutContentType(t *testing.T) {
	responsesStream := reviewSSE(
		`{"type":"response.created","response":{"status":"in_progress","output":[]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_test","type":"custom_tool_call","call_id":"call_test","name":"inspect","input":""}}`,
		`{"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"ctc_test","delta":"inspect project"}`,
		`{"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"ctc_test","input":"inspect project"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"ctc_test","type":"custom_tool_call","call_id":"call_test","name":"inspect","input":"inspect project","status":"completed"}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"id":"ctc_test","type":"custom_tool_call","call_id":"call_test","name":"inspect","input":"inspect project","status":"completed"}]}}`,
	)
	for _, tc := range []struct {
		protocol                  contract.ProtocolID
		path, input, name, secret string
	}{
		{contract.ProtocolOpenAIResponses, "/v1/responses", `{"model":"m","input":"Inspect project","stream":true}`, "inspect", "inspect project"},
		{contract.ProtocolOpenAIChat, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"Inspect project"}],"stream":true}`, "apply_patch", "Begin Patch"},
		{contract.ProtocolAnthropicMessages, "/v1/messages", `{"model":"m","messages":[{"role":"user","content":"Inspect project"}],"stream":true}`, "apply_patch", "Begin Patch"},
	} {
		stream := responsesStream
		if tc.protocol != contract.ProtocolOpenAIResponses {
			upstream := relayKitCustomToolUpstream(t, tc.protocol, true, relayKitCustomToolPatch)
			body, err := io.ReadAll(upstream.Body)
			_ = upstream.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			stream = string(body)
		}
		for _, contentType := range []string{"", "application/octet-stream", "text/event-stream"} {
			for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
				t.Run(string(tc.protocol)+"/"+contentType+"/"+string(mode), func(t *testing.T) {
					var reviews atomic.Int32
					judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						reviews.Add(1)
						sendReviewChoice(w, "flag")
					}))
					defer judge.Close()
					settings := contract.DefaultToolReviewSettings()
					settings.Enabled, settings.CredentialConfigured = true, true
					settings.Mode, settings.Endpoint = mode, judge.URL
					records := newReviewRecords()
					handler := NewWithDependencies(Dependencies{
						Resolver:           candidateResolver{candidates: []endpoint.Resolved{{Endpoint: validEndpoint(tc.protocol, true)}}},
						ToolReviewSettings: reviewSettingsSource{settings},
						RequestRecords:     records,
						Forwarder: transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) {
							header := make(http.Header)
							if contentType != "" {
								header.Set("Content-Type", contentType)
							}
							return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(stream))}, nil
						})),
					})
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.input)).WithContext(ctx))
					record := records.latest(t)
					if record.ToolReview == nil || record.ToolReview.Status == "failed" {
						t.Fatalf("review never reached Jev: %#v; Jev calls=%d", record.ToolReview, reviews.Load())
					}
					if mode == contract.ToolReviewModeMark {
						select {
						case record = <-records.updated:
						case <-ctx.Done():
							t.Fatal("mark review did not complete")
						}
						if record.Status != contract.RequestStatusSucceeded || record.ToolReview.Status != "flagged" || response.Body.String() != stream {
							t.Fatalf("mark delivery/review changed: %#v", record.ToolReview)
						}
					} else if record.Status != contract.RequestStatusBlocked || record.ToolReview.Status != "blocked" || strings.Contains(response.Body.String(), tc.secret) || !strings.Contains(response.Body.String(), "tool_review_blocked") {
						t.Fatalf("reject delivery/review incorrect: %#v; body=%s", record.ToolReview, response.Body.String())
					}
					if reviews.Load() != 1 || len(record.ToolReview.Calls) != 1 || record.ToolReview.Calls[0].Name != tc.name {
						t.Fatalf("missing tool review: %#v; Jev calls=%d", record.ToolReview, reviews.Load())
					}
				})
			}
		}
	}
}
