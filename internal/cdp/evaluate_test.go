package cdp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeEvalPage is a DevTools page target that answers Runtime.evaluate. Expressions that
// mention "throw" raise an exception; "innerText" reads the page text; querySelector calls
// resolve ".hit" to a 50x20 box at (100,200) and anything else to null; focus and select
// expressions report success; everything else echoes the expression back as a string value.
type fakeEvalPage struct {
	mu      sync.Mutex
	methods []string
	params  []json.RawMessage
	evals   []string
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
		case "Runtime.evaluate":
			var p struct {
				Expression string `json:"expression"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			f.mu.Lock()
			f.evals = append(f.evals, p.Expression)
			f.mu.Unlock()
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

func (f *fakeEvalPage) evalParams(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.methods {
		if m == "Runtime.evaluate" {
			var p map[string]any
			if err := json.Unmarshal(f.params[i], &p); err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	t.Fatal("no Runtime.evaluate call recorded")
	return nil
}

func connectFakeEval(t *testing.T) (*fakeEvalPage, *Screencast) {
	t.Helper()
	f := &fakeEvalPage{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sc, err := Connect(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return f, sc
}

func TestEvaluateReturnsRemoteObject(t *testing.T) {
	f, sc := connectFakeEval(t)
	raw, err := sc.Evaluate(context.Background(), "1+1")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if r.Type != "string" || r.Value != "1+1" {
		t.Fatalf("Evaluate = %+v, want the echoed expression", r)
	}
	p := f.evalParams(t)
	if p["returnByValue"] != true || p["awaitPromise"] != true {
		t.Fatalf("evaluate params = %v, want returnByValue and awaitPromise true", p)
	}
}

func TestEvaluateReportsExceptions(t *testing.T) {
	_, sc := connectFakeEval(t)
	_, err := sc.Evaluate(context.Background(), "throw new Error('boom')")
	if err == nil || !strings.Contains(err.Error(), "Error: boom") {
		t.Fatalf("err = %v, want the exception description", err)
	}
}

func TestPageText(t *testing.T) {
	_, sc := connectFakeEval(t)
	text, err := sc.PageText(context.Background())
	if err != nil || text != "page text" {
		t.Fatalf("PageText = %q, %v", text, err)
	}
}
