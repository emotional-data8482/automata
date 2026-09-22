package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/emotional-data8482/automata/retry"
	"reflect"
	"strings"
	"time"
)

type funcTool[P any] struct {
	definition ToolDefinition
	handler    func(context.Context, P) (ToolResult, error)
}

func (t *funcTool[P]) Definition() ToolDefinition { return cloneToolDefinition(t.definition) }
func (t *funcTool[P]) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var params P
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &params); err != nil {
			return ToolResult{}, fmt.Errorf("invalid args: %w", err)
		}
	}
	return t.handler(ctx, params)
}

// Func wraps a typed text handler. Non-nil Go errors are run-fatal after batch
// reconciliation. Use FuncResult and ErrorResult for model-recoverable failures.
func Func[P any](name, description string, handler func(context.Context, P) (string, error)) Tool {
	return FuncResult(name, description, func(ctx context.Context, p P) (ToolResult, error) {
		text, err := handler(ctx, p)
		return TextResult(text), err
	})
}

// FuncResult wraps a typed rich handler with a root input schema derived from P.
// Its single Execute method preserves blocks through registration and wrappers.
// ErrorResult is recoverable; a non-nil Go error aborts execution.
func FuncResult[P any](name, description string, handler func(context.Context, P) (ToolResult, error)) Tool {
	return &funcTool[P]{definition: buildDefinition(name, description, reflect.TypeFor[P]()), handler: handler}
}

type retryTool struct {
	Tool
	cfg retry.Config
}

func (t *retryTool) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	var last ToolResult
	_, err := retry.Do(ctx, t.cfg, func() (ToolResult, error) {
		var executeErr error
		last, executeErr = t.Tool.Execute(ctx, args)
		if executeErr != nil && (last.Effect.Status == EffectApplied || last.Effect.Status == EffectUnknown) {
			executeErr = nonRetryableToolEffectError{cause: executeErr}
		}
		return last, executeErr
	})
	var guarded nonRetryableToolEffectError
	if errors.As(err, &guarded) {
		err = guarded.cause
	}
	return last, err
}
func (t *retryTool) toolEffectPolicy() ToolEffectPolicy {
	policy, _ := effectPolicyFor(t.Tool)
	return policy
}
func (t *retryTool) durableWaitPolicy() DurableWaitPolicy {
	policy, _, _ := waitPolicyFor(t.Tool)
	return policy
}

type nonRetryableToolEffectError struct{ cause error }

func (e nonRetryableToolEffectError) Error() string   { return e.cause.Error() }
func (e nonRetryableToolEffectError) Unwrap() error   { return e.cause }
func (e nonRetryableToolEffectError) Retryable() bool { return false }

// WithToolRetry opts into retries of eligible returned Go errors, preserving rich
// results. ErrorResult with nil error is never retried, and a result reporting
// EffectApplied or EffectUnknown is returned immediately with its error even if
// that error is retryable. Do not wrap AsTool: retrying it would repeat the
// entire child operation and its side effects.
func WithToolRetry(t Tool, cfg retry.Config) Tool { return &retryTool{Tool: t, cfg: cfg} }

type legacyErrorTool struct{ Tool }

func (t *legacyErrorTool) toolEffectPolicy() ToolEffectPolicy {
	policy, _ := effectPolicyFor(t.Tool)
	return policy
}
func (t *legacyErrorTool) durableWaitPolicy() DurableWaitPolicy {
	policy, _, _ := waitPolicyFor(t.Tool)
	return policy
}

func (t *legacyErrorTool) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	result, err := t.Tool.Execute(ctx, args)
	if err == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return result, err
	}
	adapted := ErrorResult(err.Error())
	adapted.Effect = result.Effect
	return adapted, nil
}

// WithLegacyToolErrors explicitly preserves the former error-to-model behavior:
// ordinary Go errors become ErrorResult(err.Error()); context errors remain fatal.
// Apply retry inside this adapter if eligible errors should be retried first.
func WithLegacyToolErrors(t Tool) Tool { return &legacyErrorTool{Tool: t} }

func buildDefinition(name, description string, t reflect.Type) ToolDefinition {
	raw, _ := json.Marshal(objectSchema(t, map[reflect.Type]bool{}))
	return ToolDefinition{Name: name, Description: description, InputSchema: raw}
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
	nullable := false
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
		nullable = true
	}

	var schema map[string]any
	if t == timeType {
		schema = map[string]any{"type": "string", "format": "date-time"}
		return nullableTypeSchema(schema, nullable)
	}
	if t == rawMessageType {
		schema = map[string]any{}
		return nullableTypeSchema(schema, nullable)
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		schema = map[string]any{"type": "string"}
		return nullableTypeSchema(schema, nullable)
	}

	switch t.Kind() {
	case reflect.String:
		schema = map[string]any{"type": "string"}
	case reflect.Bool:
		schema = map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		schema = map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		schema = map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		schema = map[string]any{
			"type":  "array",
			"items": typeSchema(t.Elem(), visiting),
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			schema = map[string]any{"type": "object"}
			break
		}
		schema = map[string]any{
			"type":                 "object",
			"additionalProperties": typeSchema(t.Elem(), visiting),
		}
	case reflect.Struct:
		if visiting[t] {
			schema = map[string]any{"type": "object"}
			break
		}
		visiting[t] = true
		schema = objectSchema(t, visiting)
		delete(visiting, t)
	case reflect.Interface:
		schema = map[string]any{}
	default:
		schema = map[string]any{"type": "object"}
	}
	return nullableTypeSchema(schema, nullable)
}

func nullableTypeSchema(schema map[string]any, nullable bool) map[string]any {
	if !nullable {
		return schema
	}
	typ, ok := schema["type"].(string)
	if !ok || typ == "null" {
		return schema
	}
	schema["type"] = []any{typ, "null"}
	return schema
}
