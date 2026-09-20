package sandbox_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/sandbox"
)

// The grant mounts the prefix wright computes; the tool inside must compute
// the same one, so the variables that decide it have to survive.
func TestToolPrefixVarsReachTheSandbox(t *testing.T) {
	environ := []string{
		"HOMEBREW_PREFIX=/opt/brew", "GEM_HOME=/g", "GOBIN=/gb",
		"NPM_CONFIG_PREFIX=/n", "GOFLAGS=-toolexec=/evil", "ANTHROPIC_API_KEY=sk-x",
	}
	got := sandbox.EnvFrom(environ, nil, nil)
	for _, want := range []string{"HOMEBREW_PREFIX=/opt/brew", "GEM_HOME=/g", "GOBIN=/gb", "NPM_CONFIG_PREFIX=/n"} {
		if !slices.Contains(got, want) {
			t.Errorf("%q did not reach the sandbox: %v", want, got)
		}
	}
	for _, banned := range []string{"GOFLAGS", "ANTHROPIC_API_KEY"} {
		for _, kv := range got {
			if strings.HasPrefix(kv, banned+"=") {
				t.Errorf("%s must never reach a sandboxed command", banned)
			}
		}
	}
}
