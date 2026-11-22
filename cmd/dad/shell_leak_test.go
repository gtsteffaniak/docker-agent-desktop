package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitGone polls until pid no longer exists (signal 0 fails), or fails the test.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d still exists", pid)
}

func pidsFrom(t *testing.T, out string) []int {
	t.Helper()
	var pids []int
	for _, f := range strings.Fields(out) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("bad pid %q in %q", f, out)
		}
		pids = append(pids, n)
	}
	return pids
}

func TestClampShellTimeout(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 30000},
		{1, 1000},
		{-5, 1000},
		{999, 1000},
		{1000, 1000},
		{45000, 45000},
		{300000, 300000},
		{300001, 300000},
		{1 << 30, 300000},
	}
	for _, c := range cases {
		if got := clampShellTimeout(c.in); got != c.want {
			t.Errorf("clampShellTimeout(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// A foreground command that backgrounds a child holding stdout open used to
// block until that child exited, past its own timeout. It must now return
// once the main process exits, and the leftover child must be killed.
func TestShellForegroundLeftoverChildDoesNotHang(t *testing.T) {
	a, _ := newShellTestAgent(t)
	start := time.Now()
	res, err := a.execShell(t.Context(), ShellExecRequest{Command: "sleep 120 & echo $!", TimeoutMs: 60000})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > shellWaitDelay+3*time.Second {
		t.Fatalf("exec took %v; a leftover child blocked the result", took)
	}
	if res.Status != "completed" || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("result = %+v, want completed exit 0", res)
	}
	pids := pidsFrom(t, res.Stdout)
	if len(pids) != 1 {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	waitGone(t, pids[0])
}

// Killing a background job kills its whole process tree, and a poll while it
// runs returns the output captured so far.
func TestShellKillKillsWholeTree(t *testing.T) {
	a, _ := newShellTestAgent(t)
	res, err := a.execShell(t.Context(), ShellExecRequest{
		Command:    "sleep 120 & echo $!; sleep 120 & echo $!; wait",
		Background: true,
		TimeoutMs:  60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, ok := a.shellJob(res.JobID)
		if !ok {
			t.Fatal("job vanished")
		}
		if cur.Status != "running" {
			t.Fatalf("job finished early: %+v", cur)
		}
		if f := strings.Fields(cur.Stdout); len(f) == 2 {
			pids = pidsFrom(t, cur.Stdout)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(pids) != 2 {
		t.Fatal("never saw live output with both child pids")
	}
	if !a.killShellJob(res.JobID) {
		t.Fatal("kill reported false")
	}
	for _, pid := range pids {
		waitGone(t, pid)
	}
	shellJobs.mu.Lock()
	j := shellJobs.jobs[res.JobID]
	shellJobs.mu.Unlock()
	<-j.doneCh
	final, _ := a.shellJob(res.JobID)
	if final.Status != "killed" {
		t.Fatalf("status = %q, want killed", final.Status)
	}
}

// A foreground caller hanging up kills the job; that is a kill, not a timeout.
func TestShellCallerCancelIsKill(t *testing.T) {
	a, _ := newShellTestAgent(t)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	res, err := a.execShell(ctx, ShellExecRequest{Command: "sleep 30", TimeoutMs: 60000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "killed" || res.TimedOut {
		t.Fatalf("result = %+v, want killed without timed_out", res)
	}
}

// The timeout clamp applies end to end: timeout_ms 1 becomes 1 s.
func TestShellTimeoutClampedToOneSecond(t *testing.T) {
	a, _ := newShellTestAgent(t)
	start := time.Now()
	res, err := a.execShell(t.Context(), ShellExecRequest{Command: "sleep 10", TimeoutMs: 1})
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if res.Status != "timeout" || !res.TimedOut {
		t.Fatalf("result = %+v, want timeout", res)
	}
	if took < 900*time.Millisecond || took > 5*time.Second {
		t.Fatalf("took %v, want about 1 s", took)
	}
}

func TestShellRunningJobCap(t *testing.T) {
	a, _ := newShellTestAgent(t)
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			a.killShellJob(id)
			shellJobs.mu.Lock()
			j := shellJobs.jobs[id]
			shellJobs.mu.Unlock()
			if j != nil {
				<-j.doneCh
			}
		}
	})
	shellJobs.mu.Lock()
	already := shellJobs.runningLocked()
	shellJobs.mu.Unlock()
	for i := already; i < maxRunningShellJobs; i++ {
		res, err := a.execShell(t.Context(), ShellExecRequest{Command: "sleep 60", Background: true, TimeoutMs: 60000})
		if err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
		ids = append(ids, res.JobID)
	}
	if _, err := a.execShell(t.Context(), ShellExecRequest{Command: "true", Background: true}); !errors.Is(err, errTooManyJobs) {
		t.Fatalf("over the cap: err = %v, want errTooManyJobs", err)
	}
	rec := callHandler(t, a.handleShellExec, http.MethodPost, "/v1/shell/exec", ShellExecRequest{Command: "true"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("handler over the cap = %d, want 429", rec.Code)
	}
}

// Completed jobs' output is bounded in total, not just per job.
func TestShellRetainedOutputCap(t *testing.T) {
	a, _ := newShellTestAgent(t)
	for i := 0; i < 24; i++ {
		res, err := a.execShell(t.Context(), ShellExecRequest{Command: "head -c 1048576 /dev/zero | tr '\\0' a"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Stdout) != 1<<20 {
			t.Fatalf("stdout len = %d", len(res.Stdout))
		}
	}
	shellJobs.mu.Lock()
	total := 0
	for _, j := range shellJobs.jobs {
		if j.done {
			total += len(j.result.Stdout) + len(j.result.Stderr)
		}
	}
	shellJobs.mu.Unlock()
	if total > maxRetainedShellOutput {
		t.Fatalf("retained output = %d bytes, cap %d", total, maxRetainedShellOutput)
	}
}

// Exit codes survive ErrWaitDelay: a command whose child keeps the pipe open
// still reports its own exit status.
func TestShellExitCodeWithLeftoverChild(t *testing.T) {
	a, _ := newShellTestAgent(t)
	res, err := a.execShell(t.Context(), ShellExecRequest{Command: "sleep 120 & exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("result = %+v, want failed exit 3", res)
	}
}
