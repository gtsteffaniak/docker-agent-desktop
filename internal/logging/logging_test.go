package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestSlogBackendWritesText(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New(Config{Backend: "slog", Level: "info", Output: &buf})
	if err != nil {
		t.Fatal(err)
	}
	lg.With("component", "test").Info("hello", "k", "v")
	if !strings.Contains(buf.String(), "hello") || !strings.Contains(buf.String(), "component=test") {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

func TestSlogBackendFiltersBelowLevel(t *testing.T) {
	var buf bytes.Buffer
	lg, _ := New(Config{Backend: "slog", Level: "warn", Output: &buf})
	lg.Info("quiet")
	if buf.Len() != 0 {
		t.Fatalf("info should be filtered at warn level, got %q", buf.String())
	}
}

func TestGologgerBackendConstructs(t *testing.T) {
	lg, err := New(Config{Backend: "gologger", Level: "debug", NoColor: true})
	if err != nil {
		t.Fatal(err)
	}
	lg.Info("gologger smoke test", "ok", true)
}

func TestUnknownBackendRejected(t *testing.T) {
	if _, err := New(Config{Backend: "nope"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestLevelsString(t *testing.T) {
	if got := levelsString(slog.LevelWarn); got != "warning|error" {
		t.Fatalf("levelsString(warn) = %q", got)
	}
}

func TestHandlerFlattensGroups(t *testing.T) {
	var args []any
	attr := slog.Group("req", slog.String("method", "GET"), slog.Group("inner", slog.Int("n", 2)))
	args = appendAttr(args, "", attr)
	want := []any{"req.method", "GET", "req.inner.n", int64(2)}
	if len(args) != len(want) {
		t.Fatalf("args = %v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %v, want %v", i, args[i], want[i])
		}
	}
}

func TestGologgerHandlerEnabled(t *testing.T) {
	h := newGologgerHandler(nil, slog.LevelWarn)
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info should be disabled")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("error should be enabled")
	}
}

// captureStdout runs fn with os.Stdout redirected, since go-logger writes to stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestGologgerKeepsWithAttrsAndGroups(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		out := captureStdout(t, func() {
			lg, err := New(Config{Backend: "gologger", Level: "debug", NoColor: true, JSON: jsonMode})
			if err != nil {
				t.Fatal(err)
			}
			child := lg.With("component", "broadway")
			child.WithGroup("req").Info("probe", "method", "GET")
		})
		for _, want := range []string{"component", "broadway", "req.method", "GET"} {
			if !strings.Contains(out, want) {
				t.Fatalf("json=%v: output missing %q: %s", jsonMode, want, out)
			}
		}
	}
}
