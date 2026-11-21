package main

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtsteffaniak/docker-computer/internal/apps"
)

// openFDs counts this process's open file descriptors.
func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	return len(ents)
}

// fakeDisplayPort listens on a free loopback port so ensureDisplay treats the
// app's display as already running.
func fakeDisplayPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitAppGone(t *testing.T, a *Agent, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !a.appRunning(id) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("app %s still running", id)
}

// Starting an app must not leak the agent's copy of the app log descriptor.
func TestStartAppDoesNotLeakLogFD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".dc", "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &Agent{log: slog.New(slog.DiscardHandler)}
	app := apps.App{ID: "fdtest", Exec: []string{"true"}, Port: fakeDisplayPort(t)}
	before := openFDs(t)
	for i := 0; i < 20; i++ {
		if err := a.startApp(app); err != nil {
			t.Fatal(err)
		}
		waitAppGone(t, a, app.ID)
	}
	if after := openFDs(t); after > before+2 {
		t.Fatalf("open fds grew from %d to %d over 20 app starts", before, after)
	}
}

// appRunning, runningAppIDs and stopApp read the app table while the reaper
// goroutine finishes the process; under -race this must be clean.
func TestAppTableRace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &Agent{log: slog.New(slog.DiscardHandler)}
	port := fakeDisplayPort(t)
	var wg sync.WaitGroup
	stop := time.Now().Add(300 * time.Millisecond)
	wg.Add(3)
	go func() {
		defer wg.Done()
		for time.Now().Before(stop) {
			_ = a.startApp(apps.App{ID: "race", Exec: []string{"true"}, Port: port})
		}
	}()
	go func() {
		defer wg.Done()
		for time.Now().Before(stop) {
			a.appRunning("race")
			a.runningAppIDs()
		}
	}()
	go func() {
		defer wg.Done()
		for time.Now().Before(stop) {
			_ = a.stopApp("race")
		}
	}()
	wg.Wait()
	_ = a.stopApp("race")
	waitAppGone(t, a, "race")
}

// broadwayd must bind loopback only: it has no authentication.
func TestEnsureDisplayBindsLoopback(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	// A fake broadwayd that records its arguments and listens on the -p port.
	script := `#!/bin/sh
echo "$@" > "` + argsFile + `"
port=""
while [ $# -gt 0 ]; do
  case "$1" in -p) port="$2"; shift;; esac
  shift
done
exec python3 -c 'import socket,sys,time
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", int(sys.argv[1]))); s.listen(); time.sleep(30)' "$port"
`
	if err := os.WriteFile(filepath.Join(dir, "broadwayd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	a := &Agent{log: slog.New(slog.DiscardHandler)}
	if err := a.ensureDisplay(port); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		appProcs.mu.Lock()
		cmd := appProcs.displays[port]
		appProcs.mu.Unlock()
		if cmd != nil {
			_ = cmd.Process.Kill()
		}
	})
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Fields(string(raw)); !strings.Contains(" "+strings.Join(args, " ")+" ", " -a 127.0.0.1 ") {
		t.Fatalf("broadwayd args = %q, want -a 127.0.0.1", raw)
	}
}
