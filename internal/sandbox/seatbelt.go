package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// seatbeltBackend wraps commands in macOS sandbox-exec with a generated
// profile: deny by default, read the system, hide credential directories in
// $HOME, write the workspace and /private/tmp, network only when granted.
// It is only available on darwin; the profile generator itself is portable
// so its output is testable everywhere.
type seatbeltBackend struct{}

func (seatbeltBackend) Name() string { return NameSeatbelt }

func (seatbeltBackend) Available(context.Context) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupported
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return errors.New("sandbox-exec not found")
	}
	return nil
}

// Command runs `sandbox-exec -p <profile> argv…`.
func (b seatbeltBackend) Command(ctx context.Context, spec Spec) (*exec.Cmd, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("sandbox: empty argv")
	}
	home, _ := os.UserHomeDir()
	profile := SeatbeltProfile(spec, home)
	args := append([]string{"-p", profile}, spec.Argv...)
	cmd := exec.CommandContext(ctx, "sandbox-exec", args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = spec.Stdin
	return cmd, nil
}

// protectedSeatbeltDirs are hidden from reads inside the sandbox.
var protectedSeatbeltDirs = []string{".ssh", ".aws", ".gnupg", ".kube", ".config/gh", ".config/wright", ".docker", ".netrc", ".npmrc", ".pypirc"}

// SeatbeltProfile renders the sandbox-exec profile (Scheme-like SBPL) for spec.
func SeatbeltProfile(spec Spec, home string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("(allow process-exec)\n(allow process-fork)\n(allow signal (target same-sandbox))\n")
	b.WriteString("(allow sysctl-read)\n(allow mach-lookup)\n")
	b.WriteString("(allow file-read*)\n")
	for _, d := range protectedSeatbeltDirs {
		fmt.Fprintf(&b, "(deny file-read* (subpath %s))\n", sbplString(filepath.Join(home, d)))
	}
	b.WriteString("(allow file-write* (subpath \"/private/tmp\") (subpath \"/tmp\") (literal \"/dev/null\") (regex #\"^/dev/tty\"))\n")
	for _, rw := range spec.ReadWrite {
		fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", sbplString(rw))
	}
	// SBPL takes the *last* matching rule, so the protected paths are denied
	// after the read-write allows that contain them.
	for _, p := range ProtectedPaths(spec.ReadWrite) {
		fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", sbplString(p))
	}
	if spec.Network {
		b.WriteString("(allow network*)\n")
	} else {
		b.WriteString("(deny network*)\n")
	}
	return b.String()
}

// sbplString quotes a path for SBPL.
func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
