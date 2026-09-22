package core

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// validateTypedPayload checks raw — a JSON payload a model produced for a
// typed run (a [RunTyped]/[RunSessionTyped] structured-output tool call, a
// parsed prose answer, a provider-native structured response, or a structured
// child value) — against the schema implied by T. It is the Go-typed entry
// point of the one supported schema contract shared with tool input values
// and declared final-output schemas (see schemacontract.go): T's schema is
// compiled with the same rules, and the payload is validated by the same
// value validator used for raw declared schemas.
//
// The Go-derived contract preserves the rules [buildSchema] advertises:
//
//   - required = exported struct field whose json tag has no `omitempty`
//     (renamed fields follow the tag; `json:"-"` fields are excluded),
//   - type checks per kind: string↔string, bool↔boolean, ints↔integer
//     (integral numbers only), floats↔number, slices↔array (element-recursive),
//     string-keyed maps↔object (value-recursive), structs↔object
//     (field-recursive), pointers follow their element type,
//   - JSON null is valid exactly where the Go kind allows it: pointer, slice,
//     or map fields (including pointer-to-interface); non-pointer interface
//     fields reject null while accepting any other value,
//   - time.Time fields are checked as strings (the advertised schema);
//     json.RawMessage and interface{} fields are otherwise not inspected
//     deeper; []byte must be a JSON string, matching its advertised schema,
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
	contract := typeContract(reflect.TypeOf((*T)(nil)).Elem(), map[reflect.Type]bool{})
	return contract.validate(decoded, "")
}

// typeContract compiles the schema implied by a Go type into the shared
// contract. Nullability follows the Go kind, not the advertised schema:
// pointer, slice, and map fields accept JSON null exactly as the reflect
// validator always has, while non-pointer interface fields do not.
func typeContract(t reflect.Type, visiting map[reflect.Type]bool) schemaContract {
	nullable := false
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
		nullable = true
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		nullable = true
	}
	switch {
	case t == timeType:
		// Advertised as {"type":"string","format":"date-time"}; the format is
		// provider metadata and the string type is enforced.
		return schemaContract{typ: "string", display: "any", nullable: nullable}
	case t == rawMessageType:
		// Advertised as a shape-free schema; null stays valid (slice kind).
		return schemaContract{display: "any", nullable: true}
	case t.Kind() == reflect.Interface:
		// Advertised as a shape-free schema, but null is valid only when the
		// interface itself was reached through a pointer.
		return schemaContract{display: "any", nullable: nullable}
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		// []byte advertises as a string schema (see typeSchema).
		return schemaContract{typ: "string", display: "string", nullable: true}
	}

	switch t.Kind() {
	case reflect.String:
		return schemaContract{typ: "string", display: "string", nullable: nullable}
	case reflect.Bool:
		return schemaContract{typ: "boolean", display: "boolean", nullable: nullable}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return schemaContract{typ: "integer", display: "integer", nullable: nullable}
	case reflect.Float32, reflect.Float64:
		return schemaContract{typ: "number", display: "number", nullable: nullable}
	case reflect.Slice, reflect.Array:
		items := typeContract(t.Elem(), visiting)
		return schemaContract{typ: "array", display: "array", nullable: nullable, items: &items}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			// Non-string-key maps advertise as plain objects and accept any
			// value, including null.
			return schemaContract{display: "object", nullable: true}
		}
		additional := typeContract(t.Elem(), visiting)
		return schemaContract{typ: "object", display: "object", nullable: nullable, additional: &additional}
	case reflect.Struct:
		if visiting[t] {
			// Self-referential type: deeper fields are already covered by
			// buildSchema's plain-object fallback, which constrains nothing.
			return schemaContract{display: "object", nullable: nullable}
		}
		visiting[t] = true
		c := structContract(t, visiting)
		c.nullable = nullable
		delete(visiting, t)
		return c
	default:
		// Unknown kinds map to plain object schemas that accept any value;
		// null is valid only when the value itself was reached through a pointer.
		return schemaContract{display: "object", nullable: nullable}
	}
}

// structContract walks the exported fields of a struct type, mirroring
// objectSchema's field rules exactly.
func structContract(t reflect.Type, visiting map[reflect.Type]bool) schemaContract {
	c := schemaContract{typ: "object", display: "object"}
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
		c.properties = append(c.properties, contractProperty{
			name:     jsonName,
			contract: typeContract(field.Type, visiting),
			required: required,
		})
	}
	// Unknown JSON fields are ignored, matching json.Unmarshal.
	return c
}
