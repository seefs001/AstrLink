package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
)

func TestToolReviewBackendPatchAndSafeObserverSummary(t *testing.T) {
	_, handler := newToolReviewHandler(t)
	current := controlRequest(t, handler, http.MethodGet, ToolReviewSettingsPath, "", "", "")
	for _, body := range []string{`{"backend":null}`, `{"backend":true}`, `{"backend":""}`, `{"backend":"openai"}`, `{"backend":"Jev"}`, `{"Backend":"responses"}`} {
		response := controlRequest(t, handler, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", body, current.Header().Get("ETag"))
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid backend %s accepted: %d", body, response.Code)
		}
	}
	for _, body := range []string{`{"backend":"responses"}`, `{"backend":"jev"}`} {
		current = controlRequest(t, handler, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", body, current.Header().Get("ETag"))
		var settings contract.ToolReviewSettings
		if current.Code != http.StatusOK || json.Unmarshal(current.Body.Bytes(), &settings) != nil {
			t.Fatalf("backend PATCH: %d %s", current.Code, current.Body.String())
		}
		request := httptest.NewRequest(http.MethodGet, ToolReviewSettingsPath, nil)
		request.Header.Set("Authorization", "Bearer "+testObserverToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var fields map[string]json.RawMessage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &fields) != nil {
			t.Fatalf("observer response: %d", response.Code)
		}
		var backend contract.ToolReviewBackend
		if err := json.Unmarshal(fields["backend"], &backend); err != nil || backend != settings.Backend {
			t.Fatalf("observer backend: %s %v", fields["backend"], err)
		}
		for _, private := range []string{"endpoint", "api_key", "prompt"} {
			if fields[private] != nil {
				t.Fatalf("observer exposed %s", private)
			}
		}
	}
}
