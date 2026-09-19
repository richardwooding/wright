package sandbox

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
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
	// Probe runs the read-only bind mount check inside ProbeDir instead of a
	// payload, so the backend can measure what it is about to promise.
	Probe    bool
	ProbeDir string
	// ParentNS is the backend's own network and mount namespace identity,
	// set only when the backend asked for fresh ones. The helper refuses to
	// run if it is still in them.
	ParentNS string
}

// Args renders the helper flags (without the leading "__sandbox").
func (h Helper) Args() []string {
	args := []string{}
	if h.Probe {
		return []string{"--probe", "--probe-dir", h.ProbeDir}
	}
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
	if h.ParentNS != "" {
		args = append(args, "--parent-ns", h.ParentNS)
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
	fs.BoolVar(&h.Probe, "probe", false, "")
	fs.StringVar(&h.ProbeDir, "probe-dir", "", "")
	fs.StringVar(&h.ParentNS, "parent-ns", "", "")
	if err := fs.Parse(args); err != nil {
		return Helper{}, err
	}
	h.Argv = fs.Args()
	if len(h.Argv) == 0 && !h.Probe {
		return Helper{}, errors.New("__sandbox: missing payload after --")
	}
	return h, nil
}

// Exec applies the Landlock policy and replaces the current process with the
// payload. It returns only on failure. Nothing is executed when the policy
// could not be applied: an unconfined fallback here would silently defeat
// the sandbox the user was shown as active.
func (h Helper) Exec() error {
	if h.Probe {
		return probeProtection(h.ProbeDir)
	}
	if len(h.Argv) == 0 {
		return errors.New("__sandbox: empty payload")
	}
	// Capabilities are per-thread and execve takes the calling thread's, so
	// the thread that gives up CAP_SYS_ADMIN has to be the thread that execs
	// the payload.
	runtime.LockOSThread()
	if err := h.checkNamespaces(); err != nil {
		return fmt.Errorf("__sandbox: %w", err)
	}
	// The read-only bind mounts come first: they need the capability, and
	// Landlock sets no_new_privs. A mount that cannot be made is never
	// fatal — a volume a container runtime bind-mounted in is locked by the
	// user namespace that owns it and cannot be remounted read-only from a
	// nested one, and refusing to run any command there would cost far more
	// than the protection buys. The backend has already warned that those
	// paths rest on the permission layer alone. A failure to *drop* the
	// capability does stop the payload.
	if err := protect(h.RW); err != nil && !errors.Is(err, ErrNoMountProtection) {
		return fmt.Errorf("__sandbox: %w", err)
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

// checkNamespaces verifies that the namespaces the backend asked for were
// actually created. Nothing between the backend and here is supposed to
// touch the process attributes, but something did once — the bash tool
// replaced them to set a process group — and the result was a sandbox that
// reported full confinement while running in the host's network and mount
// namespaces. Running unconfined while the UI says otherwise is worse than
// not running at all, so this refuses.
func (h Helper) checkNamespaces() error {
	if h.ParentNS == "" {
		return nil // the backend promised nothing
	}
	mine := CurrentNS()
	if mine == "" {
		return nil // cannot tell; nothing to assert
	}
	parent := strings.Split(h.ParentNS, ",")
	got := strings.Split(mine, ",")
	if len(parent) != 2 || len(got) != 2 {
		return nil
	}
	if !h.Net && got[0] == parent[0] {
		return errors.New("still in the parent network namespace: the sandbox's process attributes were overwritten")
	}
	if got[1] == parent[1] {
		return errors.New("still in the parent mount namespace: the sandbox's process attributes were overwritten")
	}
	return nil
}

// String is a compact description for logs.
func (h Helper) String() string {
	return "__sandbox " + strings.Join(h.Args(), " ")
}
