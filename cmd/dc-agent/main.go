// dc-agent (Phase 0 spike) holds the sole Broadway client and exposes spike HTTP endpoints.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gtsteffaniak/docker-computer/internal/broadway"
	"github.com/gtsteffaniak/docker-computer/internal/cdp"
)

// SpikeAgent wires Broadway and optional Chromium CDP for Phase 0 spike HTTP endpoints.
type SpikeAgent struct {
	token    string
	broadway *broadway.Client
	cdp      *cdp.Screencast
	mu       sync.Mutex
}

func main() {
	token := os.Getenv("COMPUTER_AGENT_TOKEN")
	if token == "" {
		log.Fatal("COMPUTER_AGENT_TOKEN is required")
	}
	host := envOr("BROADWAY_HOST", "127.0.0.1:8081")
	w, _ := strconv.Atoi(envOr("DESKTOP_WIDTH", "1280"))
	h, _ := strconv.Atoi(envOr("DESKTOP_HEIGHT", "800"))
	client := broadway.NewClient(broadway.Config{HostPort: host, Width: w, Height: h})
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	var connErr error
	for attempt := 0; attempt < 50; attempt++ {
		connErr = client.Connect(ctx)
		if connErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if connErr != nil {
		log.Fatalf("broadway connect: %v", connErr)
	}
	defer client.Close()

	agent := &SpikeAgent{token: token, broadway: client}
	if err := agent.maybeStartChromium(ctx); err != nil {
		log.Printf("chromium (optional for idle): %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/health", agent.auth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	mux.HandleFunc("/v1/meta", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"api_version": "0", "phase": "spike"})
	})
	mux.HandleFunc("/spike/broadway/meta", agent.auth(agent.handleBroadwayMeta))
	mux.HandleFunc("/spike/screenshot", agent.auth(agent.handleScreenshot))
	mux.HandleFunc("/spike/input/click", agent.auth(agent.handleClick))
	mux.HandleFunc("/spike/input/type", agent.auth(agent.handleType))
	mux.HandleFunc("/spike/browser/frame", agent.auth(agent.handleBrowserFrame))
	mux.HandleFunc("/spike/apps/start", agent.auth(agent.handleAppStart))

	addr := envOr("DC_AGENT_ADDR", "0.0.0.0:8080")
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("dc-agent spike listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (a *SpikeAgent) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Computer-Token") != a.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (a *SpikeAgent) handleBroadwayMeta(w http.ResponseWriter, _ *http.Request) {
	b, _ := a.broadway.MarshalDebug()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (a *SpikeAgent) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	wait := r.URL.Query().Get("wait") == "1"
	if wait {
		c, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		_ = a.broadway.WaitFrame(c, 100*time.Millisecond)
	}
	png, err := a.broadway.ScreenshotPNG()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

func (a *SpikeAgent) handleClick(w http.ResponseWriter, r *http.Request) {
	var body struct {
		X int32 `json:"x"`
		Y int32 `json:"y"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.broadway.ClickAt(body.X, body.Y); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *SpikeAgent) handleType(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.broadway.TypeText(body.Text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *SpikeAgent) handleBrowserFrame(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	sc := a.cdp
	a.mu.Unlock()
	if sc == nil {
		http.Error(w, "chromium not running", http.StatusServiceUnavailable)
		return
	}
	frame, err := sc.WaitJPEG(r.Context(), 100*time.Millisecond)
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(frame)
}

func (a *SpikeAgent) handleAppStart(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	cmd := appCommand(id)
	if cmd == nil {
		http.Error(w, "unknown app", http.StatusBadRequest)
		return
	}
	cmd.Env = append(os.Environ(),
		"GDK_BACKEND=broadway",
		"BROADWAY_DISPLAY="+envOr("BROADWAY_DISPLAY", ":1"))
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"id": id, "pid": cmd.Process.Pid})
}

func (a *SpikeAgent) maybeStartChromium(ctx context.Context) error {
	if os.Getenv("SPIKE_START_CHROMIUM") != "1" {
		return nil
	}
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
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ws, err := cdp.DiscoverWS(ctx, "127.0.0.1:9222")
		if err == nil {
			sc, err := cdp.Connect(ctx, ws)
			if err == nil {
				a.mu.Lock()
				a.cdp = sc
				a.mu.Unlock()
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("chromium CDP not ready")
}

func appCommand(id string) *exec.Cmd {
	switch id {
	case "terminal":
		return exec.Command("lxterminal")
	case "editor":
		return exec.Command("mousepad")
	case "files":
		return exec.Command("thunar")
	default:
		return nil
	}
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
