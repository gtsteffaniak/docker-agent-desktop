package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newShellTestAgent returns an agent whose HOME is a temp dir, plus a mux
// with the shell and file routes mounted behind the token auth. Mutating
// routes are session-gated: callers must hold a session, so tests exercise
// mutating handlers directly (unwrapped) for success paths and check the
// 409 gate through the mux.
func newShellTestAgent(t *testing.T) (*Agent, *http.ServeMux) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	a.registerShellRoutes(mux, a.auth)
	a.registerFileRoutes(mux, a.auth)
	return a, mux
}

// callHandler invokes an unwrapped handler with a JSON body and returns the
// recorder. It is how mutating handlers are tested without a live session.
func callHandler(t *testing.T, h http.HandlerFunc, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("X-Dad-Token", testToken)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func decodeResult(t *testing.T, rec *httptest.ResponseRecorder) ShellExecResult {
	t.Helper()
	var res ShellExecResult
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("decode result: %v (body %q)", err, rec.Body.String())
	}
	return res
}

func TestShellExecEcho(t *testing.T) {
	a, _ := newShellTestAgent(t)
	res, err := a.execShell(t.Context(), ShellExecRequest{Command: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
	if res.Stdout != "hi\n" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	if res.JobID == "" || res.DurationMs < 0 {
		t.Fatalf("res = %+v", res)
	}
	// The completed job is queryable afterwards.
	if got, ok := a.shellJob(res.JobID); !ok || got.Status != "completed" {
		t.Fatalf("shellJob(%s) = %+v, %v", res.JobID, got, ok)
	}
}

func TestShellExecHTTP(t *testing.T) {
	a, _ := newShellTestAgent(t)
	rec := callHandler(t, a.handleShellExec, http.MethodPost, "/v1/shell/exec",
		ShellExecRequest{Command: "echo via-http"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if res := decodeResult(t, rec); res.Stdout != "via-http\n" || res.Status != "completed" {
		t.Fatalf("res = %+v", res)
	}
}

func TestShellExecFailingCommand(t *testing.T) {
	a, _ := newShellTestAgent(t)
	res, err := a.execShell(t.Context(), ShellExecRequest{Command: "exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("res = %+v", res)
	}
	// A missing command is a non-zero exit (127 from /bin/sh), not an error.
	res, err = a.execShell(t.Context(), ShellExecRequest{Command: "definitely-not-a-real-command-xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || res.ExitCode == nil || *res.ExitCode != 127 {
		t.Fatalf("res = %+v", res)
	}
}

func TestShellExecTimeout(t *testing.T) {
	a, _ := newShellTestAgent(t)
	res, err := a.execShell(t.Context(), ShellExecRequest{Command: "sleep 5", TimeoutMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "timeout" || !res.TimedOut || res.ExitCode != nil {
		t.Fatalf("res = %+v", res)
	}
	if res.DurationMs < 900 || res.DurationMs > 4000 {
		t.Fatalf("duration_ms = %d, want ~1000", res.DurationMs)
	}
}

func TestShellExecBackgroundPollKill(t *testing.T) {
	a, _ := newShellTestAgent(t)
	res, err := a.execShell(t.Context(), ShellExecRequest{Command: "sleep 30", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "running" || res.JobID == "" {
		t.Fatalf("res = %+v", res)
	}
	// Poll until the job shows up as running.
	deadline := time.Now().Add(5 * time.Second)
	var cur ShellExecResult
	for time.Now().Before(deadline) {
		var ok bool
		if cur, ok = a.shellJob(res.JobID); ok && cur.Status == "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cur.Status != "running" {
		t.Fatalf("job never became running: %+v", cur)
	}
	if !a.killShellJob(res.JobID) {
		t.Fatal("killShellJob returned false")
	}
	// Poll until the waiter finalizes it as killed.
	for time.Now().Before(deadline) {
		if cur, _ = a.shellJob(res.JobID); cur.Status == "killed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cur.Status != "killed" {
		t.Fatalf("job status = %s, want killed", cur.Status)
	}
	// Killing a finished job reports false.
	if a.killShellJob(res.JobID) {
		t.Fatal("killShellJob on a finished job should return false")
	}
	if a.killShellJob("job-does-not-exist") {
		t.Fatal("killShellJob on an unknown job should return false")
	}
	// The kill shows up in the jobs list.
	found := false
	for _, j := range a.listShellJobs() {
		if j.JobID == res.JobID && j.Status == "killed" {
			found = true
		}
	}
	if !found {
		t.Fatal("killed job missing from listShellJobs")
	}
}

func TestShellExecBackgroundCompletes(t *testing.T) {
	a, _ := newShellTestAgent(t)
	res, err := a.execShell(t.Context(), ShellExecRequest{Command: "echo bg-done", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var cur ShellExecResult
	for time.Now().Before(deadline) {
		if cur, _ = a.shellJob(res.JobID); cur.Status == "completed" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if cur.Status != "completed" || cur.Stdout != "bg-done\n" {
		t.Fatalf("job = %+v", cur)
	}
}

func TestShellExecArgv(t *testing.T) {
	a, _ := newShellTestAgent(t)
	res, err := a.execShell(t.Context(), ShellExecRequest{Argv: []string{"echo", "argv-ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" || res.Stdout != "argv-ok\n" {
		t.Fatalf("res = %+v", res)
	}
	// Argv with no shell: no globbing, no pipes.
	res, err = a.execShell(t.Context(), ShellExecRequest{Argv: []string{"echo", "*", "|", "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "* | x\n" {
		t.Fatalf("stdout = %q, want literal args", res.Stdout)
	}
}

func TestShellExecValidation(t *testing.T) {
	a, _ := newShellTestAgent(t)
	for _, req := range []ShellExecRequest{
		{}, // neither
		{Command: "echo x", Argv: []string{"echo"}}, // both
	} {
		if _, err := a.execShell(t.Context(), req); err == nil {
			t.Fatalf("execShell(%+v) should error", req)
		}
		rec := callHandler(t, a.handleShellExec, http.MethodPost, "/v1/shell/exec", req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("handler status = %d, want 400", rec.Code)
		}
	}
}

func TestShellExecCwdEscape(t *testing.T) {
	a, _ := newShellTestAgent(t)
	for _, cwd := range []string{"../", "/etc", "/etc/passwd", "../../tmp"} {
		if _, err := a.execShell(t.Context(), ShellExecRequest{Command: "pwd", Cwd: cwd}); err == nil {
			t.Fatalf("execShell cwd=%q should be rejected", cwd)
		}
		rec := callHandler(t, a.handleShellExec, http.MethodPost, "/v1/shell/exec",
			ShellExecRequest{Command: "pwd", Cwd: cwd})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("handler cwd=%q status = %d, want 400", cwd, rec.Code)
		}
	}
}

func TestShellExecOutputTruncation(t *testing.T) {
	a, _ := newShellTestAgent(t)
	// 2 MiB of stdout; the cap is 1 MiB.
	res, err := a.execShell(t.Context(), ShellExecRequest{Command: "head -c 2000000 /dev/zero | tr '\\0' 'x'"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" {
		t.Fatalf("res = %+v", res)
	}
	if !strings.HasSuffix(res.Stdout, "...truncated") {
		t.Fatal("stdout should carry the truncation marker")
	}
	if len(res.Stdout) > maxShellOutput+len("\n...truncated")+1 {
		t.Fatalf("stdout len = %d, exceeds 1 MiB cap", len(res.Stdout))
	}
}

func TestShellJobsHTTP(t *testing.T) {
	a, mux := newShellTestAgent(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	hdr := http.Header{"X-Dad-Token": {testToken}}

	// List starts empty.
	resp, body := get(t, srv.URL+"/v1/shell/jobs", hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var list struct {
		Jobs []ShellExecResult `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if list.Jobs == nil {
		t.Fatal("jobs should serialize as [] not null")
	}

	// Unknown job id -> 404.
	if resp, _ := get(t, srv.URL+"/v1/shell/jobs/job-nope", hdr); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	// Mutating routes are session-gated: no session header -> 409.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/shell/exec",
		strings.NewReader(`{"command":"echo x"}`))
	req.Header.Set("X-Dad-Token", testToken)
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("exec without session status = %d, want 409", resp.StatusCode)
		}
	}
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/shell/jobs/job-nope/kill", nil)
	req.Header.Set("X-Dad-Token", testToken)
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("kill without session status = %d, want 409", resp.StatusCode)
		}
	}

	// A completed job appears in the list after running one via the service.
	res, err := a.execShell(t.Context(), ShellExecRequest{Command: "echo listed"})
	if err != nil {
		t.Fatal(err)
	}
	resp, body = get(t, srv.URL+"/v1/shell/jobs", hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range list.Jobs {
		if j.JobID == res.JobID {
			found = true
		}
	}
	if !found {
		t.Fatalf("job %s missing from list: %s", res.JobID, body)
	}
	// And the single-job route serves it.
	resp, body = get(t, srv.URL+"/v1/shell/jobs/"+res.JobID, hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var one ShellExecResult
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatal(err)
	}
	if one.JobID != res.JobID || one.Status != "completed" {
		t.Fatalf("job = %+v", one)
	}
}

func TestShellKillHTTP(t *testing.T) {
	a, _ := newShellTestAgent(t)
	res, err := a.execShell(t.Context(), ShellExecRequest{Command: "sleep 30", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	// The handler reads the id from the mux path value, so set it on the
	// request the way the mux would.
	req := httptest.NewRequest(http.MethodPost, "/v1/shell/jobs/"+res.JobID+"/kill", nil)
	req.SetPathValue("id", res.JobID)
	rec := httptest.NewRecorder()
	a.handleShellJobKill(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if got := decodeResult(t, rec); got.Status != "killed" {
		t.Fatalf("kill result = %+v", got)
	}
	// Unknown job -> 404.
	req = httptest.NewRequest(http.MethodPost, "/v1/shell/jobs/job-nope/kill", nil)
	req.SetPathValue("id", "job-nope")
	rec = httptest.NewRecorder()
	a.handleShellJobKill(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestShellJobsCap(t *testing.T) {
	a, _ := newShellTestAgent(t)
	for i := 0; i < maxShellJobs+5; i++ {
		if _, err := a.execShell(t.Context(), ShellExecRequest{Command: fmt.Sprintf("echo %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(a.listShellJobs()); n > maxShellJobs {
		t.Fatalf("jobs = %d, want at most %d", n, maxShellJobs)
	}
}
