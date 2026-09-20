package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/audit"
)

// exportResultLimit caps a tool result in the export; the transcript keeps
// the full text, the Markdown is for reading.
const exportResultLimit = 2000

// ErrNoSession reports an id with neither a transcript nor a sidecar. It is
// its own error because exporting one silently produced a header for a
// session that does not exist.
var ErrNoSession = errors.New("session: no such session")

// ExportMarkdown writes the session as a readable transcript: a header that
// names the tool and model, then user and assistant text, tool calls as
// collapsed <details> blocks with the approval decision that let each one
// run, and tool results truncated to 2000 characters.
func (s *Store) ExportMarkdown(ctx context.Context, id string, w io.Writer) error {
	if !ValidID(id) {
		return ErrBadID
	}
	meta, found, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrNoSession, id)
	}
	msgs, err := s.Load(ctx, id)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	decisions := readDecisions(s.AuditPath(id))
	bw := bufio.NewWriter(w)
	modelName := meta.Model
	if modelName == "" {
		modelName = "unknown model"
	}
	fmt.Fprintf(bw, "# Transcript of an AI coding session (wright, model %s)\n\n", modelName)
	if meta.Title != "" {
		fmt.Fprintf(bw, "**%s**\n\n", meta.Title)
	}
	fmt.Fprintf(bw, "Session `%s`", id)
	if !meta.Created.IsZero() {
		fmt.Fprintf(bw, ", started %s", meta.Created.Format("2006-01-02 15:04"))
	}
	fmt.Fprint(bw, "\n\n")
	if decisions.note != "" {
		fmt.Fprintf(bw, "_%s_\n\n", decisions.note)
	}
	if len(msgs) == 0 {
		// A session exists from the moment it starts, and its transcript is
		// written a step at a time, so this is the window before the first
		// step completes. A bare header reads as lost data; say which it is.
		fmt.Fprint(bw, "_This session has no messages yet: its first step has not finished._\n")
	}
	for _, m := range msgs {
		writeMessage(bw, m, &decisions)
	}
	return bw.Flush()
}

func writeMessage(w *bufio.Writer, m core.Message, decisions *decisionLog) {
	switch m.Role {
	case core.RoleUser:
		if text := m.Text(); text != "" {
			fmt.Fprintf(w, "## User\n\n%s\n\n", text)
		}
	case core.RoleAssistant:
		if text := m.Text(); text != "" {
			fmt.Fprintf(w, "## Assistant\n\n%s\n\n", text)
		}
		for _, call := range m.ToolCalls() {
			writeCall(w, call)
			if d, ok := decisions.take(call.ID, call.Name); ok {
				writeDecision(w, d)
			}
		}
	case core.RoleTool:
		for _, res := range m.ToolResults() {
			writeResult(w, res)
		}
	case core.RoleSystem:
		// System prompts are the harness's, not the conversation's.
	}
}

// decisionEntry is one audited approval, kept with the call it belongs to.
type decisionEntry struct {
	callID string
	name   string
	rec    audit.Decision
}

// decisionLog is a session's approval decisions in the order they were made,
// plus a cursor into them. Call IDs are *not* unique within a session —
// llmkit synthesises call_1, call_2 … per response for Ollama and Gemini, so
// every turn repeats them — so the transcript and the log are walked forward
// together and each decision is consumed at most once. A map keyed by call ID
// would hand every turn's call_1 the first turn's decision.
type decisionLog struct {
	items []decisionEntry
	next  int
	note  string // why decisions are missing or incomplete, if they are
}

// readDecisions loads the decisions of one session's audit log. The log is
// evidence about the session, not a part of it: it may be absent (the
// session never had one, or Delete removed it) and Read stops at the first
// malformed line, so a failure leaves a note for the reader and keeps
// whatever was read. An export never fails because of it.
func readDecisions(path string) decisionLog {
	var dl decisionLog
	for ev, err := range audit.Read(path) {
		if err != nil {
			dl.note = decisionNote(err)
			break
		}
		// A sub-agent's decisions (Depth > 0) cover calls made inside a tool
		// call, which never appear in this transcript; rendering them would
		// show approvals for calls the reader cannot find.
		if ev.Kind != audit.KindDecision || ev.Decision == nil || ev.Tool == nil || ev.Depth > 0 {
			continue
		}
		dl.items = append(dl.items, decisionEntry{callID: ev.Tool.CallID, name: ev.Tool.Name, rec: *ev.Decision})
	}
	return dl
}

func decisionNote(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "Approval decisions are not shown: this session has no audit log."
	}
	return "Approval decisions may be incomplete: the audit log could not be read to the end (" + err.Error() + ")."
}

// take consumes the next decision for this call: the first unconsumed entry
// matching both the call ID and the tool name, never a lookup by call ID
// alone, because call IDs repeat across turns.
func (dl *decisionLog) take(callID, name string) (audit.Decision, bool) {
	for i := dl.next; i < len(dl.items); i++ {
		if dl.items[i].callID == callID && dl.items[i].name == name {
			dl.next = i + 1
			return dl.items[i].rec, true
		}
	}
	return audit.Decision{}, false
}

// writeDecision renders the approval between the call and its result. A
// policy allow is one line: an auto-allowed read_file on every call is noise,
// and the only fact worth keeping is that nobody was asked. Every other
// decision — a user's answer, any denial — gets the full block, including
// what allowing handed the call.
func writeDecision(w *bufio.Writer, d audit.Decision) {
	by := d.By
	if by == "" {
		by = "policy"
	}
	if by == "policy" && d.Outcome == "allow" {
		fmt.Fprintf(w, "*Allowed by policy%s.*\n\n", ruleSuffix(d))
		return
	}
	fmt.Fprintf(w, "**Decision: %s** — by %s\n\n", d.Outcome, by)
	for _, fact := range decisionFacts(d) {
		fmt.Fprintf(w, "- %s\n", fact)
	}
	fmt.Fprint(w, "\n")
}

func ruleSuffix(d audit.Decision) string {
	if d.Rule == "" {
		return ""
	}
	s := " — rule `" + d.Rule + "`"
	if d.Source != "" {
		s += " (" + d.Source + ")"
	}
	return s
}

// decisionFacts is the body of a full decision block: the rule that decided
// it, why, and what allowing granted. The grant is stated even when it is
// nothing, because "allowed, no network" is exactly the fact a reader cannot
// otherwise recover from the transcript.
func decisionFacts(d audit.Decision) []string {
	var out []string
	if d.Rule != "" {
		out = append(out, strings.TrimPrefix(ruleSuffix(d), " — "))
	}
	if d.HardDeny {
		out = append(out, "hard deny: refused in every mode")
	}
	if d.Reason != "" {
		out = append(out, "reason: "+d.Reason)
	}
	if rules := d.SavedRules(); len(rules) > 0 {
		label := "saved rule: "
		if len(rules) > 1 {
			label = "saved rules: "
		}
		out = append(out, label+"`"+strings.Join(rules, "`, `")+"`")
	}
	if d.Outcome == "allow" {
		granted := "no network"
		if d.GrantedNetwork {
			granted = "network"
		}
		out = append(out, "granted: "+granted)
		if d.GrantedGitHub {
			out = append(out, "granted: GitHub authentication as the user")
		}
		if len(d.GrantedWritable) > 0 {
			out = append(out, "granted writable: `"+strings.Join(d.GrantedWritable, "`, `")+"`")
		}
	}
	return out
}

func writeCall(w *bufio.Writer, call core.ToolCall) {
	fmt.Fprintf(w, "<details><summary>Tool call: %s</summary>\n\n```json\n%s\n```\n\n</details>\n\n", call.Name, prettyJSON(call.Arguments))
}

func writeResult(w *bufio.Writer, res core.ToolResult) {
	label := "Result"
	if res.IsError {
		label = "Error"
	}
	text := truncate(res.Text(), exportResultLimit)
	fmt.Fprintf(w, "<details><summary>%s: %s</summary>\n\n```\n%s\n```\n\n</details>\n\n", label, res.Name, text)
}

func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if len(raw) == 0 || json.Indent(&buf, raw, "", "  ") != nil {
		return string(raw)
	}
	return buf.String()
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + fmt.Sprintf("\n… [truncated, %d more characters]", len(runes)-n)
}
