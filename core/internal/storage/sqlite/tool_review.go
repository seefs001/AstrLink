package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

const toolReviewCredentialsTable = "tool_review_credentials"

var (
	_ storage.ToolReviewSettingsStore  = (*Store)(nil)
	_ storage.ToolReviewSettingsLoader = (*Store)(nil)
)

func decodeToolReviewSettings(document string, configured bool) (storage.ToolReviewSettingsRecord, error) {
	settings := contract.DefaultToolReviewSettings()
	if err := json.Unmarshal([]byte(document), &settings); err != nil {
		return storage.ToolReviewSettingsRecord{}, fmt.Errorf("%w: tool review settings", storage.ErrInvalidRecord)
	}
	settings.CredentialConfigured = configured
	if err := settings.Validate(); err != nil {
		return storage.ToolReviewSettingsRecord{}, fmt.Errorf("%w: tool review settings", storage.ErrInvalidRecord)
	}
	return storage.ToolReviewSettingsRecord{Settings: settings, ETag: entityTag([]byte(document))}, nil
}

func (store *Store) GetToolReviewSettings(ctx context.Context) (storage.ToolReviewSettingsRecord, error) {
	var document string
	var configured bool
	if err := store.db.QueryRowContext(ctx, `SELECT document_json, EXISTS(SELECT 1 FROM tool_review_credentials WHERE id = 1) FROM tool_review_settings WHERE id = 1`).Scan(&document, &configured); err != nil {
		return storage.ToolReviewSettingsRecord{}, err
	}
	return decodeToolReviewSettings(document, configured)
}

func (store *Store) LoadToolReviewSettings(ctx context.Context) (contract.ToolReviewSettings, []byte, error) {
	var document string
	var sealed []byte
	// A single SQL statement observes the settings and key at the same revision.
	if err := store.db.QueryRowContext(ctx, `SELECT s.document_json, c.credential_value FROM tool_review_settings s LEFT JOIN tool_review_credentials c ON c.id = s.id WHERE s.id = 1`).Scan(&document, &sealed); err != nil {
		return contract.ToolReviewSettings{}, nil, err
	}
	defer clear(sealed)
	record, err := decodeToolReviewSettings(document, len(sealed) > 0)
	if err != nil {
		return contract.ToolReviewSettings{}, nil, err
	}
	if !record.Settings.Enabled || len(sealed) == 0 {
		return record.Settings, nil, nil
	}
	key, err := store.openSecret(toolReviewCredentialsTable, "1", sealed, true)
	if err != nil {
		return record.Settings, nil, err
	}
	return record.Settings, key, nil
}

func (store *Store) UpdateToolReviewSettings(ctx context.Context, patch contract.ToolReviewSettingsPatch, expectedETag string) (record storage.ToolReviewSettingsRecord, err error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return record, err
	}
	defer tx.Rollback()
	// Take the writer lock before reading the ETag. Otherwise two deferred
	// transactions can read the same revision and fail a lock upgrade with
	// SQLITE_BUSY instead of reporting a stale If-Match.
	if _, err := tx.ExecContext(ctx, `UPDATE tool_review_settings SET document_json = document_json WHERE 0`); err != nil {
		return record, err
	}
	var document string
	var configured bool
	if err := tx.QueryRowContext(ctx, `SELECT document_json, EXISTS(SELECT 1 FROM tool_review_credentials WHERE id = 1) FROM tool_review_settings WHERE id = 1`).Scan(&document, &configured); err != nil {
		return record, err
	}
	current, err := decodeToolReviewSettings(document, configured)
	if err != nil {
		return record, err
	}
	if current.ETag != expectedETag {
		return record, storage.ErrPrecondition
	}
	settings := current.Settings
	if patch.Backend != nil {
		settings.Backend = *patch.Backend
	}
	backendChanged := settings.Backend != current.Settings.Backend
	// A saved credential belongs to the currently selected backend. Never send
	// it to a newly selected backend without explicit replacement or clearing.
	if backendChanged && current.Settings.CredentialConfigured && patch.APIKey == nil {
		return record, fmt.Errorf("%w: switching tool review backend requires an explicit credential replacement or clearing", storage.ErrInvalidArgument)
	}
	if patch.Enabled != nil {
		settings.Enabled = *patch.Enabled
	}
	if patch.Endpoint != nil {
		settings.Endpoint = *patch.Endpoint
	}
	if patch.Model != nil {
		settings.Model = *patch.Model
	}
	if patch.Prompt != nil {
		settings.Prompt = *patch.Prompt
	}
	if patch.Mode != nil {
		settings.Mode = *patch.Mode
	}
	if patch.TimeoutMS != nil {
		settings.TimeoutMS = *patch.TimeoutMS
	}
	if patch.RejectThreshold != nil {
		settings.RejectThreshold = *patch.RejectThreshold
	}
	if patch.BatchCalls != nil {
		settings.BatchCalls = *patch.BatchCalls
	}
	if patch.AskOnReviewError != nil {
		settings.AskOnReviewError = *patch.AskOnReviewError
	}
	if patch.ApprovalTimeoutMS != nil {
		settings.ApprovalTimeoutMS = *patch.ApprovalTimeoutMS
	}
	if patch.APIKey != nil {
		if len(*patch.APIKey) > 16384 || (*patch.APIKey != "" && (strings.TrimSpace(*patch.APIKey) != *patch.APIKey || strings.ContainsAny(*patch.APIKey, "\r\n\x00"))) {
			return record, fmt.Errorf("%w: invalid tool review credential", storage.ErrInvalidArgument)
		}
		settings.CredentialConfigured = *patch.APIKey != ""
	}
	if err := settings.Validate(); err != nil {
		return record, fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	needsAcknowledgement := settings.Enabled && (!current.Settings.Enabled || settings.Endpoint != current.Settings.Endpoint || backendChanged)
	if needsAcknowledgement && !patch.DataTransferAcknowledged {
		return record, storage.ErrToolReviewAcknowledgement
	}
	var revision [16]byte
	if _, err := rand.Read(revision[:]); err != nil {
		return record, err
	}
	settings.Revision = hex.EncodeToString(revision[:])
	encoded, err := json.Marshal(settings)
	if err != nil {
		return record, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE tool_review_settings SET document_json = ? WHERE id = 1 AND document_json = ?`, string(encoded), document)
	if err != nil {
		return record, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return record, storage.ErrPrecondition
	}
	if patch.APIKey != nil {
		if err := store.updateToolReviewCredential(ctx, tx, *patch.APIKey); err != nil {
			return record, err
		}
	}
	if err := tx.Commit(); err != nil {
		return record, err
	}
	return storage.ToolReviewSettingsRecord{Settings: settings, ETag: entityTag(encoded)}, nil
}

func (store *Store) updateToolReviewCredential(ctx context.Context, tx *sql.Tx, key string) error {
	if key == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM tool_review_credentials WHERE id = 1`)
		return err
	}
	plaintext := []byte(key)
	defer clear(plaintext)
	sealed, err := store.keys.sealColumn(toolReviewCredentialsTable, "1", plaintext)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tool_review_credentials (id, credential_value) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET credential_value = excluded.credential_value`, sealed)
	return err
}
