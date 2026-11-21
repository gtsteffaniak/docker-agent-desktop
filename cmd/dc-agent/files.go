// File service: everything jailed under the agent user's home directory.
// Routes are registered by registerFileRoutes.
package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxFileRead caps a single file read (and upload) at 10 MiB.
const maxFileRead = 10 << 20

var errPathEscape = errors.New("path escapes home directory")
var errTooLarge = errors.New("file exceeds 10 MiB limit")

// homeDir is the agent user's home, the root of the file jail.
func (a *Agent) homeDir() string {
	return envOr("HOME", "/home/bot")
}

// jailPath resolves p under the home directory. Relative paths are taken
// relative to home; absolute paths must already be under home. It rejects
// ".." escapes and symlinked escapes (EvalSymlinks on the nearest existing
// ancestor). The returned path is absolute and inside the jail.
func (a *Agent) jailPath(p string) (string, error) {
	home := a.homeDir()
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Join(home, p)
	}
	if abs != home && !strings.HasPrefix(abs, home+string(os.PathSeparator)) {
		return "", errPathEscape
	}
	// Resolve symlinks on the nearest existing ancestor so a symlinked
	// directory inside home cannot point outside it.
	dir := abs
	var suffix []string
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		suffix = append([]string{filepath.Base(dir)}, suffix...)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	homeResolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		homeResolved = home
	}
	out := resolved
	for _, s := range suffix {
		out = filepath.Join(out, s)
	}
	if out != homeResolved && !strings.HasPrefix(out, homeResolved+string(os.PathSeparator)) {
		return "", errPathEscape
	}
	return out, nil
}

// relHome renders an absolute jailed path as home-relative with "/" separators.
func (a *Agent) relHome(abs string) string {
	rel, err := filepath.Rel(a.homeDir(), abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// FileEntry is one directory listing entry. Path is home-relative.
type FileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Type    string    `json:"type"` // file | dir
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

func (a *Agent) listFiles(path string) ([]FileEntry, error) {
	abs, err := a.jailPath(path)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(ents))
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		fe := FileEntry{
			Name:    e.Name(),
			Path:    a.relHome(filepath.Join(abs, e.Name())),
			ModTime: info.ModTime(),
		}
		if e.IsDir() {
			fe.Type = "dir"
		} else {
			fe.Type = "file"
			fe.Size = info.Size()
		}
		out = append(out, fe)
	}
	return out, nil
}

// readFile returns a file's bytes, capped at 10 MiB.
func (a *Agent) readFile(path string) ([]byte, error) {
	abs, err := a.jailPath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	if info.Size() > maxFileRead {
		return nil, errTooLarge
	}
	return os.ReadFile(abs)
}

// writeFile writes data, creating parent directories as needed.
func (a *Agent) writeFile(path string, data []byte) error {
	abs, err := a.jailPath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, data, 0o644)
}

// deletePath removes a file or directory. Directories need recursive=true
// unless empty. The home directory itself can never be deleted.
func (a *Agent) deletePath(path string, recursive bool) error {
	abs, err := a.jailPath(path)
	if err != nil {
		return err
	}
	if abs == a.homeDir() {
		return errors.New("refusing to delete home directory")
	}
	if recursive {
		return os.RemoveAll(abs)
	}
	return os.Remove(abs)
}

func (a *Agent) mkdirPath(path string) error {
	abs, err := a.jailPath(path)
	if err != nil {
		return err
	}
	return os.MkdirAll(abs, 0o755)
}

// movePath renames within the jail. The destination must not already exist.
func (a *Agent) movePath(from, to string) error {
	fromAbs, err := a.jailPath(from)
	if err != nil {
		return err
	}
	if fromAbs == a.homeDir() {
		return errors.New("refusing to move home directory")
	}
	toAbs, err := a.jailPath(to)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(toAbs); err == nil {
		return fmt.Errorf("destination already exists: %s", to)
	}
	// Like writeFile, create the destination's parent directories.
	if err := os.MkdirAll(filepath.Dir(toAbs), 0o755); err != nil {
		return err
	}
	return os.Rename(fromAbs, toAbs)
}

// writeFileErr maps service errors to HTTP statuses: jail escapes 400,
// oversize 413, missing paths 404, everything else 500.
func writeFileErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPathEscape):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case os.IsNotExist(err):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *Agent) handleFilesList(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	entries, err := a.listFiles(path)
	if err != nil {
		writeFileErr(w, err)
		return
	}
	if entries == nil {
		entries = []FileEntry{}
	}
	writeJSON(w, map[string]any{"path": path, "entries": entries})
}

func (a *Agent) handleFilesRead(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	data, err := a.readFile(path)
	if err != nil {
		writeFileErr(w, err)
		return
	}
	ct := mime.TypeByExtension(filepath.Ext(path))
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
	}
	_, _ = w.Write(data)
}

type fileWriteRequest struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"` // text (default) | base64
}

func (a *Agent) handleFilesWrite(w http.ResponseWriter, r *http.Request) {
	var req fileWriteRequest
	if !decodeJSON(w, r, 16<<20, &req) {
		return
	}
	if req.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	var data []byte
	switch req.Encoding {
	case "", "text":
		data = []byte(req.Content)
	case "base64":
		b, err := base64.StdEncoding.DecodeString(req.Content)
		if err != nil {
			http.Error(w, "invalid base64 content: "+err.Error(), http.StatusBadRequest)
			return
		}
		data = b
	default:
		http.Error(w, "encoding must be text or base64", http.StatusBadRequest)
		return
	}
	if err := a.writeFile(req.Path, data); err != nil {
		writeFileErr(w, err)
		return
	}
	a.recordEvent("files.write", a.sessionHolderName(r), req.Path)
	abs, _ := a.jailPath(req.Path)
	writeJSON(w, map[string]any{"path": a.relHome(abs), "size": len(data)})
}

func (a *Agent) handleFilesUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "multipart field 'file' is required: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer f.Close()
	dest := r.FormValue("path")
	if dest == "" {
		http.Error(w, "form field 'path' is required", http.StatusBadRequest)
		return
	}
	// A trailing slash (or an existing directory) means "into this directory".
	if strings.HasSuffix(dest, "/") {
		dest = dest + hdr.Filename
	} else if abs, err := a.jailPath(dest); err == nil {
		if info, err := os.Stat(abs); err == nil && info.IsDir() {
			dest = filepath.Join(dest, hdr.Filename)
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileRead+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(data) > maxFileRead {
		http.Error(w, errTooLarge.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if err := a.writeFile(dest, data); err != nil {
		writeFileErr(w, err)
		return
	}
	a.recordEvent("files.write", a.sessionHolderName(r), dest)
	abs, _ := a.jailPath(dest)
	writeJSON(w, map[string]any{"path": a.relHome(abs), "size": len(data)})
}

func (a *Agent) handleFilesMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, 64<<10, &req) {
		return
	}
	if req.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if err := a.mkdirPath(req.Path); err != nil {
		writeFileErr(w, err)
		return
	}
	a.recordEvent("files.mkdir", a.sessionHolderName(r), req.Path)
	abs, _ := a.jailPath(req.Path)
	writeJSON(w, map[string]any{"path": a.relHome(abs)})
}

func (a *Agent) handleFilesMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !decodeJSON(w, r, 64<<10, &req) {
		return
	}
	if req.From == "" || req.To == "" {
		http.Error(w, "from and to are required", http.StatusBadRequest)
		return
	}
	if err := a.movePath(req.From, req.To); err != nil {
		writeFileErr(w, err)
		return
	}
	a.recordEvent("files.move", a.sessionHolderName(r), req.From+" -> "+req.To)
	fromAbs, _ := a.jailPath(req.From)
	toAbs, _ := a.jailPath(req.To)
	writeJSON(w, map[string]any{"from": a.relHome(fromAbs), "to": a.relHome(toAbs)})
}

func (a *Agent) handleFilesDelete(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	recursive := r.URL.Query().Get("recursive") == "1" || r.URL.Query().Get("recursive") == "true"
	if err := a.deletePath(path, recursive); err != nil {
		writeFileErr(w, err)
		return
	}
	a.recordEvent("files.delete", a.sessionHolderName(r), path)
	writeJSON(w, map[string]any{"path": path})
}

// registerFileRoutes mounts the file endpoints. Mutating routes are
// session-gated (auth first, then requireSession).
func (a *Agent) registerFileRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /v1/files/list", auth(a.handleFilesList))
	mux.HandleFunc("GET /v1/files/read", auth(a.handleFilesRead))
	mux.HandleFunc("POST /v1/files/write", auth(a.requireSession(a.handleFilesWrite)))
	mux.HandleFunc("POST /v1/files/upload", auth(a.requireSession(a.handleFilesUpload)))
	mux.HandleFunc("POST /v1/files/mkdir", auth(a.requireSession(a.handleFilesMkdir)))
	mux.HandleFunc("POST /v1/files/move", auth(a.requireSession(a.handleFilesMove)))
	mux.HandleFunc("DELETE /v1/files", auth(a.requireSession(a.handleFilesDelete)))
}
