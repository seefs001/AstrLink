package toolreview

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/astrlink/core/contract"
)

var (
	ErrApprovalGone      = errors.New("tool review approval is no longer pending")
	ErrApprovalConflict  = errors.New("tool review approval was already decided")
	ErrApprovalQueueFull = errors.New("tool review approval queue is full")
	ErrApprovalStore     = errors.New("tool review approval metadata could not be saved")
	ErrApprovalInput     = errors.New("tool review approval input is invalid")
)

const ApprovalCapacity = 16

type approvalEntry struct {
	ctx       context.Context
	request   contract.ToolReviewApprovalRequest
	detail    contract.ToolReviewApprovalDetail
	approval  contract.ToolReviewApproval
	published bool
	done      chan struct{}
}

// Broker retains bounded, request-scoped plaintext only while pending. Wait runs
// on the existing inference goroutine; no worker or historical queue is created.
type Broker struct {
	mu      sync.Mutex
	ctx     context.Context
	entries map[string]*approvalEntry
	closed  bool
}

func NewBroker(ctx context.Context) *Broker {
	return &Broker{ctx: ctx, entries: make(map[string]*approvalEntry)}
}

func (b *Broker) Closed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed || b.ctx.Err() != nil
}

func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, e := range b.entries {
		b.finish(e, "interrupted")
	}
}

func (b *Broker) finish(e *approvalEntry, state string) {
	if e.approval.State != "pending" {
		return
	}
	now := time.Now().UTC()
	e.approval.State, e.approval.DecidedAt = state, &now
	e.detail = contract.ToolReviewApprovalDetail{}
	close(e.done)
}

// Refresh cancellation and expiry under the same lock as Decide. A ready allow
// never wins over a cancellation or deadline already observed at this seam.
func (b *Broker) refresh(e *approvalEntry) {
	switch {
	case b.closed || b.ctx.Err() != nil:
		b.finish(e, "interrupted")
	case e.ctx.Err() != nil:
		b.finish(e, "cancelled")
	case !time.Now().Before(e.approval.ExpiresAt):
		b.finish(e, "expired")
	}
}

// Wait reserves capacity, persists safe metadata, then publishes the batch.
// persist must return success before the operator can see or decide it.
func (b *Broker) Wait(ctx context.Context, id contract.RequestID, summary contract.ToolReviewSummary, input Input, timeout time.Duration, persist func(contract.ToolReviewApproval) error) (contract.ToolReviewApproval, error) {
	if (summary.Status != "flagged" && summary.Status != "failed") || len(input.Calls) == 0 || len(input.Calls) > contract.MaxToolReviewCalls || len(input.UserRequest) > contract.MaxToolReviewInputBytes || !utf8.ValidString(input.UserRequest) || timeout < time.Second || timeout > 10*time.Minute || persist == nil || summary.Mode != contract.ToolReviewModeAsk || summary.Approval != nil || len(summary.Calls) != len(input.Calls) || summary.Validate() != nil {
		return contract.ToolReviewApproval{}, ErrApprovalInput
	}
	size := len(input.UserRequest)
	seen := make(map[int]bool)
	for i, call := range input.Calls {
		if summary.Calls[i].Index != call.Index || summary.Calls[i].Name != call.Name {
			return contract.ToolReviewApproval{}, ErrApprovalInput
		}
		size += len(call.Name) + len(call.Arguments)
		if call.Index < 0 || seen[call.Index] || call.Name == "" || safeName(call.Name) != call.Name || (!call.Custom && !validArguments(call.Arguments)) || len(call.Arguments) > contract.MaxToolReviewInputBytes || !utf8.ValidString(call.Arguments) {
			return contract.ToolReviewApproval{}, ErrApprovalInput
		}
		seen[call.Index] = true
	}
	if size > contract.MaxToolReviewBufferBytes {
		return contract.ToolReviewApproval{}, ErrApprovalInput
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return contract.ToolReviewApproval{}, ErrApprovalInput
	}
	now := time.Now().UTC()
	approval := contract.ToolReviewApproval{ID: "approval_" + hex.EncodeToString(random[:]), State: "pending", ReviewStatus: summary.Status, RequestedAt: now, ExpiresAt: now.Add(timeout)}
	summary.Status, summary.CompletedAt, summary.Approval = "pending", nil, &approval
	summary.Calls = append([]contract.ToolReviewCall(nil), summary.Calls...)
	for i := range summary.Calls {
		summary.Calls[i].Action = "withheld"
	}
	detail := contract.ToolReviewApprovalDetail{ID: approval.ID, RequestID: id, UserRequest: input.UserRequest, Calls: make([]contract.ToolReviewApprovalCall, len(input.Calls))}
	for i, call := range input.Calls {
		detail.Calls[i] = contract.ToolReviewApprovalCall{Index: call.Index, Name: call.Name, Arguments: call.Arguments}
	}
	e := &approvalEntry{ctx: ctx, request: contract.ToolReviewApprovalRequest{ID: approval.ID, RequestID: id, RequestedAt: now, ExpiresAt: approval.ExpiresAt, Summary: summary}, detail: detail, approval: approval, done: make(chan struct{})}
	b.mu.Lock()
	if b.closed || b.ctx.Err() != nil || ctx.Err() != nil {
		b.mu.Unlock()
		return approval, ErrApprovalGone
	}
	if len(b.entries) >= ApprovalCapacity {
		b.mu.Unlock()
		return approval, ErrApprovalQueueFull
	}
	b.entries[approval.ID] = e
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.entries, approval.ID)
		e.detail = contract.ToolReviewApprovalDetail{}
		b.mu.Unlock()
	}()
	if err := persist(approval); err != nil {
		return approval, ErrApprovalStore
	}
	b.mu.Lock()
	b.refresh(e)
	e.published = e.approval.State == "pending"
	b.mu.Unlock()
	timer := time.NewTimer(time.Until(approval.ExpiresAt))
	defer timer.Stop()
	select {
	case <-e.done:
	case <-ctx.Done():
	case <-b.ctx.Done():
	case <-timer.C:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refresh(e)
	return e.approval, nil
}

func (b *Broker) List() []contract.ToolReviewApprovalRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := make([]contract.ToolReviewApprovalRequest, 0, len(b.entries))
	for _, e := range b.entries {
		b.refresh(e)
		if e.published && e.approval.State == "pending" {
			item := e.request
			item.Summary = cloneApprovalSummary(item.Summary)
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func (b *Broker) Detail(id string) (contract.ToolReviewApprovalDetail, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[id]
	if e == nil || !e.published {
		return contract.ToolReviewApprovalDetail{}, ErrApprovalGone
	}
	b.refresh(e)
	if e.approval.State != "pending" {
		return contract.ToolReviewApprovalDetail{}, ErrApprovalGone
	}
	detail := e.detail
	detail.Calls = append([]contract.ToolReviewApprovalCall(nil), detail.Calls...)
	return detail, nil
}

func (b *Broker) Decide(id, decision string) (contract.ToolReviewApproval, error) {
	if decision != "allow" && decision != "deny" {
		return contract.ToolReviewApproval{}, ErrApprovalInput
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[id]
	if e == nil || !e.published {
		return contract.ToolReviewApproval{}, ErrApprovalGone
	}
	b.refresh(e)
	if e.approval.State != "pending" {
		if e.approval.State == "approved" || e.approval.State == "denied" {
			return contract.ToolReviewApproval{}, ErrApprovalConflict
		}
		return contract.ToolReviewApproval{}, ErrApprovalGone
	}
	state := "denied"
	if decision == "allow" {
		state = "approved"
	}
	b.finish(e, state)
	return e.approval, nil
}

func cloneApprovalSummary(s contract.ToolReviewSummary) contract.ToolReviewSummary {
	// Round-trip-free deep copy of metadata pointers; callers cannot mutate the
	// pending queue via its observer projection.
	s.Calls = append([]contract.ToolReviewCall(nil), s.Calls...)
	if s.Approval != nil {
		a := *s.Approval
		s.Approval = &a
	}
	if s.BatchCalls != nil {
		v := *s.BatchCalls
		s.BatchCalls = &v
	}
	if s.RejectThreshold != nil {
		v := *s.RejectThreshold
		s.RejectThreshold = &v
	}
	for i := range s.Calls {
		if s.Calls[i].PassProbability != nil {
			v := *s.Calls[i].PassProbability
			s.Calls[i].PassProbability = &v
		}
		if s.Calls[i].FlagProbability != nil {
			v := *s.Calls[i].FlagProbability
			s.Calls[i].FlagProbability = &v
		}
	}
	return s
}

// CanAskOnError accepts only reviewer-service failures. Any validation or
// collection failure in a mixed batch makes the whole batch ineligible.
func CanAskOnError(summary contract.ToolReviewSummary) bool {
	if summary.Status != "failed" && summary.Status != "flagged" {
		return false
	}
	failed := false
	for _, call := range summary.Calls {
		if call.Verdict != "error" {
			continue
		}
		failed = true
		switch call.ErrorCode {
		case "timeout", "transport_error", "unauthorized", "rate_limited", "upstream_error", "reviewer_invalid_json", "reviewer_invalid_review", "reviewer_invalid_reason", "reviewer_invalid_model":
		default:
			return false
		}
	}
	return failed
}
