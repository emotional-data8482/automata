package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
)

type registeredTool struct {
	definition ToolDefinition
	executor   Tool
}

func cloneToolDefinition(d ToolDefinition) ToolDefinition {
	d.InputSchema = append(json.RawMessage(nil), d.InputSchema...)
	return d
}

// WithTools registers additional executors for this run, before request
// preparation. Duplicate/reserved names and malformed definitions fail the run
// before provider work. Hooks can only select from the resulting registry.
func WithTools(tools ...Tool) RunOption {
	snapshot, err := freezeTools(tools, "")
	return func(c *runConfig) {
		c.extraTools = append(c.extraTools, snapshot...)
		if err != nil {
			c.optionErr = err
		}
	}
}

func registerTools(tools []Tool, terminal string) (map[string]registeredTool, []ToolDefinition, error) {
	registry := make(map[string]registeredTool, len(tools))
	definitions := make([]ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		if tool == nil || (reflect.ValueOf(tool).Kind() == reflect.Pointer && reflect.ValueOf(tool).IsNil()) {
			return nil, nil, fmt.Errorf("nil tool")
		}
		d := cloneToolDefinition(tool.Definition())
		if _, err := effectPolicyFor(tool); err != nil {
			return nil, nil, fmt.Errorf("tool %q effect policy: %w", d.Name, err)
		}
		if d.Name == "" {
			return nil, nil, fmt.Errorf("tool name is empty")
		}
		if d.Name == structuredOutputToolName && terminal != d.Name {
			return nil, nil, fmt.Errorf("reserved tool name %q", d.Name)
		}
		if _, ok := registry[d.Name]; ok {
			return nil, nil, fmt.Errorf("duplicate tool %q", d.Name)
		}
		var schema map[string]any
		if err := decodeSchemaRaw(d.InputSchema, &schema); err != nil {
			return nil, nil, fmt.Errorf("tool %q schema: %w", d.Name, err)
		}
		if schema == nil || schema["type"] != "object" {
			return nil, nil, fmt.Errorf("tool %q input schema must be an object schema", d.Name)
		}
		if err := validateSchema(schema); err != nil {
			return nil, nil, fmt.Errorf("tool %q schema: %w", d.Name, err)
		}
		registry[d.Name] = registeredTool{d, tool}
		definitions = append(definitions, cloneToolDefinition(d))
	}
	return registry, definitions, nil
}

func effectiveTools(req Request, registry map[string]registeredTool, terminal string) (map[string]registeredTool, error) {
	selected := make(map[string]registeredTool, len(req.Tools))
	for _, d := range req.Tools {
		entry, ok := registry[d.Name]
		if !ok {
			return nil, fmt.Errorf("request advertises unregistered tool %q", d.Name)
		}
		var original, modified any
		if err := json.Unmarshal(d.InputSchema, &modified); err != nil {
			return nil, fmt.Errorf("request tool %q schema: %w", d.Name, err)
		}
		_ = json.Unmarshal(entry.definition.InputSchema, &original)
		if d.Description != entry.definition.Description || !reflect.DeepEqual(original, modified) {
			return nil, fmt.Errorf("request changed definition of tool %q", d.Name)
		}
		if _, ok := selected[d.Name]; ok {
			return nil, fmt.Errorf("request repeats tool %q", d.Name)
		}
		selected[d.Name] = entry
	}
	if terminal != "" {
		if _, ok := selected[terminal]; !ok {
			return nil, fmt.Errorf("request removed terminal tool %q", terminal)
		}
	}
	if choice := req.Options.ToolChoice; choice != nil {
		switch choice.Mode {
		case ToolChoiceAuto, ToolChoiceNone:
		case ToolChoiceAny:
			if len(selected) == 0 {
				return nil, fmt.Errorf("forced tool choice requires an available tool")
			}
		case ToolChoiceTool:
			if _, ok := selected[choice.Name]; !ok {
				return nil, fmt.Errorf("forced tool %q is unavailable", choice.Name)
			}
		default:
			return nil, fmt.Errorf("invalid tool choice mode %d", choice.Mode)
		}
	}
	return selected, nil
}

// cloneRequest detaches the transformation input, including nested content.
// More general public observation/history isolation is implemented in Task 6.
func cloneRequest(req Request) Request {
	req.Messages = cloneMessages(req.Messages)
	req.Tools = append([]ToolDefinition(nil), req.Tools...)
	for i := range req.Tools {
		req.Tools[i] = cloneToolDefinition(req.Tools[i])
	}
	o := req.Options
	if o.Temperature != nil {
		v := *o.Temperature
		o.Temperature = &v
	}
	if o.ToolChoice != nil {
		v := *o.ToolChoice
		o.ToolChoice = &v
	}
	o.StopSequences = append([]string(nil), o.StopSequences...)
	o.OutputSchema = append(json.RawMessage(nil), o.OutputSchema...)
	req.Options = o
	return req
}

// decodeSchemaRaw decodes a raw schema with json.Number preservation so
// numeric assertions keep their exact literals through contract compilation.
// Exactly one top-level JSON value is accepted: a second Decode must reach
// io.EOF, so trailing garbage and concatenated values are rejected.
func decodeSchemaRaw(raw json.RawMessage, out *map[string]any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("schema must contain exactly one JSON value")
	}
	return nil
}

func validateToolArguments(schema, input json.RawMessage) error {
	if !json.Valid(input) {
		return fmt.Errorf("arguments must be valid JSON")
	}
	var s map[string]any
	if err := decodeSchemaRaw(schema, &s); err != nil {
		return err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	// Exactly one top-level JSON value: a second Decode must reach io.EOF,
	// rejecting trailing garbage and concatenated values.
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("arguments must contain exactly one JSON value")
	}
	return validateSchemaValue(s, value, "arguments")
}
