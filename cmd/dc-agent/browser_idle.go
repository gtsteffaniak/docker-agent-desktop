package main

import (
	"context"
	"strconv"
	"time"
)

// defaultBrowserIdle is how long the browser may go without a client command before Chromium is
// stopped. The next navigation starts it again.
const defaultBrowserIdle = 30 * time.Minute

// browserRun is one Chromium supervisor. stop ends it and kills Chromium.
type browserRun struct {
	stop func()
}

// browserIdleTimeout reads DC_BROWSER_IDLE_MINUTES. 0 disables the idle shutdown.
func browserIdleTimeout() time.Duration {
	n, err := strconv.Atoi(envOr("DC_BROWSER_IDLE_MINUTES", "30"))
	if err != nil || n < 0 {
		return defaultBrowserIdle
	}
	return time.Duration(n) * time.Minute
}

// idleExpired reports whether the browser has been idle for at least idle. A zero idle disables
// the check.
func idleExpired(last, now time.Time, idle time.Duration) bool {
	return idle > 0 && now.Sub(last) >= idle
}

// touch records client activity: a command, input, or the browser window connecting.
func (a *Agent) touch() {
	a.lastActivity.Store(time.Now().UnixNano())
}

func (a *Agent) lastTouch() time.Time {
	return time.Unix(0, a.lastActivity.Load())
}

// watchBrowserIdle stops Chromium once no client has acted on it for the idle timeout. Frames
// streaming to an open window do not count as activity, so a window left alone is stopped too.
func (a *Agent) watchBrowserIdle(ctx context.Context) {
	idle := browserIdleTimeout()
	if idle == 0 {
		return
	}
	a.touch()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if a.screencast() != nil && idleExpired(a.lastTouch(), now, idle) {
				a.log.Info("stopping idle browser", "idle", idle.String())
				a.stopBrowser()
			}
		}
	}
}

// stopBrowser ends the Chromium supervisor. Its context is cancelled, which kills the browser's
// process group. The next navigation starts a new supervisor.
func (a *Agent) stopBrowser() {
	a.browserMu.Lock()
	run := a.browser
	a.browser = nil
	a.browserMu.Unlock()
	if run != nil {
		run.stop()
	}
}
