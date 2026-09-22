package core

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustCompileSchema(t *testing.T, raw string) schemaContract {
	t.Helper()
	var schema map[string]any
	if err := decodeSchemaRaw(json.RawMessage(raw), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	contract, err := compileSchemaContract(schema)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return contract
}

func firstViolation(violations []string) string {
	if len(violations) == 0 {
		return ""
	}
	return violations[0]
}

// TestCompileSchemaRejectsUnsupportedAssertionKeywords pins that declaration
// fails explicitly for assertion and structural keywords core cannot honor,
// instead of silently weakening validation.
func TestCompileSchemaRejectsUnsupportedAssertionKeywords(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","properties":{"a":{}},"oneOf":[{"type":"string"}]}`,
		`{"type":"object","anyOf":[{"type":"string"}]}`,
		`{"type":"object","allOf":[{"type":"string"}]}`,
		`{"type":"object","not":{}}`,
		`{"type":"object","if":{"type":"string"},"then":{}}`,
		`{"type":"object","$ref":"#/definitions/a"}`,
		`{"type":"object","properties":{"a":{"const":1}}}`,
		`{"type":"object","properties":{"a":{"multipleOf":2}}}`,
		`{"type":"array","items":{"type":"string"},"uniqueItems":true}`,
		`{"type":"array","minItems":1}`,
		`{"type":"object","minProperties":1}`,
		`{"type":"object","properties":{"a":{}},"patternProperties":{"^a":{}}}`,
		`{"type":"array","prefixItems":[{"type":"string"}]}`,
		`{"type":"string","contains":{"type":"string"}}`,
		`{"type":"object","dependentRequired":{"a":["b"]}}`,
		`{"type":"object","properties":{"a":{}},"unevaluatedProperties":false}`,
	} {
		var schema map[string]any
		if err := decodeSchemaRaw(json.RawMessage(raw), &schema); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if _, err := compileSchemaContract(schema); err == nil {
			t.Errorf("schema accepted an unsupported keyword: %s", raw)
		} else if !strings.Contains(err.Error(), "unsupported schema") {
			t.Errorf("error for %s = %v, want explicit unsupported-schema rejection", raw, err)
		}
	}
}

// TestCompileSchemaRejectsUnsupportedTypeUnions pins that only nullable
// ["T", "null"] unions are supported.
func TestCompileSchemaRejectsUnsupportedTypeUnions(t *testing.T) {
	for _, raw := range []string{
		`{"type":["string","number"]}`,
		`{"type":["string","number","null"]}`,
		`{"type":["null"]}`,
		`{"type":["bogus","null"]}`,
		`{"type":5}`,
	} {
		var schema map[string]any
		if err := decodeSchemaRaw(json.RawMessage(raw), &schema); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if _, err := compileSchemaContract(schema); err == nil {
			t.Errorf("schema accepted an unsupported type union: %s", raw)
		}
	}
}

// TestCompileSchemaSupportsNullableUnionAndAnnotations pins the supported
// nullable union plus provider-facing annotation passthrough.
func TestCompileSchemaSupportsNullableUnionAndAnnotations(t *testing.T) {
	contract := mustCompileSchema(t, `{
		"type": ["string", "null"],
		"title": "Summary", "description": "doc", "format": "date-time",
		"strict": true, "$defs": {"x": {"type": "string"}}
	}`)
	if !contract.nullable || contract.typ != "string" {
		t.Fatalf("nullable union contract = %#v", contract)
	}
	if violations := contract.validate(nil, ""); len(violations) != 0 {
		t.Fatalf("null violations = %q", violations)
	}
	if violations := contract.validate(json.Number("5"), ""); len(violations) != 1 {
		t.Fatalf("non-string violations = %q", violations)
	}
	if violations := contract.validate("ok", ""); len(violations) != 0 {
		t.Fatalf("string violations = %q", violations)
	}
}

// TestSchemaContractEnforcedConstraints covers enum, numeric, and string
// constraints through the shared value validator.
func TestSchemaContractEnforcedConstraints(t *testing.T) {
	t.Run("enum", func(t *testing.T) {
		contract := mustCompileSchema(t, `{"type":"string","enum":["a","b"]}`)
		if got := firstViolation(contract.validate("c", "v")); !strings.Contains(got, "v: value not in enum") {
			t.Fatalf("enum violation = %q", got)
		}
		if got := firstViolation(contract.validate("a", "")); got != "" {
			t.Fatalf("valid enum value = %q", got)
		}
		numbers := mustCompileSchema(t, `{"type":"integer","enum":[1,3]}`)
		if got := firstViolation(numbers.validate(json.Number("1.0"), "")); got != "" {
			t.Fatalf("integral enum match = %q", got)
		}
		if got := firstViolation(numbers.validate(json.Number("2"), "")); got == "" {
			t.Fatal("enum mismatch accepted")
		}
	})
	t.Run("numeric bounds", func(t *testing.T) {
		contract := mustCompileSchema(t, `{"type":"number","minimum":0.1,"maximum":2,"exclusiveMaximum":2}`)
		if got := firstViolation(contract.validate(json.Number("0.1"), "")); got != "" {
			t.Fatalf("minimum boundary = %q", got)
		}
		if got := firstViolation(contract.validate(json.Number("0.05"), "v")); !strings.Contains(got, "violates minimum") {
			t.Fatalf("below minimum = %q", got)
		}
		if got := firstViolation(contract.validate(json.Number("2"), "v")); !strings.Contains(got, "violates exclusiveMaximum") {
			t.Fatalf("exclusive maximum boundary = %q", got)
		}
	})
	t.Run("integer exactness", func(t *testing.T) {
		contract := mustCompileSchema(t, `{"type":"integer"}`)
		for _, number := range []json.Number{"1e2", "1.0", "-2.5e3", "9007199254740993.0"} {
			if !isIntegralNumber(number) {
				t.Fatalf("isIntegralNumber(%q) = false, want true", number)
			}
			if got := firstViolation(contract.validate(number, "n")); got != "" {
				t.Fatalf("integer %q rejected: %q", number, got)
			}
		}
		for _, number := range []json.Number{"1.0000000000000001", "1e-1", "9007199254740993.1"} {
			if isIntegralNumber(number) {
				t.Fatalf("isIntegralNumber(%q) = true, want false", number)
			}
			if got := firstViolation(contract.validate(number, "n")); !strings.Contains(got, "non-integral number") {
				t.Fatalf("fractional %q violation = %q", number, got)
			}
		}
		for _, number := range []json.Number{"01", "+1", "1e", "NaN"} {
			if isIntegralNumber(number) {
				t.Fatalf("malformed isIntegralNumber(%q) = true, want false", number)
			}
			if got := firstViolation(contract.validate(number, "n")); !strings.Contains(got, "invalid JSON number") {
				t.Fatalf("malformed %q violation = %q", number, got)
			}
		}
	})
	t.Run("string bounds and pattern", func(t *testing.T) {
		contract := mustCompileSchema(t, `{"type":"string","minLength":2,"maxLength":3,"pattern":"^a"}`)
		if got := firstViolation(contract.validate("a", "v")); !strings.Contains(got, "below minLength") {
			t.Fatalf("min length = %q", got)
		}
		if got := firstViolation(contract.validate("abcd", "v")); !strings.Contains(got, "exceeds maxLength") {
			t.Fatalf("max length = %q", got)
		}
		if got := firstViolation(contract.validate("bbb", "v")); !strings.Contains(got, "does not match pattern") {
			t.Fatalf("pattern = %q", got)
		}
		if got := firstViolation(contract.validate("ab", "")); got != "" {
			t.Fatalf("valid string = %q", got)
		}
	})
	t.Run("invalid declaration", func(t *testing.T) {
		for _, raw := range []string{
			`{"type":"string","pattern":"("}`,
			`{"type":"string","minLength":-1}`,
			`{"type":"object","properties":{"a":"not-a-schema"}}`,
			`{"type":"object","required":[1]}`,
			`{"type":"string","minimum":"low"}`,
		} {
			var schema map[string]any
			if err := decodeSchemaRaw(json.RawMessage(raw), &schema); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
			if _, err := compileSchemaContract(schema); err == nil {
				t.Errorf("invalid schema accepted: %s", raw)
			}
		}
	})
}

// TestSchemaContractAgreesWithTypedValidator pins that the shared contract
// validator and the Go-typed validator agree on the schema a Go type derives:
// same violations (as a set) for the same payloads.
func TestSchemaContractAgreesWithTypedValidator(t *testing.T) {
	type nested struct {
		Topic string `json:"topic"`
	}
	type payload struct {
		Name     string             `json:"name"`
		Age      int                `json:"age"`
		Nick     string             `json:"nick,omitempty"`
		Addr     *nested            `json:"addr"`
		OptAddr  *nested            `json:"optAddr,omitempty"`
		Tags     []string           `json:"tags"`
		Scores   []nested           `json:"scores"`
		Meta     map[string]float64 `json:"meta"`
		Raw      json.RawMessage    `json:"raw"`
		Any      any                `json:"any,omitempty"`
		Blob     []byte             `json:"blob"`
		Node     *payload           `json:"node,omitempty"`
		When     time.Time          `json:"when"`
		Explicit string             `json:"-"`
	}

	schemaRaw, err := json.Marshal(objectSchema(reflect.TypeOf(payload{}), map[reflect.Type]bool{}))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := decodeSchemaRaw(schemaRaw, &schema); err != nil {
		t.Fatal(err)
	}
	contract, err := compileSchemaContract(schema)
	if err != nil {
		t.Fatalf("derived schema rejected by the shared contract: %v", err)
	}

	derived := typeContract(reflect.TypeOf(payload{}), map[reflect.Type]bool{})
	// Parity corpus excludes null values: nullability in the Go-typed path
	// follows the Go kind (pointer, slice, map, and []byte fields accept null),
	// while some raw-schema shapes cannot express those Go-specific rules.
	// TestSchemaContractNullableGoKinds pins the Go-kind semantics separately.
	payloads := []string{
		`{"name":"Ada","age":36,"addr":{"topic":"x"},"tags":["a"],"scores":[{"topic":"y"}],
		  "meta":{"n":1.5},"raw":{"k":1},"any":"w","when":"2024-01-02T03:04:05Z","blob":"aGk="}`,
		`{}`,
		`{"name":5,"age":"x","addr":"y","tags":"z","scores":[{}],"meta":{"a":"s"},"when":5,"blob":5}`,
		`{"name":"a","age":1.5,"scores":[{"topic":7}],"meta":{"z":"a"}}`,
		`[1,2]`,
	}
	for _, raw := range payloads {
		typed := validateTypedPayload[payload](json.RawMessage(raw))
		mapSide := contract.validate(decodePayload(t, raw), "")
		goSide := derived.validate(decodePayload(t, raw), "")
		if !equalViolationSets(typed, mapSide) {
			t.Errorf("payload %s: typed %q != contract %q", raw, typed, mapSide)
		}
		if !equalViolationSets(goSide, typed) {
			t.Errorf("payload %s: derived %q != typed %q", raw, goSide, typed)
		}
	}
}

// TestSchemaContractNullableGoKinds pins that Go-kind nullability in the
// typed path is unchanged by the shared contract: pointer, slice, map, and
// []byte fields accept null; non-pointer scalar and interface fields do not.
func TestSchemaContractNullableGoKinds(t *testing.T) {
	type payload struct {
		Addr   *struct{ Topic string } `json:"addr"`
		Title  *string                 `json:"title"`
		Count  *int                    `json:"count"`
		Flag   *bool                   `json:"flag"`
		Score  *float64                `json:"score"`
		PtrAny *any                    `json:"ptrAny"`
		Tags   []string                `json:"tags"`
		Meta   map[string]string       `json:"meta"`
		Blob   []byte                  `json:"blob"`
		Any    any                     `json:"any"`
		Name   string                  `json:"name"`
	}
	got := validateTypedPayload[payload](json.RawMessage(`{"addr":null,"title":null,"count":null,"flag":null,"score":null,"ptrAny":null,"tags":null,"meta":null,"blob":null,"any":null}`))
	if len(got) != 2 || got[0] != "any: null is not a valid value for any" || got[1] != "name: missing required field" {
		t.Fatalf("nullable go kinds = %q", got)
	}
}

func equalViolationSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int)
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		seen[v]--
		if seen[v] < 0 {
			return false
		}
	}
	return true
}

func decodePayload(t *testing.T, raw string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		t.Fatalf("decode payload %s: %v", raw, err)
	}
	return value
}

// TestSchemaContractNullTypeEnforced pins that a {"type":"null"} schema
// accepts only JSON null, restoring the pre-T06 enforcement.
func TestSchemaContractNullTypeEnforced(t *testing.T) {
	contract := mustCompileSchema(t, `{"type":"null"}`)
	if !contract.nullable || contract.typ != "null" {
		t.Fatalf("null-type contract = %#v", contract)
	}
	if violations := contract.validate(nil, ""); len(violations) != 0 {
		t.Fatalf("null value violations = %q", violations)
	}
	for _, value := range []any{json.Number("5"), "s", true, map[string]any{}, []any{}} {
		if got := firstViolation(contract.validate(value, "x")); !strings.Contains(got, "expected null") {
			t.Fatalf("non-null value %#v accepted: %q", value, got)
		}
	}

	// Through a tool-arguments property: a non-null argument must be
	// rejected before the tool executes.
	if err := validateToolArguments(
		json.RawMessage(`{"type":"object","properties":{"x":{"type":"null"}}}`),
		json.RawMessage(`{"x":5}`)); err == nil {
		t.Fatal("tool arguments accepted a non-null value for a null-typed property")
	}
	if err := validateToolArguments(
		json.RawMessage(`{"type":"object","properties":{"x":{"type":"null"}}}`),
		json.RawMessage(`{"x":null}`)); err != nil {
		t.Fatalf("tool arguments rejected null: %v", err)
	}
}

// TestSchemaContractAdditionalPropertiesFalse pins that boolean
// additionalProperties:false rejects unknown fields on raw declared schemas
// (tool inputs and structured outputs), restoring the pre-T06 enforcement.
func TestSchemaContractAdditionalPropertiesFalse(t *testing.T) {
	contract := mustCompileSchema(t,
		`{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`)
	if got := firstViolation(contract.validate(map[string]any{"path": "a", "extra": json.Number("1")}, "arguments")); got != "arguments.extra: unknown field" {
		t.Fatalf("unknown-field violation = %q", got)
	}
	if got := firstViolation(contract.validate(map[string]any{"path": "a"}, "")); got != "" {
		t.Fatalf("known fields rejected: %q", got)
	}
	// additionalProperties as a schema still validates unknown values.
	permissive := mustCompileSchema(t,
		`{"type":"object","properties":{"path":{}},"additionalProperties":{"type":"string"}}`)
	if got := firstViolation(permissive.validate(map[string]any{"path": "a", "n": json.Number("1")}, "")); got == "" {
		t.Fatal("additionalProperties schema form no longer validates unknown values")
	}
	// Without the keyword, unknown fields stay ignored (json.Unmarshal parity).
	tolerant := mustCompileSchema(t, `{"type":"object","properties":{"path":{"type":"string"}}}`)
	if got := firstViolation(tolerant.validate(map[string]any{"path": "a", "extra": json.Number("1")}, "")); got != "" {
		t.Fatalf("unknown field rejected without additionalProperties:false: %q", got)
	}

	// Through the tool-arguments path.
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`)
	if err := validateToolArguments(schema, json.RawMessage(`{"path":"a","extra":1}`)); err == nil {
		t.Fatal("tool arguments accepted an unknown field under additionalProperties:false")
	}
	if err := validateToolArguments(schema, json.RawMessage(`{"path":"a"}`)); err != nil {
		t.Fatalf("tool arguments rejected known fields: %v", err)
	}
}

// TestSchemaContractRequiredReportedOnce pins exactly one violation per
// missing required name, whether or not the properties map describes it.
func TestSchemaContractRequiredReportedOnce(t *testing.T) {
	contract := mustCompileSchema(t, `{"type":"object",
		"properties":{"a":{"type":"string"}},
		"required":["a","b"]}`)
	got := contract.validate(map[string]any{}, "out")
	if len(got) != 2 || got[0] != "out.a: missing required field" || got[1] != "out.b: missing required field" {
		t.Fatalf("missing-required violations = %q", got)
	}
	// A present-but-invalid required property reports only its own violation.
	got = contract.validate(map[string]any{"a": json.Number("5"), "b": "x"}, "out")
	if len(got) != 1 || got[0] != "out.a: expected string, got number" {
		t.Fatalf("required-with-type-violation = %q", got)
	}
	if got := contract.validate(map[string]any{"a": "x", "b": nil}, ""); len(got) != 0 {
		t.Fatalf("valid payload rejected: %q", got)
	}
}

// TestDecodeSchemaRawRejectsTrailingGarbage pins that raw schemas contain
// exactly one top-level JSON value; a second Decode must reach io.EOF.
func TestDecodeSchemaRawRejectsTrailingGarbage(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object"} {"a":1}`,
		`{"type":"object"} garbage`,
		`{"type":"object"}[]`,
		`{"type":"object"}
		5`,
	} {
		var schema map[string]any
		if err := decodeSchemaRaw(json.RawMessage(raw), &schema); err == nil {
			t.Errorf("schema accepted trailing content: %q", raw)
		}
	}
	var schema map[string]any
	if err := decodeSchemaRaw(json.RawMessage(`{"type":"object"}`), &schema); err != nil {
		t.Fatalf("single value rejected: %v", err)
	}
}

// TestDecodeSingleJSONValue pins the one-top-level-value rule on the
// tool-argument path and on the declared final-output (terminal payload)
// path.
func TestDecodeSingleJSONValue(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	for _, input := range []string{
		`{"path":"a"} {"path":"b"}`,
		`{"path":"a"} garbage`,
		`{"path":"a"}[]`,
	} {
		if err := validateToolArguments(schema, json.RawMessage(input)); err == nil {
			t.Errorf("tool arguments accepted trailing content: %q", input)
		}
	}
	if err := validateToolArguments(schema, json.RawMessage(`{"path":"a"}`)); err != nil {
		t.Fatalf("valid tool arguments rejected: %v", err)
	}

	declared, err := validateStructuredOutputDeclaration(&StructuredOutputConfig{
		Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	})
	if err != nil {
		t.Fatalf("declared schema rejected: %v", err)
	}
	state := &structuredOutputState{contract: declared.contract}
	for _, payload := range []string{
		`{"path":"a"} {"path":"b"}`,
		`{"path":"a"} garbage`,
	} {
		_, violations := state.validate(json.RawMessage(payload))
		if len(violations) != 1 || !strings.Contains(violations[0], "exactly one JSON value") {
			t.Errorf("terminal payload %q violations = %q, want one-JSON-value rejection", payload, violations)
		}
	}
	if _, violations := state.validate(json.RawMessage(`{"path":"a"}`)); len(violations) != 0 {
		t.Fatalf("valid terminal payload rejected: %q", violations)
	}
}

// TestRegisterToolsSchemaEnforcement pins the schema contract at tool
// registration: a schema with trailing garbage fails registration, and a
// supported schema with additionalProperties:false registers so per-call
// validation can enforce it.
func TestRegisterToolsSchemaEnforcement(t *testing.T) {
	_, _, err := registerTools([]Tool{
		&definitionTool{definition: ToolDefinition{
			Name: "t", Description: "d",
			InputSchema: json.RawMessage(`{"type":"object"} garbage`),
		}},
	}, "")
	if err == nil {
		t.Fatal("registration accepted a schema with trailing garbage")
	}
	if !strings.Contains(err.Error(), "exactly one JSON value") {
		t.Fatalf("registration error = %v, want one-JSON-value rejection", err)
	}
	registry, defs, err := registerTools([]Tool{
		&definitionTool{definition: ToolDefinition{
			Name: "t", Description: "d",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`),
		}},
	}, "")
	if err != nil {
		t.Fatalf("additionalProperties:false schema rejected at registration: %v", err)
	}
	if len(defs) != 1 || len(registry) != 1 {
		t.Fatalf("registration result = %d defs, %d registry", len(defs), len(registry))
	}
}
