package ingress

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

func TestToolReviewResponsesEmptyOutputTerminal(t *testing.T) {
	const command = "python3 - <<'PY'\nprint(\"hello\")\nPY\n"
	args, err := json.Marshal(map[string]any{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	item := map[string]any{"type": "function_call", "id": "a", "call_id": "ca", "name": "bash", "arguments": string(args), "status": "completed"}
	encode := func(event any) string {
		b, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	prefix := []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"r"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"a","call_id":"ca","name":"bash","arguments":""}}`,
	}
	for i := range len(args) {
		prefix = append(prefix, encode(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": "a", "delta": string(args[i : i+1])}))
	}
	prefix = append(prefix, encode(map[string]any{"type": "response.function_call_arguments.done", "output_index": 1, "item_id": "a", "arguments": string(args)}))
	for _, tc := range []struct {
		name     string
		mode     contract.ToolReviewMode
		choice   string
		itemDone bool
		status   string
	}{
		{"mark", contract.ToolReviewModeMark, "flag", true, "flagged"},
		{"reject_pass", contract.ToolReviewModeReject, "pass", true, "passed"},
		{"reject_flag", contract.ToolReviewModeReject, "flag", true, "blocked"},
		{"reject_unfinished", contract.ToolReviewModeReject, "pass", false, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := append([]string{}, prefix...)
			if tc.itemDone {
				events = append(events, encode(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": item}))
			}
			events = append(events, `{"type":"response.completed","response":{"status":"completed","output":[]}}`)
			stream := reviewSSE(events...)
			var reviews atomic.Int32
			judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reviews.Add(1)
				var request struct {
					State struct {
						Call struct {
							Name      string `json:"name"`
							Arguments struct {
								Command string `json:"command"`
							} `json:"arguments"`
						} `json:"tool_call"`
					} `json:"state"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || request.State.Call.Name != "bash" || request.State.Call.Arguments.Command != command {
					t.Error("judge did not receive the complete decoded call")
				}
				sendReviewChoice(w, tc.choice)
			}))
			defer judge.Close()
			settings := contract.DefaultToolReviewSettings()
			settings.Enabled, settings.CredentialConfigured = true, true
			settings.Mode, settings.Endpoint = tc.mode, judge.URL
			records := newReviewRecords()
			handler := NewWithDependencies(Dependencies{
				Resolver:           candidateResolver{candidates: []endpoint.Resolved{{Endpoint: validEndpoint(contract.ProtocolOpenAIResponses, true)}}},
				ToolReviewSettings: reviewSettingsSource{settings},
				RequestRecords:     records,
				Forwarder: transport.New(roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(iotest.OneByteReader(strings.NewReader(stream)))}, nil
				})),
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"Print hello without modifying files","stream":true}`)).WithContext(ctx))
			record := records.latest(t)
			if tc.mode == contract.ToolReviewModeMark {
				if record.ToolReview == nil || record.ToolReview.Status == "failed" {
					t.Fatalf("collection failed: %#v", record.ToolReview)
				}
				select {
				case record = <-records.updated:
				case <-ctx.Done():
					t.Fatal("mark review did not finish")
				}
			}
			if record.ToolReview == nil || record.ToolReview.Status != tc.status {
				t.Fatalf("review=%#v", record.ToolReview)
			}
			if tc.itemDone {
				if reviews.Load() != 1 || len(record.ToolReview.Calls) != 1 || record.ToolReview.Calls[0].Name != "bash" {
					t.Fatalf("reviews=%d summary=%#v", reviews.Load(), record.ToolReview)
				}
			} else if reviews.Load() != 0 || len(record.ToolReview.Calls) != 0 {
				t.Fatal("unfinished call reached judge")
			}
			if tc.mode == contract.ToolReviewModeMark || tc.status == "passed" {
				if record.Status != contract.RequestStatusSucceeded || response.Body.String() != stream {
					t.Fatal("successful stream changed")
				}
			} else if strings.Contains(response.Body.String(), "function_call") || strings.Contains(response.Body.String(), "python3") || !strings.Contains(response.Body.String(), "tool_review_") {
				t.Fatal("blocked stream leaked a tool or omitted its error")
			}
		})
	}
}
