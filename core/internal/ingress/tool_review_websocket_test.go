package ingress

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/gorilla/websocket"
)

// The upstream is a real WebSocket; this deterministic client sink injects a
// failure/cancellation at the actual per-message publication boundary.
type reviewWSClient struct {
	messages [][]byte
	failAt   int
	cancelAt int
	cancel   context.CancelFunc
}

func (*reviewWSClient) SetWriteDeadline(time.Time) error { return nil }
func (client *reviewWSClient) WriteMessage(_ int, data []byte) error {
	if len(client.messages) == client.failAt {
		return io.ErrClosedPipe
	}
	client.messages = append(client.messages, append([]byte(nil), data...))
	if len(client.messages) == client.cancelAt {
		client.cancel()
	}
	return nil
}

func TestToolReviewWebSocketPartialPublication(t *testing.T) {
	for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeReject, contract.ToolReviewModeMark} {
		for _, scenario := range []string{"partial write", "cancel replay", "terminal write", "pass", "terminal disconnect", "no tools terminal write"} {
			t.Run(string(mode)+"/"+scenario, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				client := &reviewWSClient{failAt: -1, cancel: cancel}
				events := append([]string(nil), reviewResponseEvents[:4]...)
				for _, event := range reviewResponseEvents[:4] {
					event = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(event, "fc_review", "fc_second"), "call_review", "call_second"), `"output_index":0`, `"output_index":1`)
					events = append(events, event)
				}
				events = append(events, `{"type":"response.completed","response":{"id":"resp_review","status":"completed","output":[{"id":"fc_review","type":"function_call","call_id":"call_review","name":"bash","arguments":"{\"command\":\"danger\"}"},{"id":"fc_second","type":"function_call","call_id":"call_second","name":"bash","arguments":"{\"command\":\"danger\"}"}]}}`)
				switch scenario {
				case "partial write":
					client.failAt = 4
				case "cancel replay":
					client.cancelAt = 4
				case "terminal write":
					client.failAt = 8
				case "terminal disconnect":
					client.cancelAt = 9
				case "no tools terminal write":
					client.failAt = 0
					events = []string{`{"type":"response.completed","response":{"id":"resp_review","status":"completed","output":[]}}`}
				}
				provider := wsUpstream(t, func(conn *websocket.Conn, _ *http.Request) {
					var create map[string]any
					if err := conn.ReadJSON(&create); err != nil {
						return
					}
					for _, event := range events {
						if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
							return
						}
					}
				})
				judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "pass") }))
				defer judge.Close()
				settings := contract.DefaultToolReviewSettings()
				settings.Enabled, settings.CredentialConfigured, settings.Mode, settings.Endpoint = true, true, mode, judge.URL
				records := newReviewRecords()
				handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{wsCandidate(provider.URL)}}, ToolReviewSettings: reviewSettingsSource{settings}, RequestRecords: records})
				session := &responsesWSSession{client: client}
				defer session.upstream.Close()
				turn := &responsesWSTurn{session: session, controls: make(chan []byte, 1), ctx: ctx}
				session.active = turn
				writer := &responsesWSWriter{turn: turn, header: make(http.Header)}
				request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"inspect","stream":true}`)).WithContext(context.WithValue(ctx, responsesWSTurnKey{}, turn))
				handler.ServeHTTP(writer, request)
				writer.finish()
				record := records.latest(t)
				success := scenario == "pass" || scenario == "terminal disconnect"
				if mode == contract.ToolReviewModeMark && success {
					select {
					case record = <-records.updated:
					case <-time.After(2 * time.Second):
						t.Fatal("mark review did not finish")
					}
				}
				want := contract.RequestStatusFailed
				if scenario == "cancel replay" {
					want = contract.RequestStatusCancelled
				}
				if success {
					want = contract.RequestStatusSucceeded
				}
				if record.Status != want || record.ToolReview == nil || (mode == contract.ToolReviewModeReject && scenario != "no tools terminal write" && len(record.ToolReview.Calls) != 2) {
					t.Fatalf("record status=%s review=%+v messages=%d", record.Status, record.ToolReview, len(client.messages))
				}
				wantAction := "pending"
				if success {
					wantAction = "delivered"
				}
				for _, call := range record.ToolReview.Calls {
					if call.Action != wantAction {
						t.Errorf("partial WS delivery falsely classified: %+v", call)
					}
				}
				terminals := 0
				for _, message := range client.messages {
					var event struct {
						Type string `json:"type"`
					}
					_ = json.Unmarshal(message, &event)
					if event.Type == "response.completed" {
						terminals++
						if !success {
							t.Error("failed/cancelled replay published success terminal")
						}
					}
					if scenario == "cancel replay" && strings.Contains(string(message), "fc_second") {
						t.Error("cancelled replay continued publishing tools")
					}
				}
				if success && terminals != 1 {
					t.Errorf("success terminal count=%d", terminals)
				}
			})
		}
	}
}
