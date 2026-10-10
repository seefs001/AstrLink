package contracts_test

import "testing"

func TestToolReviewBackendSchemas(t *testing.T) {
	compiler := toolReviewCompiler(t)
	patch := compileToolReviewSchema(t, compiler, "ToolReviewSettingsPatch")
	for _, value := range []string{"jev", "responses"} {
		if err := patch.Validate(map[string]any{"backend": value}); err != nil {
			t.Errorf("backend %s rejected: %v", value, err)
		}
	}
	for _, value := range []any{"", "Jev", "openai", "responses ", nil, true, 1} {
		if err := patch.Validate(map[string]any{"backend": value}); err == nil {
			t.Errorf("invalid backend %#v accepted", value)
		}
	}
	summary := compileToolReviewSchema(t, compiler, "ToolReviewSummary")
	instance := map[string]any{
		"status": "flagged", "mode": "ask", "model": "review-model",
		"config_revision": "test", "started_at": "2026-01-01T00:00:00Z",
		"latency_ms": 0, "calls": []any{},
	}
	if err := summary.Validate(instance); err != nil {
		t.Fatalf("historical backend rejected: %v", err)
	}
	for _, value := range []string{"jev", "responses"} {
		instance["backend"] = value
		if err := summary.Validate(instance); err != nil {
			t.Errorf("backend summary %s: %v", value, err)
		}
	}
	instance["backend"] = nil
	if err := summary.Validate(instance); err == nil {
		t.Fatal("null backend accepted")
	}
}
