package ingress

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/builtintools"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
)

func TestToolReviewMappedCodexTurnRequiresSuccessfulClientDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, choice, errorCode string
		writeFails, allowImage  bool
	}{
		{"passed", "pass", "", false, true},
		{"flagged", "flag", "tool_review_blocked", false, false},
		{"unavailable", "", "tool_review_unavailable", false, false},
		{"delivery_failed", "pass", "", true, false},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streaming), func(t *testing.T) {
				var imageCalls atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/responses"):
						writeResponsesSSE(w, "gpt-5.6-sol", codexFunctionCall("", "client_tool", "call_client", builtintools.Object{"command": "client-command-marker"}))
					case strings.HasSuffix(r.URL.Path, "/images/generations"):
						imageCalls.Add(1)
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"aW1hZ2U="}]}`)
					default:
						t.Errorf("unexpected upstream path %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer provider.Close()
				judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if tc.choice == "" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					sendReviewChoice(w, tc.choice)
				}))
				defer judge.Close()
				settings := contract.DefaultToolReviewSettings()
				settings.Enabled, settings.CredentialConfigured = true, true
				settings.Mode, settings.Endpoint = contract.ToolReviewModeReject, judge.URL
				store := newRedirectSettingsStore()
				store.settings.BuiltinTools = &contract.BuiltinTools{WebSearch: contract.BuiltinTool{Enabled: true, Backend: "external", BaseURL: provider.URL}}
				handler := NewWithDependencies(Dependencies{
					Resolver:           codexToolResolver{candidateResolver{candidates: []endpoint.Resolved{codexToolSubscription(provider.URL)}}},
					Authorizer:         endpoint.NewServiceAuthorizer(nil, codingPlanCredentials{}, accountauth.CodexIdentityPolicy{}),
					RequestRecords:     store,
					ProxyCredentials:   builtinToolSecrets{},
					ToolReviewSettings: reviewSettingsSource{settings},
				})
				turn := codexClientRequest("/v1/responses", fmt.Sprintf(`{"model":"gpt-5.6-sol","input":"Inspect only","stream":%t,"tools":[{"type":"web_search"},{"type":"function","name":"client_tool","parameters":{"type":"object"}}],"client_metadata":{"turn_id":"%s","session_id":"%s"}}`, streaming, codexTestTurn, codexTestSession), true)
				turn.Header.Set("Session-Id", codexTestSession)
				response := httptest.NewRecorder()
				if tc.writeFails {
					handler.ServeHTTP(&reviewBrokenWriter{header: make(http.Header)}, turn)
				} else {
					handler.ServeHTTP(response, turn)
					if tc.errorCode != "" && (!strings.Contains(response.Body.String(), tc.errorCode) || strings.Contains(response.Body.String(), "client-command-marker")) {
						t.Fatalf("review did not withhold the client tool: %d %s", response.Code, response.Body.String())
					}
					if tc.allowImage && (response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "client-command-marker")) {
						t.Fatalf("approved tool was not delivered: %d %s", response.Code, response.Body.String())
					}
				}
				image := codexClientRequest("/v1/images/generations", `{"prompt":"a red fox","model":"gpt-image-2"}`, true)
				image.Header.Set(codexImageTurnHeader, codexTestTurn)
				response = httptest.NewRecorder()
				handler.ServeHTTP(response, image)
				wantStatus, wantCalls := http.StatusUnprocessableEntity, int32(0)
				if tc.allowImage {
					wantStatus, wantCalls = http.StatusOK, 1
				}
				if response.Code != wantStatus || imageCalls.Load() != wantCalls {
					t.Fatalf("mapped turn publication disagrees with client delivery: status=%d upstream calls=%d body=%s", response.Code, imageCalls.Load(), response.Body.String())
				}
			})
		}
	}
}
