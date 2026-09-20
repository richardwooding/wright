package cli_test

import (
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/cli"
)

// TestTrustAcceptListForget covers the way out of trust as well as the way
// in: nothing called ForgetProject before this command existed, so a
// directory the user trusted once could not be untrusted without editing
// trust.json by hand.
func TestTrustAcceptListForget(t *testing.T) {
	cwd := isolate(t)
	code, stdout, _, err := run(t, "", "--cwd", cwd, "trust", "list")
	if err != nil || code != cli.ExitOK {
		t.Fatalf("trust list: code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout, "nothing is trusted yet") {
		t.Fatalf("empty listing = %q", stdout)
	}

	if code, stdout, _, err = run(t, "", "--cwd", cwd, "trust", "accept"); err != nil || code != cli.ExitOK {
		t.Fatalf("trust accept: code=%d err=%v", code, err)
	}
	// The message has to be exact about what was granted, because the
	// command grants it without a prompt.
	for _, want := range []string{"no longer ask", "shell commands still do", "settings.json"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("trust accept does not say %q: %s", want, stdout)
		}
	}

	if _, stdout, _, err = run(t, "", "--cwd", cwd, "trust", "list"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "workspace: accepted") || !strings.Contains(stdout, "settings:  not accepted") {
		t.Fatalf("trust list = %q", stdout)
	}

	if _, stdout, _, err = run(t, "", "--cwd", cwd, "trust", "forget"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "edits ask again") {
		t.Fatalf("trust forget = %q", stdout)
	}
	if _, stdout, _, _ = run(t, "", "--cwd", cwd, "trust", "list"); !strings.Contains(stdout, "nothing is trusted yet") {
		t.Fatalf("the record survived forget: %q", stdout)
	}
}

// TestConfigPathsNamesTrustFile pins that the file deciding whether this
// directory's edits prompt is listed among the paths, which it was not.
func TestConfigPathsNamesTrustFile(t *testing.T) {
	cwd := isolate(t)
	code, stdout, _, err := run(t, "", "--cwd", cwd, "config", "paths")
	if err != nil || code != cli.ExitOK {
		t.Fatalf("config paths: code=%d err=%v", code, err)
	}
	if !strings.Contains(stdout, "trust.json") {
		t.Fatalf("config paths omits trust.json:\n%s", stdout)
	}
}
