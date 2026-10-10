package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestToolReviewSettingsAtomicCredentialAndETag(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	defer store.Close()
	ctx := context.Background()
	initial, err := store.GetToolReviewSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Settings.Enabled || initial.Settings.CredentialConfigured || initial.ETag == "" {
		t.Fatalf("unsafe defaults: %+v", initial)
	}
	enabled, key := true, "test-review-credential"
	patch := contract.ToolReviewSettingsPatch{Enabled: &enabled, APIKey: &key}
	if _, err := store.UpdateToolReviewSettings(ctx, patch, initial.ETag); !errors.Is(err, storage.ErrToolReviewAcknowledgement) {
		t.Fatalf("missing acknowledgement: %v", err)
	}
	patch.DataTransferAcknowledged = true
	updated, err := store.UpdateToolReviewSettings(ctx, patch, initial.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ETag == initial.ETag || updated.Settings.Revision == initial.Settings.Revision {
		t.Fatal("revision did not change")
	}
	settings, secret, err := store.LoadToolReviewSettings(ctx)
	if err != nil || !settings.Enabled || string(secret) != key {
		t.Fatalf("snapshot mismatch: enabled=%v keyMatches=%v err=%v", settings.Enabled, string(secret) == key, err)
	}
	clear(secret)
	empty := ""
	if _, err := store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{APIKey: &empty}, updated.ETag); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("enabled key clear: %v", err)
	}
	if _, err := store.UpdateToolReviewSettings(ctx, patch, initial.ETag); !errors.Is(err, storage.ErrPrecondition) {
		t.Fatalf("stale etag: %v", err)
	}
	model := "jev-next"
	preserved, err := store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Model: &model}, updated.ETag)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err = store.LoadToolReviewSettings(ctx)
	if err != nil || string(secret) != key {
		t.Fatal("omitted key was not preserved")
	}
	clear(secret)
	// Disabling sends nothing, so moving the endpoint in the same save needs no acknowledgement.
	enabled = false
	endpoint := "https://review.example.test/v1/systemone"
	cleared, err := store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Enabled: &enabled, Endpoint: &endpoint, APIKey: &empty}, preserved.ETag)
	if err != nil || cleared.Settings.CredentialConfigured || cleared.Settings.Endpoint != endpoint {
		t.Fatalf("clear key: %v", err)
	}
	enabled = true
	if _, err := store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Enabled: &enabled, APIKey: &key}, cleared.ETag); !errors.Is(err, storage.ErrToolReviewAcknowledgement) {
		t.Fatalf("re-enable without acknowledgement: %v", err)
	}
	_, secret, err = store.LoadToolReviewSettings(ctx)
	if err != nil || len(secret) != 0 {
		t.Fatal("key survived clear")
	}
}

func TestToolReviewSettingsRollbackAndSealedPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	store := openTestStore(t, path)
	defer store.Close()
	initial, err := store.GetToolReviewSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key, model := "test-review-sensitive-key", "jev-private"
	enabled := true
	patch := contract.ToolReviewSettingsPatch{Enabled: &enabled, APIKey: &key, Model: &model, DataTransferAcknowledged: true}
	// A real SQLite constraint failure on the key write must roll back the
	// earlier settings write in the same transaction.
	mustExec(t, store, `CREATE TRIGGER reject_review_key BEFORE INSERT ON tool_review_credentials BEGIN SELECT RAISE(ABORT, 'test key failure'); END`)
	if _, err := store.UpdateToolReviewSettings(ctx, patch, initial.ETag); err == nil {
		t.Fatal("key write unexpectedly succeeded")
	}
	after, err := store.GetToolReviewSettings(ctx)
	if err != nil || after != initial {
		t.Fatalf("partial settings committed: %v", err)
	}
	mustExec(t, store, `DROP TRIGGER reject_review_key`)
	updated, err := store.UpdateToolReviewSettings(ctx, patch, initial.ETag)
	if err != nil {
		t.Fatal(err)
	}
	var document string
	var sealed []byte
	if err := store.db.QueryRowContext(ctx, `SELECT s.document_json, c.credential_value FROM tool_review_settings s JOIN tool_review_credentials c ON c.id=s.id`).Scan(&document, &sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(document, key) || bytes.Contains(sealed, []byte(key)) || len(sealed) != len(key)+29 {
		t.Fatal("credential was not stored as a sealed secret")
	}
	var properties map[string]json.RawMessage
	if err := json.Unmarshal([]byte(document), &properties); err != nil {
		t.Fatal(err)
	}
	if properties["api_key"] != nil || properties["data_transfer_acknowledged"] != nil {
		t.Fatal("secret or acknowledgement entered settings document")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	defer reopened.Close()
	settings, secret, err := reopened.LoadToolReviewSettings(ctx)
	if err != nil || settings != updated.Settings || string(secret) != key {
		t.Fatalf("reopened snapshot mismatch: %v", err)
	}
	clear(secret)
	// A sealed blob from another column cannot be used as the review key.
	if err := reopened.Put(ctx, "local://builtin-tool/web_search", []byte(key)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, reopened, `UPDATE tool_review_credentials SET credential_value=(SELECT credential_value FROM builtin_tool_credentials WHERE kind='web_search')`)
	settings, secret, err = reopened.LoadToolReviewSettings(ctx)
	if err == nil || settings != updated.Settings || len(secret) != 0 {
		t.Fatalf("credential failure must preserve configuration without key: settingsPreserved=%v err=%v", settings == updated.Settings, err)
	}
	enabled = false
	disabled, err := reopened.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Enabled: &enabled}, updated.ETag)
	if err != nil {
		t.Fatal(err)
	}
	settings, secret, err = reopened.LoadToolReviewSettings(ctx)
	if err != nil || settings != disabled.Settings || len(secret) != 0 {
		t.Fatalf("disabled review must not decrypt key: %v", err)
	}
	status, err := reopened.LocalDataStatus(ctx)
	if err != nil || status.UnreadableCredentials != 1 {
		t.Fatalf("unreadable review key not reported: %+v %v", status, err)
	}
}

func TestToolReviewConcurrentUpdatesKeepSettingsAndCredentialPaired(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	defer store.Close()
	ctx := context.Background()
	original, err := store.GetToolReviewSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for _, value := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight"} {
		wg.Add(1)
		go func(value string) {
			defer wg.Done()
			<-start
			enabled := true
			_, err := store.UpdateToolReviewSettings(ctx, contract.ToolReviewSettingsPatch{Enabled: &enabled, Model: &value, APIKey: &value, DataTransferAcknowledged: true}, original.ETag)
			results <- err
		}(value)
	}
	close(start)
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, storage.ErrPrecondition) {
			conflicts++
		} else {
			t.Fatalf("concurrent update: %v", err)
		}
	}
	if successes != 1 || conflicts != 7 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	settings, key, err := store.LoadToolReviewSettings(ctx)
	if err != nil || settings.Model != string(key) {
		t.Fatalf("mixed revision: %v", err)
	}
	clear(key)
}
