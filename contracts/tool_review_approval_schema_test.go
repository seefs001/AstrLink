package contracts_test

import "testing"

func TestToolReviewApprovalSchemas(t *testing.T) {
	compiler := toolReviewCompiler(t)
	mode := compileToolReviewSchema(t, compiler, "ToolReviewMode")
	if err := mode.Validate("ask"); err != nil {
		t.Fatalf("ask mode rejected: %v", err)
	}
	patch := compileToolReviewSchema(t, compiler, "ToolReviewSettingsPatch")
	for _, ms := range []int{999, 1000, 120000, 600000, 600001} {
		err := patch.Validate(map[string]any{"mode": "ask", "ask_on_review_error": true, "approval_timeout_ms": ms})
		if (err == nil) != (ms >= 1000 && ms <= 600000) {
			t.Errorf("approval timeout=%d error=%v", ms, err)
		}
	}
	for _, value := range []any{nil, "true", 1} {
		if err := patch.Validate(map[string]any{"ask_on_review_error": value}); err == nil {
			t.Errorf("invalid error policy %#v accepted", value)
		}
	}
	approval := compileToolReviewSchema(t, compiler, "ToolReviewApproval")
	instance := map[string]any{
		"id": "approval_test", "state": "pending", "review_status": "flagged",
		"requested_at": "2026-01-01T00:00:00Z", "expires_at": "2026-01-01T00:02:00Z",
	}
	if err := approval.Validate(instance); err != nil {
		t.Fatal(err)
	}
	instance["decided_at"] = "2026-01-01T00:00:01Z"
	if err := approval.Validate(instance); err == nil {
		t.Fatal("pending approval accepted a decision time")
	}
	for _, state := range []string{"approved", "denied", "expired", "cancelled", "interrupted"} {
		instance["state"] = state
		if err := approval.Validate(instance); err != nil {
			t.Errorf("approval state=%s: %v", state, err)
		}
	}
	delete(instance, "decided_at")
	if err := approval.Validate(instance); err == nil {
		t.Fatal("terminal approval accepted without decision time")
	}
	instance["state"] = "pending"
	instance["arguments"] = "sensitive command"
	if err := approval.Validate(instance); err == nil {
		t.Fatal("approval metadata accepted raw arguments")
	}
	delete(instance, "arguments")
	instance["review_status"] = "passed"
	if err := approval.Validate(instance); err == nil {
		t.Fatal("approval rewrote the review verdict")
	}
}
