package cli

import (
	"github.com/richardwooding/wright/internal/sandbox"
)

// SandboxCmd is the hidden `wright __sandbox` helper the landlock backend
// re-execs into. It applies Landlock to itself and then execs the payload, so
// the restrictions are inherited by the shell without any privileged helper.
type SandboxCmd struct {
	Dir  string   `help:"Working directory for the payload."`
	RW   []string `name:"rw" help:"Read-write directory (repeatable)."`
	RO   []string `name:"ro" help:"Read-only directory (repeatable)."`
	Net  bool     `help:"Leave TCP networking unrestricted."`
	Argv []string `arg:"" passthrough:"" help:"Payload command."`
}

// Run applies the sandbox and replaces the process with the payload. It only
// returns when something failed before exec.
func (c *SandboxCmd) Run(_ *Globals) error {
	h := sandbox.Helper{Dir: c.Dir, RW: c.RW, RO: c.RO, Net: c.Net, Argv: stripDashDash(c.Argv)}
	return h.Exec()
}

// stripDashDash drops the leading "--" kong leaves in a passthrough argument.
func stripDashDash(argv []string) []string {
	if len(argv) > 0 && argv[0] == "--" {
		return argv[1:]
	}
	return argv
}
