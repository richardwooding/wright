package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/richardwooding/wright/internal/audit"
	"github.com/richardwooding/wright/internal/session"
)

// AuditCmd inspects or verifies audit logs.
type AuditCmd struct {
	Show   AuditShowCmd   `cmd:"" default:"withargs" help:"Print the audit events of a session (default: the latest)."`
	Verify AuditVerifyCmd `cmd:"" help:"Verify the hash chain of a session's audit log."`
}

// AuditShowCmd prints events.
type AuditShowCmd struct {
	ID   string `arg:"" optional:"" help:"Session ID (default: latest)."`
	Kind string `help:"Only events of this kind (tool_call, decision, command, file_change, …)."`
	JSON bool   `help:"Print raw JSONL events instead of the summary lines."`
}

// Run prints the events, then the summary.
func (c *AuditShowCmd) Run(g *Globals) error {
	path, err := auditPath(g, c.ID)
	if err != nil {
		return err
	}
	var kept []audit.Event
	for ev, err := range audit.Read(path) {
		if err != nil {
			return err
		}
		if c.Kind != "" && string(ev.Kind) != c.Kind {
			continue
		}
		kept = append(kept, ev)
		if c.JSON {
			line, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			fmt.Fprintln(g.Stdout, string(line))
		} else {
			fmt.Fprintln(g.Stdout, eventLine(ev))
		}
	}
	if c.JSON {
		return nil
	}
	s, err := audit.Summarize(audit.Read(path))
	if err != nil {
		return err
	}
	fmt.Fprintf(g.Stdout, "\n%d event(s) · %s\nlog: %s\n", len(kept), s, path)
	return nil
}

// eventLine renders one event compactly: time, kind, then the fields that
// carry meaning for that kind.
func eventLine(ev audit.Event) string {
	parts := []string{ev.TS.Local().Format("15:04:05"), fmt.Sprintf("%-14s", ev.Kind)}
	if ev.Tool != nil {
		parts = append(parts, ev.Tool.Name)
	}
	if d := ev.Decision; d != nil {
		s := d.Outcome
		if d.Rule != "" {
			s += " by " + d.Rule
		}
		if d.HardDeny {
			s += " (hard)"
		}
		parts = append(parts, s)
	}
	if cmd := ev.Command; cmd != nil {
		parts = append(parts, fmt.Sprintf("%s exit=%d", strings.Join(cmd.Argv, " "), cmd.Exit))
	}
	for _, f := range ev.Files {
		parts = append(parts, fmt.Sprintf("%s %s +%d −%d", f.Op, f.Path, f.Added, f.Removed))
	}
	if m := ev.Model; m != nil {
		parts = append(parts, fmt.Sprintf("%s in=%d out=%d", m.Name, m.InputTokens, m.OutputTokens))
	}
	if ev.Text != "" {
		parts = append(parts, ev.Text)
	}
	if ev.Error != "" {
		parts = append(parts, "error: "+ev.Error)
	}
	return strings.Join(parts, "  ")
}

// AuditVerifyCmd checks the chain.
type AuditVerifyCmd struct {
	ID string `arg:"" optional:"" help:"Session ID (default: latest)."`
}

// Run re-hashes the log and reports the result.
func (c *AuditVerifyCmd) Run(g *Globals) error {
	path, err := auditPath(g, c.ID)
	if err != nil {
		return err
	}
	n, err := audit.Verify(path)
	if err != nil {
		return fmt.Errorf("%s: %w (%d valid event(s) before the break)", path, err, n)
	}
	fmt.Fprintf(g.Stdout, "ok: %d event(s), chain intact (%s)\n", n, path)
	return nil
}

// auditPath resolves the log file for id, or for the latest session.
func auditPath(g *Globals, id string) (string, error) {
	store, err := openStore(g)
	if err != nil {
		return "", err
	}
	if id == "" {
		m, found, err := store.Latest(g.Ctx)
		if err != nil {
			return "", err
		}
		if !found {
			return "", errors.New("no sessions for this workspace")
		}
		id = m.ID
	}
	if !session.ValidID(id) {
		return "", session.ErrBadID
	}
	return store.AuditPath(id), nil
}
