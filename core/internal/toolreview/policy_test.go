package toolreview_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

// Check the actual outbound policy, not a simulated model's interpretation of
// benign/dangerous commands. Live model calibration requires separate evidence.
func TestReviewChoiceCriteriaDeferToConfiguredPolicy(t *testing.T) {
	const custom = "Only flag credential exfiltration. Missing authorization alone is not a violation. {{user_request}} {{tool_call}}"
	for _, tc := range []struct{ name, prompt string }{
		{"default", contract.DefaultToolReviewSettings().Prompt},
		{"custom", custom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := tc.prompt
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					State struct {
						Rules string `json:"review_rules"`
					} `json:"state"`
					Questions map[string]struct {
						Instructions string            `json:"instructions"`
						Criteria     map[string]string `json:"criteria"`
					} `json:"questions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				for key, criterion := range body.Questions["review"].Criteria {
					if !strings.Contains(criterion, "state.review_rules") {
						t.Errorf("%s criterion imposes an independent policy: %s", key, criterion)
					}
				}
				if body.Questions["review"].Instructions != body.State.Rules || !strings.Contains(body.Questions["reason"].Instructions, "Do not add stricter requirements") {
					t.Error("review/reason questions disagree on policy scope")
				}
				if prompt == custom && !strings.Contains(body.State.Rules, "Only flag credential exfiltration.") {
					t.Error("custom policy replaced by default")
				}
				_, _ = io.WriteString(w, passAnswer)
			}))
			defer server.Close()
			snapshot := reviewSnapshot(server.URL, contract.ToolReviewModeMark)
			snapshot.Settings.Prompt = prompt
			summary := toolreview.NewClient(server.Client()).Review(context.Background(), snapshot, singleInput())
			if summary.Status != "passed" || snapshot.Settings.Prompt != prompt {
				t.Fatalf("policy request failed or snapshot mutated: %+v", summary)
			}
		})
	}
}

func TestDefaultReviewPolicyTargetsConcreteSevereHarm(t *testing.T) {
	prompt := contract.DefaultToolReviewSettings().Prompt
	for _, required := range []string{"concrete evidence", "credential", "system directories", "Missing context", "Routine file reading", "{{user_request}}", "{{tool_call}}"} {
		if !strings.Contains(prompt, required) {
			t.Errorf("default high-risk policy missing %q", required)
		}
	}
	if strings.Contains(prompt, "When intent or authorization is unclear, choose flag") {
		t.Error("default still rejects uncertainty alone")
	}
}
