package toolreview_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

const batchPassChoice = `{"type":"choice","choice":"pass","probabilities":{"pass":0.8,"flag":0.2},"confidence":0.6}`

func batchSnapshot(endpoint string, mode contract.ToolReviewMode) toolreview.Snapshot {
	snapshot := reviewSnapshot(endpoint, mode)
	snapshot.Settings.BatchCalls = true
	return snapshot
}

func batchInput(n int) toolreview.Input {
	input := singleInput()
	input.Calls = make([]toolreview.Call, n)
	for i := range input.Calls {
		input.Calls[i] = toolreview.Call{Index: 7 + 3*i, Name: "same", Arguments: `{}`}
	}
	return input
}

// Deliberately return question keys in reverse order. Position, not wire order,
// name, or the stored index, is the correlation protocol.
func batchAnswer(n int) string {
	entries := make([]string, 0, 2*n)
	for i := n - 1; i >= 0; i-- {
		entries = append(entries, fmt.Sprintf(`"reason_%d":%s`, i, reasonAnswer("authorized")), fmt.Sprintf(`"review_%d":%s`, i, batchPassChoice))
	}
	return `{"model":"jev-test-version","answers":{` + strings.Join(entries, ",") + `},"usage":{"input_tokens":123,"output_tokens":456}}`
}

func TestBatchProtocolPreservesDataAndSingleSharedPolicy(t *testing.T) {
	input := batchInput(3)
	input.Calls[0].Index, input.Calls[1].Index, input.Calls[2].Index = 42, 7, 19
	input.UserRequest = "仅检查本地内容 {{tool_call}} 雪 🐈"
	input.Calls[0].Arguments = `{"n":9007199254740993123456789,"text":"雪{{user_request}}","nested":{"A":1,"a":2}}`
	input.Calls[1].Arguments = `{"path":"/tmp/真实路径","ratio":1.234567890123456789}`
	input.Calls[2].Custom = true
	input.Calls[2].Arguments = "*** Begin Patch\n🐈 {{user_request}}\n*** End Patch\n"
	const policy = "Operator-specific rule: only flag credential exfiltration. {{user_request}} {{tool_call}}"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-only-key" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") == "AstrLink" {
			t.Error("HTTP policy lost")
		}
		if strings.Contains(string(data), "AstrLink") || strings.Contains(string(data), "revision-1") || strings.Count(string(data), `"user_request":`) != 1 || strings.Count(string(data), "Operator-specific rule") != 1 {
			t.Error("local identity or repeated policy/user data")
		}
		var body struct {
			Model string `json:"model"`
			State struct {
				UserRequest string `json:"user_request"`
				ToolCalls   []struct {
					Index     int             `json:"index"`
					Name      string          `json:"name"`
					Type      string          `json:"type"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"tool_calls"`
				ReviewRules string `json:"review_rules"`
				ReasonRules string `json:"reason_rules"`
			} `json:"state"`
			Questions map[string]struct {
				Type         string            `json:"type"`
				Instructions string            `json:"instructions"`
				Criteria     map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Error(err)
			return
		}
		if body.Model != "jev-latest" || body.State.UserRequest != input.UserRequest || len(body.State.ToolCalls) != len(input.Calls) || len(body.Questions) != 2*len(input.Calls) {
			t.Errorf("batch schema/data mismatch: %s", data)
			return
		}
		if !strings.Contains(body.State.ReviewRules, "Operator-specific rule") || strings.Contains(body.State.ReviewRules, "{{") || !strings.Contains(body.State.ReasonRules, "apply only to the separate review question") || !strings.Contains(body.State.ReasonRules, "configured rules determine the scope") {
			t.Error("policy expansion or reason scope lost")
		}
		for i, call := range body.State.ToolCalls {
			original := input.Calls[i]
			if call.Index != original.Index || call.Name != original.Name {
				t.Error("tool metadata lost")
			}
			if original.Custom {
				var text string
				if err := json.Unmarshal(call.Arguments, &text); err != nil || text != original.Arguments || call.Type != "custom" {
					t.Error("custom argument text rewritten")
				}
			} else if string(call.Arguments) != original.Arguments || call.Type != "function" {
				t.Error("function arguments/numbers rewritten")
			}
			for _, prefix := range []string{"review_", "reason_"} {
				question := body.Questions[prefix+strconv.Itoa(i)]
				if question.Type != "choice" || !strings.Contains(question.Instructions, fmt.Sprintf("state.tool_calls[%d] (stored index %d)", i, original.Index)) || !strings.Contains(question.Instructions, "state.review_rules") || strings.Contains(question.Instructions, input.UserRequest) || strings.Contains(question.Instructions, original.Arguments) {
					t.Error("question target, policy reference, or data isolation invalid")
				}
				wantCriteria := 2
				if prefix == "reason_" {
					wantCriteria = 10
					if !strings.Contains(question.Instructions, "state.reason_rules") {
						t.Error("reason rules not applied")
					}
				}
				if len(question.Criteria) != wantCriteria {
					t.Error("question choices lost")
				}
			}
		}
		// Distinct true diagnoses for identical names, despite reversed wire order.
		answer := strings.Replace(batchAnswer(3), `"review_1":`+batchPassChoice, `"review_1":`+strings.ReplaceAll(strings.ReplaceAll(batchPassChoice, `"pass":0.8`, `"pass":0.2`), `"flag":0.2`, `"flag":0.8`), 1)
		answer = strings.Replace(answer, `"reason_1":`+reasonAnswer("authorized"), `"reason_1":`+reasonAnswer("data_exfiltration"), 1)
		_, _ = io.WriteString(w, answer)
	}))
	defer server.Close()
	snapshot := batchSnapshot(server.URL, contract.ToolReviewModeReject)
	snapshot.Settings.Prompt = policy
	s := toolreview.NewClient(server.Client()).Review(context.Background(), snapshot, input)
	if s.Status != "blocked" || requests.Load() != 1 || s.BatchCalls == nil || !*s.BatchCalls || len(s.Calls) != 3 {
		t.Fatalf("batch=%+v requests=%d", s, requests.Load())
	}
	for i, call := range s.Calls {
		want, reason, flag := "pass", "authorized", 0.2
		if i == 1 {
			want, reason, flag = "flag", "data_exfiltration", 0.8
		}
		if call.Index != input.Calls[i].Index || call.Name != input.Calls[i].Name || call.Action != "withheld" || call.Verdict != want || call.ModelVerdict != "pass" || call.ReasonCode != reason || call.FlagProbability == nil || *call.FlagProbability != flag || call.Model != "jev-test-version" {
			t.Fatalf("incorrect call correspondence: %+v", s)
		}
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBatchToggleSingleCallAndLegacyCompatibility(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, n := range []int{0, 1, 3, 64} {
			t.Run(fmt.Sprintf("enabled=%t/calls=%d", enabled, n), func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					data, _ := io.ReadAll(r.Body)
					if enabled {
						if !strings.Contains(string(data), `"review_0"`) || strings.Contains(string(data), `"tool_call":`) || len(data) > 128<<10 {
							t.Error("batch protocol not used or exceeded input bound")
						}
						_, _ = io.WriteString(w, batchAnswer(n))
					} else {
						if !strings.Contains(string(data), `"review":`) || !strings.Contains(string(data), `"tool_call":`) {
							t.Error("legacy protocol changed")
						}
						// Omission of reason and model remains legacy-compatible.
						_, _ = io.WriteString(w, passAnswer)
					}
				}))
				defer server.Close()
				snapshot := reviewSnapshot(server.URL, contract.ToolReviewModeReject)
				snapshot.Settings.BatchCalls = enabled
				s := toolreview.NewClient(server.Client()).Review(context.Background(), snapshot, batchInput(n))
				wantRequests, status := n, "passed"
				if enabled && n > 0 {
					wantRequests = 1
				}
				if n == 0 {
					status = "not_applicable"
				}
				if requests.Load() != int32(wantRequests) || s.Status != status || s.BatchCalls == nil || *s.BatchCalls != enabled || len(s.Calls) != n {
					t.Fatalf("summary=%+v requests=%d", s, requests.Load())
				}
				// The snapshot must not alias mutable caller settings.
				snapshot.Settings.BatchCalls = !enabled
				if *s.BatchCalls != enabled {
					t.Fatal("summary snapshot mutated")
				}
			})
		}
	}
}

func TestBatchInvalidAnswersFailWholeBatchWithoutRetryOrRawData(t *testing.T) {
	valid := batchAnswer(2)
	reason := reasonAnswer("authorized")
	for _, tc := range []struct{ name, body, code string }{
		{"missing review", strings.Replace(valid, `,"review_0":`+batchPassChoice, "", 1), "reviewer_invalid_review"},
		{"missing reason", strings.Replace(valid, `,"reason_0":`+reason, "", 1), "reviewer_invalid_reason"},
		{"flagged sibling with missing reason", strings.Replace(strings.Replace(valid, `"review_0":`+batchPassChoice, `"review_0":`+strings.ReplaceAll(strings.ReplaceAll(batchPassChoice, `"pass":0.8`, `"pass":0.2`), `"flag":0.2`, `"flag":0.8`), 1), `"reason_1":`+reason+`,`, "", 1), "reviewer_invalid_reason"},
		{"extra answer", strings.Replace(valid, `"answers":{`, `"answers":{"review_2":`+batchPassChoice+`,`, 1), "reviewer_invalid_review"},
		{"wrong index", strings.Replace(valid, `"review_0":`, `"review_7":`, 1), "reviewer_invalid_review"},
		{"answer index", strings.Replace(valid, `"review_0":{`, `"review_0":{"index":10,`, 1), "reviewer_invalid_review"},
		{"extra reason field", strings.Replace(valid, `"reason_0":{`, `"reason_0":{"diagnostic":"private-response",`, 1), "reviewer_invalid_reason"},
		{"case key", strings.Replace(valid, `"review_0":`, `"Review_0":`, 1), "reviewer_invalid_review"},
		{"case alias key", strings.Replace(valid, `"answers":{`, `"answers":{"Review_0":`+batchPassChoice+`,`, 1), "reviewer_invalid_review"},
		{"duplicate answer", strings.Replace(valid, `"answers":{`, `"answers":{"review_0":`+batchPassChoice+`,`, 1), "reviewer_invalid_json"},
		{"duplicate property", strings.Replace(valid, `"choice":"pass"`, `"choice":"flag","choice":"pass"`, 1), "reviewer_invalid_json"},
		{"escaped duplicate", strings.Replace(valid, `"answers":{`, `"answers":{"review_\u0030":`+batchPassChoice+`,`, 1), "reviewer_invalid_json"},
		{"case envelope", strings.Replace(valid, `"answers":`, `"Answers":`, 1), "reviewer_invalid_json"},
		{"case choice", strings.Replace(valid, `"choice":"pass"`, `"Choice":"pass"`, 1), "reviewer_invalid_review"},
		{"case probability", strings.Replace(valid, `"pass":0.8`, `"Pass":0.8`, 1), "reviewer_invalid_review"},
		{"case confidence", strings.Replace(valid, `"confidence":0.6`, `"Confidence":0.6`, 1), "reviewer_invalid_review"},
		{"unknown choice", strings.Replace(valid, `"choice":"pass"`, `"choice":"private-response"`, 1), "reviewer_invalid_review"},
		{"wrong type", strings.Replace(valid, `"type":"choice"`, `"type":"score"`, 1), "reviewer_invalid_reason"},
		{"null review", strings.Replace(valid, `"review_1":`+batchPassChoice, `"review_1":null`, 1), "reviewer_invalid_review"},
		{"negative probability", strings.Replace(valid, `"pass":0.8`, `"pass":-0.8`, 1), "reviewer_invalid_review"},
		{"large probability", strings.Replace(valid, `"flag":0.2`, `"flag":1.2`, 1), "reviewer_invalid_review"},
		{"null probability", strings.Replace(valid, `"flag":0.2`, `"flag":null`, 1), "reviewer_invalid_review"},
		{"wrong distribution", strings.Replace(valid, `"flag":0.2`, `"flag":0.1`, 1), "reviewer_invalid_review"},
		{"confidence", strings.Replace(valid, `"confidence":0.6`, `"confidence":-0.1`, 1), "reviewer_invalid_review"},
		{"numeric overflow", strings.Replace(valid, `"confidence":0.6`, `"confidence":1e999`, 1), "reviewer_invalid_review"},
		{"unknown reason", strings.Replace(valid, `"choice":"authorized"`, `"choice":"private-response"`, 1), "reviewer_invalid_reason"},
		{"reason distribution", strings.Replace(valid, `"authorized":1`, `"authorized":0.5`, 1), "reviewer_invalid_reason"},
		{"reason probability", strings.Replace(valid, `"authorized":1`, `"authorized":-1`, 1), "reviewer_invalid_reason"},
		{"reason confidence", strings.Replace(valid, `"confidence":1`, `"confidence":null`, 1), "reviewer_invalid_reason"},
		{"case reason", strings.Replace(valid, `"choice":"authorized"`, `"CHOICE":"authorized"`, 1), "reviewer_invalid_reason"},
		{"missing reason category", strings.Replace(valid, `,"policy_violation":0`, "", 1), "reviewer_invalid_reason"},
		{"unknown reason category", strings.Replace(valid, `"policy_violation":0`, `"private-response":0`, 1), "reviewer_invalid_reason"},
		{"null reason", strings.Replace(valid, `"reason_0":`+reason, `"reason_0":null`, 1), "reviewer_invalid_reason"},
		{"model", strings.Replace(valid, `"jev-test-version"`, `"private-response\n"`, 1), "reviewer_invalid_model"},
		{"null model", strings.Replace(valid, `"jev-test-version"`, `null`, 1), "reviewer_invalid_model"},
		{"missing model", strings.Replace(valid, `"model":"jev-test-version",`, "", 1), "reviewer_invalid_model"},
		{"case model", strings.Replace(valid, `"model":`, `"Model":`, 1), "reviewer_invalid_json"},
		{"invalid json", `<html>private-response</html>`, "reviewer_invalid_json"},
		{"trailing json", valid + `{}`, "reviewer_invalid_json"},
		{"huge response", strings.Repeat("x", (256<<10)+1), "reviewer_invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject, contract.ToolReviewModeAsk} {
				snapshot := batchSnapshot(server.URL, mode)
				snapshot.Settings.AskOnReviewError = true
				s := toolreview.NewClient(server.Client()).Review(context.Background(), snapshot, batchInput(2))
				if s.Status != "failed" || s.ErrorCode != tc.code || len(s.Calls) != 2 || !toolreview.CanAskOnError(s) {
					t.Fatalf("invalid batch not rejected/ask eligible: %+v", s)
				}
				for _, call := range s.Calls {
					action := "withheld"
					if mode == contract.ToolReviewModeMark {
						action = "delivered"
					}
					if call.Verdict != "error" || call.ErrorCode != tc.code || call.Action != action || call.Model != "jev-latest" || call.ModelVerdict != "" || call.ReasonCode != "" || call.PassProbability != nil || call.FlagProbability != nil {
						t.Fatalf("partial verdict or raw response retained: %+v", s)
					}
				}
				data, _ := json.Marshal(s)
				if strings.Contains(string(data), "private-response") {
					t.Fatal("raw response leaked")
				}
				if err := s.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			if requests.Load() != 3 {
				t.Fatalf("retried/split batch: %d requests", requests.Load())
			}
		})
	}
}

func TestBatchInputFailuresNeverSendOrAllowManualBypass(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	for _, tc := range []struct {
		name, code string
		change     func(*toolreview.Snapshot, *toolreview.Input)
	}{
		{"whole rendered limit", "input_limit", func(_ *toolreview.Snapshot, i *toolreview.Input) {
			for j := range i.Calls {
				i.Calls[j].Custom, i.Calls[j].Arguments = true, strings.Repeat("x", 65<<10)
			}
		}},
		{"escaping limit", "input_limit", func(_ *toolreview.Snapshot, i *toolreview.Input) {
			i.Calls[0].Custom, i.Calls[0].Arguments = true, strings.Repeat("\x00", 24<<10)
		}},
		{"large user", "input_limit", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.UserRequest = strings.Repeat("u", 128<<10) }},
		{"args limit", "input_limit", func(_ *toolreview.Snapshot, i *toolreview.Input) {
			i.Calls[1].Arguments = strings.Repeat("x", (128<<10)+1)
		}},
		{"call limit", "call_limit", func(_ *toolreview.Snapshot, i *toolreview.Input) { *i = batchInput(65) }},
		{"bad arguments", "invalid_arguments", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Arguments = "not-json" }},
		{"duplicate arguments", "invalid_arguments", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Arguments = `{"x":1,"x":2}` }},
		{"non-object function", "invalid_arguments", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Arguments = `[1,2]` }},
		{"bad name", "invalid_call", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Name = "bad\nname" }},
		{"bad unicode args", "invalid_call", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Arguments = string([]byte{0xff}) }},
		{"bad unicode user", "invalid_input", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.UserRequest = string([]byte{0xff}) }},
		{"duplicate index", "invalid_call", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Index = i.Calls[0].Index }},
		{"negative index", "invalid_call", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Index = -1 }},
		{"config", "invalid_config", func(s *toolreview.Snapshot, _ *toolreview.Input) { s.Settings.Prompt = "invalid" }},
		{"credential", "credential_missing", func(s *toolreview.Snapshot, _ *toolreview.Input) { s.APIKey = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, input := batchSnapshot(server.URL, contract.ToolReviewModeAsk), batchInput(2)
			snapshot.Settings.AskOnReviewError = true
			tc.change(&snapshot, &input)
			s := toolreview.NewClient(server.Client()).Review(context.Background(), snapshot, input)
			if s.Status != "failed" || s.ErrorCode != tc.code || requests.Load() != 0 || toolreview.CanAskOnError(s) {
				t.Fatalf("invalid input sent/bypassed: %+v requests=%d", s, requests.Load())
			}
			for i, call := range s.Calls {
				if call.Index != input.Calls[i].Index || call.Action != "withheld" || call.Verdict != "error" || call.ErrorCode != tc.code {
					t.Fatalf("metadata/action lost: %+v", s)
				}
			}
		})
	}
}

func TestBatchModesAndThresholdBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, choice    string
		flag, threshold float64
		flagged         bool
	}{
		{"below", "flag", 0.2, 0.200001, false},
		{"equal", "pass", 0.2, 0.2, true},
		{"above", "pass", 0.2, 0.199999, true},
		{"zero endpoint", "pass", 0, 0, true},
		{"one endpoint", "pass", 1, 1, true},
		{"one below", "flag", 0.8, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			choice := fmt.Sprintf(`{"type":"choice","choice":%q,"probabilities":{"pass":%g,"flag":%g},"confidence":0.4}`, tc.choice, 1-tc.flag, tc.flag)
			answer := strings.ReplaceAll(batchAnswer(2), batchPassChoice, choice)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, answer) }))
			defer server.Close()
			for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject, contract.ToolReviewModeAsk} {
				snapshot := batchSnapshot(server.URL, mode)
				snapshot.Settings.RejectThreshold = tc.threshold
				s := toolreview.NewClient(server.Client()).Review(context.Background(), snapshot, batchInput(2))
				status, verdict, action := "passed", "pass", "delivered"
				if tc.flagged {
					status, verdict = "flagged", "flag"
					if mode != contract.ToolReviewModeMark {
						action = "withheld"
					}
					if mode == contract.ToolReviewModeReject {
						status = "blocked"
					}
				}
				if s.Status != status || s.RejectThreshold == nil || *s.RejectThreshold != tc.threshold {
					t.Fatalf("threshold summary: %+v", s)
				}
				for _, call := range s.Calls {
					if call.Verdict != verdict || call.ModelVerdict != tc.choice || call.Action != action || call.FlagProbability == nil || *call.FlagProbability != tc.flag {
						t.Fatalf("threshold call: %+v", s)
					}
				}
			}
		})
	}
}

func TestBatchHTTPFailuresDoNotRetryOrDowngrade(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 307} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "private-response")
			}))
			defer server.Close()
			s := toolreview.NewClient(server.Client()).Review(context.Background(), batchSnapshot(server.URL, contract.ToolReviewModeAsk), batchInput(3))
			if s.Status != "failed" || requests.Load() != 1 || !toolreview.CanAskOnError(s) {
				t.Fatalf("HTTP failure retried/downgraded: %+v requests=%d", s, requests.Load())
			}
		})
	}
}

func TestBatchResponseBoundAllowsLargeButBoundedChoiceAnswers(t *testing.T) {
	// 64 reviews + 64 reasons are about 32 KiB without spacing. Providers may
	// pretty-print; ensure a valid response above 64 KiB is accepted by batch.
	answer := strings.ReplaceAll(batchAnswer(64), `,"`, ","+strings.Repeat(" ", 64)+`"`)
	if len(answer) <= 64<<10 || len(answer) > 256<<10 {
		t.Fatalf("invalid response fixture size: %d", len(answer))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, answer) }))
	defer server.Close()
	s := toolreview.NewClient(server.Client()).Review(context.Background(), batchSnapshot(server.URL, contract.ToolReviewModeReject), batchInput(64))
	if s.Status != "passed" || len(s.Calls) != 64 {
		t.Fatalf("bounded large answer rejected: %+v", s)
	}
}

func TestBatchTimeoutCancellationAndNoCalls(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
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
	defer server.Close()
	defer close(release)
	client := toolreview.NewClient(server.Client())
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject, contract.ToolReviewModeAsk} {
		snapshot := batchSnapshot(server.URL, mode)
		snapshot.Settings.TimeoutMS = 40
		s := client.Review(context.Background(), snapshot, batchInput(2))
		select {
		case <-entered:
		default:
			t.Fatalf("review timed out before HTTP started: %+v", s)
		}
		if s.Status != "failed" || s.ErrorCode != "timeout" || !toolreview.CanAskOnError(s) {
			t.Fatalf("batch timeout: %+v", s)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan contract.ToolReviewSummary, 1)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		done <- client.Review(ctx, batchSnapshot(server.URL, contract.ToolReviewModeReject), batchInput(2))
	}()
	defer func() {
		cancel()
		select {
		case <-workerDone:
		case <-time.After(2 * time.Second):
			t.Error("cancelled worker did not finish during cleanup")
		}
	}()
	select {
	case <-entered:
		cancel()
	case s := <-done:
		t.Fatalf("review returned before cancellation: %+v", s)
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP did not start")
	}
	select {
	case s := <-done:
		if s.Status != "failed" || s.ErrorCode != "cancelled" || toolreview.CanAskOnError(s) {
			t.Fatalf("batch cancellation: %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled review did not exit")
	}
	snapshot := batchSnapshot(server.URL, contract.ToolReviewModeReject)
	s := client.Review(ctx, snapshot, batchInput(0))
	if s.Status != "not_applicable" || len(s.Calls) != 0 || requests.Load() != 4 {
		t.Fatalf("zero calls sent: %+v requests=%d", s, requests.Load())
	}
	snapshot.Settings.Enabled = false
	s = client.Review(context.Background(), snapshot, batchInput(2))
	if s.Status != "not_applicable" || requests.Load() != 4 {
		t.Fatalf("disabled batch sent: %+v", s)
	}
}

func TestBatchGlobalSlotsQueueBudgetAndCleanup(t *testing.T) {
	entered, release := make(chan struct{}, 16), make(chan struct{})
	var requests, active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		entered <- struct{}{}
		select {
		case <-release:
			_, _ = io.WriteString(w, batchAnswer(3))
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	completed := make(chan contract.ToolReviewSummary, 16)
	finished := make(chan struct{}, 16)
	defer func() {
		cancel()
		close(release)
		for range 16 {
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Error("active worker did not finish during cleanup")
				return
			}
		}
	}()
	client := toolreview.NewClient(server.Client())
	for range 16 {
		go func() {
			defer func() { finished <- struct{}{} }()
			completed <- client.Review(ctx, batchSnapshot(server.URL, contract.ToolReviewModeReject), batchInput(3))
		}()
	}
	for range 16 {
		select {
		case <-entered:
		case s := <-completed:
			t.Fatalf("batch took multiple slots/failed early: %+v", s)
		case <-time.After(2 * time.Second):
			t.Fatal("batch did not occupy exactly one slot each")
		}
	}
	snapshot := batchSnapshot(server.URL, contract.ToolReviewModeReject)
	snapshot.Settings.TimeoutMS = 40
	s := client.Review(context.Background(), snapshot, batchInput(3))
	if s.Status != "failed" || s.ErrorCode != "timeout" || requests.Load() != 16 || peak.Load() != 16 {
		t.Fatalf("queue escaped shared deadline/concurrency bound: %+v requests=%d peak=%d", s, requests.Load(), peak.Load())
	}
	queueCtx, queueCancel := context.WithCancel(context.Background())
	queuedDone, started := make(chan contract.ToolReviewSummary, 1), make(chan struct{})
	queuedWorkerDone := make(chan struct{})
	go func() {
		defer close(queuedWorkerDone)
		close(started)
		queuedDone <- client.Review(queueCtx, batchSnapshot(server.URL, contract.ToolReviewModeReject), batchInput(3))
	}()
	defer func() {
		queueCancel()
		select {
		case <-queuedWorkerDone:
		case <-time.After(2 * time.Second):
			t.Error("queued worker did not finish during cleanup")
		}
	}()
	<-started
	queueCancel()
	select {
	case s := <-queuedDone:
		if s.ErrorCode != "cancelled" || requests.Load() != 16 {
			t.Fatalf("cancelled queue sent HTTP: %+v requests=%d", s, requests.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled queued review did not exit")
	}
	cancel()
	for range 16 {
		select {
		case s := <-completed:
			if s.ErrorCode != "cancelled" {
				t.Errorf("active review escaped cancellation: %+v", s)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("active review did not release slot")
		}
	}
}
