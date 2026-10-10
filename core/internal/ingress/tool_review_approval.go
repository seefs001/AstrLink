package ingress

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func toolReviewAllowsSuccess(summary *contract.ToolReviewSummary) bool {
	if summary == nil || summary.Mode == contract.ToolReviewModeMark || summary.Status == "passed" {
		return true
	}
	if summary.Approval == nil || summary.Approval.State != "approved" {
		return false
	}
	for _, call := range summary.Calls {
		if call.Action != "delivered" {
			return false
		}
	}
	return true
}

// Unlike persistPending, this acknowledgment is a delivery precondition. It
// writes safe metadata only and must not silently succeed with a nil store.
func (handler *Handler) persistApproval(ctx context.Context, session *recordSession) error {
	if handler.requestRecords == nil {
		return toolreview.ErrApprovalStore
	}
	persistCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := handler.requestRecords.UpsertRequestRecord(persistCtx, session.recordSnapshot(nil, nil)); err != nil {
		return toolreview.ErrApprovalStore
	}
	return nil
}

func (handler *Handler) awaitToolApproval(writer *toolReviewWriter, session *recordSession, result *contract.ToolReviewSummary) {
	originalStatus, completed := result.Status, result.CompletedAt
	for i := range result.Calls {
		result.Calls[i].Action = "withheld"
	}
	if handler.toolReviewApprovals == nil {
		result.ErrorCode = "approval_unavailable"
		writer.refuse(session, false)
		return
	}
	approval, err := handler.toolReviewApprovals.Wait(writer.ctx, session.id, *result, writer.input, time.Duration(writer.snapshot.Settings.ApprovalTimeoutMS)*time.Millisecond, func(a contract.ToolReviewApproval) error {
		result.Status, result.CompletedAt, result.Approval = "pending", nil, &a
		return handler.persistApproval(writer.ctx, session)
	})
	if err != nil {
		result.Status, result.CompletedAt = originalStatus, completed
		result.ErrorCode = "approval_unavailable"
		if errors.Is(err, toolreview.ErrApprovalStore) {
			result.ErrorCode = "approval_store_failed"
		}
		if errors.Is(err, toolreview.ErrApprovalQueueFull) {
			result.ErrorCode = "approval_queue_full"
		}
		// A queue admission failure is not a pending human decision.
		result.Approval = nil
		writer.refuse(session, false)
		return
	}
	result.Approval = &approval
	if approval.State != "approved" {
		result.Status, result.CompletedAt = originalStatus, completed
		writer.refuse(session, approval.State == "denied")
		if approval.State == "cancelled" {
			session.status = contract.RequestStatusCancelled
		}
		return
	}
	// Persist the accepted decision while the summary is still pending. Existing
	// storage's pending->terminal guard therefore needs no relaxation.
	for i := range result.Calls {
		result.Calls[i].Action = "pending"
	}
	if err := handler.persistApproval(writer.ctx, session); err != nil {
		result.Status, result.CompletedAt = originalStatus, completed
		result.ErrorCode = "approval_store_failed"
		for i := range result.Calls {
			result.Calls[i].Action = "withheld"
		}
		writer.refuse(session, false)
		return
	}
	result.Status, result.CompletedAt = originalStatus, completed
	// Approval is a verdict, not delivery. Request cancellation, shutdown, or a
	// deadline reached during persistence can still prevent publication.
	if writer.ctx.Err() != nil || !time.Now().Before(approval.ExpiresAt) || handler.toolReviewApprovals.Closed() {
		for i := range result.Calls {
			result.Calls[i].Action = "withheld"
		}
		writer.refuse(session, false)
		if writer.ctx.Err() != nil {
			session.status = contract.RequestStatusCancelled
		}
		return
	}
	if err := writer.deliverReviewed(); err != nil {
		writer.deliveryFailed(session)
		return
	}
	for i := range result.Calls {
		result.Calls[i].Action = "delivered"
	}
}
