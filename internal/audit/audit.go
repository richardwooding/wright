// Package audit writes a tamper-evident record of everything a session did:
// one JSON event per line, each carrying the SHA-256 of the previous line,
// so a truncated or edited log is detectable with Verify. Text fields are
// redacted before they are written; the log never holds environment
// variables or request bodies.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/richardwooding/wright/internal/redact"
)

// Version is the event schema version written in every line.
const Version = 1

// Kind classifies an event.
type Kind string

// Event kinds.
const (
	KindSessionStart  Kind = "session_start"
	KindRunStart      Kind = "run_start"
	KindRunEnd        Kind = "run_end"
	KindToolCall      Kind = "tool_call"
	KindToolResult    Kind = "tool_result"
	KindDecision      Kind = "decision"
	KindFileChange    Kind = "file_change"
	KindCommand       Kind = "command"
	KindModelCall     Kind = "model_call"
	KindMCP           Kind = "mcp"
	KindRedaction     Kind = "redaction"
	KindInjection     Kind = "injection_signal"
	KindCompaction    Kind = "compaction"
	KindNotice        Kind = "notice"
	KindError         Kind = "error"
	KindHardDenyAbort Kind = "hard_deny_abort"
)

// maxArgs caps the recorded tool arguments so a large file write does not
// bloat the log; the full arguments are always summarised by ArgsSHA256.
const maxArgs = 4096

// Event is one audit line. Optional sections are pointers so they are
// omitted when absent.
type Event struct {
	V       int       `json:"v"`
	TS      time.Time `json:"ts"`
	Seq     int       `json:"seq"`
	Session string    `json:"session"`
	Run     string    `json:"run,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	Depth   int       `json:"depth,omitempty"`
	Kind    Kind      `json:"kind"`

	Tool     *Tool     `json:"tool,omitempty"`
	Decision *Decision `json:"decision,omitempty"`
	Result   *Result   `json:"result,omitempty"`
	Files    []File    `json:"files,omitempty"`
	Command  *Command  `json:"command,omitempty"`
	Model    *Model    `json:"model,omitempty"`
	Text     string    `json:"text,omitempty"`
	MCP      *MCP      `json:"mcp,omitempty"`
	Error    string    `json:"error,omitempty"`
	Prev     string    `json:"prev"`
}

// Tool identifies a tool call. Args is truncated and redacted; ArgsSHA256
// is the digest of the untruncated, unredacted arguments.
type Tool struct {
	Name       string `json:"name"`
	CallID     string `json:"call_id,omitempty"`
	ArgsSHA256 string `json:"args_sha256,omitempty"`
	Args       string `json:"args,omitempty"`
}

// Decision records a policy verdict.
type Decision struct {
	Outcome     string `json:"outcome"` // allow | ask | deny
	Rule        string `json:"rule,omitempty"`
	Source      string `json:"source,omitempty"`
	Class       string `json:"class,omitempty"`
	Mode        string `json:"mode,omitempty"`
	By          string `json:"by,omitempty"` // policy | user | headless
	OffersShown int    `json:"offers_shown,omitempty"`
	Grant       string `json:"grant,omitempty"`
	HardDeny    bool   `json:"hard_deny,omitempty"`
	Reason      string `json:"reason,omitempty"`

	// GrantedNetwork and GrantedWritable are what allowing this one call
	// handed it, as opposed to Grant, which names a rule that was saved.
	// Without them the log says a command was allowed but not that it ran
	// with no network, which is the difference between a record that
	// explains a session and one that does not. Old lines lack both, which
	// reads as "nothing granted" — the right answer for a log written
	// before an approval could grant anything.
	GrantedNetwork  bool     `json:"granted_network,omitempty"`
	GrantedWritable []string `json:"granted_writable,omitempty"`
}

// Result summarises a tool result.
type Result struct {
	IsError    bool  `json:"is_error,omitempty"`
	Bytes      int   `json:"bytes"`
	Truncated  bool  `json:"truncated,omitempty"`
	DurationMS int64 `json:"duration_ms"`
	Redactions int   `json:"redactions,omitempty"`
	Injection  bool  `json:"injection,omitempty"`
}

// File records one file operation.
type File struct {
	Path      string `json:"path"`
	Op        string `json:"op"` // create | edit | delete | write
	SHABefore string `json:"sha_before,omitempty"`
	SHAAfter  string `json:"sha_after,omitempty"`
	Added     int    `json:"added,omitempty"`
	Removed   int    `json:"removed,omitempty"`
}

// Command records one shell execution.
type Command struct {
	Argv       []string `json:"argv"`
	Class      string   `json:"class,omitempty"`
	Exit       int      `json:"exit"`
	Network    bool     `json:"network,omitempty"`
	Sandbox    string   `json:"sandbox,omitempty"`
	DurationMS int64    `json:"duration_ms"`
}

// Model records one model call's usage.
type Model struct {
	Name         string  `json:"name"`
	Provider     string  `json:"provider,omitempty"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CacheRead    int     `json:"cache_read_tokens,omitempty"`
	CacheWrite   int     `json:"cache_write_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// MCP records an MCP server interaction.
type MCP struct {
	Server    string `json:"server"`
	Tool      string `json:"tool,omitempty"`
	Transport string `json:"transport,omitempty"`
	Action    string `json:"action,omitempty"` // connect | call | trust
}

// Log is an append-only audit file for one session.
type Log struct {
	mu      sync.Mutex
	f       *os.File
	w       *bufio.Writer
	r       *redact.Redactor
	seq     int
	prev    string
	path    string
	anchors *Anchors
}

// Open opens (or creates, mode 0600) the log at path for appending. When
// the file already has events, Seq and Prev continue the chain. r may be
// nil to disable redaction (tests only). The chain is not anchored — use
// OpenAnchored for a log whose head must be verifiable from outside.
func Open(path string, r *redact.Redactor) (*Log, error) {
	return OpenAnchored(path, r, nil)
}

// OpenAnchored is Open plus an anchor store: every written line records the
// new head there, so Verify can tell a truncated or re-forged log from the
// one wright wrote. anchors may be nil.
func OpenAnchored(path string, r *redact.Redactor, anchors *Anchors) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	seq, prev, err := tail(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, w: bufio.NewWriter(f), r: r, seq: seq, prev: prev, path: path, anchors: anchors}, nil
}

// tail returns the last sequence number and line hash of an existing log.
func tail(path string) (seq int, prev string, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = f.Close() }() // read-only: nothing to flush
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return 0, "", fmt.Errorf("audit: %s: malformed line after seq %d: %w", path, seq, err)
		}
		seq, prev = ev.Seq, hashLine(line)
	}
	return seq, prev, sc.Err()
}

// Path returns the file the log writes to.
func (l *Log) Path() string { return l.path }

// Write appends ev, filling V, TS (when zero), Seq and Prev, redacting Text
// and Tool.Args and truncating Args to 4 KiB.
func (l *Log) Write(ev Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errors.New("audit: log is closed")
	}
	l.seq++
	ev.V = Version
	ev.Seq = l.seq
	ev.Prev = l.prev
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	l.scrub(&ev)
	line, err := json.Marshal(ev)
	if err != nil {
		l.seq--
		return err
	}
	if _, err := l.w.Write(line); err != nil {
		return err
	}
	if err := l.w.WriteByte('\n'); err != nil {
		return err
	}
	if err := l.w.Flush(); err != nil {
		return err
	}
	l.prev = hashLine(line)
	// The anchor moves with the line it witnesses. A failure here is
	// reported rather than swallowed: an unanchored line is a line nothing
	// outside the file can vouch for.
	return l.anchors.Record(l.path, l.seq, l.prev)
}

// scrub redacts free text and bounds argument size.
func (l *Log) scrub(ev *Event) {
	if l.r != nil {
		ev.Text, _ = l.r.Redact(ev.Text)
		ev.Error, _ = l.r.Redact(ev.Error)
	}
	if ev.Tool == nil {
		return
	}
	if ev.Tool.Args != "" && ev.Tool.ArgsSHA256 == "" {
		ev.Tool.ArgsSHA256 = hashString(ev.Tool.Args)
	}
	if len(ev.Tool.Args) > maxArgs {
		ev.Tool.Args = ev.Tool.Args[:maxArgs] + "…"
	}
	if l.r != nil {
		ev.Tool.Args, _ = l.r.Redact(ev.Tool.Args)
	}
}

// Close flushes and closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.w.Flush()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

func hashLine(line []byte) string {
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:])
}

func hashString(s string) string { return hashLine([]byte(s)) }

// Read streams the events of a log. Malformed lines yield an error and stop
// the sequence; the caller decides whether that is fatal.
func Read(path string) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		f, err := os.Open(path)
		if err != nil {
			yield(Event{}, err)
			return
		}
		defer func() { _ = f.Close() }() // read-only: nothing to flush
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var ev Event
			if err := json.Unmarshal(line, &ev); err != nil {
				yield(Event{}, fmt.Errorf("audit: malformed line: %w", err))
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			yield(Event{}, err)
		}
	}
}

// Verification outcomes. They are distinct because the remedies differ: a
// broken chain means a line was edited, a short log means lines were removed
// wholesale, and a head that does not match the anchor means the chain was
// recomputed by someone who could write the file.
var (
	// ErrTampered is returned when the chain itself does not hold.
	ErrTampered = errors.New("audit: log has been modified or truncated")
	// ErrTruncated is returned when the chain holds but the log is shorter
	// than the head wright recorded.
	ErrTruncated = errors.New("audit: log is shorter than the recorded head")
	// ErrForged is returned when the chain holds and the log has the
	// expected length but its head is not the one wright recorded — a chain
	// recomputed over rewritten lines.
	ErrForged = errors.New("audit: log head does not match the recorded head")
	// ErrNoAnchor is returned when no head was ever recorded for a log, so
	// only self-consistency could be checked.
	ErrNoAnchor = errors.New("audit: no recorded head for this log")
)

// Verify re-hashes every line and checks Seq and Prev. It returns the
// number of valid events; on failure the error names the first bad line.
// It proves only that the log is self-consistent: use VerifyAnchored to
// learn whether it is also the log wright wrote.
func Verify(path string) (int, error) {
	n, _, err := verifyFile(path)
	return n, err
}

// VerifyAnchored verifies the chain and then checks the log against the head
// recorded in anchors, which lives outside the log's directory. A log whose
// tail was cut off, or whose chain was recomputed after an edit, verifies
// cleanly on its own; only the anchor makes either visible.
func VerifyAnchored(path string, anchors *Anchors) (int, error) {
	n, head, err := verifyFile(path)
	if err != nil {
		return n, err
	}
	an, ok := anchors.Head(path)
	if !ok {
		return n, ErrNoAnchor
	}
	switch {
	case n < an.Seq:
		return n, fmt.Errorf("%w: it ends at seq %d, the recorded head is seq %d (%d event(s) removed)",
			ErrTruncated, n, an.Seq, an.Seq-n)
	case n > an.Seq:
		return n, fmt.Errorf("%w: it ends at seq %d, past the recorded head at seq %d (%d line(s) nothing witnessed)",
			ErrForged, n, an.Seq, n-an.Seq)
	case head != an.Hash:
		return n, fmt.Errorf("%w: line %d hashes to %s, the recorded head is %s",
			ErrForged, n, short(head), short(an.Hash))
	}
	return n, nil
}

// verifyFile walks the chain and returns the event count and the hash of the
// last line, which is what the anchor is compared against.
func verifyFile(path string) (int, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = f.Close() }() // read-only: nothing to flush
	return verify(f)
}

func verify(r io.Reader) (int, string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	n, prev := 0, ""
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return n, prev, fmt.Errorf("%w: line %d is not valid JSON", ErrTampered, n+1)
		}
		if ev.Seq != n+1 {
			return n, prev, fmt.Errorf("%w: line %d has seq %d, want %d", ErrTampered, n+1, ev.Seq, n+1)
		}
		if ev.Prev != prev {
			return n, prev, fmt.Errorf("%w: line %d does not chain to line %d", ErrTampered, n+1, n)
		}
		prev = hashLine(line)
		n++
	}
	return n, prev, sc.Err()
}

// short abbreviates a hash for a message.
func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}
