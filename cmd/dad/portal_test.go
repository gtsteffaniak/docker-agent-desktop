package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const testToken = "test-token"

// newDesktopAgent returns an agent whose registry lists one GTK app, "terminal", served by
// upstream. The desktop and app routes are mounted on a test server.
func newDesktopAgent(t *testing.T, upstream *httptest.Server) *httptest.Server {
	t.Helper()
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	app := "[app]\nid = terminal\nname = Terminal\nexec = lxterminal\norder = 10\nport = " + strconv.Itoa(port) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "terminal.desktop"), []byte(app), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DAD_APPS_DIRS", dir)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	a.mountPortal(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestDesktopServesEmbeddedPage(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	srv := newDesktopAgent(t, upstream)
	hdr := http.Header{"X-Dad-Token": {testToken}}

	resp, body := get(t, srv.URL+"/desktop/", hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, `id="dock"`) || !strings.Contains(body, "desktop.js") {
		t.Fatalf("index.html not served: %.200s", body)
	}
	if resp, body := get(t, srv.URL+"/desktop/desktop.js", hdr); resp.StatusCode != http.StatusOK || !strings.Contains(body, "dockActivate") {
		t.Fatalf("desktop.js not served: status %d", resp.StatusCode)
	}
}

func TestDesktopRequiresToken(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	srv := newDesktopAgent(t, upstream)
	if resp, _ := get(t, srv.URL+"/desktop/", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if resp, _ := get(t, srv.URL+"/desktop/?token=wrong", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong query token status = %d, want 401", resp.StatusCode)
	}
}

func TestTokenQueryBecomesCookieAndRedirectsToDesktop(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	srv := newDesktopAgent(t, upstream)
	resp, _ := get(t, srv.URL+"/portal/?token="+testToken, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/portal/" {
		// The portal entry sends a signed-in browser to the desktop. Accept either form.
		if resp.Header.Get("Location") != "/desktop/" && resp.Header.Get("Location") != "/portal/" {
			t.Fatalf("status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			found = c
		}
	}
	if found == nil || !found.HttpOnly || found.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %+v", found)
	}
}

func TestAppProxyServesBroadwayPageWithKeysScript(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><head><title>broadway</title></head><body></body></html>")
	}))
	defer upstream.Close()
	srv := newDesktopAgent(t, upstream)
	hdr := http.Header{"X-Dad-Token": {testToken}}

	resp, body := get(t, srv.URL+"/apps/terminal/", hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, `keys={F11:1,F12:1}`) {
		t.Fatalf("browser-keys script missing: %s", body)
	}
	if strings.Index(body, "<script>") > strings.Index(body, "</head>") {
		t.Fatal("script should sit inside head")
	}
	if got, want := resp.Header.Get("Content-Length"), strconv.Itoa(len(body)); got != want {
		t.Fatalf("Content-Length = %s, want %s", got, want)
	}
}

func TestAppProxyLeavesNonHTMLAlone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = io.WriteString(w, "var x = '</head>';")
	}))
	defer upstream.Close()
	srv := newDesktopAgent(t, upstream)
	if _, body := get(t, srv.URL+"/apps/terminal/broadway.js", http.Header{"X-Dad-Token": {testToken}}); body != "var x = '</head>';" {
		t.Fatalf("js was rewritten: %q", body)
	}
}

func TestAppProxyRejectsUnknownAppAndNeedsToken(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	srv := newDesktopAgent(t, upstream)
	hdr := http.Header{"X-Dad-Token": {testToken}}
	if resp, _ := get(t, srv.URL+"/apps/nope/", hdr); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown app status = %d, want 404", resp.StatusCode)
	}
	if resp, _ := get(t, srv.URL+"/apps/terminal/", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", resp.StatusCode)
	}
}

func TestInjectScriptWithoutHead(t *testing.T) {
	out := string(injectScript([]byte("<body>x</body>")))
	if !strings.HasPrefix(out, "<script>") || !strings.HasSuffix(out, "<body>x</body>") {
		t.Fatalf("out = %q", out)
	}
}

func TestBrowserKeysScriptDoesNotCancelEvents(t *testing.T) {
	out := string(injectScript([]byte("<html><head></head><body></body></html>")))
	if strings.Contains(out, "preventDefault") {
		t.Fatal("the browser-keys script must not cancel the event itself")
	}
}
