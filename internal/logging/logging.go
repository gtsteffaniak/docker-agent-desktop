// Package logging builds the process-wide *slog.Logger.
//
// The default backend is github.com/gtsteffaniak/go-logger. Its public API does not expose
// a slog.Handler, so handler.go adapts its Logger interface to slog.Handler. Set
// DAD_LOG_BACKEND=slog to use the standard library handlers directly.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	gologger "github.com/gtsteffaniak/go-logger/logger"
)

// Config controls logger construction. Zero values select the defaults.
type Config struct {
	Backend string // "gologger" (default) or "slog"
	Level   string // debug, info, warn, error
	JSON    bool
	NoColor bool
	Output  io.Writer // defaults to os.Stdout
}

// FromEnv reads DAD_LOG_BACKEND, DAD_LOG_LEVEL, DAD_LOG_JSON and DAD_LOG_NO_COLOR.
func FromEnv() Config {
	return Config{
		Backend: envOr("DAD_LOG_BACKEND", "gologger"),
		Level:   envOr("DAD_LOG_LEVEL", "info"),
		JSON:    os.Getenv("DAD_LOG_JSON") == "1",
		NoColor: os.Getenv("DAD_LOG_NO_COLOR") == "1",
	}
}

// New returns a *slog.Logger for cfg.
func New(cfg Config) (*slog.Logger, error) {
	out := cfg.Output
	if out == nil {
		out = os.Stdout
	}
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(cfg.Backend) {
	case "", "gologger":
		lg, err := gologger.NewLogger(gologger.JsonConfig{
			Levels:     levelsString(level),
			Output:     "stdout",
			NoColors:   cfg.NoColor,
			Json:       cfg.JSON,
			Structured: true,
		})
		if err != nil {
			return nil, fmt.Errorf("go-logger: %w", err)
		}
		return slog.New(newGologgerHandler(lg, level)), nil
	case "slog":
		opts := &slog.HandlerOptions{Level: level}
		if cfg.JSON {
			return slog.New(slog.NewJSONHandler(out, opts)), nil
		}
		return slog.New(slog.NewTextHandler(out, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log backend %q", cfg.Backend)
	}
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(strings.TrimSpace(s)))); err != nil {
		return 0, fmt.Errorf("invalid log level %q: %w", s, err)
	}
	return l, nil
}

// levelsString lists the go-logger level names enabled at or above min.
func levelsString(min slog.Level) string {
	all := []struct {
		name string
		l    slog.Level
	}{{"debug", slog.LevelDebug}, {"info", slog.LevelInfo}, {"warning", slog.LevelWarn}, {"error", slog.LevelError}}
	var names []string
	for _, a := range all {
		if a.l >= min {
			names = append(names, a.name)
		}
	}
	return strings.Join(names, "|")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
