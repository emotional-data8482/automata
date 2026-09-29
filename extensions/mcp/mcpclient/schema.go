package mcpclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Bounds on a lowered schema. $ref inlining can multiply a schema's size, so
// both are enforced after expansion.
const (
	maxSchemaDepth = 64
	maxSchemaNodes = 10_000
)

// droppedKeywords are removed after their exact rewrites are applied. The
// assertion and structural keywords mirror what core's schema contract
// rejects; the identity keywords are dropped because a rewritten schema no
// longer describes its source dialect or document.
var droppedKeywords = map[string]bool{
	"const": true, "oneOf": true, "anyOf": true, "allOf": true, "not": true,
	"if": true, "then": true, "else": true, "$ref": true, "$dynamicRef": true,
	"prefixItems": true, "contains": true, "patternProperties": true,
	"propertyNames": true, "dependentRequired": true, "dependentSchemas": true,
	"unevaluatedProperties": true, "unevaluatedItems": true, "multipleOf": true,
	"uniqueItems": true, "minItems": true, "maxItems": true,
	"minProperties": true, "maxProperties": true, "dependencies": true,

	"$schema": true, "$id": true, "$defs": true, "definitions": true,
	"$anchor": true, "$dynamicAnchor": true, "$recursiveRef": true, "$recursiveAnchor": true,
}

var schemaTypes = map[string]bool{
	"object": true, "array": true, "string": true, "number": true,
	"integer": true, "boolean": true, "null": true,
}

// lowerSchema rewrites an MCP input schema into the JSON Schema subset core
// enforces. See the package documentation for the rewrite rules.
func lowerSchema(schema any) (json.RawMessage, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	// Decode with json.Number so numeric keywords keep their literals.
	var root any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	if root == nil {
		root = map[string]any{}
	}
	document, ok := root.(map[string]any)
	if !ok {
		return nil, errors.New("input schema must be a JSON object")
	}
	l := &lowering{root: document, resolving: map[string]bool{"#": true}}
	out := l.node(document, 0)
	if l.err != nil {
		return nil, l.err
	}
	switch typ := out["type"].(type) {
	case nil:
		out["type"] = "object"
	case string:
		if typ != "object" {
			return nil, fmt.Errorf("input schema must describe an object, not %q", typ)
		}
	case []any:
		// MCP arguments are always an object, so a nullable root is not.
		out["type"] = "object"
	}
	return json.Marshal(out)
}

type lowering struct {
	root      map[string]any
	resolving map[string]bool // $ref targets being expanded, to cut cycles
	nodes     int
	err       error
}

// node lowers one schema. Boolean and malformed schemas lower to the
// unconstrained schema {}.
func (l *lowering) node(value any, depth int) map[string]any {
	out := map[string]any{}
	schema, ok := value.(map[string]any)
	if !ok || l.err != nil {
		return out
	}
	if l.nodes++; l.nodes > maxSchemaNodes {
		l.err = fmt.Errorf("input schema expands to more than %d schemas", maxSchemaNodes)
		return out
	}
	if depth > maxSchemaDepth {
		l.err = fmt.Errorf("input schema nests deeper than %d levels", maxSchemaDepth)
		return out
	}

	for key, v := range schema {
		if droppedKeywords[key] {
			continue
		}
		switch key {
		case "type", "exclusiveMinimum", "exclusiveMaximum":
			// Normalized below, once the numeric bounds are known.
		case "properties":
			if props, ok := v.(map[string]any); ok {
				lowered := make(map[string]any, len(props))
				for name, prop := range props {
					lowered[name] = l.node(prop, depth+1)
				}
				out[key] = lowered
			}
		case "items":
			if _, ok := v.(map[string]any); ok {
				out[key] = l.node(v, depth+1)
			}
		case "additionalProperties":
			switch v.(type) {
			case bool:
				out[key] = v
			case map[string]any:
				out[key] = l.node(v, depth+1)
			}
		case "required":
			if names, ok := v.([]any); ok {
				out[key] = stringsOnly(names)
			}
		case "enum":
			if _, ok := v.([]any); ok {
				out[key] = v
			}
		case "minimum", "maximum":
			if _, ok := v.(json.Number); ok {
				out[key] = v
			}
		case "minLength", "maxLength":
			if n, ok := v.(json.Number); ok {
				if length, err := strconv.Atoi(n.String()); err == nil && length >= 0 {
					out[key] = v
				}
			}
		case "pattern":
			// core compiles patterns with Go's RE2 syntax; ECMA-only
			// constructs such as lookahead cannot be enforced locally.
			if text, ok := v.(string); ok {
				if _, err := regexp.Compile(text); err == nil {
					out[key] = v
				}
			}
		default:
			out[key] = v // annotation or extension keyword, kept verbatim
		}
	}
	// Dropping these would widen what a kept keyword constrains:
	// additionalProperties skips keys that patternProperties matches, and
	// items skips the positions prefixItems covers.
	if _, ok := schema["patternProperties"]; ok {
		delete(out, "additionalProperties")
	}
	if _, ok := schema["prefixItems"]; ok {
		delete(out, "items")
	}
	l.exclusiveBound(schema, out, "exclusiveMinimum", "minimum")
	l.exclusiveBound(schema, out, "exclusiveMaximum", "maximum")
	nullable := normalizeType(schema["type"], out)
	if value, ok := schema["const"]; ok {
		if _, hasEnum := out["enum"]; !hasEnum {
			out["enum"] = []any{value}
		}
	}

	// Conjunctions: a $ref target and allOf branches all apply, so merging
	// them (keeping the first value on conflict) only ever relaxes.
	if ref, ok := schema["$ref"].(string); ok {
		if target, ok := l.resolve(ref); ok && !l.resolving[ref] {
			l.resolving[ref] = true
			mergeSchema(out, l.node(target, depth+1))
			delete(l.resolving, ref)
		}
	}
	if branches, ok := schema["allOf"].([]any); ok {
		for _, branch := range branches {
			mergeSchema(out, l.node(branch, depth+1))
		}
	}
	unionNullable := false
	for _, key := range []string{"anyOf", "oneOf"} {
		if variants, ok := schema[key].([]any); ok {
			unionNullable = l.union(variants, out, depth) || unionNullable
		}
	}
	if unionNullable {
		// A null variant admits null whatever enum the other variants carry.
		if enum, ok := out["enum"].([]any); ok && !containsValue(enum, nil) {
			out["enum"] = append(slices.Clone(enum), nil)
		}
	}

	if typ, ok := out["type"].(string); ok && (nullable || unionNullable) && typ != "null" {
		out["type"] = []any{typ, "null"}
	}
	return out
}

// union lowers an anyOf/oneOf. A single non-null variant is exact: it merges
// in, and a null variant makes the result nullable. Several variants that
// share one type keep only that type (object variants also offer their
// properties, none required); otherwise the union is dropped.
func (l *lowering) union(variants []any, out map[string]any, depth int) (nullable bool) {
	var options []map[string]any
	for _, variant := range variants {
		lowered := l.node(variant, depth+1)
		switch typ := lowered["type"].(type) {
		case string:
			if typ == "null" {
				nullable = true
				continue
			}
		case []any:
			nullable = true
			lowered["type"] = typ[0]
		}
		options = append(options, lowered)
	}
	switch len(options) {
	case 0:
		if _, ok := out["type"]; !ok && nullable {
			out["type"] = "null"
		}
		return false
	case 1:
		mergeSchema(out, options[0])
		return nullable
	}
	shared, _ := options[0]["type"].(string)
	for _, option := range options[1:] {
		if option["type"] != shared {
			return nullable
		}
	}
	if shared == "" {
		return nullable
	}
	if _, ok := out["type"]; !ok {
		out["type"] = shared
	}
	if shared == "object" {
		for _, option := range options {
			if props, ok := option["properties"]; ok {
				mergeSchema(out, map[string]any{"properties": props})
			}
		}
	}
	return nullable
}

// exclusiveBound keeps a numeric exclusive bound and converts the draft-04
// boolean form (exclusiveMinimum: true beside minimum) to it.
func (l *lowering) exclusiveBound(schema, out map[string]any, exclusive, inclusive string) {
	switch v := schema[exclusive].(type) {
	case json.Number:
		out[exclusive] = v
	case bool:
		if bound, ok := out[inclusive]; ok && v {
			out[exclusive] = bound
			delete(out, inclusive)
		}
	}
}

// resolve follows a local JSON pointer ("#/$defs/name") within the document.
// Remote references are not fetched; they lower to an unconstrained schema.
func (l *lowering) resolve(ref string) (any, bool) {
	if ref == "#" {
		return l.root, true
	}
	pointer, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil, false
	}
	var current any = l.root
	for _, token := range strings.Split(pointer, "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = object[token]; !ok {
			return nil, false
		}
	}
	return current, true
}

// normalizeType writes the supported form of a type keyword to out and
// reports whether it admitted null. Unsupported unions are dropped.
func normalizeType(raw any, out map[string]any) (nullable bool) {
	switch typ := raw.(type) {
	case string:
		if schemaTypes[typ] {
			out["type"] = typ
		}
		return false
	case []any:
		var kinds []string
		for _, entry := range typ {
			name, ok := entry.(string)
			switch {
			case !ok || !schemaTypes[name]:
				return false
			case name == "null":
				nullable = true
			default:
				kinds = append(kinds, name)
			}
		}
		switch {
		case len(kinds) == 1:
			out["type"] = kinds[0]
		case len(kinds) == 0 && nullable:
			out["type"] = "null"
			return false
		}
		return nullable && len(kinds) == 1
	}
	return false
}

// mergeSchema folds src into dst as a conjunction. dst keeps its own value
// for a conflicting keyword; properties and required accumulate.
func mergeSchema(dst, src map[string]any) {
	for key, value := range src {
		existing, present := dst[key]
		switch {
		case !present:
			dst[key] = value
		case key == "properties":
			into, ok := existing.(map[string]any)
			from, _ := value.(map[string]any)
			if !ok {
				break
			}
			for name, prop := range from {
				if _, ok := into[name]; !ok {
					into[name] = prop
				}
			}
		case key == "required":
			into, _ := existing.([]any)
			from, _ := value.([]any)
			for _, name := range from {
				if !containsValue(into, name) {
					into = append(into, name)
				}
			}
			dst[key] = into
		}
	}
}

func stringsOnly(values []any) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		if s, ok := v.(string); ok && !containsValue(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func containsValue(values []any, want any) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
