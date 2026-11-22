package main

import (
	"net/http"
	"strconv"

	"github.com/gtsteffaniak/docker-agent-desktop/internal/broadway"
)

// registerDisplayV1Routes mounts the /v1 display surface. Screenshots and metadata are reads;
// pointer and keyboard input is session-gated. Display input is not recorded in the activity
// log: it is far too noisy.
func (a *Agent) registerDisplayV1Routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	a.handle(mux, "GET /v1/display/screenshot", auth(a.handleScreenshot))
	a.handle(mux, "GET /v1/display/meta", auth(a.handleBroadwayMeta))
	a.handle(mux, "POST /v1/display/click", auth(a.requireSession(a.handleDisplayClick)))
	a.handle(mux, "POST /v1/display/type", auth(a.requireSession(a.handleDisplayType)))
	a.handle(mux, "POST /v1/display/key", auth(a.requireSession(a.handleDisplayKey)))
	a.handle(mux, "POST /v1/display/scroll", auth(a.requireSession(a.handleDisplayScroll)))
	a.handle(mux, "POST /v1/display/drag", auth(a.requireSession(a.handleDisplayDrag)))
}

// displayClickRequest is the body of POST /v1/display/click. Button defaults to left (1);
// 2 is middle, 3 is right.
type displayClickRequest struct {
	X      int32  `json:"x"`
	Y      int32  `json:"y"`
	Button uint32 `json:"button,omitempty"`
}

// displayTypeRequest is the body of POST /v1/display/type.
type displayTypeRequest struct {
	Text string `json:"text"`
}

// displayKeyRequest is the body of POST /v1/display/key. Mods are GDK modifier masks:
// Shift 1, Control 4, Alt 8.
type displayKeyRequest struct {
	Key  string `json:"key"`
	Mods uint32 `json:"mods,omitempty"`
}

// displayScrollRequest is the body of POST /v1/display/scroll. dx is ignored: horizontal
// scrolling is not supported by the Broadway encoder.
type displayScrollRequest struct {
	X  int32 `json:"x"`
	Y  int32 `json:"y"`
	DX int   `json:"dx,omitempty"`
	DY int   `json:"dy"`
}

// displayDragRequest is the body of POST /v1/display/drag.
type displayDragRequest struct {
	X1 int32 `json:"x1"`
	Y1 int32 `json:"y1"`
	X2 int32 `json:"x2"`
	Y2 int32 `json:"y2"`
}

// handleDisplayClick clicks at root coordinates. Button defaults to left (1); 2 is middle,
// 3 is right.
// @Summary Click at display coordinates
// @Description Clicks at root display coordinates. Button defaults to left (1); 2 is middle, 3 is right.
// @Tags display
// @Accept json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body displayClickRequest true "Click coordinates and button"
// @Success 204 "Clicked"
// @Failure 400 {string} string "Unparsable body, or button not 1..3"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 503 {string} string "Display unavailable"
// @Security ApiKeyAuth
// @Router /v1/display/click [post]
func (a *Agent) handleDisplayClick(w http.ResponseWriter, r *http.Request) {
	var body displayClickRequest
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
// @Summary Type text
// @Description Types text into the focused window.
// @Tags display
// @Accept json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body displayTypeRequest true "Text to type"
// @Success 204 "Typed"
// @Failure 400 {string} string "Unparsable body"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 503 {string} string "Display unavailable"
// @Security ApiKeyAuth
// @Router /v1/display/type [post]
func (a *Agent) handleDisplayType(w http.ResponseWriter, r *http.Request) {
	var body displayTypeRequest
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
// @Summary Press a key
// @Description Presses and releases one key by name (Return, Tab, Escape, arrows, F1–F12, single characters). Mods are GDK modifier masks: Shift 1, Control 4, Alt 8.
// @Tags display
// @Accept json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body displayKeyRequest true "Key name and modifiers"
// @Success 204 "Key pressed"
// @Failure 400 {string} string "Unparsable body, or unknown key"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 503 {string} string "Display unavailable"
// @Security ApiKeyAuth
// @Router /v1/display/key [post]
func (a *Agent) handleDisplayKey(w http.ResponseWriter, r *http.Request) {
	var body displayKeyRequest
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
// @Summary Scroll at display coordinates
// @Description Scrolls vertically at root display coordinates. dx is ignored: horizontal scrolling is not supported by the Broadway encoder.
// @Tags display
// @Accept json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body displayScrollRequest true "Scroll coordinates and deltas"
// @Success 204 "Scrolled"
// @Failure 400 {string} string "Unparsable body"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 503 {string} string "Display unavailable"
// @Security ApiKeyAuth
// @Router /v1/display/scroll [post]
func (a *Agent) handleDisplayScroll(w http.ResponseWriter, r *http.Request) {
	var body displayScrollRequest
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
// @Summary Drag on the display
// @Description Drags with the left button from (x1, y1) to (x2, y2).
// @Tags display
// @Accept json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body displayDragRequest true "Drag start and end coordinates"
// @Success 204 "Dragged"
// @Failure 400 {string} string "Unparsable body"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 503 {string} string "Display unavailable"
// @Security ApiKeyAuth
// @Router /v1/display/drag [post]
func (a *Agent) handleDisplayDrag(w http.ResponseWriter, r *http.Request) {
	var body displayDragRequest
	if !decodeJSON(w, r, 4096, &body) {
		return
	}
	if err := a.broadway.Drag(body.X1, body.Y1, body.X2, body.Y2); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
