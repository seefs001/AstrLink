package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

type reviewSettingsSource struct {
	settings contract.ToolReviewSettings
}

func (source reviewSettingsSource) LoadToolReviewSettings(context.Context) (contract.ToolReviewSettings, []byte, error) {
	return source.settings, []byte("test-review-key"), nil
}

type reviewRecords struct {
	mu      sync.Mutex
	records map[contract.RequestID]contract.RequestRecord
	updated chan contract.RequestRecord
}

func newReviewRecords() *reviewRecords {
	return &reviewRecords{records: make(map[contract.RequestID]contract.RequestRecord), updated: make(chan contract.RequestRecord, 4)}
}

func (store *reviewRecords) UpsertRequestRecord(_ context.Context, record contract.RequestRecord) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.records[record.ID] = record
	return nil
}

func (store *reviewRecords) InsertRequestRecord(ctx context.Context, record contract.RequestRecord) error {
	return store.UpsertRequestRecord(ctx, record)
}

func (*reviewRecords) FindSessionLink(context.Context, contract.SessionCursorKind, []string, storage.SessionCursorScope) (storage.SessionLinkMatch, bool, error) {
	return storage.SessionLinkMatch{}, false, nil
}

func (store *reviewRecords) UpdateRequestToolReview(_ context.Context, id contract.RequestID, summary contract.ToolReviewSummary) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, ok := store.records[id]
	if !ok {
		return fmt.Errorf("request missing")
	}
	record.ToolReview = &summary
	store.records[id] = record
	store.updated <- record
	return nil
}

func (store *reviewRecords) latest(t *testing.T) contract.RequestRecord {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, record := range store.records {
		return record
	}
	t.Fatal("no request record")
	return contract.RequestRecord{}
}

func reviewTestHandler(t *testing.T, mode contract.ToolReviewMode, jev http.HandlerFunc, upstream http.HandlerFunc, streaming bool) (*Handler, *reviewRecords) {
	t.Helper()
	judge := httptest.NewServer(jev)
	t.Cleanup(judge.Close)
	provider := httptest.NewServer(upstream)
	t.Cleanup(provider.Close)
	settings := contract.DefaultToolReviewSettings()
	settings.Enabled = true
	settings.CredentialConfigured = true
	settings.Mode = mode
	settings.Endpoint = judge.URL
	settings.Revision = "test-v1"
	records := newReviewRecords()
	service := validEndpoint(contract.ProtocolOpenAIResponses, streaming)
	service.BaseURL = provider.URL
	handler := NewWithDependencies(Dependencies{
		Resolver:           candidateResolver{candidates: []endpoint.Resolved{{Endpoint: service}}},
		ToolReviewSettings: reviewSettingsSource{settings: settings},
		RequestRecords:     records,
	})
	return handler, records
}

func sendReviewChoice(w http.ResponseWriter, choice string) {
	pass, flag := 0.99, 0.01
	if choice == "flag" {
		pass, flag = flag, pass
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model": "jev-test", "answers": map[string]any{"review": map[string]any{
			"type": "choice", "choice": choice, "probabilities": map[string]float64{"pass": pass, "flag": flag}, "confidence": 0.98,
		}},
	})
}

const reviewDangerousResponse = `{"id":"resp_review","status":"completed","output":[{"type":"function_call","id":"fc_review","call_id":"call_review","name":"bash","arguments":"{\"command\":\"remove-important-data\"}"}]}`

func TestToolReviewRejectsBeforeClientReceivesToolAndDoesNotRetry(t *testing.T) {
	var attempts atomic.Int32
	handler, records := reviewTestHandler(t, contract.ToolReviewModeReject, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-review-key" {
			t.Error("judge credential missing")
		}
		sendReviewChoice(w, "flag")
	}, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reviewDangerousResponse)
	}, false)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Inspect the project without changing data"}`)))
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "tool_review_blocked") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "remove-important-data") || strings.Contains(response.Body.String(), "function_call") {
		t.Fatal("blocked tool leaked to client")
	}
	if attempts.Load() != 1 {
		t.Fatalf("upstream attempts = %d", attempts.Load())
	}
	record := records.latest(t)
	if record.Status != contract.RequestStatusBlocked || record.ToolReview == nil || record.ToolReview.Status != "blocked" {
		t.Fatalf("record = %#v", record)
	}
}

func TestToolReviewMarkDoesNotWaitForJudgeAndPreservesSuccessfulRecord(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	handler, records := reviewTestHandler(t, contract.ToolReviewModeMark, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		sendReviewChoice(w, "flag")
	}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reviewDangerousResponse)
	}, false)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"Inspect only"}`)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("mark mode waited for judge")
	}
	if response.Body.String() != reviewDangerousResponse || response.Code != http.StatusOK {
		t.Fatalf("response changed: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("review did not start")
	}
	pending := records.latest(t).ToolReview
	if pending == nil || pending.Status != "pending" || pending.RejectThreshold == nil || *pending.RejectThreshold != 0.5 || len(pending.Calls) != 1 || pending.Calls[0].Name != "bash" || pending.Calls[0].Action != "delivered" || pending.CompletedAt != nil {
		t.Fatalf("admitted mark job missing safe metadata: %+v", pending)
	}
	encoded, err := json.Marshal(pending)
	if err != nil || strings.Contains(string(encoded), "remove-important-data") {
		t.Fatalf("pending arguments leaked: %s %v", encoded, err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case record := <-records.updated:
		if record.Status != contract.RequestStatusSucceeded || record.ToolReview == nil || record.ToolReview.Status != "flagged" {
			t.Fatalf("completed record = %#v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("review record not completed")
	}
}
