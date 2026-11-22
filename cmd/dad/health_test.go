package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthDegradedWithoutBroadway(t *testing.T) {
	t.Setenv("DAD_BROWSER_START", "off")
	t.Setenv("HOME", t.TempDir())
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), startedAt: time.Now()}

	h := a.sessionHealth(t.Context())
	if h["status"] != "degraded" {
		t.Fatalf("status = %v, want degraded (broadway down)", h["status"])
	}
	checks, _ := h["checks"].(map[string]any)
	if checks["chromium"] != "disabled" {
		t.Fatalf("chromium = %v, want disabled", checks["chromium"])
	}
	if checks["broadway"] != "down" {
		t.Fatalf("broadway = %v, want down", checks["broadway"])
	}
	if checks["disk"] != "ok" {
		t.Fatalf("disk = %v, want ok", checks["disk"])
	}
	if _, ok := checks["session"]; !ok {
		t.Fatal("session check missing")
	}
}

func TestHealthEndpoint(t *testing.T) {
	t.Setenv("DAD_BROWSER_START", "off")
	t.Setenv("HOME", t.TempDir())
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), startedAt: time.Now()}
	mux := http.NewServeMux()
	a.registerHealthRoutes(mux, a.auth)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/health", nil)
	req.Header.Set("X-Dad-Token", testToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "degraded" {
		t.Fatalf("status = %v, want degraded", out["status"])
	}
}
