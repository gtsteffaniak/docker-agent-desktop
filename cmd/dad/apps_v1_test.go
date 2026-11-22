package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// writeTestApps writes a two-app registry: a GTK app on port 8081 and the browser on port 0.
func writeTestApps(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	terminal := "[app]\nid = terminal\nname = Terminal\nexec = lxterminal\nicon = lxterminal\norder = 10\nport = 8081\n"
	browser := "[app]\nid = browser\nname = Browser\nexec = chromium\nicon = chromium\norder = 5\nport = 0\n"
	if err := os.WriteFile(filepath.Join(dir, "terminal.desktop"), []byte(terminal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "browser.desktop"), []byte(browser), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newAppsV1Agent(t *testing.T) (*Agent, *httptest.Server) {
	t.Helper()
	t.Setenv("DAD_APPS_DIRS", writeTestApps(t))
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	a.registerAppsV1Routes(mux, a.auth)
	a.registerSessionRoutes(mux, a.auth)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return a, srv
}

func TestAppsV1List(t *testing.T) {
	_, srv := newAppsV1Agent(t)
	code, body := doV1(t, srv, http.MethodGet, "/v1/apps", "")
	if code != 200 {
		t.Fatalf("list: %d %s", code, body)
	}
	if strings.Contains(body, `"exec"`) {
		t.Fatalf("list exposes exec: %s", body)
	}
	var out struct {
		Apps []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Icon    string `json:"icon"`
			Order   int    `json:"order"`
			Port    int    `json:"port"`
			Running bool   `json:"running"`
		} `json:"apps"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Apps) != 2 {
		t.Fatalf("apps = %v", out.Apps)
	}
	// Sorted by order: browser first.
	if out.Apps[0].ID != "browser" || out.Apps[0].Port != 0 || out.Apps[0].Running {
		t.Fatalf("browser = %+v, want port 0, not running", out.Apps[0])
	}
	if out.Apps[1].ID != "terminal" || out.Apps[1].Name != "Terminal" || out.Apps[1].Port != 8081 || out.Apps[1].Running {
		t.Fatalf("terminal = %+v, want port 8081, not running", out.Apps[1])
	}
}

func TestAppsV1StartUnknown(t *testing.T) {
	_, srv := newAppsV1Agent(t)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/apps/start?id=nope", "", ses)
	if code != http.StatusBadRequest {
		t.Fatalf("start unknown: %d, want 400", code)
	}
}

func TestAppsV1StartBrowser(t *testing.T) {
	_, srv := newAppsV1Agent(t)
	ses := acquireTestSession(t, srv)
	code, body := doV1Session(t, srv, http.MethodPost, "/v1/apps/start?id=browser", "", ses)
	if code != 200 || !strings.Contains(body, `"running":false`) {
		t.Fatalf("start browser: %d %s, want 200 running false (no screencast)", code, body)
	}
}

func TestAppsV1StopUnknown(t *testing.T) {
	_, srv := newAppsV1Agent(t)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/apps/stop?id=nope", "", ses)
	if code != http.StatusBadRequest {
		t.Fatalf("stop unknown: %d, want 400", code)
	}
}

func TestAppsV1StopBrowser(t *testing.T) {
	_, srv := newAppsV1Agent(t)
	ses := acquireTestSession(t, srv)
	code, body := doV1Session(t, srv, http.MethodPost, "/v1/apps/stop?id=browser", "", ses)
	if code != 200 || !strings.Contains(body, `"running":false`) {
		t.Fatalf("stop browser: %d %s, want 200 {\"running\":false}", code, body)
	}
}

// TestAppsV1RequiresSession checks app start/stop refuse callers without a session.
func TestAppsV1RequiresSession(t *testing.T) {
	_, srv := newAppsV1Agent(t)
	code, body := doV1(t, srv, http.MethodPost, "/v1/apps/stop?id=browser", "")
	if code != http.StatusConflict || !strings.Contains(body, "no session held") {
		t.Fatalf("stop without a session: %d %s, want 409 no session held", code, body)
	}
}

// TestStopAppKillsProcess tracks a real sleep process in the app map, then checks stopApp
// ends it and appRunning reflects that. A reaper goroutine plays startApp's Wait role, since
// stopApp waits for the process to be reaped.
func TestStopAppKillsProcess(t *testing.T) {
	const id = "test-stop-app"
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep not available: " + err.Error())
	}
	// A reaper goroutine plays startApp's Wait role, since stopApp waits for the process
	// to be reaped. It is the only Wait caller; the test synchronises on it.
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		appProcs.mu.Lock()
		delete(appProcs.apps, id)
		appProcs.mu.Unlock()
		_ = cmd.Process.Kill()
		<-reaped
	})
	appProcs.mu.Lock()
	appProcs.apps[id] = cmd
	appProcs.mu.Unlock()

	a := &Agent{log: slog.New(slog.DiscardHandler)}
	if !a.appRunning(id) {
		t.Fatal("appRunning is false for a tracked live process")
	}
	if err := a.stopApp(id); err != nil {
		t.Fatalf("stopApp: %v", err)
	}
	<-reaped // the reaper's Wait is done, so ProcessState is settled
	if a.appRunning(id) {
		t.Fatal("appRunning is still true after stopApp")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		// Signal 0 succeeding means the process still exists.
		t.Fatal("process still exists after stopApp")
	}
}

func TestStopAppNotRunning(t *testing.T) {
	a := &Agent{log: slog.New(slog.DiscardHandler)}
	if a.appRunning("never-started") {
		t.Fatal("appRunning is true for an untracked app")
	}
	if err := a.stopApp("never-started"); err != nil {
		t.Fatalf("stopApp of an untracked app = %v, want nil", err)
	}
}
