package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestOpenAPIDocumentIsServedWithoutAuthorization(t *testing.T) {
	h := newHarness(t, func(config *Config) {
		config.Authorize = func(*http.Request, Request) error { return errors.New("denied") }
	})
	resp, body := h.do(t, http.MethodGet, "/v1/openapi.json", nil, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.OpenAPI != "3.1.0" {
		t.Fatalf("document = %.80s, %v", body, err)
	}
	var got []string
	for path, methods := range doc.Paths {
		for method := range methods {
			got = append(got, strings.ToUpper(method)+" "+path)
		}
	}
	slices.Sort(got)
	want := []string{
		"GET /v1/changes", "GET /v1/definitions/{id}/{revision}", "GET /v1/openapi.json",
		"GET /v1/runs", "GET /v1/runs/{id}", "GET /v1/runs/{id}/events",
		"POST /v1/runs", "POST /v1/runs/{id}/cancel", "POST /v1/runs/{id}/hooks/acknowledge",
		"POST /v1/runs/{id}/operations/{op}/reconcile", "POST /v1/runs/{id}/waits/{wait}",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("documented routes = %v, want %v", got, want)
	}
}

// Every JSON field of a wire type is documented, so the document does not
// drift from the server.
func TestOpenAPISchemasCoverWireTypes(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPI(), &doc); err != nil {
		t.Fatal(err)
	}
	for schema, value := range map[string]any{
		"SubmitRequest": SubmitRequest{}, "ResolveWaitRequest": ResolveWaitRequest{}, "ReconcileRequest": ReconcileRequest{},
		"Run": RunView{}, "Wait": WaitView{}, "Operation": OperationView{}, "RunPage": RunPageView{},
		"Event": EventView{}, "EventPage": EventPageView{}, "Change": ChangeView{}, "ChangePage": ChangePageView{},
	} {
		properties := doc.Components.Schemas[schema].Properties
		typ := reflect.TypeOf(value)
		for i := range typ.NumField() {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			if _, ok := properties[name]; !ok {
				t.Errorf("schema %s does not document field %q", schema, name)
			}
		}
	}
}
