package toolreview_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func TestBatchRenderedInputExactLimitIncludesEnvelopeAndQuestions(t *testing.T) {
	lengths := make(chan int, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		lengths <- len(data)
		_, _ = io.WriteString(w, batchAnswer(1))
	}))
	defer server.Close()
	client := toolreview.NewClient(server.Client())
	snapshot, input := batchSnapshot(server.URL, contract.ToolReviewModeReject), batchInput(1)
	if s := client.Review(context.Background(), snapshot, input); s.Status != "passed" {
		t.Fatalf("baseline failed: %+v", s)
	}
	baseLength := <-lengths
	// User data is plain ASCII: every added byte costs exactly one JSON byte.
	input.UserRequest = strings.Repeat("u", (128<<10)-baseLength+len(input.UserRequest))
	if s := client.Review(context.Background(), snapshot, input); s.Status != "passed" {
		t.Fatalf("exact 128 KiB rejected: %+v", s)
	}
	if n := <-lengths; n != 128<<10 {
		t.Fatalf("exact limit fixture is %d bytes", n)
	}
	input.UserRequest += "u"
	if s := client.Review(context.Background(), snapshot, input); s.Status != "failed" || s.ErrorCode != "input_limit" || toolreview.CanAskOnError(s) {
		t.Fatalf("128 KiB + 1 allowed/bypassed: %+v", s)
	}
	select {
	case n := <-lengths:
		t.Fatalf("render limit still sent HTTP (%d bytes)", n)
	default:
	}
}

func TestBatchSingletonResponseKeeps64KiBBound(t *testing.T) {
	var requests atomic.Int32
	body := batchAnswer(1) + strings.Repeat(" ", 64<<10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	s := toolreview.NewClient(server.Client()).Review(context.Background(), batchSnapshot(server.URL, contract.ToolReviewModeAsk), batchInput(1))
	if s.Status != "failed" || s.ErrorCode != "reviewer_invalid_json" || requests.Load() != 1 || !toolreview.CanAskOnError(s) {
		t.Fatalf("singleton batch widened response bound or retried: %+v requests=%d", s, requests.Load())
	}
}

func TestBatchBudgetStartsBeforeRenderingAndIsNotClamped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, batchAnswer(3))
	}))
	defer server.Close()
	for _, timeoutMS := range []int{5001, 30000, 120000} {
		deadlines := make(chan time.Time, 1)
		base := server.Client().Transport
		client := toolreview.NewClient(&http.Client{Transport: observingTransport(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok || r.GetBody != nil {
				t.Error("missing shared deadline or POST opted into replay")
			}
			deadlines <- deadline
			return base.RoundTrip(r)
		})})
		snapshot := batchSnapshot(server.URL, contract.ToolReviewModeReject)
		snapshot.Settings.TimeoutMS = timeoutMS
		s := client.Review(context.Background(), snapshot, batchInput(3))
		if s.Status != "passed" {
			t.Fatalf("configured batch deadline failed: %+v", s)
		}
		select {
		case deadline := <-deadlines:
			want := s.StartedAt.Add(time.Duration(timeoutMS) * time.Millisecond)
			if !deadline.Equal(want) {
				t.Fatalf("deadline starts after rendering or is clamped: got=%v want=%v", deadline, want)
			}
		default:
			t.Fatal("no HTTP observed")
		}
	}
}

func TestBatchExpiredBudgetCannotDisguiseLocalValidationFailures(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	for _, tc := range []struct {
		code   string
		change func(*toolreview.Input)
	}{
		{"invalid_arguments", func(i *toolreview.Input) { i.Calls[63].Arguments = "invalid" }},
		{"input_limit", func(i *toolreview.Input) {
			i.Calls[63].Custom, i.Calls[63].Arguments = true, strings.Repeat("x", 20<<10)
		}},
	} {
		t.Run(fmt.Sprintf("expired/%s", tc.code), func(t *testing.T) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			input := batchInput(64)
			tc.change(&input)
			s := toolreview.NewClient(server.Client()).Review(ctx, batchSnapshot(server.URL, contract.ToolReviewModeAsk), input)
			if s.Status != "failed" || s.ErrorCode != tc.code || toolreview.CanAskOnError(s) || requests.Load() != 0 {
				t.Fatalf("expired budget disguised a local validation failure: %+v", s)
			}
		})
	}
}

func TestBatchExpiredRenderBudgetAndParentDeadlineNeverSend(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	for _, cancelOnly := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelOnly {
			cancel()
		} else {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}
		s := toolreview.NewClient(server.Client()).Review(ctx, batchSnapshot(server.URL, contract.ToolReviewModeReject), batchInput(64))
		cancel()
		code := "timeout"
		if cancelOnly {
			code = "cancelled"
		}
		if s.Status != "failed" || s.ErrorCode != code || requests.Load() != 0 {
			t.Fatalf("expired render budget sent HTTP: %+v requests=%d", s, requests.Load())
		}
		for _, call := range s.Calls {
			if call.ErrorCode != code || call.Verdict != "error" || call.Action != "withheld" {
				t.Fatalf("expired render batch partially succeeded: %+v", s)
			}
		}
	}
}
