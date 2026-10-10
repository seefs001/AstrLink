package toolreview

import (
	"encoding/json"
	"strconv"
	"strings"
)

type responseItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	Status    string          `json:"status"`
}

type responseSnapshot struct {
	Status string          `json:"status"`
	Output *[]responseItem `json:"output"`
}

func responseToolKind(kind string) (string, error) {
	switch kind {
	case "function_call":
		return "function", nil
	case "custom_tool_call":
		return "custom", nil
	case "message", "reasoning", "compaction", "compaction_summary", "function_call_output", "custom_tool_call_output",
		"web_search_call", "file_search_call", "code_interpreter_call", "image_generation_call",
		"mcp_call", "mcp_list_tools", "mcp_approval_request":
		return "", nil
	default:
		return "", safeError("unsupported_tool")
	}
}

func (c *Collector) responseItem(index int, item responseItem, final bool) error {
	kind, err := responseToolKind(item.Type)
	if err != nil {
		c.saw, c.contains = true, true
		return err
	}
	if kind == "" {
		return nil
	}
	if index < 0 {
		return safeError("invalid_payload")
	}
	key := "response:" + strconv.Itoa(index)
	v, err := c.call(key, 0, index, item.ID, item.Name, kind)
	if err != nil {
		return err
	}
	if item.CallID != "" {
		if (v.callID != "" && v.callID != item.CallID) || (c.callIDs[item.CallID] != "" && c.callIDs[item.CallID] != key) {
			return safeError("conflicting_call")
		}
		if v.callID == "" {
			if err := c.reserve(len(item.CallID) + 32); err != nil {
				return err
			}
			v.callID = item.CallID
			c.callIDs[item.CallID] = key
		}
	}
	raw := item.Arguments
	if kind == "custom" {
		raw = item.Input
	}
	var args *string
	if len(raw) > 0 && (json.Unmarshal(raw, &args) != nil || args == nil) {
		return safeError("invalid_arguments")
	}
	if args != nil && (final || *args != "") {
		if err := c.snapshotArguments(v, *args); err != nil {
			return err
		}
	}
	if final {
		if args == nil {
			return safeError("incomplete_call")
		}
		if item.Status != "" && item.Status != "completed" {
			return safeError("incomplete_call")
		}
		return c.finishCall(v)
	}
	return nil
}

func (c *Collector) responseSnapshot(snapshot responseSnapshot) error {
	if (snapshot.Status != "completed" && snapshot.Status != "incomplete") || snapshot.Output == nil {
		return safeError("incomplete_response")
	}
	// Some streaming providers send output_item.done snapshots followed by a
	// successful terminal with output: []. Only independently validated item
	// snapshots may supply calls omitted by that empty terminal. A nonempty
	// terminal remains authoritative and must account for every collected call.
	emptyFinal := c.streaming && snapshot.Status == "completed" && len(*snapshot.Output) == 0 && len(c.calls) > 0
	if c.complete && emptyFinal != c.responseEmptyFinal {
		return safeError("conflicting_call")
	}
	seen := make(map[string]bool)
	for i, item := range *snapshot.Output {
		if snapshot.Status == "incomplete" {
			kind, err := responseToolKind(item.Type)
			if err != nil || kind != "" {
				c.saw, c.contains = true, true
				if err != nil {
					return err
				}
				return safeError("incomplete_response")
			}
		}
		if err := c.responseItem(i, item, true); err != nil {
			return err
		}
		if item.Type == "function_call" || item.Type == "custom_tool_call" {
			seen["response:"+strconv.Itoa(i)] = true
		}
	}
	if snapshot.Status == "incomplete" && c.saw {
		return safeError("incomplete_response")
	}
	for key, call := range c.calls {
		if !seen[key] && !(emptyFinal && call.responseItemDone) {
			return safeError("conflicting_call")
		}
	}
	if err := c.finish(); err != nil {
		return err
	}
	c.responseEmptyFinal = emptyFinal
	return nil
}

func (c *Collector) observeResponses(payload []byte) error {
	if !c.streaming {
		var response responseSnapshot
		if err := decodePayload(payload, &response); err != nil {
			return err
		}
		return c.responseSnapshot(response)
	}
	var event struct {
		Type      string            `json:"type"`
		Index     *int              `json:"output_index"`
		ItemID    string            `json:"item_id"`
		Item      *responseItem     `json:"item"`
		Delta     *string           `json:"delta"`
		Arguments *string           `json:"arguments"`
		Input     *string           `json:"input"`
		Response  *responseSnapshot `json:"response"`
		Error     json.RawMessage   `json:"error"`
	}
	if err := decodePayload(payload, &event); err != nil {
		return err
	}
	switch event.Type {
	case "response.created", "response.in_progress":
		if c.complete {
			return safeError("conflicting_call")
		}
		if event.Response == nil {
			return safeError("invalid_payload")
		}
		if event.Response.Output != nil {
			// Tool-bearing early snapshots are not ordinary response metadata.
			// Fail before the caller can forward the containing frame.
			for _, item := range *event.Response.Output {
				kind, err := responseToolKind(item.Type)
				if err != nil || kind != "" {
					c.saw, c.contains = true, true
					if err != nil {
						return err
					}
					return safeError("unexpected_tool_snapshot")
				}
			}
		}
		return nil
	case "error", "response.failed", "response.cancelled":
		return safeError("upstream_failed")
	case "response.completed", "response.incomplete", "response.done":
		expectedStatus := strings.TrimPrefix(event.Type, "response.")
		if event.Type == "response.done" {
			expectedStatus = "completed"
		}
		if event.Response == nil || event.Response.Status != expectedStatus {
			return safeError("incomplete_response")
		}
		return c.responseSnapshot(*event.Response)
	case "response.output_item.added", "response.output_item.done":
		if event.Index == nil || event.Item == nil {
			return safeError("incomplete_call")
		}
		final := event.Type == "response.output_item.done"
		if err := c.responseItem(*event.Index, *event.Item, final); err != nil {
			return err
		}
		if final && (event.Item.Type == "function_call" || event.Item.Type == "custom_tool_call") {
			c.calls["response:"+strconv.Itoa(*event.Index)].responseItemDone = true
		}
		return nil
	case "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		c.saw, c.contains = true, true
		key := ""
		if event.Index != nil {
			key = "response:" + strconv.Itoa(*event.Index)
		}
		if event.ItemID != "" {
			if known, ok := c.ids[event.ItemID]; ok {
				if key != "" && key != known {
					return safeError("conflicting_call")
				}
				key = known
			} else {
				return safeError("incomplete_call")
			}
		}
		v := c.calls[key]
		if v == nil {
			return safeError("incomplete_call")
		}
		custom := strings.HasPrefix(event.Type, "response.custom_")
		if (v.kind == "custom") != custom {
			return safeError("conflicting_call")
		}
		if strings.HasSuffix(event.Type, ".delta") {
			if event.Delta == nil {
				return safeError("incomplete_call")
			}
			return c.appendArguments(v, *event.Delta)
		}
		args := event.Arguments
		if custom {
			args = event.Input
		}
		if args == nil {
			return safeError("incomplete_call")
		}
		if err := c.snapshotArguments(v, *args); err != nil {
			return err
		}
		return c.finishCall(v)
	default:
		// Unknown tool-bearing events must not turn an unsupported client tool
		// into a successful empty collection. Hosted events are explicitly known.
		for _, prefix := range []string{"response.web_search_call.", "response.file_search_call.", "response.code_interpreter_call.", "response.image_generation_call.", "response.mcp_call.", "response.mcp_list_tools."} {
			if strings.HasPrefix(event.Type, prefix) {
				return nil
			}
		}
		if strings.Contains(event.Type, "_call") || strings.Contains(event.Type, "_tool") || strings.Contains(event.Type, ".tool") {
			c.saw, c.contains = true, true
			return safeError("unsupported_tool")
		}
		if event.Type == "" {
			return safeError("invalid_payload")
		}
		return nil
	}
}
