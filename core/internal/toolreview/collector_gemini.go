package toolreview

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
)

type geminiFunction struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Args         json.RawMessage `json:"args"`
	PartialArgs  json.RawMessage `json:"partialArgs"`
	WillContinue *bool           `json:"willContinue"`
}

type geminiCandidate struct {
	Index   *int `json:"index"`
	Content struct {
		Parts []struct {
			FunctionCall   *geminiFunction `json:"functionCall"`
			ToolCall       json.RawMessage `json:"toolCall"`
			CustomToolCall json.RawMessage `json:"customToolCall"`
		} `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
}

// Some Gemini upstreams return the JSON chunk array even for a non-streaming
// request. Consume the entire bounded envelope before declaring it complete;
// neither an early candidate terminal nor trailing tool data may be dropped.
func (c *Collector) observeGeminiArray(payload []byte) error {
	var frames []json.RawMessage
	if json.Unmarshal(payload, &frames) != nil || len(frames) == 0 {
		return safeError("invalid_payload")
	}
	for _, frame := range frames {
		trimmed := bytes.TrimSpace(frame)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return safeError("invalid_payload")
		}
		if err := c.observeGemini(frame, true); err != nil {
			return err
		}
	}
	if !c.complete {
		return safeError("incomplete_response")
	}
	return nil
}

func (c *Collector) observeGemini(payload []byte, chunked bool) error {
	var response struct {
		Error          json.RawMessage   `json:"error"`
		Candidates     []geminiCandidate `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if err := decodePayload(payload, &response); err != nil {
		return err
	}
	if (len(response.Error) > 0 && string(response.Error) != "null") || response.PromptFeedback.BlockReason != "" {
		return safeError("upstream_failed")
	}
	if !chunked && len(response.Candidates) != c.expectedCandidates {
		return safeError("incomplete_response")
	}
	seen := make(map[int]bool)
	for ordinal, candidate := range response.Candidates {
		group := ordinal
		if candidate.Index != nil {
			group = *candidate.Index
		} else if chunked && c.expectedCandidates > 1 {
			return safeError("invalid_candidates")
		}
		if err := c.registerGroup(group, seen); err != nil {
			return err
		}
		var calls []geminiFunction
		for _, part := range candidate.Content.Parts {
			if len(part.ToolCall) > 0 || len(part.CustomToolCall) > 0 {
				c.saw, c.contains = true, true
				return safeError("unsupported_tool")
			}
			if part.FunctionCall != nil {
				c.saw, c.contains = true, true
				calls = append(calls, *part.FunctionCall)
			}
		}
		final := candidate.FinishReason != ""
		if final {
			switch candidate.FinishReason {
			case "STOP":
			case "MAX_TOKENS", "SAFETY":
				if len(calls) > 0 || c.hasGroupCalls(group) {
					return safeError("incomplete_response")
				}
			default:
				return safeError("incomplete_response")
			}
		}
		if !final && (!chunked || c.groups[group]) {
			return safeError("incomplete_response")
		}
		if err := c.geminiCalls(group, calls, final); err != nil {
			return err
		}
		if final {
			c.groups[group] = true
		}
	}
	if c.groupsComplete() {
		return c.finish()
	}
	if !chunked {
		return safeError("incomplete_response")
	}
	return nil
}

func (c *Collector) geminiCalls(group int, calls []geminiFunction, final bool) error {
	// Gemini part offsets restart with each chunk. IDs, when provided, identify
	// snapshots; otherwise full functionCall objects are separate streamed calls.
	// A final full snapshot can repeat all collected calls. Replayed final chunks
	// must match their original call list, not append those calls a second time.
	var keys []string
	if c.groups[group] {
		keys = c.geminiFinal[group]
		if len(keys) != len(calls) {
			return safeError("conflicting_call")
		}
	} else if final {
		keys = c.geminiSnapshotKeys(group, calls)
	}
	eventKeys := make([]string, 0, len(calls))
	seen := make(map[string]bool)
	for i, call := range calls {
		c.saw, c.contains = true, true
		if len(call.PartialArgs) > 0 || (call.WillContinue != nil && *call.WillContinue) {
			return safeError("unsupported_tool")
		}
		if !validArguments(string(call.Args)) {
			return safeError("invalid_arguments")
		}
		key := ""
		if keys != nil {
			key = keys[i]
		}
		if call.ID != "" {
			if known := c.ids[call.ID]; known != "" {
				if key != "" && key != known {
					return safeError("conflicting_call")
				}
				key = known
			}
		}
		slot := 0
		if key == "" {
			for _, v := range c.calls {
				if v.group == group && v.slot >= slot {
					slot = v.slot + 1
				}
			}
			key = "gemini:" + strconv.Itoa(group) + ":" + strconv.Itoa(slot)
		} else if v := c.calls[key]; v != nil {
			slot = v.slot
		}
		if seen[key] {
			return safeError("conflicting_call")
		}
		seen[key] = true
		v, err := c.call(key, group, slot, call.ID, call.Name, "function")
		if err != nil {
			return err
		}
		if v.group != group {
			return safeError("conflicting_call")
		}
		if err := c.snapshotArguments(v, string(call.Args)); err != nil {
			return err
		}
		if err := c.finishCall(v); err != nil {
			return err
		}
		eventKeys = append(eventKeys, key)
	}
	if final && !c.groups[group] {
		c.geminiFinal[group] = eventKeys
	}
	return nil
}

func (c *Collector) geminiSnapshotKeys(group int, calls []geminiFunction) []string {
	var keys []string
	for key, v := range c.calls {
		if v.group == group {
			keys = append(keys, key)
		}
	}
	if len(keys) != len(calls) || len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool { return c.calls[keys[i]].slot < c.calls[keys[j]].slot })
	for i, key := range keys {
		v, call := c.calls[key], calls[i]
		if v.name != call.Name || (call.ID != "" && v.id != call.ID) || !equalArguments(v.arguments.String(), string(call.Args), false) {
			return nil
		}
	}
	return keys
}
