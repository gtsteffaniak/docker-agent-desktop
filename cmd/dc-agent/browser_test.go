package main

import "testing"

func TestNormalizeURL(t *testing.T) {
	good := map[string]string{
		"example.com":                "https://example.com",
		"  example.com/path?q=1  ":   "https://example.com/path?q=1",
		"https://example.com/a":      "https://example.com/a",
		"HTTP://Example.com":         "HTTP://Example.com",
		"localhost":                  "http://localhost",
		"localhost:3000":             "http://localhost:3000",
		"localhost:3000/app#x":       "http://localhost:3000/app#x",
		"127.0.0.1:8080":             "http://127.0.0.1:8080",
		"192.168.1.10/status":        "http://192.168.1.10/status",
		"example.com:8443/x":         "https://example.com:8443/x",
		"about:blank":                "about:blank",
		"sub.example.co.uk/a/b?c=d":  "https://sub.example.co.uk/a/b?c=d",
		"https://example.com:8443/x": "https://example.com:8443/x",
	}
	for in, want := range good {
		got, err := NormalizeURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	bad := []string{
		"",
		"   ",
		"how to boil an egg", // spaces: not a URL, and there is no search fallback
		"javascript:alert(1)",
		"file:///etc/passwd",
		"data:text/html,hi",
		"chrome://settings",
		"view-source:https://example.com",
	}
	for _, in := range bad {
		if got, err := NormalizeURL(in); err == nil {
			t.Errorf("NormalizeURL(%q) = %q; want an error", in, got)
		}
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("a\nb\r\nc"); got != "a b  c" {
		t.Fatalf("oneLine = %q", got)
	}
}
