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

// errProtectedHome is returned for a delete or move of the home directory itself.
var errProtectedHome = errors.New("refusing to delete or move the home directory")

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

// homeResolved is the home directory with symlinks resolved, the form
// jailPath returns paths in.
func (a *Agent) homeResolved() string {
	if h, err := filepath.EvalSymlinks(a.homeDir()); err == nil {
		return h
	}
	return a.homeDir()
}

// jailPathNoFollow jails p like jailPath, except that a final symlink is not
// followed: the returned path names the link itself. Delete and move act on
// the entry the caller named, so removing a link never removes its target
// and a link pointing outside home can still be cleaned up.
func (a *Agent) jailPathNoFollow(p string) (string, error) {
	home := a.homeDir()
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(home, p)
	}
	abs = filepath.Clean(abs)
	if abs == home {
		return a.homeResolved(), nil
	}
	parent, err := a.jailPath(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// relHome renders an absolute jailed path as home-relative with "/" separators.
func (a *Agent) relHome(abs string) string {
	rel, err := filepath.Rel(a.homeResolved(), abs)
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

// writeFile writes data, creating parent directories as needed. It refuses
// content over maxFileRead, so everything written can be read back.
func (a *Agent) writeFile(path string, data []byte) error {
	if len(data) > maxFileRead {
		return errTooLarge
	}
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
	abs, err := a.jailPathNoFollow(path)
	if err != nil {
		return err
	}
	if abs == a.homeResolved() {
		return errProtectedHome
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
	fromAbs, err := a.jailPathNoFollow(from)
	if err != nil {
		return err
	}
	if fromAbs == a.homeResolved() {
		return errProtectedHome
	}
	if _, err := os.Lstat(fromAbs); err != nil {
		return err
	}
	toAbs, err := a.jailPathNoFollow(to)
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

// writeFileErr maps service errors to HTTP statuses: jail escapes and the
// protected home directory 400,
// oversize 413, missing paths 404, everything else 500.
func writeFileErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPathEscape), errors.Is(err, errProtectedHome):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case os.IsNotExist(err):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// filesListResponse is the GET /v1/files/list body.
type filesListResponse struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// fileWriteResponse is the POST /v1/files/write and /v1/files/upload body.
type fileWriteResponse struct {
	Path string `json:"path"`
	Size int    `json:"size"`
}

// filePathResponse is the POST /v1/files/mkdir and DELETE /v1/files body.
type filePathResponse struct {
	Path string `json:"path"`
}

// fileMoveResponse is the POST /v1/files/move body.
type fileMoveResponse struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// fileMkdirRequest is the body of POST /v1/files/mkdir.
type fileMkdirRequest struct {
	Path string `json:"path"`
}

// fileMoveRequest is the body of POST /v1/files/move.
type fileMoveRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// handleFilesList lists one jailed directory. ?path= is home-relative (empty means home).
// @Summary List files
// @Description Lists one jailed directory. ?path= is home-relative (empty means home). Paths are home-relative in the reply.
// @Tags files
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param path query string false "Home-relative directory (empty means home)"
// @Success 200 {object} filesListResponse "Directory listing"
// @Failure 400 {string} string "Path escapes the home jail"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 404 {string} string "Directory not found"
// @Security ApiKeyAuth
// @Router /v1/files/list [get]
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
	writeJSON(w, filesListResponse{Path: path, Entries: entries})
}

// handleFilesRead returns one file's bytes, capped at 10 MiB. ?download=1 adds a
// Content-Disposition attachment header.
// @Summary Read a file
// @Description Returns one file's bytes, capped at 10 MiB. ?download=1 adds a Content-Disposition attachment header.
// @Tags files
// @Produce octet-stream
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param path query string true "Home-relative file path"
// @Param download query string false "Send as a download attachment" Enums(1)
// @Success 200 {file} string "File bytes"
// @Failure 400 {string} string "Path escapes the home jail, or is a directory"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 404 {string} string "File not found"
// @Failure 413 {string} string "File exceeds the 10 MiB limit"
// @Security ApiKeyAuth
// @Router /v1/files/read [get]
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

// handleFilesWrite writes data, creating parent directories as needed. It refuses content over
// 10 MiB, so everything written can be read back.
// @Summary Write a file
// @Description Writes data, creating parent directories as needed. Content over 10 MiB is refused, so everything written can be read back. Encoding "text" (default) writes the content as-is; "base64" decodes it first.
// @Tags files
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body fileWriteRequest true "Path, content and encoding"
// @Success 200 {object} fileWriteResponse "Written path and byte count"
// @Failure 400 {string} string "Path missing, path escapes the jail, or bad encoding/base64"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 413 {string} string "Content exceeds the 10 MiB limit"
// @Security ApiKeyAuth
// @Router /v1/files/write [post]
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
	writeJSON(w, fileWriteResponse{Path: a.relHome(abs), Size: len(data)})
}

// handleFilesUpload writes one multipart file. The whole body is bounded, not just the file
// part, so an oversized upload cannot fill the disk with multipart temp files.
// @Summary Upload a file
// @Description Writes one multipart file (field "file") to the "path" form field. A trailing slash (or an existing directory) means "into this directory". The whole body is bounded, so an oversized upload cannot fill the disk with multipart temp files.
// @Tags files
// @Accept multipart/form-data
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param file formData file true "File to upload"
// @Param path formData string true "Home-relative destination path (or directory)"
// @Success 200 {object} fileWriteResponse "Written path and byte count"
// @Failure 400 {string} string "Missing file/path fields, or path escapes the jail"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 413 {string} string "File exceeds the 10 MiB limit"
// @Security ApiKeyAuth
// @Router /v1/files/upload [post]
func (a *Agent) handleFilesUpload(w http.ResponseWriter, r *http.Request) {
	// Bound the whole body, not just the file part, so an oversized upload
	// cannot fill the disk with multipart temp files.
	r.Body = http.MaxBytesReader(w, r.Body, maxFileRead+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, errTooLarge.Error(), http.StatusRequestEntityTooLarge)
			return
		}
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
	writeJSON(w, fileWriteResponse{Path: a.relHome(abs), Size: len(data)})
}

// handleFilesMkdir creates a directory and its parents.
// @Summary Make a directory
// @Description Creates a directory and its parents, jailed under home.
// @Tags files
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body fileMkdirRequest true "Directory path"
// @Success 200 {object} filePathResponse "Created path"
// @Failure 400 {string} string "Path missing or escapes the jail"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Security ApiKeyAuth
// @Router /v1/files/mkdir [post]
func (a *Agent) handleFilesMkdir(w http.ResponseWriter, r *http.Request) {
	var req fileMkdirRequest
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
	writeJSON(w, filePathResponse{Path: a.relHome(abs)})
}

// handleFilesMove renames within the jail. The destination must not already exist.
// @Summary Move or rename a file
// @Description Renames within the jail. The destination must not already exist. Deleting or moving the home directory itself is refused.
// @Tags files
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body fileMoveRequest true "Source and destination paths"
// @Success 200 {object} fileMoveResponse "Moved paths"
// @Failure 400 {string} string "from/to missing, path escapes the jail, destination exists, or home itself"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 404 {string} string "Source not found"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Security ApiKeyAuth
// @Router /v1/files/move [post]
func (a *Agent) handleFilesMove(w http.ResponseWriter, r *http.Request) {
	var req fileMoveRequest
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
	fromAbs, _ := a.jailPathNoFollow(req.From)
	toAbs, _ := a.jailPathNoFollow(req.To)
	writeJSON(w, fileMoveResponse{From: a.relHome(fromAbs), To: a.relHome(toAbs)})
}

// handleFilesDelete removes a file or directory. Directories need recursive=true unless
// empty. The home directory itself can never be deleted.
// @Summary Delete a file or directory
// @Description Removes a file or directory. Directories need recursive=true unless empty. The home directory itself can never be deleted. A final symlink is removed itself, never its target.
// @Tags files
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param path query string true "Home-relative path to delete"
// @Param recursive query string false "Recursively delete a non-empty directory" Enums(1, true)
// @Success 200 {object} filePathResponse "Deleted path"
// @Failure 400 {string} string "Path missing, escapes the jail, is home itself, or a non-empty directory without recursive"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 404 {string} string "Path not found"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Security ApiKeyAuth
// @Router /v1/files [delete]
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
	writeJSON(w, filePathResponse{Path: path})
}

// registerFileRoutes mounts the file endpoints. Mutating routes are
// session-gated (auth first, then requireSession).
func (a *Agent) registerFileRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	a.handle(mux, "GET /v1/files/list", auth(a.handleFilesList))
	a.handle(mux, "GET /v1/files/read", auth(a.handleFilesRead))
	a.handle(mux, "POST /v1/files/write", auth(a.requireSession(a.handleFilesWrite)))
	a.handle(mux, "POST /v1/files/upload", auth(a.requireSession(a.handleFilesUpload)))
	a.handle(mux, "POST /v1/files/mkdir", auth(a.requireSession(a.handleFilesMkdir)))
	a.handle(mux, "POST /v1/files/move", auth(a.requireSession(a.handleFilesMove)))
	a.handle(mux, "DELETE /v1/files", auth(a.requireSession(a.handleFilesDelete)))
}
