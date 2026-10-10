package toolreview

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

func approvalFixture() (contract.ToolReviewSummary, Input) {
	s := contract.ToolReviewSummary{Status: "flagged", Mode: contract.ToolReviewModeAsk, Model: "jev", StartedAt: time.Now(), Calls: []contract.ToolReviewCall{{Index: 0, Name: "bash", Verdict: "flag", ModelVerdict: "flag", Action: "withheld"}}}
	return s, Input{UserRequest: "private user text", UserRequestAvailable: true, Calls: []Call{{Index: 0, Name: "bash", Arguments: `{"command":"private arguments"}`}}}
}

func pendingApproval(t *testing.T, b *Broker, done <-chan contract.ToolReviewApproval) contract.ToolReviewApprovalRequest {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		if items := b.List(); len(items) > 0 {
			return items[0]
		}
		select {
		case a := <-done:
			t.Fatalf("approval ended before publication: %+v", a)
		case <-timer.C:
			t.Fatal("approval was not published")
		default:
			runtime.Gosched()
		}
	}
}

func startApproval(t *testing.T, b *Broker, ctx context.Context) <-chan contract.ToolReviewApproval {
	t.Helper()
	return startApprovalWithTimeout(t, b, ctx, time.Minute)
}

func startApprovalWithTimeout(t *testing.T, b *Broker, parent context.Context, timeout time.Duration) <-chan contract.ToolReviewApproval {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan contract.ToolReviewApproval, 1)
	stopped := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		b.Close()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("approval waiter did not stop during cleanup")
		}
	})
	s, input := approvalFixture()
	go func() {
		defer close(stopped)
		a, err := b.Wait(ctx, "request_test", s, input, timeout, func(contract.ToolReviewApproval) error { return nil })
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		done <- a
	}()
	return done
}

func TestApprovalBrokerSingleDecisionAndCleanup(t *testing.T) {
	for _, decision := range []string{"allow", "deny"} {
		t.Run(decision, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := NewBroker(ctx)
			defer b.Close()
			done := startApproval(t, b, ctx)
			item := pendingApproval(t, b, done)
			if item.Summary.Status != "pending" || item.Summary.Approval.ReviewStatus != "flagged" || item.Summary.Calls[0].Action != "withheld" {
				t.Fatalf("unsafe projection: %+v", item)
			}
			detail, err := b.Detail(item.ID)
			if err != nil || detail.UserRequest != "private user text" || len(detail.Calls) != 1 {
				t.Fatalf("detail: %+v %v", detail, err)
			}
			var wg sync.WaitGroup
			var successes atomic.Int32
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := b.Decide(item.ID, decision); err == nil {
						successes.Add(1)
					} else if !errors.Is(err, ErrApprovalGone) && !errors.Is(err, ErrApprovalConflict) {
						t.Errorf("decision: %v", err)
					}
				}()
			}
			wg.Wait()
			if successes.Load() != 1 {
				t.Fatalf("successful decisions=%d", successes.Load())
			}
			select {
			case a := <-done:
				want := "denied"
				if decision == "allow" {
					want = "approved"
				}
				if a.State != want {
					t.Fatalf("decision=%+v", a)
				}
			case <-time.After(time.Second):
				t.Fatal("wait did not finish")
			}
			if len(b.List()) != 0 {
				t.Fatal("consumed approval retained")
			}
			if _, err := b.Detail(item.ID); !errors.Is(err, ErrApprovalGone) {
				t.Fatalf("detail survived: %v", err)
			}
		})
	}
}

func TestApprovalBrokerCancellationExpiryAndShutdownPrecedeAllow(t *testing.T) {
	for _, state := range []string{"cancelled", "expired", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := NewBroker(context.Background())
			defer b.Close()
			done := startApproval(t, b, ctx)
			item := pendingApproval(t, b, done)
			switch state {
			case "cancelled":
				cancel()
			case "interrupted":
				b.Close()
			case "expired":
				b.mu.Lock()
				b.entries[item.ID].approval.ExpiresAt = time.Now().Add(-time.Second)
				b.mu.Unlock()
			}
			if _, err := b.Decide(item.ID, "allow"); !errors.Is(err, ErrApprovalGone) {
				t.Fatalf("late allow=%v", err)
			}
			select {
			case a := <-done:
				if a.State != state {
					t.Fatalf("state=%s want %s", a.State, state)
				}
			case <-time.After(time.Second):
				t.Fatal("cancel did not wake waiter")
			}
			if len(b.List()) != 0 {
				t.Fatal("terminal approval remains pending")
			}
		})
	}
}

func TestApprovalBrokerTimerAndCoreContext(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "timer", true: "core context"}[shutdown], func(t *testing.T) {
			core, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := NewBroker(core)
			defer b.Close()
			done := startApprovalWithTimeout(t, b, context.Background(), time.Second)
			pendingApproval(t, b, done)
			if shutdown {
				cancel()
			}
			select {
			case a := <-done:
				want := "expired"
				if shutdown {
					want = "interrupted"
				}
				if a.State != want {
					t.Fatalf("state=%s", a.State)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("wait did not terminate")
			}
		})
	}
}

func TestApprovalBrokerPersistsBeforePublicationAndFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := NewBroker(ctx)
	defer b.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	stopped := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		b.Close()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("persisting approval waiter did not stop during cleanup")
		}
	})
	s, input := approvalFixture()
	go func() {
		defer close(stopped)
		_, err := b.Wait(ctx, "request_test", s, input, time.Minute, func(a contract.ToolReviewApproval) error {
			close(entered)
			select {
			case <-release:
				return errors.New("private store error")
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("Wait ended before persistence: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Wait did not enter persistence")
	}
	if len(b.List()) != 0 {
		t.Fatal("published before storage acknowledgment")
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrApprovalStore) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait stuck")
	}
	if len(b.entries) != 0 {
		t.Fatal("failed admission leaked capacity or raw data")
	}
}

func TestApprovalBrokerCancelledPersistenceExitsWithoutPublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := NewBroker(ctx)
	entered, stopped := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		b.Close()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("cancelled persistence did not stop during cleanup")
		}
	})
	s, input := approvalFixture()
	go func() {
		defer close(stopped)
		_, err := b.Wait(ctx, "request_test", s, input, time.Minute, func(contract.ToolReviewApproval) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		})
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("Wait ended before persistence: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Wait did not enter persistence")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrApprovalStore) {
			t.Fatalf("cancelled persistence error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled persistence did not finish")
	}
	if len(b.List()) != 0 {
		t.Fatal("cancelled persistence published an approval")
	}
}

func TestApprovalBrokerBoundedQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := NewBroker(ctx)
	defer b.Close()
	done := make(chan struct{}, ApprovalCapacity)
	stopped := make(chan struct{})
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		b.Close()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("approval queue workers did not stop during cleanup")
		}
	})
	s, input := approvalFixture()
	for i := 0; i < ApprovalCapacity; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := b.Wait(ctx, "request_test", s, input, time.Minute, func(contract.ToolReviewApproval) error { return nil })
			if err != nil {
				t.Errorf("admission=%v", err)
			}
			done <- struct{}{}
		}()
	}
	go func() { workers.Wait(); close(stopped) }()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for len(b.List()) != ApprovalCapacity {
		select {
		case <-timer.C:
			t.Fatal("queue did not fill")
		default:
			runtime.Gosched()
		}
	}
	if _, err := b.Wait(ctx, "request_overflow", s, input, time.Minute, func(contract.ToolReviewApproval) error { t.Error("overflow persisted"); return ErrApprovalQueueFull }); !errors.Is(err, ErrApprovalQueueFull) {
		t.Fatalf("overflow=%v", err)
	}
	cancel()
	for i := 0; i < ApprovalCapacity; i++ {
		select {
		case <-done:
		case <-timer.C:
			t.Fatal("cleanup stuck")
		}
	}
	if len(b.List()) != 0 {
		t.Fatal("cancel leaked approvals")
	}
}

func TestApprovalBrokerConcurrentCancelAndDecide(t *testing.T) {
	for i := 0; i < 32; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		b := NewBroker(context.Background())
		done := startApproval(t, b, ctx)
		item := pendingApproval(t, b, done)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; cancel() }()
		go func() { defer wg.Done(); <-start; _, _ = b.Decide(item.ID, "allow") }()
		close(start)
		wg.Wait()
		select {
		case a := <-done:
			if a.State != "approved" && a.State != "cancelled" {
				t.Fatalf("race state=%s", a.State)
			}
		case <-time.After(time.Second):
			t.Fatal("cancel/decide leaked waiter")
		}
		b.Close()
		if len(b.List()) != 0 {
			t.Fatal("race left pending detail")
		}
	}
}

func TestApprovalBrokerRejectsInvalidOrOversizedInput(t *testing.T) {
	for _, kind := range []string{"user", "arguments", "utf8", "calls", "duplicate", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			b := NewBroker(context.Background())
			defer b.Close()
			s, input := approvalFixture()
			switch kind {
			case "user":
				input.UserRequest = string(make([]byte, contract.MaxToolReviewInputBytes+1))
			case "arguments":
				input.Calls[0].Arguments = string(make([]byte, contract.MaxToolReviewInputBytes+1))
			case "utf8":
				input.Calls[0].Custom = true
				input.Calls[0].Arguments = string([]byte{255})
			case "calls":
				input.Calls = make([]Call, contract.MaxToolReviewCalls+1)
			case "duplicate":
				input.Calls = append(input.Calls, input.Calls[0])
			case "invalid":
				input.Calls[0].Arguments = "broken"
			}
			if _, err := b.Wait(context.Background(), "request_test", s, input, time.Minute, func(contract.ToolReviewApproval) error {
				t.Error("invalid input reached persistence")
				return ErrApprovalInput
			}); !errors.Is(err, ErrApprovalInput) {
				t.Fatalf("invalid input error=%v", err)
			}
			if len(b.List()) != 0 {
				t.Fatal("invalid input published")
			}
		})
	}
}

func TestApprovalErrorEligibility(t *testing.T) {
	for _, code := range []string{"timeout", "upstream_error", "reviewer_invalid_json", "reviewer_invalid_review", "reviewer_invalid_reason", "invalid_config", "credential_missing", "invalid_call", "input_limit", "invalid_response", "invalid_arguments", "cancelled"} {
		s, _ := approvalFixture()
		s.Status = "failed"
		s.Calls[0].Verdict = "error"
		s.Calls[0].ErrorCode = code
		want := code == "timeout" || code == "upstream_error" || code == "reviewer_invalid_json" || code == "reviewer_invalid_review" || code == "reviewer_invalid_reason"
		if got := CanAskOnError(s); got != want {
			t.Errorf("%s eligibility=%v", code, got)
		}
	}
}
