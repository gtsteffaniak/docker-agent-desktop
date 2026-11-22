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

// healthSessionCheck is the "session" entry of the health checks: whether a
// lease is held and by whom.
type healthSessionCheck struct {
	Held       bool           `json:"held"`
	Holder     *SessionHolder `json:"holder,omitempty"`
	Paused     bool           `json:"paused,omitempty"`
	ExpiresInS int64          `json:"expires_in_s,omitempty"`
}

// healthChecks is the per-piece "checks" map of the health reply.
type healthChecks struct {
	Agent         string             `json:"agent"`
	Chromium      string             `json:"chromium"`
	Broadway      string             `json:"broadway"`
	Disk          string             `json:"disk"`
	DiskFreeBytes uint64             `json:"disk_free_bytes,omitempty"`
	Session       healthSessionCheck `json:"session"`
}

// healthResponse is the GET /v1/health body.
type healthResponse struct {
	Status  string       `json:"status"`
	Checks  healthChecks `json:"checks"`
	UptimeS int64        `json:"uptime_s"`
}

// sessionHealth probes the pieces a session needs and reports each one, so a
// caller can tell a dead browser from a dead display from a full disk instead
// of getting one opaque 503.
func (a *Agent) sessionHealth(ctx context.Context) healthResponse {
	checks := healthChecks{Agent: "ok"}
	status := "ok"

	// Chromium: disabled is a configuration, not a failure, and so is a browser
	// that is not running because nobody has needed it yet (lazy start), it was
	// stopped, or it was shut down idle: the next browser call starts it. Only a
	// browser that should be running (its supervisor is up) but has no working
	// CDP connection is down.
	if browserMode() == "off" {
		checks.Chromium = "disabled"
	} else if !a.browserSupervised() {
		checks.Chromium = "stopped"
	} else if sc := a.screencast(); sc == nil {
		checks.Chromium = "down"
		status = "degraded"
	} else {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := sc.RefreshNav(pctx)
		cancel()
		if err != nil {
			checks.Chromium = "down"
			status = "degraded"
		} else {
			checks.Chromium = "ok"
		}
	}

	if a.broadway != nil && a.broadway.Connected() {
		checks.Broadway = "ok"
	} else {
		checks.Broadway = "down"
		status = "degraded"
	}

	home := a.homeDir()
	var st syscall.Statfs_t
	if err := syscall.Statfs(home, &st); err != nil {
		checks.Disk = "unknown"
	} else {
		free := st.Bavail * uint64(st.Bsize)
		checks.DiskFreeBytes = free
		if free < minDiskFree {
			checks.Disk = "down"
			status = "degraded"
		} else {
			checks.Disk = "ok"
		}
	}

	if s := a.sessionState(); s != nil {
		checks.Session = healthSessionCheck{
			Held:       true,
			Holder:     &s.Holder,
			Paused:     s.Paused,
			ExpiresInS: int64(time.Until(s.ExpiresAt).Seconds()),
		}
	} else {
		checks.Session = healthSessionCheck{Held: false}
	}

	return healthResponse{
		Status:  status,
		Checks:  checks,
		UptimeS: int64(time.Since(a.startedAt).Seconds()),
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
	a.handle(mux, "GET /v1/health", auth(a.handleSessionHealth))
}

// handleSessionHealth reports session health: whether the session is actually usable,
// not whether the process is alive.
// @Summary Session health
// @Description The real health check: whether the session is actually usable. Probes Chromium, Broadway, disk space and the session lease, and reports each piece, so a caller can tell a dead browser from a dead display from a full disk instead of getting one opaque 503. A transition into degraded is recorded once in the activity log.
// @Tags health
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Success 200 {object} healthResponse "Health, ok or degraded"
// @Failure 401 {string} string "Missing or wrong token"
// @Security ApiKeyAuth
// @Router /v1/health [get]
func (a *Agent) handleSessionHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.checkHealth(r.Context()))
}

// checkHealth runs sessionHealth and records a health.degraded event on the
// transition into degraded. A transition is worth one activity-log line;
// repeated degraded polls are not. /v1/health and the health_get MCP tool
// share it, so both surfaces log the same transitions.
func (a *Agent) checkHealth(ctx context.Context) healthResponse {
	health := a.sessionHealth(ctx)
	switch health.Status {
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
