// Package sandbox runs shell commands inside an OS sandbox. The policy layer
// decides *whether* a command runs; this package limits *what it can reach*
// when it does: the workspace and declared caches read-write, the system
// read-only, $HOME hidden, and no network unless granted.
//
// Backends in detection order: container (the container is the boundary),
// bwrap (bubblewrap, Linux), landlock (Linux LSM via a re-exec helper),
// seatbelt (macOS sandbox-exec) and none (visible warning, policy only).
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Spec describes one sandboxed command.
type Spec struct {
	Argv      []string      // command and arguments, e.g. {"bash", "-c", script} — never -lc, which sources profiles
	Dir       string        // working directory (must be inside a ReadWrite root)
	Env       []string      // full environment, already filtered by Env()
	ReadWrite []string      // directories bound read-write (workspace roots, caches)
	ReadOnly  []string      // extra directories bound read-only
	Network   bool          // share the host network namespace
	Timeout   time.Duration // 0 = caller manages the context
	Stdin     io.Reader
}

// Backend is an OS sandbox implementation.
type Backend interface {
	// Name is the short identifier shown in the status bar ("bwrap").
	Name() string
	// Available reports why the backend cannot be used, or nil.
	Available(ctx context.Context) error
	// Command builds the exec.Cmd that runs spec inside the sandbox. The
	// command is not started; the caller wires stdout/stderr and Start()s it.
	Command(ctx context.Context, spec Spec) (*exec.Cmd, error)
}

// Warning is a user-visible note produced during detection (a wanted backend
// that was unavailable, or that no sandbox is active).
type Warning struct {
	Backend string
	Message string
}

// String renders "backend: message".
func (w Warning) String() string {
	if w.Backend == "" {
		return w.Message
	}
	return w.Backend + ": " + w.Message
}

// Status is one row of the doctor report.
type Status struct {
	Name      string
	Available bool
	Detail    string
}

// Names of the backends, in detection order.
const (
	NameContainer = "container"
	NameBwrap     = "bwrap"
	NameLandlock  = "landlock"
	NameSeatbelt  = "seatbelt"
	NameNone      = "none"
)

// ErrUnsupported is returned by backends that cannot exist on this OS.
var ErrUnsupported = errors.New("not supported on this platform")

// backends lists every backend in detection order (none is always last).
func backends() []Backend {
	return []Backend{containerBackend{}, bwrapBackend{}, landlockBackend{}, seatbeltBackend{}}
}

// byName returns the backend called name, or nil.
func byName(name string) Backend {
	if name == NameNone {
		return noneBackend{}
	}
	for _, b := range backends() {
		if b.Name() == name {
			return b
		}
	}
	return nil
}

// Detect picks the backend to use. want forces a specific backend ("" or
// "auto" means detect); when the wanted backend is unavailable a Warning is
// returned and detection continues in the default order. The none backend
// always succeeds and always carries a warning.
func Detect(ctx context.Context, want string) (Backend, []Warning) {
	var warnings []Warning
	if want != "" && want != "auto" {
		b := byName(want)
		switch {
		case b == nil:
			warnings = append(warnings, Warning{Backend: want, Message: "unknown sandbox backend, detecting instead"})
		case b.Name() == NameNone:
			return b, []Warning{{Backend: NameNone, Message: "OS sandbox disabled by request: shell commands run unconfined (policy only)"}}
		default:
			if err := b.Available(ctx); err == nil {
				return b, backendWarnings(b)
			} else {
				warnings = append(warnings, Warning{Backend: want, Message: "requested but unavailable: " + err.Error()})
			}
		}
	}
	for _, b := range backends() {
		if err := b.Available(ctx); err == nil {
			if b.Name() == NameContainer {
				warnings = append(warnings, Warning{Backend: NameContainer, Message: "running inside a container: the container is the sandbox boundary"})
			}
			return b, append(warnings, backendWarnings(b)...)
		}
	}
	warnings = append(warnings, Warning{Backend: NameNone, Message: "no OS sandbox available: shell commands run unconfined (policy only)"})
	return noneBackend{}, warnings
}

// warner is the optional Backend interface for a backend that cannot fully
// deliver what its name implies on this machine. Detect surfaces what it
// says, because a confinement the UI claims but does not enforce is worse
// than no sandbox at all.
type warner interface{ Warnings() []Warning }

// backendWarnings collects a backend's own limitations, if it reports any.
func backendWarnings(b Backend) []Warning {
	if w, ok := b.(warner); ok {
		return w.Warnings()
	}
	return nil
}

// Probe reports the availability of every backend for `wright doctor`.
func Probe(ctx context.Context) []Status {
	out := make([]Status, 0, len(backends()))
	for _, b := range backends() {
		st := Status{Name: b.Name(), Available: true, Detail: "available"}
		if err := b.Available(ctx); err != nil {
			st.Available, st.Detail = false, err.Error()
		} else if b.Name() == NameBwrap {
			if v, err := BwrapVersion(ctx); err == nil {
				st.Detail = v
			}
		}
		out = append(out, st)
	}
	return out
}

// plainCommand builds an unsandboxed exec.Cmd from spec; shared by none and
// container, which confine nothing themselves.
func plainCommand(ctx context.Context, spec Spec) (*exec.Cmd, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("sandbox: empty argv")
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = spec.Stdin
	return cmd, nil
}

// noneBackend runs commands directly. It exists so the rest of the system has
// a Backend to call; the status bar shows it in red.
type noneBackend struct{}

func (noneBackend) Name() string                    { return NameNone }
func (noneBackend) Available(context.Context) error { return nil }
func (noneBackend) Command(ctx context.Context, s Spec) (*exec.Cmd, error) {
	return plainCommand(ctx, s)
}

// containerBackend is selected when wright itself runs inside a container:
// the OS sandbox is the container boundary, so commands run directly.
type containerBackend struct{}

func (containerBackend) Name() string { return NameContainer }

func (containerBackend) Available(context.Context) error {
	if InContainer() {
		return nil
	}
	return errors.New("not running inside a container")
}

func (containerBackend) Command(ctx context.Context, s Spec) (*exec.Cmd, error) {
	return plainCommand(ctx, s)
}

// InContainer reports whether the process runs inside a container, judged by
// the Docker/Podman marker files or the WRIGHT_CONTAINER=1 override the
// official image sets.
func InContainer() bool {
	if os.Getenv("WRIGHT_CONTAINER") == "1" {
		return true
	}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// missingDir is the error for a Spec directory that does not exist.
func missingDir(kind, dir string) error {
	return fmt.Errorf("sandbox: %s directory %q does not exist", kind, dir)
}
