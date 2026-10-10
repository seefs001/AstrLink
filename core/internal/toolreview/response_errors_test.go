package toolreview_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

func TestReviewResponseErrorsIdentifyStageWithoutRawContent(t *testing.T) {
	for _, tc := range []struct{ name, body, code string }{
		{"json", `<html>private-response</html>`, "reviewer_invalid_json"},
		{"review", strings.Replace(passAnswer, `"choice":"pass"`, `"choice":"private-response"`, 1), "reviewer_invalid_review"},
		{"reason", withReason(passAnswer, `{"type":"choice","choice":"private-response"}`), "reviewer_invalid_reason"},
		{"model", `{"model":"private-response\n",` + passAnswer[1:], "reviewer_invalid_model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			for _, mode := range []contract.ToolReviewMode{contract.ToolReviewModeMark, contract.ToolReviewModeReject} {
				s := toolreview.NewClient(server.Client()).Review(context.Background(), reviewSnapshot(server.URL, mode), singleInput())
				wantAction := "delivered"
				if mode == contract.ToolReviewModeReject {
					wantAction = "withheld"
				}
				if s.Status != "failed" || s.ErrorCode != tc.code || len(s.Calls) != 1 || s.Calls[0].ErrorCode != tc.code || s.Calls[0].Action != wantAction || s.Calls[0].ReasonCode != "" || s.Calls[0].Model != "jev-latest" {
					t.Fatalf("response stage misattributed or raw content retained: %+v", s)
				}
			}
		})
	}
}
