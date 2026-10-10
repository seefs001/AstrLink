package sqlite

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestToolReviewOnlyFiltersBeforePaginationAndCombinesStatuses(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	defer store.Close()
	start := time.Now().UTC().Truncate(time.Second)
	fixtures := []struct {
		id, review string
		calls      bool
	}{
		{"request_passed", "passed", true},
		{"request_unreviewed", "", false},
		{"request_failed", "failed", false},
		{"request_pending_empty", "pending", false},
		{"request_interrupted", "interrupted", false},
		{"request_not_applicable", "not_applicable", false},
		{"request_cancelled", "cancelled", false},
		{"request_pending_null", "pending", false},
		{"request_pending_calls", "pending", true},
		{"request_pending_missing", "pending", false},
		{"request_flagged", "flagged", true},
		{"request_blocked", "blocked", true},
		{"request_json_null", "", false},
		{"request_empty", "", false},
	}
	for index, fixture := range fixtures {
		record := contract.RequestRecord{ID: contract.RequestID(fixture.id), StartedAt: start.Add(time.Duration(index) * time.Second), Status: contract.RequestStatusSucceeded, InputProtocol: contract.ProtocolOpenAIChat, Audit: contract.NotCapturedAuditSummary()}
		if fixture.review != "" {
			record.ToolReview = &contract.ToolReviewSummary{Status: fixture.review, Mode: contract.ToolReviewModeMark, Model: "jev-test", StartedAt: record.StartedAt}
			if fixture.calls {
				record.ToolReview.Calls = []contract.ToolReviewCall{{Index: 0, Name: "inspect", Verdict: "pass", Action: "delivered"}}
			}
		}
		if fixture.review == "blocked" {
			record.Status = contract.RequestStatusBlocked
		}
		if err := store.InsertRequestRecord(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, store, `UPDATE request_records SET tool_review_json = 'null' WHERE id = 'request_json_null'`)
	mustExec(t, store, `UPDATE request_records SET tool_review_json = json_set(tool_review_json, '$.calls', NULL) WHERE id = 'request_pending_null'`)
	mustExec(t, store, `UPDATE request_records SET tool_review_json = json_remove(tool_review_json, '$.calls') WHERE id = 'request_pending_missing'`)
	ordinary, err := store.ListRequestRecords(ctx, storage.RequestRecordListOptions{})
	if err != nil || len(ordinary.Items) != len(fixtures) {
		t.Fatalf("ordinary list lost records: %+v %v", ordinary, err)
	}
	// The expression index prevents empty JSON in current databases. Simulate
	// an unindexed legacy empty string to verify the filter guards JSON parsing.
	mustExec(t, store, `DROP INDEX request_records_tool_review_status_idx`)
	mustExec(t, store, `UPDATE request_records SET tool_review_json = '' WHERE id = 'request_empty'`)
	options := storage.RequestRecordListOptions{ToolReviewOnly: true, Limit: 2}
	var ids []string
	for pageIndex := 0; ; pageIndex++ {
		if pageIndex > len(fixtures) {
			t.Fatal("pagination did not terminate")
		}
		page, err := store.ListRequestRecords(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		if page.Items == nil || (page.NextCursor != "" && len(page.Items) != options.Limit) {
			t.Fatalf("sparse filtered page: %+v", page)
		}
		for _, record := range page.Items {
			ids = append(ids, string(record.ID))
		}
		if page.NextCursor == "" {
			break
		}
		options.Cursor = page.NextCursor
	}
	want := []string{"request_blocked", "request_flagged", "request_pending_calls", "request_cancelled", "request_interrupted", "request_failed", "request_passed"}
	if !slices.Equal(ids, want) {
		t.Fatalf("paginated records = %v, want %v", ids, want)
	}
	for _, tc := range []struct {
		review string
		status contract.RequestStatus
		want   []string
	}{
		{"pending", contract.RequestStatusSucceeded, []string{"request_pending_calls"}},
		{"not_applicable", contract.RequestStatusSucceeded, []string{}},
		{"blocked", contract.RequestStatusSucceeded, []string{}},
		{"blocked", contract.RequestStatusBlocked, []string{"request_blocked"}},
		{"failed", contract.RequestStatusSucceeded, []string{"request_failed"}},
		{"interrupted", contract.RequestStatusSucceeded, []string{"request_interrupted"}},
		{"cancelled", contract.RequestStatusSucceeded, []string{"request_cancelled"}},
	} {
		t.Run(tc.review+"_"+string(tc.status), func(t *testing.T) {
			page, err := store.ListRequestRecords(ctx, storage.RequestRecordListOptions{ToolReviewOnly: true, ToolReviewStatus: tc.review, Status: &tc.status, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(page.Items))
			for _, record := range page.Items {
				got = append(got, string(record.ID))
			}
			if !slices.Equal(got, tc.want) || page.NextCursor != "" || page.Items == nil {
				t.Fatalf("combined filter: %+v, want %v", page, tc.want)
			}
		})
	}
}
