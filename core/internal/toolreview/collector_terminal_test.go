package toolreview_test

import (
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func TestEarlyToolSnapshotsFailBeforeFrameDelivery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol contract.ProtocolID
		event    string
	}{
		{"anthropic message_start", contract.ProtocolAnthropicMessages, `{"type":"message_start","message":{"content":[{"type":"tool_use","id":"a","name":"read","input":{}}]}}`},
		{"responses created function", contract.ProtocolOpenAIResponses, `{"type":"response.created","response":{"status":"in_progress","output":[{"type":"function_call","id":"a","name":"read","arguments":"{}"}]}}`},
		{"responses created custom", contract.ProtocolOpenAIResponses, `{"type":"response.created","response":{"status":"in_progress","output":[{"type":"custom_tool_call","id":"a","name":"patch","input":"raw text"}]}}`},
		{"responses in_progress function", contract.ProtocolOpenAIResponses, `{"type":"response.in_progress","response":{"status":"in_progress","output":[{"type":"function_call","id":"a","name":"read","arguments":"{}"}]}}`},
		{"responses in_progress custom", contract.ProtocolOpenAIResponses, `{"type":"response.in_progress","response":{"status":"in_progress","output":[{"type":"custom_tool_call","id":"a","name":"patch","input":"raw text"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := toolreview.NewCollector(tc.protocol, true)
			contains, err := c.Observe([]byte(tc.event))
			var safe *toolreview.Error
			if !contains || !c.SawTool() || !errors.As(err, &safe) || safe.Code != "unexpected_tool_snapshot" || c.Complete() {
				t.Fatalf("early tool could escape: contains=%v saw=%v complete=%v err=%v", contains, c.SawTool(), c.Complete(), err)
			}
			if _, err := c.Calls(); err == nil {
				t.Fatal("early tool became an empty successful collection")
			}
		})
	}
}

func TestEarlySnapshotsExcludeHostedToolsAndNestedData(t *testing.T) {
	for _, tc := range []struct {
		protocol contract.ProtocolID
		event    string
	}{
		{contract.ProtocolAnthropicMessages, `{"type":"message_start","message":{"content":[{"type":"server_tool_use","name":"search","input":{"type":"tool_use","name":"data"}},{"type":"text","text":"tool_use"}]}}`},
		{contract.ProtocolOpenAIResponses, `{"type":"response.created","response":{"status":"in_progress","output":[{"type":"mcp_call","name":"remote","arguments":{"type":"function_call"}},{"type":"message","content":[{"type":"custom_tool_call","input":"data only"}]}]}}`},
	} {
		c := toolreview.NewCollector(tc.protocol, true)
		contains, err := c.Observe([]byte(tc.event))
		if contains || c.SawTool() || c.Complete() || err != nil {
			t.Fatalf("non-client content misclassified: contains=%v saw=%v complete=%v err=%v", contains, c.SawTool(), c.Complete(), err)
		}
	}
}

func TestTruncatedResponsesWithoutClientToolsAreTerminal(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, reason := range []string{"length", "content_filter"} {
			t.Run(fmt.Sprintf("chat/%v/%s", streaming, reason), func(t *testing.T) {
				c := toolreview.NewCollector(contract.ProtocolOpenAIChat, streaming)
				field := "message"
				if streaming {
					field = "delta"
				}
				event := fmt.Sprintf(`{"choices":[{"index":0,"%s":{"content":"partial text"},"finish_reason":"%s"}]}`, field, reason)
				if streaming {
					if contains, err := c.Observe([]byte(event)); err != nil || contains || c.Complete() {
						t.Fatalf("choice terminal: contains=%v complete=%v err=%v", contains, c.Complete(), err)
					}
					assertNoToolTerminal(t, c, `[DONE]`)
				} else {
					assertNoToolTerminal(t, c, event)
				}
			})
		}
		for _, reason := range []string{"max_tokens", "refusal", "pause_turn"} {
			t.Run(fmt.Sprintf("anthropic/%v/%s", streaming, reason), func(t *testing.T) {
				c := toolreview.NewCollector(contract.ProtocolAnthropicMessages, streaming)
				block := `{"type":"text","text":"partial text"}`
				if reason == "pause_turn" {
					block = `{"type":"server_tool_use","name":"search","id":"hosted","input":{}}`
				}
				if !streaming {
					assertNoToolTerminal(t, c, fmt.Sprintf(`{"type":"message","content":[%s],"stop_reason":"%s"}`, block, reason))
					return
				}
				for _, event := range []string{
					`{"type":"message_start","message":{"content":[]}}`,
					fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":%s}`, block),
					`{"type":"content_block_stop","index":0}`,
					fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"%s"}}`, reason),
				} {
					if contains, err := c.Observe([]byte(event)); contains || err != nil {
						t.Fatalf("non-tool event: %v %v", contains, err)
					}
				}
				assertNoToolTerminal(t, c, `{"type":"message_stop"}`)
			})
		}
		for _, reason := range []string{"MAX_TOKENS", "SAFETY"} {
			t.Run(fmt.Sprintf("gemini/%v/%s", streaming, reason), func(t *testing.T) {
				c := toolreview.NewCollector(contract.ProtocolGoogleGenerateContent, streaming)
				assertNoToolTerminal(t, c, fmt.Sprintf(`{"candidates":[{"index":0,"content":{"parts":[{"text":"partial text"}]},"finishReason":"%s"}]}`, reason))
			})
		}
		t.Run(fmt.Sprintf("responses/%v", streaming), func(t *testing.T) {
			c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, streaming)
			event := `{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"partial text"}]},{"type":"web_search_call","status":"in_progress"}]}`
			if streaming {
				event = `{"type":"response.incomplete","response":` + event + `}`
			}
			assertNoToolTerminal(t, c, event)
		})
	}
}

func assertNoToolTerminal(t *testing.T, c *toolreview.Collector, event string) {
	t.Helper()
	contains, err := c.Observe([]byte(event))
	if err != nil || contains || c.SawTool() || !c.Complete() {
		t.Fatalf("no-tool terminal rejected: contains=%v saw=%v complete=%v err=%v", contains, c.SawTool(), c.Complete(), err)
	}
	calls, err := c.Calls()
	if err != nil || len(calls) != 0 {
		t.Fatalf("no-tool calls: %#v %v", calls, err)
	}
	if _, err := c.Observe([]byte(event)); err != nil {
		t.Fatalf("terminal replay: %v", err)
	}
}

func TestTruncatedResponsesCannotReleaseClientTools(t *testing.T) {
	for _, tc := range []struct {
		protocol contract.ProtocolID
		events   []string
	}{
		{contract.ProtocolOpenAIChat, []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		}},
		{contract.ProtocolAnthropicMessages, []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"read","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`,
		}},
		{contract.ProtocolGoogleGenerateContent, []string{
			`{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"read","args":{}}}]}}]}`,
			`{"candidates":[{"index":0,"finishReason":"SAFETY"}]}`,
		}},
		{contract.ProtocolOpenAIResponses, []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"a","name":"patch","input":""}}`,
			`{"type":"response.custom_tool_call_input.delta","item_id":"a","delta":"partial custom text"}`,
			`{"type":"response.incomplete","response":{"status":"incomplete","output":[]}}`,
		}},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			c := toolreview.NewCollector(tc.protocol, true)
			for _, event := range tc.events[:len(tc.events)-1] {
				if _, err := c.Observe([]byte(event)); err != nil {
					t.Fatal(err)
				}
			}
			_, err := c.Observe([]byte(tc.events[len(tc.events)-1]))
			var safe *toolreview.Error
			if !errors.As(err, &safe) || c.Complete() || !c.SawTool() {
				t.Fatalf("truncated client tool accepted: complete=%v saw=%v err=%v", c.Complete(), c.SawTool(), err)
			}
		})
	}
	for _, tc := range []struct {
		protocol contract.ProtocolID
		event    string
	}{
		{contract.ProtocolOpenAIChat, `{"choices":[{"message":{"function_call":{"name":"read","arguments":"{\"path\":"}},"finish_reason":"content_filter"}]}`},
		{contract.ProtocolAnthropicMessages, `{"type":"message","content":[{"type":"tool_use","name":"read","input":{}}],"stop_reason":"pause_turn"}`},
		{contract.ProtocolGoogleGenerateContent, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{}}}]},"finishReason":"MAX_TOKENS"}]}`},
		{contract.ProtocolOpenAIResponses, `{"status":"incomplete","output":[{"type":"function_call","id":"a","name":"read","arguments":"{\"path\":"}]}`},
	} {
		c := toolreview.NewCollector(tc.protocol, false)
		contains, err := c.Observe([]byte(tc.event))
		if err == nil || c.Complete() || !c.SawTool() || !contains {
			t.Fatalf("truncated snapshot misclassified: protocol=%s contains=%v saw=%v complete=%v err=%v", tc.protocol, contains, c.SawTool(), c.Complete(), err)
		}
	}
}

func TestAllCollectorsBudgetStartsAtFirstToolNotConstruction(t *testing.T) {
	for _, tc := range []struct {
		protocol                contract.ProtocolID
		prelude, tool, terminal string
	}{
		{contract.ProtocolOpenAIChat, `{"choices":[{"index":0,"delta":{"content":"thinking"}}]}`, `{"choices":[{"index":0,"delta":{"function_call":{"name":"read","arguments":"{}"}}}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}`},
		{contract.ProtocolOpenAIResponses, `{"type":"response.created","response":{"status":"in_progress","output":[]}}`, `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"a","name":"read","arguments":""}}`, `{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","id":"a","name":"read","arguments":"{}"}]}}`},
		{contract.ProtocolAnthropicMessages, `{"type":"message_start","message":{"content":[]}}`, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"read","input":{}}}`, `{"type":"message_stop"}`},
		{contract.ProtocolGoogleGenerateContent, `{"candidates":[{"index":0,"content":{"parts":[{"text":"thinking"}]}}]}`, `{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"read","args":{}}}]}}]}`, `{"candidates":[{"index":0,"finishReason":"STOP"}]}`},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := toolreview.NewCollector(tc.protocol, true)
				time.Sleep(121 * time.Second)
				if contains, err := c.Observe([]byte(tc.prelude)); err != nil || contains {
					t.Fatalf("prelude: %v %v", contains, err)
				}
				time.Sleep(121 * time.Second)
				if contains, err := c.Observe([]byte(tc.tool)); err != nil || !contains {
					t.Fatalf("first tool: %v %v", contains, err)
				}
				time.Sleep(119 * time.Second)
				var safe *toolreview.Error
				if _, err := c.Calls(); !errors.As(err, &safe) || safe.Code != "incomplete_response" {
					t.Fatalf("tool budget started early: %v", err)
				}
				time.Sleep(2 * time.Second)
				if _, err := c.Observe([]byte(tc.terminal)); !errors.As(err, &safe) || safe.Code != "collection_timeout" {
					t.Fatalf("missing collection timeout: %v", err)
				}
			})
		})
	}
}
