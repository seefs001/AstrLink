package toolreview_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func TestResponsesSameNameIndependentIndexDecisions(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmtBool(batch), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var body struct {
					Input string `json:"input"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				var state struct {
					Calls []struct {
						Index int `json:"index"`
					} `json:"tool_calls"`
				}
				_ = json.Unmarshal([]byte(body.Input), &state)
				var results []string
				for _, call := range state.Calls {
					result := `{"index":11,"verdict":"flag","reason_code":"authorized","pass_probability":0.9,"flag_probability":0.1}`
					if call.Index == 18 {
						result = `{"index":18,"verdict":"pass","reason_code":"policy_violation","pass_probability":0.1,"flag_probability":0.9}`
					}
					results = append([]string{result}, results...)
				}
				_, _ = io.WriteString(w, responsesEnvelopeText(`{"results":[`+strings.Join(results, ",")+`]}`))
			}))
			defer server.Close()
			s := responsesReviewer(server).Review(context.Background(), responsesSnapshot(server.URL, batch, contract.ToolReviewModeReject), responsesInput(2))
			wantRequests := int32(2)
			if batch {
				wantRequests = 1
			}
			if s.Status != "blocked" || requests.Load() != wantRequests || len(s.Calls) != 2 {
				t.Fatalf("index decisions failed: %+v", s)
			}
			a, b := s.Calls[0], s.Calls[1]
			if a.Index != 11 || a.Verdict != "pass" || a.ModelVerdict != "flag" || a.ReasonCode != "authorized" || *a.FlagProbability != 0.1 || b.Index != 18 || b.Verdict != "flag" || b.ModelVerdict != "pass" || b.ReasonCode != "policy_violation" || *b.FlagProbability != 0.9 || a.Name != b.Name || a.Action != "withheld" || b.Action != "withheld" {
				t.Fatalf("same-name decisions conflated or matched by position: %+v", s)
			}
			if err := s.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResponsesSingletonErrorsAndSharedAggregation(t *testing.T) {
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject, contract.ToolReviewModeAsk} {
		t.Run(string(mode), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var body struct {
					Input string `json:"input"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				var state struct {
					Calls []struct {
						Index int `json:"index"`
					} `json:"tool_calls"`
				}
				_ = json.Unmarshal([]byte(body.Input), &state)
				if len(state.Calls) != 1 {
					t.Error("singleton protocol batched")
				}
				if state.Calls[0].Index == 11 {
					_, _ = io.WriteString(w, responsesEnvelopeText(`{"results":[{"index":11,"verdict":"pass","reason_code":"policy_violation","pass_probability":0.1,"flag_probability":0.9}]}`))
				} else {
					_, _ = io.WriteString(w, responsesEnvelopeText(`{"results":[{"index":18,"verdict":"pass","reason_code":"private-response","pass_probability":0.8,"flag_probability":0.2}]}`))
				}
			}))
			defer server.Close()
			s := responsesReviewer(server).Review(context.Background(), responsesSnapshot(server.URL, false, mode), responsesInput(2))
			status, action := "flagged", "delivered"
			if mode == contract.ToolReviewModeReject {
				status, action = "blocked", "withheld"
			}
			if mode == contract.ToolReviewModeAsk {
				status, action = "failed", "withheld"
			}
			if s.Status != status || s.ErrorCode != "reviewer_invalid_reason" || requests.Load() != 2 || len(s.Calls) != 2 || s.Calls[0].Verdict != "flag" || s.Calls[1].Verdict != "error" || s.Calls[1].FlagProbability != nil || s.Calls[1].ReasonCode != "" || s.Calls[0].Action != action || s.Calls[1].Action != action {
				t.Fatalf("shared aggregation changed: %+v", s)
			}
			if s.Approval != nil {
				t.Fatal("failed judgment fabricated an approval")
			}
			if err := s.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResponsesBatchFullRenderIncludesAllCalls(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	input := responsesInput(2)
	for i := range input.Calls {
		input.Calls[i].Custom, input.Calls[i].Arguments = true, strings.Repeat("x", 65<<10)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := responsesReviewer(server).Review(ctx, responsesSnapshot(server.URL, true, contract.ToolReviewModeAsk), input)
	if s.Status != "failed" || s.ErrorCode != "input_limit" || toolreview.CanAskOnError(s) || requests.Load() != 0 {
		t.Fatalf("split or timeout disguised combined render overflow: %+v", s)
	}
}
