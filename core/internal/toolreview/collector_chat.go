package toolreview

import (
	"encoding/json"
	"strconv"
)

type chatFunction struct {
	Name      string  `json:"name"`
	Arguments *string `json:"arguments"`
	Input     *string `json:"input"`
}

type chatTool struct {
	Index    *int          `json:"index"`
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function *chatFunction `json:"function"`
	Custom   *chatFunction `json:"custom"`
}

type chatMessage struct {
	ToolCalls    []chatTool    `json:"tool_calls"`
	FunctionCall *chatFunction `json:"function_call"`
}

func (c *Collector) observeChat(payload []byte) error {
	var root struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Index   *int         `json:"index"`
			Delta   *chatMessage `json:"delta"`
			Message *chatMessage `json:"message"`
			Finish  *string      `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := decodePayload(payload, &root); err != nil {
		return err
	}
	if len(root.Error) > 0 && string(root.Error) != "null" {
		return safeError("upstream_failed")
	}
	if !c.streaming && len(root.Choices) != c.expectedCandidates {
		return safeError("incomplete_response")
	}
	seen := make(map[int]bool)
	for ordinal, choice := range root.Choices {
		group := ordinal
		if choice.Index != nil {
			group = *choice.Index
		} else if c.streaming && c.expectedCandidates > 1 {
			return safeError("invalid_candidates")
		}
		if err := c.registerGroup(group, seen); err != nil {
			return err
		}
		message, delta := choice.Message, false
		if choice.Delta != nil {
			if choice.Message != nil || !c.streaming {
				return safeError("invalid_payload")
			}
			message, delta = choice.Delta, true
		}
		if message != nil {
			if message.FunctionCall != nil && len(message.ToolCalls) > 0 {
				return safeError("conflicting_call")
			}
			if message.FunctionCall != nil {
				if err := c.chatTool(group, 0, chatTool{Type: "function", Function: message.FunctionCall}, delta, true); err != nil {
					return err
				}
			}
			for i, tool := range message.ToolCalls {
				index := i
				if tool.Index != nil {
					index = *tool.Index
				} else if delta {
					return safeError("incomplete_call")
				}
				if index < 0 {
					return safeError("invalid_payload")
				}
				if err := c.chatTool(group, index, tool, delta, false); err != nil {
					return err
				}
			}
		}
		if choice.Finish != nil && *choice.Finish != "" {
			switch *choice.Finish {
			case "stop", "tool_calls", "function_call":
			case "length", "content_filter":
				if c.hasGroupCalls(group) {
					return safeError("incomplete_response")
				}
			default:
				return safeError("incomplete_response")
			}
			for _, v := range c.calls {
				if v.group == group {
					if err := c.finishCall(v); err != nil {
						return err
					}
				}
			}
			c.groups[group] = true
		} else if !c.streaming {
			return safeError("incomplete_response")
		}
	}
	if c.streaming {
		return nil
	}
	if !c.groupsComplete() {
		return safeError("incomplete_response")
	}
	return c.finish()
}

func (c *Collector) chatTool(group, index int, tool chatTool, delta, legacy bool) error {
	c.saw, c.contains = true, true
	kind := tool.Type
	body := tool.Function
	if kind == "custom" || tool.Custom != nil {
		if tool.Function != nil || (kind != "" && kind != "custom") {
			return safeError("conflicting_call")
		}
		kind, body = "custom", tool.Custom
	} else if body != nil && kind == "" {
		kind = "function"
	}
	if kind != "" && kind != "function" && kind != "custom" {
		return safeError("unsupported_tool")
	}
	key := "chat:" + strconv.Itoa(group) + ":" + strconv.Itoa(index)
	if legacy {
		key += ":legacy"
	}
	name := ""
	if body != nil {
		name = body.Name
	}
	v, err := c.call(key, group, index, tool.ID, name, kind)
	if err != nil {
		return err
	}
	if body == nil {
		if !delta {
			return safeError("incomplete_call")
		}
		return nil
	}
	arguments := body.Arguments
	if kind == "custom" {
		arguments = body.Input
	}
	if arguments != nil {
		if delta {
			return c.appendArguments(v, *arguments)
		}
		return c.snapshotArguments(v, *arguments)
	}
	return nil
}
