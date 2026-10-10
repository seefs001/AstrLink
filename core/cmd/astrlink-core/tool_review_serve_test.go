package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/console"
	"github.com/QuantumNous/astrlink/core/internal/controlapi"
)

// Exercise the shared persistent bootstrap, not a separately assembled handler:
// moving desktop startup into persistent.go must retain both review dependencies.
func TestServeToolReviewSettingsReachThePersistentGateway(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the server edition test drives the control socket")
	}
	var reviews atomic.Int32
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reviews.Add(1)
		if r.Header.Get("Authorization") != "Bearer fake-merge-review-key" {
			t.Error("persisted review credential did not reach the judge")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"jev-merge-test","answers":{"review":{"type":"choice","choice":"flag","probabilities":{"pass":0.01,"flag":0.99},"confidence":0.99}}}`)
	}))
	defer judge.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_merge","status":"completed","output":[{"type":"function_call","id":"fc_merge","call_id":"call_merge","name":"shell_demo","arguments":"{\"command\":\"withhold-this-command\"}"}]}`)
	}))
	defer provider.Close()

	running := startServe(t, serveConfig{dataDirectory: shortDataDir(t), listen: "127.0.0.1:0", outboundProxy: "environment"})
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	send := func(request clientRequest, status int) *http.Response {
		t.Helper()
		response := request.send(t, client, running.address)
		if response.StatusCode != status {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("%s %s: %d %s", request.method, request.path, response.StatusCode, body)
		}
		return response
	}
	setup := send(clientRequest{method: http.MethodPost, path: console.SetupPath, body: `{"password":"` + servePassword + `"}`, header: pageHeader}, http.StatusOK)
	session := sessionCookieOf(t, setup)
	current := send(clientRequest{method: http.MethodGet, path: controlapi.ToolReviewSettingsPath, cookie: session}, http.StatusOK)
	var defaults contract.ToolReviewSettings
	decodeBody(t, current, &defaults)
	if defaults.Enabled || current.Header.Get("ETag") == "" {
		t.Fatal("review defaults or settings ETag were lost in the shared bootstrap")
	}
	send(clientRequest{
		method: http.MethodPatch, path: controlapi.ToolReviewSettingsPath, cookie: session,
		body: fmt.Sprintf(`{"enabled":true,"mode":"reject","endpoint":%q,"api_key":"fake-merge-review-key","data_transfer_acknowledged":true}`, judge.URL),
		header: map[string]string{
			console.RequestHeader: console.RequestHeaderValue,
			"Content-Type":        "application/merge-patch+json",
			"If-Match":            current.Header.Get("ETag"),
		},
	}, http.StatusOK)
	send(clientRequest{
		method: http.MethodPost, path: controlapi.ServicesPath, cookie: session, header: pageHeader,
		body: fmt.Sprintf(`{"name":"merge test provider","kind":"openai","models":["merge-test"],"http":{"base_url":%q,"auth":{"scheme":"none"}},"capabilities":[{"protocol":"openai.responses","mode":"native","streaming":true}]}`, provider.URL),
	}, http.StatusCreated)
	created := send(clientRequest{method: http.MethodPost, path: controlapi.AccessTokensPath, cookie: session, header: pageHeader, body: `{"name":"merge test client"}`}, http.StatusCreated)
	var token struct {
		AccessToken string `json:"access_token"`
	}
	decodeBody(t, created, &token)
	blocked := send(clientRequest{
		method: http.MethodPost, path: "/v1/responses", body: `{"model":"merge-test","input":"Inspect only"}`,
		header: map[string]string{"Authorization": "Bearer " + token.AccessToken},
	}, http.StatusForbidden)
	body, err := io.ReadAll(blocked.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "tool_review_blocked") || strings.Contains(string(body), "withhold-this-command") || reviews.Load() != 1 {
		t.Fatalf("persisted review settings were not enforced: %s", body)
	}
	var records contract.RequestRecordPage
	decodeBody(t, send(clientRequest{method: http.MethodGet, path: controlapi.RequestsPath + "?tool_review_status=blocked", cookie: session}, http.StatusOK), &records)
	if len(records.Items) != 1 || records.Items[0].Status != contract.RequestStatusBlocked || records.Items[0].ToolReview == nil || records.Items[0].ToolReview.Status != "blocked" {
		t.Fatalf("shared bootstrap lost review records: %#v", records.Items)
	}
	running.cancel()
	if err := <-running.done; err != nil {
		t.Fatalf("serve after cancel: %v", err)
	}
}
