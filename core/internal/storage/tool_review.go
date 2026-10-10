package storage

import (
	"context"
	"errors"

	"github.com/QuantumNous/astrlink/core/contract"
)

var ErrToolReviewAcknowledgement = errors.New("tool review data transfer acknowledgement is required")

type ToolReviewSettingsRecord struct {
	Settings contract.ToolReviewSettings
	ETag     string
}

type ToolReviewSettingsStore interface {
	GetToolReviewSettings(context.Context) (ToolReviewSettingsRecord, error)
	UpdateToolReviewSettings(context.Context, contract.ToolReviewSettingsPatch, string) (ToolReviewSettingsRecord, error)
}

// ToolReviewSettingsLoader takes one atomic configuration and credential snapshot.
// Disabled settings do not decrypt or return the key. On credential decryption
// failure, the parsed settings are still returned alongside the error so the
// caller can apply the configured mark/reject failure policy.
// The caller owns the returned key bytes and should clear them after use.
type ToolReviewSettingsLoader interface {
	LoadToolReviewSettings(context.Context) (contract.ToolReviewSettings, []byte, error)
}

// ToolReviewRecordStore updates only review metadata, never delivery state.
type ToolReviewRecordStore interface {
	UpdateRequestToolReview(context.Context, contract.RequestID, contract.ToolReviewSummary) error
}
