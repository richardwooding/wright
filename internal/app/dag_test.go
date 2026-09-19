package app_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/richardwooding/wright"

// importGraph returns module-internal import edges: package → imports.
// Packages that do not exist yet simply have no entry, so the DAG rules can
// be written ahead of the code they guard.
func importGraph(t *testing.T) map[string][]string {
	t.Helper()
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	cmd := exec.Command("go", "list", "-deps", "-f", `{{.ImportPath}} {{join .Imports " "}}`, "./...")
	cmd.Dir = strings.TrimSpace(string(root))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	graph := map[string][]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], module) {
			continue
		}
		graph[fields[0]] = fields[1:]
	}
	return graph
}

// pkg builds the module-qualified import path of an internal package.
func pkg(name string) string { return module + "/internal/" + name }

// TestImportDAG asserts the boundaries from the plan: the TUI never sees
// tools/policy/sandbox (engine is the seam), tools never call back into the
// UI or engine, and policy never imports UI code.
func TestImportDAG(t *testing.T) {
	graph := importGraph(t)
	forbidden := []struct {
		from string
		to   []string
	}{
		{"tui", []string{"tools", "policy", "sandbox", "policy/shellclass"}},
		{"tools", []string{"tui", "engine"}},
		{"policy", []string{"tui", "engine", "tools"}},
		{"policy/shellclass", []string{"tui", "engine", "tools", "policy", "workspace"}},
	}
	for _, rule := range forbidden {
		from := pkg(rule.from)
		for p, imports := range graph {
			// Subpackages of `from` (tui/transcript, …) inherit the rule.
			if p != from && !strings.HasPrefix(p, from+"/") {
				continue
			}
			for _, to := range rule.to {
				if slices.Contains(imports, pkg(to)) {
					t.Errorf("forbidden import edge: %s → %s", p, pkg(to))
				}
			}
		}
	}
	// cli is the only internal package allowed to import app.
	for p, imports := range graph {
		if slices.Contains(imports, pkg("app")) && p != pkg("cli") {
			t.Errorf("%s imports internal/app; only internal/cli may", p)
		}
	}
}

// TestNoUnexpectedNetwork keeps "no telemetry, no phone-home" checkable: the
// only internal packages allowed to import net/http directly are listed here.
// Provider traffic goes through llmkit, MCP through agentkit/mcp, and web_fetch
// through ssrfguard — none of which appear in this list because they are not
// internal packages.
func TestNoUnexpectedNetwork(t *testing.T) {
	allow := []string{
		pkg("model"), // Ollama loopback probe (/api/tags) — added in Phase 1
	}
	graph := importGraph(t)
	for p, imports := range graph {
		if slices.Contains(imports, "net/http") && !slices.Contains(allow, p) {
			t.Errorf("%s imports net/http but is not on the allowlist", p)
		}
	}
}
