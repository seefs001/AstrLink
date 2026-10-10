package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/console"
	"github.com/QuantumNous/astrlink/core/internal/controlapi"
)

func TestServeToolReviewAskUsesSharedPersistentBroker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("server edition test uses control socket")
	}
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"model":"jev-test","answers":{"review":{"type":"choice","choice":"flag","probabilities":{"pass":0.01,"flag":0.99},"confidence":0.99}}}`)
	}))
	defer judge.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"resp_ask","status":"completed","output":[{"type":"function_call","id":"fc_ask","call_id":"call_ask","name":"shell_demo","arguments":"{\"command\":\"private-pending-command\"}"}]}`)
	}))
	defer provider.Close()
	running := startServe(t, serveConfig{dataDirectory: shortDataDir(t), listen: "127.0.0.1:0", outboundProxy: "environment"})
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	send := func(r clientRequest, status int) *http.Response {
		t.Helper()
		response := r.send(t, client, running.address)
		if response.StatusCode != status {
			data, _ := io.ReadAll(response.Body)
			t.Fatalf("%s %s=%d %s", r.method, r.path, response.StatusCode, data)
		}
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		response.Body = io.NopCloser(bytes.NewReader(data))
		return response
	}
	session := sessionCookieOf(t, send(clientRequest{method: "POST", path: console.SetupPath, body: `{"password":"` + servePassword + `"}`, header: pageHeader}, 200))
	current := send(clientRequest{method: "GET", path: controlapi.ToolReviewSettingsPath, cookie: session}, 200)
	_ = current.Body.Close()
	send(clientRequest{method: "PATCH", path: controlapi.ToolReviewSettingsPath, cookie: session, body: fmt.Sprintf(`{"enabled":true,"mode":"ask","endpoint":%q,"api_key":"fake-ask-review-key","data_transfer_acknowledged":true}`, judge.URL), header: map[string]string{console.RequestHeader: console.RequestHeaderValue, "Content-Type": "application/merge-patch+json", "If-Match": current.Header.Get("ETag")}}, 200)
	send(clientRequest{method: "POST", path: controlapi.ServicesPath, cookie: session, header: pageHeader, body: fmt.Sprintf(`{"name":"ask provider","kind":"openai","models":["ask-model"],"http":{"base_url":%q,"auth":{"scheme":"none"}},"capabilities":[{"protocol":"openai.responses","mode":"native","streaming":true}]}`, provider.URL)}, 201)
	var token struct {
		AccessToken string `json:"access_token"`
	}
	decodeBody(t, send(clientRequest{method: "POST", path: controlapi.AccessTokensPath, cookie: session, header: pageHeader, body: `{"name":"ask client"}`}, 201), &token)
	type result struct {
		status int
		body   string
		err    error
	}
	startInference := func() <-chan result {
		done := make(chan result, 1)
		go func() {
			r, err := http.NewRequest("POST", "http://"+running.address+"/v1/responses", strings.NewReader(`{"model":"ask-model","input":"private approval user"}`))
			if err != nil {
				done <- result{err: err}
				return
			}
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token.AccessToken)
			response, err := client.Do(r)
			if err != nil {
				done <- result{err: err}
				return
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			done <- result{status: response.StatusCode, body: string(data), err: err}
		}()
		return done
	}
	waitPending := func(done <-chan result) contract.ToolReviewApprovalRequest {
		t.Helper()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		for {
			var page struct {
				Items []contract.ToolReviewApprovalRequest `json:"items"`
			}
			decodeBody(t, send(clientRequest{method: "GET", path: controlapi.ToolReviewApprovalsPath, cookie: session}, 200), &page)
			if len(page.Items) > 0 {
				return page.Items[0]
			}
			select {
			case r := <-done:
				t.Fatalf("inference ended early: %+v", r)
			case <-timer.C:
				t.Fatal("shared broker never published")
			default:
				runtime.Gosched()
			}
		}
	}
	done := startInference()
	item := waitPending(done)
	raw, _ := json.Marshal(item)
	if strings.Contains(string(raw), "private approval") || strings.Contains(string(raw), "private-pending-command") {
		t.Fatal("queue exposed raw input")
	}
	send(clientRequest{method: "GET", path: controlapi.ToolReviewApprovalsPath + "/" + item.ID, cookie: session}, 423)
	var records contract.RequestRecordPage
	decodeBody(t, send(clientRequest{method: "GET", path: controlapi.RequestsPath + "?tool_review_status=pending", cookie: session}, 200), &records)
	if len(records.Items) != 1 || records.Items[0].ToolReview.Approval.State != "pending" {
		t.Fatalf("pending record not durable: %+v", records.Items)
	}
	send(clientRequest{method: "POST", path: controlapi.ToolReviewApprovalsPath + "/" + item.ID + "/decision", cookie: session, header: pageHeader, body: `{"decision":"allow"}`}, 200)
	select {
	case r := <-done:
		if r.err != nil || r.status != 200 || !strings.Contains(r.body, "private-pending-command") {
			t.Fatalf("approved response: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approved inference stuck")
	}
	decodeBody(t, send(clientRequest{method: "GET", path: controlapi.RequestsPath + "?tool_review_status=flagged", cookie: session}, 200), &records)
	if len(records.Items) != 1 || records.Items[0].Status != contract.RequestStatusSucceeded || records.Items[0].ToolReview.Approval.State != "approved" || records.Items[0].ToolReview.Calls[0].Action != "delivered" {
		t.Fatalf("approved record=%+v", records.Items)
	}
	done = startInference()
	waitPending(done)
	running.cancel()
	select {
	case r := <-done:
		if r.err == nil && strings.Contains(r.body, "private-pending-command") {
			t.Fatal("shutdown released pending tools")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("core shutdown left approval blocked")
	}
	client.CloseIdleConnections()
	select {
	case err := <-running.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("core shutdown stuck")
	}
}
