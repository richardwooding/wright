//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	for _, rw := range spec.ReadWrite {
		if _, err := os.Stat(rw); err != nil {
			return nil, missingDir("read-write", rw)
		}
	}
	h := Helper{Dir: spec.Dir, RW: spec.ReadWrite, RO: spec.ReadOnly, Net: spec.Network, Argv: spec.Argv}
	cmd := exec.CommandContext(ctx, exe, append([]string{"__sandbox"}, h.Args()...)...)
	cmd.Env = spec.Env
	cmd.Stdin = spec.Stdin
	if !spec.Network {
		// Landlock alone would leave UDP, ICMP and DNS reachable; the empty
		// network namespace is what actually makes "no network" true.
		cmd.SysProcAttr = netnsAttr()
	}
	return cmd, nil
}

// Warnings reports what this backend cannot enforce on this machine, so the
// UI never claims more confinement than is in force.
func (landlockBackend) Warnings() []Warning {
	warns := []Warning{{
		Backend: NameLandlock,
		Message: "Landlock rules are additive, so the workspace root is granted entry by entry to keep .git and .wright read-only: a shell command cannot create new entries directly in the workspace root or in .git (bwrap has no such limit)",
	}}
	if err := netnsAvailable(); err != nil {
		warns = append(warns, Warning{
			Backend: NameLandlock,
			Message: "no unprivileged network namespace here (" + err.Error() + "): with network off Landlock restricts TCP only — UDP, ICMP and DNS stay reachable",
		})
	}
	return warns
}

// LandlockNetwork describes, for `wright doctor`, what "no network" actually
// means for the landlock backend on this machine.
func LandlockNetwork() string {
	if err := netnsAvailable(); err != nil {
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
	// The requested read-write roots are split so that no rule covers a
	// protected subpath (.git/hooks, .git/config, .wright): Landlock unions
	// the rules of every ancestor, so a subtree can never be subtracted from
	// a read-write rule — only never granted. The split roots stay readable.
	splitRO, splitRW, splitRWFiles := splitReadWrite(rw)
	rules := []landlock.Rule{
		landlock.RODirs(append(systemRO, ro...)...).IgnoreIfMissing(),
		landlock.RODirs(splitRO...).IgnoreIfMissing(),
		landlock.RWDirs(append(systemRW, splitRW...)...).IgnoreIfMissing().WithRefer(),
		landlock.RWFiles(append(systemRWFiles, splitRWFiles...)...).IgnoreIfMissing(),
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
