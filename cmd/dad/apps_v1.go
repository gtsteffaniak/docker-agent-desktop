package main

import (
	"net/http"

	"github.com/gtsteffaniak/docker-agent-desktop/internal/apps"
)

// registerAppsV1Routes mounts the /v1 apps surface. Listing is a read; start and stop are
// session-gated.
func (a *Agent) registerAppsV1Routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /v1/apps", auth(a.handleAppsV1))
	mux.HandleFunc("POST /v1/apps/start", auth(a.requireSession(a.handleAppStartV1)))
	mux.HandleFunc("POST /v1/apps/stop", auth(a.requireSession(a.handleAppStopV1)))
}

// appV1 is one registry app as the /v1 API reports it. Exec is deliberately not exposed.
type appV1 struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Icon    string `json:"icon"`
	Order   int    `json:"order"`
	Port    int    `json:"port"`
	Running bool   `json:"running"`
}

// handleAppsV1 lists the registry apps with whether each is running. The browser (port 0) is
// running when its screencast is attached.
func (a *Agent) handleAppsV1(w http.ResponseWriter, _ *http.Request) {
	list := a.loadApps()
	out := make([]appV1, 0, len(list))
	for _, app := range list {
		running := a.appRunning(app.ID)
		if app.Port == 0 {
			running = a.screencast() != nil
		}
		out = append(out, appV1{
			ID: app.ID, Name: app.Name, Icon: app.Icon,
			Order: app.Order, Port: app.Port, Running: running,
		})
	}
	writeJSON(w, map[string]any{"apps": out})
}

// handleAppStartV1 delegates to the /api handler (including the browser Port-0 case) and
// records an apps.start event on success.
func (a *Agent) handleAppStartV1(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	a.handleAppStart(rec, r)
	if rec.status < 400 {
		a.recordEvent("apps.start", a.sessionHolderName(r), r.URL.Query().Get("id"))
	}
}

// handleAppStopV1 stops one registry app and reports it as not running. Unknown ids are a
// 400, mirroring start. The browser (port 0) has no app process, so stopping it stops
// Chromium. The app's broadwayd display stays running for reuse.
func (a *Agent) handleAppStopV1(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	app, ok := apps.Find(a.loadApps(), id)
	if !ok {
		http.Error(w, "unknown app", http.StatusBadRequest)
		return
	}
	if app.Port == 0 {
		a.stopBrowser()
	} else if err := a.stopApp(id); err != nil {
		a.log.Warn("app stop failed", "app", id, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.recordEvent("apps.stop", a.sessionHolderName(r), id)
	writeJSON(w, map[string]any{"id": id, "running": false})
}
