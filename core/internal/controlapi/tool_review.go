package controlapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

const ToolReviewSettingsPath = "/control/v1/tool-review-settings"

func (handler *Handler) toolReviewSettingsResource(writer http.ResponseWriter, request *http.Request) {
	if handler.toolReviewSettings == nil {
		writeError(writer, http.StatusServiceUnavailable, "tool_review_settings_unavailable", "tool review settings are unavailable")
		return
	}
	switch request.Method {
	case http.MethodGet:
		record, err := handler.toolReviewSettings.GetToolReviewSettings(request.Context())
		if err != nil {
			handler.writeStoreError(writer, err)
			return
		}
		if requestRole(request) < RoleOperator {
			// Agent observers can diagnose whether review is active without
			// learning an operator's endpoint or custom reviewer instructions.
			writeJSON(writer, http.StatusOK, struct {
				Enabled              bool                    `json:"enabled"`
				Mode                 contract.ToolReviewMode `json:"mode"`
				Model                string                  `json:"model"`
				TimeoutMS            int                     `json:"timeout_ms"`
				Revision             string                  `json:"revision"`
				CredentialConfigured bool                    `json:"credential_configured"`
			}{record.Settings.Enabled, record.Settings.Mode, record.Settings.Model, record.Settings.TimeoutMS, record.Settings.Revision, record.Settings.CredentialConfigured})
			return
		}
		writer.Header().Set("ETag", record.ETag)
		writeJSON(writer, http.StatusOK, record.Settings)
	case http.MethodPatch:
		if !requireMediaType(writer, request, "application/merge-patch+json") {
			return
		}
		etag := request.Header.Get("If-Match")
		if etag == "" {
			writeError(writer, http.StatusPreconditionRequired, "precondition_required", "If-Match is required")
			return
		}
		patch, ok := decodeToolReviewPatch(writer, request)
		if !ok {
			return
		}
		record, err := handler.toolReviewSettings.UpdateToolReviewSettings(request.Context(), patch, etag)
		if err != nil {
			if errors.Is(err, storage.ErrToolReviewAcknowledgement) {
				writeError(writer, http.StatusUnprocessableEntity, "tool_review_data_transfer_acknowledgement_required", "confirm that full user requests and tool arguments will be sent to the review provider")
			} else {
				handler.writeStoreError(writer, err)
			}
			return
		}
		writer.Header().Set("ETag", record.ETag)
		writeJSON(writer, http.StatusOK, record.Settings)
	default:
		writer.Header().Set("Allow", "GET, PATCH")
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "only GET and PATCH are allowed")
	}
}

func decodeToolReviewPatch(writer http.ResponseWriter, request *http.Request) (contract.ToolReviewSettingsPatch, bool) {
	var raw map[string]json.RawMessage
	var patch contract.ToolReviewSettingsPatch
	if !decodeControlJSON(writer, request, &raw) {
		return patch, false
	}
	if len(raw) == 0 {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_tool_review_settings", "patch must change at least one field")
		return patch, false
	}
	for name, value := range raw {
		var target any
		switch name {
		case "enabled":
			target = &patch.Enabled
		case "endpoint":
			target = &patch.Endpoint
		case "model":
			target = &patch.Model
		case "prompt":
			target = &patch.Prompt
		case "mode":
			target = &patch.Mode
		case "timeout_ms":
			target = &patch.TimeoutMS
		case "api_key":
			target = &patch.APIKey
		case "data_transfer_acknowledged":
			target = &patch.DataTransferAcknowledged
		default:
			writeError(writer, http.StatusUnprocessableEntity, "invalid_tool_review_settings", "unknown field")
			return patch, false
		}
		if isJSONNull(value) || strictUnmarshal(value, target) != nil {
			writeError(writer, http.StatusUnprocessableEntity, "invalid_tool_review_settings", "invalid field value")
			return patch, false
		}
	}
	return patch, true
}
