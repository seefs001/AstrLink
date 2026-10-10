package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
)

func TestToolReviewBatchSettingsLegacyAndPersistence(t *testing.T) {
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
	delete(fields, "batch_calls")
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, store, `UPDATE tool_review_settings SET document_json = ? WHERE id = 1`, string(data))
	current, err := store.GetToolReviewSettings(ctx)
	if err != nil || current.Settings.BatchCalls || current.ETag != entityTag(data) {
		t.Fatalf("legacy batch setting: %+v %v", current, err)
	}
	var document string
	if err := store.db.QueryRowContext(ctx, `SELECT document_json FROM tool_review_settings WHERE id = 1`).Scan(&document); err != nil || document != string(data) {
		t.Fatalf("GET changed legacy document: %s %v", document, err)
	}
	for _, value := range []bool{true, false, true} {
		current, err = store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{BatchCalls: &value}, current.ETag)
		if err != nil || current.Settings.BatchCalls != value {
			t.Fatalf("batch %v: %+v %v", value, current, err)
		}
		model := "jev-batch"
		current, err = store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Model: &model}, current.ETag)
		if err != nil || current.Settings.BatchCalls != value {
			t.Fatalf("omitted batch setting changed: %+v %v", current, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	defer reopened.Close()
	settings, key, err := reopened.LoadToolReviewSettings(ctx)
	if err != nil || settings != current.Settings || len(key) != 0 {
		t.Fatalf("batch settings lost on restart: %+v %v", settings, err)
	}
}
