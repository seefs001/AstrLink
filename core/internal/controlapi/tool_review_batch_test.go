package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
)

func TestToolReviewBatchPatchAndObserverSummary(t *testing.T) {
	_, handler := newToolReviewHandler(t)
	current := controlRequest(t, handler, http.MethodGet, ToolReviewSettingsPath, "", "", "")
	for _, body := range []string{`{"batch_calls":null}`, `{"batch_calls":1}`, `{"batch_calls":"true"}`, `{"batch_calls":{}}`} {
		w := controlRequest(t, handler, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", body, current.Header().Get("ETag"))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid batch setting accepted: %s -> %d", body, w.Code)
		}
	}
	for _, body := range []string{`{"batch_calls":true}`, `{"batch_calls":false}`, `{"batch_calls":true}`} {
		current = controlRequest(t, handler, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", body, current.Header().Get("ETag"))
		var settings contract.ToolReviewSettings
		if current.Code != http.StatusOK || json.Unmarshal(current.Body.Bytes(), &settings) != nil {
			t.Fatalf("batch PATCH=%d %s", current.Code, current.Body.String())
		}
		request := httptest.NewRequest(http.MethodGet, ToolReviewSettingsPath, nil)
		request.Header.Set("Authorization", "Bearer "+testObserverToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var fields map[string]json.RawMessage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &fields) != nil {
			t.Fatalf("observer summary=%d %s", response.Code, response.Body.String())
		}
		var batch bool
		if err := json.Unmarshal(fields["batch_calls"], &batch); err != nil || batch != settings.BatchCalls {
			t.Fatalf("observer batch_calls=%s error=%v", fields["batch_calls"], err)
		}
		for _, secret := range []string{"api_key", "endpoint", "prompt"} {
			if _, exists := fields[secret]; exists {
				t.Fatalf("observer exposed %s", secret)
			}
		}
	}
}
