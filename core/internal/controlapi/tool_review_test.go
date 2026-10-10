package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
)

func newToolReviewHandler(t *testing.T) (*sqlite.Store, *Handler) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewWithDependencies(contract.DefaultVersionResponse("test", "test"), Dependencies{ServiceStore: store, RequestRecords: store, ToolReviewSettings: store, ControlToken: testControlToken, ObserverToken: testObserverToken})
	if err != nil {
		t.Fatal(err)
	}
	return store, handler
}

func TestToolReviewControlSettingsRolesAcknowledgementAndETag(t *testing.T) {
	_, handler := newToolReviewHandler(t)
	get := controlRequest(t, handler, http.MethodGet, ToolReviewSettingsPath, "", "", "")
	if get.Code != http.StatusOK || get.Header().Get("ETag") == "" {
		t.Fatalf("get: %d %s", get.Code, get.Body.String())
	}
	for _, caller := range roleCallers {
		req := httptest.NewRequest(http.MethodGet, ToolReviewSettingsPath, nil)
		caller.set(req)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK {
			t.Fatalf("%s read: %d", caller.name, response.Code)
		}
		if caller.role == RoleObserver && (body["prompt"] != nil || body["endpoint"] != nil || response.Header().Get("ETag") != "") {
			t.Fatalf("observer leaked configuration: %s", response.Body.String())
		}
		if caller.role == RoleOperator && (body["prompt"] == nil || body["endpoint"] == nil) {
			t.Fatal("operator lost configuration")
		}
		if caller.role == RoleObserver {
			req = httptest.NewRequest(http.MethodPatch, ToolReviewSettingsPath, strings.NewReader(`{"enabled":true}`))
			caller.set(req)
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusForbidden {
				t.Fatalf("observer write: %d", response.Code)
			}
		}
	}
	etag := get.Header().Get("ETag")
	for _, test := range []struct {
		body, etag string
		status     int
	}{
		{`{"enabled":true,"api_key":"review-test-key"}`, "", http.StatusPreconditionRequired},
		{`{"enabled":true,"api_key":"review-test-key"}`, etag, http.StatusUnprocessableEntity},
		{`{"enabled":true,"data_transfer_acknowledged":true}`, etag, http.StatusUnprocessableEntity},
		{`{"api_key":null}`, etag, http.StatusUnprocessableEntity},
		{`{"prompt":null}`, etag, http.StatusUnprocessableEntity},
		{`{"threshold":0.9}`, etag, http.StatusUnprocessableEntity},
		{`{"revision":"forged"}`, etag, http.StatusUnprocessableEntity},
		{`{}`, etag, http.StatusUnprocessableEntity},
	} {
		response := controlRequest(t, handler, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", test.body, test.etag)
		if response.Code != test.status {
			t.Fatalf("patch %s: %d %s", test.body, response.Code, response.Body.String())
		}
	}
	patch := `{"enabled":true,"api_key":"review-test-key","data_transfer_acknowledged":true}`
	updated := controlRequest(t, handler, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", patch, etag)
	if updated.Code != http.StatusOK || updated.Header().Get("ETag") == etag || strings.Contains(updated.Body.String(), "review-test-key") || strings.Contains(updated.Body.String(), "data_transfer_acknowledged") {
		t.Fatalf("enable: %d %s", updated.Code, updated.Body.String())
	}
	stale := controlRequest(t, handler, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", patch, etag)
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale: %d", stale.Code)
	}
	changedEndpoint := controlRequest(t, handler, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", `{"endpoint":"https://review.example/v1"}`, updated.Header().Get("ETag"))
	if changedEndpoint.Code != http.StatusUnprocessableEntity {
		t.Fatalf("endpoint acknowledgement: %d", changedEndpoint.Code)
	}
}

func TestToolReviewControlTimeoutRange(t *testing.T) {
	_, handler := newToolReviewHandler(t)
	get := controlRequest(t, handler, http.MethodGet, ToolReviewSettingsPath, "", "", "")
	etag := get.Header().Get("ETag")
	for _, test := range []struct {
		body    string
		status  int
		timeout int
	}{
		{`{"timeout_ms":1}`, 200, 1},
		{`{"timeout_ms":5001}`, 200, 5001},
		{`{"timeout_ms":120000}`, 200, 120000},
		{`{"timeout_ms":120001}`, 422, 0},
		{`{"timeout_ms":0}`, 422, 0},
	} {
		response := controlRequest(t, handler, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", test.body, etag)
		if response.Code != test.status {
			t.Fatalf("%s: %d %s", test.body, response.Code, response.Body.String())
		}
		if response.Code == http.StatusOK {
			var settings contract.ToolReviewSettings
			if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
				t.Fatal(err)
			}
			if settings.TimeoutMS != test.timeout {
				t.Fatalf("timeout=%d", settings.TimeoutMS)
			}
			etag = response.Header().Get("ETag")
		}
	}
}

func TestToolReviewControlRecordFilter(t *testing.T) {
	store, handler := newToolReviewHandler(t)
	start := time.Now().UTC()
	summary := &contract.ToolReviewSummary{Status: "flagged", Mode: contract.ToolReviewModeMark, Model: "jev-latest", StartedAt: start, Calls: []contract.ToolReviewCall{}}
	record := contract.RequestRecord{ID: "request_review", StartedAt: start, Status: contract.RequestStatusSucceeded, InputProtocol: contract.ProtocolOpenAIResponses, Audit: contract.NotCapturedAuditSummary(), ToolReview: summary}
	if err := store.InsertRequestRecord(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		query         string
		status, count int
	}{
		{"flagged", 200, 1}, {"passed", 200, 0}, {"invalid", 400, 0}, {"", 400, 0}, {"flagged&tool_review_status=passed", 400, 0},
	} {
		response := controlRequest(t, handler, http.MethodGet, RequestsPath+"?tool_review_status="+test.query, "", "", "")
		if response.Code != test.status {
			t.Fatalf("filter %s: %d %s", test.query, response.Code, response.Body.String())
		}
		if test.status == 200 {
			var page contract.RequestRecordPage
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != test.count {
				t.Fatalf("filter count %s: %d", test.query, len(page.Items))
			}
		}
	}
}
