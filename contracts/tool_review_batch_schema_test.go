package contracts_test

import "testing"

func TestToolReviewBatchSchemas(t *testing.T) {
	compiler := toolReviewCompiler(t)
	patch := compileToolReviewSchema(t, compiler, "ToolReviewSettingsPatch")
	for _, value := range []bool{false, true} {
		if err := patch.Validate(map[string]any{"batch_calls": value}); err != nil {
			t.Errorf("batch_calls=%v: %v", value, err)
		}
	}
	for _, value := range []any{nil, "true", 0, 1, []any{}} {
		if err := patch.Validate(map[string]any{"batch_calls": value}); err == nil {
			t.Errorf("invalid batch_calls=%#v accepted", value)
		}
	}
	summary := compileToolReviewSchema(t, compiler, "ToolReviewSummary")
	instance := map[string]any{
		"status": "flagged", "mode": "ask", "model": "jev-latest",
		"config_revision": "test", "started_at": "2026-01-01T00:00:00Z",
		"latency_ms": 0, "calls": []any{},
	}
	if err := summary.Validate(instance); err != nil {
		t.Fatalf("historical record without strategy rejected: %v", err)
	}
	instance["batch_calls"] = true
	if err := summary.Validate(instance); err != nil {
		t.Fatalf("batch summary rejected: %v", err)
	}
	instance["batch_calls"] = nil
	if err := summary.Validate(instance); err == nil {
		t.Fatal("null strategy accepted")
	}
}
