package toolreview_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func TestResponsesStandardMessagePhase(t *testing.T) {
	// ResponseOutputMessage.phase is optional and nullable. See the current
	// Responses API reference and the matching official Go SDK message type:
	// https://developers.openai.com/api/reference/resources/responses/methods/create
	// https://github.com/openai/openai-go/blob/main/responses/response.go
	for _, batch := range []bool{false, true} {
		for _, tc := range []struct {
			name, fields, code string
			secondMessage      bool
		}{
			{name: "omitted"},
			{name: "null", fields: `"phase":null,`},
			{name: "final answer", fields: `"phase":"final_answer",`},
			{name: "commentary is not final", fields: `"phase":"commentary",`, code: "reviewer_invalid_review"},
			{name: "unknown", fields: `"phase":"private-response",`, code: "reviewer_invalid_review"},
			{name: "empty", fields: `"phase":"",`, code: "reviewer_invalid_review"},
			{name: "invalid type", fields: `"phase":7,`, code: "reviewer_invalid_json"},
			{name: "alias", fields: `"Phase":"final_answer",`, code: "reviewer_invalid_json"},
			{name: "alias pair", fields: `"phase":"final_answer","Phase":"commentary",`, code: "reviewer_invalid_json"},
			{name: "duplicate", fields: `"phase":"commentary","phase":"final_answer",`, code: "reviewer_invalid_json"},
			{name: "final with refusal", fields: `"phase":"final_answer","refusal":"private-response",`, code: "reviewer_invalid_review"},
			{name: "final with tool output", fields: `"phase":"final_answer","tool_output":"private-response",`, code: "reviewer_invalid_review"},
			{name: "multiple final messages", fields: `"phase":"final_answer",`, code: "reviewer_invalid_review", secondMessage: true},
			{name: "commentary then final", fields: `"phase":"commentary",`, code: "reviewer_invalid_review", secondMessage: true},
		} {
			t.Run(fmt.Sprintf("batch=%t/%s", batch, tc.name), func(t *testing.T) {
				input := responsesInput(1)
				var requests atomic.Int32
				body := strings.Replace(responsesEnvelopeText(responsesText(input.Calls)), `"role":"assistant",`, `"role":"assistant",`+tc.fields, 1)
				if tc.secondMessage {
					// Do not select a final message from an ambiguous multi-message
					// output, even if its sibling is labelled commentary.
					body = strings.Replace(body, `],"usage":`, `,{"id":"msg_second","type":"message","status":"completed","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"{}","annotations":[]}]}],"usage":`, 1)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = io.WriteString(w, body)
				}))
				defer server.Close()
				s := responsesReviewer(server).Review(context.Background(), responsesSnapshot(server.URL, batch, contract.ToolReviewModeAsk), input)
				if requests.Load() != 1 || len(s.Calls) != 1 || s.ErrorCode != tc.code {
					t.Fatalf("phase handling or no-retry changed: %+v requests=%d", s, requests.Load())
				}
				call := s.Calls[0]
				if tc.code == "" {
					if s.Status != "passed" || call.Model != "actual-model" || call.ModelVerdict != "pass" || call.Verdict != "pass" || call.Action != "delivered" || *call.FlagProbability != 0.2 || call.ReasonCode != "authorized" {
						t.Fatalf("standard final message rejected: %+v", s)
					}
				} else if s.Status != "failed" || !toolreview.CanAskOnError(s) || call.Verdict != "error" || call.Action != "withheld" || call.PassProbability != nil || call.FlagProbability != nil || call.ReasonCode != "" || s.Approval != nil {
					t.Fatalf("intermediate/ambiguous/malformed message used as result: %+v", s)
				}
				if err := s.Validate(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
