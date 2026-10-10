package ingress

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

func TestToolReviewRejectPendingContainsSafeCallsAndThreshold(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	handler, records := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			sendReviewChoice(w, "pass")
		case <-r.Context().Done():
		}
	}, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, reviewDangerousResponse)
	}, false)
	source := handler.toolReviewSettings.(reviewSettingsSource)
	source.settings.RejectThreshold = 0.9
	handler.toolReviewSettings = source
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Inspect only"}`)))
		close(done)
	}()
	select {
	case <-started:
	case <-done:
		t.Fatalf("request returned before judge: %d %s", response.Code, response.Body.String())
	case <-time.After(2 * time.Second):
		t.Fatal("review did not start")
	}
	pending := records.latest(t).ToolReview
	if pending == nil || pending.Status != "pending" || pending.CompletedAt != nil || pending.BatchCalls == nil || *pending.BatchCalls || pending.RejectThreshold == nil || *pending.RejectThreshold != 0.9 || len(pending.Calls) != 1 || pending.Calls[0].Index != 0 || pending.Calls[0].Name != "bash" || pending.Calls[0].Action != "pending" {
		t.Fatalf("review in progress not exposed: %+v", pending)
	}
	if err := pending.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(pending)
	if err != nil || strings.Contains(string(encoded), "remove-important-data") {
		t.Fatalf("pending argument leak: %s err=%v", encoded, err)
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish")
	}
	if response.Code != http.StatusOK || records.latest(t).ToolReview.Status != "passed" {
		t.Fatalf("response=%d summary=%+v", response.Code, records.latest(t).ToolReview)
	}
}

func TestToolReviewIngressOutcomesKeepThresholdSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, body, status string
	}{
		{"no tools", `{"id":"resp_text","status":"completed","output":[]}`, ""},
		{"invalid upstream", `{`, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, records := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, _ *http.Request) {
				t.Error("Jev invoked without a complete tool")
				w.WriteHeader(http.StatusInternalServerError)
			}, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}, false)
			source := handler.toolReviewSettings.(reviewSettingsSource)
			source.settings.RejectThreshold = 0.75
			source.settings.BatchCalls = true
			handler.toolReviewSettings = source
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Inspect only"}`)))
			summary := records.latest(t).ToolReview
			if tc.status == "" {
				if summary != nil {
					t.Fatalf("unreviewed response created metadata: %+v", summary)
				}
				return
			}
			if summary == nil || summary.Status != tc.status || summary.BatchCalls == nil || !*summary.BatchCalls || summary.RejectThreshold == nil || *summary.RejectThreshold != 0.75 {
				t.Fatalf("threshold lost on ingress outcome: %+v", summary)
			}
		})
	}
}
