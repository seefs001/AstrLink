package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

func TestToolReviewOnlyControlPaginationStatusesAndObserver(t *testing.T) {
	store, handler := newToolReviewHandler(t)
	start := time.Now().UTC().Truncate(time.Second)
	for index, status := range []string{"blocked", "not_applicable", "failed", "pending", ""} {
		record := contract.RequestRecord{ID: contract.RequestID("request_review_" + string(rune('a'+index))), StartedAt: start.Add(time.Duration(index) * time.Second), Status: contract.RequestStatusSucceeded, InputProtocol: contract.ProtocolOpenAIChat, Audit: contract.NotCapturedAuditSummary()}
		if status != "" {
			record.ToolReview = &contract.ToolReviewSummary{Status: status, Mode: contract.ToolReviewModeReject, Model: "jev-test", StartedAt: record.StartedAt}
		}
		if status == "blocked" {
			record.Status = contract.RequestStatusBlocked
		}
		if err := store.InsertRequestRecord(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	for _, caller := range roleCallers {
		t.Run(caller.name, func(t *testing.T) {
			get := func(path string) contract.RequestRecordPage {
				t.Helper()
				req := httptest.NewRequest(http.MethodGet, path, nil)
				caller.set(req)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				if response.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
				}
				var page contract.RequestRecordPage
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				return page
			}
			first := get(RequestsPath + "?tool_review_only=true&limit=1")
			if len(first.Items) != 1 || first.Items[0].ID != "request_review_c" || first.NextCursor == nil {
				t.Fatalf("first filtered page: %+v", first)
			}
			last := get(RequestsPath + "?tool_review_only=true&limit=1&cursor=" + url.QueryEscape(*first.NextCursor))
			if len(last.Items) != 1 || last.Items[0].ID != "request_review_a" || last.NextCursor != nil {
				t.Fatalf("last filtered page: %+v", last)
			}
			for _, tc := range []struct {
				query string
				count int
			}{
				{"tool_review_only=true&tool_review_status=failed", 1},
				{"tool_review_only=true&tool_review_status=pending", 0},
				{"tool_review_only=true&tool_review_status=not_applicable", 0},
				{"tool_review_only=true&status=blocked", 1},
				{"tool_review_only=true&status=failed", 0},
				{"tool_review_only=true&tool_review_status=failed&status=succeeded", 1},
				{"tool_review_only=true&tool_review_status=failed&status=blocked", 0},
				{"tool_review_only=false&tool_review_status=pending", 1},
			} {
				page := get(RequestsPath + "?" + tc.query)
				if len(page.Items) != tc.count || page.Items == nil || page.NextCursor != nil {
					t.Fatalf("%s: %+v", tc.query, page)
				}
			}
			ordinary := get(RequestsPath)
			unchanged := get(RequestsPath + "?tool_review_only=false")
			ids := func(page contract.RequestRecordPage) []contract.RequestID {
				result := make([]contract.RequestID, 0, len(page.Items))
				for _, record := range page.Items {
					result = append(result, record.ID)
				}
				return result
			}
			if len(ordinary.Items) != 5 || !slices.Equal(ids(ordinary), ids(unchanged)) {
				t.Fatal("false filter changed ordinary request history")
			}
		})
	}
}

func TestToolReviewOnlyControlStrictQueryAndSessionRejection(t *testing.T) {
	_, handler := newToolReviewHandler(t)
	for _, query := range []string{
		"tool_review_only", "tool_review_only=", "tool_review_only=1", "tool_review_only=0",
		"tool_review_only=TRUE", "tool_review_only=False", "tool_review_only=yes", "tool_review_only=null",
		"tool_review_only=%20true", "tool_review_only=true%20", "tool_review_only=true&tool_review_only=false",
		"tool_review_only=true&tool_review_only=true", "tool_review_only=false&tool_review_only=false",
		"tool_review_only=true&tool_review_status=", "tool_review_only=true&tool_review_status=invalid",
		"tool_review_only=true&tool_review_status=failed&tool_review_status=failed",
	} {
		response := controlRequest(t, handler, http.MethodGet, RequestsPath+"?"+query, "", "", "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", query, response.Code, response.Body.String())
		}
	}
	for _, query := range []string{"tool_review_only=true", "tool_review_only=false", "tool_review_only="} {
		response := controlRequest(t, handler, http.MethodGet, RequestSessionsPath+"?"+query, "", "", "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("session %s: %d %s", query, response.Code, response.Body.String())
		}
	}
	response := controlRequest(t, handler, http.MethodGet, RequestsPath+"?tool_review_only=true&limit=1", "", "", "")
	var page contract.RequestRecordPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != http.StatusOK || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("empty store: %d %s %v", response.Code, response.Body.String(), err)
	}
}
