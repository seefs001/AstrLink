package controlapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/toolreview"
)

const ToolReviewApprovalsPath = "/control/v1/tool-review/approvals"

func (handler *Handler) toolReviewApprovalResource(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if handler.toolReviewApprovals == nil {
		writeError(writer, http.StatusServiceUnavailable, "tool_review_approvals_unavailable", "tool review approvals are unavailable")
		return
	}
	if request.URL.Path == ToolReviewApprovalsPath {
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", "GET")
			writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is allowed")
			return
		}
		writeJSON(writer, http.StatusOK, struct {
			Items []contract.ToolReviewApprovalRequest `json:"items"`
		}{handler.toolReviewApprovals.List()})
		return
	}
	path := strings.TrimPrefix(request.URL.Path, ToolReviewApprovalsPath+"/")
	parts := strings.Split(path, "/")
	if parts[0] == "" || len(parts) > 2 || (len(parts) == 2 && parts[1] != "decision") {
		writeError(writer, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if len(parts) == 1 {
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", "GET")
			writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is allowed")
			return
		}
		// Never create a raw grant or invoke password proof. A nil vault is locked,
		// not a license to read request-scoped plaintext.
		if handler.rawVault == nil {
			writeError(writer, http.StatusLocked, "raw_locked", "raw content is locked")
			return
		}
		if opener, ok := handler.rawVault.UnlockedOpener(); !ok || opener == nil {
			writeError(writer, http.StatusLocked, "raw_locked", "raw content is locked")
			return
		}
		detail, err := handler.toolReviewApprovals.Detail(parts[0])
		if err != nil {
			writeToolApprovalError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, detail)
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", "POST")
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is allowed")
		return
	}
	if !requireMediaType(writer, request, "application/json") {
		return
	}
	var body toolApprovalDecision
	if !decodeControlJSON(writer, request, &body) {
		return
	}
	approval, err := handler.toolReviewApprovals.Decide(parts[0], body.Decision)
	if err != nil {
		writeToolApprovalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, approval)
}

type toolApprovalDecision struct{ Decision string }

func (body *toolApprovalDecision) UnmarshalJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return toolreview.ErrApprovalInput
	}
	if !decoder.More() {
		return toolreview.ErrApprovalInput
	}
	key, err := decoder.Token()
	if err != nil || key != "decision" {
		return toolreview.ErrApprovalInput
	}
	value, err := decoder.Token()
	decision, ok := value.(string)
	if err != nil || !ok || decoder.More() {
		return toolreview.ErrApprovalInput
	}
	ending, err := decoder.Token()
	if err != nil || ending != json.Delim('}') {
		return toolreview.ErrApprovalInput
	}
	body.Decision = decision
	return ensureControlJSONEOF(decoder)
}

func writeToolApprovalError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, toolreview.ErrApprovalConflict):
		writeError(writer, http.StatusConflict, "tool_review_approval_decided", "tool review approval was already decided")
	case errors.Is(err, toolreview.ErrApprovalInput):
		writeError(writer, http.StatusUnprocessableEntity, "invalid_tool_review_decision", "decision must be allow or deny")
	default:
		writeError(writer, http.StatusGone, "tool_review_approval_gone", "tool review approval is no longer pending")
	}
}
