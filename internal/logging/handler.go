package logging

import (
	"context"
	"log/slog"

	gologger "github.com/gtsteffaniak/go-logger/logger"
)

// gologgerHandler adapts a go-logger Logger to slog.Handler.
//
// go-logger uses slog internally but does not expose its handler, so records are
// forwarded through the public Logger methods. Attributes bound with WithAttrs are
// stored here and prepended to every record. go-logger's own With is not used because
// its text handler drops those attributes.
type gologgerHandler struct {
	lg     gologger.Logger
	min    slog.Level
	groups string // dotted prefix for attribute keys opened by WithGroup
	bound  []any  // key/value pairs from WithAttrs, already prefixed
}

func newGologgerHandler(lg gologger.Logger, min slog.Level) *gologgerHandler {
	return &gologgerHandler{lg: lg, min: min}
}

// Enabled reports whether level is at or above the configured minimum.
func (h *gologgerHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.min
}

// Handle forwards r to the matching go-logger method.
func (h *gologgerHandler) Handle(ctx context.Context, r slog.Record) error {
	args := make([]any, 0, len(h.bound)+r.NumAttrs()*2)
	args = append(args, h.bound...)
	r.Attrs(func(a slog.Attr) bool {
		args = appendAttr(args, h.groups, a)
		return true
	})
	switch {
	case r.Level >= slog.LevelError:
		h.lg.ErrorContext(ctx, r.Message, args...)
	case r.Level >= slog.LevelWarn:
		h.lg.WarnContext(ctx, r.Message, args...)
	case r.Level >= slog.LevelInfo:
		h.lg.InfoContext(ctx, r.Message, args...)
	default:
		h.lg.DebugContext(ctx, r.Message, args...)
	}
	return nil
}

// WithAttrs returns a handler whose records include attrs.
func (h *gologgerHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	bound := append([]any(nil), h.bound...)
	for _, a := range attrs {
		bound = appendAttr(bound, h.groups, a)
	}
	return &gologgerHandler{lg: h.lg, min: h.min, groups: h.groups, bound: bound}
}

// WithGroup returns a handler that prefixes subsequent attribute keys with name.
func (h *gologgerHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &gologgerHandler{lg: h.lg, min: h.min, groups: h.groups + name + ".", bound: h.bound}
}

// appendAttr flattens a into key/value pairs, joining group keys with dots.
func appendAttr(dst []any, prefix string, a slog.Attr) []any {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return dst
	}
	if a.Value.Kind() == slog.KindGroup {
		sub := prefix
		if a.Key != "" {
			sub = prefix + a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			dst = appendAttr(dst, sub, ga)
		}
		return dst
	}
	return append(dst, prefix+a.Key, a.Value.Any())
}
