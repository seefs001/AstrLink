package toolreview_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func TestChatWaitsForAllChoicesAndWholeResponseEnd(t *testing.T) {
	for _, end := range []string{"done", "eof"} {
		t.Run(end, func(t *testing.T) {
			c := toolreview.NewCollector(contract.ProtocolOpenAIChat, true)
			if err := c.SetExpectedCandidates(2); err != nil {
				t.Fatal(err)
			}
			for _, index := range []int{1, 0} {
				frame := fmt.Sprintf(`{"choices":[{"index":%d,"delta":{"tool_calls":[{"index":0,"id":"call_%d","type":"function","function":{"name":"read_%d","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, index, index, index)
				if contains, err := c.Observe([]byte(frame)); err != nil || !contains {
					t.Fatalf("choice %d: %v %v", index, contains, err)
				}
				if c.Complete() {
					t.Fatal("one choice terminal froze the whole response")
				}
				if _, err := c.Calls(); err == nil {
					t.Fatal("calls released before whole response ended")
				}
			}
			if _, err := c.Observe([]byte(`{"choices":[],"usage":{"total_tokens":10}}`)); err != nil {
				t.Fatal(err)
			}
			if end == "done" {
				if _, err := c.Observe([]byte(`[DONE]`)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.Finish(); err != nil {
					t.Fatal(err)
				}
			}
			calls, err := c.Calls()
			if err != nil || !c.Complete() || len(calls) != 2 || calls[0].Name != "read_0" || calls[1].Name != "read_1" {
				t.Fatalf("lost choices: %#v err=%v complete=%v", calls, err, c.Complete())
			}
		})
	}
}

func TestGeminiWaitsForExpectedLaterCandidate(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolGoogleGenerateContent, true)
	if err := c.SetExpectedCandidates(2); err != nil {
		t.Fatal(err)
	}
	first := `{"candidates":[{"index":0,"content":{"parts":[{"text":"first answer"}]},"finishReason":"STOP"}]}`
	if _, err := c.Observe([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if c.Complete() {
		t.Fatal("first candidate made later tools invisible")
	}
	last := `{"candidates":[{"index":1,"content":{"parts":[{"functionCall":{"id":"late","name":"read","args":{}}}]},"finishReason":"STOP"}]}`
	if contains, err := c.Observe([]byte(last)); err != nil || !contains {
		t.Fatalf("later candidate: %v %v", contains, err)
	}
	calls, err := c.Calls()
	if err != nil || !c.Complete() || len(calls) != 1 || calls[0].Name != "read" {
		t.Fatalf("lost candidate: %#v %v", calls, err)
	}
	if _, err := c.Observe([]byte(last)); err != nil {
		t.Fatalf("final replay: %v", err)
	}
	if err := c.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestNonStreamingResponsesRequireEveryExpectedCandidate(t *testing.T) {
	for _, protocol := range []contract.ProtocolID{contract.ProtocolOpenAIChat, contract.ProtocolGoogleGenerateContent} {
		for _, indices := range [][]int{{0, 1}, {0}, {0, 0}, {0, 1, 2}} {
			t.Run(fmt.Sprintf("%s/%v", protocol, indices), func(t *testing.T) {
				c := toolreview.NewCollector(protocol, false)
				if err := c.SetExpectedCandidates(2); err != nil {
					t.Fatal(err)
				}
				var items []string
				for _, index := range indices {
					item := fmt.Sprintf(`{"index":%d,"message":{"content":"text"},"finish_reason":"stop"}`, index)
					if protocol == contract.ProtocolGoogleGenerateContent {
						item = fmt.Sprintf(`{"index":%d,"content":{"parts":[{"text":"text"}]},"finishReason":"STOP"}`, index)
					}
					items = append(items, item)
				}
				field := "choices"
				if protocol == contract.ProtocolGoogleGenerateContent {
					field = "candidates"
				}
				_, err := c.Observe([]byte(`{"` + field + `":[` + strings.Join(items, ",") + `]}`))
				valid := len(indices) == 2 && indices[1] == 1
				if valid && (err != nil || !c.Complete()) {
					t.Fatalf("all candidates rejected: %v", err)
				}
				if !valid && (err == nil || c.Complete()) {
					t.Fatalf("missing/duplicate/extra candidate accepted: %v", err)
				}
			})
		}
	}
}

func TestNonStreamingReplayCannotDropPreviouslyCompletedCandidates(t *testing.T) {
	for _, protocol := range []contract.ProtocolID{contract.ProtocolOpenAIChat, contract.ProtocolGoogleGenerateContent} {
		c := toolreview.NewCollector(protocol, false)
		if err := c.SetExpectedCandidates(2); err != nil {
			t.Fatal(err)
		}
		full := `{"choices":[{"index":0,"message":{"content":"a"},"finish_reason":"stop"},{"index":1,"message":{"content":"b"},"finish_reason":"stop"}]}`
		partial := `{"choices":[{"index":0,"message":{"content":"a"},"finish_reason":"stop"}]}`
		if protocol == contract.ProtocolGoogleGenerateContent {
			full = `{"candidates":[{"index":0,"finishReason":"STOP"},{"index":1,"finishReason":"STOP"}]}`
			partial = `{"candidates":[{"index":0,"finishReason":"STOP"}]}`
		}
		if _, err := c.Observe([]byte(full)); err != nil || !c.Complete() {
			t.Fatalf("full snapshot: %v", err)
		}
		if _, err := c.Observe([]byte(partial)); err == nil || c.Complete() {
			t.Fatalf("partial non-stream snapshot accepted for %s: %v", protocol, err)
		}
	}
}

func TestStreamEndCannotInventMissingCandidatesOrProtocolTerminals(t *testing.T) {
	for _, protocol := range []contract.ProtocolID{contract.ProtocolOpenAIChat, contract.ProtocolGoogleGenerateContent} {
		c := toolreview.NewCollector(protocol, true)
		if err := c.SetExpectedCandidates(2); err != nil {
			t.Fatal(err)
		}
		event := `{"choices":[{"index":0,"delta":{"content":"text"},"finish_reason":"stop"}]}`
		if protocol == contract.ProtocolGoogleGenerateContent {
			event = `{"candidates":[{"index":0,"finishReason":"STOP"}]}`
		}
		if _, err := c.Observe([]byte(event)); err != nil {
			t.Fatal(err)
		}
		if err := c.Finish(); err == nil || c.Complete() {
			t.Fatalf("EOF synthesized missing candidate for %s: %v", protocol, err)
		}
	}
	for _, protocol := range []contract.ProtocolID{contract.ProtocolOpenAIResponses, contract.ProtocolAnthropicMessages} {
		c := toolreview.NewCollector(protocol, true)
		if err := c.Finish(); err == nil || c.Complete() {
			t.Fatalf("EOF synthesized protocol terminal for %s: %v", protocol, err)
		}
	}
	c := toolreview.NewCollector(contract.ProtocolOpenAIChat, true)
	if _, err := c.Observe([]byte(`{"choices":[{"index":0,"delta":{"content":"unfinished"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Observe([]byte(`[DONE]`)); err == nil || c.Complete() {
		t.Fatalf("DONE accepted missing finish_reason: %v", err)
	}
}

func TestExpectedCandidatesMustBeConfiguredBeforeObservation(t *testing.T) {
	for _, count := range []int{0, -1} {
		c := toolreview.NewCollector(contract.ProtocolOpenAIChat, true)
		if err := c.SetExpectedCandidates(count); err == nil {
			t.Fatalf("invalid candidate count %d accepted", count)
		}
	}
	c := toolreview.NewCollector(contract.ProtocolGoogleGenerateContent, true)
	if _, err := c.Observe([]byte(`{"candidates":[{"index":0,"content":{"parts":[{"text":"text"}]}}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.SetExpectedCandidates(2); err == nil {
		t.Fatal("candidate count changed mid-response")
	}
}
