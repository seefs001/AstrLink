package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/builtintools"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

type builtinApprovalRecords struct {
	*approvalRecords
	routing contract.RoutingSettings
}

func (s *builtinApprovalRecords) GetRoutingSettings(context.Context) (contract.RoutingSettings, error) {
	return s.routing, nil
}
func (s *builtinApprovalRecords) UpdateRoutingSettings(context.Context, contract.RoutingSettings) error {
	return fmt.Errorf("test routing settings are immutable")
}
func (s *builtinApprovalRecords) root(t *testing.T) contract.RequestRecord {
	t.Helper()
	s.reviewRecords.mu.Lock()
	defer s.reviewRecords.mu.Unlock()
	for _, record := range s.reviewRecords.records {
		if record.ToolReview != nil {
			return record
		}
	}
	t.Fatal("no outer tool review record")
	return contract.RequestRecord{}
}

// Only fail publication of the reviewed client batch, not the earlier created
// event or the gateway-owned web-search progress shown while approval is pending.
type builtinPublicationWriter struct {
	*askRecorder
	fail         atomic.Bool
	failures     atomic.Int32
	terminalOnly bool
}

func (w *builtinPublicationWriter) FlushError() error {
	body := w.body()
	terminal := strings.Contains(body, "event: response.completed\n") ||
		(strings.HasPrefix(strings.TrimSpace(body), "{") && strings.Contains(body, `"status":"completed"`))
	if w.fail.Load() && strings.Contains(body, `"id":"fc_client"`) && (!w.terminalOnly || terminal) {
		w.failures.Add(1)
		return io.ErrClosedPipe
	}
	return nil
}

func builtinPublicIDs(t *testing.T, body string, streaming bool) (string, string) {
	t.Helper()
	var responseID, itemID string
	observe := func(response builtintools.Object) {
		if id := builtintools.String(response["id"]); strings.HasPrefix(id, "resp_tool_") {
			responseID = id
		}
		for _, raw := range builtintools.Array(response["output"]) {
			item := builtintools.Map(raw)
			if id := builtintools.String(item["id"]); strings.HasPrefix(id, "ws_tool_") {
				itemID = id
			}
		}
	}
	if streaming {
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event builtintools.Object
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
				t.Fatal("invalid emitted SSE")
			}
			observe(builtintools.Map(event["response"]))
			if id := builtintools.String(builtintools.Map(event["item"])["id"]); strings.HasPrefix(id, "ws_tool_") {
				itemID = id
			}
		}
	} else {
		var response builtintools.Object
		if json.NewDecoder(strings.NewReader(body)).Decode(&response) != nil {
			t.Fatal("invalid emitted JSON")
		}
		observe(response)
	}
	if responseID == "" || itemID == "" {
		t.Fatalf("public response/item IDs missing: %s", body)
	}
	return responseID, itemID
}

func requireBuiltinPrepare(t *testing.T, h *Handler, responseID, itemID string, visible bool) {
	t.Helper()
	for _, input := range []builtintools.Object{{"model": "main", "previous_response_id": responseID, "input": "continue"}, {"model": "main", "input": []any{builtintools.Object{"id": itemID, "type": "web_search_call"}}}} {
		state, err := h.builtinStates.Prepare("", builtintools.Clone(input))
		if (err == nil) != visible {
			t.Fatalf("Prepare visibility=%v want %v body=%v error=%v", err == nil, visible, input, err)
		}
		if visible && (state.ServiceID != "service_main" || state.RoutingModel != "main" || len(state.Replacements[itemID]) != 2 || !strings.Contains(builtintools.Text(state.History), "call_owned")) {
			t.Fatalf("published continuation lost full state: %+v", state)
		}
	}
}

func newBuiltinApprovalGateway(t *testing.T, mode contract.ToolReviewMode, enabled bool) (*Handler, *builtinApprovalRecords, *toolreview.Broker) {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body builtintools.Object
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid model request")
			return
		}
		var output []any
		if body["model"] == "search" {
			output = []any{builtintools.Object{"type": "web_search_call", "id": "ws_native", "status": "completed", "action": builtintools.Object{"type": "search", "query": "fact", "sources": []any{}}}, builtinMessage("search result")}
		} else {
			tool := builtintools.Map(builtintools.Array(body["tools"])[0])
			output = []any{builtintools.Object{"id": "fc_owned", "type": "function_call", "call_id": "call_owned", "name": tool["name"], "arguments": `{"action":"search","query":"fact"}`, "status": "completed"}, builtintools.Object{"id": "fc_client", "type": "function_call", "call_id": "call_client", "name": "client_tool", "arguments": `{"command":"client pending command"}`, "status": "completed"}}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", builtintools.Text(builtintools.Object{"type": "response.completed", "response": builtintools.Object{"id": "resp_inner", "status": "completed", "model": body["model"], "output": output}}))
	}))
	t.Cleanup(provider.Close)
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }))
	t.Cleanup(judge.Close)
	main := wsCandidate(provider.URL)
	main.Service.ID = "service_main"
	main.Service.Models = []string{"main"}
	main.Service.ResponsesWebSocketEnabled = nil
	native := wsCandidate(provider.URL)
	native.Service.ID = "service_search"
	native.Service.Models = []string{"search"}
	settings := contract.DefaultToolReviewSettings()
	settings.Enabled, settings.CredentialConfigured, settings.Mode, settings.Endpoint = enabled, true, mode, judge.URL
	routing := contract.DefaultRoutingSettings()
	routing.BuiltinTools = &contract.BuiltinTools{WebSearch: contract.BuiltinTool{Enabled: true, Backend: "upstream", ServiceID: "service_search", Model: "search"}}
	records := &builtinApprovalRecords{approvalRecords: &approvalRecords{reviewRecords: newReviewRecords()}, routing: routing}
	broker := toolreview.NewBroker(context.Background())
	t.Cleanup(broker.Close)
	handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{main, native}}, RequestRecords: records, ToolReviewSettings: reviewSettingsSource{settings}, ToolReviewApprovals: broker})
	return handler, records, broker
}

func builtinApprovalRequest(streaming bool) *http.Request {
	return httptest.NewRequest("POST", "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"main","input":"inspect","stream":%t,"tools":[{"type":"web_search"},{"type":"function","name":"client_tool","parameters":{"type":"object"}}]}`, streaming)))
}

func TestToolReviewAskBuiltinStatePublishesOnlyAfterApprovalAndDelivery(t *testing.T) {
	for _, outcome := range []string{"allow", "deny", "cancel", "expire", "flush_failure"} {
		t.Run(outcome, func(t *testing.T) {
			h, records, broker := newBuiltinApprovalGateway(t, contract.ToolReviewModeAsk, true)
			source := h.toolReviewSettings.(reviewSettingsSource)
			if outcome == "expire" {
				source.settings.ApprovalTimeoutMS = 1000
			}
			h.toolReviewSettings = source
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writer := &builtinPublicationWriter{askRecorder: newAskRecorder(), terminalOnly: true}
			if outcome == "flush_failure" {
				records.beforeApproved = func(contract.RequestRecord) { writer.fail.Store(true) }
			}
			done := startAskRequest(t, h, writer, builtinApprovalRequest(true).WithContext(ctx))
			approval := waitIngressApproval(t, broker, done)
			responseID, itemID := builtinPublicIDs(t, writer.body(), true)
			requireBuiltinPrepare(t, h, responseID, itemID, false)
			if strings.Contains(writer.body(), `"id":"fc_client"`) || writer.failures.Load() != 0 {
				t.Fatal("pending phase leaked client output or encountered an injected flush failure")
			}
			switch outcome {
			case "cancel":
				cancel()
			case "expire":
			case "deny":
				if _, err := broker.Decide(approval.ID, "deny"); err != nil {
					t.Fatal(err)
				}
			default:
				if _, err := broker.Decide(approval.ID, "allow"); err != nil {
					t.Fatal(err)
				}
			}
			waitIngressDone(t, done)
			requireBuiltinPrepare(t, h, responseID, itemID, outcome == "allow")
			record := records.root(t)
			if outcome == "allow" {
				if record.Status != contract.RequestStatusSucceeded || record.ToolReview.Calls[0].Action != "delivered" {
					t.Fatalf("approved builtin failed: %s %+v", record.Status, record.ToolReview)
				}
			} else if record.Status == contract.RequestStatusSucceeded || record.ToolReview.Calls[0].Action == "delivered" {
				t.Fatalf("unconfirmed builtin succeeded: %s %+v", record.Status, record.ToolReview)
			}
			if outcome == "flush_failure" {
				if record.ToolReview.Approval.State != "approved" {
					t.Fatal("flush failure rewrote approval")
				}
				if writer.failures.Load() == 0 {
					t.Fatal("approved terminal never reached the injected flush failure")
				}
			}
		})
	}
}

func TestToolReviewAskBuiltinBufferedFlushFailureDoesNotPublish(t *testing.T) {
	h, records, broker := newBuiltinApprovalGateway(t, contract.ToolReviewModeAsk, true)
	writer := &builtinPublicationWriter{askRecorder: newAskRecorder(), terminalOnly: true}
	records.beforeApproved = func(contract.RequestRecord) { writer.fail.Store(true) }
	done := startAskRequest(t, h, writer, builtinApprovalRequest(false))
	approval := waitIngressApproval(t, broker, done)
	if len(writer.body()) != 0 || writer.failures.Load() != 0 {
		t.Fatal("buffered pending phase wrote or flushed the response")
	}
	if _, err := broker.Decide(approval.ID, "allow"); err != nil {
		t.Fatal(err)
	}
	waitIngressDone(t, done)
	responseID, itemID := builtinPublicIDs(t, writer.body(), false)
	requireBuiltinPrepare(t, h, responseID, itemID, false)
	if writer.failures.Load() == 0 {
		t.Fatal("approved buffered response never reached the injected flush failure")
	}
	record := records.root(t)
	if record.Status != contract.RequestStatusFailed || record.ToolReview.Approval.State != "approved" || record.ToolReview.Status != "flagged" || record.ToolReview.Calls[0].Action == "delivered" {
		t.Fatalf("buffered flush failure=%s %+v", record.Status, record.ToolReview)
	}
}

func TestBuiltinStateMarkAndDisabledReviewRequireSuccessfulFinish(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			for _, fails := range []bool{false, true} {
				t.Run(fmt.Sprintf("review=%t/stream=%t/fails=%t", enabled, streaming, fails), func(t *testing.T) {
					h, records, _ := newBuiltinApprovalGateway(t, contract.ToolReviewModeMark, enabled)
					writer := &builtinPublicationWriter{askRecorder: newAskRecorder()}
					writer.fail.Store(fails)
					done := startAskRequest(t, h, writer, builtinApprovalRequest(streaming))
					waitIngressDone(t, done)
					// On stream failure, an error terminal may follow created; buffered failure
					// already wrote the response JSON into the failing writer's private buffer.
					body := writer.body()
					if !streaming && fails {
						if end := strings.Index(body, "}\n{"); end >= 0 {
							body = body[:end+1]
						}
					}
					responseID, itemID := builtinPublicIDs(t, body, streaming)
					requireBuiltinPrepare(t, h, responseID, itemID, !fails)
					if enabled && !fails {
						select {
						case <-records.updated:
						case <-time.After(time.Second):
							t.Fatal("mark review did not finish")
						}
					}
				})
			}
		}
	}
}
