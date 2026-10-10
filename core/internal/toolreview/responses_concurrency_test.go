package toolreview_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func awaitResponsesSummary(t *testing.T, done <-chan contract.ToolReviewSummary) contract.ToolReviewSummary {
	t.Helper()
	select {
	case s := <-done:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("review did not join after cancellation")
		return contract.ToolReviewSummary{}
	}
}

func TestResponsesBudgetStartsAtReviewAndParentDeadlineWins(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, responsesEnvelopeText(responsesText(responsesInput(1).Calls)))
	}))
	defer server.Close()
	for _, batch := range []bool{false, true} {
		for _, timeoutMS := range []int{5001, 30000, 120000} {
			deadlines := make(chan time.Time, 1)
			base := server.Client().Transport
			var reviewer toolreview.Reviewer = toolreview.NewClient(&http.Client{Transport: observingTransport(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok || r.GetBody != nil {
					t.Error("missing whole-Review budget or POST replay enabled")
				}
				deadlines <- deadline
				return base.RoundTrip(r)
			})})
			snapshot := responsesSnapshot(server.URL, batch, contract.ToolReviewModeReject)
			snapshot.Settings.TimeoutMS = timeoutMS
			s := reviewer.Review(context.Background(), snapshot, responsesInput(1))
			if s.Status != "passed" {
				t.Fatalf("deadline review failed: %+v", s)
			}
			if got, want := <-deadlines, s.StartedAt.Add(time.Duration(timeoutMS)*time.Millisecond); !got.Equal(want) {
				t.Fatalf("render excluded or budget clamped: got=%v want=%v", got, want)
			}
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
			parent, _ := ctx.Deadline()
			s = reviewer.Review(ctx, snapshot, responsesInput(1))
			cancel()
			if got := <-deadlines; !got.Equal(parent) || s.Status != "passed" {
				t.Fatalf("parent deadline escaped: %+v got=%v parent=%v", s, got, parent)
			}
		}
	}
}

func TestResponsesCancellationAndNoHTTPRetry(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmtBool(batch), func(t *testing.T) {
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				entered <- struct{}{}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan contract.ToolReviewSummary, 1)
			joined := make(chan struct{})
			t.Cleanup(func() { cancel(); close(release); <-joined; server.Close() })
			go func() {
				defer close(joined)
				done <- responsesReviewer(server).Review(ctx, responsesSnapshot(server.URL, batch, contract.ToolReviewModeAsk), responsesInput(1))
			}()
			select {
			case <-entered:
			case s := <-done:
				t.Fatalf("review returned before blocking HTTP: %+v", s)
			case <-time.After(2 * time.Second):
				t.Fatal("HTTP did not enter")
			}
			cancel()
			s := awaitResponsesSummary(t, done)
			if s.Status != "failed" || s.ErrorCode != "cancelled" || toolreview.CanAskOnError(s) || requests.Load() != 1 || s.Calls[0].Action != "withheld" {
				t.Fatalf("cancel retried or eligible for approval: %+v requests=%d", s, requests.Load())
			}
		})
	}
}

func TestResponsesSharedSlotsBoundQueueAndCancellation(t *testing.T) {
	entered := make(chan struct{}, 64)
	release := make(chan struct{})
	var requests, active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan contract.ToolReviewSummary, 1)
	joined := make(chan struct{})
	t.Cleanup(func() { cancel(); close(release); <-joined; server.Close() })
	// Sixty-four singleton jobs must consume no more than sixteen shared slots.
	go func() {
		defer close(joined)
		done <- responsesReviewer(server).Review(ctx, responsesSnapshot(server.URL, false, contract.ToolReviewModeAsk), responsesInput(64))
	}()
	for range 16 {
		select {
		case <-entered:
		case s := <-done:
			t.Fatalf("workers returned before filling slots: %+v", s)
		case <-time.After(2 * time.Second):
			t.Fatal("global slots not filled")
		}
	}
	// Both backends, and batch singleton/multiple jobs, must queue behind the
	// same pool. A separate backend semaphore would produce a seventeenth POST.
	for _, tc := range []struct {
		backend contract.ToolReviewBackend
		batch   bool
		n       int
	}{
		{contract.ToolReviewBackendResponses, false, 1},
		{contract.ToolReviewBackendResponses, true, 1},
		{contract.ToolReviewBackendResponses, true, 3},
		{contract.ToolReviewBackendJev, false, 1},
	} {
		snapshot := responsesSnapshot(server.URL, tc.batch, contract.ToolReviewModeAsk)
		snapshot.Settings.Backend, snapshot.Settings.TimeoutMS = tc.backend, 25
		queued := make(chan contract.ToolReviewSummary, 1)
		queuedJoined := make(chan struct{})
		qctx, qcancel := context.WithCancel(context.Background())
		go func() {
			defer close(queuedJoined)
			queued <- responsesReviewer(server).Review(qctx, snapshot, responsesInput(tc.n))
		}()
		select {
		case s := <-queued:
			if s.Status != "failed" || s.ErrorCode != "timeout" || !toolreview.CanAskOnError(s) {
				t.Errorf("queue budget failure: %+v", s)
			}
		case <-entered:
			t.Error("queue escaped process-wide semaphore")
			qcancel()
		case <-time.After(2 * time.Second):
			t.Error("queued review did not stop at deadline")
			qcancel()
		}
		qcancel()
		<-queuedJoined
	}
	cancel()
	s := awaitResponsesSummary(t, done)
	if s.Status != "failed" || s.ErrorCode != "cancelled" || requests.Load() != 16 || peak.Load() > 16 {
		t.Fatalf("bound/cancellation failure: %+v requests=%d peak=%d", s, requests.Load(), peak.Load())
	}
	for _, call := range s.Calls {
		if call.ErrorCode != "cancelled" || call.Verdict != "error" || call.Action != "withheld" {
			t.Fatalf("pending worker leaked or completed: %+v", call)
		}
	}
}

func TestResponsesBatchUsesOneSlotAndOnePOST(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
			_, _ = io.WriteString(w, responsesEnvelopeText(responsesText(responsesInput(64).Calls)))
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan contract.ToolReviewSummary, 1)
	joined := make(chan struct{})
	// release is closed below on the success path, or by cleanup on failure.
	released := false
	t.Cleanup(func() {
		cancel()
		if !released {
			close(release)
		}
		<-joined
		server.Close()
	})
	go func() {
		defer close(joined)
		done <- responsesReviewer(server).Review(ctx, responsesSnapshot(server.URL, true, contract.ToolReviewModeAsk), responsesInput(64))
	}()
	select {
	case <-entered:
	case s := <-done:
		t.Fatalf("batch returned without HTTP: %+v", s)
	case <-time.After(2 * time.Second):
		t.Fatal("batch did not enter")
	}
	// A separate completed singleton Review proves the batch did not reserve
	// one slot per call or monopolize all sixteen slots.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, responsesEnvelopeText(responsesText(responsesInput(1).Calls)))
	}))
	defer other.Close()
	snapshot := responsesSnapshot(other.URL, true, contract.ToolReviewModeReject)
	snapshot.Settings.TimeoutMS = 1000
	if s := responsesReviewer(other).Review(context.Background(), snapshot, responsesInput(1)); s.Status != "passed" {
		t.Fatalf("batch acquired extra slots: %+v", s)
	}
	close(release)
	released = true
	if s := awaitResponsesSummary(t, done); s.Status != "passed" || requests.Load() != 1 {
		t.Fatalf("batch split/retried: %+v requests=%d", s, requests.Load())
	}
}

func TestResponsesTransportFailureIsSafeAndNeverApproves(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("synthetic failed transport reached HTTP") }))
	defer server.Close()
	var attempts atomic.Int32
	var reviewer toolreview.Reviewer = toolreview.NewClient(&http.Client{Transport: observingTransport(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		if r.GetBody != nil {
			t.Error("transport replay enabled")
		}
		return nil, io.ErrUnexpectedEOF
	})})
	for _, enabled := range []bool{false, true} {
		snapshot := responsesSnapshot(server.URL, true, contract.ToolReviewModeAsk)
		snapshot.Settings.AskOnReviewError = enabled
		s := reviewer.Review(context.Background(), snapshot, responsesInput(2))
		if s.Status != "failed" || s.ErrorCode != "transport_error" || !toolreview.CanAskOnError(s) || s.Approval != nil {
			t.Fatalf("opt-in fabricated approval/pass: %+v", s)
		}
		data, _ := json.Marshal(s)
		if len(data) == 0 {
			t.Fatal("missing safe summary")
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("transport retried: %d", attempts.Load())
	}
}
