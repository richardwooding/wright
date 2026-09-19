package sandbox

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Helper is the payload of `wright __sandbox`: the landlock backend re-execs
// the wright binary with these flags, the helper applies Landlock to itself
// and execs Argv, which inherits the restrictions.
type Helper struct {
	Dir  string
	RW   []string
	RO   []string
	Net  bool
	Argv []string
}

// Args renders the helper flags (without the leading "__sandbox").
func (h Helper) Args() []string {
	args := []string{}
	if h.Dir != "" {
		args = append(args, "--dir", h.Dir)
	}
	for _, p := range h.RW {
		args = append(args, "--rw", p)
	}
	for _, p := range h.RO {
		args = append(args, "--ro", p)
	}
	if h.Net {
		args = append(args, "--net")
	}
	args = append(args, "--")
	return append(args, h.Argv...)
}

// ParseHelperArgs is the inverse of Args, used by the test binary that stands
// in for wright when exercising the landlock backend end to end.
func ParseHelperArgs(args []string) (Helper, error) {
	var h Helper
	fs := flag.NewFlagSet("__sandbox", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&h.Dir, "dir", "", "")
	fs.Func("rw", "", func(s string) error { h.RW = append(h.RW, s); return nil })
	fs.Func("ro", "", func(s string) error { h.RO = append(h.RO, s); return nil })
	fs.BoolVar(&h.Net, "net", false, "")
	if err := fs.Parse(args); err != nil {
		return Helper{}, err
	}
	h.Argv = fs.Args()
	if len(h.Argv) == 0 {
		return Helper{}, errors.New("__sandbox: missing payload after --")
	}
	return h, nil
}

// Exec applies the Landlock policy and replaces the current process with the
// payload. It returns only on failure. Nothing is executed when the policy
// could not be applied: an unconfined fallback here would silently defeat
// the sandbox the user was shown as active.
func (h Helper) Exec() error {
	if len(h.Argv) == 0 {
		return errors.New("__sandbox: empty payload")
	}
	if _, err := enterLandlock(h.RW, h.RO, h.Net); err != nil {
		return fmt.Errorf("__sandbox: landlock: %w", err)
	}
	if h.Dir != "" {
		if err := os.Chdir(h.Dir); err != nil {
			return err
		}
	}
	path, err := exec.LookPath(h.Argv[0])
	if err != nil {
		return err
	}
	return sysExec(path, h.Argv, os.Environ())
}

// String is a compact description for logs.
func (h Helper) String() string {
	return "__sandbox " + strings.Join(h.Args(), " ")
}
