package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/policy"
)

// maxJobOutput is how much of a background job's output is kept in memory.
// A dev server left running for an hour would otherwise grow without bound,
// so the oldest bytes are dropped and the drop is reported rather than
// hidden — output that silently vanished is worse than output that says it
// did.
const maxJobOutput = 256 * 1024

// JobSet is the background bash jobs of one session.
//
// Jobs do not outlive the session: Close kills every one of them. A
// background job keeps whatever the approval of its own call granted it —
// the network, an install's writable prefixes — for as long as it runs, so
// letting one survive the session would leave a process holding a grant
// nobody could see or revoke. (A SIGKILL of wright itself still orphans
// them; nothing in a process can promise otherwise.)
type JobSet struct {
	mu     sync.Mutex
	jobs   map[string]*Job
	order  []string
	next   int
	closed bool
}

// NewJobSet returns an empty set.
func NewJobSet() *JobSet {
	return &JobSet{jobs: map[string]*Job{}}
}

// Job is one background command.
type Job struct {
	ID          string
	Command     string
	Description string
	Started     time.Time

	out    jobBuffer
	cancel context.CancelFunc

	mu       sync.Mutex
	done     bool
	exitCode int
	failure  string
	finished time.Time
}

// JobStatus is a snapshot of one job, for /jobs and the job tool.
type JobStatus struct {
	ID          string
	Command     string
	Description string
	Running     bool
	ExitCode    int
	Failure     string
	Elapsed     time.Duration
	Output      int // bytes currently held
}

// add registers a job under a fresh id. It returns false once the set is
// closed, so a job cannot be started during shutdown and then leak.
func (s *JobSet) add(j *Job) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.next++
	j.ID = "job_" + strconv.Itoa(s.next)
	s.jobs[j.ID] = j
	s.order = append(s.order, j.ID)
	return true
}

// Get returns a job by id.
func (s *JobSet) Get(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	return j, ok
}

// List returns every job, oldest first.
func (s *JobSet) List() []JobStatus {
	s.mu.Lock()
	jobs := make([]*Job, 0, len(s.order))
	for _, id := range s.order {
		jobs = append(jobs, s.jobs[id])
	}
	s.mu.Unlock()
	out := make([]JobStatus, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Status())
	}
	return out
}

// Close kills every job and marks the set closed. It is safe to call twice.
func (s *JobSet) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	jobs := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.Unlock()
	for _, j := range jobs {
		j.Kill()
	}
	return nil
}

// Status snapshots the job.
func (j *Job) Status() JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := JobStatus{
		ID: j.ID, Command: j.Command, Description: j.Description,
		Running: !j.done, ExitCode: j.exitCode, Failure: j.failure,
		Output: j.out.len(),
	}
	end := time.Now()
	if j.done {
		end = j.finished
	}
	st.Elapsed = end.Sub(j.Started).Round(time.Millisecond)
	return st
}

// Kill stops the job. A job that has already finished is untouched.
func (j *Job) Kill() {
	if j.cancel != nil {
		j.cancel()
	}
}

// finish records the outcome exactly once.
func (j *Job) finish(code int, failure string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done {
		return
	}
	j.done, j.exitCode, j.failure, j.finished = true, code, failure, time.Now()
}

// jobBuffer holds a job's output, capped, with a cursor so a reader can ask
// for what it has not seen. A long-running job is polled, not read once, so
// re-reading the whole buffer every time would fill the model's context with
// what it already has.
type jobBuffer struct {
	mu      sync.Mutex
	buf     []byte
	read    int // bytes handed out by take
	dropped int // bytes discarded from the front
}

func (b *jobBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - maxJobOutput; over > 0 {
		b.buf = slices.Delete(b.buf, 0, over)
		b.dropped += over
		b.read = max(b.read-over, 0)
	}
	return len(p), nil
}

func (b *jobBuffer) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

// take returns the output not yet taken, and how many bytes were dropped
// before it.
func (b *jobBuffer) take() (s string, dropped int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s = string(b.buf[b.read:])
	b.read = len(b.buf)
	dropped, b.dropped = b.dropped, 0
	return s, dropped
}

// all returns everything held, without moving the cursor.
func (b *jobBuffer) all() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// FormatJobs renders the job list for /jobs.
func FormatJobs(jobs []JobStatus) string {
	if len(jobs) == 0 {
		return "No background jobs.\n\nStart one by asking for a command with background: true."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d background job(s):\n", len(jobs))
	for _, j := range jobs {
		fmt.Fprintf(&b, "\n  %s  %s\n", j.ID, jobState(j))
		fmt.Fprintf(&b, "    $ %s\n", singleLine(j.Command))
		if j.Description != "" {
			fmt.Fprintf(&b, "    %s\n", j.Description)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// jobState is the one-line status: running and for how long, or how it ended.
func jobState(j JobStatus) string {
	switch {
	case j.Running:
		return fmt.Sprintf("running for %s", j.Elapsed)
	case j.Failure != "":
		return fmt.Sprintf("failed after %s: %s", j.Elapsed, j.Failure)
	default:
		return fmt.Sprintf("exited %d after %s", j.ExitCode, j.Elapsed)
	}
}

// singleLine collapses a script onto one line so a job list stays a list.
func singleLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 100 {
		return s[:100] + "…"
	}
	return s
}

// jobArgs is the job tool's argument.
type jobArgs struct {
	Action string `json:"action" jsonschema:"one of: list, output, kill"`
	ID     string `json:"id,omitempty" jsonschema:"the job id, required for output and kill"`
	All    bool   `json:"all,omitempty" jsonschema:"for output: return everything held, not just what is new since the last read"`
}

func (d *Deps) job() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameJob,
			"Inspect the background commands this session started: list them, read a job's new output since you last read it (or all of it), or kill one.",
			d.runJob),
		describe: describeJob,
	}
}

func describeJob(args json.RawMessage) (policy.Request, Preview, error) {
	var a jobArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	title := NameJob + " " + a.Action
	if a.ID != "" {
		title += " " + a.ID
	}
	return policy.Request{Tool: NameJob, Args: args}, Preview{Title: title}, nil
}

func (d *Deps) runJob(ctx context.Context, a jobArgs) (agentkit.Output, error) {
	if d.Jobs == nil {
		return agentkit.Output{}, errors.New("background jobs are not available in this session")
	}
	switch a.Action {
	case "list":
		return agentkit.Text(FormatJobs(d.Jobs.List())), nil
	case "output", "kill":
	default:
		return agentkit.Output{}, fmt.Errorf("unknown action %q; use list, output or kill", a.Action)
	}
	if a.ID == "" {
		return agentkit.Output{}, fmt.Errorf("%s needs an id; run the list action to see them", a.Action)
	}
	j, ok := d.Jobs.Get(a.ID)
	if !ok {
		return agentkit.Output{}, fmt.Errorf("no such job %q; run the list action to see them", a.ID)
	}
	if a.Action == "kill" {
		j.Kill()
		return agentkit.Text("Killed " + j.ID + "."), nil
	}
	return agentkit.Text(d.jobOutput(ctx, j, a.All)), nil
}

// jobOutput renders a job's output. Redaction runs here rather than at write
// time because the buffer is also what /jobs shows, and a secret must not
// reach the model or the screen either way.
func (d *Deps) jobOutput(ctx context.Context, j *Job, all bool) string {
	var text string
	var dropped int
	if all {
		text = j.out.all()
	} else {
		text, dropped = j.out.take()
	}
	text = d.redact(ctx, NameJob, text)
	var b strings.Builder
	if dropped > 0 {
		fmt.Fprintf(&b, "[%d earlier byte(s) dropped: the job produced more output than is kept]\n", dropped)
	}
	if text == "" {
		b.WriteString("[no new output]")
	} else {
		b.WriteString(strings.TrimRight(text, "\n"))
	}
	fmt.Fprintf(&b, "\n[%s %s]", j.ID, jobState(j.Status()))
	return b.String()
}
