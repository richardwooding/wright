package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/richardwooding/wright/internal/diag"
)

// diagnostics installs the self-inspection: the dump signal always, and the
// loopback HTTP endpoint only when --debug-addr asked for one. It runs last,
// because everything it reports on has to exist first.
//
// A bad address fails the session here rather than in a goroutine: a user who
// asked for a debug endpoint and silently did not get one is worse off than
// one who was told the port was taken.
func (b *builder) diagnostics() error {
	start := time.Now()
	srv, err := diag.Open(diag.Options{
		Addr:    b.o.DebugAddr,
		Source:  func() []diag.Section { return b.sections(start) },
		DumpDir: b.store.DebugDir(b.sessionID),
		Redact:  b.redactText,
		OnDump:  b.onDump,
	})
	if err != nil {
		return err
	}
	b.diag = srv
	if url := srv.URL(); url != "" {
		// Stated at startup, not only on request: a listening debug endpoint
		// exposes this process's stacks and a CPU profiler to anything
		// running as this user, so it is a state the user must not be able
		// to forget about.
		b.warn("debug endpoint is serving on %s — it exposes this session's state and pprof to anything running as you on this machine", url)
	}
	return nil
}

// redactText cleans a report before it is written or served. A dump quotes
// commands and tool arguments, which is precisely where a secret ends up.
func (b *builder) redactText(s string) string {
	if b.redactor == nil {
		return s
	}
	out, _ := b.redactor.Redact(s)
	return out
}

// onDump tells the user where the dump went. The signal usually arrives
// because the UI looks stuck, so this may well not be seen — the file is the
// deliverable, and this is the courtesy.
func (b *builder) onDump(path string, err error) {
	if b.eng == nil {
		return
	}
	if err != nil {
		b.eng.Notice("diagnostics dump failed: " + err.Error())
		return
	}
	b.eng.Notice("diagnostics dump written to " + path)
}

// sections are the report: what this session is, what it is doing, and what
// it is waiting for. Ordered by what a person looks at first when a session
// has gone quiet.
func (b *builder) sections(start time.Time) []diag.Section {
	now := time.Now()
	return []diag.Section{
		b.sessionSection(now, start),
		b.stateSection(),
		b.waitingSection(now),
		b.inFlightSection(now),
		b.jobsSection(),
	}
}

func (b *builder) sessionSection(now, start time.Time) diag.Section {
	lines := []string{
		"version:   " + valueOr(b.o.Version, "dev"),
		"session:   " + b.sessionID,
		"uptime:    " + now.Sub(start).Round(time.Second).String(),
		"workspace: " + b.ws.Root(),
		"sessions:  " + b.store.Dir(),
		"audit:     " + b.store.AuditPath(b.sessionID),
		"dumps:     " + b.store.DebugDir(b.sessionID),
	}
	if exe, err := os.Executable(); err == nil {
		lines = append(lines, "binary:    "+exe)
	}
	return diag.Section{Title: "session", Lines: lines}
}

func (b *builder) stateSection() diag.Section {
	if b.eng == nil {
		return diag.Section{Title: "engine"}
	}
	s := b.eng.Status()
	sandbox := s.Sandbox
	if sandbox == "" {
		sandbox = "none"
	}
	net := "no network"
	if s.SandboxNet {
		net = "network"
	}
	github := "off"
	if b.gitHub.On() {
		// The source's *name*, never the token. "why can this session push"
		// is exactly the kind of question a dump exists to answer.
		github = "on (token from " + b.gitHub.Source() + "), carried by networked calls only"
	}
	return diag.Section{Title: "engine", Lines: []string{
		fmt.Sprintf("model:   %s (%s)", s.Model, s.Provider),
		fmt.Sprintf("mode:    %s%s", s.Mode, boolSuffix(s.Bypass, " · BYPASS")),
		fmt.Sprintf("sandbox: %s, %s", sandbox, net),
		fmt.Sprintf("running: %t · queued messages: %d", s.Running, s.Queued),
		fmt.Sprintf("context: %d of %d tokens", s.ContextUsed, s.ContextWindow),
		fmt.Sprintf("usage:   %d tokens", s.Usage.TotalTokens),
		"github:  " + github,
	}}
}

// waitingSection is the one that explains a session that has simply stopped:
// an approval prompt with nobody to answer it blocks its tool call, which
// blocks the step, which blocks the run.
func (b *builder) waitingSection(now time.Time) diag.Section {
	sec := diag.Section{Title: "approvals waiting for an answer"}
	if b.eng == nil {
		return sec
	}
	for _, w := range b.eng.Waiting() {
		line := fmt.Sprintf("%-22s %-10s waiting %s", w.ID, w.Tool, now.Sub(w.Since).Round(time.Second))
		if w.Title != "" {
			line += "  " + w.Title
		}
		sec.Lines = append(sec.Lines, line)
	}
	return sec
}

func (b *builder) inFlightSection(now time.Time) diag.Section {
	sec := diag.Section{Title: "tool calls that have not returned"}
	if b.eng == nil {
		return sec
	}
	for _, c := range b.eng.InFlight() {
		sec.Lines = append(sec.Lines, fmt.Sprintf("%s%-10s %-12s running %s",
			strings.Repeat("  ", c.Depth), c.Tool, c.CallID, now.Sub(c.Since).Round(time.Second)))
	}
	return sec
}

func (b *builder) jobsSection() diag.Section {
	sec := diag.Section{Title: "background jobs"}
	if b.jobs == nil {
		return sec
	}
	for _, j := range b.jobs.List() {
		state := fmt.Sprintf("exit %d", j.ExitCode)
		if j.Running {
			state = "running"
		}
		if j.Failure != "" {
			state = j.Failure
		}
		sec.Lines = append(sec.Lines, fmt.Sprintf("%-8s %-10s %s  %s", j.ID, state, j.Elapsed.Round(time.Second), j.Command))
	}
	return sec
}

func valueOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func boolSuffix(b bool, suffix string) string {
	if b {
		return suffix
	}
	return ""
}

// debugReport is what /debug shows: the endpoint, the signal, and where the
// dumps go. It is a command rather than a status line because the address is
// only half of it — knowing the pid to signal is the other half.
func (b *Built) debugReport() string {
	// Every line starts with its own label rather than aligning under one:
	// a notice is wrapped to the terminal's width, and a continuation line
	// that begins with spaces reads as a line that lost its label.
	var out strings.Builder
	out.WriteString("Diagnostics\n\n")
	if url := b.diag.URL(); url != "" {
		fmt.Fprintf(&out, "  endpoint   %s\n", url)
		fmt.Fprintf(&out, "  state      %sdebug/state (this session, with goroutine stacks)\n", url)
		fmt.Fprintf(&out, "  profiles   %sdebug/pprof/\n", url)
	} else {
		out.WriteString("  endpoint   off — restart with --debug-addr 127.0.0.1:6060 to serve one\n")
	}
	if diagSignal != "" {
		fmt.Fprintf(&out, "  dump       kill -s %s %d (no endpoint needed), or /debug dump\n", diagSignal, os.Getpid())
	}
	fmt.Fprintf(&out, "  dumps in   %s\n", b.diag.DumpDir())
	if last := b.diag.LastDump(); last != "" {
		fmt.Fprintf(&out, "  last dump  %s\n", last)
	}
	out.WriteString("\nDumps and the endpoint carry the same redaction as tool output, and the endpoint listens on loopback only.")
	return out.String()
}

// dumpNow writes a dump on request, so /debug dump does not need a second
// terminal to send a signal from.
func (b *Built) dumpNow() (string, error) {
	path, err := b.diag.Dump()
	if err != nil {
		return "", err
	}
	return "Dump written to " + path, nil
}

const diagSignal = diag.DumpSignal

// DebugURL is the diagnostics endpoint's address, or "" when none is running.
func (b *Built) DebugURL() string { return b.diag.URL() }
