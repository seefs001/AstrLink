package ingress

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

func TestToolReviewWaitsForAllRequestedCandidates(t *testing.T) {
	chat0 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c0","type":"function","function":{"name":"safe","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	chat1 := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(chat0, `"index":0,"delta"`, `"index":1,"delta"`), `"c0"`, `"c1"`), `"safe"`, `"danger"`)
	gemini0 := `{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"safe","args":{}}}]},"finishReason":"STOP"}]}`
	gemini1 := strings.ReplaceAll(strings.ReplaceAll(gemini0, `"index":0`, `"index":1`), `"safe"`, `"danger"`)
	chatJSON := strings.ReplaceAll(strings.TrimSuffix(chat0, "]}")+","+strings.TrimPrefix(chat1, `{"choices":[`), `"delta"`, `"message"`)
	geminiJSON := strings.TrimSuffix(gemini0, "]}") + "," + strings.TrimPrefix(gemini1, `{"candidates":[`)
	cases := []struct {
		name, path, input, stream, contentType string
		protocol                               contract.ProtocolID
		missing                                bool
	}{
		{"chat JSON", "/v1/chat/completions", `{"model":"m","n":2,"messages":[{"role":"user","content":"inspect"}]}`, chatJSON, "application/json", contract.ProtocolOpenAIChat, false},
		{"gemini JSON", "/v1beta/models/m:generateContent", `{"generationConfig":{"candidateCount":2},"contents":[{"role":"user","parts":[{"text":"inspect"}]}]}`, geminiJSON, "application/json", contract.ProtocolGoogleGenerateContent, false},
		{"chat", "/v1/chat/completions", `{"model":"m","stream":true,"n":2,"messages":[{"role":"user","content":"inspect"}]}`, reviewSSE(chat0, chat1, "[DONE]"), "text/event-stream", contract.ProtocolOpenAIChat, false},
		{"chat missing", "/v1/chat/completions", `{"model":"m","stream":true,"n":2,"messages":[{"role":"user","content":"inspect"}]}`, reviewSSE(chat0, "[DONE]"), "text/event-stream", contract.ProtocolOpenAIChat, true},
		{"gemini SSE", "/v1beta/models/m:streamGenerateContent?alt=sse", `{"generationConfig":{"candidateCount":2},"contents":[{"role":"user","parts":[{"text":"inspect"}]}]}`, reviewSSE(gemini0, gemini1), "text/event-stream", contract.ProtocolGoogleGenerateContent, false},
		{"gemini array", "/v1beta/models/m:streamGenerateContent", `{"generationConfig":{"candidateCount":2},"contents":[{"role":"user","parts":[{"text":"inspect"}]}]}`, "[" + gemini0 + "," + gemini1 + "]", "application/json", contract.ProtocolGoogleGenerateContent, false},
		{"gemini missing", "/v1beta/models/m:streamGenerateContent?alt=sse", `{"generationConfig":{"candidateCount":2},"contents":[{"role":"user","parts":[{"text":"inspect"}]}]}`, reviewSSE(gemini0), "text/event-stream", contract.ProtocolGoogleGenerateContent, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reviews atomic.Int32
			judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				reviews.Add(1)
				choice := "pass"
				if strings.Contains(string(data), "danger") {
					choice = "flag"
				}
				sendReviewChoice(w, choice)
			}))
			defer judge.Close()
			settings := contract.DefaultToolReviewSettings()
			settings.Enabled, settings.CredentialConfigured, settings.Mode, settings.Endpoint = true, true, contract.ToolReviewModeReject, judge.URL
			records := newReviewRecords()
			handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{{Endpoint: validEndpoint(tc.protocol, true)}}}, ToolReviewSettings: reviewSettingsSource{settings}, RequestRecords: records, Forwarder: transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.stream))}, nil
			}))})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.input)))
			wantStatus, wantReviews := "blocked", int32(2)
			if tc.missing {
				wantStatus, wantReviews = "failed", 0
			}
			if tc.protocol == contract.ProtocolGoogleGenerateContent {
				var failure struct {
					Error struct {
						Code    int `json:"code"`
						Details []struct {
							Reason string `json:"reason"`
						} `json:"details"`
					} `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Error.Code != response.Code || len(failure.Error.Details) != 1 || !strings.HasPrefix(failure.Error.Details[0].Reason, "tool_review_") {
					t.Errorf("Google error must retain numeric code and safe reason: %s err=%v", response.Body.String(), err)
				}
			}
			record := records.latest(t)
			if record.ToolReview == nil || record.ToolReview.Status != wantStatus || reviews.Load() != wantReviews || strings.Contains(response.Body.String(), `"name"`) {
				t.Fatalf("candidate batch leaked or ended early: review=%+v reviews=%d response=%s", record.ToolReview, reviews.Load(), response.Body.String())
			}
		})
	}
}

type reviewReadFailure struct{ *strings.Reader }

func (r reviewReadFailure) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func TestToolReviewChatNormalEOFOnly(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{true: "read error", false: "normal EOF"}[interrupted], func(t *testing.T) {
			var reviews atomic.Int32
			judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reviews.Add(1); sendReviewChoice(w, "pass") }))
			defer judge.Close()
			settings := contract.DefaultToolReviewSettings()
			settings.Enabled, settings.CredentialConfigured, settings.Mode, settings.Endpoint = true, true, contract.ToolReviewModeReject, judge.URL
			upstream := relayKitCustomToolUpstream(t, contract.ProtocolOpenAIChat, true, "inspect")
			data, _ := io.ReadAll(upstream.Body)
			data = []byte(strings.ReplaceAll(string(data), "data: [DONE]\n\n", ""))
			var reader io.Reader = strings.NewReader(string(data))
			if interrupted {
				reader = reviewReadFailure{strings.NewReader(string(data))}
			}
			upstream.Body = io.NopCloser(reader)
			records := newReviewRecords()
			handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{{Endpoint: validEndpoint(contract.ProtocolOpenAIChat, true)}}}, ToolReviewSettings: reviewSettingsSource{settings}, RequestRecords: records, Forwarder: transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) { return upstream, nil }))})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"inspect"}]}`)))
			record := records.latest(t)
			if interrupted {
				if reviews.Load() != 0 || strings.Contains(response.Body.String(), "apply_patch") || record.ToolReview.Status != "failed" {
					t.Fatalf("read failure approved: response=%s review=%+v", response.Body.String(), record.ToolReview)
				}
			} else if response.Body.String() != string(data) || reviews.Load() != 1 || record.Status != contract.RequestStatusSucceeded || record.ToolReview.Status != "passed" {
				t.Fatalf("normal EOF rejected: response=%s review=%+v", response.Body.String(), record.ToolReview)
			}
		})
	}
}
