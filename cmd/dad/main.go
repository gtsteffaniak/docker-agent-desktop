// dad serves the web desktop, proxies each GTK app's Broadway display, and streams the browser.
package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gtsteffaniak/docker-agent-desktop/internal/apps"
	"github.com/gtsteffaniak/docker-agent-desktop/internal/background"
	"github.com/gtsteffaniak/docker-agent-desktop/internal/broadway"
	"github.com/gtsteffaniak/docker-agent-desktop/internal/cdp"
	"github.com/gtsteffaniak/docker-agent-desktop/internal/logging"
)

const cookieName = "dad_token"

// Agent wires Broadway and optional Chromium CDP into the HTTP handlers.
type Agent struct {
	token    string
	log      *slog.Logger
	broadway *broadway.Client
	bg       *background.Spec

	mu  sync.Mutex
	cdp *cdp.Screencast

	// root is the process context. Chromium's supervisor runs under it, not under a request.
	root      context.Context
	browserMu sync.Mutex
	browser   *browserRun // the running Chromium supervisor, nil when none is running
	// lastActivity is the last time a client sent a command or opened the browser window. It drives
	// the idle shutdown in browser_idle.go.
	lastActivity atomic.Int64

	// startedAt is when this agent process started, for uptime in health/session state.
	startedAt time.Time
	// sessions is the single live session lease, if any (see session.go).
	sessions sessionStore
	// events is the in-memory activity log (see events.go).
	events eventLog
	// healthDegraded remembers the last /v1/health status so a transition into
	// degraded is logged once instead of on every poll. It is atomic because
	// concurrent health polls read and flip it.
	healthDegraded atomic.Bool
}

func main() {
	token, legacyToken := tokenFromEnv(os.Getenv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "DAD_TOKEN is required")
		os.Exit(2)
	}
	log, err := logging.New(logging.FromEnv())
	if err != nil {
		fmt.Fprintf(os.Stderr, "logging: %v\n", err)
		os.Exit(2)
	}
	if legacyToken {
		log.Warn("COMPUTER_AGENT_TOKEN is deprecated; set DAD_TOKEN instead")
	}
	if err := run(token, log); err != nil {
		log.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}

func run(token string, log *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	host := envOr("BROADWAY_HOST", "127.0.0.1:8081")
	w, _ := strconv.Atoi(envOr("DESKTOP_WIDTH", "1280"))
	h, _ := strconv.Atoi(envOr("DESKTOP_HEIGHT", "800"))
	bg, err := background.Parse(os.Getenv("DAD_BACKGROUND"))
	if err != nil {
		return fmt.Errorf("DAD_BACKGROUND: %w", err)
	}
	client := broadway.NewClient(broadway.Config{HostPort: host, Width: w, Height: h, Backdrop: bg}, log)
	go func() { _ = client.Run(ctx) }()
	defer client.Close()

	a := &Agent{token: token, log: log, broadway: client, bg: bg, root: ctx, startedAt: time.Now()}
	go a.watchBrowserIdle(ctx)
	go a.maybeStartChromium(ctx)
	go a.autostartApps()
	go a.reapSessions(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	// /v1/health is the real session health check, registered by registerV1 below.
	// /healthz above stays a bare 200: container liveness only.
	mux.HandleFunc("GET /v1/meta", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"api_version": "1"})
	})
	mux.HandleFunc("GET /api/display/meta", a.auth(a.handleBroadwayMeta))
	mux.HandleFunc("GET /api/display/screenshot", a.auth(a.handleScreenshot))
	mux.HandleFunc("POST /api/display/click", a.auth(a.handleClick))
	mux.HandleFunc("POST /api/display/type", a.auth(a.handleType))
	a.registerBrowser(mux, a.auth)
	mux.HandleFunc("GET /api/apps", a.auth(a.handleApps))
	mux.HandleFunc("POST /api/apps/start", a.auth(a.handleAppStart))
	a.registerV1(mux, a.auth)
	a.registerMCP(mux, a.auth)
	a.mountPortal(mux)

	addr := envOr("DAD_ADDR", "0.0.0.0:8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           a.logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Deprecated names from before the docker-computer -> docker-agent-desktop rename. They are
// still accepted so existing clients (quantum-bots' adapter) keep working; remove them once those
// clients send the new names.
const (
	legacyTokenEnv      = "COMPUTER_AGENT_TOKEN"
	tokenHeader         = "X-Dad-Token"
	legacyTokenHeader   = "X-Computer-Token"
	legacySessionHeader = "X-Dc-Session"
)

// tokenFromEnv reads DAD_TOKEN, falling back to the deprecated COMPUTER_AGENT_TOKEN. legacy
// reports that the fallback was used.
func tokenFromEnv(getenv func(string) string) (token string, legacy bool) {
	if v := getenv("DAD_TOKEN"); v != "" {
		return v, false
	}
	if v := getenv(legacyTokenEnv); v != "" {
		return v, true
	}
	return "", false
}

// tokenFrom returns the request's token: header first (X-Dad-Token, then the deprecated
// X-Computer-Token), then the portal cookie.
func tokenFrom(r *http.Request) string {
	if v := r.Header.Get(tokenHeader); v != "" {
		return v
	}
	if v := r.Header.Get(legacyTokenHeader); v != "" {
		return v
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

// validToken compares in constant time so timing cannot reveal the token.
func (a *Agent) validToken(got string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1
}

func (a *Agent) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.validToken(tokenFrom(r)) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// logRequests logs method, path, status and duration. It never logs headers, query strings
// or cookies, so tokens stay out of the log.
func (a *Agent) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		// The browser window polls frames and state, and sends input on every pointer move and key.
		// Keep those high-frequency routes out of the info log.
		if quietRoute(r.URL.Path) {
			a.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rec.status)
			return
		}
		a.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// quietRoute reports routes that the browser window calls many times a second.
func quietRoute(path string) bool {
	switch path {
	case "/api/browser/frame", "/api/browser/state", "/api/browser/input", "/api/browser/ws":
		return true
	}
	return false
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader records the status code and passes it on.
func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Hijack keeps WebSocket upgrades working through the recorder.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	return h.Hijack()
}

func (a *Agent) handleBroadwayMeta(w http.ResponseWriter, _ *http.Request) {
	b, _ := a.broadway.MarshalDebug()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (a *Agent) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("wait") == "1" {
		c, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := a.broadway.WaitFrame(c, 100*time.Millisecond); err != nil {
			http.Error(w, "no frame yet: "+err.Error(), http.StatusGatewayTimeout)
			return
		}
	}
	png, err := a.broadway.ScreenshotPNG()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

func (a *Agent) handleClick(w http.ResponseWriter, r *http.Request) {
	var body struct {
		X int32 `json:"x"`
		Y int32 `json:"y"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.broadway.ClickAt(body.X, body.Y); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleType(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.broadway.TypeText(body.Text); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// appDirs lists the registry directories, system first so user entries override it.
func appDirs() []string {
	def := "/etc/dad/apps.d:" + filepath.Join(envOr("HOME", "/home/bot"), ".config/dad/apps.d")
	return filepath.SplitList(envOr("DAD_APPS_DIRS", def))
}

// loadApps reads the registry on every call so edits apply without a restart.
func (a *Agent) loadApps() []apps.App {
	list, errs := apps.Load(appDirs()...)
	for _, err := range errs {
		a.log.Warn("skipped app entry", "err", err)
	}
	return list
}

func (a *Agent) handleApps(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"apps": a.loadApps()})
}

// handleAppStart starts one registry app: its Broadway display first, then the app. Starting an
// app that is already running does nothing. The browser has no process to start here; Chromium
// is started by maybeStartChromium.
func (a *Agent) handleAppStart(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	app, ok := apps.Find(a.loadApps(), id)
	if !ok {
		http.Error(w, "unknown app", http.StatusBadRequest)
		return
	}
	if app.Port == 0 {
		writeJSON(w, map[string]any{"id": id, "running": a.screencast() != nil})
		return
	}
	if err := a.startApp(app); err != nil {
		a.log.Warn("app start failed", "app", id, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"id": id, "running": true})
}

// browserMode reads DAD_BROWSER_START. "boot" starts Chromium with the agent. "off" disables the
// browser. Anything else, including unset, starts Chromium the first time the browser window needs
// it, so an idle desktop does not pay for a browser process.
func browserMode() string {
	return envOr("DAD_BROWSER_START", "lazy")
}

// maybeStartChromium starts the Chromium supervisor at boot when DAD_BROWSER_START=boot.
func (a *Agent) maybeStartChromium(ctx context.Context) {
	if browserMode() == "boot" {
		a.touch()
		a.startBrowserLoop(ctx)
	}
}

// startBrowserLoop starts the Chromium supervisor unless one is running. It keeps a screencast
// attached and reattaches if Chromium exits. stopBrowser ends it.
func (a *Agent) startBrowserLoop(root context.Context) {
	a.browserMu.Lock()
	defer a.browserMu.Unlock()
	if a.browser != nil {
		return
	}
	ctx, cancel := context.WithCancel(root)
	a.browser = &browserRun{stop: cancel}
	go a.chromiumLoop(ctx)
}

func (a *Agent) chromiumLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if err := a.startChromiumOnce(ctx); err != nil {
			a.log.Warn("chromium not available", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// ensureBrowser starts Chromium on first use and waits until its screencast is attached.
func (a *Agent) ensureBrowser(ctx context.Context) (*cdp.Screencast, error) {
	if browserMode() == "off" {
		return nil, errors.New("browser is disabled (DAD_BROWSER_START=off)")
	}
	a.touch()
	if sc := a.screencast(); sc != nil {
		return sc, nil
	}
	root := a.root
	if root == nil {
		root = context.Background()
	}
	a.startBrowserLoop(root)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if sc := a.screencast(); sc != nil {
			return sc, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, errors.New("chromium did not start")
}

func (a *Agent) startChromiumOnce(ctx context.Context) error {
	args := []string{
		"--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--remote-debugging-address=127.0.0.1", "--remote-debugging-port=9222",
		"about:blank",
	}
	chrome := "chromium"
	if _, err := exec.LookPath(chrome); err != nil {
		chrome = "/usr/bin/chromium"
	}
	cmd := exec.CommandContext(ctx, chrome, args...)
	// Chromium forks renderer and utility processes. Give it its own process group, and kill the
	// whole group when the context ends, so none of them outlive the browser.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		return err
	}
	a.log.Info("chromium started", "pid", cmd.Process.Pid)
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ws, err := cdp.DiscoverWS(ctx, "127.0.0.1:9222")
		if err == nil {
			sc, err := cdp.Connect(ctx, ws)
			if err == nil {
				a.mu.Lock()
				a.cdp = sc
				a.openHome(ctx, sc)
				a.mu.Unlock()
				a.log.Info("screencast attached")
				select {
				case <-sc.Done():
				case <-exited:
				case <-ctx.Done():
				}
				a.mu.Lock()
				a.cdp = nil
				a.mu.Unlock()
				_ = sc.Close()
				a.log.Warn("screencast detached")
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			return errors.New("chromium exited before CDP was ready")
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("chromium CDP not ready")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
