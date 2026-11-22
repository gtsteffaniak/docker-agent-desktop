// The OpenAPI contract test: the generated spec (cmd/dad/docs/swagger.json, produced by
// `make swagger` / `swag init`) must validate clean, and must cover exactly the routes the
// server registers — every HandleFunc route in the spec with its method, and every spec
// path backed by a handler. A route added without an annotation (or an annotation without
// a route) fails here, so the spec cannot drift from the server.
package main

import (
	_ "embed"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
)

//go:embed docs/swagger.json
var swaggerJSON []byte

func TestOpenAPIContract(t *testing.T) {
	// The spec is Swagger 2.0 (what swag generates): unmarshal it, convert to
	// OpenAPI 3, and validate. Conversion failing, or validation failing, means
	// the spec is not shippable.
	var doc2 openapi2.T
	if err := json.Unmarshal(swaggerJSON, &doc2); err != nil {
		t.Fatalf("loading generated spec: %v", err)
	}
	doc, err := openapi2conv.ToV3(&doc2)
	if err != nil {
		t.Fatalf("converting generated spec to OpenAPI 3: %v", err)
	}
	if err := doc.Validate(t.Context()); err != nil {
		t.Fatalf("generated spec does not validate: %v", err)
	}

	// The registered routes, straight from the server: handler() records every
	// API route it registers on the Agent.
	a := &Agent{log: slog.New(slog.DiscardHandler)}
	_ = a.handler()
	registered := map[string]map[string]bool{}
	for _, pattern := range a.routes {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("registered route %q has no method", pattern)
		}
		if registered[path] == nil {
			registered[path] = map[string]bool{}
		}
		registered[path][strings.ToLower(method)] = true
	}

	// Every registered route exists in the spec with the same method.
	for path, methods := range registered {
		item := doc.Paths.Find(path)
		if item == nil {
			t.Errorf("registered route %s is missing from the spec", path)
			continue
		}
		for method := range methods {
			if item.GetOperation(strings.ToUpper(method)) == nil {
				t.Errorf("registered route %s %s is missing from the spec", method, path)
			}
		}
	}

	// Every spec operation has a handler behind it.
	for path, item := range doc.Paths.Map() {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			if item.GetOperation(method) != nil && !registered[path][strings.ToLower(method)] {
				t.Errorf("spec operation %s %s has no registered handler", strings.ToLower(method), path)
			}
		}
	}
}
