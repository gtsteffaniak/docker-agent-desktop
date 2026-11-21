// Session health is the real health check: whether the session is actually
// usable, not whether the process is alive. /healthz stays a bare 200 for
// container liveness (Docker HEALTHCHECK) and is intentionally not touched.
package main

import (
	"context"
	"net/http"
	"syscall"
	"time"
)

// minDiskFree is the free space on the home volume below which health is degraded.
const minDiskFree = 100 << 20

// sessionHealth probes the pieces a session needs and reports each one, so a
// caller can tell a dead browser from a dead display from a full disk instead
// of getting one opaque 503.
func (a *Agent) sessionHealth(ctx context.Context) map[string]any {
	checks := map[string]any{"agent": "ok"}
	status := "ok"

	// Chromium: disabled is a configuration, not a failure, and so is a browser
	// that is not running because nobody has needed it yet (lazy start), it was
	// stopped, or it was shut down idle: the next browser call starts it. Only a
	// browser that should be running (its supervisor is up) but has no working
	// CDP connection is down.
	if browserMode() == "off" {
		checks["chromium"] = "disabled"
	} else if !a.browserSupervised() {
		checks["chromium"] = "stopped"
	} else if sc := a.screencast(); sc == nil {
		checks["chromium"] = "down"
		status = "degraded"
	} else {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := sc.RefreshNav(pctx)
		cancel()
		if err != nil {
			checks["chromium"] = "down"
			status = "degraded"
		} else {
			checks["chromium"] = "ok"
		}
	}

	if a.broadway != nil && a.broadway.Connected() {
		checks["broadway"] = "ok"
	} else {
		checks["broadway"] = "down"
		status = "degraded"
	}

	home := a.homeDir()
	var st syscall.Statfs_t
	if err := syscall.Statfs(home, &st); err != nil {
		checks["disk"] = "unknown"
	} else {
		free := st.Bavail * uint64(st.Bsize)
		checks["disk_free_bytes"] = free
		if free < minDiskFree {
			checks["disk"] = "down"
			status = "degraded"
		} else {
			checks["disk"] = "ok"
		}
	}

	if s := a.sessionState(); s != nil {
		checks["session"] = map[string]any{
			"held":         true,
			"holder":       s.Holder,
			"paused":       s.Paused,
			"expires_in_s": int64(time.Until(s.ExpiresAt).Seconds()),
		}
	} else {
		checks["session"] = map[string]any{"held": false}
	}

	return map[string]any{
		"status":   status,
		"checks":   checks,
		"uptime_s": int64(time.Since(a.startedAt).Seconds()),
	}
}

// browserSupervised reports whether a Chromium supervisor is running, that is,
// whether the browser is meant to be up.
func (a *Agent) browserSupervised() bool {
	a.browserMu.Lock()
	defer a.browserMu.Unlock()
	return a.browser != nil
}

func (a *Agent) registerHealthRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /v1/health", auth(a.handleSessionHealth))
}

func (a *Agent) handleSessionHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.checkHealth(r.Context()))
}

// checkHealth runs sessionHealth and records a health.degraded event on the
// transition into degraded. A transition is worth one activity-log line;
// repeated degraded polls are not. /v1/health and the health_get MCP tool
// share it, so both surfaces log the same transitions.
func (a *Agent) checkHealth(ctx context.Context) map[string]any {
	health := a.sessionHealth(ctx)
	switch health["status"] {
	case "degraded":
		// CompareAndSwap makes exactly one of several concurrent polls
		// record the transition.
		if a.healthDegraded.CompareAndSwap(false, true) {
			a.recordEvent("health.degraded", "system", "session health degraded")
		}
	case "ok":
		a.healthDegraded.Store(false)
	}
	return health
}
