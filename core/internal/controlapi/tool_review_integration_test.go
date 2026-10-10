package controlapi

import (
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
	"github.com/QuantumNous/astrlink/core/internal/ingress"
)

// Only the two external providers are simulated. Configuration, credentials,
// routing, the review client, record persistence and reads use production code.
const integrationToolResponse = `{"id":"resp_integration","status":"completed","output":[{"type":"function_call","id":"fc_integration","call_id":"call_integration","name":"shell_demo","arguments":"{\"command\":\"private-tool-argument-marker\"}"}]}`
const integrationReviewKey = "fake-integration-review-key"

type toolReviewIntegration struct {
	url    string
	client *http.Client
}

type toolReviewHTTPResult struct {
	status int
	header http.Header
	body   string
	err    error
}

func newToolReviewIntegration(t *testing.T, upstream http.HandlerFunc) toolReviewIntegration {
	t.Helper()
	store, control := newToolReviewHandler(t)
	provider := httptest.NewServer(upstream)
	t.Cleanup(provider.Close)
	createServiceForTest(t, control, fmt.Sprintf(`{
		"name":"review integration provider","kind":"openai","models":["integration-model"],
		"http":{"base_url":%q,"auth":{"scheme":"none"}},
		"capabilities":[{"protocol":"openai.responses","mode":"native","streaming":true}]
	}`, provider.URL))
	resolver, err := endpoint.NewStoreResolver(store)
	if err != nil {
		t.Fatal(err)
	}
	gateway := ingress.NewWithDependencies(ingress.Dependencies{
		Resolver: resolver, Authorizer: endpoint.NewServiceAuthorizer(store, nil),
		ToolReviewSettings: store, RequestRecords: store,
	})
	mux := http.NewServeMux()
	mux.Handle("/control/", control)
	mux.Handle("/v1/", gateway)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	return toolReviewIntegration{url: server.URL, client: client}
}

func (h toolReviewIntegration) request(method, path, body, etag string) toolReviewHTTPResult {
	request, err := http.NewRequest(method, h.url+path, strings.NewReader(body))
	if err != nil {
		return toolReviewHTTPResult{err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	if method == http.MethodPatch {
		request.Header.Set("Content-Type", "application/merge-patch+json")
	}
	if strings.HasPrefix(path, "/control/") {
		request.Header.Set("Authorization", "Bearer "+testControlToken)
	}
	if etag != "" {
		request.Header.Set("If-Match", etag)
	}
	response, err := h.client.Do(request)
	if err != nil {
		return toolReviewHTTPResult{err: err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return toolReviewHTTPResult{status: response.StatusCode, header: response.Header, body: string(data), err: err}
}

func (h toolReviewIntegration) mustRequest(t *testing.T, method, path, body, etag string, status int) toolReviewHTTPResult {
	t.Helper()
	result := h.request(method, path, body, etag)
	if result.err != nil || result.status != status {
		t.Fatalf("%s %s: status=%d error=%v body=%s", method, path, result.status, result.err, result.body)
	}
	return result
}

func (h toolReviewIntegration) patch(t *testing.T, body string) contract.ToolReviewSettings {
	t.Helper()
	current := h.mustRequest(t, http.MethodGet, ToolReviewSettingsPath, "", "", http.StatusOK)
	result := h.mustRequest(t, http.MethodPatch, ToolReviewSettingsPath, body, current.header.Get("ETag"), http.StatusOK)
	var settings contract.ToolReviewSettings
	if err := json.Unmarshal([]byte(result.body), &settings); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.body, integrationReviewKey) {
		t.Fatal("settings API exposed the configured credential")
	}
	return settings
}

func (h toolReviewIntegration) records(t *testing.T, status string) []contract.RequestRecord {
	t.Helper()
	path := RequestsPath
	if status != "" {
		path += "?tool_review_status=" + status
	}
	result := h.mustRequest(t, http.MethodGet, path, "", "", http.StatusOK)
	var page contract.RequestRecordPage
	if err := json.Unmarshal([]byte(result.body), &page); err != nil {
		t.Fatal(err)
	}
	for _, record := range page.Items {
		if status != "" && (record.ToolReview == nil || record.ToolReview.Status != status) {
			t.Fatalf("%s filter returned a different review: %#v", status, record.ToolReview)
		}
	}
	return page.Items
}

func integrationSendTool(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, integrationToolResponse)
}

func integrationSendFlag(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"model":"jev-integration","answers":{"review":{"type":"choice","choice":"flag","probabilities":{"pass":0.01,"flag":0.99},"confidence":0.99}}}`)
}

func (h toolReviewIntegration) waitForRecord(t *testing.T, status string) contract.RequestRecord {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if records := h.records(t, status); len(records) == 1 {
			return records[0]
		}
		select {
		case <-deadline.C:
			t.Fatalf("review did not reach %s through the records API", status)
		case <-tick.C:
		}
	}
}

func TestToolReviewIntegrationConfigChangesApplyToNewRequestsOnly(t *testing.T) {
	providerStarted, releaseProvider := make(chan struct{}), make(chan struct{})
	judgeStarted, releaseJudge := make(chan struct{}), make(chan struct{})
	var providerCalls, judgeCalls atomic.Int32
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := judgeCalls.Add(1) == 1
		expectedKey, expectedModel := integrationReviewKey, "jev-latest"
		if !first {
			expectedKey, expectedModel = integrationReviewKey+"-rotated", "jev-updated"
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") != "Bearer "+expectedKey || payload.Model != expectedModel {
			t.Error("in-flight review mixed persisted settings with the new configuration")
		}
		if first {
			close(judgeStarted)
			select {
			case <-releaseJudge:
			case <-r.Context().Done():
				return
			}
		}
		integrationSendFlag(w)
	}))
	defer judge.Close()
	// Release every blocked HTTP handler even when an assertion fails.
	var providerOnce, judgeOnce sync.Once
	defer providerOnce.Do(func() { close(releaseProvider) })
	defer judgeOnce.Do(func() { close(releaseJudge) })
	h := newToolReviewIntegration(t, func(w http.ResponseWriter, r *http.Request) {
		if providerCalls.Add(1) == 1 {
			close(providerStarted)
			select {
			case <-releaseProvider:
			case <-r.Context().Done():
				return
			}
		}
		integrationSendTool(w, r)
	})
	original := h.patch(t, fmt.Sprintf(`{"enabled":true,"mode":"mark","endpoint":%q,"api_key":%q,"data_transfer_acknowledged":true}`, judge.URL, integrationReviewKey))
	const request = `{"model":"integration-model","input":"Inspect only"}`
	delivered := make(chan toolReviewHTTPResult, 1)
	go func() { delivered <- h.request(http.MethodPost, "/v1/responses", request, "") }()
	select {
	case <-providerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach the provider")
	}
	updated := h.patch(t, fmt.Sprintf(`{"mode":"reject","model":"jev-updated","api_key":%q}`, integrationReviewKey+"-rotated"))
	providerOnce.Do(func() { close(releaseProvider) })
	select {
	case response := <-delivered:
		if response.err != nil || response.status != http.StatusOK || response.body != integrationToolResponse {
			t.Fatalf("in-flight mark delivery changed: %#v", response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mark delivery waited for the reviewer after configuration changed")
	}
	select {
	case <-judgeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request lost its review when configuration changed")
	}
	pending := h.waitForRecord(t, "pending")
	if pending.Status != contract.RequestStatusSucceeded || pending.ToolReview.ConfigRevision != original.Revision || pending.ToolReview.Mode != contract.ToolReviewModeMark {
		t.Fatalf("pending metadata changed delivery or configuration: %#v", pending)
	}
	judgeOnce.Do(func() { close(releaseJudge) })
	flagged := h.waitForRecord(t, "flagged")
	if flagged.ID != pending.ID || flagged.Status != contract.RequestStatusSucceeded || flagged.ToolReview.ConfigRevision != original.Revision || flagged.ToolReview.Mode != contract.ToolReviewModeMark || len(flagged.ToolReview.Calls) != 1 || flagged.ToolReview.Calls[0].Action != "delivered" {
		t.Fatalf("async review did not update only the original record metadata: %#v", flagged)
	}
	if len(h.records(t, "pending")) != 0 {
		t.Fatal("terminal review remains in the pending filter")
	}

	h.mustRequest(t, http.MethodPost, "/v1/responses", request, "", http.StatusForbidden)
	blocked := h.waitForRecord(t, "blocked")
	if blocked.Status != contract.RequestStatusBlocked || blocked.ToolReview.ConfigRevision != updated.Revision || blocked.ToolReview.Mode != contract.ToolReviewModeReject || original.Revision == updated.Revision {
		t.Fatalf("new request ignored updated settings: %#v", blocked)
	}
	h.patch(t, `{"enabled":false}`)
	unreviewed := h.mustRequest(t, http.MethodPost, "/v1/responses", request, "", http.StatusOK)
	all := h.records(t, "")
	if unreviewed.body != integrationToolResponse || judgeCalls.Load() != 2 || providerCalls.Load() != 3 || len(all) != 3 {
		t.Fatal("disabled review changed new delivery, called the judge, or lost request history")
	}
	for _, record := range all {
		if record.ID != flagged.ID && record.ID != blocked.ID && (record.Status != contract.RequestStatusSucceeded || record.ToolReview != nil) {
			t.Fatalf("disabled review inherited an earlier request's metadata: %#v", record)
		}
	}
}

func TestToolReviewIntegrationJudgeFailurePreservesModeAndHidesRawError(t *testing.T) {
	for _, tc := range []struct {
		mode   contract.ToolReviewMode
		status int
		state  contract.RequestStatus
		action string
	}{
		{contract.ToolReviewModeMark, http.StatusOK, contract.RequestStatusSucceeded, "delivered"},
		{contract.ToolReviewModeReject, http.StatusServiceUnavailable, contract.RequestStatusFailed, "withheld"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			var reviews atomic.Int32
			const privateError = "private-judge-error-marker"
			judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reviews.Add(1)
				http.Error(w, privateError, http.StatusUnauthorized)
			}))
			defer judge.Close()
			h := newToolReviewIntegration(t, integrationSendTool)
			h.patch(t, fmt.Sprintf(`{"enabled":true,"mode":%q,"endpoint":%q,"api_key":%q,"data_transfer_acknowledged":true}`, tc.mode, judge.URL, integrationReviewKey))
			response := h.mustRequest(t, http.MethodPost, "/v1/responses", `{"model":"integration-model","input":"Inspect only"}`, "", tc.status)
			if tc.mode == contract.ToolReviewModeMark && response.body != integrationToolResponse {
				t.Fatal("review failure changed mark delivery")
			}
			if tc.mode == contract.ToolReviewModeReject && (!strings.Contains(response.body, "tool_review_unavailable") || strings.Contains(response.body, "private-tool-argument-marker")) {
				t.Fatal("review failure did not withhold the tool batch")
			}
			record := h.waitForRecord(t, "failed")
			if record.Status != tc.state || record.ToolReview.Mode != tc.mode || record.ToolReview.ErrorCode != "unauthorized" || len(record.ToolReview.Calls) != 1 || record.ToolReview.Calls[0].Action != tc.action || reviews.Load() != 1 {
				t.Fatalf("failed review changed mode, delivery or retry policy: %#v", record)
			}
			page := h.mustRequest(t, http.MethodGet, RequestsPath, "", "", http.StatusOK)
			if strings.Contains(page.body, privateError) || strings.Contains(response.body, privateError) || strings.Contains(page.body, integrationReviewKey) {
				t.Fatal("upstream error or credential leaked through the gateway or record API")
			}
		})
	}
}

func TestToolReviewIntegrationSavedSettingsGateRequestsAndExposeFilteredRecords(t *testing.T) {
	var reviews atomic.Int32
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reviews.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+integrationReviewKey {
			t.Error("review did not use the persisted credential")
		}
		integrationSendFlag(w)
	}))
	defer judge.Close()
	h := newToolReviewIntegration(t, integrationSendTool)
	const request = `{"model":"integration-model","input":"Only inspect; do not change data"}`

	initial := h.mustRequest(t, http.MethodPost, "/v1/responses", request, "", http.StatusOK)
	if initial.body != integrationToolResponse || reviews.Load() != 0 {
		t.Fatal("default-disabled review changed delivery or contacted the reviewer")
	}
	settings := h.patch(t, fmt.Sprintf(`{"enabled":true,"mode":"reject","endpoint":%q,"api_key":%q,"data_transfer_acknowledged":true}`, judge.URL, integrationReviewKey))
	blocked := h.mustRequest(t, http.MethodPost, "/v1/responses", request, "", http.StatusForbidden)
	if !strings.Contains(blocked.body, "tool_review_blocked") || strings.Contains(blocked.body, "private-tool-argument-marker") {
		t.Fatalf("unsafe blocked response: %s", blocked.body)
	}
	records := h.records(t, "blocked")
	if len(records) != 1 || records[0].Status != contract.RequestStatusBlocked || records[0].ToolReview == nil {
		t.Fatalf("blocked request was not persisted and filtered: %#v", records)
	}
	review := records[0].ToolReview
	if review.ConfigRevision != settings.Revision || len(review.Calls) != 1 || review.Calls[0].Action != "withheld" || review.Calls[0].Verdict != "flag" || review.Calls[0].Model != "jev-integration" {
		t.Fatalf("stored review lost its configuration or verdict: %#v", review)
	}
	metadata, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{integrationReviewKey, "private-tool-argument-marker", "Only inspect; do not change data"} {
		if strings.Contains(string(metadata), private) {
			t.Fatal("review metadata retained private input")
		}
	}
	if len(h.records(t, "passed")) != 0 || len(h.records(t, "")) != 2 || reviews.Load() != 1 {
		t.Fatal("review filters or default-disabled request history are incorrect")
	}
}
