//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// systemRO are the directories every sandboxed command needs to read to run
// at all. They are listed explicitly rather than as RODirs("/") because a
// blanket root rule would also expose /home, /root and /mnt.
var systemRO = []string{"/usr", "/etc", "/lib", "/lib64", "/lib32", "/bin", "/sbin", "/opt", "/proc", "/dev", "/sys/fs/cgroup", "/var/lib", "/run", "/nix", "/home/linuxbrew"}

// systemRW are the scratch directories commands expect to write.
var systemRW = []string{"/tmp", "/var/tmp", "/dev/pts", "/dev/shm", "/proc/self"}

// systemRWFiles are the device nodes commands write to (file rules, since
// Landlock rejects directory access rights on regular files).
var systemRWFiles = []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/random", "/dev/tty"}

// landlockBackend restricts the payload with the Landlock LSM by re-executing
// wright as `wright __sandbox … -- argv`, so the rules are applied in a fresh
// process and inherited by the shell. exe overrides os.Executable (tests point
// it at the test binary, whose TestMain understands __sandbox).
type landlockBackend struct{ exe string }

// LandlockWithExecutable returns the landlock backend re-executing exe
// instead of the running binary. It exists for end-to-end tests.
func LandlockWithExecutable(exe string) Backend { return landlockBackend{exe: exe} }

func (landlockBackend) Name() string { return NameLandlock }

// Available requires a kernel with Landlock enabled (ABI ≥ 1).
func (landlockBackend) Available(context.Context) error {
	abi, err := LandlockABI()
	if err != nil {
		return err
	}
	if abi < 1 {
		return errors.New("landlock ABI 0: LSM not enabled in this kernel")
	}
	return nil
}

// LandlockABI reports the kernel's Landlock ABI version (0 = not available).
func LandlockABI() (int, error) {
	v, err := llsys.LandlockGetABIVersion()
	if err != nil {
		return 0, fmt.Errorf("landlock unavailable: %w", err)
	}
	return v, nil
}

// Command builds the re-exec command line.
func (b landlockBackend) Command(ctx context.Context, spec Spec) (*exec.Cmd, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("sandbox: empty argv")
	}
	exe := b.exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, err
		}
	}
	EnsureProtected(spec.Roots)
	for _, rw := range spec.ReadWrite {
		if _, err := os.Stat(rw); err != nil {
			return nil, missingDir("read-write", rw)
		}
	}
	h := Helper{Dir: spec.Dir, RW: spec.ReadWrite, RO: spec.ReadOnly, Net: spec.Network, Argv: spec.Argv}
	// The namespaces carry what Landlock cannot express: an empty network
	// namespace (Landlock restricts TCP only) and a mount namespace for the
	// read-only bind mounts over .git/hooks and .wright. Recording this
	// process's own namespaces lets the helper refuse if they were not
	// created after all.
	attr := nsAttr(spec.Network)
	if attr != nil {
		h.ParentNS = CurrentNS()
	}
	cmd := exec.CommandContext(ctx, exe, append([]string{"__sandbox"}, h.Args()...)...)
	cmd.Env = spec.Env
	cmd.Stdin = spec.Stdin
	cmd.SysProcAttr = attr
	return cmd, nil
}

// Warnings reports what this backend cannot enforce on this machine at all.
// Whether the read-only bind mounts work depends on the filesystem the
// workspace sits on, so that half is in WarningsFor.
func (landlockBackend) Warnings() []Warning {
	err := nsAvailable()
	if err == nil {
		return nil
	}
	return []Warning{
		{
			Backend: NameLandlock,
			Message: "no unprivileged user namespace here (" + err.Error() + "): .git/hooks, .git/config and .wright are writable inside the sandbox, protected by the permission layer only, and with network off Landlock restricts TCP only — UDP, ICMP and DNS stay reachable",
		},
	}
}

// LandlockNamespaces reports why this machine cannot create the user,
// network and mount namespaces the landlock backend's confinement rests on,
// or nil when it can. It is the measured probe nsAvailable performs, not a
// guess from the environment: where unprivileged user namespaces are
// forbidden (a hardened kernel, a CI container without the capability) the
// backend still applies Landlock's path rules but loses the network
// namespace — "no network" then covers TCP only — and the mount namespace
// that carries the read-only binds over .git/hooks, .git/config and
// .wright. Warnings() says this in prose for the user; this is the same
// answer for a caller that has to branch on it.
func LandlockNamespaces() error { return nsAvailable() }

// WarningsFor adds what can only be known once the workspace is known: a
// volume a container runtime bind-mounted in is locked by the user namespace
// that owns it, so a nested one cannot remount any part of it read-only,
// however well the same call works elsewhere on the machine.
func (b landlockBackend) WarningsFor(spec Spec) []Warning {
	if warns := b.Warnings(); len(warns) > 0 {
		return warns
	}
	root := spec.Dir
	if len(spec.Roots) > 0 {
		root = spec.Roots[0]
	}
	err := b.protection(root)
	if err == nil {
		return nil
	}
	return []Warning{{
		Backend: NameLandlock,
		Message: "this filesystem refuses read-only bind mounts (" + err.Error() + "): .git/hooks, .git/config and .wright are writable inside the sandbox, protected by the permission layer only",
	}}
}

// protection reports whether the helper will really get its read-only bind
// mounts in root, by running `wright __sandbox --probe` there with the same
// attributes the helper gets. The answer is cached per root.
func (b landlockBackend) protection(root string) error {
	cached, ok := protectProbe.Load(root)
	if ok {
		err, _ := cached.(error)
		return err
	}
	err := b.probe(root)
	protectProbe.Store(root, err)
	return err
}

// probe runs one `__sandbox --probe` and turns its output into an error.
func (b landlockBackend) probe(root string) error {
	exe := b.exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	h := Helper{Probe: true, ProbeDir: root}
	cmd := exec.CommandContext(ctx, exe, append([]string{"__sandbox"}, h.Args()...)...)
	cmd.SysProcAttr = nsAttr(true) // the network need not be unshared to test a mount
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return errors.New(msg)
}

// protectProbe caches the read-only bind mount probe per workspace root.
var protectProbe sync.Map

// LandlockNetwork describes, for `wright doctor`, what "no network" actually
// means for the landlock backend on this machine.
func LandlockNetwork() string {
	if err := nsAvailable(); err != nil {
		return "TCP only (no network namespace: " + err.Error() + "); UDP, ICMP and DNS remain reachable"
	}
	return "network namespace + Landlock TCP restriction"
}

// enterLandlock applies the filesystem and (unless net) TCP restrictions to
// the current process using V5 best-effort, so older kernels degrade to what
// they support. It returns the ABI actually available.
func enterLandlock(rw, ro []string, net bool) (int, error) {
	abi, err := LandlockABI()
	if err != nil {
		return 0, err
	}
	// The roots are granted read-write whole, so creating a file or a
	// directory in the workspace root works. The paths that must stay
	// read-only are bind-mounted read-only by protect() before this runs:
	// Landlock rules are additive and cannot subtract a subtree, and
	// withholding write on the root to compensate would cost more than it
	// buys — an agent that cannot create a file in the repository root is
	// not usable.
	rules := []landlock.Rule{
		landlock.RODirs(append(systemRO, ro...)...).IgnoreIfMissing(),
		landlock.RWDirs(append(systemRW, rw...)...).IgnoreIfMissing().WithRefer(),
		landlock.RWFiles(systemRWFiles...).IgnoreIfMissing(),
	}
	cfg := landlock.V5.BestEffort()
	if err := cfg.RestrictPaths(rules...); err != nil {
		return abi, err
	}
	if !net {
		// No rules = nothing permitted: every TCP bind/connect is refused.
		if err := cfg.RestrictNet(); err != nil {
			return abi, err
		}
	}
	return abi, nil
}

// sysExec replaces the process image.
func sysExec(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
