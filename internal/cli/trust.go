package cli

import (
	"fmt"

	"github.com/richardwooding/wright/internal/app"
)

// TrustCmd groups the workspace-trust subcommands. Trusting a directory is
// what stops wright asking before every edit inside it, so there has to be a
// way to see what is trusted and to take it back without editing
// ~/.config/wright/trust.json by hand.
type TrustCmd struct {
	List   TrustListCmd   `cmd:"" default:"1" help:"List the directories that have been trusted."`
	Accept TrustAcceptCmd `cmd:"" help:"Trust this directory (the scripted equivalent of answering yes at startup)."`
	Forget TrustForgetCmd `cmd:"" help:"Forget this directory: its edits ask again and its settings need accepting again."`
}

// TrustListCmd lists the trust records.
type TrustListCmd struct{}

// Run prints one line per trusted directory, saying which of the two
// acceptances it holds: the workspace itself and its settings files.
func (c *TrustListCmd) Run(g *Globals) error {
	cwd, err := workingDir(g.CLI.Cwd)
	if err != nil {
		return err
	}
	entries, err := app.ListTrust(cwd)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(g.Stdout, "nothing is trusted yet")
		return nil
	}
	for _, e := range entries {
		fmt.Fprintf(g.Stdout, "%s\n  workspace: %s\n  settings:  %s\n", e.Root,
			acceptedAt(e.Workspace, e.WorkspaceAccepted.Format("2006-01-02")),
			acceptedAt(e.Settings, e.Accepted.Format("2006-01-02")))
	}
	return nil
}

// acceptedAt renders one acceptance: when it happened, or that it has not.
func acceptedAt(ok bool, when string) string {
	if !ok {
		return "not accepted"
	}
	return "accepted " + when
}

// TrustAcceptCmd records this directory as trusted.
type TrustAcceptCmd struct{}

// Run accepts the workspace. It deliberately does not accept the project's
// settings files: those are content somebody has to read, and they keep
// their own prompt.
func (c *TrustAcceptCmd) Run(g *Globals) error {
	cwd, err := workingDir(g.CLI.Cwd)
	if err != nil {
		return err
	}
	root, err := app.AcceptWorkspaceTrust(cwd)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.Stdout, "trusted %s: edits to files inside it no longer ask (shell commands still do)\n", root)
	fmt.Fprintln(g.Stdout, "its .wright/settings.json, if any, still needs accepting on its own")
	return nil
}

// TrustForgetCmd removes this directory's trust record.
type TrustForgetCmd struct{}

// Run revokes both acceptances for the directory.
func (c *TrustForgetCmd) Run(g *Globals) error {
	cwd, err := workingDir(g.CLI.Cwd)
	if err != nil {
		return err
	}
	root, err := app.ForgetTrust(cwd)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.Stdout, "forgot %s: edits ask again, and its settings need accepting again\n", root)
	return nil
}
