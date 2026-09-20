package audit_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/redact"
)

const token = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"

func openLog(t *testing.T) (*audit.Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "s1.jsonl")
	l, err := audit.Open(path, redact.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

func collect(t *testing.T, path string) []audit.Event {
	t.Helper()
	var out []audit.Event
	for ev, err := range audit.Read(path) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func TestWriteReadVerify(t *testing.T) {
	l, path := openLog(t)
	if l.Path() != path {
		t.Errorf("Path = %s", l.Path())
	}
	events := []audit.Event{
		{Session: "s1", Kind: audit.KindSessionStart},
		{Session: "s1", Run: "r1", Kind: audit.KindToolCall, Tool: &audit.Tool{Name: "bash", CallID: "c1", Args: `{"command":"echo ` + token + `"}`}},
		{Session: "s1", Run: "r1", Kind: audit.KindDecision, Decision: &audit.Decision{Outcome: "allow", Rule: "bash(echo *)", Source: "user"}},
		{Session: "s1", Run: "r1", Kind: audit.KindToolResult, Text: "output " + token, Result: &audit.Result{Bytes: 42, DurationMS: 3, Redactions: 1}},
	}
	for _, ev := range events {
		if err := l.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), token) {
		t.Fatal("token written to audit log unredacted")
	}

	got := collect(t, path)
	if len(got) != 4 {
		t.Fatalf("read %d events, want 4", len(got))
	}
	for i, ev := range got {
		if ev.Seq != i+1 || ev.V != audit.Version || ev.TS.IsZero() {
			t.Errorf("event %d: seq=%d v=%d ts=%v", i, ev.Seq, ev.V, ev.TS)
		}
		if i == 0 && ev.Prev != "" {
			t.Errorf("first event has prev %q", ev.Prev)
		}
		if i > 0 && len(ev.Prev) != 64 {
			t.Errorf("event %d prev = %q", i, ev.Prev)
		}
	}
	if got[1].Tool.ArgsSHA256 == "" || strings.Contains(got[1].Tool.Args, token) || !strings.Contains(got[1].Tool.Args, "[redacted: github") {
		t.Errorf("tool args not hashed/redacted: %+v", got[1].Tool)
	}
	if !strings.Contains(got[3].Text, "[redacted: github…ghij]") {
		t.Errorf("text not redacted: %q", got[3].Text)
	}
	n, err := audit.Verify(path)
	if err != nil || n != 4 {
		t.Errorf("Verify = %d, %v", n, err)
	}
}

func TestReopenContinuesChain(t *testing.T) {
	l, path := openLog(t)
	if err := l.Write(audit.Event{Session: "s", Kind: audit.KindSessionStart}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Write(audit.Event{Kind: audit.KindNotice}); err == nil {
		t.Error("write after close should fail")
	}
	again, err := audit.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Write(audit.Event{Session: "s", Kind: audit.KindRunStart}); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := audit.Verify(path)
	if err != nil || n != 2 {
		t.Fatalf("Verify after reopen = %d, %v", n, err)
	}
	if got := collect(t, path); got[1].Seq != 2 || got[1].Prev == "" {
		t.Errorf("reopened log did not continue the chain: %+v", got[1])
	}
}

func TestArgsTruncated(t *testing.T) {
	l, path := openLog(t)
	big := strings.Repeat("x", 10000)
	if err := l.Write(audit.Event{Kind: audit.KindToolCall, Tool: &audit.Tool{Name: "write_file", Args: big}}); err != nil {
		t.Fatal(err)
	}
	got := collect(t, path)
	if len(got[0].Tool.Args) > 4100 || !strings.HasSuffix(got[0].Tool.Args, "…") {
		t.Errorf("args not truncated: len=%d", len(got[0].Tool.Args))
	}
	if got[0].Tool.ArgsSHA256 == "" {
		t.Error("ArgsSHA256 missing")
	}
}

func TestTamperDetection(t *testing.T) {
	write := func(t *testing.T) string {
		t.Helper()
		l, path := openLog(t)
		for i := range 3 {
			if err := l.Write(audit.Event{Session: "s", Kind: audit.KindNotice, Text: strings.Repeat("a", i+1)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name   string
		mutate func(lines []string) []string
		wantN  int
	}{
		{"edit first line", func(l []string) []string { l[0] = strings.Replace(l[0], `"a"`, `"b"`, 1); return l }, 1}, // detected at line 2
		{"edit middle text", func(l []string) []string { l[1] = strings.Replace(l[1], `"aa"`, `"ab"`, 1); return l }, 2},
		{"delete middle line", func(l []string) []string { return []string{l[0], l[2]} }, 1},
		{"truncate last line", func(l []string) []string { return []string{l[0], l[1], l[2][:len(l[2])/2]} }, 2},
		{"reorder", func(l []string) []string { return []string{l[1], l[0], l[2]} }, 0},
		{"append forged", func(l []string) []string {
			return append(l, `{"v":1,"ts":"2026-01-01T00:00:00Z","seq":4,"session":"s","kind":"notice","prev":"deadbeef"}`)
		}, 3},
		{"drop trailing line is undetectable by chain but seq holds", func(l []string) []string { return l[:2] }, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := write(t)
			raw, _ := os.ReadFile(path)
			lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			lines = tt.mutate(lines)
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			n, err := audit.Verify(path)
			if strings.HasPrefix(tt.name, "drop trailing") {
				// A plain truncation of whole lines cannot be detected by a
				// backward chain; it is caught by the session's expected
				// run_end event, not by Verify.
				if err != nil || n != tt.wantN {
					t.Errorf("Verify = %d, %v", n, err)
				}
				return
			}
			if !errors.Is(err, audit.ErrTampered) {
				t.Errorf("Verify = %d, %v; want ErrTampered", n, err)
			}
			if n != tt.wantN {
				t.Errorf("valid prefix = %d, want %d", n, tt.wantN)
			}
		})
	}
}

func TestReadErrors(t *testing.T) {
	for _, err := range audit.Read(filepath.Join(t.TempDir(), "missing.jsonl")) {
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	}
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(path, []byte("{\"v\":1,\"seq\":1,\"kind\":\"notice\",\"prev\":\"\"}\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var n int
	var last error
	for _, err := range audit.Read(path) {
		if err != nil {
			last = err
			break
		}
		n++
	}
	if n != 1 || last == nil {
		t.Errorf("Read of malformed log: n=%d err=%v", n, last)
	}
	if _, err := audit.Open(path, nil); err == nil {
		t.Error("Open should refuse to append to a malformed log")
	}
}

func TestSummarize(t *testing.T) {
	l, path := openLog(t)
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	events := []audit.Event{
		{TS: ts, Kind: audit.KindFileChange, Files: []audit.File{{Path: "a.go", Op: "edit", Added: 40, Removed: 5}, {Path: "b.go", Op: "create", Added: 1}}},
		{TS: ts, Kind: audit.KindFileChange, Files: []audit.File{{Path: "a.go", Op: "edit", Added: 0, Removed: 2}, {Path: "c.go", Op: "edit"}}},
		{TS: ts, Kind: audit.KindCommand, Command: &audit.Command{Argv: []string{"go", "test"}, Exit: 0}},
		{TS: ts, Kind: audit.KindCommand, Command: &audit.Command{Argv: []string{"make"}, Exit: 2}},
		{TS: ts, Kind: audit.KindCommand, Command: &audit.Command{Argv: []string{"ls"}}},
		{TS: ts, Kind: audit.KindCommand, Command: &audit.Command{Argv: []string{"pwd"}}},
		{TS: ts, Kind: audit.KindDecision, Decision: &audit.Decision{Outcome: "allow"}},
		{TS: ts, Kind: audit.KindDecision, Decision: &audit.Decision{Outcome: "ask"}},
		{TS: ts, Kind: audit.KindDecision, Decision: &audit.Decision{Outcome: "deny", HardDeny: true}},
		// A prompt the user answered: the outcome is the answer, and the
		// prompt itself still counts as asked.
		{TS: ts, Kind: audit.KindDecision, Decision: &audit.Decision{Outcome: "allow", By: "user"}},
		{TS: ts, Kind: audit.KindDecision, Decision: &audit.Decision{Outcome: "deny", By: "user", Reason: "not today"}},
		{TS: ts, Kind: audit.KindToolResult, Result: &audit.Result{Redactions: 2, Injection: true}},
		{TS: ts, Kind: audit.KindModelCall, Model: &audit.Model{Name: "m", InputTokens: 12000, OutputTokens: 800, CostUSD: 0.30}},
		{TS: ts, Kind: audit.KindModelCall, Model: &audit.Model{Name: "m", InputTokens: 300, OutputTokens: 300, CostUSD: 0.08}},
		{TS: ts, Kind: audit.KindError, Error: "boom"},
	}
	for _, ev := range events {
		if err := l.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
	s, err := audit.Summarize(audit.Read(path))
	if err != nil {
		t.Fatal(err)
	}
	want := audit.Summary{
		Files: 3, Added: 41, Removed: 7, ChangedPaths: []string{"a.go", "b.go", "c.go"},
		Commands: 4, Failed: 1, Allowed: 2, Asked: 3, Denied: 2, HardDenied: 1,
		Redactions: 2, Injections: 1, InputTokens: 12300, OutputTokens: 1100, CostUSD: 0.38, Errors: 1,
	}
	if s.Files != want.Files || s.Added != want.Added || s.Removed != want.Removed || strings.Join(s.ChangedPaths, ",") != strings.Join(want.ChangedPaths, ",") ||
		s.Commands != want.Commands || s.Failed != want.Failed || s.Allowed != want.Allowed || s.Asked != want.Asked || s.Denied != want.Denied || s.HardDenied != want.HardDenied ||
		s.Redactions != want.Redactions || s.Injections != want.Injections || s.InputTokens != want.InputTokens || s.OutputTokens != want.OutputTokens || s.Errors != want.Errors {
		t.Errorf("Summary = %+v\n   want %+v", s, want)
	}
	if s.CostUSD < 0.379 || s.CostUSD > 0.381 {
		t.Errorf("CostUSD = %v", s.CostUSD)
	}
	const wantStr = "Changed 3 files (+41 −7) · Ran 4 commands (1 failed) · Denied 2 (1 hard) · Redacted 2 · Injection signals 1 · 12.3k in / 1.1k out tokens · $0.38 · 1 error"
	if got := s.String(); got != wantStr {
		t.Errorf("String =\n  %s\nwant\n  %s", got, wantStr)
	}
	if (audit.Summary{}).String() != "Nothing changed" {
		t.Error("empty summary string")
	}
	if got := (audit.Summary{Files: 1, Commands: 1}).String(); got != "Changed 1 file (+0 −0) · Ran 1 command" {
		t.Errorf("singular form = %q", got)
	}
}

func TestSummarizeStopsOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(path, []byte("{\"v\":1,\"seq\":1,\"kind\":\"command\",\"command\":{\"argv\":[\"ls\"],\"exit\":0,\"duration_ms\":1},\"prev\":\"\"}\n{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := audit.Summarize(audit.Read(path))
	if err == nil {
		t.Fatal("expected error")
	}
	if s.Commands != 1 {
		t.Errorf("partial summary lost: %+v", s)
	}
}
