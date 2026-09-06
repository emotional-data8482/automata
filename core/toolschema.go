package core

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
)

// Validate the supported structural schema rules. Other provider-specific
// keywords are preserved verbatim for adapters; core does not claim to enforce
// an entire JSON Schema dialect.
func validateSchema(s map[string]any) error {
	if typ, ok := s["type"]; ok {
		switch typ {
		case "object", "array", "string", "number", "integer", "boolean", "null":
		default:
			return fmt.Errorf("unsupported schema type %v", typ)
		}
	}
	if p, ok := s["properties"]; ok {
		props, ok := p.(map[string]any)
		if !ok {
			return fmt.Errorf("properties must be an object")
		}
		for name, v := range props {
			child, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("property %q must be a schema", name)
			}
			if err := validateSchema(child); err != nil {
				return err
			}
		}
	}
	if r, ok := s["required"]; ok {
		names, ok := r.([]any)
		if !ok {
			return fmt.Errorf("required must be an array")
		}
		for _, n := range names {
			if _, ok := n.(string); !ok {
				return fmt.Errorf("required entries must be strings")
			}
		}
	}
	if item, ok := s["items"]; ok {
		child, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("items must be a schema")
		}
		if err := validateSchema(child); err != nil {
			return err
		}
	}
	if ap, ok := s["additionalProperties"]; ok {
		switch v := ap.(type) {
		case bool:
		case map[string]any:
			if err := validateSchema(v); err != nil {
				return err
			}
		default:
			return fmt.Errorf("additionalProperties must be a boolean or schema")
		}
	}
	return nil
}

func validateSchemaValue(s map[string]any, v any, path string) error {
	mismatch := func() error { return fmt.Errorf("%s: expected %s", path, s["type"]) }
	switch s["type"] {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return mismatch()
		}
		required, _ := s["required"].([]any)
		for _, key := range required {
			name := key.(string)
			if _, ok := obj[name]; !ok {
				return fmt.Errorf("%s.%s: missing required field", path, name)
			}
		}
		props, _ := s["properties"].(map[string]any)
		keys := make([]string, 0, len(obj))
		for key := range obj {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child, known := props[key]
			if !known {
				child = s["additionalProperties"]
				if allowed, ok := child.(bool); ok && !allowed {
					return fmt.Errorf("%s.%s: unknown field", path, key)
				}
			}
			if schema, ok := child.(map[string]any); ok {
				if err := validateSchemaValue(schema, obj[key], path+"."+key); err != nil {
					return err
				}
			}
		}
	case "array":
		values, ok := v.([]any)
		if !ok {
			return mismatch()
		}
		if child, ok := s["items"].(map[string]any); ok {
			for i, value := range values {
				if err := validateSchemaValue(child, value, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "string":
		if _, ok := v.(string); !ok {
			return mismatch()
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return mismatch()
		}
	case "number", "integer":
		number, ok := v.(json.Number)
		if !ok {
			return mismatch()
		}
		if s["type"] == "integer" {
			r, ok := new(big.Rat).SetString(string(number))
			if !ok || !r.IsInt() {
				return mismatch()
			}
		}
	case "null":
		if v != nil {
			return mismatch()
		}
	}
	return nil
}
