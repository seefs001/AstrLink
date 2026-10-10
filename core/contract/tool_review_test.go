package contract

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestToolReviewDefaultsAndValidation(t *testing.T) {
	settings := DefaultToolReviewSettings()
	if settings.Enabled || settings.Mode != ToolReviewModeMark || settings.TimeoutMS != 5000 {
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
