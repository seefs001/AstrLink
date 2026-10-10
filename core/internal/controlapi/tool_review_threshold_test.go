package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
)

func TestToolReviewControlThresholdValidationPersistenceAndObserver(t *testing.T) {
	store, handler := newToolReviewHandler(t)
	get := controlRequest(t, handler, http.MethodGet, ToolReviewSettingsPath, "", "", "")
	etag := get.Header().Get("ETag")
	if get.Code != http.StatusOK || etag == "" {
		t.Fatalf("GET: %d %s", get.Code, get.Body.String())
	}
	for _, tc := range []struct {
		value string
		code  int
		want  float64
	}{
		{"0", 200, 0}, {"1", 200, 1}, {"0.73", 200, 0.73},
		{"-0.01", 422, 0}, {"1.01", 422, 0}, {"null", 422, 0},
		{`"0.5"`, 422, 0}, {`"NaN"`, 422, 0}, {"NaN", 400, 0},
		{"1e999", 422, 0}, {"true", 422, 0}, {"[]", 422, 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			before, err := store.GetToolReviewSettings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			response := controlRequest(t, handler, http.MethodPatch, ToolReviewSettingsPath, "application/merge-patch+json", `{"reject_threshold":`+tc.value+`}`, etag)
			if response.Code != tc.code {
				t.Fatalf("PATCH: %d %s", response.Code, response.Body.String())
			}
			after, err := store.GetToolReviewSettings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tc.code != http.StatusOK {
				if after != before {
					t.Fatal("invalid threshold changed persisted settings")
				}
				return
			}
			var settings contract.ToolReviewSettings
			if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
				t.Fatal(err)
			}
			if settings.RejectThreshold != tc.want || after.Settings != settings || after.ETag == etag || after.ETag != response.Header().Get("ETag") {
				t.Fatalf("threshold not persisted: %+v", after)
			}
			etag = after.ETag
			for _, caller := range roleCallers {
				req := httptest.NewRequest(http.MethodGet, ToolReviewSettingsPath, nil)
				caller.set(req)
				read := httptest.NewRecorder()
				handler.ServeHTTP(read, req)
				var body map[string]json.RawMessage
				if err := json.Unmarshal(read.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				var threshold float64
				if err := json.Unmarshal(body["reject_threshold"], &threshold); err != nil || read.Code != http.StatusOK || threshold != tc.want {
					t.Fatalf("%s threshold: %s %v", caller.name, read.Body.String(), err)
				}
				if caller.role == RoleObserver && (body["endpoint"] != nil || body["prompt"] != nil || body["api_key"] != nil || read.Header().Get("ETag") != "") {
					t.Fatalf("observer leaked sensitive configuration: %s", read.Body.String())
				}
			}
		})
	}
}
