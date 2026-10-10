package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestToolReviewLegacyThresholdReadPreservesDocumentAndETag(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	defer store.Close()
	// Reproduce a pre-threshold document, including its original revision.
	legacy := contract.DefaultToolReviewSettings()
	legacy.Revision = "legacy-revision"
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "reject_threshold")
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	document := string(data)
	mustExec(t, store, `UPDATE tool_review_settings SET document_json = ? WHERE id = 1`, document)
	for range 2 {
		record, err := store.GetToolReviewSettings(ctx)
		if err != nil || record.Settings.RejectThreshold != 0.5 || record.ETag != entityTag(data) || record.Settings.Revision != legacy.Revision {
			t.Fatalf("legacy GET: %+v %v", record, err)
		}
		settings, key, err := store.LoadToolReviewSettings(ctx)
		if err != nil || settings != record.Settings || len(key) != 0 {
			t.Fatalf("legacy gateway load: %+v %v", settings, err)
		}
		var stored string
		if err := store.db.QueryRowContext(ctx, `SELECT document_json FROM tool_review_settings WHERE id = 1`).Scan(&stored); err != nil || stored != document {
			t.Fatalf("read changed legacy document: %q %v", stored, err)
		}
	}
	model := "jev-updated"
	updated, err := store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Model: &model}, entityTag(data))
	if err != nil || updated.Settings.RejectThreshold != 0.5 || updated.ETag == entityTag(data) || updated.Settings.Revision == legacy.Revision {
		t.Fatalf("first PATCH: %+v %v", updated, err)
	}
	var threshold float64
	if err := store.db.QueryRowContext(ctx, `SELECT json_extract(document_json, '$.reject_threshold') FROM tool_review_settings WHERE id = 1`).Scan(&threshold); err != nil || threshold != 0.5 {
		t.Fatalf("PATCH did not persist default: %v %v", threshold, err)
	}
}

func TestToolReviewThresholdBoundariesValidationAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	store := openTestStore(t, path)
	defer store.Close()
	current, err := store.GetToolReviewSettings(ctx)
	if err != nil || current.Settings.RejectThreshold != 0.5 {
		t.Fatalf("default: %+v %v", current, err)
	}
	for _, threshold := range []float64{0, 1, 0.75} {
		updated, err := store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{RejectThreshold: &threshold}, current.ETag)
		if err != nil || updated.Settings.RejectThreshold != threshold || updated.ETag == current.ETag {
			t.Fatalf("threshold %v: %+v %v", threshold, updated, err)
		}
		current = updated
		model := "jev-preserved"
		current, err = store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Model: &model}, current.ETag)
		if err != nil || current.Settings.RejectThreshold != threshold {
			t.Fatalf("omitted threshold changed: %+v %v", current, err)
		}
	}
	for _, threshold := range []float64{-0.001, 1.001, math.NaN(), math.Inf(1), math.Inf(-1)} {
		key := "must-not-persist"
		_, err := store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{RejectThreshold: &threshold, APIKey: &key}, current.ETag)
		if !errors.Is(err, storage.ErrInvalidArgument) {
			t.Fatalf("invalid threshold %v: %v", threshold, err)
		}
		after, err := store.GetToolReviewSettings(ctx)
		if err != nil || after != current {
			t.Fatalf("invalid threshold committed settings or credential: %+v %v", after, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	defer reopened.Close()
	loaded, key, err := reopened.LoadToolReviewSettings(ctx)
	if err != nil || loaded != current.Settings || len(key) != 0 {
		t.Fatalf("reopened threshold: %+v %v", loaded, err)
	}
}
