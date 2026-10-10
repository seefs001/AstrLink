package toolreview_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

const emptyResponsesCompleted = `{"type":"response.completed","response":{"status":"completed","output":[]}}`
const emptyResponsesDone = `{"type":"response.done","response":{"status":"completed","output":[]}}`

func TestResponsesEmptyOutputMinimalReplay(t *testing.T) {
	// The capture's deltas and both done snapshots agreed. Only the successful
	// terminal omitted output. No arguments escaping or reasoning slot is needed.
	item := `{"type":"function_call","id":"a","call_id":"ca","name":"read","arguments":"{}","status":"completed"}`
	for _, terminal := range []string{
		`{"type":"response.completed","response":{"status":"completed","output":[` + item + `]}}`,
		emptyResponsesCompleted,
	} {
		t.Run(fmt.Sprintf("terminal_bytes_%d", len(terminal)), func(t *testing.T) {
			c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
			calls := observeAll(t, c, `{"type":"response.output_item.done","output_index":0,"item":`+item+`}`, terminal)
			if len(calls) != 1 || calls[0].Name != "read" || calls[0].Arguments != "{}" {
				t.Fatalf("calls=%#v", calls)
			}
		})
	}
}

func responseEmptyOutputEvents(t *testing.T, custom bool) ([]string, string) {
	t.Helper()
	kind, field, eventName := "function_call", "arguments", "response.function_call_arguments"
	args := `{"command":"python3 - <<'PY'\nprint(\"hello\")\nPY\n","timeout":10}`
	if custom {
		kind, field, eventName = "custom_tool_call", "input", "response.custom_tool_call_input"
		args = "line one\n  line \\\"two\\\"\n"
	}
	encode := func(value any) string {
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	item := func(value string) map[string]any {
		return map[string]any{"id": "a", "call_id": "ca", "type": kind, "name": "inspect", field: value, "status": "completed"}
	}
	events := []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"r"}}`,
		encode(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": item("")}),
	}
	// Split at every byte, including each backslash and newline. Payload JSON
	// escaping must not affect the decoded input collected from the stream.
	for i := range len(args) {
		events = append(events, encode(map[string]any{"type": eventName + ".delta", "output_index": 1, "item_id": "a", "delta": args[i : i+1]}))
	}
	events = append(events,
		encode(map[string]any{"type": eventName + ".done", "output_index": 1, "item_id": "a", field: args}),
		encode(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": item(args)}),
	)
	return events, args
}

func TestResponsesEmptyOutputRetainsValidatedDoneCalls(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, terminal := range []string{emptyResponsesCompleted, emptyResponsesDone} {
			t.Run(fmt.Sprintf("custom_%v/terminal_bytes_%d", custom, len(terminal)), func(t *testing.T) {
				events, args := responseEmptyOutputEvents(t, custom)
				c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
				for _, event := range events {
					if _, err := c.Observe([]byte(event)); err != nil {
						t.Fatal(err)
					}
				}
				if c.Complete() {
					t.Fatal("item done completed the response without a terminal")
				}
				// Identical done/terminal duplicates remain idempotent, not new calls.
				calls := observeAll(t, c, events[len(events)-1], terminal, terminal)
				if len(calls) != 1 || calls[0].Arguments != args || calls[0].Custom != custom {
					t.Fatalf("calls=%#v", calls)
				}
				if err := c.Finish(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestResponsesEmptyOutputCannotCompleteUnvalidatedCalls(t *testing.T) {
	events, _ := responseEmptyOutputEvents(t, false)
	for _, tc := range []struct {
		name   string
		events []string
	}{
		{"added_only", events[:2]},
		{"deltas_only", events[:len(events)-2]},
		{"arguments_done_only", events[:len(events)-1]},
		{"non_tool_done", append(append([]string{}, events[:len(events)-1]...), `{"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"r"}}`)},
		{"one_done_one_open", append(append([]string{}, events...), `{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"b","call_id":"cb","name":"other","arguments":"{}"}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertResponsesReplayError(t, append(append([]string{}, tc.events...), emptyResponsesCompleted), "conflicting_call")
		})
	}
	// Neither output-item completion nor arguments completion replaces the
	// successful response terminal. An EOF must still be rejected.
	c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
	for _, event := range events {
		if _, err := c.Observe([]byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Finish(); err == nil || c.Complete() {
		t.Fatal("accepted truncated response")
	}
}

func TestResponsesEmptyOutputCannotHideConflicts(t *testing.T) {
	for _, custom := range []bool{false, true} {
		events, args := responseEmptyOutputEvents(t, custom)
		kind, field, eventName := "function_call", "arguments", "response.function_call_arguments"
		if custom {
			kind, field, eventName = "custom_tool_call", "input", "response.custom_tool_call_input"
		}
		for _, tc := range []struct {
			name, event, code string
		}{
			{"changed_input", fmt.Sprintf(`{"type":"response.output_item.done","output_index":1,"item":{"id":"a","call_id":"ca","type":%q,"name":"inspect",%q:"{}"}}`, kind, field), "conflicting_call"},
			{"changed_name", fmt.Sprintf(`{"type":"response.output_item.done","output_index":1,"item":{"id":"a","call_id":"ca","type":%q,"name":"other",%q:%q}}`, kind, field, args), "conflicting_call"},
			{"duplicate_id", fmt.Sprintf(`{"type":"response.output_item.done","output_index":2,"item":{"id":"a","call_id":"cb","type":%q,"name":"inspect",%q:"{}"}}`, kind, field), "conflicting_call"},
			{"duplicate_call_id", fmt.Sprintf(`{"type":"response.output_item.done","output_index":2,"item":{"id":"b","call_id":"ca","type":%q,"name":"inspect",%q:"{}"}}`, kind, field), "conflicting_call"},
			{"missing_input", fmt.Sprintf(`{"type":"response.output_item.done","output_index":1,"item":{"id":"a","call_id":"ca","type":%q,"name":"inspect"}}`, kind), "incomplete_call"},
			{"incomplete_item", fmt.Sprintf(`{"type":"response.output_item.done","output_index":1,"item":{"id":"a","call_id":"ca","type":%q,"name":"inspect",%q:%q,"status":"in_progress"}}`, kind, field, args), "incomplete_call"},
			{"conflicting_arguments_done", fmt.Sprintf(`{"type":%q,"output_index":1,"item_id":"a",%q:"{}"}`, eventName+".done", field), "conflicting_call"},
			{"late_delta", fmt.Sprintf(`{"type":%q,"output_index":1,"item_id":"a","delta":" "}`, eventName+".delta"), "conflicting_call"},
			{"nonempty_omission", `{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"r"}]}}`, "conflicting_call"},
			{"missing_output", `{"type":"response.completed","response":{"status":"completed"}}`, "incomplete_response"},
			{"incomplete_terminal", `{"type":"response.incomplete","response":{"status":"incomplete","output":[]}}`, "incomplete_response"},
		} {
			t.Run(fmt.Sprintf("custom_%v/%s", custom, tc.name), func(t *testing.T) {
				assertResponsesReplayError(t, append(append([]string{}, events...), tc.event, emptyResponsesCompleted), tc.code)
			})
		}
	}
}

func TestResponsesEmptyOutputCannotChangeTerminalCoverage(t *testing.T) {
	item := `{"type":"function_call","id":"a","call_id":"ca","name":"read","arguments":"{}","status":"completed"}`
	done := `{"type":"response.output_item.done","output_index":0,"item":` + item + `}`
	full := `{"type":"response.completed","response":{"status":"completed","output":[` + item + `]}}`
	for _, events := range [][]string{
		{done, full, emptyResponsesCompleted},
		{done, emptyResponsesCompleted, full},
		{done, emptyResponsesCompleted, `{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"a","delta":" "}`},
	} {
		assertResponsesReplayError(t, events, "conflicting_call")
	}
}

func assertResponsesReplayError(t *testing.T, events []string, code string) {
	t.Helper()
	c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
	var err error
	for _, event := range events {
		if _, err = c.Observe([]byte(event)); err != nil {
			break
		}
	}
	var safe *toolreview.Error
	if !errors.As(err, &safe) || safe.Code != code || c.Complete() {
		t.Fatalf("expected %s, complete=%v error=%v", code, c.Complete(), err)
	}
	calls, err := c.Calls()
	if err == nil || len(calls) != 0 {
		t.Fatal("failed collector returned calls")
	}
	if _, err := c.Observe([]byte(emptyResponsesCompleted)); err == nil {
		t.Fatal("poisoned collector recovered")
	}
}
