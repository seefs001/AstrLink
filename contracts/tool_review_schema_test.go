package contracts_test

import (
	"bytes"
	"os/exec"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Reuse the contract validator's Ruby YAML loader instead of adding a Go YAML
// dependency. Only local schema data is loaded; no control API is contacted.
func toolReviewCompiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	data, err := exec.Command("ruby", "-ryaml", "-rjson", "-e",
		"puts JSON.generate(YAML.load_file(ARGV.fetch(0)))", "control-api.openapi.yaml").Output()
	if err != nil {
		t.Fatalf("load OpenAPI YAML: %v", err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode OpenAPI: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("urn:astrlink:control-api", document); err != nil {
		t.Fatalf("add OpenAPI schemas: %v", err)
	}
	return compiler
}

func compileToolReviewSchema(t *testing.T, compiler *jsonschema.Compiler, name string) *jsonschema.Schema {
	t.Helper()
	schema, err := compiler.Compile("urn:astrlink:control-api#/components/schemas/" + name)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return schema
}

func TestToolReviewThresholdSchemas(t *testing.T) {
	compiler := toolReviewCompiler(t)
	for _, name := range []string{"ToolReviewSettings", "ToolReviewSettingsSummary", "ToolReviewSettingsPatch", "ToolReviewSummary"} {
		t.Run(name, func(t *testing.T) {
			schema := compileToolReviewSchema(t, compiler, name)
			var instance map[string]any
			switch name {
			case "ToolReviewSettings", "ToolReviewSettingsSummary":
				instance = map[string]any{
					"backend": "jev", "enabled": false, "mode": "mark", "model": "test-model",
					"timeout_ms": 5000, "revision": "test-revision", "credential_configured": false,
					"ask_on_review_error": false, "approval_timeout_ms": 120000, "batch_calls": false,
				}
				if name == "ToolReviewSettings" {
					instance["endpoint"] = "https://example.invalid/review"
					instance["prompt"] = "Review {{user_request}} and {{tool_call}}."
				}
			case "ToolReviewSettingsPatch":
				instance = map[string]any{"enabled": false}
			case "ToolReviewSummary":
				instance = map[string]any{
					"status": "passed", "mode": "mark", "model": "test-model",
					"config_revision": "test-revision", "started_at": "2026-01-01T00:00:00Z",
					"latency_ms": 0, "calls": []any{},
				}
			}
			optional := name == "ToolReviewSettingsPatch" || name == "ToolReviewSummary"
			if err := schema.Validate(instance); (err == nil) != optional {
				t.Fatalf("missing threshold: optional=%v, error=%v", optional, err)
			}
			for _, threshold := range []float64{0, 0.5, 1} {
				instance["reject_threshold"] = threshold
				if err := schema.Validate(instance); err != nil {
					t.Errorf("valid threshold %v rejected: %v", threshold, err)
				}
			}
			for _, threshold := range []any{-0.001, 1.001, nil, "0.5", "NaN", "Infinity", true} {
				instance["reject_threshold"] = threshold
				if err := schema.Validate(instance); err == nil {
					t.Errorf("invalid threshold %#v accepted", threshold)
				}
			}
		})
	}
}

func TestToolReviewCallClassificationSchema(t *testing.T) {
	schema := compileToolReviewSchema(t, toolReviewCompiler(t), "ToolReviewCall")
	instance := map[string]any{
		"index": 0, "name": "test_tool", "verdict": "pass", "action": "delivered",
		"latency_ms": 0, "model": "test-model",
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatalf("historical call without model verdict or reason rejected: %v", err)
	}
	for _, reason := range []string{
		"authorized", "unclear_authorization", "unauthorized_action", "destructive_action",
		"data_exfiltration", "privilege_escalation", "untrusted_execution", "prompt_injection",
		"unrelated_action", "policy_violation",
	} {
		instance["reason_code"] = reason
		if err := schema.Validate(instance); err != nil {
			t.Errorf("valid reason %q rejected: %v", reason, err)
		}
	}
	for _, reason := range []any{"", "unknown", "the action seems safe", nil, 1} {
		instance["reason_code"] = reason
		if err := schema.Validate(instance); err == nil {
			t.Errorf("invalid reason %#v accepted", reason)
		}
	}
	delete(instance, "reason_code")
	instance["reason"] = "free text"
	if err := schema.Validate(instance); err == nil {
		t.Error("free-text reason accepted")
	}
	delete(instance, "reason")
	for _, modelVerdict := range []string{"pass", "flag"} {
		instance["model_verdict"] = modelVerdict
		for _, verdict := range []string{"pass", "flag", "error"} {
			instance["verdict"] = verdict
			for _, action := range []string{"delivered", "withheld", "pending"} {
				instance["action"] = action
				if err := schema.Validate(instance); err != nil {
					t.Errorf("independent model verdict=%s verdict=%s action=%s rejected: %v", modelVerdict, verdict, action, err)
				}
			}
		}
	}
	for _, verdict := range []any{"error", "unknown", "", nil} {
		instance["model_verdict"] = verdict
		if err := schema.Validate(instance); err == nil {
			t.Errorf("invalid model verdict %#v accepted", verdict)
		}
	}
}

func TestToolReviewOnlyQuerySchema(t *testing.T) {
	// Locate by name, not by index: adding an unrelated filter must not alter
	// which query schema is tested.
	data, err := exec.Command("ruby", "-ryaml", "-rjson", "-e", `
parameters = YAML.load_file(ARGV.fetch(0)).dig("paths", "/control/v1/requests", "get", "parameters")
puts JSON.generate(parameters.find { |p| p["name"] == "tool_review_only" }.fetch("schema"))
`, "control-api.openapi.yaml").Output()
	if err != nil {
		t.Fatalf("load review-only query schema: %v", err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("urn:astrlink:review-only-query", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("urn:astrlink:review-only-query")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"true", "false"} {
		if err := schema.Validate(value); err != nil {
			t.Errorf("query %q rejected: %v", value, err)
		}
	}
	for _, value := range []any{"", "TRUE", "False", "1", "0", "yes", " true", "false ", true, false, nil, 1} {
		if err := schema.Validate(value); err == nil {
			t.Errorf("invalid query %#v accepted", value)
		}
	}
}
