package sqlite

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

var _ storage.ToolReviewRecordStore = (*Store)(nil)

func (store *Store) UpdateRequestToolReview(ctx context.Context, id contract.RequestID, summary contract.ToolReviewSummary) error {
	if err := id.Validate(); err != nil {
		return fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	if err := summary.Validate(); err != nil {
		return fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	if summary.Calls == nil {
		summary.Calls = []contract.ToolReviewCall{}
	}
	document, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	// Never replace a root or its delivery snapshot. A late pending review
	// cannot downgrade a terminal outcome; terminal results are immutable.
	result, err := store.db.ExecContext(ctx, `UPDATE request_records SET tool_review_json = CASE
    WHEN json_extract(tool_review_json, '$.status') <> 'pending' THEN tool_review_json
    ELSE ? END WHERE id = ?`, string(document), string(id))
	if err != nil {
		return fmt.Errorf("update tool review metadata: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return storage.ErrNotFound
	}
	return nil
}
