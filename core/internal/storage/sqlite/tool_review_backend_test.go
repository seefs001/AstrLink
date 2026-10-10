package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestToolReviewBackendLegacyDoesNotRewriteStoredConfiguration(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	defer store.Close()
	data, err := json.Marshal(contract.DefaultToolReviewSettings())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "backend")
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, store, `UPDATE tool_review_settings SET document_json = ? WHERE id = 1`, string(data))
	record, err := store.GetToolReviewSettings(ctx)
	if err != nil || record.Settings.Backend != contract.ToolReviewBackendJev || record.ETag != entityTag(data) {
		t.Fatalf("legacy backend defaults: %+v %v", record, err)
	}
	var document string
	if err := store.db.QueryRowContext(ctx, `SELECT document_json FROM tool_review_settings WHERE id=1`).Scan(&document); err != nil || document != string(data) {
		t.Fatalf("legacy GET rewrote document: %v", err)
	}
	responses := contract.ToolReviewBackendResponses
	record, err = store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Backend: &responses}, record.ETag)
	if err != nil || record.Settings.Backend != responses || record.Settings.CredentialConfigured {
		t.Fatalf("unconfigured backend switch: %+v %v", record, err)
	}
}

func TestToolReviewBackendSwitchIsAtomicAndRequiresExplicitCredential(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	store := openTestStore(t, path)
	defer store.Close()
	record, err := store.GetToolReviewSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	enabled, batch, mode := true, true, contract.ToolReviewModeAsk
	jevKey, responsesKey := "synthetic-jev-key", "synthetic-responses-key"
	record, err = store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{
		Enabled: &enabled, BatchCalls: &batch, Mode: &mode, APIKey: &jevKey, DataTransferAcknowledged: true,
	}, record.ETag)
	if err != nil {
		t.Fatal(err)
	}
	responses := contract.ToolReviewBackendResponses
	patch := contract.ToolReviewSettingsPatch{Backend: &responses, DataTransferAcknowledged: true}
	if _, err := store.UpdateToolReviewSettings(ctx, patch, record.ETag); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("old backend key could be reused: %v", err)
	}
	patch.APIKey, patch.DataTransferAcknowledged = &responsesKey, false
	// Even an unchanged URL needs acknowledgement when its backend changes.
	if _, err := store.UpdateToolReviewSettings(ctx, patch, record.ETag); !errors.Is(err, storage.ErrToolReviewAcknowledgement) {
		t.Fatalf("backend-only switch did not require acknowledgement: %v", err)
	}
	after, err := store.GetToolReviewSettings(ctx)
	if err != nil || after != record {
		t.Fatalf("failed switch changed settings: %v", err)
	}
	settings, key, err := store.LoadToolReviewSettings(ctx)
	if err != nil || settings != record.Settings || string(key) != jevKey {
		t.Fatal("failed switch changed credential pair")
	}
	clear(key)
	endpoint, model := "https://review.example.test/v1/responses", "structured-review-model"
	patch.Endpoint, patch.Model, patch.DataTransferAcknowledged = &endpoint, &model, true
	record, err = store.UpdateToolReviewSettings(ctx, patch, record.ETag)
	if err != nil || record.Settings.Backend != responses || record.Settings.Mode != mode || !record.Settings.BatchCalls {
		t.Fatalf("Responses save lost independent settings: %+v %v", record, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	defer reopened.Close()
	settings, key, err = reopened.LoadToolReviewSettings(ctx)
	if err != nil || settings != record.Settings || string(key) != responsesKey {
		t.Fatal("backend credential pair lost on restart")
	}
	clear(key)
	// Omitted backend and credential keep the saved pair.
	batch = false
	record, err = reopened.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{BatchCalls: &batch}, record.ETag)
	if err != nil || record.Settings.Backend != responses {
		t.Fatalf("unrelated save changed backend: %v", err)
	}
	jev, empty := contract.ToolReviewBackendJev, ""
	enabled = false
	record, err = reopened.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Backend: &jev, Enabled: &enabled, APIKey: &empty}, record.ETag)
	if err != nil || record.Settings.CredentialConfigured || record.Settings.Backend != jev {
		t.Fatalf("explicit disabled clear failed: %v", err)
	}
	var credentials int
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_review_credentials`).Scan(&credentials); err != nil || credentials != 0 {
		t.Fatal("old backend credential survived clear")
	}
}
