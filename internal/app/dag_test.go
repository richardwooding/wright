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
		// tui may name policy's Mode/GrantOffer types (engine.Approval carries
		// them and Controller.SetMode takes a Mode) but never the classifier.
		{"tui", []string{"tools", "sandbox", "policy/shellclass"}},
		// toolview decides how a card looks and highlight colours its text.
		// Both are leaves on purpose: a renderer must never be able to reach
		// the permission engine, and deciding a cosmetic question by asking
		// the classifier would put the security component on the render path.
		{"tui/toolview", []string{"engine", "policy", "tools", "tui/transcript"}},
		{"tui/highlight", []string{"engine", "policy", "tools", "tui/transcript", "tui/toolview"}},
		{"tools", []string{"tui", "engine"}},
		{"policy", []string{"tui", "engine", "tools"}},
		{"policy/shellclass", []string{"tui", "engine", "tools", "policy", "workspace"}},
		// The Phase 3 feature packages are built by the app and know
		// nothing about the UI or the engine: mcpclient and skillsdir
		// produce tools and text, agents produces sub-agent tools. The
		// engine reaches them only through Options (Tools, Extra,
		// Describe), which is what keeps the seam intact.
		{"mcpclient", []string{"tui", "engine", "tools", "app"}},
		{"skillsdir", []string{"tui", "engine", "tools", "app"}},
		{"agents", []string{"tui", "engine", "app"}},
		// diag renders sections it is handed; knowing what an engine or a
		// tool is would make the thing being debugged a dependency of the
		// debugger.
		{"diag", []string{"tui", "engine", "tools", "app", "policy"}},
		// ghauth resolves a credential on the host and knows nothing else.
		// It execs gh; it must never learn about sandboxes, policies or
		// tools, because the one thing it does is the thing with the
		// narrowest blast radius in the tree.
		{"ghauth", []string{"tui", "engine", "tools", "app", "policy", "sandbox", "git"}},
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
	// cli is the only internal package allowed to import app, plus tuiwire
	// (the adapter that hands app the TUI's Interactive hook) and package main.
	for p, imports := range graph {
		if slices.Contains(imports, pkg("app")) && p != pkg("cli") && p != pkg("tuiwire") && p != module+"/cmd/wright" {
			t.Errorf("%s imports internal/app; only internal/cli, internal/tuiwire and cmd/wright may", p)
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
		pkg("model"),     // Ollama loopback probe (/api/tags) — added in Phase 1
		pkg("tools"),     // web_fetch takes the ssrfguard *http.Client and builds its requests
		pkg("app"),       // constructs that client (ssrfguard.New().Client() + CheckRedirect re-validation)
		pkg("mcpclient"), // takes that same client for HTTP MCP transports and adds the configured headers
		pkg("websearch"), // queries the configured search API, through the same guarded client; absent unless the user names a provider
		// diag *listens*; it never dials. The claim this test defends is
		// that wright sends nothing anywhere, and a server bound to a
		// loopback address at the user's explicit request sends nothing —
		// it answers the user's own browser. It is off unless --debug-addr
		// is passed, and diag.CheckAddr refuses any address that is not a
		// loopback IP. If this package ever grows an outbound request, it
		// belongs somewhere else, not on this list.
		pkg("diag"),
	}
	graph := importGraph(t)
	for p, imports := range graph {
		if slices.Contains(imports, "net/http") && !slices.Contains(allow, p) {
			t.Errorf("%s imports net/http but is not on the allowlist", p)
		}
	}
}
