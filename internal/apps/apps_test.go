package apps

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseFullEntry(t *testing.T) {
	a, err := Parse("# comment\n[app]\nid = terminal\nname = Terminal\nexec = lxterminal --geometry=120x34\nicon = lxterminal\norder = 5\n")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "terminal" || a.Name != "Terminal" || a.Icon != "lxterminal" || a.Order != 5 {
		t.Fatalf("app = %+v", a)
	}
	if len(a.Exec) != 2 || a.Exec[1] != "--geometry=120x34" {
		t.Fatalf("exec = %v", a.Exec)
	}
}

func TestParseDefaults(t *testing.T) {
	a, err := Parse("[app]\nid = files\nexec = thunar\n")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "files" || a.Order != defaultOrder {
		t.Fatalf("app = %+v", a)
	}
}

func TestParseRejectsBadEntries(t *testing.T) {
	for name, src := range map[string]string{
		"no section": "id = x\nexec = y\n",
		"no id":      "[app]\nexec = y\n",
		"bad id":     "[app]\nid = ../evil\nexec = y\n",
		"upper id":   "[app]\nid = Term\nexec = y\n",
		"no exec":    "[app]\nid = x\n",
		"bad order":  "[app]\nid = x\nexec = y\norder = soon\n",
		"no equals":  "[app]\nid x\n",
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("%s: accepted, want error", name)
		}
	}
}

func TestLoadOverridesAndSorts(t *testing.T) {
	system, user := t.TempDir(), t.TempDir()
	write(t, system, "a.desktop", "[app]\nid = terminal\nname = Terminal\nexec = lxterminal\norder = 20\n")
	write(t, system, "b.desktop", "[app]\nid = editor\nexec = mousepad\norder = 10\n")
	write(t, user, "t.desktop", "[app]\nid = terminal\nname = My Terminal\nexec = xterm\norder = 1\n")
	write(t, user, "x.desktop", "[app]\nid = extra\nexec = xclock\n")

	list, errs := Load(system, user, filepath.Join(t.TempDir(), "missing"))
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	var ids []string
	for _, a := range list {
		ids = append(ids, a.ID)
	}
	if want := []string{"terminal", "editor", "extra"}; len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if got, _ := Find(list, "terminal"); got.Name != "My Terminal" || got.Exec[0] != "xterm" {
		t.Fatalf("override not applied: %+v", got)
	}
}

func TestLoadSkipsInvalidFilesButKeepsOthers(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "bad.desktop", "[app]\nid = Bad\nexec = x\n")
	write(t, dir, "good.desktop", "[app]\nid = good\nexec = x\n")
	list, errs := Load(dir)
	if len(list) != 1 || list[0].ID != "good" || len(errs) != 1 {
		t.Fatalf("list=%v errs=%v", list, errs)
	}
}

func TestFindUnknown(t *testing.T) {
	if _, ok := Find(nil, "nope"); ok {
		t.Fatal("found nonexistent app")
	}
}
