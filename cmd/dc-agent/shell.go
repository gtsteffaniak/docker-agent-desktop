// Shell execution service: foreground/background jobs with process groups,
// capped output, and a jobs map. Routes are registered by registerShellRoutes.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxShellOutput caps captured stdout/stderr at 1 MiB each.
const maxShellOutput = 1 << 20

// maxShellJobs caps the jobs map; the oldest completed job is dropped first.
const maxShellJobs = 100

// ShellExecRequest is the body of POST /v1/shell/exec. Exactly one of Command
// and Argv must be set. Command runs via /bin/sh -c; Argv runs directly.
type ShellExecRequest struct {
	Command    string            `json:"command,omitempty"`
	Argv       []string          `json:"argv,omitempty"`
	Cwd        string            `json:"cwd,omitempty"`
	TimeoutMs  int               `json:"timeout_ms,omitempty"`
	Background bool              `json:"background,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// ShellExecResult is the outcome of one shell execution. ExitCode is nil for
// running, killed and timed-out jobs.
type ShellExecResult struct {
	JobID      string `json:"job_id"`
	Status     string `json:"status"` // completed | running | killed | failed | timeout
	ExitCode   *int   `json:"exit_code,omitempty"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// shellJob is one tracked execution. The waiter goroutine finalizes result
// and closes doneCh; killShellJob sets killed and SIGKILLs the process group.
type shellJob struct {
	seq      uint64
	cmd      *exec.Cmd
	result   ShellExecResult
	done     bool
	killed   bool
	timedOut bool
	doneCh   chan struct{}
}

// shellJobStore is the process-wide jobs map. It is package-level because the
// Agent struct lives in main.go (owned by the integrator); job ids are unique
// per process so sharing the store across Agent values is safe.
type shellJobStore struct {
	mu   sync.Mutex
	jobs map[string]*shellJob
	seq  uint64
}

var shellJobs = &shellJobStore{jobs: map[string]*shellJob{}}

func newJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return "job-" + hex.EncodeToString(b[:])
}

// cappedWriter keeps at most cap bytes and appends a truncation marker.
type cappedWriter struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.buf.Len() >= w.cap {
		w.truncated = true
		return len(p), nil
	}
	if room := w.cap - w.buf.Len(); len(p) > room {
		w.buf.Write(p[:room])
		w.truncated = true
	} else {
		w.buf.Write(p)
	}
	return len(p), nil
}

func (w *cappedWriter) String() string {
	if !w.truncated {
		return w.buf.String()
	}
	return w.buf.String() + "\n...truncated"
}

// execShell runs req synchronously unless req.Background, in which case it
// starts the process and returns immediately with status "running"; the
// completion is recorded in the jobs map. Non-zero exits are results with
// Status "failed", not errors; errors are for bad requests and start failures.
func (a *Agent) execShell(ctx context.Context, req ShellExecRequest) (ShellExecResult, error) {
	if (req.Command == "") == (len(req.Argv) == 0) {
		return ShellExecResult{}, errors.New("exactly one of command or argv is required")
	}
	timeoutMs := req.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = 30000
	}
	if timeoutMs < 1000 {
		timeoutMs = 1000
	}
	if timeoutMs > 300000 {
		timeoutMs = 300000
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = a.homeDir()
	}
	dir, err := a.jailPath(cwd)
	if err != nil {
		return ShellExecResult{}, err
	}

	// A background job must outlive the HTTP request, so detach its context
	// from the request; the timeout still applies.
	runCtx := ctx
	if req.Background {
		runCtx = context.WithoutCancel(ctx)
	}
	runCtx, cancel := context.WithTimeout(runCtx, time.Duration(timeoutMs)*time.Millisecond)

	var cmd *exec.Cmd
	if req.Command != "" {
		cmd = exec.CommandContext(runCtx, "/bin/sh", "-c", req.Command)
	} else {
		cmd = exec.CommandContext(runCtx, req.Argv[0], req.Argv[1:]...)
	}
	cmd.Dir = dir
	// Env adds/overrides variables but never PATH or HOME.
	env := os.Environ()
	for k, v := range req.Env {
		if k == "PATH" || k == "HOME" {
			continue
		}
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	// Own process group so a kill takes the whole tree (children included).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	id := newJobID()
	job := &shellJob{
		result: ShellExecResult{JobID: id, Status: "running"},
		cmd:    cmd,
		doneCh: make(chan struct{}),
	}
	// On timeout the context fires Cancel: kill the whole process group.
	cmd.Cancel = func() error {
		shellJobs.mu.Lock()
		job.timedOut = true
		shellJobs.mu.Unlock()
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	outW := &cappedWriter{cap: maxShellOutput}
	errW := &cappedWriter{cap: maxShellOutput}
	cmd.Stdout = outW
	cmd.Stderr = errW

	start := time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		return ShellExecResult{}, err
	}
	shellJobs.mu.Lock()
	shellJobs.seq++
	job.seq = shellJobs.seq
	shellJobs.jobs[id] = job
	shellJobs.mu.Unlock()

	go func() {
		waitErr := cmd.Wait()
		cancel() // release the timeout timer
		shellJobs.finish(id, waitErr, start, outW, errW)
	}()

	if req.Background {
		return ShellExecResult{JobID: id, Status: "running"}, nil
	}
	<-job.doneCh
	res, _ := a.shellJob(id)
	return res, nil
}

// finish finalizes a job's result after its process exits.
func (s *shellJobStore) finish(id string, waitErr error, start time.Time, outW, errW *cappedWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return
	}
	res := &j.result
	res.DurationMs = time.Since(start).Milliseconds()
	res.Stdout = outW.String()
	res.Stderr = errW.String()
	switch {
	case j.killed:
		res.Status = "killed"
	case j.timedOut:
		res.Status = "timeout"
		res.TimedOut = true
	case waitErr != nil:
		res.Status = "failed"
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			code := ee.ExitCode()
			res.ExitCode = &code
		}
	default:
		res.Status = "completed"
		code := 0
		res.ExitCode = &code
	}
	j.done = true
	close(j.doneCh)
	// Cap the map at maxShellJobs by dropping the oldest completed jobs.
	for len(s.jobs) > maxShellJobs {
		var oldestID string
		var oldestSeq uint64
		for cid, cj := range s.jobs {
			if !cj.done {
				continue
			}
			if oldestID == "" || cj.seq < oldestSeq {
				oldestID, oldestSeq = cid, cj.seq
			}
		}
		if oldestID == "" {
			break
		}
		delete(s.jobs, oldestID)
	}
}

// shellJob returns a copy of a job's current result.
func (a *Agent) shellJob(id string) (ShellExecResult, bool) {
	shellJobs.mu.Lock()
	defer shellJobs.mu.Unlock()
	j, ok := shellJobs.jobs[id]
	if !ok {
		return ShellExecResult{}, false
	}
	return j.result, true
}

// killShellJob SIGKILLs a running job's whole process group. It reports false
// for unknown or already-finished jobs.
func (a *Agent) killShellJob(id string) bool {
	shellJobs.mu.Lock()
	j, ok := shellJobs.jobs[id]
	if !ok || j.done {
		shellJobs.mu.Unlock()
		return false
	}
	j.killed = true
	pid := 0
	if j.cmd != nil && j.cmd.Process != nil {
		pid = j.cmd.Process.Pid
	}
	shellJobs.mu.Unlock()
	if pid == 0 {
		return false
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return true
}

// listShellJobs returns all tracked jobs, oldest first.
func (a *Agent) listShellJobs() []ShellExecResult {
	shellJobs.mu.Lock()
	defer shellJobs.mu.Unlock()
	out := make([]ShellExecResult, 0, len(shellJobs.jobs))
	ids := make([]string, 0, len(shellJobs.jobs))
	for id := range shellJobs.jobs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return shellJobs.jobs[ids[i]].seq < shellJobs.jobs[ids[j]].seq })
	for _, id := range ids {
		out = append(out, shellJobs.jobs[id].result)
	}
	return out
}

// shellDisplay is the human-readable command for event details.
func shellDisplay(req ShellExecRequest) string {
	if req.Command != "" {
		return req.Command
	}
	return strings.Join(req.Argv, " ")
}

func (a *Agent) handleShellExec(w http.ResponseWriter, r *http.Request) {
	var req ShellExecRequest
	if !decodeJSON(w, r, 1<<20, &req) {
		return
	}
	res, err := a.execShell(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	detail := shellDisplay(req) + " (status=" + res.Status
	if res.ExitCode != nil {
		detail += fmt.Sprintf(", exit=%d", *res.ExitCode)
	}
	if req.Background {
		detail += ", job=" + res.JobID
	}
	detail += ")"
	a.recordEvent("shell.exec", a.sessionHolderName(r), detail)
	writeJSON(w, res)
}

func (a *Agent) handleShellJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"jobs": a.listShellJobs()})
}

func (a *Agent) handleShellJob(w http.ResponseWriter, r *http.Request) {
	res, ok := a.shellJob(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	writeJSON(w, res)
}

func (a *Agent) handleShellJobKill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a.killShellJob(id) {
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	// Wait briefly for the waiter goroutine to finalize the result so the
	// response reflects the kill.
	shellJobs.mu.Lock()
	j, ok := shellJobs.jobs[id]
	shellJobs.mu.Unlock()
	if ok {
		select {
		case <-j.doneCh:
		case <-time.After(2 * time.Second):
		}
	}
	res, _ := a.shellJob(id)
	a.recordEvent("shell.exec", a.sessionHolderName(r), "kill job="+id+" (status="+res.Status+")")
	writeJSON(w, res)
}

// registerShellRoutes mounts the shell endpoints. Mutating routes are
// session-gated (auth first, then requireSession).
func (a *Agent) registerShellRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /v1/shell/exec", auth(a.requireSession(a.handleShellExec)))
	mux.HandleFunc("GET /v1/shell/jobs", auth(a.handleShellJobs))
	mux.HandleFunc("GET /v1/shell/jobs/{id}", auth(a.handleShellJob))
	mux.HandleFunc("POST /v1/shell/jobs/{id}/kill", auth(a.requireSession(a.handleShellJobKill)))
}
