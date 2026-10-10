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
)

type blockedReviewUpdates struct {
	*reviewRecords
	entered chan struct{}
	release chan struct{}
	updates atomic.Int32
}

func (store *blockedReviewUpdates) UpdateRequestToolReview(ctx context.Context, id contract.RequestID, result contract.ToolReviewSummary) error {
	store.updates.Add(1)
	store.entered <- struct{}{}
	select {
	case <-store.release:
		return store.reviewRecords.UpdateRequestToolReview(ctx, id, result)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestToolReviewMarkAdmissionBoundsSlowMetadataUpdates(t *testing.T) {
	var reviews atomic.Int32
	handler, records := reviewTestHandler(t, contract.ToolReviewModeMark, func(w http.ResponseWriter, _ *http.Request) {
		reviews.Add(1)
		sendReviewChoice(w, "pass")
	}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reviewDangerousResponse)
	}, false)
	records.updated = make(chan contract.RequestRecord, 32)
	store := &blockedReviewUpdates{reviewRecords: records, entered: make(chan struct{}, 32), release: make(chan struct{})}
	handler.requestRecords = store
	defer func() {
		close(store.release)
		for i := int32(0); i < store.updates.Load(); i++ {
			select {
			case <-records.updated:
			case <-time.After(time.Second):
				t.Error("metadata job did not stop")
			}
		}
	}()
	serve := func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)))
		if response.Code != 200 || response.Body.String() != reviewDangerousResponse {
			t.Fatal("mark admission affected delivered response")
		}
	}
	for i := 0; i < 16; i++ {
		serve()
		select {
		case <-store.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("admitted review did not reach metadata store")
		}
	}
	serve()
	records.mu.Lock()
	queueFull := 0
	for _, record := range records.records {
		if record.ToolReview != nil && record.ToolReview.ErrorCode == "queue_full" {
			queueFull++
			if record.Status != contract.RequestStatusSucceeded || record.ToolReview.Status != "failed" {
				t.Error("queue rejection changed request success or misreported review")
			}
		}
	}
	records.mu.Unlock()
	if queueFull != 1 || store.updates.Load() != 16 || reviews.Load() != 16 {
		t.Fatalf("unbounded mark overflow: queue_full=%d metadata_jobs=%d judges=%d", queueFull, store.updates.Load(), reviews.Load())
	}
}
