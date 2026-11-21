package main

import (
	"bytes"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jailFixture builds a home with a symlink to an outside directory, a dangling
// link to an outside path, and an in-home link to a directory.
func jailFixture(t *testing.T) (a *Agent, home, outside string) {
	t.Helper()
	a, _ = newShellTestAgent(t)
	home = a.homeResolved()
	outside = t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600))
	must(os.Mkdir(filepath.Join(home, "dir"), 0o755))
	must(os.WriteFile(filepath.Join(home, "dir", "f.txt"), []byte("f"), 0o600))
	must(os.Symlink(outside, filepath.Join(home, "out")))
	must(os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(home, "outfile")))
	must(os.Symlink(filepath.Join(outside, "missing"), filepath.Join(home, "dangling")))
	must(os.Symlink(filepath.Join(home, "dir"), filepath.Join(home, "indir")))
	return a, home, outside
}

func TestJailPathTable(t *testing.T) {
	a, home, _ := jailFixture(t)
	cases := []struct {
		in     string
		want   string // expected path when allowed
		escape bool   // expect errPathEscape
		err    bool   // expect some other error
	}{
		{in: "", want: home},
		{in: ".", want: home},
		{in: "a/b", want: home + "/a/b"},
		{in: "a/../b", want: home + "/b"},
		{in: "dir/f.txt", want: home + "/dir/f.txt"},
		{in: "indir/f.txt", want: home + "/dir/f.txt"},
		{in: home, want: home},
		{in: home + "/x", want: home + "/x"},
		{in: "new/deep/path.txt", want: home + "/new/deep/path.txt"},
		{in: "..", escape: true},
		{in: "../x", escape: true},
		{in: "a/../../x", escape: true},
		{in: home + "/../x", escape: true},
		{in: home + "x/evil", escape: true}, // sibling with a shared prefix
		{in: "/etc/passwd", escape: true},
		{in: "/", escape: true},
		{in: "out", escape: true},
		{in: "out/secret.txt", escape: true},
		{in: "out/new/dir", escape: true},
		{in: "outfile", escape: true},
		{in: "dangling", err: true},
	}
	for _, c := range cases {
		got, err := a.jailPath(c.in)
		switch {
		case c.escape:
			if !errors.Is(err, errPathEscape) {
				t.Errorf("jailPath(%q) = %q, %v; want errPathEscape", c.in, got, err)
			}
		case c.err:
			if err == nil {
				t.Errorf("jailPath(%q) = %q; want an error", c.in, got)
			}
		default:
			if err != nil || got != c.want {
				t.Errorf("jailPath(%q) = %q, %v; want %q", c.in, got, err, c.want)
			}
		}
	}
}

func TestJailPathNoFollowTable(t *testing.T) {
	a, home, _ := jailFixture(t)
	cases := []struct {
		in     string
		want   string
		escape bool
	}{
		{in: "out", want: home + "/out"},
		{in: "outfile", want: home + "/outfile"},
		{in: "dangling", want: home + "/dangling"},
		{in: "indir", want: home + "/indir"},
		{in: "", want: home},
		{in: "x/..", want: home},
		{in: "out/secret.txt", escape: true},
		{in: "../x", escape: true},
		{in: "/etc/passwd", escape: true},
	}
	for _, c := range cases {
		got, err := a.jailPathNoFollow(c.in)
		if c.escape {
			if !errors.Is(err, errPathEscape) {
				t.Errorf("jailPathNoFollow(%q) = %q, %v; want errPathEscape", c.in, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("jailPathNoFollow(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// Deleting a link removes the link, never what it points to.
func TestDeleteSymlinkRemovesLinkOnly(t *testing.T) {
	a, home, outside := jailFixture(t)
	if err := a.deletePath("indir", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, "indir")); !os.IsNotExist(err) {
		t.Fatalf("link still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "dir", "f.txt")); err != nil {
		t.Fatalf("link target was deleted: %v", err)
	}
	// A link that points outside home can be cleaned up; the outside file stays.
	if err := a.deletePath("outfile", false); err != nil {
		t.Fatal(err)
	}
	if err := a.deletePath("out", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatalf("outside file touched: %v", err)
	}
}

func TestMoveSymlinkMovesLink(t *testing.T) {
	a, home, _ := jailFixture(t)
	if err := a.movePath("indir", "renamed"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(home, "renamed"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("renamed is not the link: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(home, "dir", "f.txt")); err != nil {
		t.Fatalf("target moved: %v", err)
	}
	if err := a.movePath("missing-src", "x"); !os.IsNotExist(err) {
		t.Fatalf("move of a missing source = %v, want not-exist", err)
	}
	if err := a.movePath("dir", "out/stolen"); !errors.Is(err, errPathEscape) {
		t.Fatalf("move into an outside link = %v, want errPathEscape", err)
	}
}

// When HOME itself is reached through a symlink, home is still protected.
func TestDeleteHomeThroughSymlinkedHome(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", link)
	a := &Agent{token: testToken}
	for _, p := range []string{"", ".", link, real, "x/.."} {
		if err := a.deletePath(p, true); err == nil {
			t.Fatalf("deletePath(%q) deleted home", p)
		}
	}
	if err := a.movePath("", "elsewhere"); err == nil {
		t.Fatal("moved home")
	}
	if err := a.writeFile("a.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	ents, err := a.listFiles("")
	if err != nil || len(ents) != 1 || ents[0].Path != "a.txt" {
		t.Fatalf("list under symlinked home = %+v, %v", ents, err)
	}
}

func TestWriteOverLimitIs413(t *testing.T) {
	a, _ := newShellTestAgent(t)
	big := bytes.Repeat([]byte("a"), maxFileRead+1)
	if err := a.writeFile("big.txt", big); !errors.Is(err, errTooLarge) {
		t.Fatalf("writeFile over limit = %v, want errTooLarge", err)
	}
	rec := callHandler(t, a.handleFilesWrite, http.MethodPost, "/v1/files/write",
		fileWriteRequest{Path: "big.txt", Content: string(big)})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("handler status = %d, want 413", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(a.homeDir(), "big.txt")); !os.IsNotExist(err) {
		t.Fatal("oversized file was written")
	}
}

func TestUploadBodyOverLimitIs413(t *testing.T) {
	a, _ := newShellTestAgent(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("path", "up.bin")
	fw, _ := mw.CreateFormFile("file", "up.bin")
	_, _ = fw.Write(bytes.Repeat([]byte("z"), maxFileRead+(2<<20)))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/files/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.handleFilesUpload(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d (%s), want 413", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
