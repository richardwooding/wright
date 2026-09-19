package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/cli"
)

var build = cli.BuildInfo{Version: "1.2.3", Commit: "abc", Date: "2026-01-01"}

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
		{name: "not implemented", args: []string{"sessions", "list"}, wantCode: cli.ExitError, wantErr: "not implemented yet"},
		{name: "default run not implemented", args: []string{"fix the bug"}, wantCode: cli.ExitError, wantErr: "interactive session"},
		{name: "doctor plain", args: []string{"doctor"}, wantCode: cli.ExitOK, wantStdout: "Credentials (names only)"},
		{name: "config paths", args: []string{"config", "paths"}, wantCode: cli.ExitOK, wantStdout: "project local"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WRIGHT_SANDBOX", "none")
			var stdout, stderr bytes.Buffer
			code, err := cli.Run(context.Background(), tt.args, build, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("code = %d, want %d (stdout=%q stderr=%q err=%v)", code, tt.wantCode, stdout.String(), stderr.String(), err)
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want substring %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), tt.wantStderr)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestHelpHidesSandboxHelper(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if _, err := cli.Run(context.Background(), []string{"--help"}, build, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "__sandbox") {
		t.Error("__sandbox must be hidden from help")
	}
}

func TestDoctorNeverPrintsCredentialValues(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret-value-1234567890")
	t.Setenv("WRIGHT_SANDBOX", "none")
	var stdout, stderr bytes.Buffer
	if _, err := cli.Run(context.Background(), []string{"doctor"}, build, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	if strings.Contains(out, "secret-value") {
		t.Fatal("doctor leaked a credential value")
	}
	if !strings.Contains(out, "ANTHROPIC_API_KEY present") {
		t.Errorf("expected presence line, got:\n%s", out)
	}
}
