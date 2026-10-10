package toolreview_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func TestGeminiNonStreamingJSONArrayCollectsEveryChunk(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolGoogleGenerateContent, false)
	payload := `[
		{"candidates":[{"index":0,"content":{"parts":[{"text":"Inspecting."}]}}]},
		{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"id":"a","name":"read","args":{"path":"/tmp/a","Path":"case distinct"}}}]}}]},
		{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"id":"b","name":"write","args":{"text":"x"}}}]}}]},
		{"candidates":[{"index":0,"finishReason":"STOP"}]}
	]`
	contains, err := c.Observe([]byte(payload))
	if err != nil || !contains || !c.SawTool() || !c.Complete() {
		t.Fatalf("array response: contains=%v saw=%v complete=%v err=%v", contains, c.SawTool(), c.Complete(), err)
	}
	calls, err := c.Calls()
	if err != nil || len(calls) != 2 || calls[0].Name != "read" || calls[0].Arguments != `{"path":"/tmp/a","Path":"case distinct"}` || calls[1].Name != "write" {
		t.Fatalf("array calls: %#v %v", calls, err)
	}
	if err := c.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestGeminiNonStreamingJSONArrayWaitsForAllCandidates(t *testing.T) {
	c := toolreview.NewCollector(contract.ProtocolGoogleGenerateContent, false)
	if err := c.SetExpectedCandidates(2); err != nil {
		t.Fatal(err)
	}
	calls := observeAll(t, c, `[
		{"candidates":[{"index":0,"content":{"parts":[{"text":"first answer"}]},"finishReason":"STOP"}]},
		{"candidates":[{"index":1,"content":{"parts":[{"functionCall":{"name":"read","args":{}}}]},"finishReason":"STOP"}]}
	]`)
	if len(calls) != 1 || calls[0].Name != "read" {
		t.Fatalf("late candidate omitted: %#v", calls)
	}
}

func TestGeminiNonStreamingJSONArrayCannotHideInvalidOrLateTools(t *testing.T) {
	for _, payload := range []string{
		`[]`,
		`[null]`,
		`[[{"candidates":[]}]]`,
		`[{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{}}}]}}]}]`,
		`[{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{}},"FunctionCall":null}]},"finishReason":"STOP"}]}]`,
		`[{"candidates":[{"index":0,"finishReason":"STOP"}]},{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"late","args":{}}}]},"finishReason":"STOP"}]}]`,
	} {
		if !json.Valid([]byte(payload)) {
			t.Fatalf("invalid JSON fixture: %s", payload)
		}
		c := toolreview.NewCollector(contract.ProtocolGoogleGenerateContent, false)
		_, err := c.Observe([]byte(payload))
		var safe *toolreview.Error
		if !errors.As(err, &safe) || c.Complete() {
			t.Fatalf("unsafe array accepted: complete=%v err=%v", c.Complete(), err)
		}
	}
}
