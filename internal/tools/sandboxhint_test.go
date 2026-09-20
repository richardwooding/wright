package tools_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/tools"
)

// confinedBackend runs the command for real but reports an enforcing name,
// so the hints fire as they would under bwrap.
type confinedBackend struct{}

func (confinedBackend) Name() string                    { return "bwrap" }
func (confinedBackend) Available(context.Context) error { return nil }
func (confinedBackend) Command(ctx context.Context, spec sandbox.Spec) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...) //nolint:gosec // the test builds the argv
	cmd.Dir = spec.Dir
	return cmd, nil
}

// A command that fails because wright withheld something must say so. The
// session that prompted this spent ten tool calls rediscovering that the
// sandbox had no network, and still concluded the wrong thing.
func TestBashExplainsASandboxFailure(t *testing.T) {
	f := newFixture(t, func(d *tools.Deps) {
		d.Sandbox = confinedBackend{}
		d.SandboxSpec = sandbox.Spec{Network: false, ReadWrite: []string{f2root(t)}}
	})

	cases := []struct {
		name, command, want string
	}{
		{"no network", "echo 'curl: (7) Failed to connect: Network is unreachable'", "without network access"},
		{"read-only", "echo 'touch: cannot touch /opt/x: Read-only file system'", "read-only mount"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.text(tools.NameBash, jsonArgs(map[string]any{"command": tt.command}))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("the sandbox failure was not explained (want %q):\n%s", tt.want, got)
			}
		})
	}

	t.Run("ordinary output is not annotated", func(t *testing.T) {
		got, err := f.text(tools.NameBash, jsonArgs(map[string]any{"command": "echo hello"}))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "without network access") || strings.Contains(got, "read-only mount") {
			t.Errorf("a successful command collected a sandbox note:\n%s", got)
		}
	})
}

// f2root is the workspace root, named separately so the fixture closure can
// use it before f exists.
func f2root(t *testing.T) string { return t.TempDir() }
