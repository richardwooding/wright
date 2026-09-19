package cli

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/richardwooding/wright/internal/app"
	"github.com/richardwooding/wright/internal/session"
)

// SessionsCmd groups the session management subcommands.
type SessionsCmd struct {
	List   SessionsListCmd   `cmd:"" default:"1" help:"List sessions for this workspace."`
	Show   SessionsShowCmd   `cmd:"" help:"Print a session transcript."`
	Export SessionsExportCmd `cmd:"" help:"Export a session as Markdown."`
	Delete SessionsDeleteCmd `cmd:"" help:"Delete a session, its snapshots and audit log."`
	Purge  SessionsPurgeCmd  `cmd:"" help:"Delete sessions for this workspace (all, or older than a duration)."`
}

// openStore opens the workspace's session store for the --cwd flag.
func openStore(g *Globals) (*session.Store, error) {
	store, _, err := app.OpenStore(g.CLI.Cwd)
	return store, err
}

// SessionsListCmd lists sessions.
type SessionsListCmd struct{}

// Run prints one row per session, most recent first.
func (c *SessionsListCmd) Run(g *Globals) error {
	store, err := openStore(g)
	if err != nil {
		return err
	}
	metas, err := store.List(g.Ctx)
	if err != nil {
		return err
	}
	if len(metas) == 0 {
		fmt.Fprintln(g.Stdout, "no sessions for this workspace")
		return nil
	}
	tw := tabwriter.NewWriter(g.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUPDATED\tTURNS\tMODEL\tCOST\tTITLE")
	for _, m := range metas {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", m.ID, m.Updated.Local().Format("2006-01-02 15:04"), m.Turns, m.Model, cost(m.CostUSD), m.Title)
	}
	return tw.Flush()
}

func cost(usd float64) string {
	if usd == 0 {
		return "—"
	}
	return fmt.Sprintf("$%.2f", usd)
}

// SessionsShowCmd prints one session.
type SessionsShowCmd struct {
	ID string `arg:"" help:"Session ID."`
}

// Run prints the session's metadata and its transcript.
func (c *SessionsShowCmd) Run(g *Globals) error {
	store, err := openStore(g)
	if err != nil {
		return err
	}
	m, found, err := store.Get(g.Ctx, c.ID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("session %q not found", c.ID)
	}
	fmt.Fprintf(g.Stdout, "session %s\ncreated %s · updated %s · %d turn(s) · model %s · mode %s · cost %s\n\n",
		m.ID, m.Created.Local().Format(time.RFC3339), m.Updated.Local().Format(time.RFC3339), m.Turns, m.Model, m.Mode, cost(m.CostUSD))
	return store.ExportMarkdown(g.Ctx, c.ID, g.Stdout)
}

// SessionsExportCmd exports a session as Markdown.
type SessionsExportCmd struct {
	ID  string `arg:"" help:"Session ID."`
	Out string `short:"o" help:"Write to this file instead of stdout."`
}

// Run writes the Markdown transcript.
func (c *SessionsExportCmd) Run(g *Globals) error {
	store, err := openStore(g)
	if err != nil {
		return err
	}
	w := g.Stdout
	if c.Out != "" {
		f, err := os.OpenFile(c.Out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	return store.ExportMarkdown(g.Ctx, c.ID, w)
}

// SessionsDeleteCmd deletes one session.
type SessionsDeleteCmd struct {
	ID string `arg:"" help:"Session ID."`
}

// Run deletes the transcript, sidecar, snapshots and audit log.
func (c *SessionsDeleteCmd) Run(g *Globals) error {
	store, err := openStore(g)
	if err != nil {
		return err
	}
	if !session.ValidID(c.ID) {
		return session.ErrBadID
	}
	if err := store.Delete(g.Ctx, c.ID); err != nil {
		return err
	}
	fmt.Fprintln(g.Stdout, "deleted "+c.ID)
	return nil
}

// SessionsPurgeCmd deletes every session for the workspace, or only those
// not updated within --older-than.
type SessionsPurgeCmd struct {
	OlderThan time.Duration `help:"Only delete sessions last updated longer ago than this (e.g. 720h); default all."`
}

// Run purges sessions and reports how many went.
func (c *SessionsPurgeCmd) Run(g *Globals) error {
	if c.OlderThan < 0 {
		return errors.New("--older-than must not be negative")
	}
	store, err := openStore(g)
	if err != nil {
		return err
	}
	n, err := store.Purge(g.Ctx, c.OlderThan)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.Stdout, "deleted %d session(s)\n", n)
	return nil
}
