package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestToolReviewApprovalSettingsLegacyAndPatch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	store := openTestStore(t, path)
	defer store.Close()
	data, err := json.Marshal(contract.DefaultToolReviewSettings())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "ask_on_review_error")
	delete(fields, "approval_timeout_ms")
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, store, `UPDATE tool_review_settings SET document_json = ? WHERE id = 1`, string(data))
	current, err := store.GetToolReviewSettings(ctx)
	if err != nil || current.Settings.AskOnReviewError || current.Settings.ApprovalTimeoutMS != 120000 || current.ETag != entityTag(data) {
		t.Fatalf("legacy defaults: %+v %v", current, err)
	}
	var unchanged string
	if err := store.db.QueryRowContext(ctx, `SELECT document_json FROM tool_review_settings WHERE id = 1`).Scan(&unchanged); err != nil || unchanged != string(data) {
		t.Fatalf("GET rewrote legacy config: %q %v", unchanged, err)
	}
	mode, ask, timeout := contract.ToolReviewModeAsk, true, 600000
	current, err = store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Mode: &mode, AskOnReviewError: &ask, ApprovalTimeoutMS: &timeout}, current.ETag)
	if err != nil || current.Settings.Mode != mode || !current.Settings.AskOnReviewError || current.Settings.ApprovalTimeoutMS != timeout {
		t.Fatalf("approval patch: %+v %v", current, err)
	}
	model := "jev-preserved"
	current, err = store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Model: &model}, current.ETag)
	if err != nil || !current.Settings.AskOnReviewError || current.Settings.ApprovalTimeoutMS != timeout {
		t.Fatalf("omitted settings lost: %+v %v", current, err)
	}
	for _, invalid := range []int{0, 999, 600001} {
		key := "must-not-persist"
		_, err := store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{ApprovalTimeoutMS: &invalid, APIKey: &key}, current.ETag)
		if !errors.Is(err, storage.ErrInvalidArgument) {
			t.Fatalf("invalid approval timeout=%d: %v", invalid, err)
		}
		after, err := store.GetToolReviewSettings(ctx)
		if err != nil || after != current {
			t.Fatalf("invalid patch committed settings/key: %+v %v", after, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	defer reopened.Close()
	loaded, key, err := reopened.LoadToolReviewSettings(ctx)
	if err != nil || loaded != current.Settings || len(key) != 0 {
		t.Fatalf("reopened approval settings: %+v %v", loaded, err)
	}
}

func approvalRecord(start time.Time, id contract.RequestID) contract.RequestRecord {
	return contract.RequestRecord{
		ID: id, StartedAt: start, Status: contract.RequestStatusPending,
		InputProtocol: contract.ProtocolOpenAIChat, Audit: contract.NotCapturedAuditSummary(),
		ToolReview: &contract.ToolReviewSummary{
			Status: "pending", Mode: contract.ToolReviewModeAsk, Model: "jev-latest", StartedAt: start,
			Approval: &contract.ToolReviewApproval{ID: "approval_" + string(id), State: "pending", ReviewStatus: "flagged", RequestedAt: start, ExpiresAt: start.Add(time.Minute)},
			Calls:    []contract.ToolReviewCall{{Index: 0, Name: "execute", Verdict: "flag", ModelVerdict: "flag", ReasonCode: "destructive_action", Action: "withheld"}},
		},
	}
}

func TestToolReviewApprovalPersistenceBeforeAndAfterDelivery(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	defer store.Close()
	start := time.Now().UTC().Add(-time.Minute)
	record := approvalRecord(start, "request_approval")
	if err := store.UpsertRequestRecord(ctx, record); err != nil {
		t.Fatal(err)
	}
	decisionAt := start.Add(time.Second)
	record.ToolReview.Approval.State = "approved"
	record.ToolReview.Approval.DecidedAt = &decisionAt
	record.ToolReview.Calls[0].Action = "pending"
	if err := store.UpsertRequestRecord(ctx, record); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRequestRecord(ctx, record.ID)
	if err != nil || got.ToolReview.Approval.State != "approved" || got.ToolReview.Status != "pending" || got.ToolReview.Calls[0].Action != "pending" {
		t.Fatalf("pre-delivery permission not saved: %+v %v", got, err)
	}
	completedAt := start.Add(2 * time.Second)
	record.Status, record.CompletedAt = contract.RequestStatusSucceeded, &completedAt
	record.ToolReview.Status, record.ToolReview.CompletedAt = "flagged", &completedAt
	record.ToolReview.Calls[0].Action = "delivered"
	if err := store.UpsertRequestRecord(ctx, record); err != nil {
		t.Fatal(err)
	}
	// Late review work must not undo an approved batch's final delivery.
	late := approvalRecord(start, record.ID)
	if err := store.UpdateRequestToolReview(ctx, record.ID, *late.ToolReview); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRequestRecord(ctx, late); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetRequestRecord(ctx, record.ID)
	if err != nil || got.Status != contract.RequestStatusSucceeded || got.ToolReview.Status != "flagged" || got.ToolReview.Approval.State != "approved" || got.ToolReview.Calls[0].Action != "delivered" || got.ToolReview.Calls[0].Verdict != "flag" {
		t.Fatalf("final approval/delivery downgraded: %+v %v", got, err)
	}
}

func TestToolReviewApprovalRecoveryDoesNotInventDelivery(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	defer store.Close()
	start := time.Now().UTC().Add(-time.Minute)
	for _, state := range []string{"pending", "approved"} {
		record := approvalRecord(start, contract.RequestID("request_"+state))
		if state == "approved" {
			record.ToolReview.Approval.State = state
			record.ToolReview.Approval.DecidedAt = ptrTime(start.Add(time.Second))
			record.ToolReview.Calls[0].Action = "pending"
		}
		if err := store.InsertRequestRecord(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RecoverPendingRequestRecords(ctx); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "approved"} {
		got, err := store.GetRequestRecord(ctx, contract.RequestID("request_"+state))
		if err != nil || got.Status != contract.RequestStatusFailed || got.ToolReview.Status != "interrupted" {
			t.Fatalf("unclosed request: %+v %v", got, err)
		}
		expectedState, expectedAction := "interrupted", "withheld"
		if state == "approved" {
			expectedState, expectedAction = "approved", "pending"
		}
		a := got.ToolReview.Approval
		if a.State != expectedState || a.DecidedAt == nil || a.ReviewStatus != "flagged" || got.ToolReview.Calls[0].Action != expectedAction || got.ToolReview.Calls[0].Verdict != "flag" {
			t.Fatalf("recovery invented decision/verdict/delivery: %+v", got.ToolReview)
		}
		if err := got.ToolReview.Validate(); err != nil {
			t.Fatalf("invalid recovered summary: %v", err)
		}
	}
	if _, err := store.RecoverPendingRequestRecords(ctx); err != nil {
		t.Fatal(err)
	}
}
