package toolreview_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func TestProtocolFieldCaseAliasesFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol contract.ProtocolID
		stream   bool
		payload  string
	}{
		{"chat calls", contract.ProtocolOpenAIChat, false, `{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"unsafe","arguments":"{}"}}],"Tool_Calls":[]},"finish_reason":"tool_calls"}]}`},
		{"chat function name", contract.ProtocolOpenAIChat, false, `{"choices":[{"message":{"function_call":{"name":"unsafe","NAME":"safe","arguments":"{}"}},"finish_reason":"function_call"}]}`},
		{"responses item type", contract.ProtocolOpenAIResponses, false, `{"status":"completed","output":[{"type":"function_call","Type":"message","name":"unsafe","arguments":"{}"}]}`},
		{"responses event type", contract.ProtocolOpenAIResponses, true, `{"type":"response.output_item.added","Type":"response.output_text.delta","output_index":0,"item":{"type":"function_call","name":"unsafe","arguments":"{}"}}`},
		{"anthropic block type", contract.ProtocolAnthropicMessages, false, `{"type":"message","content":[{"type":"tool_use","Type":"text","name":"unsafe","input":{}}],"stop_reason":"tool_use"}`},
		{"anthropic early content", contract.ProtocolAnthropicMessages, true, `{"type":"message_start","message":{"content":[{"type":"tool_use","name":"unsafe","input":{}}],"Content":[]}}`},
		{"gemini function", contract.ProtocolGoogleGenerateContent, false, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"unsafe","args":{}},"FunctionCall":null}]},"finishReason":"STOP"}]}`},
		{"gemini finish", contract.ProtocolGoogleGenerateContent, true, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"unsafe","args":{}}}]},"finishReason":"MALFORMED_FUNCTION_CALL","FinishReason":"STOP"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := toolreview.NewCollector(tc.protocol, tc.stream)
			_, err := c.Observe([]byte(tc.payload))
			var safe *toolreview.Error
			if !errors.As(err, &safe) || safe.Code != "invalid_payload" || c.Complete() {
				t.Fatalf("protocol alias accepted: complete=%v err=%v", c.Complete(), err)
			}
		})
	}
}

func TestCaseDistinctUserArgumentKeysRemainData(t *testing.T) {
	const arguments = `{"path":"lower","Path":"upper","type":"data","Type":"different"}`
	for _, tc := range []struct {
		protocol contract.ProtocolID
		payload  string
	}{
		{contract.ProtocolOpenAIChat, `{"choices":[{"message":{"function_call":{"name":"read","arguments":"{\"path\":\"lower\",\"Path\":\"upper\",\"type\":\"data\",\"Type\":\"different\"}"}},"finish_reason":"function_call"}]}`},
		{contract.ProtocolOpenAIResponses, `{"status":"completed","output":[{"type":"function_call","name":"read","arguments":"{\"path\":\"lower\",\"Path\":\"upper\",\"type\":\"data\",\"Type\":\"different\"}"}]}`},
		{contract.ProtocolAnthropicMessages, `{"type":"message","content":[{"type":"tool_use","name":"read","input":` + arguments + `}],"stop_reason":"tool_use"}`},
		{contract.ProtocolGoogleGenerateContent, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":` + arguments + `}}]},"finishReason":"STOP"}]}`},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			calls := observeAll(t, toolreview.NewCollector(tc.protocol, false), tc.payload)
			if len(calls) != 1 || calls[0].Arguments != arguments {
				t.Fatalf("user argument keys were folded: %#v", calls)
			}
		})
	}
}

func TestJevFieldCaseAliasesFailClosed(t *testing.T) {
	for _, body := range []string{
		strings.Replace(flagAnswer, `"choice":"flag"`, `"choice":"flag","Choice":"pass"`, 1),
		strings.Replace(passAnswer, `"type":"choice"`, `"type":"score","TYPE":"choice"`, 1),
		strings.Replace(passAnswer, `"confidence":0.6`, `"confidence":2,"Confidence":0.6`, 1),
		`{"answers":{"review":{"type":"choice","choice":"flag","probabilities":{"pass":0.2,"flag":0.8},"confidence":0.6}},"Answers":{"review":{"type":"choice","choice":"pass","probabilities":{"pass":0.8,"flag":0.2},"confidence":0.6}}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		s := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, contract.ToolReviewModeReject), singleInput())
		server.Close()
		if s.Status != "failed" || s.ErrorCode != "invalid_response" || s.Calls[0].Action != "withheld" {
			t.Fatalf("Jev alias accepted: %+v", s)
		}
	}
}
