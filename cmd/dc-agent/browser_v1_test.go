package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gtsteffaniak/docker-computer/internal/cdp"
)

// fakeEvalPage is a DevTools page target for the /v1 browser tests. It answers
// Runtime.evaluate: expressions mentioning "throw" raise an exception; "innerText" reads the
// page text; querySelector calls resolve ".hit" to a 50x20 box at (100,200) and anything else
// to null; focus and select expressions report success; everything else echoes the expression
// back as a string value. It records every method and its parameters.
type fakeEvalPage struct {
	mu      sync.Mutex
	methods []string
	params  []json.RawMessage
}

func (f *fakeEvalPage) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var msg struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		f.mu.Lock()
		f.methods = append(f.methods, msg.Method)
		f.params = append(f.params, msg.Params)
		f.mu.Unlock()
		result := map[string]any{}
		switch msg.Method {
		case "Page.getNavigationHistory":
			result["currentIndex"] = 0
			result["entries"] = []map[string]any{{"id": 1, "url": "about:blank", "title": ""}}
		case "Page.captureScreenshot":
			result["data"] = "c2hvdA==" // "shot"
		case "Runtime.evaluate":
			var p struct {
				Expression string `json:"expression"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			switch {
			case strings.Contains(p.Expression, "throw"):
				result["exceptionDetails"] = map[string]any{
					"text":      "Uncaught",
					"exception": map[string]any{"description": "Error: boom"},
				}
			case strings.Contains(p.Expression, "innerText"):
				result["result"] = map[string]any{"type": "string", "value": "page text"}
			case strings.Contains(p.Expression, "dispatchEvent"):
				result["result"] = map[string]any{"type": "boolean", "value": true}
			case strings.Contains(p.Expression, "el.focus()"):
				result["result"] = map[string]any{"type": "boolean", "value": true}
			case strings.Contains(p.Expression, "querySelector"):
				if strings.Contains(p.Expression, ".hit") {
					result["result"] = map[string]any{"type": "object", "value": map[string]any{
						"x": 125, "y": 210, "w": 50, "h": 20,
					}}
				} else {
					result["result"] = map[string]any{"type": "object", "subtype": "null", "value": nil}
				}
			default:
				result["result"] = map[string]any{"type": "string", "value": p.Expression}
			}
		}
		out, _ := json.Marshal(map[string]any{"id": msg.ID, "result": result})
		if conn.Write(r.Context(), websocket.MessageText, out) != nil {
			return
		}
	}
}

// calls returns the parameters of every call to method, in order.
func (f *fakeEvalPage) calls(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for i, m := range f.methods {
		if m != method {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(f.params[i], &p)
		out = append(out, p)
	}
	return out
}

func connectFakeEval(t *testing.T) (*fakeEvalPage, *cdp.Screencast) {
	t.Helper()
	f := &fakeEvalPage{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sc, err := cdp.Connect(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return f, sc
}

// newBrowserV1Agent mounts the /v1 browser routes (plus the session routes, so tests can
// acquire a session) on a test server.
func newBrowserV1Agent(t *testing.T, a *Agent) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	a.registerBrowserV1Routes(mux, a.auth)
	a.registerSessionRoutes(mux, a.auth)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func doV1(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	return doV1Session(t, srv, method, path, body, "")
}

func doV1Session(t *testing.T, srv *httptest.Server, method, path, body, session string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Computer-Token", testToken)
	if session != "" {
		req.Header.Set("X-Dc-Session", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// acquireTestSession acquires a live session and returns its id.
func acquireTestSession(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	code, body := doV1(t, srv, http.MethodPost, "/v1/session/acquire", `{"holder":{"kind":"agent","name":"test"}}`)
	if code != http.StatusCreated {
		t.Fatalf("acquire session: %d %s", code, body)
	}
	var s struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	return s.ID
}

func TestBrowserV1StartDisabled(t *testing.T) {
	t.Setenv("DC_BROWSER_START", "off")
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/browser/start", "", ses)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("start with browser off: %d, want 503", code)
	}
}

func TestBrowserV1StopWhenStopped(t *testing.T) {
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, body := doV1Session(t, srv, http.MethodPost, "/v1/browser/stop", "", ses)
	if code != 200 || !strings.Contains(body, `"running":false`) {
		t.Fatalf("stop: %d %s, want 200 {\"running\":false}", code, body)
	}
}

// TestBrowserV1RequiresSession checks the mutating routes refuse callers without a session.
func TestBrowserV1RequiresSession(t *testing.T) {
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	srv := newBrowserV1Agent(t, a)
	code, body := doV1(t, srv, http.MethodPost, "/v1/browser/stop", "")
	if code != http.StatusConflict || !strings.Contains(body, "no session held") {
		t.Fatalf("stop without a session: %d %s, want 409 no session held", code, body)
	}
}

func TestBrowserV1Evaluate(t *testing.T) {
	f, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, body := doV1Session(t, srv, http.MethodPost, "/v1/browser/evaluate", `{"expression":"1+1"}`, ses)
	if code != 200 {
		t.Fatalf("evaluate: %d %s", code, body)
	}
	// The handler unwraps the CDP RemoteObject: value is the expression's
	// own JSON value (the fake echoes the expression as a string).
	var out struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if string(out.Value) != `"1+1"` {
		t.Fatalf("value = %s, want the bare echoed expression", out.Value)
	}
	calls := f.calls("Runtime.evaluate")
	if len(calls) != 1 || calls[0]["returnByValue"] != true || calls[0]["awaitPromise"] != true {
		t.Fatalf("evaluate calls = %v", calls)
	}
}

func TestBrowserV1EvaluateNoBrowser(t *testing.T) {
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/browser/evaluate", `{"expression":"1"}`, ses)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("evaluate with no browser: %d, want 503", code)
	}
}

func TestBrowserV1EvaluateTooLarge(t *testing.T) {
	_, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	expr := strings.Repeat("x", 65<<10)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/browser/evaluate", `{"expression":"`+expr+`"}`, ses)
	if code != http.StatusBadRequest {
		t.Fatalf("oversize expression: %d, want 400", code)
	}
}

func TestBrowserV1EvaluateThrows(t *testing.T) {
	_, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, body := doV1Session(t, srv, http.MethodPost, "/v1/browser/evaluate", `{"expression":"throw new Error('boom')"}`, ses)
	if code != http.StatusBadGateway || !strings.Contains(body, "Error: boom") {
		t.Fatalf("throwing expression: %d %s", code, body)
	}
}

func TestBrowserV1Text(t *testing.T) {
	_, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	code, body := doV1(t, srv, http.MethodGet, "/v1/browser/text", "")
	if code != 200 || !strings.Contains(body, `"text":"page text"`) {
		t.Fatalf("text: %d %s", code, body)
	}
}

func TestBrowserV1ActClickSelector(t *testing.T) {
	f, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/browser/act", `{"kind":"click","selector":".hit"}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("click: %d, want 204", code)
	}
	mouse := f.calls("Input.dispatchMouseEvent")
	if len(mouse) != 2 || mouse[0]["type"] != "mousePressed" || mouse[1]["type"] != "mouseReleased" {
		t.Fatalf("mouse events = %v, want press then release", mouse)
	}
	if mouse[0]["x"] != 125.0 || mouse[0]["y"] != 210.0 || mouse[0]["button"] != "left" {
		t.Fatalf("press = %v, want the element centre (125,210) left", mouse[0])
	}
}

func TestBrowserV1ActClickCoords(t *testing.T) {
	f, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/browser/act", `{"kind":"click","x":10,"y":20,"button":3,"clicks":2}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("click: %d, want 204", code)
	}
	mouse := f.calls("Input.dispatchMouseEvent")
	if len(mouse) != 2 || mouse[0]["button"] != "right" || mouse[0]["clickCount"] != 2.0 {
		t.Fatalf("mouse events = %v, want a double right-click", mouse)
	}
}

func TestBrowserV1ActUnknownSelector(t *testing.T) {
	_, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, body := doV1Session(t, srv, http.MethodPost, "/v1/browser/act", `{"kind":"click","selector":".miss"}`, ses)
	if code != http.StatusNotFound {
		t.Fatalf("unknown selector: %d, want 404", code)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(body), &out); err != nil || out["error"] != "selector not found" {
		t.Fatalf("body = %s, want {\"error\":\"selector not found\"}", body)
	}
}

func TestBrowserV1ActUnknownKind(t *testing.T) {
	_, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/browser/act", `{"kind":"dance"}`, ses)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown kind: %d, want 400", code)
	}
}

func TestBrowserV1ActType(t *testing.T) {
	f, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/browser/act", `{"kind":"type","selector":".hit","text":"hi"}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("type: %d, want 204", code)
	}
	evs := f.calls("Runtime.evaluate")
	if len(evs) != 1 || !strings.Contains(evs[0]["expression"].(string), "el.focus()") {
		t.Fatalf("evaluate calls = %v, want one focus call", evs)
	}
	ins := f.calls("Input.insertText")
	if len(ins) != 1 || ins[0]["text"] != "hi" {
		t.Fatalf("insertText calls = %v, want one with text hi", ins)
	}
}

func TestBrowserV1ActSelect(t *testing.T) {
	f, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/browser/act", `{"kind":"select","selector":".hit","text":"b"}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("select: %d, want 204", code)
	}
	evs := f.calls("Runtime.evaluate")
	if len(evs) != 1 {
		t.Fatalf("evaluate calls = %v", evs)
	}
	expr := evs[0]["expression"].(string)
	if !strings.Contains(expr, `el.value="b"`) || !strings.Contains(expr, "dispatchEvent") {
		t.Fatalf("select expression = %s", expr)
	}
}

func TestBrowserV1ActScroll(t *testing.T) {
	f, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/browser/act", `{"kind":"scroll","x":5,"y":6,"dy":100}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("scroll: %d, want 204", code)
	}
	mouse := f.calls("Input.dispatchMouseEvent")
	if len(mouse) != 1 || mouse[0]["type"] != "mouseWheel" || mouse[0]["deltaY"] != 100.0 {
		t.Fatalf("mouse events = %v, want one wheel event with deltaY 100", mouse)
	}
}

func TestBrowserV1ActKey(t *testing.T) {
	f, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/browser/act", `{"kind":"key","key":"Enter"}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("key: %d, want 204", code)
	}
	keys := f.calls("Input.dispatchKeyEvent")
	if len(keys) != 2 || keys[0]["type"] != "keyDown" || keys[1]["type"] != "keyUp" {
		t.Fatalf("key events = %v, want down then up", keys)
	}
	if keys[0]["key"] != "Enter" || keys[0]["text"] != "\r" {
		t.Fatalf("key down = %v", keys[0])
	}
}

func TestBrowserV1Aliases(t *testing.T) {
	f, sc := connectFakeEval(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	srv := newBrowserV1Agent(t, a)
	ses := acquireTestSession(t, srv)

	code, body := doV1(t, srv, http.MethodGet, "/v1/browser/state", "")
	if code != 200 || !strings.Contains(body, `"running":true`) {
		t.Fatalf("state: %d %s", code, body)
	}
	code, body = doV1Session(t, srv, http.MethodPost, "/v1/browser/navigate", `{"url":"example.com"}`, ses)
	if code != 200 || !strings.Contains(body, `"url":"https://example.com"`) {
		t.Fatalf("navigate: %d %s", code, body)
	}
	if len(f.calls("Page.navigate")) != 1 {
		t.Fatal("navigate did not reach the page")
	}
	code, _ = doV1Session(t, srv, http.MethodPost, "/v1/browser/input", `{"type":"paste","text":"x"}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("input: %d, want 204", code)
	}
	code, _ = doV1Session(t, srv, http.MethodPost, "/v1/browser/size", `{"w":900,"h":500,"scale":1}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("size: %d, want 204", code)
	}
	for _, path := range []string{"/v1/browser/back", "/v1/browser/forward", "/v1/browser/reload"} {
		code, _ := doV1Session(t, srv, http.MethodPost, path, "", ses)
		if code != http.StatusNoContent {
			t.Fatalf("%s: %d, want 204", path, code)
		}
	}
}

func TestSelectorResolveJSEscapes(t *testing.T) {
	js := selectorResolveJS(`a[href="x"]`)
	if !strings.Contains(js, `document.querySelector("a[href=\"x\"]")`) {
		t.Fatalf("js = %s", js)
	}
	if !strings.Contains(js, "getBoundingClientRect") {
		t.Fatalf("js = %s, want the centre computation", js)
	}
}

func TestKeyCodeFor(t *testing.T) {
	cases := map[string]string{"a": "KeyA", "Z": "KeyZ", "7": "Digit7", " ": "Space", "Enter": "Enter", "ArrowLeft": "ArrowLeft"}
	for in, want := range cases {
		if got := keyCodeFor(in); got != want {
			t.Errorf("keyCodeFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoteValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"type":"number","value":2,"description":"2"}`, `2`},
		{`{"type":"string","value":"x"}`, `"x"`},
		{`{"type":"object","value":{"a":[1,"x"]}}`, `{"a":[1,"x"]}`},
		{`{"type":"object","subtype":"null","value":null}`, `null`},
		{`{"type":"undefined"}`, `null`},
		{`{"type":"boolean","value":false}`, `false`},
	}
	for _, c := range cases {
		got, err := remoteValue(json.RawMessage(c.in))
		if err != nil || string(got) != c.want {
			t.Errorf("remoteValue(%s) = %s, %v; want %s", c.in, got, err, c.want)
		}
	}
	if _, err := remoteValue(json.RawMessage(`not json`)); err == nil {
		t.Error("remoteValue of invalid JSON should fail")
	}
}
