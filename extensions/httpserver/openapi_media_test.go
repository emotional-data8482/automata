package httpserver

import (
	"encoding/json"
	"strings"
	"testing"
)

// OpenAPI 3.1 Media Type Objects allow schema/example/examples/encoding and
// specification extensions, not description. Descriptions belong on a schema
// or response. JSON syntax and wire-field coverage alone miss this error.
func TestOpenAPIMediaTypeFields(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]map[string]json.RawMessage `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(OpenAPI(), &doc); err != nil {
		t.Fatal(err)
	}
	streams := 0
	for path, methods := range doc.Paths {
		for method, operation := range methods {
			for status, response := range operation.Responses {
				for media, fields := range response.Content {
					for field := range fields {
						switch field {
						case "schema", "example", "examples", "encoding":
						default:
							if !strings.HasPrefix(field, "x-") {
								t.Errorf("%s %s %s %s: invalid Media Type Object field %q", method, path, status, media, field)
							}
						}
					}
					if media == "text/event-stream" {
						streams++
						var schema struct {
							Description string `json:"description"`
						}
						if err := json.Unmarshal(fields["schema"], &schema); err != nil || schema.Description == "" {
							t.Errorf("%s %s: SSE schema description missing: %v", method, path, err)
						}
					}
				}
			}
		}
	}
	if streams != 2 {
		t.Fatalf("SSE definitions = %d, want two", streams)
	}
}
