package toolreview_test

import (
	"errors"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func TestResponsesDoneIsValidatedTerminalSnapshot(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
	for _, event := range []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"a","call_id":"ca","name":"read","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"a","delta":"{\"path\":\"/tmp/a\"}"}`,
	} {
		if _, err := c.Observe([]byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	if c.Complete() {
		t.Fatal("completed before response.done")
	}
	terminal := `{"type":"response.done","response":{"status":"completed","output":[{"type":"function_call","id":"a","call_id":"ca","name":"read","arguments":"{\"path\":\"/tmp/a\"}"}]}}`
	if contains, err := c.Observe([]byte(terminal)); err != nil || !contains || !c.Complete() {
		t.Fatalf("done not recognized: contains=%v complete=%v err=%v", contains, c.Complete(), err)
	}
	if _, err := c.Observe([]byte(terminal)); err != nil {
		t.Fatal(err)
	}
	calls, err := c.Calls()
	if err != nil || len(calls) != 1 || calls[0].Arguments != `{"path":"/tmp/a"}` {
		t.Fatalf("done snapshot lost or duplicated call: %#v %v", calls, err)
	}
	if err := c.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesDoneWithoutDeltasStillCollectsCustomTool(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
	calls := observeAll(t, c, `{"type":"response.done","response":{"status":"completed","output":[{"type":"custom_tool_call","id":"a","name":"patch","input":"original text\n"}]}}`)
	if len(calls) != 1 || !calls[0].Custom || calls[0].Arguments != "original text\n" {
		t.Fatalf("custom done snapshot: %#v", calls)
	}
}

func TestResponsesDoneCannotHideConflictingOrIncompleteSnapshot(t *testing.T) {
	for _, terminal := range []string{
		`{"type":"response.done"}`,
		`{"type":"response.done","response":{"status":"completed"}}`,
		`{"type":"response.done","response":{"status":"in_progress","output":[]}}`,
		`{"type":"response.done","response":{"status":"failed","output":[]}}`,
		`{"type":"response.done","response":{"status":"completed","output":[]}}`,
		`{"type":"response.done","response":{"status":"completed","output":[{"type":"function_call","id":"a","name":"read","arguments":"{\"x\":2}"}]}}`,
	} {
		c := toolreview.NewCollector(contract.ProtocolOpenAIResponses, true)
		if _, err := c.Observe([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"a","name":"read","arguments":"{\"x\":1}"}}`)); err != nil {
			t.Fatal(err)
		}
		_, err := c.Observe([]byte(terminal))
		var safe *toolreview.Error
		if !errors.As(err, &safe) || c.Complete() {
			t.Fatalf("invalid done accepted: complete=%v err=%v", c.Complete(), err)
		}
	}
}
