package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolReviewBackendDefaultsAndHistoricalSummary(t *testing.T) {
	settings := DefaultToolReviewSettings()
	if settings.Backend != ToolReviewBackendJev {
		t.Fatal("new settings must default to Jev")
	}
	for _, backend := range []ToolReviewBackend{ToolReviewBackendJev, ToolReviewBackendResponses} {
		settings.Backend = backend
		if err := settings.Validate(); err != nil {
			t.Fatalf("backend %s: %v", backend, err)
		}
	}
	for _, backend := range []ToolReviewBackend{"", "Jev", "openai", "responses ", "unknown"} {
		settings.Backend = backend
		if err := settings.Validate(); err == nil {
			t.Fatalf("invalid backend %q accepted", backend)
		}
	}
	var summary ToolReviewSummary
	if err := json.Unmarshal([]byte(`{"status":"passed","mode":"mark","model":"old-model","started_at":"2026-01-01T00:00:00Z","calls":[]}`), &summary); err != nil {
		t.Fatal(err)
	}
	if err := summary.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(summary)
	if err != nil || strings.Contains(string(data), "backend") {
		t.Fatalf("historical backend fabricated: %s %v", data, err)
	}
	for _, backend := range []ToolReviewBackend{ToolReviewBackendJev, ToolReviewBackendResponses} {
		summary.Backend = backend
		if err := summary.Validate(); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(summary)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ToolReviewSummary
		if err := json.Unmarshal(data, &decoded); err != nil || decoded.Backend != backend {
			t.Fatalf("backend snapshot lost: %s %v", data, err)
		}
	}
	summary.Backend = "unknown"
	if err := summary.Validate(); err == nil {
		t.Fatal("unknown summary backend accepted")
	}
}
