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

// maxRetainedShellOutput caps the captured output kept across all completed
// jobs. Each job may hold up to 2 MiB (stdout plus stderr), so without this a
// full jobs map could pin ~200 MiB; the oldest completed jobs are dropped
// first once the total passes this cap.
const maxRetainedShellOutput = 16 << 20

// maxRunningShellJobs caps concurrently running jobs, so a runaway caller
// cannot fork without bound through the API.
const maxRunningShellJobs = 32

// shellWaitDelay bounds how long a job's result waits for its output pipes
// after the job's main process exits. A command that leaves a child holding
// stdout open (for example "sleep 600 &") would otherwise block forever,
// because the timeout only covers the main process.
const shellWaitDelay = 2 * time.Second

// errTooManyJobs is returned when maxRunningShellJobs jobs are already running.
var errTooManyJobs = errors.New("too many running shell jobs")

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
	// out and errw capture output while the job runs, so a poll can return
	// partial output; finish copies them into result.
	out, errw *cappedWriter
	start     time.Time
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

// cappedWriter keeps at most cap bytes and appends a truncation marker. It is
// safe for concurrent use: exec's copy goroutines write while a poll reads.
type cappedWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	cap       int
	truncated bool
}

// Write keeps what fits under the cap and drops the rest. It always reports
// len(p) so the writing process never sees a short write.
func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
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

// String returns the captured output, with a marker when it was truncated.
func (w *cappedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
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
	timeoutMs := clampShellTimeout(req.TimeoutMs)
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
	outW := &cappedWriter{cap: maxShellOutput}
	errW := &cappedWriter{cap: maxShellOutput}
	job := &shellJob{
		result: ShellExecResult{JobID: id, Status: "running"},
		cmd:    cmd,
		doneCh: make(chan struct{}),
		out:    outW,
		errw:   errW,
	}
	// When the context ends, Cancel kills the whole process group. Only a
	// deadline is a timeout; a foreground caller that hangs up is a kill.
	cmd.Cancel = func() error {
		shellJobs.mu.Lock()
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			job.timedOut = true
		} else {
			job.killed = true
		}
		shellJobs.mu.Unlock()
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = shellWaitDelay
	cmd.Stdout = outW
	cmd.Stderr = errW

	// Reserve a running slot and register the job before Start, under one
	// lock, so the running cap cannot be raced past.
	shellJobs.mu.Lock()
	if shellJobs.runningLocked() >= maxRunningShellJobs {
		shellJobs.mu.Unlock()
		cancel()
		return ShellExecResult{}, errTooManyJobs
	}
	job.start = time.Now()
	if err := cmd.Start(); err != nil {
		shellJobs.mu.Unlock()
		cancel()
		return ShellExecResult{}, err
	}
	shellJobs.seq++
	job.seq = shellJobs.seq
	shellJobs.jobs[id] = job
	shellJobs.mu.Unlock()

	go func() {
		waitErr := cmd.Wait()
		// The group dies with its leader: anything the job left running in
		// its process group (backgrounded children, a stuck pipe holder) is
		// killed, so finished jobs never leak processes. ESRCH (nothing
		// left) is the normal case. Daemons that must outlive the job have
		// to leave the group themselves (setsid).
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cancel() // release the timeout timer
		shellJobs.finish(id, waitErr, cmd.ProcessState)
	}()

	if req.Background {
		return ShellExecResult{JobID: id, Status: "running"}, nil
	}
	<-job.doneCh
	res, _ := a.shellJob(id)
	return res, nil
}

// clampShellTimeout applies the exec timeout rules: 0 means the 30 s
// default, and anything else is clamped to 1 s..5 min.
func clampShellTimeout(ms int) int {
	if ms == 0 {
		return 30000
	}
	return min(max(ms, 1000), 300000)
}

// runningLocked counts jobs that have not finished. The caller holds s.mu.
func (s *shellJobStore) runningLocked() int {
	n := 0
	for _, j := range s.jobs {
		if !j.done {
			n++
		}
	}
	return n
}

// finish finalizes a job's result after its process exits. state is the
// main process's exit state; it is nil only when Wait failed before the
// process was reaped.
func (s *shellJobStore) finish(id string, waitErr error, state *os.ProcessState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return
	}
	res := &j.result
	res.DurationMs = time.Since(j.start).Milliseconds()
	res.Stdout = j.out.String()
	res.Stderr = j.errw.String()
	j.out, j.errw = nil, nil
	switch {
	case j.killed:
		res.Status = "killed"
	case j.timedOut:
		res.Status = "timeout"
		res.TimedOut = true
	case state != nil && state.Exited():
		// ErrWaitDelay (a child kept the output pipes open after the main
		// process exited) is not a failure of the command itself: the exit
		// status still decides.
		code := state.ExitCode()
		res.ExitCode = &code
		if code == 0 {
			res.Status = "completed"
		} else {
			res.Status = "failed"
		}
	default:
		_ = waitErr // killed by a signal, or Wait failed outright
		res.Status = "failed"
	}
	j.done = true
	close(j.doneCh)
	s.pruneLocked()
}

// pruneLocked drops the oldest completed jobs while the map holds more than
// maxShellJobs jobs or more than maxRetainedShellOutput bytes of output.
// Running jobs are never dropped. The caller holds s.mu.
func (s *shellJobStore) pruneLocked() {
	retained := 0
	for _, j := range s.jobs {
		if j.done {
			retained += len(j.result.Stdout) + len(j.result.Stderr)
		}
	}
	for len(s.jobs) > maxShellJobs || retained > maxRetainedShellOutput {
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
			return
		}
		old := s.jobs[oldestID]
		retained -= len(old.result.Stdout) + len(old.result.Stderr)
		delete(s.jobs, oldestID)
	}
}

// shellJob returns a copy of a job's current result. A running job reports
// the output captured so far and its elapsed time.
func (a *Agent) shellJob(id string) (ShellExecResult, bool) {
	shellJobs.mu.Lock()
	defer shellJobs.mu.Unlock()
	j, ok := shellJobs.jobs[id]
	if !ok {
		return ShellExecResult{}, false
	}
	return j.snapshotLocked(), true
}

// snapshotLocked returns the job's result, with live output while it runs.
// The caller holds shellJobs.mu.
func (j *shellJob) snapshotLocked() ShellExecResult {
	res := j.result
	if !j.done && j.out != nil {
		res.Stdout = j.out.String()
		res.Stderr = j.errw.String()
		res.DurationMs = time.Since(j.start).Milliseconds()
	}
	return res
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
		out = append(out, shellJobs.jobs[id].snapshotLocked())
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

// shellJobsResponse is the GET /v1/shell/jobs body.
type shellJobsResponse struct {
	Jobs []ShellExecResult `json:"jobs"`
}

// handleShellExec runs one command: synchronously unless background=true, in which case it
// starts the process and returns immediately with status "running".
// @Summary Run a shell command
// @Description Runs one command. Exactly one of command (/bin/sh -c) or argv (direct exec) must be set. Synchronously unless background=true, in which case it starts the process and returns immediately with status "running" and a job id to poll. Non-zero exits are results with status "failed", not errors. Output is capped at 1 MiB per stream.
// @Tags shell
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param body body ShellExecRequest true "Command to run"
// @Success 200 {object} ShellExecResult "Execution result (or a running job)"
// @Failure 400 {string} string "Neither or both of command/argv set"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Failure 429 {string} string "Too many running shell jobs"
// @Security ApiKeyAuth
// @Router /v1/shell/exec [post]
func (a *Agent) handleShellExec(w http.ResponseWriter, r *http.Request) {
	var req ShellExecRequest
	if !decodeJSON(w, r, 1<<20, &req) {
		return
	}
	res, err := a.execShell(r.Context(), req)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errTooManyJobs) {
			code = http.StatusTooManyRequests
		}
		http.Error(w, err.Error(), code)
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

// handleShellJobs lists all tracked jobs, oldest first. A running job reports the output
// captured so far.
// @Summary List shell jobs
// @Description All tracked jobs, oldest first. A running job reports the output captured so far and its elapsed time.
// @Tags shell
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Success 200 {object} shellJobsResponse "Tracked jobs"
// @Failure 401 {string} string "Missing or wrong token"
// @Security ApiKeyAuth
// @Router /v1/shell/jobs [get]
func (a *Agent) handleShellJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, shellJobsResponse{Jobs: a.listShellJobs()})
}

// handleShellJob returns one job's current result.
// @Summary Get a shell job
// @Description One job's current result. A running job reports the output captured so far and its elapsed time.
// @Tags shell
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param id path string true "Job id"
// @Success 200 {object} ShellExecResult "Job result"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 404 {string} string "Unknown job"
// @Security ApiKeyAuth
// @Router /v1/shell/jobs/{id} [get]
func (a *Agent) handleShellJob(w http.ResponseWriter, r *http.Request) {
	res, ok := a.shellJob(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	writeJSON(w, res)
}

// handleShellJobKill SIGKILLs a running job's whole process group. It waits briefly for the
// waiter goroutine to finalize the result so the response reflects the kill.
// @Summary Kill a shell job
// @Description SIGKILLs a running job's whole process group (children included). Waits briefly for the waiter to finalize the result, so the response reflects the kill.
// @Tags shell
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param X-Dad-Session header string true "Session id of the live session"
// @Param id path string true "Job id"
// @Success 200 {object} ShellExecResult "Finalized job result"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 404 {string} string "Unknown or already-finished job"
// @Failure 409 {object} sessionError "No live session, or held by someone else"
// @Security ApiKeyAuth
// @Router /v1/shell/jobs/{id}/kill [post]
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
	a.handle(mux, "POST /v1/shell/exec", auth(a.requireSession(a.handleShellExec)))
	a.handle(mux, "GET /v1/shell/jobs", auth(a.handleShellJobs))
	a.handle(mux, "GET /v1/shell/jobs/{id}", auth(a.handleShellJob))
	a.handle(mux, "POST /v1/shell/jobs/{id}/kill", auth(a.requireSession(a.handleShellJobKill)))
}
