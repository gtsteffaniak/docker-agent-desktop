package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gtsteffaniak/docker-computer/internal/cdp"
)

const browserCallTimeout = 5 * time.Second

// registerBrowser adds the browser window routes. wrap guards them: the TCP mux passes the
// token check, and the unix socket mux (private to the container user) passes them through.
func (a *Agent) registerBrowser(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/browser/frame", wrap(a.handleBrowserFrame))
	mux.HandleFunc("GET /api/browser/state", wrap(a.handleBrowserState))
	mux.HandleFunc("POST /api/browser/navigate", wrap(a.handleBrowserNavigate))
	mux.HandleFunc("POST /api/browser/back", wrap(a.browserAction((*cdp.Screencast).GoBack)))
	mux.HandleFunc("POST /api/browser/forward", wrap(a.browserAction((*cdp.Screencast).GoForward)))
	mux.HandleFunc("POST /api/browser/reload", wrap(a.browserAction((*cdp.Screencast).Reload)))
	mux.HandleFunc("POST /api/browser/input", wrap(a.handleBrowserInput))
	mux.HandleFunc("POST /api/browser/size", wrap(a.handleBrowserSize))
	mux.HandleFunc("GET /api/browser/ws", wrap(a.handleBrowserWS))
}

func (a *Agent) screencast() *cdp.Screencast {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cdp
}

// current returns the attached screencast or writes a 503 and returns nil.
func (a *Agent) current(w http.ResponseWriter) *cdp.Screencast {
	a.touch()
	sc := a.screencast()
	if sc == nil {
		http.Error(w, "chromium not running", http.StatusServiceUnavailable)
	}
	return sc
}

// handleBrowserFrame returns the latest screencast JPEG. With ?since=N it returns 204 when no
// frame newer than N exists, so the browser window can poll cheaply.
func (a *Agent) handleBrowserFrame(w http.ResponseWriter, r *http.Request) {
	sc := a.current(w)
	if sc == nil {
		return
	}
	if q := r.URL.Query().Get("since"); q != "" {
		since, _ := strconv.ParseUint(q, 10, 64)
		frame, seq := sc.Frame()
		w.Header().Set("X-Frame-Seq", strconv.FormatUint(seq, 10))
		if len(frame) == 0 || seq <= since {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(frame)
		return
	}
	c, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := sc.WaitJPEG(c); err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}
	frame, seq := sc.Frame()
	w.Header().Set("X-Frame-Seq", strconv.FormatUint(seq, 10))
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(frame)
}

// handleBrowserState returns the page's real URL, title and history availability. ?fmt=lines
// returns a plain text form for the C browser window, so it needs no JSON parser:
// url, title, back=0|1, forward=0|1, seq=N, one per line.
func (a *Agent) handleBrowserState(w http.ResponseWriter, r *http.Request) {
	sc := a.screencast()
	if sc == nil {
		// Chromium has not started yet (lazy start). The window shows its start page until then.
		writeJSON(w, browserStateOut{Running: false})
		return
	}
	st := sc.Nav()
	if r.URL.Query().Get("fmt") == "lines" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "%s\n%s\nback=%d\nforward=%d\nseq=%d\n",
			oneLine(st.URL), oneLine(st.Title), b2i(st.CanGoBack), b2i(st.CanGoForward), st.Seq)
		return
	}
	writeJSON(w, browserStateOut{NavState: st, Running: true})
}

// browserStateOut is the state JSON: the page's navigation state plus whether Chromium is up.
type browserStateOut struct {
	cdp.NavState
	Running bool `json:"running"`
}

func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (a *Agent) handleBrowserNavigate(w http.ResponseWriter, r *http.Request) {
	// Navigating is what starts Chromium, so the first address the user enters starts it.
	sc, err := a.ensureBrowser(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := NormalizeURL(body.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), browserCallTimeout)
	defer cancel()
	if err := sc.Navigate(ctx, target); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"url": target})
}

// browserAction wraps a no-argument screencast action (back, forward, reload).
func (a *Agent) browserAction(fn func(*cdp.Screencast, context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc := a.current(w)
		if sc == nil {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), browserCallTimeout)
		defer cancel()
		if err := fn(sc, ctx); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleBrowserInput forwards one pointer, scroll, key or paste event to the page.
func (a *Agent) handleBrowserInput(w http.ResponseWriter, r *http.Request) {
	sc := a.current(w)
	if sc == nil {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), browserCallTimeout)
	defer cancel()
	switch head.Type {
	case "pointer":
		var ev cdp.MouseEvent
		if err = json.Unmarshal(raw, &ev); err == nil {
			err = sc.Mouse(ctx, ev)
		}
	case "key":
		var ev cdp.KeyEvent
		if err = json.Unmarshal(raw, &ev); err == nil {
			err = sc.Key(ctx, ev)
		}
	case "scroll":
		var ev struct {
			X, Y, DX, DY float64
			Mods         int
		}
		if err = json.Unmarshal(raw, &ev); err == nil {
			err = sc.Scroll(ctx, ev.X, ev.Y, ev.DX, ev.DY, ev.Mods)
		}
	case "paste":
		var ev struct{ Text string }
		if err = json.Unmarshal(raw, &ev); err == nil {
			err = sc.InsertText(ctx, ev.Text)
		}
	default:
		http.Error(w, "unknown input type", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleBrowserSize(w http.ResponseWriter, r *http.Request) {
	sc := a.current(w)
	if sc == nil {
		return
	}
	var body struct {
		W     int     `json:"w"`
		H     int     `json:"h"`
		Scale float64 `json:"scale"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), browserCallTimeout)
	defer cancel()
	if err := sc.SetViewport(ctx, body.W, body.H, body.Scale); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var (
	schemeRE   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	hostPortRE = regexp.MustCompile(`^[^/:?#\s]+:\d+([/?#].*)?$`)
)

// NormalizeURL turns what the user typed into an address Chromium can load. Bare hostnames get
// https, local hosts and IP addresses get http, and host:port is not mistaken for a scheme.
// Only http, https and about:blank are allowed. There is deliberately no search fallback: that
// would send what the user types to an external service, which is a decision for the operator.
func NormalizeURL(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", errors.New("empty address")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return "", errors.New("not a web address")
	}
	if schemeRE.MatchString(s) && !hostPortRE.MatchString(s) {
		lower := strings.ToLower(s)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || lower == "about:blank" {
			return s, nil
		}
		return "", errors.New("unsupported address scheme")
	}
	host := s
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	if host == "localhost" || net.ParseIP(host) != nil {
		return "http://" + s, nil
	}
	return "https://" + s, nil
}

// browserHome is the page the browser window opens first. DC_BROWSER_HOME overrides it, and
// an empty value leaves Chromium on about:blank.
func browserHome() string {
	return envOr("DC_BROWSER_HOME", "https://example.com")
}

// openHome loads the start page once Chromium has a screencast. A failure is logged and the
// browser stays on about:blank, so the window still works.
func (a *Agent) openHome(ctx context.Context, sc *cdp.Screencast) {
	home := browserHome()
	if home == "" {
		return
	}
	target, err := NormalizeURL(home)
	if err != nil {
		a.log.Warn("DC_BROWSER_HOME is not a web address", "err", err)
		return
	}
	nctx, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	if err := sc.Navigate(nctx, target); err != nil {
		a.log.Warn("browser start page failed", "url", target, "err", err)
	}
}
