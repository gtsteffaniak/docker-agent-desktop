package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesRoundTrip(t *testing.T) {
	a, _ := newShellTestAgent(t)
	if err := a.writeFile("notes/hello.txt", []byte("hello files")); err != nil {
		t.Fatal(err)
	}
	// Parent dirs were created by the write.
	data, err := a.readFile("notes/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello files" {
		t.Fatalf("read = %q", data)
	}
	entries, err := a.listFiles("notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "hello.txt" || entries[0].Type != "file" ||
		entries[0].Size != int64(len("hello files")) || entries[0].Path != "notes/hello.txt" {
		t.Fatalf("entries = %+v", entries)
	}
	// Home listing includes the directory.
	entries, err = a.listFiles("")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Type != "dir" || entries[0].Path != "notes" {
		t.Fatalf("home entries = %+v", entries)
	}
}

func TestFilesWriteReadHTTP(t *testing.T) {
	a, mux := newShellTestAgent(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	hdr := http.Header{"X-Dad-Token": {testToken}}

	// Write via the handler directly (the mux route is session-gated).
	rec := callHandler(t, a.handleFilesWrite, http.MethodPost, "/v1/files/write",
		fileWriteRequest{Path: "doc.txt", Content: "via http", Encoding: "text"})
	if rec.Code != http.StatusOK {
		t.Fatalf("write status = %d, body %q", rec.Code, rec.Body.String())
	}
	// Read back through the mux (a read route, no session needed).
	resp, body := get(t, srv.URL+"/v1/files/read?path=doc.txt", hdr)
	if resp.StatusCode != http.StatusOK || body != "via http" {
		t.Fatalf("read status = %d, body %q", resp.StatusCode, body)
	}
	// List through the mux.
	resp, body = get(t, srv.URL+"/v1/files/list", hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var list struct {
		Entries []FileEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 1 || list.Entries[0].Name != "doc.txt" {
		t.Fatalf("list = %+v", list)
	}
	// download=1 sets an attachment disposition.
	resp, _ = get(t, srv.URL+"/v1/files/read?path=doc.txt&download=1", hdr)
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("Content-Disposition = %q", resp.Header.Get("Content-Disposition"))
	}
	// Missing file -> 404.
	if resp, _ := get(t, srv.URL+"/v1/files/read?path=nope.txt", hdr); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestFilesBase64Write(t *testing.T) {
	a, _ := newShellTestAgent(t)
	rec := callHandler(t, a.handleFilesWrite, http.MethodPost, "/v1/files/write",
		fileWriteRequest{Path: "bin.dat", Content: "aGVsbG8=", Encoding: "base64"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	data, err := a.readFile("bin.dat")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("read = %q", data)
	}
	// Bad base64 -> 400.
	rec = callHandler(t, a.handleFilesWrite, http.MethodPost, "/v1/files/write",
		fileWriteRequest{Path: "bin.dat", Content: "!!!", Encoding: "base64"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	// Unknown encoding -> 400.
	rec = callHandler(t, a.handleFilesWrite, http.MethodPost, "/v1/files/write",
		fileWriteRequest{Path: "bin.dat", Content: "x", Encoding: "hex"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestFilesJailEscapes(t *testing.T) {
	a, _ := newShellTestAgent(t)
	escapes := []string{"../", "../secret.txt", "../../etc/passwd", "/etc/passwd", "/etc", "/tmp/x"}
	for _, p := range escapes {
		if _, err := a.listFiles(p); err == nil {
			t.Fatalf("listFiles(%q) should be rejected", p)
		}
		if _, err := a.readFile(p); err == nil {
			t.Fatalf("readFile(%q) should be rejected", p)
		}
		if err := a.writeFile(p, []byte("x")); err == nil {
			t.Fatalf("writeFile(%q) should be rejected", p)
		}
		if err := a.mkdirPath(p); err == nil {
			t.Fatalf("mkdirPath(%q) should be rejected", p)
		}
		if err := a.movePath("a.txt", p); err == nil {
			t.Fatalf("movePath to %q should be rejected", p)
		}
		if err := a.deletePath(p, true); err == nil {
			t.Fatalf("deletePath(%q) should be rejected", p)
		}
	}
	// The same escapes through HTTP map to 400.
	rec := callHandler(t, a.handleFilesWrite, http.MethodPost, "/v1/files/write",
		fileWriteRequest{Path: "../evil.txt", Content: "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("write escape status = %d, want 400", rec.Code)
	}
}

func TestFilesSymlinkEscape(t *testing.T) {
	a, _ := newShellTestAgent(t)
	home := a.homeDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	// Reading through the symlinked dir escapes the jail.
	if _, err := a.readFile("link/secret.txt"); err == nil {
		t.Fatal("read through symlinked dir should be rejected")
	}
	if err := a.writeFile("link/evil.txt", []byte("x")); err == nil {
		t.Fatal("write through symlinked dir should be rejected")
	}
	if _, err := a.listFiles("link"); err == nil {
		t.Fatal("list through symlinked dir should be rejected")
	}
	// A symlink to a file inside home is fine.
	if err := os.WriteFile(filepath.Join(home, "real.txt"), []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "real.txt"), filepath.Join(home, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	data, err := a.readFile("alias.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "real" {
		t.Fatalf("read = %q", data)
	}
}

func TestFilesDelete(t *testing.T) {
	a, _ := newShellTestAgent(t)
	if err := a.writeFile("d/sub/f.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	// Non-recursive delete of a non-empty dir fails.
	if err := a.deletePath("d", false); err == nil {
		t.Fatal("non-recursive delete of non-empty dir should fail")
	}
	if err := a.deletePath("d/sub/f.txt", false); err != nil {
		t.Fatal(err)
	}
	// Now the dir is empty and a plain delete works.
	if err := a.deletePath("d/sub", false); err != nil {
		t.Fatal(err)
	}
	if err := a.deletePath("d", false); err != nil {
		t.Fatal(err)
	}
	// Recursive delete removes a whole tree.
	if err := a.writeFile("tree/a/b.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := a.deletePath("tree", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.homeDir(), "tree")); !os.IsNotExist(err) {
		t.Fatalf("tree still exists: %v", err)
	}
	// Deleting home itself is refused.
	if err := a.deletePath("", true); err == nil {
		t.Fatal("deleting home should be refused")
	}
	// Deleting a missing path is a 404 (handler called directly; the mux
	// route is session-gated and would 409 without a session).
	rec := callHandler(t, a.handleFilesDelete, http.MethodDelete, "/v1/files?path=gone.txt", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestFilesMove(t *testing.T) {
	a, _ := newShellTestAgent(t)
	if err := a.writeFile("src.txt", []byte("move me")); err != nil {
		t.Fatal(err)
	}
	if err := a.movePath("src.txt", "dst/sub.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.homeDir(), "src.txt")); !os.IsNotExist(err) {
		t.Fatal("source should be gone after move")
	}
	data, err := a.readFile("dst/sub.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "move me" {
		t.Fatalf("read = %q", data)
	}
	// Moving onto an existing destination is refused and leaves both alone.
	if err := a.writeFile("other.txt", []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := a.movePath("dst/sub.txt", "other.txt"); err == nil {
		t.Fatal("move onto existing destination should fail")
	}
	if data, err := a.readFile("other.txt"); err != nil || string(data) != "keep" {
		t.Fatalf("other.txt = %q, %v", data, err)
	}
	if data, err := a.readFile("dst/sub.txt"); err != nil || string(data) != "move me" {
		t.Fatalf("dst/sub.txt = %q, %v", data, err)
	}
}

func TestFilesUpload(t *testing.T) {
	a, _ := newShellTestAgent(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "up.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("uploaded")); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("path", "uploads/up.txt"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/files/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.handleFilesUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	data, err := a.readFile("uploads/up.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "uploaded" {
		t.Fatalf("read = %q", data)
	}
}

func TestFilesUploadIntoDirectory(t *testing.T) {
	a, _ := newShellTestAgent(t)
	if err := a.mkdirPath("drop"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "named.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("named")); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("path", "drop/"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/files/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.handleFilesUpload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if data, err := a.readFile("drop/named.txt"); err != nil || string(data) != "named" {
		t.Fatalf("read = %q, %v", data, err)
	}
}

func TestFilesReadCap(t *testing.T) {
	a, mux := newShellTestAgent(t)
	home := a.homeDir()
	big := make([]byte, maxFileRead+1)
	if err := os.WriteFile(filepath.Join(home, "big.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.readFile("big.bin"); !strings.Contains(err.Error(), "10 MiB") {
		t.Fatalf("readFile big = %v, want size-cap error", err)
	}
	// Exactly 10 MiB reads fine.
	if err := os.WriteFile(filepath.Join(home, "exact.bin"), make([]byte, maxFileRead), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := a.readFile("exact.bin"); err != nil || len(data) != maxFileRead {
		t.Fatalf("read exact = %d bytes, %v", len(data), err)
	}
	// HTTP maps the cap to 413.
	srv := httptest.NewServer(mux)
	defer srv.Close()
	hdr := http.Header{"X-Dad-Token": {testToken}}
	if resp, _ := get(t, srv.URL+"/v1/files/read?path=big.bin", hdr); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestFilesMkdirMoveHTTP(t *testing.T) {
	a, _ := newShellTestAgent(t)
	rec := callHandler(t, a.handleFilesMkdir, http.MethodPost, "/v1/files/mkdir",
		struct {
			Path string `json:"path"`
		}{Path: "a/b/c"})
	if rec.Code != http.StatusOK {
		t.Fatalf("mkdir status = %d, body %q", rec.Code, rec.Body.String())
	}
	if info, err := os.Stat(filepath.Join(a.homeDir(), "a/b/c")); err != nil || !info.IsDir() {
		t.Fatalf("dir missing: %v", err)
	}
	if err := a.writeFile("a/b/c/f.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	rec = callHandler(t, a.handleFilesMove, http.MethodPost, "/v1/files/move",
		struct {
			From string `json:"from"`
			To   string `json:"to"`
		}{From: "a/b/c/f.txt", To: "moved.txt"})
	if rec.Code != http.StatusOK {
		t.Fatalf("move status = %d, body %q", rec.Code, rec.Body.String())
	}
	if _, err := a.readFile("moved.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestFilesRoutesGated(t *testing.T) {
	_, mux := newShellTestAgent(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	do := func(method, target, body string) int {
		var rdr *strings.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		} else {
			rdr = strings.NewReader("")
		}
		req, _ := http.NewRequest(method, srv.URL+target, rdr)
		req.Header.Set("X-Dad-Token", testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Reads are open (no session needed).
	if got := do(http.MethodGet, "/v1/files/list", ""); got != http.StatusOK {
		t.Fatalf("list status = %d, want 200", got)
	}
	// Every mutating route requires a session: no header -> 409.
	for _, tc := range []struct {
		method, target, body string
	}{
		{http.MethodPost, "/v1/files/write", `{"path":"x.txt","content":"x"}`},
		{http.MethodPost, "/v1/files/mkdir", `{"path":"d"}`},
		{http.MethodPost, "/v1/files/move", `{"from":"a","to":"b"}`},
		{http.MethodDelete, "/v1/files?path=x.txt", ""},
	} {
		if got := do(tc.method, tc.target, tc.body); got != http.StatusConflict {
			t.Fatalf("%s %s status = %d, want 409", tc.method, tc.target, got)
		}
	}
}
