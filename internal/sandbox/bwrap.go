package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// probeTimeout bounds the one-off bwrap capability probe.
const probeTimeout = 5 * time.Second

// bwrapProbe caches the result of the first availability check: bwrap may be
// installed yet unable to create namespaces (locked-down kernels, some CI).
var bwrapProbe = struct {
	once sync.Once
	err  error
}{}

// bwrapBackend confines commands with bubblewrap. The whole filesystem is
// bound read-only, /tmp and $HOME are fresh tmpfs, then the workspace roots
// and caches are bound back read-write. Network is unshared unless granted.
type bwrapBackend struct{}

func (bwrapBackend) Name() string { return NameBwrap }

// Available runs `bwrap --ro-bind / / --unshare-all true` once and caches it.
func (bwrapBackend) Available(ctx context.Context) error {
	bwrapProbe.once.Do(func() {
		if _, err := exec.LookPath("bwrap"); err != nil {
			bwrapProbe.err = errors.New("bwrap not found on PATH")
			return
		}
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		out, err := exec.CommandContext(pctx, "bwrap", "--ro-bind", "/", "/", "--unshare-all", "true").CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			bwrapProbe.err = errors.New("bwrap cannot create namespaces: " + msg)
		}
	})
	return bwrapProbe.err
}

// BwrapVersion returns the first line of `bwrap --version`.
func BwrapVersion(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "bwrap", "--version").Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line, nil
}

// Command assembles the bwrap invocation for spec.
func (bwrapBackend) Command(ctx context.Context, spec Spec) (*exec.Cmd, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("sandbox: empty argv")
	}
	home, _ := os.UserHomeDir()
	args, err := BwrapArgs(spec, home)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "bwrap", args...)
	// bwrap sees a clean environment; the payload's environment comes from
	// --clearenv/--setenv so nothing leaks through inheritance.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.Stdin = spec.Stdin
	return cmd, nil
}

// BwrapArgs builds the bwrap argument list for spec; exported so tests can
// inspect the profile without bwrap installed.
func BwrapArgs(spec Spec, home string) ([]string, error) {
	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-all",
	}
	if spec.Network {
		args = append(args, "--share-net")
	}
	args = append(args,
		"--unshare-user-try",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	)
	if home != "" {
		// Hide the real home (keys, tokens, shell history); roots and caches
		// that live under it are re-bound below, in order, on top of the tmpfs.
		args = append(args, "--tmpfs", home)
	}
	for _, rw := range spec.ReadWrite {
		if _, err := os.Stat(rw); err != nil {
			return nil, missingDir("read-write", rw)
		}
		args = append(args, "--bind", rw, rw)
	}
	for _, ro := range spec.ReadOnly {
		if _, err := os.Stat(ro); err != nil {
			return nil, missingDir("read-only", ro)
		}
		args = append(args, "--ro-bind", ro, ro)
	}
	if spec.Network {
		if _, err := os.Stat("/etc/resolv.conf"); err == nil {
			args = append(args, "--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf")
		}
	}
	args = append(args, "--clearenv")
	for _, kv := range spec.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		args = append(args, "--setenv", k, v)
	}
	if spec.Dir != "" {
		args = append(args, "--chdir", spec.Dir)
	}
	args = append(args, "--")
	args = append(args, spec.Argv...)
	return args, nil
}
