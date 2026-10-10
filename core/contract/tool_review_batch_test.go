package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolReviewBatchDefaultsAndHistoricalMetadata(t *testing.T) {
	if DefaultToolReviewSettings().BatchCalls {
		t.Fatal("batch review must be opt-in")
	}
	var summary ToolReviewSummary
	if err := json.Unmarshal([]byte(`{"status":"passed","mode":"mark","model":"jev-latest","started_at":"2026-01-01T00:00:00Z","calls":[]}`), &summary); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(summary)
	if err != nil || summary.BatchCalls != nil || strings.Contains(string(encoded), "batch_calls") {
		t.Fatalf("fabricated historical batch strategy: %s %v", encoded, err)
	}
	for _, value := range []bool{false, true} {
		summary.BatchCalls = &value
		encoded, err := json.Marshal(summary)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ToolReviewSummary
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.BatchCalls == nil || *decoded.BatchCalls != value {
			t.Fatalf("lost explicit batch strategy: %s %v", encoded, err)
		}
	}
}
