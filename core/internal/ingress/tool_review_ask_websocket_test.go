package ingress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
	"github.com/gorilla/websocket"
)

func TestToolReviewAskWebSocketApprovalCancelAndDisconnect(t *testing.T) {
	for _, scenario := range []string{"allow", "deny", "cancel", "disconnect"} {
		t.Run(scenario, func(t *testing.T) {
			var attempts atomic.Int32
			provider := wsUpstream(t, func(conn *websocket.Conn, _ *http.Request) {
				var create map[string]any
				if conn.ReadJSON(&create) != nil {
					return
				}
				attempts.Add(1)
				for _, event := range reviewResponseEvents {
					if conn.WriteMessage(websocket.TextMessage, []byte(event)) != nil {
						return
					}
				}
			})
			judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }))
			defer judge.Close()
			settings := contract.DefaultToolReviewSettings()
			settings.Enabled, settings.CredentialConfigured, settings.Mode, settings.Endpoint = true, true, contract.ToolReviewModeAsk, judge.URL
			broker := toolreview.NewBroker(context.Background())
			defer broker.Close()
			records := &approvalRecords{reviewRecords: newReviewRecords(), terminal: make(chan contract.RequestRecord, 1)}
			h := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{wsCandidate(provider.URL)}}, AccessTokenAuthenticator: AccessTokenAuthenticatorFunc(func(context.Context, string) (contract.AccessTokenID, error) { return "token_test", nil }), ToolReviewSettings: reviewSettingsSource{settings}, ToolReviewApprovals: broker, RequestRecords: records})
			client := dialResponses(t, h, http.Header{"Authorization": {"Bearer local-test"}})
			sendWS(t, client, `{"type":"response.create","model":"gpt-5","input":"inspect"}`)
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			var item contract.ToolReviewApprovalRequest
			for {
				items := broker.List()
				if len(items) > 0 {
					item = items[0]
					break
				}
				select {
				case r := <-records.terminal:
					t.Fatalf("ended before approval: %+v", r)
				case <-timer.C:
					t.Fatal("WS approval not published")
				default:
					runtime.Gosched()
				}
			}
			switch scenario {
			case "cancel":
				sendWS(t, client, `{"type":"response.cancel"}`)
			case "disconnect":
				_ = client.Close()
			default:
				if _, err := broker.Decide(item.ID, scenario); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "allow" {
				found := false
				for !found {
					event := readWS(t, client)
					if event["type"] == "error" {
						t.Fatalf("approved WS failed: %#v", event)
					}
					found = event["type"] == "response.completed"
				}
			}
			if scenario == "deny" {
				event := readWS(t, client)
				if event["type"] != "error" {
					t.Fatalf("denied WS leaked tool: %#v", event)
				}
			}
			var record contract.RequestRecord
			select {
			case record = <-records.terminal:
			case <-time.After(2 * time.Second):
				t.Fatal("WS request did not clean up")
			}
			want := "approved"
			switch scenario {
			case "deny":
				want = "denied"
			case "cancel", "disconnect":
				want = "cancelled"
			}
			if record.ToolReview == nil || record.ToolReview.Approval == nil || record.ToolReview.Approval.State != want || record.ToolReview.Status != "flagged" {
				t.Fatalf("WS record=%s %+v", record.Status, record.ToolReview)
			}
			if scenario == "allow" {
				if record.Status != contract.RequestStatusSucceeded || record.ToolReview.Calls[0].Action != "delivered" {
					t.Fatalf("WS allow=%s %+v", record.Status, record.ToolReview)
				}
			} else if record.Status == contract.RequestStatusSucceeded || record.ToolReview.Calls[0].Action != "withheld" {
				t.Fatalf("WS cancelled/rejected=%s %+v", record.Status, record.ToolReview)
			}
			if attempts.Load() != 1 || len(broker.List()) != 0 {
				t.Fatal("WS approval retried or leaked queue")
			}
			if _, err := broker.Decide(item.ID, "allow"); err == nil {
				t.Fatal("WS late decision took effect")
			}
		})
	}
}
