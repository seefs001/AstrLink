package ingress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
)

func TestToolReviewRoutingSuccessWaitsForDelivery(t *testing.T) {
	for _, outcome := range []string{"pass", "flag", "failure", "cancel", "write_failure"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "routing.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if _, err := store.CreateEndpoint(context.Background(), validEndpoint(contract.ProtocolOpenAIResponses, true), storage.CredentialMutation{}); err != nil {
				t.Fatal(err)
			}
			assertBindings := func(want int) {
				t.Helper()
				page, err := store.ListRequestSessions(context.Background(), storage.RequestSessionListOptions{})
				if err != nil || len(page.Items) != 1 {
					t.Fatalf("session page=%+v err=%v", page, err)
				}
				audit, err := store.GetChannelBindingAudit(context.Background(), page.Items[0].ID, 0)
				if err != nil || len(audit.Bindings) != want {
					t.Errorf("channel binding before successful delivery: got=%d want=%d err=%v", len(audit.Bindings), want, err)
				}
				_, found, err := store.GetResponseAffinity(context.Background(), "", "resp_review")
				if err != nil || found != (want == 1) {
					t.Errorf("response affinity before successful delivery: found=%v want=%d err=%v", found, want, err)
				}
			}
			handler, _ := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, _ *http.Request) {
				assertBindings(0)
				switch outcome {
				case "failure":
					w.WriteHeader(503)
				case "cancel":
					cancel()
					sendReviewChoice(w, "pass")
				case "flag":
					sendReviewChoice(w, "flag")
				default:
					sendReviewChoice(w, "pass")
				}
			}, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				// The prefix exposes the response ID without exposing any tools.
				_, _ = io.WriteString(w, reviewSSE(`{"type":"response.created","response":{"id":"resp_review","status":"in_progress","output":[]}}`)+reviewSSE(reviewResponseEvents...))
			}, true)
			handler.requestRecords = store
			var writer http.ResponseWriter = httptest.NewRecorder()
			if outcome == "write_failure" {
				writer = &reviewBrokenWriter{header: make(http.Header)}
			}
			handler.ServeHTTP(writer, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect","stream":true}`)).WithContext(ctx))
			want := 0
			if outcome == "pass" {
				want = 1
			}
			assertBindings(want)
		})
	}
}
