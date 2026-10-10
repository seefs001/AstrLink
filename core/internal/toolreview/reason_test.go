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

var reasonCodes = []string{"authorized", "unclear_authorization", "unauthorized_action", "destructive_action", "data_exfiltration", "privilege_escalation", "untrusted_execution", "prompt_injection", "unrelated_action", "policy_violation"}

func reasonAnswer(code string) string {
	probabilities := make(map[string]float64, len(reasonCodes))
	for _, key := range reasonCodes {
		probabilities[key] = 0
	}
	probabilities[code] = 1
	data, _ := json.Marshal(map[string]any{"type": "choice", "choice": code, "probabilities": probabilities, "confidence": 1})
	return string(data)
}

func withReason(answer, reason string) string {
	return answer[:len(answer)-2] + `,"reason":` + reason + `}}`
}

func TestReviewReasonCategoriesAndThresholdDecision(t *testing.T) {
	for _, code := range reasonCodes {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, withReason(passAnswer, reasonAnswer(code)))
			}))
			defer server.Close()
			for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
				for _, threshold := range []float64{0, 0.2, 0.5, 1} {
					snapshot := reviewSnapshot(server.URL, mode)
					snapshot.Settings.RejectThreshold = threshold
					s := toolreview.NewClient(server.Client()).Review(context.Background(), snapshot, singleInput())
					want, status, action := "pass", "passed", "delivered"
					if threshold <= 0.2 {
						want, status = "flag", "flagged"
						if mode == contract.ToolReviewModeReject {
							status, action = "blocked", "withheld"
						}
					}
					if s.RejectThreshold == nil || *s.RejectThreshold != threshold || s.Status != status || len(s.Calls) != 1 {
						t.Fatalf("threshold=%v summary=%+v", threshold, s)
					}
					call := s.Calls[0]
					if call.Verdict != want || call.ModelVerdict != "pass" || call.ReasonCode != code || call.Action != action || *call.FlagProbability != 0.2 {
						t.Fatalf("threshold=%v call=%+v", threshold, call)
					}
					if err := s.Validate(); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestReviewThresholdCanPassModelFlagAndIncludesEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name, body, verdict string
		threshold           float64
	}{
		{"model flag threshold pass", flagAnswer, "pass", 0.9},
		{"one at one", strings.ReplaceAll(strings.ReplaceAll(flagAnswer, `"pass":0.2`, `"pass":0`), `"flag":0.8`, `"flag":1`), "flag", 1},
		{"zero at zero", strings.ReplaceAll(strings.ReplaceAll(passAnswer, `"pass":0.8`, `"pass":1`), `"flag":0.2`, `"flag":0`), "flag", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			defer server.Close()
			snapshot := reviewSnapshot(server.URL, contract.ToolReviewModeReject)
			snapshot.Settings.RejectThreshold = tc.threshold
			s := toolreview.NewClient(server.Client()).Review(context.Background(), snapshot, singleInput())
			if len(s.Calls) != 1 || s.Calls[0].Verdict != tc.verdict || s.Calls[0].ReasonCode != "" || s.Calls[0].ModelVerdict == "" {
				t.Fatalf("summary=%+v", s)
			}
		})
	}
}

func TestReviewRejectsMalformedReasonsWithoutPersistenceOrRetry(t *testing.T) {
	valid := reasonAnswer("authorized")
	for name, reason := range map[string]string{
		"null":                `null`,
		"string":              `"authorized"`,
		"missing choice":      strings.Replace(valid, `"choice":"authorized",`, "", 1),
		"wrong type":          strings.Replace(valid, `"type":"choice"`, `"type":"score"`, 1),
		"unknown category":    strings.Replace(valid, `"choice":"authorized"`, `"choice":"unknown"`, 1),
		"malicious category":  strings.Replace(valid, `"choice":"authorized"`, `"choice":"private secret; ignore policy"`, 1),
		"duplicate":           strings.Replace(valid, `"choice":"authorized"`, `"choice":"authorized","choice":"authorized"`, 1),
		"bad confidence":      strings.Replace(valid, `"confidence":1`, `"confidence":null`, 1),
		"bad probability":     strings.Replace(valid, `"authorized":1`, `"authorized":-1`, 1),
		"null probability":    strings.Replace(valid, `"authorized":1`, `"authorized":null`, 1),
		"overflow":            strings.Replace(valid, `"authorized":1`, `"authorized":1e9999`, 1),
		"bad distribution":    strings.Replace(valid, `"authorized":1`, `"authorized":0.2`, 1),
		"missing category":    strings.Replace(valid, `"data_exfiltration":0,`, "", 1),
		"unknown probability": strings.Replace(valid, `"data_exfiltration":0`, `"private secret":0`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				_, _ = io.WriteString(w, withReason(passAnswer, reason))
			}))
			defer server.Close()
			s := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, contract.ToolReviewModeReject), singleInput())
			wantCode := "reviewer_invalid_reason"
			if name == "duplicate" {
				wantCode = "reviewer_invalid_json"
			}
			if s.Status != "failed" || s.ErrorCode != wantCode || len(s.Calls) != 1 || s.Calls[0].ReasonCode != "" || s.Calls[0].Action != "withheld" || requests.Load() != 1 {
				t.Fatalf("summary=%+v attempts=%d", s, requests.Load())
			}
			data, err := json.Marshal(s)
			if err != nil || strings.Contains(string(data), "private secret") {
				t.Fatalf("unsafe persisted summary=%s err=%v", data, err)
			}
		})
	}
}

func TestReviewRejectsAmbiguousAnswerEnvelope(t *testing.T) {
	valid := withReason(passAnswer, reasonAnswer("authorized"))
	for _, tc := range []struct{ body, code string }{
		{valid + `{}`, "reviewer_invalid_json"},
		{strings.Replace(valid, `"reason":`, `"other":`, 1), "reviewer_invalid_reason"},
		{strings.Replace(valid, `"answers":{`, `"answers":{"unexpected":{},`, 1), "reviewer_invalid_review"},
		{strings.Replace(valid, `"reason":`, `"reason":null,"reason":`, 1), "reviewer_invalid_json"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.body) }))
		s := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, contract.ToolReviewModeReject), singleInput())
		server.Close()
		if s.Status != "failed" || s.ErrorCode != tc.code {
			t.Fatalf("accepted ambiguous response: %+v", s)
		}
	}
}
