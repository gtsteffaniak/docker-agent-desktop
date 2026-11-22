package main

import (
	"log/slog"
	"testing"
	"time"
)

func TestEventsRingBuffer(t *testing.T) {
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), startedAt: time.Now()}

	a.recordEvent("session.acquired", "bot-a", "session ses_1 acquired")
	a.recordEvent("shell.exec", "bot-a", "shell exec exit 0: echo hi")
	if got := a.eventsSince(0, 10); len(got) != 2 || got[0].Type != "session.acquired" || got[1].Seq != got[0].Seq+1 {
		t.Fatalf("events = %+v", got)
	}
	if got := a.eventsSince(1, 10); len(got) != 1 || got[0].Type != "shell.exec" {
		t.Fatalf("since=1 events = %+v", got)
	}
	if got := a.eventsSince(0, 1); len(got) != 1 {
		t.Fatalf("limit=1 events = %+v", got)
	}
}

func TestEventsCapDropsOldest(t *testing.T) {
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), startedAt: time.Now()}
	for i := 0; i < maxActivityEvents+10; i++ {
		a.recordEvent("shell.exec", "bot-a", "x")
	}
	got := a.eventsSince(0, maxActivityEvents+10)
	if len(got) != maxActivityEvents {
		t.Fatalf("len = %d, want %d", len(got), maxActivityEvents)
	}
	if got[0].Seq != 11 {
		t.Fatalf("first seq = %d, want 11", got[0].Seq)
	}
}
