package toolreview

import (
	"encoding/json"
	"strconv"
	"strings"
)

type anthropicBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func anthropicBlockKind(kind string) (string, error) {
	switch kind {
	case "tool_use":
		return "tool", nil
	case "server_tool_use", "mcp_tool_use":
		return "hosted", nil
	case "text", "thinking", "redacted_thinking", "tool_result", "mcp_tool_result", "web_search_tool_result", "web_fetch_tool_result", "code_execution_tool_result", "bash_code_execution_tool_result", "text_editor_code_execution_tool_result", "container_upload":
		return "other", nil
	default:
		return "", safeError("unsupported_tool")
	}
}

func (c *Collector) anthropicBlock(index int, block anthropicBlock, stream bool) error {
	kind, err := anthropicBlockKind(block.Type)
	if err != nil {
		c.saw, c.contains = true, true
		return err
	}
	if stream {
		if c.complete || c.blocks[index] != "" {
			return safeError("conflicting_call")
		}
		if err := c.reserve(64); err != nil {
			return err
		}
		c.blocks[index] = kind
	}
	if kind != "tool" {
		return nil
	}
	v, err := c.call("anthropic:"+strconv.Itoa(index), 0, index, block.ID, block.Name, "function")
	if err != nil {
		return err
	}
	input := string(block.Input)
	if !validArguments(input) {
		return safeError("invalid_arguments")
	}
	// Anthropic starts streamed tool blocks with an empty object, which is a
	// placeholder rather than an argument fragment. No deltas means empty args.
	if !stream || !equalArguments(input, "{}", false) {
		if err := c.snapshotArguments(v, input); err != nil {
			return err
		}
	}
	if !stream {
		return c.finishCall(v)
	}
	return nil
}

func (c *Collector) validAnthropicStop(stop string) bool {
	switch stop {
	case "end_turn", "tool_use", "stop_sequence":
		return true
	case "max_tokens", "refusal", "pause_turn":
		return !c.saw
	default:
		return false
	}
}

func (c *Collector) observeAnthropic(payload []byte) error {
	var event struct {
		Type       string           `json:"type"`
		Index      *int             `json:"index"`
		Content    []anthropicBlock `json:"content"`
		StopReason string           `json:"stop_reason"`
		Block      *anthropicBlock  `json:"content_block"`
		Message    *struct {
			Content []anthropicBlock `json:"content"`
		} `json:"message"`
		Delta struct {
			Type        string  `json:"type"`
			PartialJSON *string `json:"partial_json"`
			StopReason  string  `json:"stop_reason"`
		} `json:"delta"`
	}
	if err := decodePayload(payload, &event); err != nil {
		return err
	}
	if !c.streaming {
		if event.Type != "message" {
			return safeError("incomplete_response")
		}
		for i, block := range event.Content {
			if err := c.anthropicBlock(i, block, false); err != nil {
				return err
			}
		}
		if !c.validAnthropicStop(event.StopReason) {
			return safeError("incomplete_response")
		}
		return c.finish()
	}
	switch event.Type {
	case "error":
		return safeError("upstream_failed")
	case "message_start":
		if c.complete {
			return safeError("conflicting_call")
		}
		if event.Message == nil {
			return safeError("invalid_payload")
		}
		// Client tool blocks must begin via content_block_start. An early
		// snapshot cannot be forwarded before the review buffer is engaged.
		for _, block := range event.Message.Content {
			kind, err := anthropicBlockKind(block.Type)
			if err != nil || kind == "tool" {
				c.saw, c.contains = true, true
				if err != nil {
					return err
				}
				return safeError("unexpected_tool_snapshot")
			}
		}
		return nil
	case "ping":
		if c.complete {
			return safeError("conflicting_call")
		}
		return nil
	case "content_block_start", "content_block_delta", "content_block_stop":
		if c.anthropicStop != "" {
			return safeError("conflicting_call")
		}
		if event.Index == nil || *event.Index < 0 {
			return safeError("invalid_payload")
		}
		index := *event.Index
		if event.Type == "content_block_start" {
			if event.Block == nil {
				return safeError("invalid_payload")
			}
			return c.anthropicBlock(index, *event.Block, true)
		}
		kind := c.blocks[index]
		if kind == "" {
			return safeError("incomplete_call")
		}
		if event.Type == "content_block_stop" {
			if strings.HasPrefix(kind, "closed:") {
				return nil
			}
			if kind == "tool" {
				v := c.calls["anthropic:"+strconv.Itoa(index)]
				if !v.hasArguments {
					if err := c.snapshotArguments(v, "{}"); err != nil {
						return err
					}
				}
				if err := c.finishCall(v); err != nil {
					return err
				}
			}
			c.blocks[index] = "closed:" + kind
			return nil
		}
		if strings.HasPrefix(kind, "closed:") || c.complete {
			return safeError("conflicting_call")
		}
		if kind == "tool" {
			c.contains = true
			if event.Delta.Type != "input_json_delta" || event.Delta.PartialJSON == nil {
				return safeError("unsupported_tool")
			}
			return c.appendArguments(c.calls["anthropic:"+strconv.Itoa(index)], *event.Delta.PartialJSON)
		}
		if kind != "hosted" && event.Delta.Type == "input_json_delta" {
			return safeError("unsupported_tool")
		}
		return nil
	case "message_delta":
		if event.Delta.StopReason != "" {
			if !c.validAnthropicStop(event.Delta.StopReason) {
				return safeError("incomplete_response")
			}
			if c.anthropicStop != "" && c.anthropicStop != event.Delta.StopReason {
				return safeError("conflicting_call")
			}
			c.anthropicStop = event.Delta.StopReason
		}
		return nil
	case "message_stop":
		for _, kind := range c.blocks {
			if !strings.HasPrefix(kind, "closed:") {
				return safeError("incomplete_call")
			}
		}
		return c.finish()
	default:
		return safeError("invalid_payload")
	}
}
