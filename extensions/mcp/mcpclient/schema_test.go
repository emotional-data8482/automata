package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emotional-data8482/automata/core"
	"github.com/google/jsonschema-go/jsonschema"
)

// decodeSDK decodes JSON the way the SDK hands a client an input schema:
// the default map[string]any form, with float64 numbers.
func decodeSDK(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("bad test JSON %s: %v", text, err)
	}
	return v
}

func canonical(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("bad JSON %s: %v", raw, err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

type nopProvider struct{}

func (nopProvider) Invoke(context.Context, core.Request) (core.Response, error) {
	return core.Response{}, fmt.Errorf("not called")
}

// registerable asserts core accepts schema as a tool input schema, which is
// the contract lowerSchema exists to satisfy.
func registerable(t *testing.T, schema json.RawMessage) {
	t.Helper()
	tool := &remoteTool{remoteName: "t", definition: core.ToolDefinition{Name: "t", InputSchema: schema}}
	if _, err := core.New(nopProvider{}, core.AgentConfig{Tools: []core.Tool{tool}}); err != nil {
		t.Fatalf("core rejected lowered schema %s: %v", schema, err)
	}
}

// originalAccepts asserts a test sample is valid under the source schema,
// using the JSON Schema validator the MCP SDK itself uses.
func originalAccepts(t *testing.T, schema, args string) {
	t.Helper()
	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(schema), &s); err != nil {
		t.Fatalf("original schema: %v", err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("original schema: %v", err)
	}
	if err := resolved.Validate(decodeSDK(t, args)); err != nil {
		t.Fatalf("sample %s is invalid under the original schema: %v", args, err)
	}
}

// acceptTool records whether core's argument validation let a call through.
type acceptTool struct {
	definition core.ToolDefinition
	executed   atomic.Bool
}

func (a *acceptTool) Definition() core.ToolDefinition { return a.definition }
func (a *acceptTool) Execute(context.Context, json.RawMessage) (core.ToolResult, error) {
	a.executed.Store(true)
	return core.TextResult("ok"), nil
}

// oneCall requests a single tool call, then ends the run.
type oneCall struct {
	args  json.RawMessage
	turns atomic.Int32
}

func (p *oneCall) Invoke(context.Context, core.Request) (core.Response, error) {
	if p.turns.Add(1) == 1 {
		return core.Response{Message: core.AssistantMessage(core.ToolUseBlock{ID: "c1", Name: "t", Input: p.args}), StopReason: core.StopToolUse}, nil
	}
	return core.Response{Message: core.AssistantMessage(core.TextBlock{Text: "done"}), StopReason: core.StopEndTurn}, nil
}

// coreAccepts runs one call with args through a Runtime, which validates
// arguments against the lowered schema before executing the tool.
func coreAccepts(t *testing.T, schema json.RawMessage, args string) {
	t.Helper()
	tool := &acceptTool{definition: core.ToolDefinition{Name: "t", InputSchema: schema}}
	agent, err := core.New(&oneCall{args: json.RawMessage(args)}, core.AgentConfig{Tools: []core.Tool{tool}, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("core rejected lowered schema %s: %v", schema, err)
	}
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ref, err := runtime.Register("t", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), ref, "go")
	if err != nil {
		t.Fatal(err)
	}
	if !tool.executed.Load() {
		t.Fatalf("lowering tightened the schema: core rejected %s under %s: %s", args, schema, result.Messages[len(result.Messages)-2].Text())
	}
}

// TestLowerSchema pins each rewrite and the property the package docs
// promise: lowering only ever relaxes. Every sample the original schema
// accepts must pass core's own argument validation of the lowered schema.
func TestLowerSchema(t *testing.T) {
	cases := []struct {
		name, in, want string
		// accepts are argument objects valid under in; omit them when the
		// SDK's validator cannot load in (remote refs, draft-04, ECMA-only
		// regexps, malformed keywords).
		accepts []string
	}{
		{
			name: "pydantic optional becomes nullable type",
			in: `{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema","title":"Args",
				"properties":{"q":{"type":"string"},
				"limit":{"anyOf":[{"type":"integer"},{"type":"null"}],"default":null,"title":"Limit"}},
				"required":["q"]}`,
			want: `{"type":"object","title":"Args","properties":{"q":{"type":"string"},
				"limit":{"type":["integer","null"],"default":null,"title":"Limit"}},"required":["q"]}`,
			accepts: []string{`{"q":"x"}`, `{"q":"x","limit":null}`, `{"q":"x","limit":3}`},
		},
		{
			name: "optional literal admits null beside its enum",
			in: `{"type":"object","properties":{"level":{"anyOf":[{"enum":["low","high"],"type":"string"},{"type":"null"}],"default":null},
				"either":{"oneOf":[{"enum":[1,2]},{"type":"null"}]}}}`,
			want: `{"type":"object","properties":{"level":{"type":["string","null"],"enum":["low","high",null],"default":null},
				"either":{"enum":[1,2,null]}}}`,
			accepts: []string{`{}`, `{"level":null,"either":null}`, `{"level":"low","either":2}`},
		},
		{
			name: "local refs inline and siblings win",
			in: `{"type":"object","properties":{"item":{"$ref":"#/$defs/Item","description":"the item"}},
				"$defs":{"Item":{"type":"object","description":"an item","properties":{"id":{"type":"integer","minimum":1}},"required":["id"]}}}`,
			want: `{"type":"object","properties":{"item":{"type":"object","description":"the item",
				"properties":{"id":{"type":"integer","minimum":1}},"required":["id"]}}}`,
			accepts: []string{`{"item":{"id":2}}`, `{}`},
		},
		{
			name: "recursive ref is cut to an unconstrained schema",
			in: `{"type":"object","properties":{"node":{"$ref":"#/definitions/Node"}},
				"definitions":{"Node":{"type":"object","properties":{"children":{"type":"array","items":{"$ref":"#/definitions/Node"}}}}}}`,
			want:    `{"type":"object","properties":{"node":{"type":"object","properties":{"children":{"type":"array","items":{}}}}}}`,
			accepts: []string{`{"node":{"children":[{"children":[{}]}]}}`},
		},
		{
			name: "escaped pointer, root and remote refs",
			in: `{"type":"object","properties":{"a":{"$ref":"#/$defs/a~1b"},"self":{"$ref":"#"},"far":{"$ref":"https://example.com/s.json"}},
				"$defs":{"a/b":{"type":"boolean"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"boolean"},"self":{},"far":{}}}`,
		},
		{
			name: "const becomes enum and allOf branches merge",
			in: `{"type":"object","properties":{"mode":{"const":"fast"}},"required":["mode"],
				"allOf":[{"properties":{"a":{"type":"string"}},"required":["a"]},{"properties":{"b":{"type":"number"}},"required":["b","a"]}]}`,
			want: `{"type":"object","properties":{"mode":{"enum":["fast"]},"a":{"type":"string"},"b":{"type":"number"}},
				"required":["mode","a","b"]}`,
			accepts: []string{`{"mode":"fast","a":"x","b":1.5}`},
		},
		{
			name: "unenforceable assertions are dropped",
			in: `{"type":"object","properties":{
				"tags":{"type":"array","items":{"type":"string"},"minItems":1,"uniqueItems":true},
				"code":{"type":"string","minLength":2,"not":{"const":"no"}},
				"n":{"type":"number","multipleOf":2},
				"m":{"type":"object","additionalProperties":{"type":"integer","maxProperties":2}}}}`,
			want: `{"type":"object","properties":{
				"tags":{"type":"array","items":{"type":"string"}},
				"code":{"type":"string","minLength":2},
				"n":{"type":"number"},
				"m":{"type":"object","additionalProperties":{"type":"integer"}}}}`,
			accepts: []string{`{"tags":["a"],"code":"ok","n":4,"m":{"k":1}}`},
		},
		{
			name: "keywords that depend on a dropped keyword go with it",
			in: `{"type":"object","properties":{
				"m":{"type":"object","patternProperties":{"^x":{"type":"string"}},"additionalProperties":{"type":"integer"}},
				"pair":{"type":"array","prefixItems":[{"type":"string"}],"items":{"type":"integer"}}}}`,
			want:    `{"type":"object","properties":{"m":{"type":"object"},"pair":{"type":"array"}}}`,
			accepts: []string{`{"m":{"x1":"s","k":2},"pair":["a",1,2]}`},
		},
		{
			name: "draft-07 tuples and ECMA-only patterns are dropped",
			in: `{"type":"object","properties":{"code":{"type":"string","pattern":"^(?=a)"},
				"pair":{"type":"array","items":[{"type":"string"}]}}}`,
			want: `{"type":"object","properties":{"code":{"type":"string"},"pair":{"type":"array"}}}`,
		},
		{
			name: "unions keep only what every variant shares",
			in: `{"type":"object","properties":{
				"id":{"type":["string","integer"]},
				"flag":{"type":["null","boolean"]},
				"when":{"oneOf":[{"type":"string","maxLength":2},{"type":"string","minLength":5,"format":"date"}]},
				"shape":{"anyOf":[{"type":"object","properties":{"r":{"type":"number"}},"required":["r"]},
					{"type":"object","properties":{"w":{"type":"number"}},"required":["w"]},{"type":"null"}]},
				"mixed":{"anyOf":[{"type":"string"},{"type":"integer"}],"description":"either"},
				"nothing":{"anyOf":[{"type":"null"}]}}}`,
			want: `{"type":"object","properties":{
				"id":{},
				"flag":{"type":["boolean","null"]},
				"when":{"type":"string"},
				"shape":{"type":["object","null"],"properties":{"r":{"type":"number"},"w":{"type":"number"}}},
				"mixed":{"description":"either"},
				"nothing":{"type":"null"}}}`,
			accepts: []string{
				`{"id":"x","flag":null,"when":"2024-01-01","shape":{"r":1},"mixed":3,"nothing":null}`,
				`{"id":1,"flag":true,"shape":null,"mixed":"s"}`,
			},
		},
		{
			name: "draft-04 exclusive bounds convert",
			in: `{"type":"object","properties":{"x":{"type":"number","minimum":0.5,"exclusiveMinimum":true,"maximum":10,"exclusiveMaximum":false},
				"y":{"type":"integer","exclusiveMaximum":3}}}`,
			want: `{"type":"object","properties":{"x":{"type":"number","exclusiveMinimum":0.5,"maximum":10},
				"y":{"type":"integer","exclusiveMaximum":3}}}`,
		},
		{
			name: "malformed keywords are dropped",
			in: `{"type":"object","properties":{"s":{"type":"string","minLength":-1,"maxLength":"5","pattern":5,"enum":"a","minimum":"0"},
				"t":{"type":"text"},"u":true,"v":false,"w":[1]},"required":["s",1,"s"],"additionalProperties":"no"}`,
			want: `{"type":"object","properties":{"s":{"type":"string"},"t":{},"u":{},"v":{},"w":{}},"required":["s"]}`,
		},
		{
			name: "annotations and extensions are kept",
			in: `{"type":"object","properties":{"p":{"type":"string","format":"uri","title":"P","examples":["x"],"x-order":2,"$comment":"c"}},
				"additionalProperties":false}`,
			want: `{"type":"object","properties":{"p":{"type":"string","format":"uri","title":"P","examples":["x"],"x-order":2,"$comment":"c"}},
				"additionalProperties":false}`,
			accepts: []string{`{"p":"https://example.com"}`, `{}`},
		},
		{name: "empty root becomes an object", in: `{}`, want: `{"type":"object"}`, accepts: []string{`{}`, `{"any":[1]}`}},
		{name: "null root becomes an object", in: `null`, want: `{"type":"object"}`},
		{name: "untyped root with properties", in: `{"properties":{"a":{"type":"string"}}}`, want: `{"type":"object","properties":{"a":{"type":"string"}}}`,
			accepts: []string{`{"a":"x"}`}},
		{name: "nullable root", in: `{"type":["object","null"],"properties":{}}`, want: `{"type":"object","properties":{}}`, accepts: []string{`{}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := lowerSchema(decodeSDK(t, tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if g, w := canonical(t, got), canonical(t, []byte(tc.want)); g != w {
				t.Fatalf("lowered schema\n got: %s\nwant: %s", g, w)
			}
			registerable(t, got)
			for _, args := range tc.accepts {
				originalAccepts(t, tc.in, args)
				coreAccepts(t, got, args)
			}
		})
	}
}

func TestLowerSchemaErrors(t *testing.T) {
	deep := strings.Repeat(`{"type":"array","items":`, maxSchemaDepth+2) + `{}` + strings.Repeat(`}`, maxSchemaDepth+2)
	// Each definition refers to the next twice, so inlining doubles per level.
	var defs []string
	for i := range 20 {
		defs = append(defs, fmt.Sprintf(`"d%d":{"type":"object","properties":{"l":{"$ref":"#/$defs/d%d"},"r":{"$ref":"#/$defs/d%d"}}}`, i, i+1, i+1))
	}
	wide := `{"type":"object","properties":{"root":{"$ref":"#/$defs/d0"}},"$defs":{` + strings.Join(defs, ",") + `}}`

	cases := []struct{ name, in, want string }{
		{"string root", `{"type":"string"}`, "must describe an object"},
		{"array root", `[]`, "must be a JSON object"},
		{"too deep", `{"type":"object","properties":{"p":` + deep + `}}`, "deeper than"},
		{"too wide", wide, "expands to more than"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := lowerSchema(decodeSDK(t, tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}
