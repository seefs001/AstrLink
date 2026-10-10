package ingress

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

func TestToolReviewNativeStreamProtocols(t *testing.T) {
	cases := []struct {
		protocol    contract.ProtocolID
		path, input string
	}{
		{contract.ProtocolOpenAIChat, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"Inspect only"}]}`},
		{contract.ProtocolAnthropicMessages, "/v1/messages", `{"model":"m","stream":true,"messages":[{"role":"user","content":"Inspect only"}]}`},
		{contract.ProtocolGoogleGenerateContent, "/v1beta/models/m:streamGenerateContent?alt=sse", `{"contents":[{"role":"user","parts":[{"text":"Inspect only"}]}]}`},
	}
	for _, tc := range cases {
		for _, choice := range []string{"pass", "flag"} {
			t.Run(string(tc.protocol)+"/"+choice, func(t *testing.T) {
				judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, choice) }))
				defer judge.Close()
				settings := contract.DefaultToolReviewSettings()
				settings.Enabled = true
				settings.CredentialConfigured = true
				settings.Mode = contract.ToolReviewModeReject
				settings.Endpoint = judge.URL
				upstream := relayKitCustomToolUpstream(t, tc.protocol, true, relayKitCustomToolPatch)
				expected, _ := io.ReadAll(upstream.Body)
				upstream.Body = io.NopCloser(strings.NewReader(string(expected)))
				handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{{Endpoint: validEndpoint(tc.protocol, true)}}}, ToolReviewSettings: reviewSettingsSource{settings}, Forwarder: transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) { return upstream, nil }))})
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.input)))
				body := response.Body.String()
				if choice == "pass" {
					if body != string(expected) {
						t.Fatalf("passed stream changed: %s", body)
					}
				} else if strings.Contains(body, "apply_patch") || strings.Contains(body, "Begin Patch") || !strings.Contains(body, "tool_review_blocked") {
					t.Fatalf("blocked stream=%s", body)
				}
			})
		}
	}
}

func TestToolReviewGeminiJSONArrayStreamPreservesEnvelope(t *testing.T) {
	original := `[{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Inspecting."}]}}]}, {"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"bash","args":{"command":"danger"}}}]}}]}, {"candidates":[{"index":0,"content":{"role":"model","parts":[]},"finishReason":"STOP"}]}]`
	for _, choice := range []string{"pass", "flag"} {
		t.Run(choice, func(t *testing.T) {
			judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, choice) }))
			defer judge.Close()
			settings := contract.DefaultToolReviewSettings()
			settings.Enabled = true
			settings.CredentialConfigured = true
			settings.Mode = contract.ToolReviewModeReject
			settings.Endpoint = judge.URL
			handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{{Endpoint: validEndpoint(contract.ProtocolGoogleGenerateContent, true)}}}, ToolReviewSettings: reviewSettingsSource{settings}, Forwarder: transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(200, original), nil }))})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1beta/models/m:streamGenerateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"Inspect only"}]}]}`)))
			body := response.Body.String()
			if !json.Valid([]byte(body)) {
				t.Fatalf("invalid JSON envelope: %s", body)
			}
			if choice == "pass" {
				if body != original {
					t.Fatalf("passed stream changed: %s", body)
				}
			} else if strings.Contains(body, "danger") || !strings.Contains(body, "tool_review_blocked") {
				t.Fatalf("blocked stream=%s", body)
			}
		})
	}
}

func TestToolReviewEarlyToolSnapshotsNeverLeak(t *testing.T) {
	cases := []struct {
		name                string
		protocol            contract.ProtocolID
		path, input, stream string
	}{
		{"responses_created", contract.ProtocolOpenAIResponses, "/v1/responses", `{"model":"m","input":"Inspect only","stream":true}`, reviewSSE(`{"type":"response.created","response":{"id":"resp_early","status":"in_progress","output":[{"id":"fc_early","type":"function_call","call_id":"call_early","name":"danger_tool","arguments":"{}"}]}}`, `{"type":"response.completed","response":{"id":"resp_early","status":"completed","output":[{"id":"fc_early","type":"function_call","call_id":"call_early","name":"danger_tool","arguments":"{}"}]}}`)},
		{"anthropic_start", contract.ProtocolAnthropicMessages, "/v1/messages", `{"model":"m","messages":[{"role":"user","content":"Inspect only"}],"stream":true}`, reviewSSE(`{"type":"message_start","message":{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"tu_early","name":"danger_tool","input":{}}]}}`, `{"type":"message_stop"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }))
			defer judge.Close()
			settings := contract.DefaultToolReviewSettings()
			settings.Enabled = true
			settings.CredentialConfigured = true
			settings.Mode = contract.ToolReviewModeReject
			settings.Endpoint = judge.URL
			handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{{Endpoint: validEndpoint(tc.protocol, true)}}}, ToolReviewSettings: reviewSettingsSource{settings}, Forwarder: transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.stream))}, nil
			}))})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.input)))
			body := response.Body.String()
			if strings.Contains(body, "danger_tool") || (!strings.Contains(body, "tool_review_blocked") && !strings.Contains(body, "tool_review_unavailable")) {
				t.Fatalf("early snapshot leaked: %s", body)
			}
		})
	}
}
