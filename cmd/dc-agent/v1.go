// The stable /v1 API. The old /api/* routes stay exactly as they are (the web
// desktop uses them); /v1 is the surface agents drive, with a session contract
// (see session.go) and real health (see health.go). /v1 mirrors /api behaviour
// where an endpoint already exists and adds the new ones.
package main

import (
	"encoding/json"
	"net/http"
)

// decodeJSON decodes a request body into v, bounded by maxBytes. It writes a
// 400 and reports false when the body is unreadable or not valid JSON.
func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes)).Decode(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// registerV1 mounts every /v1 route. Each module registers its own routes;
// main.go only calls this one function.
func (a *Agent) registerV1(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	a.registerSessionRoutes(mux, auth)
	a.registerEventRoutes(mux, auth)
	a.registerHealthRoutes(mux, auth)
	a.registerShellRoutes(mux, auth)
	a.registerFileRoutes(mux, auth)
	a.registerBrowserV1Routes(mux, auth)
	a.registerDisplayV1Routes(mux, auth)
	a.registerAppsV1Routes(mux, auth)
}
