package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/richardwooding/llmkit/core"
)

// exportResultLimit caps a tool result in the export; the transcript keeps
// the full text, the Markdown is for reading.
const exportResultLimit = 2000

// ExportMarkdown writes the session as a readable transcript: a header that
// names the tool and model, then user and assistant text, tool calls as
// collapsed <details> blocks and tool results truncated to 2000 characters.
func (s *Store) ExportMarkdown(ctx context.Context, id string, w io.Writer) error {
	if !ValidID(id) {
		return ErrBadID
	}
	meta, _, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	msgs, err := s.Load(ctx, id)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
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
	for _, m := range msgs {
		writeMessage(bw, m)
	}
	return bw.Flush()
}

func writeMessage(w *bufio.Writer, m core.Message) {
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
		}
	case core.RoleTool:
		for _, res := range m.ToolResults() {
			writeResult(w, res)
		}
	case core.RoleSystem:
		// System prompts are the harness's, not the conversation's.
	}
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
