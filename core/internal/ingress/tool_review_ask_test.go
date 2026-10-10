package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

type askRecorder struct {
	*httptest.ResponseRecorder
	mu sync.Mutex
}

func newAskRecorder() *askRecorder { return &askRecorder{ResponseRecorder: httptest.NewRecorder()} }
func (w *askRecorder) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ResponseRecorder.Write(b)
}
func (w *askRecorder) WriteString(body string) (int, error) { return w.Write([]byte(body)) }
func (w *askRecorder) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ResponseRecorder.WriteHeader(status)
}
func (w *askRecorder) Flush()       { w.mu.Lock(); defer w.mu.Unlock(); w.ResponseRecorder.Flush() }
func (w *askRecorder) body() string { w.mu.Lock(); defer w.mu.Unlock(); return w.Body.String() }

type approvalRecords struct {
	*reviewRecords
	muApproval     sync.Mutex
	history        []contract.ToolReviewSummary
	failState      string
	terminal       chan contract.RequestRecord
	beforeApproved func(contract.RequestRecord)
}

func (s *approvalRecords) UpsertRequestRecord(ctx context.Context, record contract.RequestRecord) error {
	if s.beforeApproved != nil && record.ToolReview != nil && record.ToolReview.Status == "pending" && record.ToolReview.Approval != nil && record.ToolReview.Approval.State == "approved" {
		s.beforeApproved(record)
	}
	if record.ToolReview != nil && record.ToolReview.Approval != nil {
		s.muApproval.Lock()
		if record.ToolReview.Approval.State == s.failState {
			s.muApproval.Unlock()
			return errors.New("private storage diagnostic")
		}
		raw, _ := json.Marshal(record.ToolReview)
		var snapshot contract.ToolReviewSummary
		_ = json.Unmarshal(raw, &snapshot)
		s.history = append(s.history, snapshot)
		s.muApproval.Unlock()
	}
	raw, _ := json.Marshal(record)
	var copy contract.RequestRecord
	_ = json.Unmarshal(raw, &copy)
	err := s.reviewRecords.UpsertRequestRecord(ctx, copy)
	if s.terminal != nil && copy.Status != contract.RequestStatusPending {
		select {
		case s.terminal <- copy:
		default:
		}
	}
	return err
}
func (s *approvalRecords) InsertRequestRecord(ctx context.Context, r contract.RequestRecord) error {
	return s.UpsertRequestRecord(ctx, r)
}

func waitIngressApproval(t *testing.T, b *toolreview.Broker, done <-chan struct{}) contract.ToolReviewApprovalRequest {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		if items := b.List(); len(items) > 0 {
			return items[0]
		}
		select {
		case <-done:
			t.Fatal("request finished without approval")
		case <-timer.C:
			t.Fatal("approval not published")
		default:
			runtime.Gosched()
		}
	}
}
func startAskRequest(t *testing.T, handler *Handler, writer http.ResponseWriter, request *http.Request) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(request.Context())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("cancelled ask request did not stop during cleanup")
		}
	})
	go func() {
		defer close(done)
		handler.ServeHTTP(writer, request.WithContext(ctx))
	}()
	return done
}

func waitIngressDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish")
	}
}

func TestToolReviewAskHTTPAndSSEApprovalDeliveryAndAffinity(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, decision := range []string{"allow", "deny", "cancel", "expire", "shutdown"} {
			t.Run(map[bool]string{false: "buffered", true: "sse"}[streaming]+"/"+decision, func(t *testing.T) {
				var attempts atomic.Int32
				h, _ := reviewTestHandler(t, contract.ToolReviewModeAsk, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }, func(w http.ResponseWriter, _ *http.Request) {
					attempts.Add(1)
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						for _, e := range reviewResponseEvents {
							_, _ = io.WriteString(w, "data: "+e+"\n\n")
						}
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, reviewDangerousResponse)
					}
				}, streaming)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				broker := toolreview.NewBroker(context.Background())
				defer broker.Close()
				h.toolReviewApprovals = broker
				settings := h.toolReviewSettings.(reviewSettingsSource)
				settings.settings.TimeoutMS = 500
				if decision == "expire" {
					settings.settings.ApprovalTimeoutMS = 1000
				}
				h.toolReviewSettings = settings
				records := &approvalRecords{reviewRecords: newReviewRecords()}
				h.requestRecords = records
				response := newAskRecorder()
				body := `{"model":"m","input":"private user constraints"}`
				if streaming {
					body = `{"model":"m","input":"private user constraints","stream":true}`
				}
				done := startAskRequest(t, h, response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)).WithContext(ctx))
				item := waitIngressApproval(t, broker, done)
				pending := records.latest(t).ToolReview
				if pending == nil || pending.Status != "pending" || pending.Approval.State != "pending" || pending.Approval.ReviewStatus != "flagged" || pending.Calls[0].Verdict != "flag" || pending.Calls[0].ModelVerdict != "flag" || pending.Calls[0].Action != "withheld" {
					t.Fatalf("pending=%+v", pending)
				}
				raw, _ := json.Marshal(item)
				if strings.Contains(string(raw), "private user") || strings.Contains(string(raw), "remove-important") {
					t.Fatal("pending contains raw input")
				}
				// The approval timer may expire concurrently with this inspection.
				if strings.Contains(response.body(), "function_call") {
					t.Fatal("tools leaked before decision")
				}
				if _, err := h.bindResponseAffinity(context.Background(), Request{Protocol: contract.ProtocolOpenAIResponses, Model: "m", PreviousResponseID: "resp_review"}, h.resolver.(candidateResolver).candidates); err == nil {
					t.Fatal("pending request bound affinity")
				}
				if decision == "allow" {
					// Saving new settings must not mutate the in-flight snapshot.
					next := settings
					next.settings.Mode = contract.ToolReviewModeReject
					next.settings.Revision = "next-revision"
					h.toolReviewSettings = next
				}
				switch decision {
				case "cancel":
					cancel()
				case "shutdown":
					broker.Close()
				case "expire":
				case "allow", "deny":
					if _, err := broker.Decide(item.ID, decision); err != nil {
						t.Fatal(err)
					}
				}
				waitIngressDone(t, done)
				record := records.latest(t)
				summary := record.ToolReview
				want := "approved"
				switch decision {
				case "deny":
					want = "denied"
				case "cancel":
					want = "cancelled"
				case "expire":
					want = "expired"
				case "shutdown":
					want = "interrupted"
				}
				if summary == nil || summary.Approval == nil || summary.Approval.State != want || summary.Status != "flagged" || summary.Mode != contract.ToolReviewModeAsk || summary.ConfigRevision != "test-v1" {
					t.Fatalf("final=%+v", summary)
				}
				allowed := decision == "allow"
				if allowed {
					if record.Status != contract.RequestStatusSucceeded || summary.Calls[0].Action != "delivered" || !strings.Contains(response.Body.String(), "function_call") {
						t.Fatalf("approved delivery=%s %+v %s", record.Status, summary, response.Body.String())
					}
					if _, err := h.bindResponseAffinity(context.Background(), Request{Protocol: contract.ProtocolOpenAIResponses, Model: "m", PreviousResponseID: "resp_review"}, h.resolver.(candidateResolver).candidates); err != nil {
						t.Fatalf("approved affinity: %v", err)
					}
					records.muApproval.Lock()
					found := false
					for _, s := range records.history {
						if s.Status == "pending" && s.Approval.State == "approved" && s.Calls[0].Action == "pending" {
							found = true
						}
					}
					records.muApproval.Unlock()
					if !found {
						t.Fatal("release skipped approved pending persistence")
					}
				} else if strings.Contains(response.Body.String(), "function_call") || summary.Calls[0].Action != "withheld" || record.Status == contract.RequestStatusSucceeded {
					t.Fatalf("unapproved delivery=%s %+v %s", record.Status, summary, response.Body.String())
				}
				if attempts.Load() != 1 || len(broker.List()) != 0 {
					t.Fatalf("attempts=%d queue=%d", attempts.Load(), len(broker.List()))
				}
				if _, err := broker.Decide(item.ID, "allow"); err == nil {
					t.Fatal("late decision took effect")
				}
			})
		}
	}
}

func TestToolReviewAskPendingRequestCleanupCancels(t *testing.T) {
	var records *reviewRecords
	t.Run("return while pending", func(t *testing.T) {
		var handler *Handler
		handler, records = reviewTestHandler(t, contract.ToolReviewModeAsk, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, reviewDangerousResponse) }, false)
		broker := toolreview.NewBroker(context.Background())
		t.Cleanup(broker.Close)
		handler.toolReviewApprovals = broker
		done := startAskRequest(t, handler, httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)))
		waitIngressApproval(t, broker, done)
		// Return without deciding. Cleanup must cancel and join this request,
		// exactly as it would after an earlier assertion called Fatal.
	})
	if t.Failed() {
		return
	}
	record := records.latest(t)
	if record.Status != contract.RequestStatusCancelled || record.ToolReview.Approval.State != "cancelled" {
		t.Fatalf("cleanup did not cancel pending request: %s %+v", record.Status, record.ToolReview)
	}
}

func TestToolReviewAskStoreFailureNeverReleases(t *testing.T) {
	for _, state := range []string{"pending", "approved"} {
		t.Run(state, func(t *testing.T) {
			h, _ := reviewTestHandler(t, contract.ToolReviewModeAsk, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, reviewDangerousResponse) }, false)
			broker := toolreview.NewBroker(context.Background())
			defer broker.Close()
			h.toolReviewApprovals = broker
			records := &approvalRecords{reviewRecords: newReviewRecords(), failState: state}
			h.requestRecords = records
			response := httptest.NewRecorder()
			done := startAskRequest(t, h, response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)))
			if state == "approved" {
				item := waitIngressApproval(t, broker, done)
				if _, err := broker.Decide(item.ID, "allow"); err != nil {
					t.Fatal(err)
				}
			}
			waitIngressDone(t, done)
			record := records.latest(t)
			if response.Code != 503 || strings.Contains(response.Body.String(), "function_call") || strings.Contains(response.Body.String(), "private storage") || record.Status == contract.RequestStatusSucceeded {
				t.Fatalf("store fail=%d %s %+v", response.Code, response.Body.String(), record)
			}
			if len(broker.List()) != 0 {
				t.Fatal("failed persistence published approval")
			}
		})
	}
}

func TestToolReviewAskReviewErrorOptInAndIndependentBudget(t *testing.T) {
	for _, failure := range []string{"http", "schema", "timeout"} {
		for _, optIn := range []bool{false, true} {
			t.Run(failure+map[bool]string{false: "/closed", true: "/ask"}[optIn], func(t *testing.T) {
				releaseJudge := make(chan struct{})
				defer close(releaseJudge)
				h, records := reviewTestHandler(t, contract.ToolReviewModeAsk, func(w http.ResponseWriter, r *http.Request) {
					switch failure {
					case "http":
						w.WriteHeader(503)
					case "schema":
						_, _ = io.WriteString(w, `{"answers":{"review":{}}}`)
					case "timeout":
						select {
						case <-r.Context().Done():
						case <-releaseJudge:
						}
					}
				}, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, reviewDangerousResponse) }, false)
				settings := h.toolReviewSettings.(reviewSettingsSource)
				settings.settings.AskOnReviewError = optIn
				settings.settings.TimeoutMS = 20
				h.toolReviewSettings = settings
				broker := toolreview.NewBroker(context.Background())
				defer broker.Close()
				h.toolReviewApprovals = broker
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				response := httptest.NewRecorder()
				done := startAskRequest(t, h, response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)).WithContext(ctx))
				if optIn {
					item := waitIngressApproval(t, broker, done)
					if item.Summary.Approval.ReviewStatus != "failed" {
						t.Fatalf("error approval=%+v", item)
					}
					// Let the original review budget elapse; manual approval must still work.
					select {
					case <-time.After(30 * time.Millisecond):
					case <-done:
						t.Fatal("review timeout constrained human wait")
					}
					if _, err := broker.Decide(item.ID, "allow"); err != nil {
						t.Fatal(err)
					}
				}
				waitIngressDone(t, done)
				record := records.latest(t)
				if record.ToolReview.Status != "failed" {
					t.Fatalf("approved error rewritten=%+v", record.ToolReview)
				}
				if optIn {
					if record.Status != contract.RequestStatusSucceeded || record.ToolReview.Approval.State != "approved" || record.ToolReview.Calls[0].Action != "delivered" {
						t.Fatalf("approved error delivery=%s %+v", record.Status, record.ToolReview)
					}
				} else if response.Code != 503 || strings.Contains(response.Body.String(), "function_call") {
					t.Fatalf("default failure opened: %d %s", response.Code, response.Body.String())
				}
			})
		}
	}
}

func TestToolReviewAskInvalidCollectionNeverQueues(t *testing.T) {
	for _, body := range []string{`{"output":[{"type":"function_call","name":"bash","arguments":"invalid"}],"status":"completed"}`, `{"output":[{"type":"function_call","name":"bash","arguments":"{}"}],"status":"in_progress"}`, `{not json}`} {
		t.Run(body, func(t *testing.T) {
			h, _ := reviewTestHandler(t, contract.ToolReviewModeAsk, func(http.ResponseWriter, *http.Request) { t.Error("invalid tool reached reviewer") }, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }, false)
			settings := h.toolReviewSettings.(reviewSettingsSource)
			settings.settings.AskOnReviewError = true
			h.toolReviewSettings = settings
			broker := toolreview.NewBroker(context.Background())
			defer broker.Close()
			h.toolReviewApprovals = broker
			response := httptest.NewRecorder()
			done := startAskRequest(t, h, response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)))
			waitIngressDone(t, done)
			if response.Code != 503 || len(broker.List()) != 0 || strings.Contains(response.Body.String(), "function_call") {
				t.Fatalf("invalid collection approved=%d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestToolReviewAskApprovedPersistenceRechecksReleaseConditions(t *testing.T) {
	for _, event := range []string{"cancel", "shutdown", "deadline"} {
		t.Run(event, func(t *testing.T) {
			h, _ := reviewTestHandler(t, contract.ToolReviewModeAsk, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, reviewDangerousResponse) }, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			broker := toolreview.NewBroker(context.Background())
			defer broker.Close()
			h.toolReviewApprovals = broker
			source := h.toolReviewSettings.(reviewSettingsSource)
			source.settings.ApprovalTimeoutMS = 1000
			h.toolReviewSettings = source
			records := &approvalRecords{reviewRecords: newReviewRecords(), beforeApproved: func(r contract.RequestRecord) {
				switch event {
				case "cancel":
					cancel()
				case "shutdown":
					broker.Close()
				case "deadline":
					timer := time.NewTimer(time.Until(r.ToolReview.Approval.ExpiresAt))
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-ctx.Done():
					}
				}
			}}
			h.requestRecords = records
			response := httptest.NewRecorder()
			done := startAskRequest(t, h, response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)).WithContext(ctx))
			item := waitIngressApproval(t, broker, done)
			if _, err := broker.Decide(item.ID, "allow"); err != nil {
				t.Fatal(err)
			}
			waitIngressDone(t, done)
			record := records.latest(t)
			if record.Status == contract.RequestStatusSucceeded || record.ToolReview.Status != "flagged" || record.ToolReview.Approval.State != "approved" || record.ToolReview.Calls[0].Action != "withheld" || strings.Contains(response.Body.String(), "function_call") {
				t.Fatalf("released after %s during persist: %s %+v", event, record.Status, record.ToolReview)
			}
		})
	}
}

func TestToolReviewAskQueueFullDoesNotRetryOrRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	broker := toolreview.NewBroker(ctx)
	defer broker.Close()
	var wg sync.WaitGroup
	workersDone := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		broker.Close()
		select {
		case <-workersDone:
		case <-time.After(time.Second):
			t.Error("queue workers did not stop during cleanup")
		}
	})
	for i := 0; i < toolreview.ApprovalCapacity; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := broker.Wait(ctx, "request_queue", contract.ToolReviewSummary{Status: "flagged", Mode: contract.ToolReviewModeAsk, Model: "jev", StartedAt: time.Now(), Calls: []contract.ToolReviewCall{{Name: "bash", Verdict: "flag", Action: "withheld"}}}, toolreview.Input{Calls: []toolreview.Call{{Name: "bash", Arguments: "{}"}}}, time.Minute, func(contract.ToolReviewApproval) error { return nil })
			if err != nil {
				t.Errorf("filling queue: %v", err)
			}
		}()
	}
	go func() { wg.Wait(); close(workersDone) }()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for len(broker.List()) != toolreview.ApprovalCapacity {
		select {
		case <-timer.C:
			t.Fatal("queue did not fill")
		default:
			runtime.Gosched()
		}
	}
	var attempts atomic.Int32
	h, records := reviewTestHandler(t, contract.ToolReviewModeAsk, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		_, _ = io.WriteString(w, reviewDangerousResponse)
	}, false)
	h.toolReviewApprovals = broker
	response := httptest.NewRecorder()
	done := startAskRequest(t, h, response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)))
	waitIngressDone(t, done)
	record := records.latest(t)
	if response.Code != 503 || record.ToolReview.ErrorCode != "approval_queue_full" || record.ToolReview.Approval != nil || record.ToolReview.Calls[0].Action != "withheld" || strings.Contains(response.Body.String(), "function_call") || attempts.Load() != 1 {
		t.Fatalf("queue full=%d %s %+v", response.Code, response.Body.String(), record.ToolReview)
	}
	cancel()
	waitIngressDone(t, workersDone)
	if len(broker.List()) != 0 {
		t.Fatal("queue cancellation leaked entries")
	}
}

func TestToolReviewAskPassNeedsNoApproval(t *testing.T) {
	h, records := reviewTestHandler(t, contract.ToolReviewModeAsk, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "pass") }, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, reviewDangerousResponse) }, false)
	broker := toolreview.NewBroker(context.Background())
	defer broker.Close()
	h.toolReviewApprovals = broker
	response := httptest.NewRecorder()
	done := startAskRequest(t, h, response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)))
	waitIngressDone(t, done)
	record := records.latest(t)
	if response.Code != 200 || record.Status != contract.RequestStatusSucceeded || record.ToolReview.Status != "passed" || record.ToolReview.Approval != nil || len(broker.List()) != 0 {
		t.Fatalf("pass path=%d %s %+v", response.Code, record.Status, record.ToolReview)
	}
}

func TestToolReviewAskApprovedDeliveryFailureNeverBinds(t *testing.T) {
	for _, zero := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "zero"}[zero], func(t *testing.T) {
			h, records := reviewTestHandler(t, contract.ToolReviewModeAsk, func(w http.ResponseWriter, _ *http.Request) { sendReviewChoice(w, "flag") }, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, reviewDangerousResponse) }, false)
			broker := toolreview.NewBroker(context.Background())
			defer broker.Close()
			h.toolReviewApprovals = broker
			response := &reviewBrokenWriter{header: make(http.Header), zero: zero}
			done := startAskRequest(t, h, response, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m","input":"inspect"}`)))
			item := waitIngressApproval(t, broker, done)
			if _, err := broker.Decide(item.ID, "allow"); err != nil {
				t.Fatal(err)
			}
			waitIngressDone(t, done)
			record := records.latest(t)
			if record.ToolReview.Status != "flagged" || record.ToolReview.Approval.State != "approved" || record.Status != contract.RequestStatusFailed || record.ToolReview.Calls[0].Action == "delivered" {
				t.Fatalf("delivery failure=%s %+v", record.Status, record.ToolReview)
			}
			if _, err := h.bindResponseAffinity(context.Background(), Request{Protocol: contract.ProtocolOpenAIResponses, Model: "m", PreviousResponseID: "resp_review"}, h.resolver.(candidateResolver).candidates); err == nil {
				t.Fatal("failed delivery bound affinity")
			}
		})
	}
}
