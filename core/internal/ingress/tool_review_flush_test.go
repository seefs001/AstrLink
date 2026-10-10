package ingress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

// No Flusher methods: ResponseController must traverse this wrapper to observe
// the underlying error, even through recordStatusWriter/usageScanningWriter.
type reviewUnwrapOnly struct{ inner http.ResponseWriter }

func (w reviewUnwrapOnly) Header() http.Header            { return w.inner.Header() }
func (w reviewUnwrapOnly) WriteHeader(status int)         { w.inner.WriteHeader(status) }
func (w reviewUnwrapOnly) Write(body []byte) (int, error) { return w.inner.Write(body) }
func (w reviewUnwrapOnly) Unwrap() http.ResponseWriter    { return w.inner }

func TestToolReviewAskBufferedFlushFailureNeverConfirmsDelivery(t *testing.T) {
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
		for _, wrapped := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/direct", true: "/wrapped"}[wrapped], func(t *testing.T) {
				judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }))
				t.Cleanup(judge.Close)
				settings := contract.DefaultToolReviewSettings()
				settings.Enabled, settings.CredentialConfigured, settings.Mode, settings.Endpoint = true, true, contract.ToolReviewModeAsk, judge.URL
				records := newReviewRecords()
				broker := toolreview.NewBroker(context.Background())
				t.Cleanup(broker.Close)
				candidates := []endpoint.Resolved{{Endpoint: validEndpoint(tc.protocol, false)}}
				handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: candidates}, ToolReviewSettings: reviewSettingsSource{settings}, ToolReviewApprovals: broker, RequestRecords: records, Forwarder: transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(200, tc.response), nil }))})
				sink := reviewFlushFailure{httptest.NewRecorder()}
				var writer http.ResponseWriter = sink
				if wrapped {
					writer = reviewUnwrapOnly{inner: sink}
				}
				done := startAskRequest(t, handler, writer, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.input)))
				approval := waitIngressApproval(t, broker, done)
				if _, err := broker.Decide(approval.ID, "allow"); err != nil {
					t.Fatal(err)
				}
				waitIngressDone(t, done)
				record := records.latest(t)
				if sink.Body.Len() == 0 {
					t.Fatal("test never reached successful buffered Write")
				}
				if record.Status != contract.RequestStatusFailed || record.ToolReview.Status != "flagged" || record.ToolReview.Approval.State != "approved" || record.ToolReview.Calls[0].Action == "delivered" || record.Error == nil || record.Error.Code != "client_delivery_failed" {
					t.Fatalf("flush failure hidden: %s %+v error=%+v", record.Status, record.ToolReview, record.Error)
				}
				if tc.protocol == contract.ProtocolOpenAIResponses {
					if _, err := handler.bindResponseAffinity(context.Background(), Request{Protocol: tc.protocol, Model: "gpt-5", PreviousResponseID: "resp_review"}, candidates); err == nil {
						t.Fatal("buffered flush failure published affinity")
					}
				}
			})
		}
	}
}
