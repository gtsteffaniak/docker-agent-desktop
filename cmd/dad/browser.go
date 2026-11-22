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

	"github.com/gtsteffaniak/docker-agent-desktop/internal/cdp"
)

const browserCallTimeout = 5 * time.Second

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

// browserNavigateRequest is the body of POST /v1/browser/navigate.
type browserNavigateRequest struct {
	URL string `json:"url"`
}

// browserNavigateResponse is the reply to POST /v1/browser/navigate: the normalized URL
// that was navigated to.
type browserNavigateResponse struct {
	URL string `json:"url"`
}

// browserInputRequest is the body of POST /v1/browser/input: one pointer, scroll, key or
// paste event. Which fields apply depends on type.
type browserInputRequest struct {
	Type    string  `json:"type"` // pointer | key | scroll | paste
	Action  string  `json:"action,omitempty"`
	X       float64 `json:"x,omitempty"`
	Y       float64 `json:"y,omitempty"`
	Button  int     `json:"button,omitempty"`
	Buttons int     `json:"buttons,omitempty"`
	Clicks  int     `json:"clicks,omitempty"`
	Mods    int     `json:"mods,omitempty"`
	Key     string  `json:"key,omitempty"`
	Code    string  `json:"code,omitempty"`
	Text    string  `json:"text,omitempty"`
	DX      float64 `json:"dx,omitempty"`
	DY      float64 `json:"dy,omitempty"`
}

// browserSizeRequest is the body of POST /v1/browser/size.
type browserSizeRequest struct {
	W     int     `json:"w"`
	H     int     `json:"h"`
	Scale float64 `json:"scale,omitempty"`
}

// handleBrowserFrame returns an image of the page.
//
// Without ?since it takes a fresh screenshot with Page.captureScreenshot, so it works whether or
// not anyone is watching the stream. ?format=png returns a lossless PNG (easier for agents to
// read text from); the default is JPEG, as before.
//
// With ?since=N it serves the screencast for a polling client: each poll keeps the stream running
// for cdp.DefaultLinger, and the reply is 204 when no frame newer than N exists. The first poll
// after the stream was idle waits briefly for a fresh frame, so it does not return a stale one.
// @Summary Browser frame
// @Description One frame of the page. Without ?since it takes a fresh screenshot (JPEG by default, PNG with ?format=png). With ?since=N it serves the screencast for a polling client: 204 when no frame newer than N exists. The X-Frame-Seq header carries the frame sequence number to poll with next.
// @Tags browser
// @Produce jpeg
// @Produce png
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param format query string false "Image format" Enums(jpeg, png)
// @Param since query integer false "Poll: only a frame newer than this sequence number (JPEG only)"
// @Success 200 {file} string "Frame image"
// @Success 204 "No frame newer than ?since="
// @Failure 400 {string} string "Unknown format, or since polling with format=png"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 503 {string} string "Chromium not running"
// @Failure 504 {string} string "Screenshot timed out"
// @Header 200 {string} X-Frame-Seq "Sequence number of the frame"
// @Security ApiKeyAuth
// @Router /v1/browser/frame [get]
func (a *Agent) handleBrowserFrame(w http.ResponseWriter, r *http.Request) {
	sc := a.current(w)
	if sc == nil {
		return
	}
	q := r.URL.Query()
	format := q.Get("format")
	if q.Has("since") {
		if format != "" && format != "jpeg" {
			http.Error(w, "since polling only serves jpeg frames", http.StatusBadRequest)
			return
		}
		since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
		if base := sc.Keep(); base > 0 || !sc.Streaming() {
			// The stream was idle: wait for its first frame rather than answer with a stale one.
			c, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			_, _, _ = sc.WaitNewer(c, base)
			cancel()
		}
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
	contentType := "image/jpeg"
	switch format {
	case "", "jpeg":
		format = "jpeg"
	case "png":
		contentType = "image/png"
	default:
		http.Error(w, "format must be jpeg or png", http.StatusBadRequest)
		return
	}
	c, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	img, err := sc.Capture(c, format)
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}
	_, seq := sc.Frame()
	w.Header().Set("X-Frame-Seq", strconv.FormatUint(seq, 10))
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(img)
}

// handleBrowserState returns the page's real URL, title and history availability. ?fmt=lines
// returns a plain text form for the C browser window, so it needs no JSON parser:
// url, title, back=0|1, forward=0|1, seq=N, one per line.
// @Summary Browser state
// @Description The page's real URL, title and history availability. ?fmt=lines returns a plain text form (url, title, back=0|1, forward=0|1, seq=N, one per line). Chromium not running yet is running=false, not an error.
// @Tags browser
// @Produce json
// @Produce plain
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param fmt query string false "Plain-text form" Enums(lines)
// @Success 200 {object} browserStateOut "Navigation state"
// @Failure 401 {string} string "Missing or wrong token"
// @Security ApiKeyAuth
// @Router /v1/browser/state [get]
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

// handleBrowserNavigate navigates the page. Navigating is what starts Chromium, so the first
// address starts it. The reply is the normalized URL that was navigated to.
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
func (a *Agent) handleBrowserNavigate(w http.ResponseWriter, r *http.Request) {
	// Navigating is what starts Chromium, so the first address the user enters starts it.
	sc, err := a.ensureBrowser(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	var body browserNavigateRequest
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
	writeJSON(w, browserNavigateResponse{URL: target})
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

// handleBrowserBack goes back in the page history.
// @Summary Browser back
// @Description Goes back in the page history.
// @Tags browser
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Success 204 "Went back"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 502 {string} string "Navigation failed"
// @Failure 503 {string} string "Chromium not running"
// @Security ApiKeyAuth
// @Router /v1/browser/back [post]
func (a *Agent) handleBrowserBack(w http.ResponseWriter, r *http.Request) {
	a.browserAction((*cdp.Screencast).GoBack)(w, r)
}

// handleBrowserForward goes forward in the page history.
// @Summary Browser forward
// @Description Goes forward in the page history.
// @Tags browser
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Success 204 "Went forward"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 502 {string} string "Navigation failed"
// @Failure 503 {string} string "Chromium not running"
// @Security ApiKeyAuth
// @Router /v1/browser/forward [post]
func (a *Agent) handleBrowserForward(w http.ResponseWriter, r *http.Request) {
	a.browserAction((*cdp.Screencast).GoForward)(w, r)
}

// handleBrowserReload reloads the page.
// @Summary Reload the browser
// @Description Reloads the page.
// @Tags browser
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Success 204 "Reloaded"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 502 {string} string "Navigation failed"
// @Failure 503 {string} string "Chromium not running"
// @Security ApiKeyAuth
// @Router /v1/browser/reload [post]
func (a *Agent) handleBrowserReload(w http.ResponseWriter, r *http.Request) {
	a.browserAction((*cdp.Screencast).Reload)(w, r)
}

// handleBrowserInput forwards one pointer, scroll, key or paste event to the page.
// @Summary Send browser input
// @Description Forwards one pointer, scroll, key or paste event to the page. Which fields apply depends on type: pointer takes action/x/y/button/buttons/clicks/mods, key takes action/key/code/text/mods, scroll takes x/y/dx/dy/mods, paste takes text.
// @Tags browser
// @Accept json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body browserInputRequest true "One input event"
// @Success 204 "Input delivered"
// @Failure 400 {string} string "Unparsable body, or unknown input type"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 502 {string} string "Input failed"
// @Failure 503 {string} string "Chromium not running"
// @Security ApiKeyAuth
// @Router /v1/browser/input [post]
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

// handleBrowserSize sets the browser viewport size.
// @Summary Set browser viewport size
// @Description Sets the browser viewport size (CSS pixels, with the device pixel ratio as scale).
// @Tags browser
// @Accept json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body browserSizeRequest true "Viewport width, height and scale"
// @Success 204 "Viewport resized"
// @Failure 400 {string} string "Unparsable body"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 502 {string} string "Resize failed"
// @Failure 503 {string} string "Chromium not running"
// @Security ApiKeyAuth
// @Router /v1/browser/size [post]
func (a *Agent) handleBrowserSize(w http.ResponseWriter, r *http.Request) {
	sc := a.current(w)
	if sc == nil {
		return
	}
	var body browserSizeRequest
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

// browserHome is the page the browser window opens first. DAD_BROWSER_HOME overrides it, and
// an empty value leaves Chromium on about:blank.
func browserHome() string {
	return envOr("DAD_BROWSER_HOME", "https://example.com")
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
		a.log.Warn("DAD_BROWSER_HOME is not a web address", "err", err)
		return
	}
	nctx, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	if err := sc.Navigate(nctx, target); err != nil {
		a.log.Warn("browser start page failed", "url", target, "err", err)
	}
}
