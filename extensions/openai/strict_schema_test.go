package openai

import (
	"encoding/json"
	"testing"
)

// Core emits "type":["object","null"] for pointer-to-struct fields. Strict
// mode must close such objects like plain ones instead of sending strict:true
// with an invalid schema (bug review P3).
func TestStrictSchemaClosesNullableObjects(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"owner":{"type":["object","null"],"properties":{"name":{"type":"string"}},"required":["name"]}},"required":["owner"]}`)
	format := jsonSchemaResponseFormat(raw)
	if format == nil || !format.JSONSchema.Strict {
		t.Fatalf("format = %#v, want strict", format)
	}
	owner := format.JSONSchema.Schema.(map[string]any)["properties"].(map[string]any)["owner"].(map[string]any)
	if owner["additionalProperties"] != false {
		t.Fatalf("nullable object = %#v, want additionalProperties:false", owner)
	}
	required, _ := owner["required"].([]string)
	if len(required) != 1 || required[0] != "name" {
		t.Fatalf("nullable object required = %#v", owner["required"])
	}

	open := json.RawMessage(`{"type":"object","properties":{"meta":{"type":["object","null"],"additionalProperties":{"type":"string"}}}}`)
	if format := jsonSchemaResponseFormat(open); format.JSONSchema.Strict {
		t.Fatal("a nullable map field must disable strict mode")
	}
}
