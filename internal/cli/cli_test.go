package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/cli"
)

var build = cli.BuildInfo{Version: "1.2.3", Commit: "abc", Date: "2026-01-01"}

// isolate keeps every command away from the real home directory and env.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WRIGHT_CONFIG_DIR", filepath.Join(home, "config"))
	t.Setenv("WRIGHT_DATA_DIR", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("WRIGHT_SANDBOX", "none")
	t.Setenv("WRIGHT_PLAIN", "false") // kong rejects "" for a bool env
	for _, k := range []string{"WRIGHT_MODEL", "WRIGHT_MODE"} {
		t.Setenv(k, "")
	}
	return t.TempDir()
}

func run(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string, err error) {
	t.Helper()
	var out, errw bytes.Buffer
	code, err = cli.Run(context.Background(), args, build, cli.IO{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errw}, nil)
	return code, out.String(), errw.String(), err
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
		wantErr    string
	}{
		{name: "version", args: []string{"--version"}, wantCode: cli.ExitOK, wantStdout: "wright 1.2.3 (abc, 2026-01-01)"},
		{name: "short version", args: []string{"-V"}, wantCode: cli.ExitOK, wantStdout: "wright 1.2.3"},
		{name: "help", args: []string{"--help"}, wantCode: cli.ExitOK, wantStdout: "doctor"},
		{name: "unknown flag", args: []string{"--bogus"}, wantCode: cli.ExitUsage, wantStderr: "Run 'wright --help'"},
		{name: "bad enum", args: []string{"--sandbox", "vm", "doctor"}, wantCode: cli.ExitUsage},
		{name: "phase 3 stub", args: []string{"mcp", "list"}, wantCode: cli.ExitFailure, wantErr: "not implemented yet"},
		{name: "skills stub", args: []string{"skills"}, wantCode: cli.ExitFailure, wantErr: "Phase 3"},
		{name: "interactive without a UI", args: []string{"fix the bug"}, wantCode: cli.ExitFailure, wantErr: "interactive UI not wired"},
		{name: "print without a prompt", args: []string{"-p"}, wantCode: cli.ExitUsage, wantErr: "needs a prompt"},
		{name: "doctor plain", args: []string{"doctor"}, wantCode: cli.ExitOK, wantStdout: "Credentials (names only)"},
		{name: "config paths", args: []string{"config", "paths"}, wantCode: cli.ExitOK, wantStdout: "project local"},
		{name: "config show", args: []string{"config", "show"}, wantCode: cli.ExitOK, wantStdout: `"permissions"`},
		{name: "sessions list empty", args: []string{"sessions", "list"}, wantCode: cli.ExitOK, wantStdout: "no sessions"},
		{name: "sessions default subcommand", args: []string{"sessions"}, wantCode: cli.ExitOK, wantStdout: "no sessions"},
		{name: "sessions show missing", args: []string{"sessions", "show", "20260101-000000-0000"}, wantCode: cli.ExitFailure, wantErr: "not found"},
		{name: "sessions delete bad id", args: []string{"sessions", "delete", "../x"}, wantCode: cli.ExitFailure, wantErr: "invalid session id"},
		{name: "sessions purge empty", args: []string{"sessions", "purge"}, wantCode: cli.ExitOK, wantStdout: "deleted 0"},
		{name: "audit without sessions", args: []string{"audit"}, wantCode: cli.ExitFailure, wantErr: "no sessions"},
		{name: "audit verify without sessions", args: []string{"audit", "verify"}, wantCode: cli.ExitFailure, wantErr: "no sessions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := isolate(t)
			args := append([]string{"--cwd", cwd}, tt.args...)
			if tt.args[0] == "--version" || tt.args[0] == "-V" || tt.args[0] == "--help" || tt.args[0] == "--bogus" {
				args = tt.args
			}
			code, stdout, stderr, err := run(t, "", args...)
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d (stdout=%q stderr=%q err=%v)", code, tt.wantCode, stdout, stderr, err)
			}
			if tt.wantStdout != "" && !strings.Contains(stdout, tt.wantStdout) {
				t.Errorf("stdout = %q, want substring %q", stdout, tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want substring %q", stderr, tt.wantStderr)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestInitCreatesProjectFiles(t *testing.T) {
	cwd := isolate(t)
	code, stdout, _, err := run(t, "", "--cwd", cwd, "init")
	if err != nil || code != cli.ExitOK {
		t.Fatalf("init: code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout, "created AGENTS.md") || !strings.Contains(stdout, "created .wright/settings.json") {
		t.Fatalf("stdout = %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(cwd, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	_, stdout, _, _ = run(t, "", "--cwd", cwd, "init")
	if !strings.Contains(stdout, "nothing to do") {
		t.Fatalf("second init: %q", stdout)
	}
}

func TestExitErrorUnwraps(t *testing.T) {
	inner := os.ErrNotExist
	e := &cli.ExitError{Code: cli.ExitApproval, Err: inner}
	if e.Error() != inner.Error() || e.Unwrap() != inner {
		t.Fatalf("ExitError = %v", e)
	}
	if bare := (&cli.ExitError{Code: 3}); bare.Error() != "exit status 3" {
		t.Fatalf("bare ExitError = %q", bare.Error())
	}
}

func TestHelpHidesSandboxHelper(t *testing.T) {
	_, stdout, _, err := run(t, "", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "__sandbox") {
		t.Error("__sandbox must be hidden from help")
	}
}

func TestDoctorNeverPrintsCredentialValues(t *testing.T) {
	isolate(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret-value-1234567890")
	_, stdout, _, err := run(t, "", "doctor")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "secret-value") {
		t.Fatal("doctor leaked a credential value")
	}
	if !strings.Contains(stdout, "ANTHROPIC_API_KEY present") {
		t.Errorf("expected presence line, got:\n%s", stdout)
	}
}
