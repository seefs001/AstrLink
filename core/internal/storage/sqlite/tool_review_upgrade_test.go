package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage/migrate"
)

func TestToolReviewUpgradePreservesHistoricalRecordsAndDefaultsOff(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	database, err := sql.Open(driverName, sqliteFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var shipped []migrate.Migration
	for _, migration := range migrate.DefaultMigrations() {
		if migration.Name == "tool_review_settings_and_records" {
			break
		}
		shipped = append(shipped, migration)
	}
	if len(shipped) == 0 || shipped[len(shipped)-1].Version != 49 {
		t.Fatal("unexpected shipped migration baseline")
	}
	runner, err := migrate.New(migrate.SQLDatabase{DB: database}, shipped)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Up(ctx); err != nil {
		t.Fatal(err)
	}
	audit, err := json.Marshal(contract.NotCapturedAuditSummary())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO request_records (id,started_at,status,input_protocol,streaming,audit_json,created_at) VALUES ('request_legacy','2026-10-01T00:00:00Z','succeeded','openai.chat',0,?,'2026-10-01T00:00:00Z')`, string(audit)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO request_records (id,started_at,status,input_protocol,streaming,audit_json,created_at) VALUES ('request_pending','2026-10-01T00:00:01Z','pending','openai.chat',1,?,'2026-10-01T00:00:01Z')`, string(audit)); err != nil {
		t.Fatal(err)
	}
	// A v49 plaintext credential must survive the upgrade and still be sealed by Open.
	if _, err := database.ExecContext(ctx, `INSERT INTO builtin_tool_credentials (kind, credential_value, sealed) VALUES ('web_search', CAST('legacy-search-key' AS BLOB), 0)`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var etag string
	for open := 0; open < 2; open++ {
		store := openTestStore(t, path)
		legacy, err := store.GetRequestRecord(ctx, "request_legacy")
		if err != nil || legacy.Status != contract.RequestStatusSucceeded || legacy.ToolReview != nil {
			t.Fatalf("historical record changed: %+v %v", legacy, err)
		}
		secret, err := store.Get(ctx, "local://builtin-tool/web_search")
		if err != nil || string(secret) != "legacy-search-key" {
			t.Fatalf("historical credential lost: %v", err)
		}
		settings, key, err := store.LoadToolReviewSettings(ctx)
		if err != nil || settings.Enabled || settings.CredentialConfigured || len(key) != 0 || settings.Mode != contract.ToolReviewModeMark ||
			settings.Endpoint != "https://api.typesafe.ai/v1/systemone" || settings.Model != "jev-latest" || settings.TimeoutMS != 5000 {
			t.Fatalf("unsafe upgrade defaults: %+v %v", settings, err)
		}
		record, err := store.GetToolReviewSettings(ctx)
		if err != nil || (etag != "" && record.ETag != etag) {
			t.Fatalf("reopen changed settings: %v", err)
		}
		etag = record.ETag
		var versions, latest int
		var name string
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(version), (SELECT name FROM schema_migrations WHERE version = 50) FROM schema_migrations`).Scan(&versions, &latest, &name); err != nil ||
			versions != len(shipped)+1 || latest != 50 || name != "tool_review_settings_and_records" {
			t.Fatalf("migration history: count=%d latest=%d name=%q err=%v", versions, latest, name, err)
		}
		var violations int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
			t.Fatalf("foreign keys: %d %v", violations, err)
		}
		if open == 0 {
			if _, err := store.RecoverPendingRequestRecords(ctx); err != nil {
				t.Fatal(err)
			}
		}
		pending, err := store.GetRequestRecord(ctx, "request_pending")
		if err != nil || pending.Status != contract.RequestStatusFailed || pending.ToolReview != nil {
			t.Fatalf("pending historical record recovery: %+v %v", pending, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
