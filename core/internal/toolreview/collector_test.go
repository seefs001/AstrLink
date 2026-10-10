package toolreview_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func observeAll(t *testing.T, c *toolreview.Collector, events ...string) []toolreview.Call {
	t.Helper()
	for _, event := range events {
		if _, err := c.Observe([]byte(event)); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}
	calls, err := c.Calls()
	if err != nil || !c.Complete() {
		t.Fatalf("calls: %v; complete=%v", err, c.Complete())
	}
	return calls
}

func TestResponsesSnapshotsAndInterleavedCustomInput(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
	calls := observeAll(t, c,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"a","call_id":"ca","type":"function_call","name":"read","arguments":""}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"b","call_id":"cb","type":"custom_tool_call","name":"patch","input":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"a","delta":"{\"x\":"}`,
		`{"type":"response.custom_tool_call_input.delta","output_index":1,"item_id":"b","delta":"*** Begin Patch\n"}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"a","delta":"1}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"a","arguments":"{\"x\":1}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"a","arguments":"{\"x\":1}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"a","call_id":"ca","type":"function_call","name":"read","arguments":"{\"x\":1}"}}`,
		`{"type":"response.custom_tool_call_input.done","item_id":"b","input":"*** Begin Patch\n"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"id":"a","call_id":"ca","type":"function_call","name":"read","arguments":"{\"x\":1}"},{"id":"b","call_id":"cb","type":"custom_tool_call","name":"patch","input":"*** Begin Patch\n"}]}}`,
	)
	if len(calls) != 2 || calls[0].Arguments != `{"x":1}` || !calls[1].Custom || calls[1].Arguments != "*** Begin Patch\n" {
		t.Fatalf("calls=%#v", calls)
	}
}

func TestResponsesConflictsAndUnknownClientToolsAreSafeErrors(t *testing.T) {
	for _, event := range []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"a","type":"function_call","name":"different","arguments":"{}"}}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"a","arguments":"{\"secret\":true}"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"b","type":"local_shell_call","action":{"secret":"sensitive"}}}`,
	} {
		c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
		_, err := c.Observe([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"a","type":"function_call","name":"read","arguments":"{}"}}`))
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Observe([]byte(event))
		var safe *toolreview.Error
		if !errors.As(err, &safe) || strings.Contains(err.Error(), "secret") || c.Complete() {
			t.Fatalf("unsafe error: %v", err)
		}
	}
}

func TestResponsesIgnoresHostedToolsAndNestedToolLikeContent(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, false)
	calls := observeAll(t, c, `{"status":"completed","output":[{"type":"web_search_call","id":"web"},{"type":"mcp_call","name":"remote","arguments":"{}"},{"type":"message","content":[{"type":"text","function_call":{"name":"fake"}}]}]}`)
	if len(calls) != 0 || c.SawTool() {
		t.Fatalf("hosted tools collected: %#v", calls)
	}
}

func TestAnthropicFragmentsAndHostedExclusion(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolAnthropicMessages, true)
	calls := observeAll(t, c,
		`{"type":"message_start","message":{"content":[]}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"server","name":"search","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"remote\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"client","name":"read","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"9007199254740993}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
	)
	if len(calls) != 1 || calls[0].Name != "read" || calls[0].Arguments != `{"x":9007199254740993}` {
		t.Fatalf("calls=%#v", calls)
	}
}

func TestGeminiCallsSnapshotsAndFinish(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolGoogleGenerateContent, true)
	calls := observeAll(t, c,
		`{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"id":"a","name":"read","args":{"path":"/tmp/a"}}},{"functionCall":{"id":"b","name":"write","args":{"text":"x"}}}]}}]}`,
		`{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"id":"a","name":"read","args":{"path":"/tmp/a"}}},{"functionCall":{"id":"b","name":"write","args":{"text":"x"}}}]},"finishReason":"STOP"}]}`,
	)
	if len(calls) != 2 || calls[0].Name != "read" || calls[1].Name != "write" {
		t.Fatalf("calls=%#v", calls)
	}
}

func TestFourNonStreamingProtocols(t *testing.T) {
	for _, tc := range []struct {
		protocol contract.ProtocolID
		body     string
	}{
		{contract.ProtocolOpenAIChat, `{"choices":[{"message":{"tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`},
		{contract.ProtocolOpenAIResponses, `{"status":"completed","output":[{"id":"a","type":"function_call","name":"read","arguments":"{}"}]}`},
		{contract.ProtocolAnthropicMessages, `{"type":"message","content":[{"id":"a","type":"tool_use","name":"read","input":{}}],"stop_reason":"tool_use"}`},
		{contract.ProtocolGoogleGenerateContent, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{}}}]},"finishReason":"STOP"}]}`},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			calls := observeAll(t, toolreview.NewCollector(tc.protocol, false), tc.body)
			if len(calls) != 1 || calls[0].Arguments != "{}" || calls[0].Custom {
				t.Fatalf("calls=%#v", calls)
			}
		})
	}
}

func TestCollectorLimitsAndPoisonedState(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolOpenAIChat, true)
	for i := range 64 {
		payload := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":"id_%d","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`, i, i)
		if _, err := c.Observe([]byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.Observe([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":64,"id":"too_many","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`))
	var safe *toolreview.Error
	if !errors.As(err, &safe) || safe.Code != "call_limit" {
		t.Fatalf("limit: %v", err)
	}
	if _, err := c.Calls(); err == nil {
		t.Fatal("failed collector recovered")
	}
	if _, err := c.Observe([]byte(`[DONE]`)); err == nil {
		t.Fatal("failed collector accepted more events")
	}
	c = toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
	if _, err := c.Observe([]byte(strings.Repeat("x", (8<<20)+1))); !errors.As(err, &safe) || safe.Code != "buffer_limit" {
		t.Fatalf("buffer: %v", err)
	}
}

func TestCollectorRejectsTruncationAndUnknownTools(t *testing.T) {
	for _, tc := range []struct {
		protocol contract.ProtocolID
		stream   bool
		body     string
	}{
		{contract.ProtocolOpenAIChat, true, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"read","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`},
		{contract.ProtocolOpenAIChat, false, `{"choices":[{"message":{"tool_calls":[{"type":"unknown","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`},
		{contract.ProtocolOpenAIResponses, true, `{"type":"response.incomplete","response":{"status":"incomplete"}}`},
		{contract.ProtocolOpenAIResponses, true, `{"type":"response.future_tool_call.delta","delta":"unparsed"}`},
		{contract.ProtocolOpenAIResponses, true, `{"type":"response.future_tool.delta","delta":"unparsed"}`},
		{contract.ProtocolAnthropicMessages, true, `{"type":"content_block_start","index":0,"content_block":{"type":"new_client_tool","name":"read","input":{}}}`},
		{contract.ProtocolGoogleGenerateContent, false, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{}}}]},"finishReason":"MALFORMED_FUNCTION_CALL"}]}`},
		{contract.ProtocolGoogleGenerateContent, false, `{"candidates":[{"content":{"parts":[{"toolCall":{"name":"read"}}]},"finishReason":"STOP"}]}`},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			c := toolreview.NewCollector(tc.protocol, tc.stream)
			_, err := c.Observe([]byte(tc.body))
			var safe *toolreview.Error
			if !errors.As(err, &safe) || c.Complete() {
				t.Fatalf("accepted invalid call: %v", err)
			}
		})
	}
}

func TestChatCustomLegacyAndIdentityConflicts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		custom bool
		args   string
	}{
		{"custom", []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"custom"}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"custom":{"name":"patch","input":"one"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"custom":{"input":"\ntwo"}}]},"finish_reason":"tool_calls"}]}`,
		}, true, "one\ntwo"},
		{"legacy", []string{
			`{"choices":[{"index":0,"delta":{"function_call":{"name":"read","arguments":"{"}}}]}`,
			`{"choices":[{"index":0,"delta":{"function_call":{"arguments":"}"}},"finish_reason":"function_call"}]}`,
		}, false, "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := observeAll(t, toolreview.NewCollector(contract.ProtocolOpenAIChat, true), append(tc.events, `[DONE]`)...)
			if len(calls) != 1 || calls[0].Custom != tc.custom || calls[0].Arguments != tc.args {
				t.Fatalf("calls=%#v", calls)
			}
		})
	}
	for _, delta := range []string{
		`{"index":0,"id":"renamed","function":{"arguments":"{}"}}`,
		`{"index":0,"id":"a","function":{"name":"renamed","arguments":"{}"}}`,
		`{"index":1,"id":"a","function":{"name":"read","arguments":"{}"}}`,
	} {
		c := toolreview.NewCollector(contract.ProtocolOpenAIChat, true)
		_, err := c.Observe([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"read","arguments":""}}]}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Observe([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[` + delta + `]}}]}`)); err == nil {
			t.Fatal("conflicting identity accepted")
		}
	}
}

func TestSnapshotsCompareJSONSemanticsWithoutRounding(t *testing.T) {
	for _, final := range []string{`{ "b":2, "a":9007199254740993 }`, `{"a":9007199254740992,"b":2}`} {
		c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
		_, err := c.Observe([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"a","type":"function_call","name":"read","arguments":"{\"a\":9007199254740993,\"b\":2}"}}`))
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(final)
		_, err = c.Observe([]byte(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"a","arguments":` + string(encoded) + `}`))
		if (err != nil) != strings.Contains(final, "4740992") {
			t.Fatalf("snapshot %s: %v", final, err)
		}
	}
}

func TestCollectorRejectsConflictingLateSnapshotAndMissingBlocks(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
	observeAll(t, c, `{"type":"response.completed","response":{"status":"completed","output":[{"id":"a","type":"function_call","name":"read","arguments":"{}"}]}}`)
	if _, err := c.Observe([]byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`)); err == nil {
		t.Fatal("missing snapshot call accepted")
	}
	c = toolreview.NewCollector(contract.ProtocolAnthropicMessages, true)
	_, err := c.Observe([]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"read","input":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Observe([]byte(`{"type":"message_stop"}`)); err == nil {
		t.Fatal("unterminated block accepted")
	}
}

func TestGeminiSeparateChunksContainDistinctCalls(t *testing.T) {
	for _, ids := range []bool{false, true} {
		id1, id2 := "", ""
		if ids {
			id1, id2 = `"id":"a",`, `"id":"b",`
		}
		c := toolreview.NewCollector(contract.ProtocolGoogleGenerateContent, true)
		last := `{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{` + id2 + `"name":"write","args":{"text":"x"}}}]},"finishReason":"STOP"}]}`
		calls := observeAll(t, c, `{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{`+id1+`"name":"read","args":{}}}]}}]}`, last, last)
		if len(calls) != 2 || calls[0].Name != "read" || calls[1].Name != "write" {
			t.Fatalf("calls=%#v", calls)
		}
	}
}

func TestResponsesInvalidSnapshotCannotHideBehindValidPrefix(t *testing.T) {
	for _, args := range []string{`{} trailing`, `{"x":1,"x":2}`} {
		c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
		_, err := c.Observe([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"a","type":"function_call","name":"read","arguments":"{\"x\":2}"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if args == `{} trailing` {
			args = `{"x":2} trailing`
		}
		encoded, _ := json.Marshal(args)
		if _, err := c.Observe([]byte(`{"type":"response.function_call_arguments.done","item_id":"a","arguments":` + string(encoded) + `}`)); err == nil {
			t.Fatal("invalid final snapshot accepted")
		}
	}
}

func TestCollectorBudgetStartsWithFirstTool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := toolreview.NewCollector(contract.ProtocolOpenAIChat, true)
		time.Sleep(121 * time.Second)
		if _, err := c.Observe([]byte(`{"choices":[{"index":0,"delta":{"content":"ordinary text"}}]}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Observe([]byte(`{"choices":[{"index":0,"delta":{"function_call":{"name":"read","arguments":"{}"}}}]}`)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(121 * time.Second)
		_, err := c.Calls()
		var safe *toolreview.Error
		if !errors.As(err, &safe) || safe.Code != "collection_timeout" {
			t.Fatalf("timeout=%v", err)
		}
	})
}

func TestHostedPayloadsAreNotParsedAsClientArguments(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, false)
	calls := observeAll(t, c, `{"status":"completed","output":[{"type":"mcp_call","arguments":{"query":"remote"},"input":{"value":1}},{"type":"message","content":[{"type":"function_call","name":"not_real"}]}]}`)
	if len(calls) != 0 || c.SawTool() {
		t.Fatalf("hosted calls=%#v", calls)
	}
}

func TestCollectorRejectsAmbiguousJSON(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolOpenAIChat, false)
	_, err := c.Observe([]byte(`{"choices":[{"message":{"function_call":{"name":"read","arguments":"{\"x\":1,\"x\":2}"}},"finish_reason":"function_call"}]}`))
	if err == nil {
		t.Fatal("duplicate argument keys accepted")
	}
}

func TestChatFragmentsRequireTerminalAndAssembleExactly(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolOpenAIChat, true)
	for _, payload := range []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/tmp/a\"}"}}]},"finish_reason":null}]}`,
	} {
		contains, err := c.Observe([]byte(payload))
		if err != nil || !contains || !c.SawTool() || c.Complete() {
			t.Fatalf("observe = %v, %v; saw=%v complete=%v", contains, err, c.SawTool(), c.Complete())
		}
	}
	if _, err := c.Calls(); err == nil {
		t.Fatal("unfinished stream accepted")
	}
	if _, err := c.Observe([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Observe([]byte(`[DONE]`)); err != nil {
		t.Fatal(err)
	}
	calls, err := c.Calls()
	if err != nil || !c.Complete() || len(calls) != 1 || calls[0].Name != "read" || calls[0].Arguments != `{"path":"/tmp/a"}` {
		t.Fatalf("calls = %#v, %v; complete=%v", calls, err, c.Complete())
	}
}
