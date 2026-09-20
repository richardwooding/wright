package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/session"
	"github.com/richardwooding/wright/internal/workspace"
)

type fixture struct {
	data  string
	ws    *workspace.Workspace
	store *session.Store
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("WRIGHT_CONFIG_DIR", filepath.Join(base, "cfg"))
	ws, err := workspace.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(base, "data")
	st, err := session.Open(data, ws)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{data: data, ws: ws, store: st}
}

func TestOpenLayout(t *testing.T) {
	f := newFixture(t)
	want := filepath.Join(f.data, "projects", f.ws.Hash(), "sessions")
	if f.store.Dir() != want || f.store.MetaDir() != filepath.Join(want, "meta") {
		t.Errorf("Dir = %s, MetaDir = %s, want %s", f.store.Dir(), f.store.MetaDir(), want)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("sessions dir mode = %o, want 700", perm)
	}
	if got := f.store.AuditPath("abc"); got != filepath.Join(f.data, "projects", f.ws.Hash(), "audit", "abc.jsonl") {
		t.Errorf("AuditPath = %s", got)
	}
	if got := f.store.SnapshotDir("abc"); got != filepath.Join(f.data, "projects", f.ws.Hash(), "snapshots", "abc") {
		t.Errorf("SnapshotDir = %s", got)
	}
}

func TestNewID(t *testing.T) {
	now := time.Date(2026, 9, 19, 15, 30, 12, 0, time.UTC)
	re := regexp.MustCompile(`^20260919-153012-[0-9a-f]{4}$`)
	seen := map[string]bool{}
	for range 20 {
		id := session.NewID(now)
		if !re.MatchString(id) {
			t.Fatalf("NewID = %q", id)
		}
		if !session.ValidID(id) {
			t.Fatalf("NewID produced an invalid id %q", id)
		}
		seen[id] = true
	}
	if len(seen) < 2 {
		t.Error("random suffix never varied")
	}
}

func TestValidID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"20260919-153012-a1b2", true},
		{"abc", true},
		{"", false},
		{"..", false},
		{"../x", false},
		{"a/b", false},
		{".hidden", false},
		{"a b", false},
	}
	for _, tt := range tests {
		if got := session.ValidID(tt.id); got != tt.want {
			t.Errorf("ValidID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestListMergesSidecars(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	// A: transcript + sidecar (sidecar wins for title/model/usage).
	if err := f.store.Append(ctx, "a", core.UserText("first line of A\nmore"), core.Assistant(core.Text("hi"))); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Touch(ctx, session.Meta{ID: "a", Title: "Fix retries", Model: "gpt-5", Usage: core.Usage{InputTokens: 10}, CostUSD: 0.5, Turns: 1, Mode: "plan", Created: t0, Updated: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// B: transcript only (title from the first user message).
	if err := f.store.Append(ctx, "b", core.UserText("hello from B")); err != nil {
		t.Fatal(err)
	}
	// C: sidecar only, most recent.
	if err := f.store.Touch(ctx, session.Meta{ID: "c", Title: "C only", Updated: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	all, err := f.store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("List = %d sessions, want 3: %+v", len(all), all)
	}
	if all[0].ID != "c" {
		t.Errorf("most recent first: got %s", all[0].ID)
	}
	byID := map[string]session.Meta{}
	for _, m := range all {
		byID[m.ID] = m
	}
	a := byID["a"]
	if a.Title != "Fix retries" || a.Model != "gpt-5" || a.Usage.InputTokens != 10 || a.CostUSD != 0.5 || a.Turns != 1 || a.Mode != "plan" {
		t.Errorf("sidecar did not win: %+v", a)
	}
	if a.Updated.Before(t0.Add(time.Hour)) {
		t.Errorf("Updated regressed: %v", a.Updated)
	}
	if b := byID["b"]; b.Title != "hello from B" || b.Created.IsZero() {
		t.Errorf("transcript fallback: %+v", b)
	}

	got, found, err := f.store.Get(ctx, "a")
	if err != nil || !found || got.Title != "Fix retries" {
		t.Errorf("Get(a) = %+v, %v, %v", got, found, err)
	}
	if _, found, err := f.store.Get(ctx, "zzz"); err != nil || found {
		t.Errorf("Get(zzz) = found=%v err=%v", found, err)
	}
	if _, _, err := f.store.Get(ctx, "../etc"); !errors.Is(err, session.ErrBadID) {
		t.Errorf("Get(../etc) err = %v", err)
	}
	latest, found, err := f.store.Latest(ctx)
	if err != nil || !found || latest.ID != "c" {
		t.Errorf("Latest = %+v, %v, %v", latest, found, err)
	}
}

func TestListEmpty(t *testing.T) {
	f := newFixture(t)
	all, err := f.store.List(context.Background())
	if err != nil || len(all) != 0 {
		t.Fatalf("List = %v, %v", all, err)
	}
	if _, found, err := f.store.Latest(context.Background()); err != nil || found {
		t.Errorf("Latest on empty = found=%v err=%v", found, err)
	}
}

func TestTouchIsAtomicAndPrivate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.store.Touch(ctx, session.Meta{ID: "s1", Title: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Touch(ctx, session.Meta{ID: "s1", Title: "two"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(f.store.MetaDir(), "s1.meta.json")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("sidecar mode = %o, want 600", perm)
	}
	data, _ := os.ReadFile(p)
	var m session.Meta
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Title != "two" || m.Updated.IsZero() || m.Created.IsZero() {
		t.Errorf("sidecar = %+v", m)
	}
	entries, _ := os.ReadDir(f.store.MetaDir())
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
	if err := f.store.Touch(ctx, session.Meta{ID: "bad/id"}); !errors.Is(err, session.ErrBadID) {
		t.Errorf("Touch(bad/id) err = %v", err)
	}
}

func TestDeleteRemovesEverything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.store.Append(ctx, "d", core.UserText("x")); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Touch(ctx, session.Meta{ID: "d"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f.store.AuditPath("d"), filepath.Join(f.store.SnapshotDir("d"), "blob"), filepath.Join(f.store.DebugDir("d"), "dump.txt")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Delete(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(f.store.Dir(), "d.jsonl"), filepath.Join(f.store.MetaDir(), "d.meta.json"), f.store.AuditPath("d"), f.store.SnapshotDir("d"), f.store.DebugDir("d")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists (err=%v)", p, err)
		}
	}
	if err := f.store.Delete(ctx, "d"); err != nil {
		t.Errorf("second delete should be a no-op, got %v", err)
	}
	if err := f.store.Delete(ctx, "../x"); !errors.Is(err, session.ErrBadID) {
		t.Errorf("Delete(../x) err = %v", err)
	}
	if all, _ := f.store.List(ctx); len(all) != 0 {
		t.Errorf("List after delete = %+v", all)
	}
}

func TestPurge(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	old := time.Now().Add(-72 * time.Hour)
	for _, m := range []session.Meta{
		{ID: "old1", Updated: old},
		{ID: "old2", Updated: old.Add(time.Hour)},
		{ID: "fresh", Updated: time.Now()},
	} {
		if err := f.store.Touch(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	n, err := f.store.Purge(ctx, 24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("Purge = %d, %v", n, err)
	}
	all, _ := f.store.List(ctx)
	if len(all) != 1 || all[0].ID != "fresh" {
		t.Errorf("after purge: %+v", all)
	}
}

func TestExportMarkdown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	long := strings.Repeat("line\n", 1000)
	msgs := []core.Message{
		core.UserText("Fix the flaky test"),
		core.Assistant(core.Text("Looking."), core.ToolCall{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"go test ./..."}`)}),
		core.ToolResults(core.ToolResultText("c1", "bash", long)),
		core.Assistant(core.ToolCall{ID: "c2", Name: "edit_file", Arguments: json.RawMessage(`{"path":"x.go"}`)}),
		{Role: core.RoleTool, Parts: []core.Part{core.ToolResult{CallID: "c2", Name: "edit_file", Content: []core.Part{core.Text("boom")}, IsError: true}}},
		core.Assistant(core.Text("Done.")),
	}
	if err := f.store.Append(ctx, "e", msgs...); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Touch(ctx, session.Meta{ID: "e", Title: "Flaky test", Model: "claude-sonnet-4-5"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := f.store.ExportMarkdown(ctx, "e", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	want := []string{
		"# Transcript of an AI coding session (wright, model claude-sonnet-4-5)",
		"**Flaky test**",
		"## User\n\nFix the flaky test",
		"## Assistant\n\nLooking.",
		"<details><summary>Tool call: bash</summary>",
		`"command": "go test ./..."`,
		"<details><summary>Result: bash</summary>",
		"truncated, 3000 more characters",
		"<details><summary>Tool call: edit_file</summary>",
		"<details><summary>Error: edit_file</summary>",
		"## Assistant\n\nDone.",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("export missing %q", w)
		}
	}
	if strings.Count(out, "line\n") > 500 {
		t.Error("tool result was not truncated")
	}
	if err := f.store.ExportMarkdown(ctx, "../x", &buf); !errors.Is(err, session.ErrBadID) {
		t.Errorf("ExportMarkdown(../x) err = %v", err)
	}
	buf.Reset()
	// Exporting a session that does not exist used to emit a header for it,
	// claiming "unknown model", because Get's found result was discarded.
	if err := f.store.ExportMarkdown(ctx, "nope", &buf); !errors.Is(err, session.ErrNoSession) {
		t.Errorf("unknown session export = %v, %q", err, buf.String())
	}
}

// writeAuditLog writes decision events to the session's own audit log, the
// file ExportMarkdown joins the transcript against.
func writeAuditLog(t *testing.T, f fixture, id string, events ...audit.Event) {
	t.Helper()
	l, err := audit.Open(f.store.AuditPath(id), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, ev := range events {
		if err := l.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
}

func decisionEvent(callID, name string, d audit.Decision) audit.Event {
	return audit.Event{Kind: audit.KindDecision, Tool: &audit.Tool{Name: name, CallID: callID}, Decision: &d}
}

func exported(t *testing.T, f fixture, id string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := f.store.ExportMarkdown(context.Background(), id, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestExportMarkdownDecisions is the point of the join: a transcript that
// shows a call and its result but not the approval cannot explain the
// session — a reader cannot see that a command was allowed *without* the
// network.
func TestExportMarkdownDecisions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	msgs := []core.Message{
		core.UserText("install it"),
		core.Assistant(core.ToolCall{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"brew install fpc"}`)}),
		core.ToolResults(core.ToolResultText("c1", "bash", "installed")),
		core.Assistant(core.ToolCall{ID: "c2", Name: "web_fetch", Arguments: json.RawMessage(`{"url":"https://x.test"}`)}),
		core.Assistant(core.ToolCall{ID: "c3", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)}),
		core.ToolResults(core.ToolResultText("c3", "read_file", "package main")),
	}
	if err := f.store.Append(ctx, "d", msgs...); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Touch(ctx, session.Meta{ID: "d", Model: "fake"}); err != nil {
		t.Fatal(err)
	}
	writeAuditLog(t, f, "d",
		decisionEvent("c1", "bash", audit.Decision{
			Outcome: "allow", By: "user", Class: "Install",
			GrantedNetwork: true, GrantedWritable: []string{"/opt/homebrew", "/usr/local"},
		}),
		decisionEvent("c2", "web_fetch", audit.Decision{Outcome: "deny", By: "user", Reason: "not this time"}),
		// A sub-agent's decision covers a call made inside a tool call, which
		// is not in this transcript — and it carries a synthesised call ID
		// that collides with the main agent's, so leaving it in the stream
		// would hand the next real call the sub-agent's answer.
		func() audit.Event {
			ev := decisionEvent("c3", "read_file", audit.Decision{Outcome: "allow", By: "user", Reason: "sub-agent write"})
			ev.Depth = 1
			return ev
		}(),
		decisionEvent("c3", "read_file", audit.Decision{Outcome: "allow", By: "policy", Rule: "read_file(**)", Source: "builtin"}),
	)
	out := exported(t, f, "d")
	want := []string{
		"**Decision: allow** — by user",
		"granted: network",
		"granted writable: `/opt/homebrew`, `/usr/local`",
		"**Decision: deny** — by user",
		"reason: not this time",
		"*Allowed by policy — rule `read_file(**)` (builtin).*",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("export missing %q\n%s", w, out)
		}
	}
	if strings.Contains(out, "sub-agent write") {
		t.Error("a sub-agent decision (Depth > 0) has no call in this transcript and must not be rendered")
	}
	if n := strings.Count(out, "**Decision:"); n != 2 {
		t.Errorf("full decision blocks = %d, want 2 (a policy allow is one line)", n)
	}
	if strings.Contains(out, "Result: web_fetch") {
		t.Error("the denied call has no result")
	}
	// The decision belongs between the call and its result.
	call, dec, res := strings.Index(out, "Tool call: bash"), strings.Index(out, "**Decision: allow**"), strings.Index(out, "Result: bash")
	if call >= dec || dec >= res {
		t.Errorf("decision is not between the call and its result: call=%d decision=%d result=%d", call, dec, res)
	}
}

// TestExportMarkdownRepeatedCallIDs pins the join rule. Call IDs are not
// session-unique: llmkit synthesises call_1, call_2 … per response for Ollama
// and Gemini, so every turn repeats them. A map keyed by call ID would give
// the second turn the first turn's decision.
func TestExportMarkdownRepeatedCallIDs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	msgs := []core.Message{
		core.UserText("twice"),
		core.Assistant(core.ToolCall{ID: "call_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}),
		core.ToolResults(core.ToolResultText("call_1", "bash", "a.go")),
		core.UserText("again"),
		core.Assistant(core.ToolCall{ID: "call_1", Name: "bash", Arguments: json.RawMessage(`{"command":"rm -rf x"}`)}),
		core.ToolResults(core.ToolResultText("call_1", "bash", "denied")),
	}
	if err := f.store.Append(ctx, "r", msgs...); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Touch(ctx, session.Meta{ID: "r", Model: "fake"}); err != nil {
		t.Fatal(err)
	}
	writeAuditLog(t, f, "r",
		decisionEvent("call_1", "bash", audit.Decision{Outcome: "allow", By: "user", Reason: "first turn"}),
		decisionEvent("call_1", "bash", audit.Decision{Outcome: "deny", By: "user", Reason: "second turn"}),
	)
	out := exported(t, f, "r")
	first, second := strings.Index(out, "first turn"), strings.Index(out, "second turn")
	if first < 0 || second < 0 {
		t.Fatalf("each turn's own decision must appear: first=%d second=%d\n%s", first, second, out)
	}
	if first > second {
		t.Errorf("decisions are out of order: first=%d second=%d", first, second)
	}
	if strings.Count(out, "**Decision:") != 2 {
		t.Errorf("want one decision per call\n%s", out)
	}
}

// TestExportMarkdownWithoutDecisions covers the degraded cases: the audit log
// is evidence about a session, not part of it, so an export never fails
// because of it.
func TestExportMarkdownWithoutDecisions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	msgs := []core.Message{
		core.UserText("hello"),
		core.Assistant(core.ToolCall{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}),
		core.ToolResults(core.ToolResultText("c1", "bash", "a.go")),
	}
	if err := f.store.Append(ctx, "n", msgs...); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Touch(ctx, session.Meta{ID: "n", Model: "fake"}); err != nil {
		t.Fatal(err)
	}
	out := exported(t, f, "n")
	if !strings.Contains(out, "no audit log") || !strings.Contains(out, "Tool call: bash") {
		t.Errorf("a session with no audit log must export with a note\n%s", out)
	}
	if strings.Contains(out, "**Decision:") {
		t.Error("no decisions to render")
	}
	// A log that stops being readable keeps what was read and says so.
	writeAuditLog(t, f, "n", decisionEvent("c1", "bash", audit.Decision{Outcome: "allow", By: "user", Reason: "fine"}))
	log, err := os.OpenFile(f.store.AuditPath("n"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	out = exported(t, f, "n")
	if !strings.Contains(out, "could not be read to the end") || !strings.Contains(out, "reason: fine") {
		t.Errorf("a malformed log must degrade, not fail\n%s", out)
	}
}

func TestStoreSatisfiesAgentkitStore(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.store.Append(ctx, "s", core.UserText("a")); err != nil {
		t.Fatal(err)
	}
	msgs, err := f.store.Load(ctx, "s")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Load = %v, %v", msgs, err)
	}
}

// TestExportMidRunSaysSoRatherThanLookingEmpty pins what an export of a
// session whose first run is still going produces. The session exists from
// the moment it starts, but the transcript is written when a run finishes,
// so the export has a header and nothing else — which reads as lost data
// unless it says otherwise.
func TestExportMidRunSaysSoRatherThanLookingEmpty(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.Open(filepath.Join(dir, "data"), ws)
	if err != nil {
		t.Fatal(err)
	}
	const id = "20260920-101010-aaaa"
	if err := store.Touch(context.Background(), session.Meta{ID: id, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := store.ExportMarkdown(context.Background(), id, &buf); err != nil {
		t.Fatalf("export of a started session must not fail: %v", err)
	}
	if !strings.Contains(buf.String(), "no messages yet") {
		t.Errorf("export = %q, want it to say the first step has not finished", buf.String())
	}
}
