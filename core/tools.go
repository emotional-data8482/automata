package core

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

type funcTool[P any] struct {
	name    string
	schema  json.RawMessage
	handler func(context.Context, P) (string, error)
}

func (t *funcTool[P]) Name() string            { return t.name }
func (t *funcTool[P]) Schema() json.RawMessage { return t.schema }

func (t *funcTool[P]) Execute(ctx context.Context, args string) (string, error) {
	var params P
	if args != "" && args != "null" && args != "{}" {
		if err := json.Unmarshal([]byte(args), &params); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
	}
	return t.handler(ctx, params)
}

// Func wraps a typed handler as a [Tool]. P must be a struct (or
// struct{} for no-arg tools); its fields drive the JSON schema advertised to
// the model.
//
// Tool error semantics: errors returned by the handler are converted to
// strings of the form "error: <msg>" and returned to the model as the tool
// result. The model can then retry, choose a different tool, or surface the
// error in its final response — a tool error never hard-fails the run. The
// only exceptions are [context.Canceled] and [context.DeadlineExceeded],
// which propagate up and abort the run; return one of those (or panic) to
// stop the loop.
func Func[P any](name, description string, handler func(context.Context, P) (string, error)) Tool {
	var zero P
	schema := buildSchema(name, description, reflect.TypeOf(zero))
	return &funcTool[P]{name: name, schema: schema, handler: handler}
}

func buildSchema(name, description string, t reflect.Type) json.RawMessage {
	params := objectSchema(t, map[reflect.Type]bool{})
	schema := map[string]any{
		"name":        name,
		"description": description,
		"parameters":  params,
	}
	raw, _ := json.Marshal(schema)
	return raw
}

// objectSchema walks the exported fields of a struct type and returns a JSON
// Schema object describing them. Non-struct (or nil) types yield an empty
// object schema, matching the original no-arg / struct{} behavior.
func objectSchema(t reflect.Type, visiting map[reflect.Type]bool) map[string]any {
	props := map[string]any{}
	required := []string{}

	if t != nil && t.Kind() == reflect.Struct {
		for i := range t.NumField() {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}

			jsonName := field.Name
			optional := false
			if tag := field.Tag.Get("json"); tag != "" {
				parts := strings.Split(tag, ",")
				if parts[0] == "-" {
					continue
				}
				if parts[0] != "" {
					jsonName = parts[0]
				}
				optional = strings.Contains(tag, "omitempty")
			}

			prop := typeSchema(field.Type, visiting)
			if desc := field.Tag.Get("desc"); desc != "" {
				prop["description"] = desc
			}
			props[jsonName] = prop

			if !optional {
				required = append(required, jsonName)
			}
		}
	}

	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

var (
	timeType       = reflect.TypeFor[time.Time]()
	rawMessageType = reflect.TypeFor[json.RawMessage]()
)

// typeSchema returns the JSON Schema fragment for a Go type. It recurses
// through slices (populating items), structs (populating properties/required),
// and string-keyed maps (populating additionalProperties). A visiting set
// breaks cycles on self-referential structs.
func typeSchema(t reflect.Type, visiting map[reflect.Type]bool) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if t == timeType {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	if t == rawMessageType {
		return map[string]any{}
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		return map[string]any{"type": "string"}
	}

	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{
			"type":  "array",
			"items": typeSchema(t.Elem(), visiting),
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return map[string]any{"type": "object"}
		}
		return map[string]any{
			"type":                 "object",
			"additionalProperties": typeSchema(t.Elem(), visiting),
		}
	case reflect.Struct:
		if visiting[t] {
			return map[string]any{"type": "object"}
		}
		visiting[t] = true
		out := objectSchema(t, visiting)
		delete(visiting, t)
		return out
	case reflect.Interface:
		return map[string]any{}
	default:
		return map[string]any{"type": "object"}
	}
}
