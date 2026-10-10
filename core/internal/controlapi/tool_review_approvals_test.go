package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func TestToolReviewApprovalAPIRolesRawLockAndStrictDecision(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	broker := toolreview.NewBroker(ctx)
	done := make(chan error, 1)
	summary := contract.ToolReviewSummary{Status: "flagged", Mode: contract.ToolReviewModeAsk, Model: "jev", StartedAt: time.Now(), Calls: []contract.ToolReviewCall{{Index: 0, Name: "bash", Verdict: "flag", Action: "withheld"}}}
	go func() {
		_, err := broker.Wait(ctx, "request_approval", summary, toolreview.Input{UserRequest: "private approval user", Calls: []toolreview.Call{{Index: 0, Name: "bash", Arguments: `{"command":"private approval args"}`}}}, time.Minute, func(contract.ToolReviewApproval) error { return nil })
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		broker.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("waiter did not stop")
		}
	})
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
		case err := <-done:
			t.Fatalf("wait failed before publication: %v", err)
		case <-timer.C:
			t.Fatal("not published")
		default:
			runtime.Gosched()
		}
	}
	store, _ := newToolReviewHandler(t)
	vault := &fakeRawVault{}
	newHandler := func(v RawVault) *Handler {
		h, err := NewWithDependencies(contract.DefaultVersionResponse("test", "test"), Dependencies{ServiceStore: store, ToolReviewApprovals: broker, RawVault: v, ControlToken: testControlToken, ObserverToken: testObserverToken})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	handler := newHandler(vault)
	call := func(h *Handler, method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	detailPath := ToolReviewApprovalsPath + "/" + item.ID
	decisionPath := detailPath + "/decision"
	for _, tc := range []struct{ method, path, body string }{{"GET", ToolReviewApprovalsPath, ""}, {"GET", detailPath, ""}, {"POST", decisionPath, `{"decision":"allow"}`}} {
		for _, token := range []string{testObserverToken, "inference-only-token", ""} {
			w := call(handler, tc.method, tc.path, tc.body, token)
			if w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
				t.Fatalf("unauthorized %s %s=%d", tc.method, tc.path, w.Code)
			}
			if strings.Contains(w.Body.String(), "private approval") {
				t.Fatal("unauthorized raw leakage")
			}
		}
	}
	w := call(handler, "GET", ToolReviewApprovalsPath, "", testControlToken)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private approval") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list=%d %s", w.Code, w.Body.String())
	}
	for _, h := range []*Handler{handler, newHandler(nil)} {
		w = call(h, "GET", detailPath, "", testControlToken)
		if w.Code != 423 || !strings.Contains(w.Body.String(), "raw_locked") {
			t.Fatalf("locked=%d %s", w.Code, w.Body.String())
		}
	}
	vault.unlocked = true
	w = call(handler, "GET", detailPath, "", testControlToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "private approval user") || !strings.Contains(w.Body.String(), "private approval args") {
		t.Fatalf("unlocked detail=%d %s", w.Code, w.Body.String())
	}
	if vault.proofs != 0 || vault.opened != 0 || handler.rawGrants.pendingCount() != 0 || handler.rawGrants.activeCount() != 0 {
		t.Fatal("detail issued raw grant or proof")
	}
	for _, body := range []string{`{"decision":"other"}`, `{"decision":"allow","extra":true}`, `{"decision":"deny","decision":"allow"}`, `{"Decision":"allow"}`, `{"decision":null}`, `{"decision":"allow"} {}`} {
		w = call(handler, "POST", decisionPath, body, testControlToken)
		if w.Code < 400 {
			t.Fatalf("invalid decision accepted: %s", body)
		}
	}
	w = call(handler, "POST", decisionPath, `{"decision":"allow"}`, testControlToken)
	var approval contract.ToolReviewApproval
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &approval) != nil || approval.State != "approved" {
		t.Fatalf("allow=%d %s", w.Code, w.Body.String())
	}
	w = call(handler, "POST", decisionPath, `{"decision":"allow"}`, testControlToken)
	if w.Code != 409 && w.Code != 410 {
		t.Fatalf("replay=%d", w.Code)
	}
	w = call(handler, "GET", detailPath, "", testControlToken)
	if w.Code != 410 || strings.Contains(w.Body.String(), "private approval") {
		t.Fatalf("consumed detail=%d %s", w.Code, w.Body.String())
	}
}

func TestToolReviewAskSettingsPatchAndObserver(t *testing.T) {
	_, h := newToolReviewHandler(t)
	current := controlRequest(t, h, http.MethodGet, ToolReviewSettingsPath, "", "", "")
	for _, body := range []string{`{"ask_on_review_error":null}`, `{"ask_on_review_error":"true"}`, `{"approval_timeout_ms":999}`, `{"approval_timeout_ms":600001}`, `{"approval_timeout_ms":1.5}`, `{"approval_timeout_ms":null}`, `{"unknown":true}`} {
		w := controlRequest(t, h, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", body, current.Header().Get("ETag"))
		if w.Code != 422 {
			t.Fatalf("%s=%d %s", body, w.Code, w.Body.String())
		}
	}
	w := controlRequest(t, h, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", `{"mode":"ask","ask_on_review_error":true,"approval_timeout_ms":600000}`, current.Header().Get("ETag"))
	if w.Code != 200 {
		t.Fatalf("patch=%d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest(http.MethodGet, ToolReviewSettingsPath, nil)
	r.Header.Set("Authorization", "Bearer "+testObserverToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var fields map[string]json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &fields) != nil || string(fields["mode"]) != `"ask"` || string(fields["ask_on_review_error"]) != "true" || string(fields["approval_timeout_ms"]) != "600000" {
		t.Fatalf("observer=%s", w.Body.String())
	}
	for _, field := range []string{"endpoint", "prompt", "api_key"} {
		if _, ok := fields[field]; ok {
			t.Fatalf("observer exposed %s", field)
		}
	}
}
