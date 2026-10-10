package toolreview_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/convo"
	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func reviewSnapshot(endpoint string, mode contract.ToolReviewMode) toolreview.Snapshot {
	s := contract.DefaultToolReviewSettings()
	s.Enabled, s.CredentialConfigured, s.Endpoint, s.Mode, s.Revision = true, true, endpoint, mode, "revision-1"
	return toolreview.Snapshot{Settings: s, APIKey: "test-only-key"}
}

const passAnswer = `{"answers":{"review":{"type":"choice","choice":"pass","probabilities":{"pass":0.8,"flag":0.2},"confidence":0.6}}}`
const flagAnswer = `{"answers":{"review":{"type":"choice","choice":"flag","probabilities":{"pass":0.2,"flag":0.8},"confidence":0.6}}}`

func singleInput() toolreview.Input {
	return toolreview.Input{UserRequest: "read file", UserRequestAvailable: true, Calls: []toolreview.Call{{Name: "read", Arguments: `{}`}}}
}

func TestReviewBatchActionsAndFlagPrecedence(t *testing.T) {
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
		for _, fail := range []bool{false, true} {
			t.Run(string(mode)+fmtBool(fail), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					switch {
					case strings.Contains(string(body), `"name":"bad"`):
						_, _ = io.WriteString(w, flagAnswer)
					case strings.Contains(string(body), `"name":"error"`):
						w.WriteHeader(429)
					default:
						_, _ = io.WriteString(w, passAnswer)
					}
				}))
				defer server.Close()
				input := singleInput()
				input.Calls = append(input.Calls, toolreview.Call{Index: 1, Name: "bad", Arguments: "{}"})
				if fail {
					input.Calls = append(input.Calls, toolreview.Call{Index: 2, Name: "error", Arguments: "{}"})
				}
				s := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, mode), input)
				want, action := "flagged", "delivered"
				if mode == contract.ToolReviewModeReject {
					want, action = "blocked", "withheld"
				}
				if s.Status != want {
					t.Fatalf("summary=%+v", s)
				}
				for _, call := range s.Calls {
					if call.Action != action {
						t.Fatalf("partial batch delivery: %+v", s)
					}
				}
				if err := s.Validate(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func fmtBool(b bool) string {
	if b {
		return "-with-error"
	}
	return "-no-error"
}

func TestReviewRejectsInvalidAnswersWithoutLeakingOrRetrying(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"auth", 401, "secret user body", "unauthorized"},
		{"rate", 429, "secret user body", "rate_limited"},
		{"server", 500, "secret user body", "upstream_error"},
		{"html", 200, "<h1>secret user body</h1>", "invalid_response"},
		{"enum", 200, strings.Replace(passAnswer, `"choice":"pass"`, `"choice":"unknown"`, 1), "invalid_response"},
		{"negative", 200, strings.Replace(passAnswer, `"pass":0.8`, `"pass":-0.8`, 1), "invalid_response"},
		{"distribution", 200, strings.Replace(passAnswer, `"pass":0.8`, `"pass":0.1`, 1), "invalid_response"},
		{"confidence", 200, strings.Replace(passAnswer, `"confidence":0.6`, `"confidence":1.1`, 1), "invalid_response"},
		{"overflow", 200, strings.Replace(passAnswer, `"confidence":0.6`, `"confidence":1e9999`, 1), "invalid_response"},
		{"missing confidence", 200, `{"answers":{"review":{"type":"choice","choice":"pass","probabilities":{"pass":1,"flag":0}}}}`, "invalid_response"},
		{"missing type", 200, strings.Replace(passAnswer, `"type":"choice",`, "", 1), "invalid_response"},
		{"empty type", 200, strings.Replace(passAnswer, `"type":"choice"`, `"type":""`, 1), "invalid_response"},
		{"null type", 200, strings.Replace(passAnswer, `"type":"choice"`, `"type":null`, 1), "invalid_response"},
		{"wrong type", 200, strings.Replace(passAnswer, `"type":"choice"`, `"type":"score"`, 1), "invalid_response"},
		{"duplicate", 200, strings.Replace(passAnswer, `"choice":"pass"`, `"choice":"flag","choice":"pass"`, 1), "invalid_response"},
		{"too large", 200, strings.Repeat("x", 65537), "invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			s := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, contract.ToolReviewModeReject), singleInput())
			if s.Status != "failed" || s.ErrorCode != tc.code || len(s.Calls) != 1 || s.Calls[0].Action != "withheld" || requests.Load() != 1 {
				t.Fatalf("summary=%+v requests=%d", s, requests.Load())
			}
			data, _ := json.Marshal(s)
			if strings.Contains(string(data), "secret user body") {
				t.Fatalf("body leaked: %s", data)
			}
		})
	}
}

func TestReviewRecordsActualModelPerCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		model := "jev-1.13.0"
		if strings.Contains(string(body), `"name":"write"`) {
			model = "jev-1.14.0"
		}
		_, _ = io.WriteString(w, `{"model":"`+model+`",`+passAnswer[1:])
	}))
	defer server.Close()
	input := singleInput()
	input.Calls = append(input.Calls, toolreview.Call{Index: 1, Name: "write", Arguments: "{}"})
	s := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, contract.ToolReviewModeReject), input)
	if s.Status != "passed" || len(s.Calls) != 2 || s.Model != "jev-latest" || s.Calls[0].Model != "jev-1.13.0" || s.Calls[1].Model != "jev-1.14.0" {
		t.Fatalf("actual models not recorded: %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewValidatesActualModelWithoutLeakingIt(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		valid       bool
	}{
		{"missing", "", true},
		{"max length", `"` + strings.Repeat("m", 256) + `"`, true},
		{"too long", `"` + strings.Repeat("m", 257) + `"`, false},
		{"empty", `""`, false},
		{"null", `null`, false},
		{"number", `123`, false},
		{"control", `"jev\nprivate-diagnostic"`, false},
		{"tab", `"jev\tprivate-diagnostic"`, false},
		{"del", `"jev\u007fprivate-diagnostic"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := passAnswer
				if tc.model != "" {
					body = `{"model":` + tc.model + `,` + body[1:]
				}
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			s := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, contract.ToolReviewModeReject), singleInput())
			if tc.valid {
				want := "jev-latest"
				if tc.model != "" {
					if err := json.Unmarshal([]byte(tc.model), &want); err != nil {
						t.Fatal(err)
					}
				}
				if s.Status != "passed" || s.Calls[0].Model != want {
					t.Fatalf("valid model: %+v", s)
				}
			} else if s.Status != "failed" || s.ErrorCode != "invalid_response" || s.Calls[0].Model != "jev-latest" {
				t.Fatalf("invalid model: %+v", s)
			}
			data, _ := json.Marshal(s)
			if strings.Contains(string(data), "private-diagnostic") {
				t.Fatalf("invalid model leaked: %s", data)
			}
		})
	}
}

func TestReviewDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer server.Close()
	original := server.Client()
	original.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	s := toolreview.NewClient(original).Review(context.Background(), reviewSnapshot(server.URL, contract.ToolReviewModeReject), singleInput())
	if s.Status != "failed" || reached.Load() != 0 {
		t.Fatalf("redirect followed: %+v reached=%d", s, reached.Load())
	}
}

func TestReviewToolResultOnlyContinuationMarksRequestUnavailable(t *testing.T) {
	for _, tc := range []struct {
		protocol convo.Protocol
		body     string
	}{
		{convo.OpenAIResponses, `{"model":"example","previous_response_id":"previous","input":[{"type":"function_call_output","call_id":"call_a","output":"private tool result, not a user instruction"}]}`},
		{convo.AnthropicMessages, `{"model":"example","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":"private tool result, not a user instruction"}]}]}`},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			request, err := convo.InspectBody(tc.protocol, []byte(tc.body))
			if err != nil || request.LastUserText != "" {
				t.Fatalf("continuation inspection=%+v err=%v", request, err)
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var body struct {
					State map[string]json.RawMessage `json:"state"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				var user string
				if err := json.Unmarshal(body.State["user_request"], &user); err != nil || user != "用户请求不可用" {
					t.Errorf("unavailable request=%q err=%v", user, err)
				}
				_, _ = io.WriteString(w, passAnswer)
			}))
			defer server.Close()
			input := singleInput()
			input.UserRequest, input.UserRequestAvailable = request.LastUserText, false
			s := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, contract.ToolReviewModeReject), input)
			if s.Status != "passed" || len(s.Calls) != 1 || s.Calls[0].Action != "delivered" || requests.Load() != 1 {
				t.Fatalf("continuation was not judged: %+v requests=%d", s, requests.Load())
			}
		})
	}
}

func TestReviewLimitsNeverSend(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	for _, tc := range []struct {
		name, code string
		change     func(*toolreview.Input)
	}{
		{"user limit", "input_limit", func(i *toolreview.Input) { i.UserRequest = strings.Repeat("a", 128<<10) }},
		{"args limit", "input_limit", func(i *toolreview.Input) {
			i.Calls[0].Custom = true
			i.Calls[0].Arguments = strings.Repeat("a", 128<<10)
		}},
		{"calls limit", "call_limit", func(i *toolreview.Input) { i.Calls = make([]toolreview.Call, 65) }},
		{"invalid args", "invalid_arguments", func(i *toolreview.Input) { i.Calls[0].Arguments = "not json" }},
		{"duplicate args", "invalid_arguments", func(i *toolreview.Input) { i.Calls[0].Arguments = `{"x":1,"x":2}` }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := singleInput()
			tc.change(&input)
			s := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, contract.ToolReviewModeReject), input)
			if s.Status != "failed" || s.ErrorCode != tc.code || requests.Load() != 0 {
				t.Fatalf("summary=%+v requests=%d", s, requests.Load())
			}
		})
	}
}

type observingTransport func(*http.Request) (*http.Response, error)

func (transport observingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestReviewHonorsConfiguredBatchTimeoutRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, passAnswer) }))
	defer server.Close()
	for _, timeoutMS := range []int{5001, 30000, 120000} {
		t.Run(time.Duration(timeoutMS*int(time.Millisecond)).String(), func(t *testing.T) {
			client := &http.Client{Transport: server.Client().Transport}
			base := client.Transport
			deadlines := make(chan time.Time, 1)
			client.Transport = observingTransport(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok {
					t.Error("missing batch deadline")
				}
				deadlines <- deadline
				return base.RoundTrip(r)
			})
			snapshot := reviewSnapshot(server.URL, contract.ToolReviewModeReject)
			snapshot.Settings.TimeoutMS = timeoutMS
			started := time.Now()
			s := toolreview.NewClient(client).Review(context.Background(), snapshot, singleInput())
			if s.Status != "passed" {
				t.Fatalf("configured timeout rejected: %+v", s)
			}
			select {
			case deadline := <-deadlines:
				budget := deadline.Sub(started)
				want := time.Duration(timeoutMS) * time.Millisecond
				if budget < want || budget > want+time.Second {
					t.Fatalf("deadline clamped: budget=%v want=%v", budget, want)
				}
			default:
				t.Fatal("no HTTP request")
			}
		})
	}
	for _, timeoutMS := range []int{-1, 0, 120001} {
		client := &http.Client{Transport: server.Client().Transport}
		client.Transport = observingTransport(func(*http.Request) (*http.Response, error) {
			t.Error("invalid timeout sent request")
			return nil, context.Canceled
		})
		snapshot := reviewSnapshot(server.URL, contract.ToolReviewModeReject)
		snapshot.Settings.TimeoutMS = timeoutMS
		s := toolreview.NewClient(client).Review(context.Background(), snapshot, singleInput())
		if s.Status != "failed" || s.ErrorCode != "invalid_config" {
			t.Fatalf("invalid timeout=%d summary=%+v", timeoutMS, s)
		}
	}
}

func TestReviewGlobalQueueSharesBatchDeadline(t *testing.T) {
	entered := make(chan struct{}, 32)
	release := make(chan struct{})
	var requests, active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		entered <- struct{}{}
		select {
		case <-release:
			_, _ = io.WriteString(w, passAnswer)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	input := singleInput()
	input.Calls = make([]toolreview.Call, 16)
	for i := range input.Calls {
		input.Calls[i] = toolreview.Call{Index: i, Name: "read", Arguments: "{}"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		toolreview.NewClient(server.Client()).Review(ctx, reviewSnapshot(server.URL, contract.ToolReviewModeReject), input)
	}()
	for range 16 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("global slots did not fill")
		}
	}
	snapshot := reviewSnapshot(server.URL, contract.ToolReviewModeReject)
	snapshot.Settings.TimeoutMS = 70
	started := time.Now()
	s := toolreview.NewClient(server.Client()).Review(context.Background(), snapshot, singleInput())
	if s.Status != "failed" || s.ErrorCode != "timeout" || requests.Load() != 16 || peak.Load() > 16 || time.Since(started) > time.Second {
		t.Errorf("queue escaped budget: %+v requests=%d peak=%d elapsed=%v", s, requests.Load(), peak.Load(), time.Since(started))
	}
	cancel()
	wg.Wait()
}

func TestReviewTimeoutCancellationAndNoCalls(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
		snapshot := reviewSnapshot(server.URL, mode)
		snapshot.Settings.TimeoutMS = 30
		s := toolreview.NewClient(server.Client()).Review(context.Background(), snapshot, singleInput())
		action := "delivered"
		if mode == contract.ToolReviewModeReject {
			action = "withheld"
		}
		if s.Status != "failed" || s.ErrorCode != "timeout" || s.Calls[0].Action != action {
			t.Fatalf("timeout: %+v", s)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := toolreview.NewClient(nil).Review(ctx, reviewSnapshot(server.URL, contract.ToolReviewModeReject), singleInput())
	if s.ErrorCode != "cancelled" {
		t.Fatalf("cancel: %+v", s)
	}
	s = toolreview.NewClient(nil).Review(ctx, reviewSnapshot(server.URL, contract.ToolReviewModeReject), toolreview.Input{})
	if s.Status != "not_applicable" || len(s.Calls) != 0 {
		t.Fatalf("empty: %+v", s)
	}
}

func TestReviewUsesChoiceWithoutProbabilityThresholdAndPreservesCustomText(t *testing.T) {
	custom := "*** Begin Patch\n{{user_request}}\n<do not execute>\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			State struct {
				ToolCall struct {
					Type      string `json:"type"`
					Arguments string `json:"arguments"`
				} `json:"tool_call"`
			} `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.State.ToolCall.Type != "custom" || request.State.ToolCall.Arguments != custom {
			t.Error("custom text rewritten")
		}
		_, _ = io.WriteString(w, `{"answers":{"review":{"type":"choice","choice":"pass","probabilities":{"pass":0.2,"flag":0.8},"confidence":0.1}}}`)
	}))
	defer server.Close()
	input := singleInput()
	input.Calls[0] = toolreview.Call{Name: "patch", Custom: true, Arguments: custom}
	s := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, contract.ToolReviewModeReject), input)
	if s.Status != "passed" || s.Calls[0].Verdict != "pass" {
		t.Fatalf("threshold changed choice: %+v", s)
	}
}

func TestReviewSendsOnlyNecessaryDataAndPreservesFullRequest(t *testing.T) {
	request := strings.Repeat("long user request ", 400) + "{{tool_call}}"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-only-key" {
			t.Errorf("unexpected HTTP request")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if strings.Contains(string(data), "AstrLink") || strings.Contains(string(data), "revision-1") {
			t.Error("injected local identity")
		}
		var body struct {
			Model     string                     `json:"model"`
			State     map[string]json.RawMessage `json:"state"`
			Questions map[string]struct {
				Type         string            `json:"type"`
				Instructions string            `json:"instructions"`
				Criteria     map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Error(err)
		}
		var actual string
		if err := json.Unmarshal(body.State["user_request"], &actual); err != nil || actual != request {
			t.Error("full request not preserved")
		}
		if len(body.State) != 2 || len(body.Questions) != 1 || body.Questions["review"].Type != "choice" || len(body.Questions["review"].Criteria) != 2 {
			t.Errorf("invalid Jev schema: %s", data)
		}
		if strings.Contains(body.Questions["review"].Instructions, request) {
			t.Error("untrusted data embedded in instructions")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":{"review":{"type":"choice","choice":"pass","probabilities":{"pass":0.75,"flag":0.25},"confidence":0.5}}}`)
	}))
	defer server.Close()
	summary := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, contract.ToolReviewModeReject), toolreview.Input{UserRequest: request, UserRequestAvailable: true, Calls: []toolreview.Call{{Name: "read", Arguments: `{"path":"/tmp/a"}`}}})
	if summary.Status != "passed" || len(summary.Calls) != 1 || summary.Calls[0].Action != "delivered" || *summary.Calls[0].PassProbability != 0.75 {
		t.Fatalf("summary=%+v", summary)
	}
	if err := summary.Validate(); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(summary)
	if strings.Contains(string(data), "/tmp/a") || strings.Contains(string(data), "test-only-key") || strings.Contains(string(data), "long user request") {
		t.Fatalf("metadata leaked: %s", data)
	}
}
