package contract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestToolReviewApprovalSettings(t *testing.T) {
	settings := DefaultToolReviewSettings()
	if settings.AskOnReviewError || settings.ApprovalTimeoutMS != 120000 {
		t.Fatalf("unsafe approval defaults: %+v", settings)
	}
	settings.Mode = ToolReviewModeAsk
	for _, ms := range []int{0, 999, 1000, 120000, 600000, 600001} {
		settings.ApprovalTimeoutMS = ms
		if err := settings.Validate(); (err == nil) != (ms >= 1000 && ms <= 600000) {
			t.Errorf("approval timeout=%d error=%v", ms, err)
		}
	}
}

func TestToolReviewApprovalMetadataValidation(t *testing.T) {
	now := time.Now().UTC()
	approval := ToolReviewApproval{ID: "approval_test", State: "pending", ReviewStatus: "flagged", RequestedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := approval.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"approved", "denied", "expired", "cancelled", "interrupted"} {
		a := approval
		a.State, a.DecidedAt = state, &now
		if err := a.Validate(); err != nil {
			t.Errorf("state %s: %v", state, err)
		}
	}
	for name, mutate := range map[string]func(*ToolReviewApproval){
		"unsafe id":     func(a *ToolReviewApproval) { a.ID = "raw text/secret" },
		"empty id":      func(a *ToolReviewApproval) { a.ID = "" },
		"unknown state": func(a *ToolReviewApproval) { a.State = "allowed_forever" },
		"fake verdict":  func(a *ToolReviewApproval) { a.ReviewStatus = "passed" },
		"zero start":    func(a *ToolReviewApproval) { a.RequestedAt = time.Time{} },
		"bad deadline":  func(a *ToolReviewApproval) { a.ExpiresAt = now },
		"pending time":  func(a *ToolReviewApproval) { a.DecidedAt = &now },
		"missing time":  func(a *ToolReviewApproval) { a.State = "approved" },
		"early time": func(a *ToolReviewApproval) {
			earlier := now.Add(-time.Second)
			a.State, a.DecidedAt = "approved", &earlier
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := approval
			mutate(&a)
			if err := a.Validate(); err == nil {
				t.Fatal("invalid approval accepted")
			}
		})
	}
}

func TestToolReviewApprovalPreservesReviewAndDeliveryIndependently(t *testing.T) {
	now := time.Now().UTC()
	approval := ToolReviewApproval{ID: "approval_test", State: "approved", ReviewStatus: "flagged", RequestedAt: now, ExpiresAt: now.Add(time.Minute), DecidedAt: &now}
	summary := ToolReviewSummary{Status: "flagged", Mode: ToolReviewModeAsk, Model: "jev-latest", StartedAt: now, Approval: &approval, Calls: []ToolReviewCall{{Index: 0, Name: "execute", Verdict: "flag", ModelVerdict: "flag", ReasonCode: "destructive_action", Action: "delivered"}}}
	for _, status := range []string{"pending", "flagged", "interrupted"} {
		summary.Status = status
		if err := summary.Validate(); err != nil {
			t.Fatalf("permission confused with review or delivery: %v", err)
		}
	}
	summary.Mode = ToolReviewModeMark
	if err := summary.Validate(); err == nil {
		t.Fatal("approval accepted outside ask mode")
	}
	var historical ToolReviewSummary
	if err := json.Unmarshal([]byte(`{"status":"passed","mode":"reject","model":"jev-latest","started_at":"2026-01-01T00:00:00Z","calls":[]}`), &historical); err != nil {
		t.Fatal(err)
	}
	if historical.Approval != nil {
		t.Fatal("fabricated historical permission")
	}
	encoded, err := json.Marshal(historical)
	if err != nil || strings.Contains(string(encoded), "approval") {
		t.Fatalf("historical permission fabricated: %s %v", encoded, err)
	}
}
