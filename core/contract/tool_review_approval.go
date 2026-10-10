package contract

import (
	"fmt"
	"regexp"
	"time"
)

// ToolReviewApproval records permission for one buffered batch, not a new
// model verdict, proof of delivery, raw-content grant, or reusable exception.
// ReviewStatus retains the completed review while Summary.Status is pending.
type ToolReviewApproval struct {
	ID           string     `json:"id"`
	State        string     `json:"state"`
	ReviewStatus string     `json:"review_status"`
	RequestedAt  time.Time  `json:"requested_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
}

var toolReviewApprovalID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func (approval ToolReviewApproval) Validate() error {
	if !toolReviewApprovalID.MatchString(approval.ID) {
		return fmt.Errorf("invalid tool review approval id")
	}
	if approval.ReviewStatus != "flagged" && approval.ReviewStatus != "failed" {
		return fmt.Errorf("invalid tool review approval review status")
	}
	if approval.RequestedAt.IsZero() || !approval.ExpiresAt.After(approval.RequestedAt) {
		return fmt.Errorf("invalid tool review approval timing")
	}
	switch approval.State {
	case "pending":
		if approval.DecidedAt != nil {
			return fmt.Errorf("pending tool review approval cannot have a decision time")
		}
	case "approved", "denied", "expired", "cancelled", "interrupted":
		if approval.DecidedAt == nil || approval.DecidedAt.Before(approval.RequestedAt) {
			return fmt.Errorf("invalid tool review approval decision time")
		}
	default:
		return fmt.Errorf("invalid tool review approval state")
	}
	return nil
}

// ToolReviewApprovalRequest is the operator-only pending queue's safe view.
// Arguments and user text exist only in the separately gated live detail.
type ToolReviewApprovalRequest struct {
	ID          string            `json:"id"`
	RequestID   RequestID         `json:"request_id"`
	RequestedAt time.Time         `json:"requested_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Summary     ToolReviewSummary `json:"summary"`
}

type ToolReviewApprovalCall struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolReviewApprovalDetail is ephemeral. Never persist it or include it in
// notifications, logs or cross-window events. Reading it requires an operator
// and the existing raw-content unlock session, without issuing a raw grant.
type ToolReviewApprovalDetail struct {
	ID          string                   `json:"id"`
	RequestID   RequestID                `json:"request_id"`
	UserRequest string                   `json:"user_request"`
	Calls       []ToolReviewApprovalCall `json:"calls"`
}
