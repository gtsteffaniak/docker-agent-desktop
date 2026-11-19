package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gtsteffaniak/docker-computer/internal/apps"
)

// displayReadyTimeout bounds how long a broadwayd display may take to accept connections.
const displayReadyTimeout = 5 * time.Second

// procs tracks the running app processes and their Broadway displays, one per port.
type procs struct {
	mu       sync.Mutex
	apps     map[string]*exec.Cmd
	displays map[int]*exec.Cmd
}

var appProcs = &procs{apps: map[string]*exec.Cmd{}, displays: map[int]*exec.Cmd{}}

// displayFor maps a Broadway port to its GDK display. Port 8081 is :1, 8082 is :2, and so on.
func displayFor(port int) string {
	return ":" + strconv.Itoa(port-8080)
}

// portOpen reports whether something is already listening on a loopback port.
func portOpen(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// ensureDisplay starts broadwayd for an app's port if it is not already serving. Each GTK app
// gets its own display, so each app runs as a separate broadwayd on its own port.
func (a *Agent) ensureDisplay(port int) error {
	appProcs.mu.Lock()
	defer appProcs.mu.Unlock()
	if portOpen(port) {
		return nil
	}
	bin, err := exec.LookPath("broadwayd")
	if err != nil {
		return fmt.Errorf("broadwayd not installed: %w", err)
	}
	cmd := exec.Command(bin, displayFor(port), "-p", strconv.Itoa(port))
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	appProcs.displays[port] = cmd
	go func() {
		err := cmd.Wait()
		a.log.Info("broadwayd exited", "port", port, "err", err)
		appProcs.mu.Lock()
		if appProcs.displays[port] == cmd {
			delete(appProcs.displays, port)
		}
		appProcs.mu.Unlock()
	}()
	deadline := time.Now().Add(displayReadyTimeout)
	for time.Now().Before(deadline) {
		if portOpen(port) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("broadwayd did not start on port " + strconv.Itoa(port))
}

// startApp runs an app on its display. It does nothing if the app is already running.
func (a *Agent) startApp(app apps.App) error {
	if err := a.ensureDisplay(app.Port); err != nil {
		return err
	}
	appProcs.mu.Lock()
	defer appProcs.mu.Unlock()
	if cur, ok := appProcs.apps[app.ID]; ok && cur.ProcessState == nil {
		return nil
	}
	bin, err := exec.LookPath(app.Exec[0])
	if err != nil {
		return fmt.Errorf("app not installed: %s", app.Exec[0])
	}
	cmd := exec.Command(bin, app.Exec[1:]...)
	// Keep app output in a per-app log so GTK warnings can be read.
	if logf, err := os.OpenFile(filepath.Join(envOr("HOME", "/home/bot"), ".dc", "logs", app.ID+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	cmd.Env = append(os.Environ(),
		"GDK_BACKEND=broadway",
		"BROADWAY_DISPLAY="+displayFor(app.Port),
		// dc-windows names its control socket after this id.
		"DC_APP_ID="+app.ID)
	if err := cmd.Start(); err != nil {
		return err
	}
	appProcs.apps[app.ID] = cmd
	a.log.Info("app started", "app", app.ID, "pid", cmd.Process.Pid)
	go func(id string, c *exec.Cmd) {
		err := c.Wait()
		a.log.Info("app exited", "app", id, "pid", c.Process.Pid, "err", err)
		appProcs.mu.Lock()
		if appProcs.apps[id] == c {
			delete(appProcs.apps, id)
		}
		appProcs.mu.Unlock()
	}(app.ID, cmd)
	return nil
}

// autostartApps starts every GTK app at boot when DC_AUTOSTART_APPS=1. Without it, apps start
// when their window opens on the desktop.
func (a *Agent) autostartApps() {
	if os.Getenv("DC_AUTOSTART_APPS") != "1" {
		return
	}
	for _, app := range a.loadApps() {
		if app.Port == 0 {
			continue
		}
		if err := a.startApp(app); err != nil {
			a.log.Warn("autostart failed", "app", app.ID, "err", err)
		}
	}
}
