package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gtsteffaniak/docker-agent-desktop/internal/cdp"
)

// registerBrowserV1Routes mounts the /v1 browser surface. Reads are open to any authed caller;
// the mutating routes are session-gated. The browser stream (GET
// /v1/browser/stream) is the live frame + command socket; its control
// messages are session-gated through the session bound to the socket (see
// browser_ws.go).
func (a *Agent) registerBrowserV1Routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	a.handle(mux, "POST /v1/browser/start", auth(a.requireSession(a.handleBrowserStart)))
	a.handle(mux, "POST /v1/browser/stop", auth(a.requireSession(a.handleBrowserStop)))
	a.handle(mux, "POST /v1/browser/evaluate", auth(a.requireSession(a.handleBrowserEvaluate)))
	a.handle(mux, "POST /v1/browser/act", auth(a.requireSession(a.handleBrowserAct)))
	a.handle(mux, "GET /v1/browser/text", auth(a.handleBrowserText))
	a.handle(mux, "GET /v1/browser/state", auth(a.handleBrowserState))
	a.handle(mux, "POST /v1/browser/navigate", auth(a.requireSession(a.handleV1BrowserNavigate)))
	a.handle(mux, "POST /v1/browser/back", auth(a.requireSession(a.handleBrowserBack)))
	a.handle(mux, "POST /v1/browser/forward", auth(a.requireSession(a.handleBrowserForward)))
	a.handle(mux, "POST /v1/browser/reload", auth(a.requireSession(a.handleBrowserReload)))
	a.handle(mux, "POST /v1/browser/input", auth(a.requireSession(a.handleBrowserInput)))
	a.handle(mux, "POST /v1/browser/size", auth(a.requireSession(a.handleBrowserSize)))
	a.handle(mux, "GET /v1/browser/frame", auth(a.handleBrowserFrame))
	a.handle(mux, "GET /v1/browser/stream", auth(a.handleBrowserStream))
}

// browserRunningResponse is the reply to POST /v1/browser/start and /v1/browser/stop.
type browserRunningResponse struct {
	Running bool `json:"running"`
}

// browserEvaluateRequest is the body of POST /v1/browser/evaluate.
type browserEvaluateRequest struct {
	Expression string `json:"expression"`
}

// browserEvaluateResponse is the reply to POST /v1/browser/evaluate: the expression's
// value as raw JSON.
type browserEvaluateResponse struct {
	Value any `json:"value"`
}

// browserTextResponse is the reply to GET /v1/browser/text.
type browserTextResponse struct {
	Text string `json:"text"`
}

// handleBrowserStart starts Chromium on first use and reports whether it is running.
// @Summary Start the browser
// @Description Starts Chromium on first use and reports whether it is running. Starting an already-running browser does nothing.
// @Tags browser
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Success 200 {object} browserRunningResponse "Browser running"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 503 {string} string "Chromium could not start"
// @Security ApiKeyAuth
// @Router /v1/browser/start [post]
func (a *Agent) handleBrowserStart(w http.ResponseWriter, r *http.Request) {
	if _, err := a.ensureBrowser(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	a.recordEvent("browser.start", a.sessionHolderName(r), "")
	writeJSON(w, browserRunningResponse{Running: true})
}

// handleBrowserStop stops Chromium. The next navigation starts it again.
// @Summary Stop the browser
// @Description Stops Chromium. The next navigation starts it again.
// @Tags browser
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Success 200 {object} browserRunningResponse "Browser stopped"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Security ApiKeyAuth
// @Router /v1/browser/stop [post]
func (a *Agent) handleBrowserStop(w http.ResponseWriter, r *http.Request) {
	a.stopBrowser()
	a.recordEvent("browser.stop", a.sessionHolderName(r), "")
	writeJSON(w, browserRunningResponse{Running: false})
}

// handleBrowserEvaluate runs one JavaScript expression in the page and returns its value as
// raw JSON. The expression is capped at 64 KiB by the request body limit.
// @Summary Evaluate JavaScript in the page
// @Description Runs one JavaScript expression in the page and returns its value as raw JSON (undefined, functions and other values with no JSON form become null). The expression is capped at 64 KiB.
// @Tags browser
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body browserEvaluateRequest true "Expression to evaluate"
// @Success 200 {object} browserEvaluateResponse "Expression value"
// @Failure 400 {string} string "Unparsable body"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 502 {string} string "Evaluation failed"
// @Failure 503 {string} string "Chromium not running"
// @Security ApiKeyAuth
// @Router /v1/browser/evaluate [post]
func (a *Agent) handleBrowserEvaluate(w http.ResponseWriter, r *http.Request) {
	sc := a.current(w)
	if sc == nil {
		return
	}
	var body browserEvaluateRequest
	if !decodeJSON(w, r, 64<<10, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), browserCallTimeout)
	defer cancel()
	raw, err := sc.Evaluate(ctx, body.Expression)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	val, err := remoteValue(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, browserEvaluateResponse{Value: val})
}

// remoteValue unwraps a CDP RemoteObject (Runtime.evaluate with returnByValue)
// to the expression's JSON value, so callers get 2 for "1+1" rather than
// {"type":"number","value":2,...}. undefined, functions and other values with
// no JSON form become null.
func remoteValue(raw json.RawMessage) (json.RawMessage, error) {
	var obj struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if len(obj.Value) == 0 {
		return json.RawMessage("null"), nil
	}
	return obj.Value, nil
}

// handleBrowserText returns the page's visible text.
// @Summary Browser page text
// @Description Returns the page's visible text.
// @Tags browser
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Success 200 {object} browserTextResponse "Visible text"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 502 {string} string "Could not read the page"
// @Failure 503 {string} string "Chromium not running"
// @Security ApiKeyAuth
// @Router /v1/browser/text [get]
func (a *Agent) handleBrowserText(w http.ResponseWriter, r *http.Request) {
	sc := a.current(w)
	if sc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), browserCallTimeout)
	defer cancel()
	text, err := sc.PageText(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, browserTextResponse{Text: text})
}

// handleV1BrowserNavigate delegates to the shared navigate handler and records a
// browser.navigate event on success. The body is read first so the URL can be recorded,
// then restored for the inner handler.
// @Summary Navigate the browser
// @Description Navigates the page. Navigating is what starts Chromium, so the first address starts it. The reply is the normalized URL that was navigated to.
// @Tags browser
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body browserNavigateRequest true "URL to navigate to"
// @Success 200 {object} browserNavigateResponse "Normalized URL navigated to"
// @Failure 400 {string} string "Unparsable body, or unusable URL"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 502 {string} string "Navigation failed"
// @Failure 503 {string} string "Chromium could not start"
// @Security ApiKeyAuth
// @Router /v1/browser/navigate [post]
func (a *Agent) handleV1BrowserNavigate(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	var body struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	a.handleBrowserNavigate(rec, r)
	if rec.status < 400 {
		a.recordEvent("browser.navigate", a.sessionHolderName(r), body.URL)
	}
}

// errSelectorNotFound is returned when an act selector matches no element.
var errSelectorNotFound = errors.New("selector not found")

// browserActRequest is one /v1/browser/act call. Fields are used by kind.
type browserActRequest struct {
	Kind     string  `json:"kind"`
	Selector string  `json:"selector,omitempty"`
	Text     string  `json:"text,omitempty"`
	X        float64 `json:"x,omitempty"`
	Y        float64 `json:"y,omitempty"`
	DX       float64 `json:"dx,omitempty"`
	DY       float64 `json:"dy,omitempty"`
	Button   int     `json:"button,omitempty"`
	Clicks   int     `json:"clicks,omitempty"`
	Key      string  `json:"key,omitempty"`
}

// actErrorResponse is the 404 body when an act selector matches no element.
type actErrorResponse struct {
	Error string `json:"error"`
}

// handleBrowserAct performs one high-level browser action: click, type, select, scroll or key.
// A selector that matches nothing is a 404 with a JSON error body.
// @Summary Browser action
// @Description Performs one high-level browser action: click, type, select, scroll or key. Click and scroll take a selector (resolved to its centre) or x/y coordinates; type and select take a selector plus text; key takes a key name. A selector that matches nothing is a 404.
// @Tags browser
// @Accept json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body browserActRequest true "Action to perform"
// @Success 204 "Action performed"
// @Failure 400 {string} string "Unknown act kind, or select/key missing what they need"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 404 {object} actErrorResponse "Selector matched nothing"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 502 {string} string "Action failed"
// @Failure 503 {string} string "Chromium not running"
// @Security ApiKeyAuth
// @Router /v1/browser/act [post]
func (a *Agent) handleBrowserAct(w http.ResponseWriter, r *http.Request) {
	sc := a.current(w)
	if sc == nil {
		return
	}
	var body browserActRequest
	if !decodeJSON(w, r, 64<<10, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), browserCallTimeout)
	defer cancel()
	var err error
	switch body.Kind {
	case "click":
		err = browserActClick(ctx, sc, body)
	case "type":
		err = browserActType(ctx, sc, body)
	case "select":
		err = browserActSelect(ctx, sc, body)
	case "scroll":
		err = browserActScroll(ctx, sc, body)
	case "key":
		err = browserActKey(ctx, sc, body)
	default:
		http.Error(w, "unknown act kind "+strconv.Quote(body.Kind), http.StatusBadRequest)
		return
	}
	if err != nil {
		if errors.Is(err, errSelectorNotFound) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(actErrorResponse{Error: "selector not found"})
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	a.recordEvent("browser.act", a.sessionHolderName(r), actDetail(body))
	w.WriteHeader(http.StatusNoContent)
}

// actDetail is the activity-log detail for an act: its kind and selector when it has one.
func actDetail(b browserActRequest) string {
	if b.Selector != "" {
		return b.Kind + " " + b.Selector
	}
	return b.Kind
}

// selectorResolveJS resolves a selector to its centre in one Runtime.evaluate call, or null
// when nothing matches.
func selectorResolveJS(selector string) string {
	sel, _ := json.Marshal(selector)
	return `(()=>{const el=document.querySelector(` + string(sel) + `); if(!el) return null; const r=el.getBoundingClientRect(); return {x:r.x+r.width/2, y:r.y+r.height/2, w:r.width, h:r.height};})()`
}

// selectorFocusJS focuses the first element matching selector, or false when nothing matches.
func selectorFocusJS(selector string) string {
	sel, _ := json.Marshal(selector)
	return `(()=>{const el=document.querySelector(` + string(sel) + `); if(!el) return false; el.focus(); return true;})()`
}

// selectorSelectJS sets the value of the first element matching selector and dispatches the
// input and change events a real user edit would, or false when nothing matches.
func selectorSelectJS(selector, text string) string {
	sel, _ := json.Marshal(selector)
	val, _ := json.Marshal(text)
	return `(()=>{const el=document.querySelector(` + string(sel) + `); if(!el) return false; el.value=` + string(val) + `; el.dispatchEvent(new Event('input',{bubbles:true})); el.dispatchEvent(new Event('change',{bubbles:true})); return true;})()`
}

// resolveSelector returns the centre of the first element matching selector, or
// errSelectorNotFound when nothing matches.
func resolveSelector(ctx context.Context, sc *cdp.Screencast, selector string) (x, y float64, err error) {
	raw, err := sc.Evaluate(ctx, selectorResolveJS(selector))
	if err != nil {
		return 0, 0, err
	}
	var hit struct {
		Value *struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &hit); err != nil {
		return 0, 0, err
	}
	if hit.Value == nil {
		return 0, 0, errSelectorNotFound
	}
	return hit.Value.X, hit.Value.Y, nil
}

// evalBool evaluates an expression expected to return a boolean, or false when it returns
// nothing (null) or a non-boolean.
func evalBool(ctx context.Context, sc *cdp.Screencast, expr string) (bool, error) {
	raw, err := sc.Evaluate(ctx, expr)
	if err != nil {
		return false, err
	}
	var r struct {
		Value *bool `json:"value"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return false, err
	}
	return r.Value != nil && *r.Value, nil
}

// browserActClick clicks a selector's centre, or the given coordinates when there is no
// selector. Button defaults to left, clicks to one.
func browserActClick(ctx context.Context, sc *cdp.Screencast, b browserActRequest) error {
	x, y := b.X, b.Y
	if b.Selector != "" {
		var err error
		x, y, err = resolveSelector(ctx, sc, b.Selector)
		if err != nil {
			return err
		}
	}
	button := b.Button
	if button == 0 {
		button = 1
	}
	clicks := b.Clicks
	if clicks == 0 {
		clicks = 1
	}
	press := cdp.MouseEvent{Action: "press", X: x, Y: y, Button: button, Buttons: mouseButtonsMask(button), Clicks: clicks}
	if err := sc.Mouse(ctx, press); err != nil {
		return err
	}
	press.Action = "release"
	press.Buttons = 0
	return sc.Mouse(ctx, press)
}

// mouseButtonsMask is the held-buttons bitmask for one button: 1 left, 2 right, 4 middle.
func mouseButtonsMask(button int) int {
	switch button {
	case 1:
		return 1
	case 2:
		return 4
	case 3:
		return 2
	}
	return 0
}

// browserActType focuses the selector's element when one is given, then types the text into
// the focused field.
func browserActType(ctx context.Context, sc *cdp.Screencast, b browserActRequest) error {
	if b.Selector != "" {
		ok, err := evalBool(ctx, sc, selectorFocusJS(b.Selector))
		if err != nil {
			return err
		}
		if !ok {
			return errSelectorNotFound
		}
	}
	return sc.InsertText(ctx, b.Text)
}

// browserActSelect sets a selector's value to the text and dispatches change/input events.
func browserActSelect(ctx context.Context, sc *cdp.Screencast, b browserActRequest) error {
	if b.Selector == "" {
		return errors.New("select requires a selector")
	}
	ok, err := evalBool(ctx, sc, selectorSelectJS(b.Selector, b.Text))
	if err != nil {
		return err
	}
	if !ok {
		return errSelectorNotFound
	}
	return nil
}

// browserActScroll scrolls at a selector's centre, or at the given coordinates.
func browserActScroll(ctx context.Context, sc *cdp.Screencast, b browserActRequest) error {
	x, y := b.X, b.Y
	if b.Selector != "" {
		var err error
		x, y, err = resolveSelector(ctx, sc, b.Selector)
		if err != nil {
			return err
		}
	}
	return sc.Scroll(ctx, x, y, b.DX, b.DY, 0)
}

// browserActKey presses and releases one key by name.
func browserActKey(ctx context.Context, sc *cdp.Screencast, b browserActRequest) error {
	if b.Key == "" {
		return errors.New("key requires a key name")
	}
	ev := cdp.KeyEvent{Key: b.Key, Code: keyCodeFor(b.Key)}
	if len(b.Key) == 1 {
		ev.Text = b.Key
	}
	ev.Action = "down"
	if err := sc.Key(ctx, ev); err != nil {
		return err
	}
	ev.Action = "up"
	return sc.Key(ctx, ev)
}

// keyCodeFor derives a DOM code from a key name: KeyA for letters, Digit7 for digits, Space
// for a space, the name itself for named keys.
func keyCodeFor(name string) string {
	if len(name) == 1 {
		c := name[0]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			return "Key" + strings.ToUpper(name)
		case c >= '0' && c <= '9':
			return "Digit" + name
		case c == ' ':
			return "Space"
		}
	}
	return name
}
