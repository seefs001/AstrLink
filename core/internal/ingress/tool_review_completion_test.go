package ingress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/relaykitbridge"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

func TestToolReviewChatConversionFinalizerIsSuccessful(t *testing.T) {
	for _, protocol := range []contract.ProtocolID{contract.ProtocolAnthropicMessages, contract.ProtocolGoogleGenerateContent} {
		t.Run(string(protocol), func(t *testing.T) {
			judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "pass") }))
			defer judge.Close()
			settings := contract.DefaultToolReviewSettings()
			settings.Enabled, settings.CredentialConfigured, settings.Mode, settings.Endpoint = true, true, contract.ToolReviewModeReject, judge.URL
			records := newReviewRecords()
			handler := NewWithDependencies(Dependencies{
				Resolver:         candidateResolver{candidates: []endpoint.Resolved{relayKitCandidate("service_review", protocol, "m")}},
				ConversionEngine: relaykitbridge.NewEngine(), ToolReviewSettings: reviewSettingsSource{settings}, RequestRecords: records,
				Forwarder: transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) {
					return relayKitCustomToolUpstream(t, protocol, true, relayKitCustomToolPatch), nil
				})),
			})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"add file"}],"tools":[{"type":"function","function":{"name":"apply_patch","parameters":{"type":"object"}}}]}`)))
			record := records.latest(t)
			if response.Code != 200 || !strings.Contains(response.Body.String(), "[DONE]") || !strings.Contains(response.Body.String(), "apply_patch") || record.Status != contract.RequestStatusSucceeded || record.ToolReview == nil || record.ToolReview.Status != "passed" {
				t.Fatalf("response=%d %s status=%s review=%+v error=%+v", response.Code, response.Body.String(), record.Status, record.ToolReview, record.Error)
			}
		})
	}
}

func TestToolReviewNonStreamingReadFailureNeverReleasesTools(t *testing.T) {
	var reviews int
	handler, records := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, _ *http.Request) { reviews++; sendReviewChoice(w, "pass") }, func(http.ResponseWriter, *http.Request) {}, false)
	handler.forwarder = transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(reviewReadFailure{strings.NewReader(reviewDangerousResponse)})}, nil
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)))
	if response.Code != 503 || reviews != 0 || strings.Contains(response.Body.String(), "function_call") || records.latest(t).ToolReview.Status != "failed" {
		t.Fatalf("read failure approved: response=%d %s review=%+v", response.Code, response.Body.String(), records.latest(t).ToolReview)
	}
}

type reviewCancelAfterWrite struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w reviewCancelAfterWrite) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	if strings.Contains(string(data), "response.completed") {
		w.cancel()
	}
	return n, err
}

func TestToolReviewNoToolsDisconnectAfterTerminalStaysSuccessful(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler, records := reviewTestHandler(t, contract.ToolReviewModeReject, func(http.ResponseWriter, *http.Request) { t.Error("no-tool response reached judge") }, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reviewSSE(`{"type":"response.completed","response":{"id":"resp_text","status":"completed","output":[]}}`))
	}, true)
	response := reviewCancelAfterWrite{httptest.NewRecorder(), cancel}
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect","stream":true}`)).WithContext(ctx))
	record := records.latest(t)
	if record.Status != contract.RequestStatusSucceeded || record.ToolReview.Status != "not_applicable" {
		t.Fatalf("delivered terminal downgraded: status=%s review=%+v", record.Status, record.ToolReview)
	}
}

type reviewFlushFailure struct{ *httptest.ResponseRecorder }

func (w reviewFlushFailure) FlushError() error { return io.ErrClosedPipe }

func TestToolReviewMarkDoesNotHideFlushFailure(t *testing.T) {
	handler, records := reviewTestHandler(t, contract.ToolReviewModeMark, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "pass") }, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reviewSSE(reviewResponseEvents...))
	}, true)
	response := reviewFlushFailure{httptest.NewRecorder()}
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect","stream":true}`)))
	record := records.latest(t)
	if record.Status != contract.RequestStatusFailed || record.ToolReview == nil || record.ToolReview.Status != "failed" {
		t.Fatalf("flush failure hidden: status=%s review=%+v", record.Status, record.ToolReview)
	}
}

type reviewBrokenWriter struct {
	header  http.Header
	written int
	zero    bool
	short   bool
}

func (w *reviewBrokenWriter) Header() http.Header { return w.header }
func (w *reviewBrokenWriter) WriteHeader(int)     {}
func (w *reviewBrokenWriter) Write(b []byte) (int, error) {
	if w.zero {
		return 0, io.ErrClosedPipe
	}
	w.written += min(8, len(b))
	if w.short {
		return min(8, len(b)), nil
	}
	return min(8, len(b)), io.ErrClosedPipe
}

func TestToolReviewDeliveryFailureDoesNotClaimDelivered(t *testing.T) {
	for _, failure := range []string{"partial", "zero", "short"} {
		t.Run(failure, func(t *testing.T) {
			handler, records := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "pass") }, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, reviewDangerousResponse)
			}, false)
			response := &reviewBrokenWriter{header: make(http.Header), zero: failure == "zero", short: failure == "short"}
			handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)))
			record := records.latest(t)
			wantAction := "pending"
			if failure == "zero" {
				wantAction = "withheld"
			}
			if record.Status != contract.RequestStatusFailed || record.ToolReview == nil || len(record.ToolReview.Calls) != 1 || record.ToolReview.Calls[0].Action != wantAction {
				t.Fatalf("partial delivery falsely confirmed: status=%s review=%+v", record.Status, record.ToolReview)
			}
			// Even an upstream response ID observed before the failed write must not pin a later turn.
			if _, err := handler.bindResponseAffinity(context.Background(), Request{Protocol: contract.ProtocolOpenAIResponses, Model: "m", PreviousResponseID: "resp_review"}, handler.resolver.(candidateResolver).candidates); err == nil {
				t.Fatal("failed delivery created affinity")
			}
		})
	}
}

func TestToolReviewMarkDeliversBytesAfterProtocolTerminal(t *testing.T) {
	handler, records := reviewTestHandler(t, contract.ToolReviewModeMark, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "pass") }, func(http.ResponseWriter, *http.Request) {}, true)
	// Separate reads deliver the trailing frame after the collector's terminal.
	handler.forwarder = transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := io.MultiReader(strings.NewReader(reviewSSE(reviewResponseEvents...)), strings.NewReader("data: [DONE]\n\n"))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(body)}, nil
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect","stream":true}`)))
	want := reviewSSE(reviewResponseEvents...) + "data: [DONE]\n\n"
	if response.Code != 200 || response.Body.String() != want {
		t.Fatalf("mark mode changed delivery: %d %q", response.Code, response.Body.String())
	}
	select {
	case record := <-records.updated:
		if record.Status != contract.RequestStatusSucceeded || record.ToolReview == nil || record.ToolReview.Status != "passed" {
			t.Fatalf("record = %s %+v", record.Status, record.ToolReview)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("review record not completed")
	}
}
