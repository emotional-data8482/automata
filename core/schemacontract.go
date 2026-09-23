package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
)

// This file is the one supported schema contract shared by every declared
// JSON-schema shape in core: tool input schemas, CallOptions.OutputSchema,
// the final-output schema of typed runs, and structured child values. It
// defines which keywords core enforces, which annotation keywords pass
// through to providers untouched, and which unsupported assertion keywords
// are rejected explicitly instead of being silently weakened.
//
// Enforced: type (single, or ["T", "null"] nullable unions), enum,
// properties, required, items, additionalProperties, minimum, maximum,
// exclusiveMinimum, exclusiveMaximum, minLength, maxLength, pattern.
//
// Rejected explicitly: assertion and structural keywords core cannot honor
// (oneOf, anyOf, allOf, not, if/then/else, $ref, prefixItems, contains,
// patternProperties, propertyNames, dependent*, unevaluated*, const,
// multipleOf, uniqueItems, minItems, maxItems, minProperties, maxProperties,
// dependencies).
//
// Everything else (title, description, format, default, examples, $defs,
// provider-native options such as OpenAI's strict) is preserved verbatim as
// provider-facing metadata and is not enforced locally.

// schemaContract is a compiled supported-subset schema. Zero typ means any
// value is accepted shape-wise; nullable reports whether JSON null is valid.
type schemaContract struct {
	typ      string
	display  string // name used in violation messages for null/type failures
	nullable bool
	// rejectUnknown is set only by the raw-schema form of
	// additionalProperties:false. Go-derived contracts never set it, so
	// unknown JSON fields are still ignored on the typed path, matching
	// json.Unmarshal.
	rejectUnknown bool

	enum []any

	minimum          *json.Number
	maximum          *json.Number
	exclusiveMinimum *json.Number
	exclusiveMaximum *json.Number

	minLength *int
	maxLength *int
	pattern   *regexp.Regexp

	properties []contractProperty // deterministic order: field order or sorted names
	required   []string
	items      *schemaContract
	additional *schemaContract // nil means unknown fields are ignored
}

type contractProperty struct {
	name     string
	contract schemaContract
	required bool
}

// schemaUnsupportedKeywords are assertion or structural keywords core cannot
// honor. Declaring one fails compilation instead of silently weakening
// validation.
var schemaUnsupportedKeywords = map[string]bool{
	"const": true, "oneOf": true, "anyOf": true, "allOf": true, "not": true,
	"if": true, "then": true, "else": true, "$ref": true, "$dynamicRef": true,
	"prefixItems": true, "contains": true, "patternProperties": true,
	"propertyNames": true, "dependentRequired": true, "dependentSchemas": true,
	"unevaluatedProperties": true, "unevaluatedItems": true, "multipleOf": true,
	"uniqueItems": true, "minItems": true, "maxItems": true,
	"minProperties": true, "maxProperties": true, "dependencies": true,
}

// supportedSchemaTypes are the primitive type strings core enforces.
var supportedSchemaTypes = map[string]bool{
	"object": true, "array": true, "string": true, "number": true,
	"integer": true, "boolean": true, "null": true,
}

var schemaJSONNumberLiteral = regexp.MustCompile(`^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$`)

// compileSchemaContract validates a decoded schema against the supported
// subset and compiles it for value validation. Schemas must be decoded with
// json.Number preservation (UseNumber) so numeric assertions keep their exact
// literals.
func compileSchemaContract(s map[string]any) (schemaContract, error) {
	return compileSchemaNode(s)
}

func compileSchemaNode(s map[string]any) (schemaContract, error) {
	if s == nil {
		return schemaContract{}, nil
	}
	// Compile-time rejection: an unsupported assertion keyword must fail
	// declaration, not weaken validation.
	for keyword := range s {
		if schemaUnsupportedKeywords[keyword] {
			return schemaContract{}, fmt.Errorf("unsupported schema keyword %q", keyword)
		}
	}

	c := schemaContract{}
	if raw, ok := s["type"]; ok {
		typ, nullable, err := schemaTypeOf(raw)
		if err != nil {
			return schemaContract{}, err
		}
		c.typ, c.nullable = typ, nullable
	} else {
		// A schema without a type constrains nothing; null is valid.
		c.nullable = true
	}
	c.display = c.typ
	if c.typ == "" {
		c.display = "any"
	}

	if raw, ok := s["enum"]; ok {
		values, ok := raw.([]any)
		if !ok {
			return schemaContract{}, errors.New("enum must be an array")
		}
		c.enum = values
	}
	var err error
	if c.minimum, err = schemaNumberKeyword(s, "minimum"); err != nil {
		return schemaContract{}, err
	}
	if c.maximum, err = schemaNumberKeyword(s, "maximum"); err != nil {
		return schemaContract{}, err
	}
	if c.exclusiveMinimum, err = schemaNumberKeyword(s, "exclusiveMinimum"); err != nil {
		return schemaContract{}, err
	}
	if c.exclusiveMaximum, err = schemaNumberKeyword(s, "exclusiveMaximum"); err != nil {
		return schemaContract{}, err
	}
	if c.minLength, c.maxLength, err = schemaLengthKeywords(s); err != nil {
		return schemaContract{}, err
	}
	if raw, ok := s["pattern"]; ok {
		text, ok := raw.(string)
		if !ok {
			return schemaContract{}, errors.New("pattern must be a string")
		}
		expr, err := regexp.Compile(text)
		if err != nil {
			return schemaContract{}, fmt.Errorf("pattern %q: %w", text, err)
		}
		c.pattern = expr
	}

	if raw, ok := s["properties"]; ok {
		props, ok := raw.(map[string]any)
		if !ok {
			return schemaContract{}, errors.New("properties must be an object")
		}
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child, ok := props[name].(map[string]any)
			if !ok {
				return schemaContract{}, fmt.Errorf("property %q must be a schema", name)
			}
			compiled, err := compileSchemaNode(child)
			if err != nil {
				return schemaContract{}, fmt.Errorf("property %q: %w", name, err)
			}
			c.properties = append(c.properties, contractProperty{name: name, contract: compiled})
		}
	}
	if raw, ok := s["required"]; ok {
		names, ok := raw.([]any)
		if !ok {
			return schemaContract{}, errors.New("required must be an array")
		}
		for _, n := range names {
			name, ok := n.(string)
			if !ok {
				return schemaContract{}, errors.New("required entries must be strings")
			}
			c.required = append(c.required, name)
		}
	}
	// Described required names are enforced through their property entries so
	// each missing required name is reported exactly once; the validateObject
	// required loop then covers only names the properties map does not
	// describe.
	if len(c.required) > 0 {
		reqSet := make(map[string]bool, len(c.required))
		for _, name := range c.required {
			reqSet[name] = true
		}
		for i := range c.properties {
			c.properties[i].required = reqSet[c.properties[i].name]
		}
	}
	if raw, ok := s["items"]; ok {
		child, ok := raw.(map[string]any)
		if !ok {
			return schemaContract{}, errors.New("items must be a schema")
		}
		compiled, err := compileSchemaNode(child)
		if err != nil {
			return schemaContract{}, fmt.Errorf("items: %w", err)
		}
		c.items = &compiled
	}
	if raw, ok := s["additionalProperties"]; ok {
		switch v := raw.(type) {
		case bool:
			if !v {
				// Boolean false restores unknown-field rejection for raw
				// declared schemas (tool inputs and structured outputs).
				c.rejectUnknown = true
			}
		case map[string]any:
			compiled, err := compileSchemaNode(v)
			if err != nil {
				return schemaContract{}, fmt.Errorf("additionalProperties: %w", err)
			}
			c.additional = &compiled
		default:
			return schemaContract{}, errors.New("additionalProperties must be a boolean or schema")
		}
	}
	return c, nil
}

// schemaTypeOf compiles the "type" keyword: a single supported type string,
// or a ["T", "null"] union (nullable T). Other unions are rejected explicitly.
func schemaTypeOf(raw any) (string, bool, error) {
	switch typ := raw.(type) {
	case string:
		if !supportedSchemaTypes[typ] {
			return "", false, fmt.Errorf("unsupported schema type %q", typ)
		}
		if typ == "null" {
			return "null", true, nil
		}
		return typ, false, nil
	case []any:
		if len(typ) != 2 {
			return "", false, fmt.Errorf("unsupported schema type union %v", raw)
		}
		first, ok := typ[0].(string)
		if !ok || !supportedSchemaTypes[first] || first == "null" {
			return "", false, fmt.Errorf("unsupported schema type union %v", raw)
		}
		second, ok := typ[1].(string)
		if !ok || second != "null" {
			return "", false, fmt.Errorf("unsupported schema type union %v", raw)
		}
		return first, true, nil
	default:
		return "", false, fmt.Errorf("unsupported schema type %v", raw)
	}
}

func schemaNumberKeyword(s map[string]any, keyword string) (*json.Number, error) {
	raw, ok := s[keyword]
	if !ok {
		return nil, nil
	}
	number, ok := raw.(json.Number)
	if !ok {
		return nil, fmt.Errorf("%s must be a number", keyword)
	}
	if _, ok := schemaNumberRat(number); !ok {
		return nil, fmt.Errorf("%s must be a valid JSON number", keyword)
	}
	return &number, nil
}

func schemaLengthKeywords(s map[string]any) (*int, *int, error) {
	var minLength, maxLength *int
	if raw, ok := s["minLength"]; ok {
		n, err := schemaIntKeyword(raw, "minLength")
		if err != nil {
			return nil, nil, err
		}
		minLength = &n
	}
	if raw, ok := s["maxLength"]; ok {
		n, err := schemaIntKeyword(raw, "maxLength")
		if err != nil {
			return nil, nil, err
		}
		maxLength = &n
	}
	return minLength, maxLength, nil
}

func schemaIntKeyword(raw any, keyword string) (int, error) {
	number, ok := raw.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%s must be a number", keyword)
	}
	n, err := strconv.Atoi(number.String())
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", keyword)
	}
	return n, nil
}

// violation builds one path-qualified violation message.
func (c *schemaContract) violation(path, format string, args ...any) string {
	msg := fmt.Sprintf(format, args...)
	if path != "" {
		msg = path + ": " + msg
	}
	return msg
}

// validate checks one decoded JSON value against the contract and returns
// path-qualified violations in deterministic order. An empty slice means the
// value satisfies the contract.
func (c *schemaContract) validate(val any, path string) []string {
	if val == nil {
		if !c.nullable {
			return []string{c.violation(path, "null is not a valid value for %s", c.display)}
		}
		return c.checkEnum(val, path)
	}
	switch c.typ {
	case "":
		return c.checkEnum(val, path) // shape-free schema: any value is accepted
	case "object":
		obj, ok := val.(map[string]any)
		if !ok {
			return []string{c.violation(path, "expected object, got %s", jsonKindOf(val))}
		}
		violations := c.validateObject(obj, path)
		return append(violations, c.checkEnum(val, path)...)
	case "array":
		items, ok := val.([]any)
		if !ok {
			return []string{c.violation(path, "expected array, got %s", jsonKindOf(val))}
		}
		var violations []string
		if c.items != nil {
			for i, item := range items {
				violations = append(violations, c.items.validate(item, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
		return append(violations, c.checkEnum(val, path)...)
	case "string":
		text, ok := val.(string)
		if !ok {
			return []string{c.violation(path, "expected string, got %s", jsonKindOf(val))}
		}
		var violations []string
		if c.minLength != nil && len([]rune(text)) < *c.minLength {
			violations = append(violations, c.violation(path,
				"string length %d is below minLength %d", len([]rune(text)), *c.minLength))
		}
		if c.maxLength != nil && len([]rune(text)) > *c.maxLength {
			violations = append(violations, c.violation(path,
				"string length %d exceeds maxLength %d", len([]rune(text)), *c.maxLength))
		}
		if c.pattern != nil && !c.pattern.MatchString(text) {
			violations = append(violations, c.violation(path,
				"string does not match pattern %q", c.pattern.String()))
		}
		return append(violations, c.checkEnum(val, path)...)
	case "boolean":
		if _, ok := val.(bool); !ok {
			return []string{c.violation(path, "expected boolean, got %s", jsonKindOf(val))}
		}
		return c.checkEnum(val, path)
	case "number", "integer":
		number, ok := val.(json.Number)
		if !ok {
			return []string{c.violation(path, "expected %s, got %s", c.typ, jsonKindOf(val))}
		}
		value, ok := schemaNumberRat(number)
		if !ok {
			return []string{c.violation(path, "invalid JSON number %q", number.String())}
		}
		if c.typ == "integer" && !value.IsInt() {
			return []string{c.violation(path, "expected integer, got non-integral number %s", number)}
		}
		for _, bound := range []struct {
			limit     *json.Number
			belowOK   bool // false for minimum-style bounds, true for maximum-style
			exclusive bool
			name      string
		}{
			{c.minimum, false, false, "minimum"},
			{c.maximum, true, false, "maximum"},
			{c.exclusiveMinimum, false, true, "exclusiveMinimum"},
			{c.exclusiveMaximum, true, true, "exclusiveMaximum"},
		} {
			if bound.limit == nil {
				continue
			}
			limit, ok := schemaNumberRat(*bound.limit)
			if !ok {
				return []string{c.violation(path, "schema %s is not a valid JSON number", bound.name)}
			}
			cmp := value.Cmp(limit)
			if bound.belowOK {
				// Upper bound: a violation is value > limit (or >= when exclusive).
				if cmp > 0 || (bound.exclusive && cmp == 0) {
					return []string{c.violation(path, "number %s violates %s %s", number, bound.name, bound.limit)}
				}
				continue
			}
			// Lower bound: a violation is value < limit (or <= when exclusive).
			if cmp < 0 || (bound.exclusive && cmp == 0) {
				return []string{c.violation(path, "number %s violates %s %s", number, bound.name, bound.limit)}
			}
		}
		return c.checkEnum(val, path)
	case "null":
		return []string{c.violation(path, "expected null, got %s", jsonKindOf(val))}
	}
	return nil
}

// checkEnum applies the enum keyword to an already shape-valid value.
func (c *schemaContract) checkEnum(val any, path string) []string {
	if len(c.enum) == 0 {
		return nil
	}
	for _, allowed := range c.enum {
		if jsonValuesEqual(val, allowed) {
			return nil
		}
	}
	return []string{c.violation(path, "value not in enum")}
}

func (c *schemaContract) validateObject(obj map[string]any, path string) []string {
	var violations []string
	described := make(map[string]bool, len(c.properties))
	for _, prop := range c.properties {
		described[prop.name] = true
		value, present := obj[prop.name]
		if !present {
			if prop.required {
				violations = append(violations, c.violation(joinPath(path, prop.name), "missing required field"))
			}
			continue
		}
		violations = append(violations, prop.contract.validate(value, joinPath(path, prop.name))...)
	}
	// Required names without a matching property entry (schema-level required
	// of fields the properties map does not describe) are checked in schema
	// order after the described fields; described names were already enforced
	// by their property entries above.
	for _, name := range c.required {
		if described[name] {
			continue
		}
		if _, ok := obj[name]; !ok {
			violations = append(violations, c.violation(joinPath(path, name), "missing required field"))
		}
	}
	if c.additional != nil || c.rejectUnknown {
		known := make(map[string]bool, len(c.properties))
		for _, prop := range c.properties {
			known[prop.name] = true
		}
		keys := make([]string, 0, len(obj))
		for key := range obj {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if known[key] {
				continue
			}
			if c.rejectUnknown {
				violations = append(violations, c.violation(joinPath(path, key), "unknown field"))
				continue
			}
			violations = append(violations, c.additional.validate(obj[key], joinPath(path, key))...)
		}
	}
	return violations
}

// jsonKindOf names the JSON kind of a decoded value for violation messages.
func jsonKindOf(val any) string {
	switch val.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	default:
		return fmt.Sprintf("%T", val)
	}
}

// joinPath qualifies a violation path under its parent.
func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// schemaNumberRat parses a JSON number literal exactly.
func schemaNumberRat(n json.Number) (*big.Rat, bool) {
	literal := n.String()
	if !schemaJSONNumberLiteral.MatchString(literal) {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(literal)
	return r, ok
}

// jsonValuesEqual compares two decoded JSON values with exact number
// literals, used for enum matching.
func jsonValuesEqual(a, b any) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case json.Number:
		b, ok := b.(json.Number)
		if !ok {
			return false
		}
		ar, ok := schemaNumberRat(a)
		if !ok {
			return false
		}
		br, ok := schemaNumberRat(b)
		return ok && ar.Cmp(br) == 0
	case string:
		b, ok := b.(string)
		return ok && a == b
	case bool:
		b, ok := b.(bool)
		return ok && a == b
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !jsonValuesEqual(a[i], b[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for key, value := range a {
			other, ok := b[key]
			if !ok || !jsonValuesEqual(value, other) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
