package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestToolReviewRecordSurvivesDeliveryAndRecoversIndependently(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	defer store.Close()
	ctx := context.Background()
	start := time.Now().UTC().Add(-time.Minute)
	summary := contract.ToolReviewSummary{Status: "pending", Mode: contract.ToolReviewModeMark, Model: "jev-latest", ConfigRevision: "r1", StartedAt: start}
	record := contract.RequestRecord{ID: "request_review", StartedAt: start, Status: contract.RequestStatusPending, InputProtocol: contract.ProtocolOpenAIChat, Audit: contract.NotCapturedAuditSummary(), ToolReview: &summary}
	if err := store.InsertRequestRecord(ctx, record); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRequestRecord(ctx, record.ID)
	if err != nil || got.ToolReview == nil || got.ToolReview.Status != "pending" {
		t.Fatalf("review not persisted: %+v %v", got.ToolReview, err)
	}
	encoded, err := json.Marshal(got.ToolReview)
	if err != nil || !strings.Contains(string(encoded), `"calls":[]`) {
		t.Fatalf("empty review calls must be an array: %s %v", encoded, err)
	}
	// A later ordinary delivery snapshot must not erase independently persisted metadata.
	record.ToolReview = nil
	record.Status = contract.RequestStatusSucceeded
	record.CompletedAt = ptrTime(start.Add(time.Second))
	if err := store.UpsertRequestRecord(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverPendingRequestRecords(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetRequestRecord(ctx, record.ID)
	if err != nil || got.Status != contract.RequestStatusSucceeded || got.ToolReview == nil || got.ToolReview.Status != "interrupted" || got.ToolReview.CompletedAt == nil {
		t.Fatalf("recovery overwrote delivery or lost review: %+v %v", got, err)
	}
	if _, err := store.RecoverPendingRequestRecords(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestToolReviewRecordUpdateOnlyTouchesReviewAndFilters(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	defer store.Close()
	ctx := context.Background()
	start := time.Now().UTC().Add(-time.Minute)
	record := contract.RequestRecord{ID: "request_review", StartedAt: start, Status: contract.RequestStatusSucceeded, CompletedAt: ptrTime(start.Add(time.Second)), InputProtocol: contract.ProtocolOpenAIChat, Audit: contract.NotCapturedAuditSummary()}
	if err := store.InsertRequestRecord(ctx, record); err != nil {
		t.Fatal(err)
	}
	summary := contract.ToolReviewSummary{Status: "flagged", Mode: contract.ToolReviewModeMark, Model: "jev-latest", ConfigRevision: "r1", StartedAt: start, CompletedAt: ptrTime(start.Add(time.Second)), Calls: []contract.ToolReviewCall{{Index: 0, Name: "execute", Verdict: "flag", Action: "delivered", Model: "jev-latest"}}}
	if err := store.UpdateRequestToolReview(ctx, record.ID, summary); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRequestToolReview(ctx, "request_missing", summary); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing row: %v", err)
	}
	// A delayed pending review or delivery snapshot cannot downgrade a final review.
	summary.Status = "pending"
	summary.CompletedAt = nil
	if err := store.UpdateRequestToolReview(ctx, record.ID, summary); err != nil {
		t.Fatal(err)
	}
	record.ToolReview = &summary
	if err := store.UpsertRequestRecord(ctx, record); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListRequestRecords(ctx, storage.RequestRecordListOptions{ToolReviewStatus: "flagged"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != contract.RequestStatusSucceeded || page.Items[0].ToolReview.Calls[0].Action != "delivered" {
		t.Fatalf("filtered page: %+v %v", page, err)
	}
	page, err = store.ListRequestRecords(ctx, storage.RequestRecordListOptions{ToolReviewStatus: "passed"})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("wrong filter: %+v %v", page, err)
	}
	if _, err := store.ListRequestRecords(ctx, storage.RequestRecordListOptions{ToolReviewStatus: "bogus"}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("invalid status: %v", err)
	}
}
