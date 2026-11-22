package main

import (
	"net/http"

	"github.com/gtsteffaniak/docker-agent-desktop/internal/apps"
)

// registerAppsV1Routes mounts the /v1 apps surface. Listing is a read; start and stop are
// session-gated.
func (a *Agent) registerAppsV1Routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	a.handle(mux, "GET /v1/apps", auth(a.handleAppsV1))
	a.handle(mux, "POST /v1/apps/start", auth(a.requireSession(a.handleAppStartV1)))
	a.handle(mux, "POST /v1/apps/stop", auth(a.requireSession(a.handleAppStopV1)))
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

// appsListResponse is the GET /v1/apps body.
type appsListResponse struct {
	Apps []appV1 `json:"apps"`
}

// appStopResponse is the POST /v1/apps/stop body.
type appStopResponse struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
}

// handleAppsV1 lists the registry apps with whether each is running. The browser (port 0) is
// running when its screencast is attached.
// @Summary List apps
// @Description Lists the registry apps with whether each is running. The browser (port 0) is running when its screencast is attached.
// @Tags apps
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Success 200 {object} appsListResponse "Registry apps"
// @Failure 401 {string} string "Missing or wrong token"
// @Security ApiKeyAuth
// @Router /v1/apps [get]
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
	writeJSON(w, appsListResponse{Apps: out})
}

// handleAppStartV1 delegates to the shared start handler (including the browser Port-0 case)
// and records an apps.start event on success.
// @Summary Start an app
// @Description Starts one registry app: its Broadway display first, then the app. Starting an app that is already running does nothing. The browser (port 0) has no process to start; this reports whether Chromium is running instead.
// @Tags apps
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param id query string true "Registry app id"
// @Success 200 {object} appStartResponse "App started (or already running)"
// @Failure 400 {string} string "Unknown app id"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 500 {string} string "App start failed"
// @Security ApiKeyAuth
// @Router /v1/apps/start [post]
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
// @Summary Stop an app
// @Description Stops one registry app and reports it as not running. Unknown ids are a 400, mirroring start. The browser (port 0) has no app process, so stopping it stops Chromium. The app's broadwayd display stays running for reuse.
// @Tags apps
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param id query string true "Registry app id"
// @Success 200 {object} appStopResponse "App stopped"
// @Failure 400 {string} string "Unknown app id"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 500 {string} string "App stop failed"
// @Security ApiKeyAuth
// @Router /v1/apps/stop [post]
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
	writeJSON(w, appStopResponse{ID: id, Running: false})
}
