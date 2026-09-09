package core

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// blockValue normalizes pointer implementations without changing the persisted shape.
func blockValue(b Block) Block {
	if nilDependency(b) {
		return nil
	}
	v := reflect.ValueOf(b)
	if v.Kind() == reflect.Pointer {
		return v.Elem().Interface().(Block)
	}
	return b
}
func validateBlock(b Block) error {
	b = blockValue(b)
	switch v := b.(type) {
	case TextBlock, ThinkingBlock:
		return nil
	case ImageBlock:
		if v.URL == "" && (v.MediaType == "" || len(v.Data) == 0) {
			return fmt.Errorf("image requires URL or media type and data")
		}
	case RawBlock:
		if v.Type == "" || !json.Valid(v.Data) {
			return fmt.Errorf("invalid raw block")
		}
	case ToolUseBlock:
		var args map[string]json.RawMessage
		if v.ID == "" || v.Name == "" {
			return fmt.Errorf("tool call requires id and name")
		}
		if err := json.Unmarshal(v.Input, &args); err != nil || args == nil {
			return fmt.Errorf("tool %q arguments must be a JSON object", v.ID)
		}
	case ToolResultBlock:
		if v.ToolUseID == "" {
			return fmt.Errorf("tool result requires call id")
		}
		for i, c := range v.Content {
			switch blockValue(c).(type) {
			case ToolUseBlock, ToolResultBlock:
				return fmt.Errorf("nested tool block %d", i)
			}
			if err := validateBlock(c); err != nil {
				return fmt.Errorf("content %d: %w", i, err)
			}
		}
	default:
		return fmt.Errorf("nil or unsupported block")
	}
	return nil
}
func validateHistory(messages []Message) error {
	pending := map[string]bool{}
	for i, m := range messages {
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" && m.Role != "tool" {
			return fmt.Errorf("message %d: invalid role %q", i, m.Role)
		}
		if len(pending) > 0 && m.Role != "tool" {
			return fmt.Errorf("message %d: missing results for preceding tool calls", i)
		}
		ids := map[string]bool{}
		for j, b := range m.Blocks {
			if err := validateBlock(b); err != nil {
				return fmt.Errorf("message %d block %d: %w", i, j, err)
			}
			switch v := blockValue(b).(type) {
			case ToolUseBlock:
				if m.Role != "assistant" || ids[v.ID] {
					return fmt.Errorf("message %d block %d: invalid or duplicate tool call %q", i, j, v.ID)
				}
				ids[v.ID] = true
				pending[v.ID] = true
			case ToolResultBlock:
				if m.Role != "tool" || !pending[v.ToolUseID] {
					return fmt.Errorf("message %d block %d: unmatched tool result %q", i, j, v.ToolUseID)
				}
				delete(pending, v.ToolUseID)
			default:
				if m.Role == "tool" {
					return fmt.Errorf("message %d block %d: tool message requires results", i, j)
				}
			}
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("message %d: missing tool results", len(messages)-1)
	}
	return nil
}
func reconcileAssistant(original Message) (Message, error) {
	m := cloneMessages([]Message{original})[0]
	var err error
	if m.Role != "assistant" {
		err = fmt.Errorf("invalid assistant role %q", m.Role)
	}
	ids := map[string]bool{}
	for i, b := range m.Blocks {
		b = blockValue(b)
		m.Blocks[i] = b
		if e := validateBlock(b); e != nil {
			err = fmt.Errorf("block %d: %w", i, e)
		}
		switch v := b.(type) {
		case ToolUseBlock:
			if ids[v.ID] {
				err = fmt.Errorf("block %d: duplicate tool id %q", i, v.ID)
			}
			ids[v.ID] = true
		case ToolResultBlock:
			err = fmt.Errorf("block %d: assistant cannot contain tool results", i)
		}
	}
	if err == nil {
		return m, nil
	}
	m.Role = "assistant"
	m.Blocks = nil
	for _, b := range original.Blocks {
		b = blockValue(b)
		switch b.(type) {
		case TextBlock, ThinkingBlock, ImageBlock:
			if validateBlock(b) == nil {
				m.Blocks = append(m.Blocks, cloneBlock(b))
			}
		}
	}
	return m, err
}
func responseDiagnostics(m Message, turn int, cause error) []RunDiagnostic {
	out := []RunDiagnostic{{Turn: turn, Kind: "rejected_response", Message: cause.Error()}}
	for i, b := range m.Blocks {
		d := RunDiagnostic{Turn: turn, Kind: "rejected_block", Message: fmt.Sprintf("block %d", i)}
		switch v := blockValue(b).(type) {
		case ToolUseBlock:
			d.ToolCallID = v.ID
			d.Message += ": " + v.Name
			d.Data = append([]byte(nil), v.Input...)
		case RawBlock:
			d.Data = append([]byte(nil), v.Data...)
		default:
			d.Data, _ = json.Marshal(b)
		}
		out = append(out, d)
	}
	return out
}
