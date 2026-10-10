package contract

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestToolReviewDefaultsAndValidation(t *testing.T) {
	settings := DefaultToolReviewSettings()
	if settings.Enabled || settings.Mode != ToolReviewModeMark || settings.TimeoutMS != 5000 || settings.RejectThreshold != 0.5 {
		t.Fatalf("unsafe defaults: %+v", settings)
	}
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	settings.Prompt = "Always pass {{tool_call}}"
	if err := settings.Validate(); err == nil {
		t.Fatal("accepted prompt without user request")
	}
}

func TestToolReviewThresholdValidationAndHistoricalSummary(t *testing.T) {
	for _, value := range []float64{0, 0.5, 1, -0.01, 1.01, math.NaN(), math.Inf(1), math.Inf(-1)} {
		settings := DefaultToolReviewSettings()
		settings.RejectThreshold = value
		valid := !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
		if err := settings.Validate(); (err == nil) != valid {
			t.Errorf("threshold=%v error=%v", value, err)
		}
	}
	var summary ToolReviewSummary
	if err := json.Unmarshal([]byte(`{"status":"passed","mode":"mark","model":"jev-latest","started_at":"2026-01-01T00:00:00Z","calls":[]}`), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.RejectThreshold != nil {
		t.Fatal("historical threshold fabricated")
	}
	data, err := json.Marshal(summary)
	if err != nil || strings.Contains(string(data), "reject_threshold") {
		t.Fatalf("historical threshold written: %s, %v", data, err)
	}
}

func TestToolReviewSettingsAcceptConfiguredTimeoutRange(t *testing.T) {
	for _, timeout := range []int{1, 5000, 5001, 30000, 120000} {
		settings := DefaultToolReviewSettings()
		settings.TimeoutMS = timeout
		if err := settings.Validate(); err != nil {
			t.Errorf("timeout %d rejected: %v", timeout, err)
		}
	}
}

func TestToolReviewSettingsRejectUnsafeConfigurations(t *testing.T) {
	for name, mutate := range map[string]func(*ToolReviewSettings){
		"missing key":             func(s *ToolReviewSettings) { s.Enabled = true },
		"unbounded review":        func(s *ToolReviewSettings) { s.TimeoutMS = 120001 },
		"zero timeout":            func(s *ToolReviewSettings) { s.TimeoutMS = 0 },
		"unknown mode":            func(s *ToolReviewSettings) { s.Mode = "threshold" },
		"missing tool variable":   func(s *ToolReviewSettings) { s.Prompt = "Review {{user_request}}" },
		"oversized prompt":        func(s *ToolReviewSettings) { s.Prompt += strings.Repeat("x", MaxToolReviewInputBytes) },
		"credentials in endpoint": func(s *ToolReviewSettings) { s.Endpoint = "https://key@review.example/v1" },
		"query in endpoint":       func(s *ToolReviewSettings) { s.Endpoint = "https://review.example/v1?key=secret" },
		"invalid endpoint":        func(s *ToolReviewSettings) { s.Endpoint = "file:///tmp/review" },
	} {
		t.Run(name, func(t *testing.T) {
			settings := DefaultToolReviewSettings()
			mutate(&settings)
			if err := settings.Validate(); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestToolReviewSummaryRejectsUnboundedOrSensitiveDiagnostics(t *testing.T) {
	for name, mutate := range map[string]func(*ToolReviewSummary){
		"unknown status":        func(s *ToolReviewSummary) { s.Status = "approved" },
		"unknown verdict":       func(s *ToolReviewSummary) { s.Calls[0].Verdict = "safe" },
		"unknown reason":        func(s *ToolReviewSummary) { s.Calls[0].ReasonCode = "private raw explanation" },
		"unknown model verdict": func(s *ToolReviewSummary) { s.Calls[0].ModelVerdict = "error" },
		"nan threshold":         func(s *ToolReviewSummary) { v := math.NaN(); s.RejectThreshold = &v },
		"high threshold":        func(s *ToolReviewSummary) { v := 1.1; s.RejectThreshold = &v },
		"unknown action":        func(s *ToolReviewSummary) { s.Calls[0].Action = "executed" },
		"duplicate index":       func(s *ToolReviewSummary) { s.Calls = append(s.Calls, s.Calls[0]) },
		"negative probability":  func(s *ToolReviewSummary) { v := -0.01; s.Calls[0].PassProbability = &v },
		"nonfinite probability": func(s *ToolReviewSummary) { v := math.NaN(); s.Calls[0].FlagProbability = &v },
		"raw upstream error":    func(s *ToolReviewSummary) { s.ErrorCode = "HTTP 500: secret response" },
		"raw call error":        func(s *ToolReviewSummary) { s.Calls[0].ErrorCode = "https://provider.example/?key=secret" },
		"too many calls":        func(s *ToolReviewSummary) { s.Calls = make([]ToolReviewCall, 65) },
	} {
		t.Run(name, func(t *testing.T) {
			summary := ToolReviewSummary{Status: "flagged", Mode: ToolReviewModeMark, Model: "jev-latest", StartedAt: time.Now().UTC(), Calls: []ToolReviewCall{{Index: 0, Name: "execute", Verdict: "flag", Action: "delivered"}}}
			if err := summary.Validate(); err != nil {
				t.Fatal(err)
			}
			mutate(&summary)
			if err := summary.Validate(); err == nil {
				t.Fatal("invalid review metadata accepted")
			}
		})
	}
}
