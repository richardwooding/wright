package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/tools"
)

// jobFixture is a fixture whose bash tool can start background jobs.
func jobFixture(t *testing.T) (*fixture, *tools.JobSet) {
	t.Helper()
	jobs := tools.NewJobSet()
	f := newFixture(t, func(d *tools.Deps) { d.Jobs = jobs })
	t.Cleanup(func() { _ = jobs.Close() })
	return f, jobs
}

// waitFor polls until cond holds or the deadline passes. Background jobs are
// real processes, so a test has to wait for them; it must never wait forever.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestBackgroundJobReturnsAtOnce(t *testing.T) {
	f, jobs := jobFixture(t)

	// A command that outlives the call by a long way: if the call waited for
	// it, this test would take 30 seconds instead of milliseconds.
	start := time.Now()
	out, err := f.text(tools.NameBash, jsonArgs(map[string]any{
		"command": "echo hello; sleep 30", "background": true, "description": "a slow one",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("a background call took %s; it must not wait for the command", elapsed)
	}
	if !strings.Contains(out, "job_1") {
		t.Errorf("result = %q, want the job id", out)
	}

	list := jobs.List()
	if len(list) != 1 || !list[0].Running || list[0].Description != "a slow one" {
		t.Fatalf("jobs = %+v", list)
	}

	// The output arrives while the job is still running, which is the whole
	// point: a dev server is read before it exits, not after.
	waitFor(t, "the job's first output", func() bool {
		return strings.Contains(readJob(t, f, "job_1", false), "hello")
	})

	// Read again: only what is new, so polling does not re-send what the
	// model already has.
	if got := readJob(t, f, "job_1", false); !strings.Contains(got, "no new output") {
		t.Errorf("second read = %q, want nothing new", got)
	}
	// ...unless everything is asked for.
	if got := readJob(t, f, "job_1", true); !strings.Contains(got, "hello") {
		t.Errorf("read all = %q, want the earlier output", got)
	}

	// Killing it ends the job rather than leaving it to the 30s sleep.
	if _, err := f.text(tools.NameJob, jsonArgs(map[string]any{"action": "kill", "id": "job_1"})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the killed job to stop running", func() bool {
		return !jobs.List()[0].Running
	})
}

// readJob is the job tool's output action.
func readJob(t *testing.T, f *fixture, id string, all bool) string {
	t.Helper()
	args := map[string]any{"action": "output", "id": id}
	if all {
		args["all"] = true
	}
	out, err := f.text(tools.NameJob, jsonArgs(args))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestJobSetCloseKillsEveryJob pins the lifetime rule. A background job keeps
// whatever its own approval granted it — the network, an install's writable
// prefixes — for as long as it runs, so a job that outlived the session would
// be a process holding a grant nobody could see or revoke.
func TestJobSetCloseKillsEveryJob(t *testing.T) {
	f, jobs := jobFixture(t)
	for range 3 {
		if _, err := f.text(tools.NameBash, jsonArgs(map[string]any{
			"command": "sleep 60", "background": true,
		})); err != nil {
			t.Fatal(err)
		}
	}
	if len(jobs.List()) != 3 {
		t.Fatalf("jobs = %d, want 3", len(jobs.List()))
	}
	if err := jobs.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "every job to stop", func() bool {
		for _, j := range jobs.List() {
			if j.Running {
				return false
			}
		}
		return true
	})
	// Closing twice is safe, and a closed set starts nothing new.
	if err := jobs.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.text(tools.NameBash, jsonArgs(map[string]any{
		"command": "sleep 60", "background": true,
	})); err == nil {
		t.Error("a closed set must not start a job")
	}
}

// TestBackgroundWithoutAJobSetIsRefused pins that a session with nowhere to
// put a job never starts one — a process nothing can list or kill is exactly
// what the set exists to prevent.
func TestBackgroundWithoutAJobSetIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	_, err := f.text(tools.NameBash, jsonArgs(map[string]any{
		"command": "sleep 60", "background": true,
	}))
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if _, ok := tools.Lookup(f.ts, tools.NameJob); ok {
		t.Error("the job tool must not be registered without a job set")
	}
}

func TestJobToolErrors(t *testing.T) {
	f, _ := jobFixture(t)
	tests := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{name: "unknown action", args: map[string]any{"action": "restart"}, wantErr: "unknown action"},
		{name: "output without an id", args: map[string]any{"action": "output"}, wantErr: "needs an id"},
		{name: "kill without an id", args: map[string]any{"action": "kill"}, wantErr: "needs an id"},
		{name: "unknown job", args: map[string]any{"action": "output", "id": "job_99"}, wantErr: "no such job"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.text(tools.NameJob, jsonArgs(tt.args)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
	// An empty list is a sentence, not an empty string.
	out, err := f.text(tools.NameJob, jsonArgs(map[string]any{"action": "list"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No background jobs") {
		t.Errorf("list with nothing running = %q", out)
	}
}

// TestBackgroundJobRecordsHowItEnded pins that a finished job keeps its exit
// code: a job list that only says "not running" cannot tell a dev server that
// stopped cleanly from one that crashed.
func TestBackgroundJobRecordsHowItEnded(t *testing.T) {
	f, jobs := jobFixture(t)
	if _, err := f.text(tools.NameBash, jsonArgs(map[string]any{
		"command": "echo bye; exit 3", "background": true,
	})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to finish", func() bool { return !jobs.List()[0].Running })
	st := jobs.List()[0]
	if st.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", st.ExitCode)
	}
	if got := tools.FormatJobs(jobs.List()); !strings.Contains(got, "exited 3") {
		t.Errorf("/jobs = %q, want the exit code", got)
	}
	if got := readJob(t, f, "job_1", true); !strings.Contains(got, "bye") {
		t.Errorf("a finished job's output = %q", got)
	}
}

// TestBackgroundJobDoesNotMoveTheWorkingDirectory pins that a background
// command cannot change where the next foreground command runs. The two are
// concurrent, so a job that moved the shared cwd would move it under a
// command already deciding which files it was allowed to touch.
func TestBackgroundJobDoesNotMoveTheWorkingDirectory(t *testing.T) {
	jobs := tools.NewJobSet()
	t.Cleanup(func() { _ = jobs.Close() })
	var cwd *tools.CwdState
	f := newFixture(t, func(d *tools.Deps) {
		d.Jobs = jobs
		cwd = tools.NewCwd(d.WS.Root())
		d.Cwd = cwd
	})
	f.write("sub/keep.txt", "x")
	before := cwd.Get()
	if _, err := f.text(tools.NameBash, jsonArgs(map[string]any{
		"command": "cd sub && sleep 0.1", "background": true,
	})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to finish", func() bool { return !jobs.List()[0].Running })
	if after := cwd.Get(); after != before {
		t.Errorf("cwd moved from %q to %q because of a background job", before, after)
	}
}

func TestJobOutputIsRedacted(t *testing.T) {
	jobs := tools.NewJobSet()
	t.Cleanup(func() { _ = jobs.Close() })
	f := newFixture(t, func(d *tools.Deps) {
		d.Jobs = jobs
		d.Redactor = redact.New()
	})
	const secret = "sk_" + "live_0123456789abcdefghijklmn"
	if _, err := f.text(tools.NameBash, jsonArgs(map[string]any{
		"command": "echo " + secret, "background": true,
	})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to finish", func() bool { return !jobs.List()[0].Running })
	got := readJob(t, f, "job_1", true)
	if strings.Contains(got, secret) {
		t.Errorf("a job's output leaked a secret: %q", got)
	}
	if !strings.Contains(got, "redacted") {
		t.Errorf("output = %q, want the redaction marker", got)
	}
}

func TestJobsSurviveACancelledCall(t *testing.T) {
	f, jobs := jobFixture(t)
	// agentkit cancels the call's context when the tool returns, which for a
	// background call is immediately. The job must not die with it.
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := f.call(ctx, tools.NameBash, jsonArgs(map[string]any{
		"command": "sleep 5", "background": true,
	})); err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(200 * time.Millisecond)
	if st := jobs.List()[0]; !st.Running {
		t.Errorf("the job died with its call's context: %+v", st)
	}
}

// TestJobSetCloseKillsTheProcessTree is the assertion the "not running" check
// cannot make. cmd.Wait returns once the direct child is reaped and WaitDelay
// has elapsed, so a job can read as finished while the process it spawned is
// still alive — and a survivor keeps whatever the session granted it. This
// test asks the operating system instead.
func TestJobSetCloseKillsTheProcessTree(t *testing.T) {
	f, jobs := jobFixture(t)
	// The shell writes its own pid, then sleeps: killing only the direct
	// child would leave the sleep behind.
	if _, err := f.text(tools.NameBash, jsonArgs(map[string]any{
		"command": "echo $$ > pid; sleep 120", "background": true,
	})); err != nil {
		t.Fatal(err)
	}
	var pid int
	pidFile := filepath.Join(f.root, "pid")
	waitFor(t, "the job to report its pid", func() bool {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pid > 0
	})
	if err := jobs.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job's process to be gone", func() bool {
		return syscall.Kill(pid, 0) != nil
	})
}
