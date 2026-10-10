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
)

func TestToolReviewCancellationWhileReviewingNeverReleasesTools(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	handler, records := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
			sendReviewChoice(w, "pass")
		}
	}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reviewDangerousResponse)
	}, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Inspect only"}`)).WithContext(ctx))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("judge did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not stop review")
	}
	if strings.Contains(response.Body.String(), "function_call") {
		t.Fatal("cancelled call was released")
	}
	record := records.latest(t)
	if record.Status != contract.RequestStatusCancelled || record.ToolReview == nil || record.ToolReview.Status != "cancelled" {
		t.Fatalf("cancel outcome=%s %#v", record.Status, record.ToolReview)
	}
}
