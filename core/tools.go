package core

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/emotional-data8482/automata/retry"
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
// which normally propagate up and abort the run. A deadline created by
// [ToolPolicy.Timeout] becomes a recoverable [ErrToolTimeout] result instead;
// parent cancellation remains fatal. Panics are not a supported control-flow
// mechanism.
func Func[P any](name, description string, handler func(context.Context, P) (string, error)) Tool {
	var zero P
	schema := buildSchema(name, description, reflect.TypeOf(zero))
	return &funcTool[P]{name: name, schema: schema, handler: handler}
}

// funcResultTool is the rich-result counterpart of funcTool: it implements
// both [Tool] (so it registers anywhere a Tool does, flattening rich results
// to text) and [ResultTool] (so the run loop executes it richly).
type funcResultTool[P any] struct {
	name    string
	schema  json.RawMessage
	handler func(context.Context, P) (ToolResult, error)
}

func (t *funcResultTool[P]) Name() string            { return t.name }
func (t *funcResultTool[P]) Schema() json.RawMessage { return t.schema }

func (t *funcResultTool[P]) ExecuteResult(ctx context.Context, args string) (ToolResult, error) {
	var params P
	if args != "" && args != "null" && args != "{}" {
		if err := json.Unmarshal([]byte(args), &params); err != nil {
			return ToolResult{}, fmt.Errorf("invalid args: %w", err)
		}
	}
	return t.handler(ctx, params)
}

// Execute satisfies [Tool] for registration paths and consumers that only know
// the string API. It delegates to ExecuteResult and flattens the result with
// [ToolResult.Text].
func (t *funcResultTool[P]) Execute(ctx context.Context, args string) (string, error) {
	res, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return res.Text(), nil
}

// FuncResult is [Func] for tools that return rich content: the same typed
// schema generation applies to P, but the handler returns a [ToolResult] —
// built with [TextResult], [BlockResult], [ImageResult], [URLImageResult], or
// [ErrorResult] — which the run loop records as block-based
// [ToolResultBlock] content in the transcript. The returned value satisfies
// [Tool], so it registers via [Agent.RegisterTool] and [Agent.WithTools] like
// any other tool; the run loop detects the richer interface and preserves the
// blocks.
//
// Tool error semantics match [Func]: non-context errors from the handler (or
// from arg decoding, reported as "invalid args: …") become error tool results
// the model can recover from; parent [context.Canceled] and
// [context.DeadlineExceeded] abort the run, while a [ToolPolicy.Timeout]
// deadline becomes a recoverable [ErrToolTimeout] result.
func FuncResult[P any](name, description string, handler func(context.Context, P) (ToolResult, error)) Tool {
	var zero P
	schema := buildSchema(name, description, reflect.TypeOf(zero))
	return &funcResultTool[P]{name: name, schema: schema, handler: handler}
}

// retryTool wraps a [Tool] so its Execute is retried under cfg. See
// [WithToolRetry].
type retryTool struct {
	Tool
	cfg retry.Config
}

func (t *retryTool) Execute(ctx context.Context, args string) (string, error) {
	return retry.Do(ctx, t.cfg, func() (string, error) {
		return t.Tool.Execute(ctx, args)
	})
}

// retryResultTool additionally forwards [ResultTool] so wrapping a rich-result
// tool does not strip its block-carrying ExecuteResult (the run loop would
// otherwise silently fall back to the flattening Execute).
type retryResultTool struct {
	Tool
	result ResultTool
	cfg    retry.Config
}

func (t *retryResultTool) Execute(ctx context.Context, args string) (string, error) {
	return retry.Do(ctx, t.cfg, func() (string, error) {
		return t.Tool.Execute(ctx, args)
	})
}

func (t *retryResultTool) ExecuteResult(ctx context.Context, args string) (ToolResult, error) {
	return retry.Do(ctx, t.cfg, func() (ToolResult, error) {
		return t.result.ExecuteResult(ctx, args)
	})
}

// WithToolRetry wraps t so its Execute is retried under cfg, using the same
// [retry] policy the agent applies to provider calls. The run loop does not
// retry tools on its own (a retry there would replay a whole sub-agent run), so
// this is the opt-in for plain tools — an HTTP fetch, a database query — whose
// transient failures are worth retrying. The wrapped tool keeps t's name and
// schema; only Execute is affected.
//
// Do not wrap an [AsTool] sub-agent with this: sub-agents already retry at
// their provider layer, and retrying the Execute would re-run the entire
// sub-agent, re-emitting its stream events and double-counting its usage.
//
// If t implements [ResultTool], the wrapper forwards ExecuteResult so rich
// tool results survive the wrap.
func WithToolRetry(t Tool, cfg retry.Config) Tool {
	if rt, ok := t.(ResultTool); ok {
		return &retryResultTool{Tool: t, result: rt, cfg: cfg}
	}
	return &retryTool{Tool: t, cfg: cfg}
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
