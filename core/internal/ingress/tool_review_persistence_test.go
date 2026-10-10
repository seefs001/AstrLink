package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
)

type reviewSQLiteStore struct {
	*sqlite.Store
	updated chan error
}

func (store *reviewSQLiteStore) UpdateRequestToolReview(ctx context.Context, id contract.RequestID, summary contract.ToolReviewSummary) error {
	err := store.Store.UpdateRequestToolReview(ctx, id, summary)
	store.updated <- err
	return err
}

func TestToolReviewRealPendingPersistsWithoutPreparationPlaceholder(t *testing.T) {
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, streaming), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "reviews.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				store := &reviewSQLiteStore{Store: db, updated: make(chan error, 1)}
				providerStarted, releaseProvider := make(chan struct{}), make(chan struct{})
				judgeStarted, releaseJudge := make(chan struct{}), make(chan struct{})
				var providerOnce, judgeOnce sync.Once
				body := reviewDangerousResponse
				if streaming {
					body = reviewSSE(reviewResponseEvents...)
				}
				handler, _ := reviewTestHandler(t, mode, func(w http.ResponseWriter, r *http.Request) {
					close(judgeStarted)
					select {
					case <-releaseJudge:
						sendReviewChoice(w, "pass")
					case <-r.Context().Done():
					}
				}, func(w http.ResponseWriter, r *http.Request) {
					close(providerStarted)
					select {
					case <-releaseProvider:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
					}
					_, _ = io.WriteString(w, body)
				}, streaming)
				handler.requestRecords = store
				response := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					defer close(done)
					handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5","input":"Inspect only","stream":%t}`, streaming))).WithContext(ctx))
				}()
				t.Cleanup(func() {
					providerOnce.Do(func() { close(releaseProvider) })
					judgeOnce.Do(func() { close(releaseJudge) })
					cancel()
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						t.Error("request did not stop")
					}
				})
				select {
				case <-providerStarted:
				case <-done:
					t.Fatal("request ended before provider")
				case <-time.After(2 * time.Second):
					t.Fatal("provider did not start")
				}
				page, err := db.ListRequestRecords(ctx, storage.RequestRecordListOptions{})
				if err != nil || len(page.Items) != 1 || page.Items[0].Status != contract.RequestStatusPending || page.Items[0].ToolReview != nil {
					t.Fatalf("preparation lost ordinary record or wrote review: %+v %v", page, err)
				}
				id := page.Items[0].ID
				providerOnce.Do(func() { close(releaseProvider) })
				select {
				case <-judgeStarted:
				case <-time.After(2 * time.Second):
					t.Fatal("judge did not start")
				}
				if mode == contract.ToolReviewModeMark {
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						t.Fatal("mark held delivery for judge")
					}
				} else {
					select {
					case <-done:
						t.Fatal("reject released delivery before judge")
					default:
					}
				}
				pending, err := db.GetRequestRecord(ctx, id)
				action := "pending"
				if mode == contract.ToolReviewModeMark {
					action = "delivered"
				}
				if err != nil || pending.ToolReview == nil || pending.ToolReview.Status != "pending" || pending.ToolReview.CompletedAt != nil || pending.ToolReview.RejectThreshold == nil || len(pending.ToolReview.Calls) != 1 || pending.ToolReview.Calls[0].Name != "bash" || pending.ToolReview.Calls[0].Action != action {
					t.Fatalf("real pending was not persisted from NULL: %+v %v", pending.ToolReview, err)
				}
				encoded, err := json.Marshal(pending.ToolReview)
				if err != nil || strings.Contains(string(encoded), "command") || strings.Contains(string(encoded), "Inspect only") {
					t.Fatalf("unsafe pending metadata: %s %v", encoded, err)
				}
				// Normal request upserts cannot erase independently owned review metadata.
				ordinary := pending
				ordinary.ToolReview = nil
				if err := db.UpsertRequestRecord(ctx, ordinary); err != nil {
					t.Fatal(err)
				}
				retained, err := db.GetRequestRecord(ctx, id)
				if err != nil || !reflect.DeepEqual(retained.ToolReview, pending.ToolReview) {
					t.Fatalf("ordinary upsert erased review: %+v %v", retained.ToolReview, err)
				}
				judgeOnce.Do(func() { close(releaseJudge) })
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("request did not finish")
				}
				if mode == contract.ToolReviewModeMark {
					select {
					case err := <-store.updated:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("detached review did not persist")
					}
				}
				terminal, err := db.GetRequestRecord(ctx, id)
				if err != nil || terminal.Status != contract.RequestStatusSucceeded || terminal.ToolReview == nil || terminal.ToolReview.Status != "passed" || len(terminal.ToolReview.Calls) != 1 || terminal.ToolReview.Calls[0].Action != "delivered" || response.Code != http.StatusOK || response.Body.String() != body {
					t.Fatalf("terminal or delivery lost: %+v %v; response=%d %s", terminal, err, response.Code, response.Body.String())
				}
				for _, late := range []contract.RequestRecord{ordinary, pending} {
					if err := db.UpsertRequestRecord(ctx, late); err != nil {
						t.Fatal(err)
					}
				}
				retained, err = db.GetRequestRecord(ctx, id)
				if err != nil || !reflect.DeepEqual(retained.ToolReview, terminal.ToolReview) || retained.Status != terminal.Status {
					t.Fatalf("late snapshot overwrote terminal: %+v %v", retained, err)
				}

				// Reuse a real ingress pending snapshot to simulate interruption,
				// rather than seeding an empty placeholder that ingress no longer writes.
				pending.ID = "request_interrupted_review"
				if err := db.InsertRequestRecord(ctx, pending); err != nil {
					t.Fatal(err)
				}
				if _, err := db.RecoverPendingRequestRecords(ctx); err != nil {
					t.Fatal(err)
				}
				recovered, err := db.GetRequestRecord(ctx, pending.ID)
				if err != nil || recovered.ToolReview == nil || recovered.ToolReview.Status != "interrupted" || !reflect.DeepEqual(recovered.ToolReview.Calls, pending.ToolReview.Calls) || !reflect.DeepEqual(recovered.ToolReview.RejectThreshold, pending.ToolReview.RejectThreshold) || recovered.ToolReview.CompletedAt == nil {
					t.Fatalf("real pending recovery lost metadata: %+v %v", recovered, err)
				}
				if mode == contract.ToolReviewModeMark && recovered.Status != contract.RequestStatusSucceeded {
					t.Fatal("review recovery changed completed mark delivery")
				}
			})
		}
	}
}
