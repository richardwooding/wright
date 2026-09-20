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
		// The shape a bind-mounted file produces. git renames a temporary
		// file over .git/config, which on a bind mount is EBUSY, not EROFS —
		// and "Device or resource busy" tells the model nothing.
		{
			"a protected file reports EBUSY, not EROFS",
			`echo "error: could not write config file .git/config: Device or resource busy"`,
			"mounts it read-only inside the sandbox",
		},
		{
			"and the note says not to retry",
			`echo "error: could not write config file .git/config: Device or resource busy"`,
			"do not retry it another way",
		},
		{
			// The way out that needs no remote at all.
			"and names the way round it",
			`echo "error: could not write config file .git/config: Device or resource busy"`,
			"needs no remote",
		},
		{
			"hooks too",
			`echo "cannot create .git/hooks/pre-commit: Device or resource busy"`,
			"mounts it read-only inside the sandbox",
		},
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

	// EBUSY on something wright does not protect is an ordinary error and
	// must not be explained away as the sandbox's doing.
	t.Run("a busy device wright does not protect is left alone", func(t *testing.T) {
		got, err := f.text(tools.NameBash, jsonArgs(map[string]any{
			"command": `echo "umount: /mnt/disk: Device or resource busy"`,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "mounts it read-only") {
			t.Errorf("an unrelated EBUSY was blamed on the sandbox:\n%s", got)
		}
	})
}

// f2root is the workspace root, named separately so the fixture closure can
// use it before f exists.
func f2root(t *testing.T) string { return t.TempDir() }
