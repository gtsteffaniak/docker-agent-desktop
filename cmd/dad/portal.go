package main

import (
	"bytes"
	"context"
	"embed"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gtsteffaniak/docker-agent-desktop/internal/broadway"
)

// desktopFiles is the web desktop: dock, window frames, and the browser toolbar. It is plain
// HTML, CSS and JavaScript with no build step.
//
//go:embed desktop
var desktopFiles embed.FS

// mountPortal serves the web desktop and proxies each GTK app's Broadway display.
//
//	GET /portal/?token=…        sets an HttpOnly cookie, then redirects to /desktop/
//	GET /desktop/              the web desktop (cookie or header auth)
//	GET /apps/{id}/            one app's Broadway HTML5 client
//	GET /apps/{id}/socket      WebSocket relay to that app's broadwayd
func (a *Agent) mountPortal(mux *http.ServeMux) {
	// DAD_DESKTOP_DIR serves the desktop from disk instead of the embedded copy, so a CSS or JS
	// change can be tried without rebuilding the image. It is for development only.
	var files fs.FS
	if dir := os.Getenv("DAD_DESKTOP_DIR"); dir != "" {
		files = os.DirFS(dir)
	} else {
		sub, err := fs.Sub(desktopFiles, "desktop")
		if err != nil {
			panic(err) // the embed path is fixed at build time
		}
		files = sub
	}
	static := http.StripPrefix("/desktop", http.FileServerFS(files))

	mux.HandleFunc("GET /portal", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/desktop/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /portal/", a.portalAuth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/desktop/", http.StatusSeeOther)
	}))
	mux.HandleFunc("GET /desktop", a.portalAuth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/desktop/", http.StatusMovedPermanently)
	}))
	mux.HandleFunc("GET /desktop/", a.portalAuth(static.ServeHTTP))
	mux.HandleFunc("GET /apps/{id}/socket", a.portalAuth(a.appSocket))
	mux.HandleFunc("GET /apps/{id}/", a.portalAuth(a.appProxy))
}

// appPort returns the Broadway port for a registry app, or 0 when the app has none.
func (a *Agent) appPort(id string) int {
	for _, app := range a.loadApps() {
		if app.ID == id {
			return app.Port
		}
	}
	return 0
}

// appProxy serves one app's Broadway HTML client. The app id prefix is removed before the
// request reaches broadwayd, which serves from its own root.
func (a *Agent) appProxy(w http.ResponseWriter, r *http.Request) {
	port := a.appPort(r.PathValue("id"))
	if port == 0 {
		http.NotFound(w, r)
		return
	}
	prefix := "/apps/" + r.PathValue("id")
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	// Built directly with Rewrite (not NewSingleHostReverseProxy): Director is
	// deprecated since Go 1.26, and even clearing it trips staticcheck. SetURL
	// reproduces the default director (scheme/host from the target).
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.URL.Path = strings.TrimPrefix(r.Out.URL.Path, prefix)
			if r.Out.URL.Path == "" {
				r.Out.URL.Path = "/"
			}
			r.Out.Host = target.Host
			// Ask for an uncompressed body so the HTML can get the key script below.
			r.Out.Header.Del("Accept-Encoding")
		},
	}
	proxy.ModifyResponse = injectKeysScript
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		a.log.Warn("app proxy error", "app", r.PathValue("id"), "err", err)
		http.Error(w, "app not ready", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

// portalAuth accepts the token from the cookie or header. A ?token= query value is exchanged
// for an HttpOnly cookie and removed from the URL.
func (a *Agent) portalAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("token"); q != "" {
			if !a.validToken(q) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    q,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				Secure:   r.TLS != nil,
				MaxAge:   12 * 60 * 60,
			})
			q2 := r.URL.Query()
			q2.Del("token")
			dest := r.URL.Path
			if enc := q2.Encode(); enc != "" {
				dest += "?" + enc
			}
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		if !a.validToken(tokenFrom(r)) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// appSocket relays Broadway frames between the browser and one app's broadwayd.
func (a *Agent) appSocket(w http.ResponseWriter, r *http.Request) {
	port := a.appPort(r.PathValue("id"))
	if port == 0 {
		http.NotFound(w, r)
		return
	}
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{"broadway"},
		CompressionMode: websocket.CompressionDisabled, // broadwayd rejects permessage-deflate
	})
	if err != nil {
		a.log.Warn("app accept failed", "err", err)
		return
	}
	client.SetReadLimit(16 << 20)
	defer client.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	upstream, err := broadway.Dial(ctx, "ws://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+"/socket")
	if err != nil {
		a.log.Warn("app upstream dial failed", "app", r.PathValue("id"), "err", err)
		_ = client.Close(websocket.StatusTryAgainLater, "app not ready")
		return
	}
	defer upstream.CloseNow()

	errc := make(chan error, 2)
	go relay(ctx, client, upstream, errc)
	go relay(ctx, upstream, client, errc)
	<-errc
	cancel()
	_ = client.Close(websocket.StatusNormalClosure, "")
	_ = upstream.Close(websocket.StatusNormalClosure, "")
}

// relay copies messages from src to dst, preserving message type, until either side fails.
func relay(ctx context.Context, src, dst *websocket.Conn, errc chan<- error) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			errc <- err
			return
		}
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = dst.Write(wctx, typ, data)
		cancel()
		if err != nil {
			errc <- err
			return
		}
	}
}

// maxHTMLBytes bounds how much of an upstream page is buffered for rewriting.
const maxHTMLBytes = 1 << 20

// injectKeysScript adds the browser-keys script to a Broadway HTML page. Other content types
// pass through untouched.
func injectKeysScript(resp *http.Response) error {
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLBytes))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	body = injectScript(body)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("Content-Encoding")
	return nil
}

// browserKeysScript keeps browser-only shortcuts out of the remote desktop. The HTML client
// cancels every key it forwards, so without this F12 (dev tools) and F11 (full screen) never
// reach the browser. The listener runs in the capture phase on window, before the client sees
// the event, and does not cancel it, so the browser performs its own default action.
const browserKeysScript = `<script>(function(){` +
	`var keys={F11:1,F12:1};` +
	`function pass(e){` +
	`var k=e.key||"";` +
	`if(keys[k]||(e.ctrlKey&&e.shiftKey&&/^[ICJ]$/i.test(k))){e.stopImmediatePropagation();}` +
	`}` +
	`window.addEventListener("keydown",pass,true);` +
	`window.addEventListener("keyup",pass,true);` +
	`})();</script>`

// injectScript inserts the browser-keys script before </head>, or at the start when the page has
// no head.
func injectScript(html []byte) []byte {
	head := []byte(browserKeysScript)
	if i := bytes.Index(bytes.ToLower(html), []byte("</head>")); i >= 0 {
		out := make([]byte, 0, len(html)+len(head))
		out = append(out, html[:i]...)
		out = append(out, head...)
		return append(out, html[i:]...)
	}
	return append(head, html...)
}
