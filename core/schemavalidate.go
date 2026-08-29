package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// validateTypedPayload checks raw — a JSON payload a model produced for a
// typed run (a [RunTyped]/[RunSessionTyped] structured-output tool call, a
// parsed prose answer, or a provider-native structured response) — against the
// schema implied by T. It applies the same rules [buildSchema] advertises to
// the model:
//
//   - required = exported struct field whose json tag has no `omitempty`
//     (renamed fields follow the tag; `json:"-"` fields are excluded),
//   - type checks per kind: string↔string, bool↔boolean, ints↔integer
//     (integral numbers only), floats↔number, slices↔array (element-recursive),
//     string-keyed maps↔object (value-recursive), structs↔object
//     (field-recursive), pointers follow their element type,
//   - JSON null is valid only where the Go type allows it: pointer, slice, or
//     map fields (including through pointers),
//   - time.Time, json.RawMessage, interface{}, and []byte fields are checked
//     for presence only ([]byte must be a JSON string, matching its advertised
//     string schema) and are not inspected deeper,
//   - unknown JSON fields are ignored, matching json.Unmarshal.
//
// It returns path-qualified violations so the model can locate its own
// mistakes ("questions[2].topic: missing required field"); an empty slice
// means the payload is valid. Violation order is deterministic (struct field
// order, slice index order, sorted map keys).
func validateTypedPayload[T any](raw json.RawMessage) []string {
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return []string{"payload is not valid JSON: " + err.Error()}
	}
	v := &validator{visiting: map[reflect.Type]bool{}}
	v.validate(reflect.TypeOf((*T)(nil)).Elem(), decoded, "")
	return v.violations
}

type validator struct {
	violations []string
	// visiting breaks recursion on self-referential struct types, mirroring
	// typeSchema's cycle handling.
	visiting map[reflect.Type]bool
}

func (v *validator) addf(path, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if path != "" {
		msg = path + ": " + msg
	}
	v.violations = append(v.violations, msg)
}

// validate checks one JSON value against one Go type at the given path.
func (v *validator) validate(t reflect.Type, val any, path string) {
	if val == nil {
		// JSON null is valid only where the schema/model can represent it.
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			// nullable
		default:
			v.addf(path, "null is not a valid value for %s", jsonKindName(t))
		}
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType || t == rawMessageType || t.Kind() == reflect.Interface {
		return // opaque: presence only
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		// []byte advertises as a string schema (see typeSchema).
		if _, ok := val.(string); !ok {
			v.addf(path, "expected string, got %s", jsonKindOf(val))
		}
		return
	}

	switch t.Kind() {
	case reflect.String:
		if _, ok := val.(string); !ok {
			v.addf(path, "expected string, got %s", jsonKindOf(val))
		}
	case reflect.Bool:
		if _, ok := val.(bool); !ok {
			v.addf(path, "expected boolean, got %s", jsonKindOf(val))
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, ok := val.(json.Number)
		if !ok {
			v.addf(path, "expected integer, got %s", jsonKindOf(val))
			return
		}
		if !isIntegralNumber(n) {
			v.addf(path, "expected integer, got non-integral number %s", n)
		}
	case reflect.Float32, reflect.Float64:
		if _, ok := val.(json.Number); !ok {
			v.addf(path, "expected number, got %s", jsonKindOf(val))
		}
	case reflect.Slice, reflect.Array:
		items, ok := val.([]any)
		if !ok {
			v.addf(path, "expected array, got %s", jsonKindOf(val))
			return
		}
		elem := t.Elem()
		for i, item := range items {
			v.validate(elem, item, fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return // non-string-key maps advertise as plain objects
		}
		obj, ok := val.(map[string]any)
		if !ok {
			v.addf(path, "expected object, got %s", jsonKindOf(val))
			return
		}
		valType := t.Elem()
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v.validate(valType, obj[k], joinPath(path, k))
		}
	case reflect.Struct:
		obj, ok := val.(map[string]any)
		if !ok {
			v.addf(path, "expected object, got %s", jsonKindOf(val))
			return
		}
		if v.visiting[t] {
			return // recursive type: deeper fields already covered by buildSchema's plain-object fallback
		}
		v.visiting[t] = true
		v.validateObject(t, obj, path)
		delete(v.visiting, t)
	default:
		// buildSchema maps unknown kinds to plain object schemas (anything
		// goes); accept any value here too.
	}
}

// validateObject walks the exported fields of a struct against a decoded JSON
// object, mirroring objectSchema's field rules exactly.
func (v *validator) validateObject(t reflect.Type, obj map[string]any, path string) {
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		jsonName := field.Name
		required := true
		if tag := field.Tag.Get("json"); tag != "" {
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			if parts[0] != "" {
				jsonName = parts[0]
			}
			required = !strings.Contains(tag, "omitempty")
		}
		fieldPath := joinPath(path, jsonName)
		value, present := obj[jsonName]
		if !present {
			if required {
				v.addf(fieldPath, "missing required field")
			}
			continue
		}
		v.validate(field.Type, value, fieldPath)
	}
	// Unknown JSON fields are ignored, matching json.Unmarshal.
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// isIntegralNumber reports whether the JSON number literal denotes an integer
// ("1.0" and "1e2" count; "1.5" does not).
func isIntegralNumber(n json.Number) bool {
	if _, err := n.Int64(); err == nil {
		return true
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return false
	}
	return f == math.Trunc(f)
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

// jsonKindName names the JSON kind a Go type advertises as (see typeSchema).
func jsonKindName(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType || t == rawMessageType || t.Kind() == reflect.Interface {
		return "any"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Struct:
		return "object"
	default:
		return "object"
	}
}
