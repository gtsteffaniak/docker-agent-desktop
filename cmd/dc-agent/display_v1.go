package main

import (
	"net/http"
	"strconv"

	"github.com/gtsteffaniak/docker-computer/internal/broadway"
)

// registerDisplayV1Routes mounts the /v1 display surface. Screenshots and metadata are reads;
// pointer and keyboard input is session-gated. Display input is not recorded in the activity
// log: it is far too noisy.
func (a *Agent) registerDisplayV1Routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /v1/display/screenshot", auth(a.handleScreenshot))
	mux.HandleFunc("GET /v1/display/meta", auth(a.handleBroadwayMeta))
	mux.HandleFunc("POST /v1/display/click", auth(a.requireSession(a.handleDisplayClick)))
	mux.HandleFunc("POST /v1/display/type", auth(a.requireSession(a.handleDisplayType)))
	mux.HandleFunc("POST /v1/display/key", auth(a.requireSession(a.handleDisplayKey)))
	mux.HandleFunc("POST /v1/display/scroll", auth(a.requireSession(a.handleDisplayScroll)))
	mux.HandleFunc("POST /v1/display/drag", auth(a.requireSession(a.handleDisplayDrag)))
}

// handleDisplayClick clicks at root coordinates. Button defaults to left (1); 2 is middle,
// 3 is right.
func (a *Agent) handleDisplayClick(w http.ResponseWriter, r *http.Request) {
	var body struct {
		X      int32  `json:"x"`
		Y      int32  `json:"y"`
		Button uint32 `json:"button,omitempty"`
	}
	if !decodeJSON(w, r, 4096, &body) {
		return
	}
	if body.Button == 0 {
		body.Button = 1
	}
	if body.Button < 1 || body.Button > 3 {
		http.Error(w, "button must be 1, 2 or 3", http.StatusBadRequest)
		return
	}
	if err := a.broadway.ClickButton(body.X, body.Y, body.Button); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDisplayType types text into the focused window.
func (a *Agent) handleDisplayType(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, 64<<10, &body) {
		return
	}
	if err := a.broadway.TypeText(body.Text); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDisplayKey presses and releases one key by name (Return, Tab, Escape, arrows, F1–F12,
// single characters). Mods are GDK modifier masks: Shift 1, Control 4, Alt 8.
func (a *Agent) handleDisplayKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key  string `json:"key"`
		Mods uint32 `json:"mods,omitempty"`
	}
	if !decodeJSON(w, r, 4096, &body) {
		return
	}
	ks, ok := broadway.KeysymForName(body.Key)
	if !ok {
		http.Error(w, "unknown key "+strconv.Quote(body.Key), http.StatusBadRequest)
		return
	}
	if err := a.broadway.Key(ks, body.Mods); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDisplayScroll scrolls vertically at root coordinates. dx is ignored: horizontal
// scrolling is not supported by the Broadway encoder.
func (a *Agent) handleDisplayScroll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		X  int32 `json:"x"`
		Y  int32 `json:"y"`
		DX int   `json:"dx,omitempty"`
		DY int   `json:"dy"`
	}
	if !decodeJSON(w, r, 4096, &body) {
		return
	}
	if err := a.broadway.ScrollAt(body.X, body.Y, body.DX, body.DY); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDisplayDrag drags with the left button from (x1, y1) to (x2, y2).
func (a *Agent) handleDisplayDrag(w http.ResponseWriter, r *http.Request) {
	var body struct {
		X1 int32 `json:"x1"`
		Y1 int32 `json:"y1"`
		X2 int32 `json:"x2"`
		Y2 int32 `json:"y2"`
	}
	if !decodeJSON(w, r, 4096, &body) {
		return
	}
	if err := a.broadway.Drag(body.X1, body.Y1, body.X2, body.Y2); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
