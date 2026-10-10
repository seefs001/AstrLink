package toolreview_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func responsesSnapshot(endpoint string, batch bool, mode contract.ToolReviewMode) toolreview.Snapshot {
	s := reviewSnapshot(endpoint, mode)
	s.Settings.Backend = contract.ToolReviewBackendResponses
	s.Settings.Model = "configured-model"
	s.Settings.BatchCalls = batch
	return s
}

func responsesInput(n int) toolreview.Input {
	i := toolreview.Input{UserRequest: "请检查工具 {{tool_call}}", UserRequestAvailable: true}
	for index := range n {
		i.Calls = append(i.Calls, toolreview.Call{Index: 11 + 7*index, Name: "same_name", Arguments: `{"integer":900719925474099312345,"text":"你好 {{user_request}}","X":1,"x":2}`})
	}
	return i
}

func responsesText(calls []toolreview.Call) string {
	results := make([]string, len(calls))
	for i, call := range calls {
		// Reverse order to verify matching original indices, not output positions.
		results[len(calls)-i-1] = fmt.Sprintf(`{"index":%d,"verdict":"pass","reason_code":"authorized","pass_probability":0.8,"flag_probability":0.2}`, call.Index)
	}
	return `{"results":[` + strings.Join(results, ",") + `]}`
}

func responsesEnvelopeText(text string) string {
	encoded, _ := json.Marshal(text)
	return `{"id":"resp_synthetic","status":"completed","model":"actual-model","error":null,"incomplete_details":null,"output":[{"id":"rs_synthetic","type":"reasoning","summary":[{"type":"summary_text","text":"synthetic reasoning"}]},{"id":"msg_synthetic","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + string(encoded) + `,"annotations":[],"logprobs":[]}]}],"usage":{"total_tokens":1}}`
}

func responsesReviewer(server *httptest.Server) toolreview.Reviewer {
	return toolreview.NewClient(server.Client())
}

func checkResponsesSchema(t *testing.T, schema map[string]any) {
	t.Helper()
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema not closed: %v", schema)
	}
	props := schema["properties"].(map[string]any)
	required := schema["required"].([]any)
	if len(required) != len(props) {
		t.Fatalf("optional fields in schema: %v", schema)
	}
	for _, key := range required {
		if _, ok := props[key.(string)]; !ok {
			t.Fatalf("invalid required field %v", key)
		}
	}
}

func TestResponsesProtocolAndDataIsolation(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, n := range []int{0, 1, 3, 64} {
			t.Run(fmt.Sprintf("batch=%t/n=%d", batch, n), func(t *testing.T) {
				input := responsesInput(n)
				custom := "*** Patch\n雪 {{user_request}} {{tool_call}} <instructions>\n"
				if n > 1 {
					input.Calls[1].Custom, input.Calls[1].Arguments = true, custom
				}
				policy := "Operator policy: allow only reads. {{user_request}} {{tool_call}}"
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.URL.Path != "/exact/custom/v1/responses" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-only-key" || r.Header.Get("Content-Type") != "application/json" {
						t.Errorf("wrong endpoint/auth/method: %s %s", r.Method, r.URL)
					}
					for key, values := range r.Header {
						if strings.Contains(strings.ToLower(key+strings.Join(values, "")), "astrlink") {
							t.Errorf("gateway identity header %s", key)
						}
					}
					body, _ := io.ReadAll(r.Body)
					var request map[string]any
					if err := json.Unmarshal(body, &request); err != nil {
						t.Error(err)
						return
					}
					if len(request) != 6 || request["model"] != "configured-model" || request["stream"] != false || request["store"] != false || request["response_format"] != nil || request["state"] != nil {
						t.Errorf("wrong Responses request: %v", request)
					}
					instructions := request["instructions"].(string)
					if strings.Contains(instructions, input.UserRequest) || strings.Contains(instructions, custom) || strings.Contains(instructions, "900719925474099312345") || strings.Contains(strings.ToLower(string(body)), "astrlink") {
						t.Error("data interpolated into instructions or gateway identity injected")
					}
					var state struct {
						Rules string `json:"review_rules"`
						User  string `json:"user_request"`
						Calls []struct {
							Index     int             `json:"index"`
							Name      string          `json:"name"`
							Type      string          `json:"type"`
							Arguments json.RawMessage `json:"arguments"`
						} `json:"tool_calls"`
					}
					if err := json.Unmarshal([]byte(request["input"].(string)), &state); err != nil {
						t.Error(err)
						return
					}
					want := 1
					if batch {
						want = n
					}
					if len(state.Calls) != want || state.User != input.UserRequest || !strings.Contains(state.Rules, "allow only reads") || strings.Contains(state.Rules, "{{") || strings.Count(request["input"].(string), "Operator policy") != 1 {
						t.Errorf("policy/user/batching changed: %+v", state)
					}
					calls := make([]toolreview.Call, len(state.Calls))
					for i, call := range state.Calls {
						if call.Name != "same_name" {
							t.Error("tool name changed")
						}
						if call.Type == "custom" {
							var args string
							_ = json.Unmarshal(call.Arguments, &args)
							if args != custom {
								t.Error("custom input changed")
							}
						} else if !strings.Contains(string(call.Arguments), "900719925474099312345") || !strings.Contains(string(call.Arguments), "你好 {{user_request}}") {
							t.Error("integer/Unicode/placeholder changed")
						}
						calls[i].Index = call.Index
					}
					format := request["text"].(map[string]any)["format"].(map[string]any)
					if format["type"] != "json_schema" || format["name"] != "tool_review" || format["strict"] != true {
						t.Error("not strict structured Responses output")
					}
					schema := format["schema"].(map[string]any)
					checkResponsesSchema(t, schema)
					item := schema["properties"].(map[string]any)["results"].(map[string]any)["items"].(map[string]any)
					checkResponsesSchema(t, item)
					props := item["properties"].(map[string]any)
					if len(props) != 5 || props["index"].(map[string]any)["type"] != "integer" || len(props["reason_code"].(map[string]any)["enum"].([]any)) != 10 {
						t.Error("result schema changed")
					}
					for _, name := range []string{"pass_probability", "flag_probability"} {
						p := props[name].(map[string]any)
						if p["type"] != "number" || p["minimum"] != float64(0) || p["maximum"] != float64(1) {
							t.Error("unbounded probability schema")
						}
					}
					_, _ = io.WriteString(w, responsesEnvelopeText(responsesText(calls)))
				}))
				defer server.Close()
				snapshot := responsesSnapshot(server.URL+"/exact/custom/v1/responses", batch, contract.ToolReviewModeReject)
				snapshot.Settings.Prompt = policy
				s := responsesReviewer(server).Review(context.Background(), snapshot, input)
				wantRequests, status := n, "passed"
				if batch && n > 0 {
					wantRequests = 1
				}
				if n == 0 {
					status = "not_applicable"
				}
				if requests.Load() != int32(wantRequests) || s.Status != status || s.Backend != contract.ToolReviewBackendResponses || s.BatchCalls == nil || *s.BatchCalls != batch || s.ConfigRevision != snapshot.Settings.Revision {
					t.Fatalf("protocol summary=%+v requests=%d", s, requests.Load())
				}
				for i, call := range s.Calls {
					if call.Index != input.Calls[i].Index || call.Name != "same_name" || call.Model != "actual-model" || call.ModelVerdict != "pass" || call.Verdict != "pass" || call.ReasonCode != "authorized" || *call.FlagProbability != 0.2 || *call.PassProbability != 0.8 || call.Action != "delivered" {
						t.Fatalf("bad result: %+v", call)
					}
				}
				if err := s.Validate(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestResponsesModesThresholdAndModelChoice(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject, contract.ToolReviewModeAsk} {
			for _, tc := range []struct {
				flag, threshold float64
				choice          string
				flagged         bool
			}{
				{0.2, 0.200001, "flag", false}, {0.2, 0.2, "pass", true}, {0.2, 0.199999, "pass", true}, {0, 0, "pass", true}, {1, 1, "pass", true}, {0.8, 1, "flag", false},
			} {
				t.Run(fmt.Sprintf("%t/%s/%g/%g", batch, mode, tc.flag, tc.threshold), func(t *testing.T) {
					text := fmt.Sprintf(`{"results":[{"index":11,"verdict":%q,"reason_code":"policy_violation","pass_probability":%g,"flag_probability":%g}]}`, tc.choice, 1-tc.flag, tc.flag)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, responsesEnvelopeText(text)) }))
					defer server.Close()
					snapshot := responsesSnapshot(server.URL, batch, mode)
					snapshot.Settings.RejectThreshold = tc.threshold
					s := responsesReviewer(server).Review(context.Background(), snapshot, responsesInput(1))
					status, verdict, action := "passed", "pass", "delivered"
					if tc.flagged {
						status, verdict = "flagged", "flag"
						if mode == contract.ToolReviewModeReject {
							status = "blocked"
						}
						if mode != contract.ToolReviewModeMark {
							action = "withheld"
						}
					}
					if s.Status != status || s.Calls[0].Verdict != verdict || s.Calls[0].ModelVerdict != tc.choice || s.Calls[0].Action != action || *s.RejectThreshold != tc.threshold || s.Calls[0].Model != "actual-model" {
						t.Fatalf("threshold semantics changed: %+v", s)
					}
					if err := s.Validate(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestResponsesInvalidOutputFailsAtomically(t *testing.T) {
	validText := responsesText(responsesInput(2).Calls)
	valid := responsesEnvelopeText(validText)
	textCase := func(name, text, code string) struct{ name, body, code string } {
		return struct{ name, body, code string }{name, responsesEnvelopeText(text), code}
	}
	cases := []struct{ name, body, code string }{
		{"incomplete", strings.Replace(valid, `"status":"completed"`, `"status":"incomplete"`, 1), "reviewer_invalid_review"},
		{"missing status", strings.Replace(valid, `"status":"completed",`, "", 1), "reviewer_invalid_review"},
		{"error despite completed", strings.Replace(valid, `"error":null`, `"error":{"message":"private-response"}`, 1), "reviewer_invalid_review"},
		{"incomplete despite completed", strings.Replace(valid, `"incomplete_details":null`, `"incomplete_details":{"reason":"max_output_tokens"}`, 1), "reviewer_invalid_review"},
		{"refusal", strings.Replace(valid, `"type":"output_text"`, `"type":"refusal","refusal":"private-response"`, 1), "reviewer_invalid_review"},
		{"extra refusal", strings.Replace(valid, `"annotations":[]`, `"refusal":"private-response","annotations":[]`, 1), "reviewer_invalid_review"},
		{"top refusal", strings.Replace(valid, `"error":null`, `"error":null,"refusal":"private-response"`, 1), "reviewer_invalid_review"},
		{"function output", strings.Replace(valid, `"type":"message"`, `"type":"function_call_output"`, 1), "reviewer_invalid_review"},
		{"tool output", strings.Replace(valid, `"type":"message"`, `"type":"tool_result"`, 1), "reviewer_invalid_review"},
		{"not assistant", strings.Replace(valid, `"role":"assistant"`, `"role":"user"`, 1), "reviewer_invalid_review"},
		{"unfinished message", strings.Replace(valid, `"role":"assistant","status":"completed"`, `"role":"assistant","status":"in_progress"`, 1), "reviewer_invalid_review"},
		{"multiple text", strings.Replace(valid, `"annotations":[],"logprobs":[]}`, `"annotations":[],"logprobs":[]},{"type":"output_text","text":"private-response"}`, 1), "reviewer_invalid_review"},
		{"multiple message", strings.Replace(valid, `],"usage"`, `,{"type":"message","role":"assistant","status":"completed","content":[]}],"usage"`, 1), "reviewer_invalid_review"},
		{"SDK convenience", `{"status":"completed","model":"actual-model","output":[],"output_text":"private-response"}`, "reviewer_invalid_review"},
		{"reasoning refusal", strings.Replace(valid, `"type":"summary_text"`, `"type":"refusal","refusal":"private-response"`, 1), "reviewer_invalid_review"},
		{"unknown message field", strings.Replace(valid, `"role":"assistant"`, `"role":"assistant","tool_output":"private-response"`, 1), "reviewer_invalid_review"},
		{"missing model", strings.Replace(valid, `"model":"actual-model",`, "", 1), "reviewer_invalid_model"},
		{"empty model", strings.Replace(valid, `"model":"actual-model"`, `"model":" "`, 1), "reviewer_invalid_model"},
		{"control model", strings.Replace(valid, `"model":"actual-model"`, `"model":"private-response\n"`, 1), "reviewer_invalid_model"},
		{"null model", strings.Replace(valid, `"model":"actual-model"`, `"model":null`, 1), "reviewer_invalid_model"},
		{"long model", strings.Replace(valid, "actual-model", strings.Repeat("x", 257), 1), "reviewer_invalid_model"},
		{"duplicate envelope", strings.Replace(valid, `"model":`, `"model":"other","model":`, 1), "reviewer_invalid_json"},
		{"alias envelope", strings.Replace(valid, `"model":`, `"Model":`, 1), "reviewer_invalid_json"},
		{"alias pair", strings.Replace(valid, `"model":`, `"Model":"other","model":`, 1), "reviewer_invalid_json"},
		{"alias nested", strings.Replace(valid, `"role":`, `"Role":`, 1), "reviewer_invalid_json"},
		{"duplicate nested", strings.Replace(valid, `"role":`, `"role":"other","role":`, 1), "reviewer_invalid_json"},
		{"trailing JSON", valid + `{}`, "reviewer_invalid_json"},
		{"not JSON", "private-response", "reviewer_invalid_json"},
	}
	for _, tc := range []struct{ name, text, code string }{
		{"missing index", strings.Replace(validText, `"index":18,`, "", 1), "reviewer_invalid_review"},
		{"null index", strings.Replace(validText, `"index":18`, `"index":null`, 1), "reviewer_invalid_review"},
		{"wrong index", strings.Replace(validText, `"index":18`, `"index":19`, 1), "reviewer_invalid_review"},
		{"duplicate index", strings.Replace(validText, `"index":18`, `"index":11`, 1), "reviewer_invalid_review"},
		{"missing result", responsesText(responsesInput(1).Calls), "reviewer_invalid_review"},
		{"extra result", responsesText(responsesInput(3).Calls), "reviewer_invalid_review"},
		{"empty results", `{"results":[]}`, "reviewer_invalid_review"},
		{"null results", `{"results":null}`, "reviewer_invalid_review"},
		{"extra root field", strings.Replace(validText, `{"results":`, `{"private-response":true,"results":`, 1), "reviewer_invalid_review"},
		{"extra result field", strings.Replace(validText, `"index":18`, `"index":18,"private-response":true`, 1), "reviewer_invalid_review"},
		{"missing probability", strings.Replace(validText, `,"flag_probability":0.2`, "", 1), "reviewer_invalid_review"},
		{"null probability", strings.Replace(validText, `"flag_probability":0.2`, `"flag_probability":null`, 1), "reviewer_invalid_review"},
		{"negative probability", strings.Replace(validText, `"flag_probability":0.2`, `"flag_probability":-0.2`, 1), "reviewer_invalid_review"},
		{"too large probability", strings.Replace(validText, `"flag_probability":0.2`, `"flag_probability":1.2`, 1), "reviewer_invalid_review"},
		{"wrong sum", strings.Replace(validText, `"flag_probability":0.2`, `"flag_probability":0.1`, 1), "reviewer_invalid_review"},
		{"NaN", strings.Replace(validText, `"flag_probability":0.2`, `"flag_probability":NaN`, 1), "reviewer_invalid_json"},
		{"overflow", strings.Replace(validText, `"flag_probability":0.2`, `"flag_probability":1e999`, 1), "reviewer_invalid_json"},
		{"invalid verdict", strings.Replace(validText, `"verdict":"pass"`, `"verdict":"private-response"`, 1), "reviewer_invalid_review"},
		{"invalid reason", strings.Replace(validText, `"reason_code":"authorized"`, `"reason_code":"private-response"`, 1), "reviewer_invalid_reason"},
		{"missing reason", strings.Replace(validText, `"reason_code":"authorized",`, "", 1), "reviewer_invalid_reason"},
		{"duplicate text key", strings.Replace(validText, `"index":18`, `"index":18,"index":18`, 1), "reviewer_invalid_json"},
		{"escaped duplicate", strings.Replace(validText, `"index":18`, `"index":18,"\u0069ndex":18`, 1), "reviewer_invalid_json"},
		{"alias text", strings.Replace(validText, `"index":18`, `"Index":18`, 1), "reviewer_invalid_json"},
		{"alias pair text", strings.Replace(validText, `"index":18`, `"index":18,"Index":18`, 1), "reviewer_invalid_json"},
		{"code fence", "```json\n" + validText + "\n```", "reviewer_invalid_json"},
	} {
		cases = append(cases, textCase(tc.name, tc.text, tc.code))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject, contract.ToolReviewModeAsk} {
				s := responsesReviewer(server).Review(context.Background(), responsesSnapshot(server.URL, true, mode), responsesInput(2))
				if s.Status != "failed" || s.ErrorCode != tc.code || !toolreview.CanAskOnError(s) {
					t.Fatalf("bad error classification: %+v", s)
				}
				for _, call := range s.Calls {
					action := "withheld"
					if mode == contract.ToolReviewModeMark {
						action = "delivered"
					}
					if call.Verdict != "error" || call.ErrorCode != tc.code || call.Action != action || call.ModelVerdict != "" || call.ReasonCode != "" || call.PassProbability != nil || call.FlagProbability != nil || call.Model != "configured-model" {
						t.Fatalf("partial batch published: %+v", s)
					}
				}
				data, _ := json.Marshal(s)
				if strings.Contains(string(data), "private-response") || strings.Contains(string(data), "test-only-key") {
					t.Fatal("private error content leaked")
				}
				if err := s.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			if requests.Load() != 3 {
				t.Fatalf("split/retried/fell back: %d", requests.Load())
			}
		})
	}
}

func TestResponsesLocalValidationPrecedesExpiredBudget(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	for _, batch := range []bool{false, true} {
		for _, tc := range []struct {
			name, code string
			change     func(*toolreview.Snapshot, *toolreview.Input)
		}{
			{"invalid args", "invalid_arguments", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Arguments = "not JSON" }},
			{"duplicate args", "invalid_arguments", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Arguments = `{"x":1,"x":2}` }},
			{"invalid name", "invalid_call", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Name = "bad\n" }},
			{"invalid unicode", "invalid_call", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Arguments = string([]byte{0xff}) }},
			{"duplicate index", "invalid_call", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Index = i.Calls[0].Index }},
			{"negative index", "invalid_call", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.Calls[1].Index = -1 }},
			{"invalid user", "invalid_input", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.UserRequest = string([]byte{0xff}) }},
			{"render limit", "input_limit", func(_ *toolreview.Snapshot, i *toolreview.Input) { i.UserRequest = strings.Repeat("u", 128<<10) }},
			{"escaping render limit", "input_limit", func(_ *toolreview.Snapshot, i *toolreview.Input) {
				i.Calls[1].Custom = true
				i.Calls[1].Arguments = strings.Repeat("\x00", 24<<10)
			}},
			{"call limit", "call_limit", func(_ *toolreview.Snapshot, i *toolreview.Input) { *i = responsesInput(65) }},
			{"empty backend", "invalid_config", func(s *toolreview.Snapshot, _ *toolreview.Input) { s.Settings.Backend = "" }},
			{"unknown backend", "invalid_config", func(s *toolreview.Snapshot, _ *toolreview.Input) { s.Settings.Backend = "unknown" }},
			{"invalid config", "invalid_config", func(s *toolreview.Snapshot, _ *toolreview.Input) { s.Settings.TimeoutMS = 0 }},
			{"missing key", "credential_missing", func(s *toolreview.Snapshot, _ *toolreview.Input) { s.APIKey = "" }},
		} {
			t.Run(fmt.Sprintf("%t/%s", batch, tc.name), func(t *testing.T) {
				for _, expired := range []bool{false, true} {
					ctx := context.Background()
					if expired {
						var cancel context.CancelFunc
						ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
						defer cancel()
					}
					snapshot, input := responsesSnapshot(server.URL, batch, contract.ToolReviewModeAsk), responsesInput(2)
					tc.change(&snapshot, &input)
					s := responsesReviewer(server).Review(ctx, snapshot, input)
					if s.Status != "failed" || s.ErrorCode != tc.code || toolreview.CanAskOnError(s) || requests.Load() != 0 {
						t.Fatalf("local failure bypassed: %+v requests=%d", s, requests.Load())
					}
				}
			})
		}
	}
}

func TestResponsesInputAndOutputExactLimits(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("input/batch=%t", batch), func(t *testing.T) {
			lengths := make(chan int, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				lengths <- len(b)
				_, _ = io.WriteString(w, responsesEnvelopeText(responsesText(responsesInput(1).Calls)))
			}))
			defer server.Close()
			snapshot, input := responsesSnapshot(server.URL, batch, contract.ToolReviewModeAsk), responsesInput(1)
			input.UserRequest = "u"
			if s := responsesReviewer(server).Review(context.Background(), snapshot, input); s.Status != "passed" {
				t.Fatalf("baseline: %+v", s)
			}
			base := <-lengths
			input.UserRequest = strings.Repeat("u", (128<<10)-base+1)
			if s := responsesReviewer(server).Review(context.Background(), snapshot, input); s.Status != "passed" {
				t.Fatalf("exact input bound: %+v", s)
			}
			if n := <-lengths; n != 128<<10 {
				t.Fatalf("fixture=%d", n)
			}
			input.UserRequest += "u"
			s := responsesReviewer(server).Review(context.Background(), snapshot, input)
			if s.ErrorCode != "input_limit" || toolreview.CanAskOnError(s) {
				t.Fatalf("input overflow: %+v", s)
			}
			select {
			case <-lengths:
				t.Fatal("overflow sent")
			default:
			}
		})
	}
	for _, tc := range []struct {
		batch    bool
		n, limit int
	}{{false, 1, 64 << 10}, {true, 1, 64 << 10}, {true, 2, 256 << 10}} {
		t.Run(fmt.Sprintf("output/%t/%d", tc.batch, tc.n), func(t *testing.T) {
			body := responsesEnvelopeText(responsesText(responsesInput(tc.n).Calls))
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				padding := tc.limit - len(body)
				if request == 2 {
					padding++
				}
				_, _ = io.WriteString(w, body+strings.Repeat(" ", padding))
			}))
			defer server.Close()
			snapshot := responsesSnapshot(server.URL, tc.batch, contract.ToolReviewModeAsk)
			if s := responsesReviewer(server).Review(context.Background(), snapshot, responsesInput(tc.n)); s.Status != "passed" {
				t.Fatalf("exact response bound: %+v", s)
			}
			s := responsesReviewer(server).Review(context.Background(), snapshot, responsesInput(tc.n))
			if s.Status != "failed" || s.ErrorCode != "reviewer_invalid_json" || !toolreview.CanAskOnError(s) || requests.Load() != 2 {
				t.Fatalf("response overflow retried/accepted: %+v", s)
			}
		})
	}
}

func TestResponsesHTTPFailuresSafetyAndNoRetry(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{401, "unauthorized"}, {403, "unauthorized"}, {429, "rate_limited"}, {500, "upstream_error"}, {307, "upstream_error"}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			var requests, redirected atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer other.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Cookie") != "" {
					t.Error("cookie forwarded")
				}
				w.Header().Set("Location", other.URL)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "private-response test-only-key")
			}))
			defer server.Close()
			client := server.Client()
			jar, _ := cookiejar.New(nil)
			req, _ := http.NewRequest("GET", server.URL, nil)
			jar.SetCookies(req.URL, []*http.Cookie{{Name: "session", Value: "private-cookie"}})
			client.Jar = jar
			client.CheckRedirect = func(*http.Request, []*http.Request) error { t.Error("caller redirect policy used"); return nil }
			for _, batch := range []bool{false, true} {
				s := toolreview.NewClient(client).Review(context.Background(), responsesSnapshot(server.URL, batch, contract.ToolReviewModeAsk), responsesInput(1))
				if s.Status != "failed" || s.ErrorCode != tc.code || !toolreview.CanAskOnError(s) {
					t.Fatalf("HTTP error: %+v", s)
				}
				data, _ := json.Marshal(s)
				if strings.Contains(string(data), "private-response") || strings.Contains(string(data), "test-only-key") {
					t.Fatal("unsafe error")
				}
			}
			if requests.Load() != 2 || redirected.Load() != 0 || client.Jar != jar || client.CheckRedirect == nil {
				t.Fatalf("retry/redirect/caller mutation requests=%d redirected=%d", requests.Load(), redirected.Load())
			}
		})
	}
}

func TestResponsesUnavailableUserAndExpiredContexts(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var state struct {
			User string `json:"user_request"`
		}
		_ = json.Unmarshal([]byte(body.Input), &state)
		if state.User != "用户请求不可用" {
			t.Error("unavailable user became fake intent")
		}
		_, _ = io.WriteString(w, responsesEnvelopeText(responsesText(responsesInput(1).Calls)))
	}))
	defer server.Close()
	for _, batch := range []bool{false, true} {
		snapshot, input := responsesSnapshot(server.URL, batch, contract.ToolReviewModeAsk), responsesInput(1)
		input.UserRequestAvailable = false
		if s := responsesReviewer(server).Review(context.Background(), snapshot, input); s.Status != "passed" {
			t.Fatalf("unavailable user: %+v", s)
		}
		for _, cancelled := range []bool{false, true} {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			code := "timeout"
			if cancelled {
				cancel()
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
				code = "cancelled"
			}
			s := responsesReviewer(server).Review(ctx, snapshot, input)
			cancel()
			if s.Status != "failed" || s.ErrorCode != code || toolreview.CanAskOnError(s) != !cancelled {
				t.Fatalf("expired budget: %+v", s)
			}
		}
		if s := responsesReviewer(server).Review(context.Background(), snapshot, toolreview.Input{}); s.Status != "not_applicable" || s.Backend != contract.ToolReviewBackendResponses {
			t.Fatalf("zero calls: %+v", s)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("expired/zero calls sent HTTP: %d", requests.Load())
	}
}
