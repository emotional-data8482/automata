package httpserver

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openAPIDocument []byte

// OpenAPI returns the server's OpenAPI 3.1 document, which it also serves
// at GET /v1/openapi.json without authorization: it describes the API and
// carries no run data.
func OpenAPI() []byte { return append([]byte(nil), openAPIDocument...) }

func (s *Server) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openAPIDocument)
}
