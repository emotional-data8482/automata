package core

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// canonicalIntegralNumbers rewrites only numbers destined for Go integer
// fields. All other bytes, including floats and opaque/custom-decoded JSON,
// stay unchanged. Invalid JSON stays invalid for the caller's json.Unmarshal.
func canonicalIntegralNumbers(raw json.RawMessage, typ reflect.Type) json.RawMessage {
	if !bytes.ContainsAny(raw, ".eE") || !json.Valid(raw) {
		return raw
	}
	type replacement struct {
		start, end int
		literal    string
	}
	var edits []replacement
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	fields := map[reflect.Type][]jsonInputField{}
	var walk func(reflect.Type) error
	walk = func(t reflect.Type) error {
		for t != nil {
			if t.Implements(jsonUnmarshalerType) || (t.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(jsonUnmarshalerType)) {
				t = nil // RawMessage, json.Number, and custom UnmarshalJSON own their bytes.
				break
			}
			if t.Kind() != reflect.Pointer {
				break
			}
			t = t.Elem()
		}
		if t == nil || (!integerKind(t.Kind()) && t.Kind() != reflect.Struct && t.Kind() != reflect.Map && t.Kind() != reflect.Slice && t.Kind() != reflect.Array) {
			var opaque json.RawMessage
			return decoder.Decode(&opaque)
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token := token.(type) {
		case json.Delim:
			for decoder.More() {
				var child reflect.Type
				if token == '{' {
					key, err := decoder.Token()
					if err != nil {
						return err
					}
					switch t.Kind() {
					case reflect.Map:
						child = t.Elem()
					case reflect.Struct:
						if _, ok := fields[t]; !ok {
							fields[t] = jsonInputFields(t)
						}
						// Like encoding/json, exact names win over case-folded names.
						for _, field := range fields[t] {
							if field.name == key.(string) {
								child = field.typ
								break
							}
							if child == nil && strings.EqualFold(field.name, key.(string)) {
								child = field.typ
							}
						}
					}
				} else if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
					child = t.Elem()
				}
				if err := walk(child); err != nil {
					return err
				}
			}
			_, err = decoder.Token() // closing delimiter
			return err
		case json.Number:
			if integerKind(t.Kind()) {
				if literal, ok := canonicalIntegerLiteral(token.String()); ok {
					end := int(decoder.InputOffset())
					edits = append(edits, replacement{end - len(token.String()), end, literal})
				}
			}
		}
		return nil
	}
	if walk(typ) != nil || len(edits) == 0 {
		return raw
	}
	var out []byte
	after := 0
	for _, edit := range edits {
		out = append(out, raw[after:edit.start]...)
		out = append(out, edit.literal...)
		after = edit.end
	}
	return append(out, raw[after:]...)
}

var jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()

func integerKind(kind reflect.Kind) bool {
	return kind >= reflect.Int && kind <= reflect.Uintptr
}

func canonicalIntegerLiteral(literal string) (string, bool) {
	if !strings.ContainsAny(literal, ".eE") {
		return "", false
	}
	mantissa, exponent, _ := strings.Cut(strings.ToLower(literal), "e")
	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign, mantissa = "-", mantissa[1:]
	}
	fraction := 0
	if dot := strings.IndexByte(mantissa, '.'); dot >= 0 {
		fraction = len(mantissa) - dot - 1
		mantissa = mantissa[:dot] + mantissa[dot+1:]
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		return "0", true
	}
	power := 0
	if exponent != "" {
		var err error
		power, err = strconv.Atoi(exponent)
		if err != nil || power < -len(literal) || power > len(literal)+20 {
			return "", false
		}
	}
	// Decimal shifts need no big-number allocation. No Go integer has more
	// than 20 digits; an unrepresentable value stays for json.Unmarshal to reject.
	shift := power - fraction
	if shift >= 0 {
		if len(digits)+shift > 20 {
			return "", false
		}
		return sign + digits + strings.Repeat("0", shift), true
	}
	end := len(digits) + shift
	if end <= 0 || end > 20 || strings.Trim(digits[end:], "0") != "" {
		return "", false
	}
	return sign + digits[:end], true
}

type jsonInputField struct {
	name   string
	typ    reflect.Type
	index  []int
	tagged bool
	count  int
}

// jsonInputFields follows encoding/json's embedded-field dominance rules:
// shallower fields win, then tagged fields, while remaining ties are ignored.
func jsonInputFields(t reflect.Type) []jsonInputField {
	byName := map[string]jsonInputField{}
	visiting := map[reflect.Type]bool{}
	var visit func(reflect.Type, []int)
	visit = func(t reflect.Type, prefix []int) {
		if visiting[t] {
			return
		}
		visiting[t] = true
		defer delete(visiting, t)
		for i := range t.NumField() {
			field := t.Field(i)
			elem := field.Type
			if elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if field.PkgPath != "" && (!field.Anonymous || elem.Kind() != reflect.Struct) {
				continue
			}
			tag := field.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			index := append(slices.Clone(prefix), i)
			if field.Anonymous && name == "" && elem.Kind() == reflect.Struct {
				visit(elem, index)
				continue
			}
			tagged := name != ""
			if name == "" {
				name = field.Name
			}
			candidate := jsonInputField{name: name, typ: field.Type, index: index, tagged: tagged, count: 1}
			old, ok := byName[name]
			switch {
			case !ok, len(index) < len(old.index), len(index) == len(old.index) && tagged && !old.tagged:
				byName[name] = candidate
			case len(index) == len(old.index) && tagged == old.tagged:
				old.count++
				byName[name] = old
			}
		}
	}
	visit(t, nil)
	var fields []jsonInputField
	for _, field := range byName {
		if field.count == 1 {
			fields = append(fields, field)
		}
	}
	slices.SortFunc(fields, func(a, b jsonInputField) int { return slices.Compare(a.index, b.index) })
	return fields
}
