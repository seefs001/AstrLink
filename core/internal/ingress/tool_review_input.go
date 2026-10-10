package ingress

import (
	"encoding/json"
	"strings"

	"github.com/QuantumNous/astrlink/core/contract"
)

// Review input is authorization context, not a display preview. Never trim,
// strip markup, read tool results, or fall back across a new non-text user turn.
func extractToolReviewInput(protocol contract.ProtocolID, body []byte) (string, bool, int, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return "", false, 1, err
	}
	count := 1
	var countJSON json.RawMessage
	switch protocol {
	case contract.ProtocolOpenAIChat:
		countJSON = root["n"]
	case contract.ProtocolGoogleGenerateContent:
		var config map[string]json.RawMessage
		if err := json.Unmarshal(root["generationConfig"], &config); len(root["generationConfig"]) > 0 && err != nil {
			return "", false, count, err
		}
		countJSON = config["candidateCount"]
	}
	if len(countJSON) > 0 && string(countJSON) != "null" {
		if err := json.Unmarshal(countJSON, &count); err != nil {
			return "", false, count, err
		}
	}
	items := root["messages"]
	if protocol == contract.ProtocolOpenAIResponses {
		items = root["input"]
		var text string
		if json.Unmarshal(items, &text) == nil && string(items) != "null" {
			return text, text != "", count, nil
		}
	} else if protocol == contract.ProtocolGoogleGenerateContent {
		items = root["contents"]
	}
	if len(items) == 0 {
		return "", false, count, nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(items, &messages); err != nil {
		return "", false, count, err
	}
	var latest string
	for _, message := range messages {
		role := reviewString(message["role"])
		// Gemini Content.role is optional and defaults to user. No other
		// protocol gets this default, and explicit non-user roles stay excluded.
		_, hasRole := message["role"]
		if role != "user" && !(protocol == contract.ProtocolGoogleGenerateContent && !hasRole) {
			continue
		}
		content := message["content"]
		if protocol == contract.ProtocolGoogleGenerateContent {
			content = message["parts"]
		}
		var text string
		if json.Unmarshal(content, &text) == nil && string(content) != "null" {
			latest = text
			continue
		}
		var blocks []map[string]json.RawMessage
		if len(content) > 0 && string(content) != "null" {
			if err := json.Unmarshal(content, &blocks); err != nil {
				return "", false, count, err
			}
		}
		var joined strings.Builder
		results, other := false, false
		for _, block := range blocks {
			kind := reviewString(block["type"])
			if protocol == contract.ProtocolGoogleGenerateContent {
				if _, ok := block["functionResponse"]; ok {
					results = true
					continue
				}
				other = true
				joined.WriteString(reviewString(block["text"]))
			} else {
				if kind == "tool_result" {
					results = true
					continue
				}
				other = true
				if kind == "text" || kind == "input_text" {
					joined.WriteString(reviewString(block["text"]))
				}
			}
		}
		switch {
		case !results:
			latest = joined.String()
		case other && joined.Len() > 0 && latest != "":
			// Text sent beside tool results continues the same turn; it must
			// not replace the request that authorized the loop.
			latest += "\n\n" + joined.String()
		case other && joined.Len() > 0:
			latest = joined.String()
		}
	}
	return latest, latest != "", count, nil
}

func reviewString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
