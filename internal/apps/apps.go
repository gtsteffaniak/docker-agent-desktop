// Package apps loads the desktop app registry shared by the agent and the dock.
//
// Each app is a small file in an apps.d directory, using the same [app] layout as Quantum Bots:
//
//	[app]
//	id = terminal
//	name = Terminal
//	exec = lxterminal --geometry=120x34
//	icon = lxterminal
//	order = 10
//
// exec is split on whitespace and run directly. There is no shell, so quotes and pipes
// are not interpreted. Later directories override earlier ones by id.
package apps

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// App is one launchable desktop application.
type App struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Icon  string `json:"icon"`
	Order int    `json:"order"`
	// Port is the Broadway port this app's display listens on, or 0 for apps without one (the
	// browser, which is served by Chromium's screencast instead).
	Port int      `json:"port"`
	Exec []string `json:"exec"`
}

const defaultOrder = 100

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Parse reads one app definition. It returns an error for a missing id or exec, or an invalid id.
func Parse(src string) (App, error) {
	var (
		a         App
		inApp     bool
		sawApp    bool
		execField string
	)
	a.Order = defaultOrder
	sc := bufio.NewScanner(strings.NewReader(src))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inApp = line == "[app]"
			sawApp = sawApp || inApp
			continue
		}
		if !inApp {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return App{}, fmt.Errorf("line %d: expected key = value", n)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "id":
			a.ID = val
		case "name":
			a.Name = val
		case "exec":
			execField = val
		case "icon":
			a.Icon = val
		case "order":
			o, err := strconv.Atoi(val)
			if err != nil {
				return App{}, fmt.Errorf("line %d: bad order %q", n, val)
			}
			a.Order = o
		case "port":
			p, err := strconv.Atoi(val)
			if err != nil || p < 0 || p > 65535 {
				return App{}, fmt.Errorf("line %d: bad port %q", n, val)
			}
			a.Port = p
		}
	}
	if !sawApp {
		return App{}, fmt.Errorf("no [app] section")
	}
	if !idPattern.MatchString(a.ID) {
		return App{}, fmt.Errorf("invalid id %q (lowercase letters, digits, - and _ only)", a.ID)
	}
	a.Exec = strings.Fields(execField)
	if len(a.Exec) == 0 {
		return App{}, fmt.Errorf("app %q has no exec", a.ID)
	}
	if a.Name == "" {
		a.Name = a.ID
	}
	return a, nil
}

// Load reads every *.desktop file in dirs. Missing directories are skipped. A later directory
// replaces an earlier app with the same id. Unreadable or invalid files are reported in the
// returned error list but do not stop the rest from loading. The result is sorted by order, then id.
func Load(dirs ...string) ([]App, []error) {
	byID := map[string]App{}
	var errs []error
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.desktop"))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		sort.Strings(files)
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			a, err := Parse(string(b))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", f, err))
				continue
			}
			byID[a.ID] = a
		}
	}
	out := make([]App, 0, len(byID))
	for _, a := range byID {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
	return out, errs
}

// Find returns the app with the given id.
func Find(list []App, id string) (App, bool) {
	for _, a := range list {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}
