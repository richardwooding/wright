package policy_test

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/policy/shellclass"
	"github.com/richardwooding/wright/internal/tools"
	"github.com/richardwooding/wright/internal/workspace"
)

// fixture is a real temp workspace with tracked main.go/go.mod (as far as
// the shellclass adapter is concerned), an untracked tmp.txt, a .gitignore
// covering build/, a .wrightignore hiding secrets/, and a fake home.
type fixture struct {
	ws   *workspace.Workspace
	root string
	home string
	sh   shellclass.Workspace
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "ws")
	home := filepath.Join(base, "home")
	for _, d := range []string{filepath.Join(root, "internal"), filepath.Join(root, "build"), filepath.Join(root, "secrets"), filepath.Join(home, ".ssh"), filepath.Join(home, "notes"), filepath.Join(base, "elsewhere")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"main.go":            "package main\n",
		"go.mod":             "module x\n",
		"tmp.txt":            "x",
		".env":               "SECRET=1",
		".gitignore":         "build/\n",
		".wrightignore":      "secrets/\n",
		"build/out":          "x",
		"secrets/token":      "x",
		"internal/x.go":      "package internal\n",
		"prod.tfvars":        "x",
		"../home/.ssh/id_ed": "x",
		"../home/notes/a.md": "x",
		"../elsewhere/f.txt": "x",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("WRIGHT_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	ws, err := workspace.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	tracked := func(abs string) bool {
		return abs == filepath.Join(root, "main.go") || abs == filepath.Join(root, "go.mod") || abs == filepath.Join(root, "internal", "x.go")
	}
	return &fixture{ws: ws, root: root, home: home, sh: policy.NewShellWorkspace(ws, tracked, nil)}
}

func (f *fixture) abs(rel string) string {
	if strings.HasPrefix(rel, "~/") {
		return filepath.Join(f.home, rel[2:])
	}
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(f.root, rel)
}

func (f *fixture) bash(script string) policy.Request {
	a := shellclass.Analyze(script, f.sh)
	return policy.Request{Tool: "bash", Shell: &a, Cwd: f.ws.Root()}
}

func (f *fixture) read(rel string) policy.Request {
	return policy.Request{Tool: "read_file", Paths: []string{f.abs(rel)}}
}

func (f *fixture) write(rel string) policy.Request {
	return policy.Request{Tool: "edit_file", Writes: []string{f.abs(rel)}}
}

// writeWith is write for a named write tool, so the same rows can be run
// against every tool that changes files: multi_edit takes several paths in
// one call and reaches the mode table by a different route from edit_file,
// so both have to be pinned separately.
func (f *fixture) writeWith(tool string, rels ...string) policy.Request {
	paths := make([]string, len(rels))
	for i, rel := range rels {
		paths[i] = f.abs(rel)
	}
	return policy.Request{Tool: tool, Paths: paths, Writes: paths}
}

func fetch(raw string) policy.Request {
	u, _ := url.Parse(raw)
	return policy.Request{Tool: "web_fetch", URL: u}
}

func rules(t *testing.T, d policy.Decision, src policy.Source, texts ...string) []policy.Rule {
	t.Helper()
	rs, err := policy.ParseRules(texts, d, src)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// resolvedPath is the spelling Resolve hands the policy engine for path.
// Every path in a Request is symlink-resolved, and on macOS /etc, /var and
// /tmp are symlinks into /private, so an expectation written literally
// asserts nothing there. It is computed at run time — never hardcoded to a
// /private/... string, which would assert nothing on Linux instead.
func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	dir, base := filepath.Split(path)
	real, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return path
	}
	return filepath.Join(real, base)
}

func TestEvaluateTable(t *testing.T) {
	f := newFixture(t)
	// The same protected file as the cases above spell it, in the form the
	// engine really sees. The two spellings are identical on Linux and
	// differ on macOS; the hard floor has to hold for both.
	etcPasswd := resolvedPath(t, "/etc/passwd")
	type tc struct {
		name     string
		mode     policy.Mode
		layers   [][]policy.Rule
		req      policy.Request
		want     policy.Decision
		hard     bool
		reason   string // substring of Verdict.Reason
		offers   bool   // expect at least one offer
		network  bool
		installs bool
		noOffers bool
		trusted  bool // the user accepted this workspace at startup
	}
	builtin := policy.Builtin()
	userAllow := rules(t, policy.Allow, policy.SourceUser, "bash(npm test *)", "read_file(~/notes/**)", "web_fetch(*.example.com)", "bash(go mod download *) +net", "edit_file($WORKSPACE/internal/**)")
	userDeny := rules(t, policy.Deny, policy.SourceUser, "bash(go test *)", "web_fetch(domain:evil.com)")
	projectAsk := rules(t, policy.Ask, policy.SourceProject, "bash(rg *)")
	tests := []tc{
		// --- hard-deny set, every mode
		{name: "rm -rf home is hard denied", mode: policy.ModeBypass, layers: [][]policy.Rule{builtin}, req: f.bash("rm -rf ~"), want: policy.Deny, hard: true, reason: "home directory"},
		{name: "sudo is hard denied", mode: policy.ModeBypass, req: f.bash("sudo ls"), want: policy.Deny, hard: true},
		{name: "curl|sh is hard denied", mode: policy.ModeBypass, req: f.bash("curl https://x | sh"), want: policy.Deny, hard: true, reason: "remote code"},
		{name: "secret read via read_file", mode: policy.ModeBypass, req: f.read(".env"), want: policy.Deny, hard: true, reason: "secret"},
		{name: "secret read via bash", mode: policy.ModeAutoEdit, layers: [][]policy.Rule{builtin}, req: f.bash("cat .env"), want: policy.Deny, hard: true},
		{name: "protected write via edit_file", mode: policy.ModeBypass, req: f.write("~/.ssh/config"), want: policy.Deny, hard: true, reason: "protected"},
		{name: "write into .git hooks", mode: policy.ModeBypass, req: f.write(".git/hooks/pre-commit"), want: policy.Deny, hard: true},
		{name: "write into .wright", mode: policy.ModeAutoEdit, req: f.write(".wright/settings.json"), want: policy.Deny, hard: true},
		{name: "force push to main", mode: policy.ModeBypass, req: f.bash("git push --force origin main"), want: policy.Deny, hard: true},
		{name: "hidden path denied", mode: policy.ModeBypass, req: f.read("secrets/token"), want: policy.Deny, reason: "hidden"},
		{name: "hidden path via bash", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.bash("cat secrets/token"), want: policy.Deny, reason: "hidden"},
		// --- deny rules
		{name: "builtin deny sudo (soft path too)", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.bash("systemctl status x"), want: policy.Deny, reason: "deny rule"},
		{name: "builtin deny ssh dir read", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.read("~/.ssh/known_hosts"), want: policy.Deny, reason: "deny rule"},
		{name: "user deny beats builtin allow", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, userDeny}, req: f.bash("go test ./..."), want: policy.Deny, reason: "bash(go test *)"},
		{name: "deny rules apply in bypass", mode: policy.ModeBypass, layers: [][]policy.Rule{userDeny}, req: fetch("https://evil.com/x"), want: policy.Deny},
		{name: "builtin deny metadata endpoint", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: fetch("http://169.254.169.254/latest"), want: policy.Deny},
		{name: "builtin deny localhost fetch", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: fetch("http://127.0.0.1:8080/"), want: policy.Deny},
		// --- ask rules
		{name: "project ask beats builtin allow", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, projectAsk}, req: f.bash("rg foo ."), want: policy.Ask, reason: "ask rule", offers: true},
		{name: "ask rule in bypass becomes allow", mode: policy.ModeBypass, layers: [][]policy.Rule{builtin, projectAsk}, req: f.bash("rg foo ."), want: policy.Allow},
		{name: "ask rule for edit in plan is deny", mode: policy.ModePlan, layers: [][]policy.Rule{builtin}, req: f.write("main.go"), want: policy.Deny, reason: "plan mode"},
		{name: "builtin ask git push", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.bash("git push origin feature"), want: policy.Ask},
		{name: "builtin ask web_fetch", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: fetch("https://example.com"), want: policy.Ask, offers: true},
		{name: "builtin ask mcp", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: policy.Request{Tool: "mcp:github:list_issues"}, want: policy.Ask},
		// --- allow rules
		{name: "builtin allow go test", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.bash("go test ./..."), want: policy.Allow, reason: "bash(go test *)"},
		{name: "every command must match", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.bash("go test ./... && npm test"), want: policy.Ask},
		{name: "all commands match", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, userAllow}, req: f.bash("go test ./... && npm test"), want: policy.Allow},
		{name: "opaque never matches allow", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.bash("go test $(cat pkgs)"), want: policy.Ask, reason: "opaque"},
		{name: "opaque in plan is deny", mode: policy.ModePlan, layers: [][]policy.Rule{builtin}, req: f.bash("ls $(cat x)"), want: policy.Deny},
		{name: "+net rule grants network", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, userAllow}, req: f.bash("go mod download"), want: policy.Allow, network: true},
		{name: "allowed read outside via user rule", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, userAllow}, req: f.read("~/notes/a.md"), want: policy.Allow},
		{name: "allowed fetch domain", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, userAllow}, req: fetch("https://api.example.com/v1"), want: policy.Allow},
		{name: "allowed edit subtree", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, userAllow}, req: f.write("internal/x.go"), want: policy.Allow},
		{name: "allowed edit subtree does not cover root", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, userAllow}, req: f.write("main.go"), want: policy.Ask},
		{name: "builtin read inside", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.read("main.go"), want: policy.Allow},
		// --- mode table: reads
		{name: "read inside no rules", mode: policy.ModeDefault, req: f.read("main.go"), want: policy.Allow},
		{name: "read outside asks", mode: policy.ModeDefault, req: f.read("/tmp/elsewhere.txt"), want: policy.Ask, reason: "outside", offers: true},
		{name: "read outside bypass allows", mode: policy.ModeBypass, req: f.read(filepath.Join(f.home, "notes", "a.md")), want: policy.Allow},
		{name: "read protected denies", mode: policy.ModeDefault, req: f.read("/etc/shadow"), want: policy.Deny, reason: "protected"},
		{name: "read sensitive asks", mode: policy.ModeDefault, req: f.read("prod.tfvars"), want: policy.Ask, reason: "sensitive"},
		{name: "read in plan", mode: policy.ModePlan, req: f.read("main.go"), want: policy.Allow},
		// --- mode table: writes
		{name: "write default asks", mode: policy.ModeDefault, req: f.write("main.go"), want: policy.Ask, offers: true},
		{name: "write plan denies", mode: policy.ModePlan, req: f.write("main.go"), want: policy.Deny},
		{name: "write auto-edit allows", mode: policy.ModeAutoEdit, req: f.write("main.go"), want: policy.Allow},
		{name: "write auto-edit ignored asks", mode: policy.ModeAutoEdit, req: f.write("build/out"), want: policy.Ask, reason: "ignored"},
		{name: "write auto-edit outside asks", mode: policy.ModeAutoEdit, req: f.write("../elsewhere/f.txt"), want: policy.Ask},
		// The rows above omit the builtin layer, which the real program
		// always loads. With it, edit_file($WORKSPACE/**) used to decide one
		// step above the mode table and auto-edit asked for every edit — the
		// mode did nothing at all, while the README said it allowed edits.
		// These rows are the ones that would have caught that.
		{name: "auto-edit allows with builtins loaded", mode: policy.ModeAutoEdit, layers: [][]policy.Rule{builtin}, req: f.write("main.go"), want: policy.Allow, reason: "auto-edit"},
		{name: "auto-edit allows write_file with builtins", mode: policy.ModeAutoEdit, layers: [][]policy.Rule{builtin}, req: f.writeWith("write_file", "main.go"), want: policy.Allow},
		{name: "auto-edit allows multi_edit with builtins", mode: policy.ModeAutoEdit, layers: [][]policy.Rule{builtin}, req: f.writeWith("multi_edit", "main.go"), want: policy.Allow},
		{name: "auto-edit with builtins still asks for a sensitive file", mode: policy.ModeAutoEdit, layers: [][]policy.Rule{builtin}, req: f.write("prod.tfvars"), want: policy.Ask, reason: "sensitive"},
		{name: "auto-edit with builtins still asks for an ignored file", mode: policy.ModeAutoEdit, layers: [][]policy.Rule{builtin}, req: f.write("build/out"), want: policy.Ask, reason: "ignored"},
		{name: "auto-edit with builtins still asks outside", mode: policy.ModeAutoEdit, layers: [][]policy.Rule{builtin}, req: f.write("../elsewhere/f.txt"), want: policy.Ask, reason: "outside"},
		{name: "auto-edit with builtins still hard-denies .git", mode: policy.ModeAutoEdit, layers: [][]policy.Rule{builtin}, req: f.write(".git/hooks/pre-commit"), want: policy.Deny, hard: true},
		{name: "auto-edit with builtins still hard-denies a secret", mode: policy.ModeAutoEdit, layers: [][]policy.Rule{builtin}, req: f.write(".env"), want: policy.Deny, hard: true},
		{name: "auto-edit does not widen an explicit user ask rule", mode: policy.ModeAutoEdit, layers: [][]policy.Rule{builtin, rules(t, policy.Ask, policy.SourceUser, "edit_file($WORKSPACE/**)")}, req: f.write("main.go"), want: policy.Ask, reason: "ask rule"},
		{name: "auto-edit does not widen bash", mode: policy.ModeAutoEdit, layers: [][]policy.Rule{builtin}, req: f.bash("make build"), want: policy.Ask},
		{name: "write bypass allows", mode: policy.ModeBypass, req: f.write("main.go"), want: policy.Allow},
		{name: "write bypass outside allows", mode: policy.ModeBypass, req: f.write("../elsewhere/f.txt"), want: policy.Allow},
		// --- mode table: bash
		{name: "bash safe read default", mode: policy.ModeDefault, req: f.bash("git status"), want: policy.Allow},
		{name: "bash mutating default asks", mode: policy.ModeDefault, req: f.bash("make build"), want: policy.Ask, offers: true},
		{name: "bash mutating auto-edit asks", mode: policy.ModeAutoEdit, req: f.bash("make build"), want: policy.Ask},
		{name: "bash mutating plan denies", mode: policy.ModePlan, req: f.bash("make build"), want: policy.Deny},
		{name: "bash network default asks", mode: policy.ModeDefault, req: f.bash("go get x"), want: policy.Ask, offers: true},
		{name: "bash destructive default asks no offers", mode: policy.ModeDefault, req: f.bash("rm -rf build"), want: policy.Ask, noOffers: true},
		{name: "bash destructive bypass allows", mode: policy.ModeBypass, req: f.bash("rm -rf build"), want: policy.Allow},
		{name: "bash privilege denied everywhere", mode: policy.ModeBypass, req: f.bash("apt-get install x"), want: policy.Deny, reason: "privilege"},
		{name: "bash privilege hard-denied", mode: policy.ModeBypass, req: f.bash("mkfs.ext4 /dev/sda"), want: policy.Deny, hard: true},
		{name: "user allow beats builtin ask for git push", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Allow, policy.SourceUser, "bash(git push origin feature) +net")}, req: f.bash("git push origin feature"), want: policy.Allow, network: true},
		{name: "project ask beats user allow", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, userAllow, rules(t, policy.Ask, policy.SourceProject, "bash(npm *)")}, req: f.bash("npm test"), want: policy.Ask},
		// An opaque script — the analyser cannot see what runs — is asked
		// about and offered nothing, because no rule could honour the offer.
		{name: "bash opaque default asks no offers", mode: policy.ModeDefault, req: f.bash(`eval "$CMD"`), want: policy.Ask, noOffers: true},
		// An unrecognised *program* is fully legible, so it still asks but
		// the user can be offered a rule that names it. Before the split,
		// this row asserted noOffers, which is why a user compiling with an
		// unmodelled compiler was asked on every single call for ever.
		{name: "bash unrecognised default asks with offers", mode: policy.ModeDefault, req: f.bash("frobnicate --all"), want: policy.Ask, offers: true},
		{name: "bash unrecognised is never auto-allowed", mode: policy.ModeAutoEdit, req: f.bash("frobnicate --all"), want: policy.Ask},
		{name: "a rule naming an unrecognised program covers it", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Allow, policy.SourceUser, "bash(frobnicate *)")}, req: f.bash("frobnicate --all"), want: policy.Allow},
		{name: "no rule can cover an opaque script", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Allow, policy.SourceUser, "bash(eval *)", "bash")}, req: f.bash(`eval "$CMD"`), want: policy.Ask},
		{name: "bash unknown bypass allows", mode: policy.ModeBypass, req: f.bash("frobnicate --all"), want: policy.Allow},
		{name: "bash read outside asks", mode: policy.ModeDefault, req: f.bash("cat " + filepath.Join(f.home, "notes", "a.md")), want: policy.Ask, reason: "outside"},
		{name: "bash read protected denies", mode: policy.ModeDefault, req: f.bash("cat /etc/passwd"), want: policy.Deny, reason: "protected"},
		{name: "bash read protected denies in its resolved spelling", mode: policy.ModeDefault, req: f.bash("cat " + etcPasswd), want: policy.Deny, reason: "protected"},
		// A builtin allow rule matches argv alone, so the containment checks
		// have to run first or `bash(cat *)` covers the whole filesystem.
		{name: "allow rule cannot cover a protected read", mode: policy.ModeDefault, req: f.bash("head -c 200 /etc/passwd"), want: policy.Deny, reason: "protected"},
		{name: "allow rule cannot cover a read outside the workspace", mode: policy.ModeDefault, req: f.bash("cat " + filepath.Join(f.home, "notes", "a.md")), want: policy.Ask, reason: "outside"},
		{name: "recursive read exposing a secret asks", mode: policy.ModeDefault, req: f.bash(`grep -r "" .`), want: policy.Ask, reason: "credential"},
		// A recursive reader with no path operand walks the working
		// directory while naming nothing; the scan has to notice anyway.
		{name: "recursive read with an implied cwd asks", mode: policy.ModeDefault, req: f.bash("grep -rI SECRET"), want: policy.Ask, reason: "credential"},
		{name: "recursive rg with an implied cwd asks", mode: policy.ModeDefault, req: f.bash("rg -n SECRET"), want: policy.Ask, reason: "credential"},
		{name: "find with an implied cwd asks", mode: policy.ModeDefault, req: f.bash("find -name '*.go'"), want: policy.Ask, reason: "credential"},
		{name: "non-recursive grep of one file stays allowed", mode: policy.ModeDefault, req: f.bash("grep SECRET main.go"), want: policy.Allow},
		{name: "echo is not a directory reader", mode: policy.ModeDefault, req: f.bash("echo hello"), want: policy.Allow},
		{name: "recursive read of a clean subtree stays allowed", mode: policy.ModeDefault, req: f.bash("grep -r x internal"), want: policy.Allow},
		// A submodule's hooks run outside the sandbox just like the root's.
		{name: "nested .git hooks are protected", mode: policy.ModeDefault, req: f.bash("echo x > internal/.git/hooks/pre-commit"), want: policy.Deny, reason: "protected", hard: true},
		{name: "bypass still cannot read a protected path", mode: policy.ModeBypass, req: f.bash("head -c 200 /etc/passwd"), want: policy.Deny, reason: "protected"},
		{name: "bash write outside asks", mode: policy.ModeDefault, req: f.bash("echo x > " + filepath.Join(f.home, "notes", "b.md")), want: policy.Ask, reason: "outside"},
		{name: "bash bypass never grants network", mode: policy.ModeBypass, req: f.bash("go get x"), want: policy.Allow, network: false},
		{name: "model network request asks even for reads", mode: policy.ModeDefault, req: withNet(f.bash("git status")), want: policy.Ask, reason: "network"},
		{name: "allow rule without +net does not cover a network request", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Allow, policy.SourceUser, "bash(git status *)")}, req: withNet(f.bash("git status")), want: policy.Ask, reason: "network"},
		{name: "allow rule without +net does not cover a network command", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Allow, policy.SourceUser, "bash(go get *)")}, req: f.bash("go get x"), want: policy.Ask, reason: "network"},
		{name: "allow rule with +net covers and grants network", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Allow, policy.SourceUser, "bash(go get *) +net")}, req: f.bash("go get x"), want: policy.Allow, network: true},
		// A saved rule for an installing command needs +install as well as
		// the network: without it the call was allowed and then failed on a
		// read-only prefix, which is a grant that cannot do its job. The
		// rule now has to say so, and the prompt it falls through to can.
		{name: "allow rule with only +net does not cover an install", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Allow, policy.SourceUser, "bash(brew install *) +net")}, req: f.bash("brew install fpc"), want: policy.Ask},
		{name: "allow rule with +install covers and grants both", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Allow, policy.SourceUser, "bash(brew install *) +install")}, req: f.bash("brew install fpc"), want: policy.Allow, network: true, installs: true},
		{name: "+install does not grant an install to another command", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Allow, policy.SourceUser, "bash(go get *) +install")}, req: f.bash("go get x"), want: policy.Allow, network: true, installs: true},
		{name: "a non-installing command allowed by +net grants no install", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Allow, policy.SourceUser, "bash(go get *) +net")}, req: f.bash("go get x"), want: policy.Allow, network: true, installs: false},
		{name: "bypass grants no install", mode: policy.ModeBypass, req: f.bash("brew install fpc"), want: policy.Allow, installs: false},
		{name: "bash plan safe read allowed", mode: policy.ModePlan, req: f.bash("git log"), want: policy.Allow},
		// Allow rules match tool and argv, never the effect, so plan mode
		// must not consult them: the builtin bash(git show *) would
		// otherwise authorise --output, and bash(go build *) a build.
		{name: "plan mode ignores an allow rule that would write", mode: policy.ModePlan, req: f.bash("git show --output=out.txt HEAD"), want: policy.Deny, reason: "plan mode"},
		{name: "plan mode ignores a builtin allow for a build", mode: policy.ModePlan, req: f.bash("go build ./..."), want: policy.Deny, reason: "plan mode"},
		{name: "plan mode ignores a user allow rule for an edit", mode: policy.ModePlan, layers: [][]policy.Rule{builtin, rules(t, policy.Allow, policy.SourceUser, "edit_file(**)")}, req: f.write("main.go"), want: policy.Deny, reason: "plan mode"},
		{name: "bash plan read protected denies", mode: policy.ModePlan, req: f.bash("cat /etc/passwd"), want: policy.Deny, reason: "protected"},
		{name: "bash plan read outside asks", mode: policy.ModePlan, req: f.bash("cat " + filepath.Join(f.home, "notes", "a.md")), want: policy.Ask, reason: "outside"},
		{name: "bash bypass read protected denies", mode: policy.ModeBypass, req: f.bash("cat /etc/passwd"), want: policy.Deny, reason: "protected"},
		{name: "bash bypass read protected denies in its resolved spelling", mode: policy.ModeBypass, req: f.bash("cat " + etcPasswd), want: policy.Deny, reason: "protected"},
		{name: "bash plan network denied", mode: policy.ModePlan, req: f.bash("go get x"), want: policy.Deny},
		{name: "bash without analysis denied", mode: policy.ModeBypass, req: policy.Request{Tool: "bash"}, want: policy.Deny},
		// --- web / mcp / other
		{name: "web default asks", mode: policy.ModeDefault, req: fetch("https://example.com"), want: policy.Ask},
		{name: "web plan asks", mode: policy.ModePlan, req: fetch("https://example.com"), want: policy.Ask},
		{name: "web bypass allows", mode: policy.ModeBypass, req: fetch("https://example.com"), want: policy.Allow},
		{name: "mcp default asks", mode: policy.ModeDefault, req: policy.Request{Tool: "mcp:s:t"}, want: policy.Ask, offers: true},
		{name: "mcp plan read-only asks", mode: policy.ModePlan, req: policy.Request{Tool: "mcp:s:t", ReadOnly: true}, want: policy.Ask},
		{name: "mcp plan mutating denies", mode: policy.ModePlan, req: policy.Request{Tool: "mcp:s:t"}, want: policy.Deny},
		{name: "mcp bypass allows", mode: policy.ModeBypass, req: policy.Request{Tool: "mcp:s:t"}, want: policy.Allow},
		{name: "mcp allow rule", mode: policy.ModeDefault, layers: [][]policy.Rule{rules(t, policy.Allow, policy.SourceUser, "mcp:s:t*")}, req: policy.Request{Tool: "mcp:s:tool"}, want: policy.Allow},
		// --- workspace trust: the baseline, and every floor above it
		//
		// The builtin layer is present in all of these on purpose: the
		// builtin ask rules for write_file/edit_file are what the baseline
		// has to reach past, and without them the table would pass with
		// only half the mechanism in place.
		{name: "trusted edit inside asks nothing", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.write("main.go"), want: policy.Allow, reason: "workspace-trust", trusted: true},
		{name: "trusted write_file inside asks nothing", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.writeWith("write_file", "new.go"), want: policy.Allow, trusted: true},
		{name: "trusted multi_edit inside asks nothing", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.writeWith("multi_edit", "main.go"), want: policy.Allow, trusted: true},
		{name: "untrusted edit inside still asks", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.write("main.go"), want: policy.Ask, offers: true},
		{name: "untrusted multi_edit inside still asks", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.writeWith("multi_edit", "main.go"), want: policy.Ask, offers: true},
		{name: "trusted still asks for a sensitive file", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.write("prod.tfvars"), want: policy.Ask, reason: "sensitive", trusted: true},
		{name: "trusted still asks for an ignored file", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.write("build/out"), want: policy.Ask, reason: "ignored", trusted: true},
		{name: "trusted still asks outside the workspace", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.write("../elsewhere/f.txt"), want: policy.Ask, reason: "outside", trusted: true},
		{name: "trusted still asks for a multi_edit outside", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.writeWith("multi_edit", "../elsewhere/f.txt"), want: policy.Ask, reason: "outside", trusted: true},
		// Trust plus several paths in one call: the trusted path must not
		// carry the untrusted one through with it. This is the combination
		// workspace trust and multi_edit only create together.
		{name: "trusted multi_edit does not carry a path outside", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.writeWith("multi_edit", "main.go", "../elsewhere/f.txt"), want: policy.Ask, reason: "outside", trusted: true},
		{name: "trusted multi_edit does not carry a protected path", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.writeWith("multi_edit", "main.go", ".git/config"), want: policy.Deny, hard: true, trusted: true},
		{name: "trusted still hard-denies .git", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.write(".git/hooks/pre-commit"), want: policy.Deny, hard: true, trusted: true},
		{name: "trusted still hard-denies .wright", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.write(".wright/settings.json"), want: policy.Deny, hard: true, trusted: true},
		{name: "trusted still hard-denies a secret file", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.write(".env"), want: policy.Deny, hard: true, reason: "protected", trusted: true},
		{name: "trusted still hard-denies a protected path", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.write("~/.ssh/config"), want: policy.Deny, hard: true, reason: "protected", trusted: true},
		{name: "trusted still denies a hidden path", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.write("secrets/token"), want: policy.Deny, reason: "hidden", trusted: true},
		{name: "trusted still obeys a deny rule", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Deny, policy.SourceProject, "edit_file($WORKSPACE/internal/**)")}, req: f.write("internal/x.go"), want: policy.Deny, reason: "deny rule", trusted: true},
		{name: "trusted still obeys an explicit ask rule", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Ask, policy.SourceUser, "edit_file(**)")}, req: f.write("main.go"), want: policy.Ask, reason: "ask rule", trusted: true},
		// A builtin ask rule that is *not* the whole-workspace default is
		// not the mode table written as a rule, so trust does not replace it.
		{name: "trusted still obeys a narrower builtin ask rule", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Ask, policy.SourceBuiltin, "edit_file($WORKSPACE/internal/**)")}, req: f.write("internal/x.go"), want: policy.Ask, reason: "ask rule", trusted: true},
		{name: "trusted plan mode still denies edits", mode: policy.ModePlan, layers: [][]policy.Rule{builtin}, req: f.write("main.go"), want: policy.Deny, reason: "plan mode", trusted: true},
		// Trust is about writing files, not about running programs: every
		// shell command is still approved one at a time.
		{name: "trusted bash still asks", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.bash("make build"), want: policy.Ask, trusted: true},
		{name: "trusted bash write inside still asks", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.bash("echo x > main.go"), want: policy.Ask, trusted: true},
		// A write tool call that declares no path is not an edit inside the
		// workspace; the baseline must not cover what it cannot see.
		{name: "trusted write with no declared path still asks", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: policy.Request{Tool: "edit_file"}, want: policy.Ask, trusted: true},
		// Trust is not bypass: reads and fetches are unchanged by it.
		{name: "trusted web_fetch still asks", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: fetch("https://example.com"), want: policy.Ask, trusted: true},
		{name: "trusted read outside still asks", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.read("/tmp/elsewhere.txt"), want: policy.Ask, reason: "outside", trusted: true},
		{name: "explore allowed", mode: policy.ModePlan, req: policy.Request{Tool: "explore"}, want: policy.Allow},
		{name: "unknown tool asks", mode: policy.ModeDefault, req: policy.Request{Tool: "teleport"}, want: policy.Ask},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := policy.New(f.ws, tt.mode, tt.layers...)
			if tt.trusted {
				e.TrustWorkspace()
			}
			v := e.Evaluate(tt.req)
			if v.Decision != tt.want {
				t.Errorf("Decision = %v, want %v\n  reason: %s\n  explain: %s", v.Decision, tt.want, v.Reason, strings.Join(v.Explain, "\n           "))
			}
			if v.HardDeny != tt.hard {
				t.Errorf("HardDeny = %v, want %v (%s)", v.HardDeny, tt.hard, v.Reason)
			}
			if tt.reason != "" && !strings.Contains(v.Reason, tt.reason) {
				t.Errorf("Reason = %q, want substring %q", v.Reason, tt.reason)
			}
			if tt.offers && len(v.Offers) == 0 {
				t.Errorf("expected offers, got none (%s)", v.Reason)
			}
			if tt.noOffers && len(v.Offers) != 0 {
				t.Errorf("expected no offers, got %v", v.Offers)
			}
			if v.Network != tt.network {
				t.Errorf("Network = %v, want %v", v.Network, tt.network)
			}
			if v.Installs != tt.installs {
				t.Errorf("Installs = %v, want %v", v.Installs, tt.installs)
			}
			if len(v.Explain) == 0 {
				t.Error("Explain is empty")
			}
			if tt.hard && e.HardDenials() != 1 {
				t.Errorf("HardDenials = %d, want 1", e.HardDenials())
			}
		})
	}
}

func TestGrantsAndSuggest(t *testing.T) {
	f := newFixture(t)
	e := policy.New(f.ws, policy.ModeDefault, policy.Builtin())
	req := f.bash("make build")
	v := e.Evaluate(req)
	if v.Decision != policy.Ask || len(v.Offers) == 0 {
		t.Fatalf("expected ask with offers, got %v %v", v.Decision, v.Offers)
	}
	var session policy.GrantOffer
	for _, o := range v.Offers {
		if o.Scope == policy.ScopeSession {
			session = o
		}
	}
	if session.Rule.String() != "bash(make build *)" {
		t.Errorf("offer rule = %s, want bash(make build *)", session.Rule)
	}
	if !strings.Contains(session.Label, "session") {
		t.Errorf("label = %q", session.Label)
	}
	if err := e.Grant(session); err != nil {
		t.Fatal(err)
	}
	if v := e.Evaluate(req); v.Decision != policy.Allow || v.Rule == nil || v.Rule.Source != policy.SourceSession {
		t.Errorf("after grant: %v %v", v.Decision, v.Rule)
	}
	if got := e.Grants(); len(got) != 1 {
		t.Errorf("Grants = %v", got)
	}
	// Once records nothing.
	if err := e.Grant(policy.GrantOffer{Rule: session.Rule, Scope: policy.ScopeOnce}); err != nil || len(e.Grants()) != 1 {
		t.Error("once grant must not persist")
	}
	// Project-local needs a persister and calls it.
	proj := policy.GrantOffer{Rule: session.Rule, Scope: policy.ScopeProjectLocal}
	if err := e.Grant(proj); !errors.Is(err, policy.ErrNoPersist) {
		t.Errorf("expected ErrNoPersist, got %v", err)
	}
	var persisted []string
	e.SetPersist(func(r policy.Rule) error { persisted = append(persisted, r.String()); return nil })
	if err := e.Grant(proj); err != nil || len(persisted) != 1 || persisted[0] != "bash(make build *)" {
		t.Errorf("persist: %v %v", err, persisted)
	}
	e.SetPersist(func(policy.Rule) error { return errors.New("disk full") })
	if err := e.Grant(proj); err == nil || err.Error() != "disk full" {
		t.Errorf("persist error not propagated: %v", err)
	}
}

func TestSuggestShapes(t *testing.T) {
	f := newFixture(t)
	e := policy.New(f.ws, policy.ModeDefault)
	got := func(req policy.Request) []string {
		var out []string
		for _, o := range e.Suggest(req) {
			if o.Scope == policy.ScopeSession {
				out = append(out, o.Rule.String())
			}
		}
		return out
	}
	tests := []struct {
		name string
		req  policy.Request
		want []string
	}{
		{"bash two words", f.bash("go test ./... -run X"), []string{"bash(go test *)"}},
		{"bash one word", f.bash("make"), []string{"bash(make *)"}},
		{"bash network gets +net", f.bash("go mod download"), []string{"bash(go mod *) +net"}},
		{"bash multi", f.bash("go build ./... && go vet ./..."), []string{"bash(go build *)", "bash(go vet *)"}},
		{"bash destructive none", f.bash("rm -rf build"), nil},
		{"bash opaque none", f.bash("eval x"), nil},
		{"read parent dir", f.read("internal/x.go"), []string{"read_file($WORKSPACE/internal/**)"}},
		{"read root", f.read("main.go"), []string{"read_file($WORKSPACE/**)"}},
		{"read home", f.read("~/notes/a.md"), []string{"read_file(~/notes/**)"}},
		{"read elsewhere", f.read("/opt/x/y"), []string{"read_file(/opt/x/**)"}},
		{"read protected none", f.read("~/.ssh/id_ed"), nil},
		{"fetch domain", fetch("https://api.example.com/x"), []string{"web_fetch(domain:api.example.com)"}},
		{"mcp", policy.Request{Tool: "mcp:s:t"}, []string{"mcp:s:t"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := got(tt.req)
			if strings.Join(g, ",") != strings.Join(tt.want, ",") {
				t.Errorf("Suggest = %v, want %v", g, tt.want)
			}
		})
	}
}

func TestChildEngine(t *testing.T) {
	f := newFixture(t)
	parent := policy.New(f.ws, policy.ModeBypass, policy.Builtin())
	if err := parent.Grant(policy.GrantOffer{Rule: mustRule(t, "bash(make *)"), Scope: policy.ScopeSession}); err != nil {
		t.Fatal(err)
	}
	child := parent.Child(1)
	if child.Mode() != policy.ModeDefault {
		t.Errorf("child mode = %v, want default (no bypass inheritance)", child.Mode())
	}
	if child.Depth() != 1 {
		t.Errorf("Depth = %d", child.Depth())
	}
	if v := child.Evaluate(f.bash("make build")); v.Decision != policy.Allow {
		t.Errorf("child should see parent's grants: %v %s", v.Decision, v.Reason)
	}
	if err := child.Grant(policy.GrantOffer{Rule: mustRule(t, "bash(npm *)"), Scope: policy.ScopeSession}); !errors.Is(err, policy.ErrChildGrant) {
		t.Errorf("child grant: %v", err)
	}
	if err := child.SetMode(policy.ModeBypass); !errors.Is(err, policy.ErrBypassNotSettable) {
		t.Errorf("SetMode(bypass) = %v", err)
	}
	if err := parent.SetMode(policy.ModeBypass); !errors.Is(err, policy.ErrBypassNotSettable) {
		t.Errorf("parent SetMode(bypass) = %v", err)
	}
	if err := parent.SetMode(policy.ModePlan); err != nil {
		t.Fatal(err)
	}
	strict := parent.Child(1)
	if err := strict.SetMode(policy.ModeAutoEdit); !errors.Is(err, policy.ErrModeAboveParent) {
		t.Errorf("child above parent: %v", err)
	}
	if err := strict.SetMode(policy.ModePlan); err != nil {
		t.Errorf("child equal to parent: %v", err)
	}
	// Workspace trust is inherited: a sub-agent writes into the directory
	// the user accepted, under the same floors. It is not a grant, so
	// ErrChildGrant does not apply to it.
	trusting := policy.New(f.ws, policy.ModeDefault, policy.Builtin())
	trusting.TrustWorkspace()
	if !trusting.WorkspaceTrusted() {
		t.Error("TrustWorkspace did not take")
	}
	sub := trusting.Child(1)
	if !sub.WorkspaceTrusted() {
		t.Error("a child engine should inherit workspace trust")
	}
	if v := sub.Evaluate(f.write("main.go")); v.Decision != policy.Allow {
		t.Errorf("child edit inside a trusted workspace = %v (%s)", v.Decision, v.Reason)
	}
	if v := parent.Child(1).Evaluate(f.write("main.go")); v.Decision == policy.Allow {
		t.Error("an untrusted parent must not hand a child the baseline")
	}
	// Hard denials propagate to the parent.
	child.Evaluate(f.bash("sudo ls"))
	if parent.HardDenials() != 1 || child.HardDenials() != 1 {
		t.Errorf("hard denials parent=%d child=%d", parent.HardDenials(), child.HardDenials())
	}
}

func mustRule(t *testing.T, s string) policy.Rule {
	t.Helper()
	r, err := policy.ParseRule(s, policy.SourceUser)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExplainTrace(t *testing.T) {
	f := newFixture(t)
	e := policy.New(f.ws, policy.ModeDefault, policy.Builtin())
	v := e.Evaluate(f.bash("go test ./... && make"))
	joined := strings.Join(v.Explain, "\n")
	for _, want := range []string{"allow rules do not cover", "make", "→ ask"} {
		if !strings.Contains(joined, want) {
			t.Errorf("explain missing %q:\n%s", want, joined)
		}
	}
	if v.Class != shellclass.MutatingWorkspace {
		t.Errorf("Class = %v", v.Class)
	}
}

func TestShellWorkspaceAdapter(t *testing.T) {
	f := newFixture(t)
	sh := policy.NewShellWorkspace(f.ws, nil, []string{"trunk"})
	if sh.Home() != f.home || sh.Roots()[0] != f.root {
		t.Errorf("adapter home/roots mismatch")
	}
	if !sh.IsSecretFile(f.abs(".env")) || !sh.IsProtected(f.abs("~/.ssh/x")) {
		t.Error("adapter predicates mismatch")
	}
	if sh.IsTracked(f.abs("main.go")) {
		t.Error("no git repo: nothing is tracked")
	}
	a := shellclass.Analyze("git push --force origin trunk", sh)
	if a.HardDeny == "" {
		t.Error("custom protected branch not honoured")
	}
	if a := shellclass.Analyze("git push --force origin main", sh); a.HardDeny != "" {
		t.Error("override should replace the default set")
	}
}

// withNet marks a request as one where the model asked for network access.
func withNet(r policy.Request) policy.Request {
	r.Network = true
	return r
}

// Paths reach the classifier already symlink-resolved, and most system roots
// are symlinks on a real machine: merged-usr Linux points /bin at /usr/bin,
// macOS points /etc and /var into /private. Matching only the configured
// spelling let `chmod -R 777 /bin` through as an ordinary write.
func TestRecursiveChmodOnSystemRootsIsHardDenied(t *testing.T) {
	f := newFixture(t)
	for _, dir := range []string{"/", "/usr", "/etc", "/bin", "/sbin", "/lib", "/var"} {
		if _, err := os.Stat(dir); err != nil {
			continue // not every root exists on every platform
		}
		for _, spelling := range []string{dir, resolvedPath(t, dir)} {
			e := policy.New(f.ws, policy.ModeBypass, policy.Builtin())
			v := e.Evaluate(f.bash("chmod -R 777 " + spelling))
			if v.Decision != policy.Deny || !v.HardDeny {
				t.Errorf("chmod -R on %q (for %s) = %v hard=%v, want a hard deny", spelling, dir, v.Decision, v.HardDeny)
			}
		}
	}
	// An ordinary recursive chmod inside the workspace must still be fine.
	e := policy.New(f.ws, policy.ModeAutoEdit, policy.Builtin())
	if v := e.Evaluate(f.bash("chmod -R 755 sub")); v.HardDeny {
		t.Errorf("chmod -R inside the workspace was hard-denied: %s", v.Reason)
	}
}

// TestMultiEditIsAWriteEverywhere pins multi_edit against the whole write
// lattice. It is the newest write tool and the one most likely to be missed
// when a check is added, because unlike edit_file it can name several files
// in one call: a floor that holds for edit_file and not for multi_edit would
// be a way around the floor rather than a gap in one tool.
func TestMultiEditIsAWriteEverywhere(t *testing.T) {
	f := newFixture(t)
	builtin := policy.Builtin()
	tests := []struct {
		name   string
		mode   policy.Mode
		layers [][]policy.Rule
		req    policy.Request
		want   policy.Decision
		reason string
		hard   bool
	}{
		{name: "asks in default mode", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin}, req: f.writeWith("multi_edit", "main.go"), want: policy.Ask},
		{name: "denies in plan mode", mode: policy.ModePlan, layers: [][]policy.Rule{builtin}, req: f.writeWith("multi_edit", "main.go"), want: policy.Deny, reason: "plan mode"},
		{name: "a .git write is hard-denied", mode: policy.ModeBypass, req: f.writeWith("multi_edit", ".git/hooks/pre-commit"), want: policy.Deny, hard: true},
		{name: "a secret write is hard-denied", mode: policy.ModeBypass, req: f.writeWith("multi_edit", ".env"), want: policy.Deny, hard: true},
		{name: "a user deny rule wins", mode: policy.ModeDefault, layers: [][]policy.Rule{builtin, rules(t, policy.Deny, policy.SourceUser, "multi_edit(**)")}, req: f.writeWith("multi_edit", "main.go"), want: policy.Deny},
		// One call names several files, so a permitted path must not carry a
		// forbidden one through with it. These are the rows a single-path
		// write tool could never express.
		{name: "a good path does not carry a protected one", mode: policy.ModeBypass, layers: [][]policy.Rule{builtin}, req: f.writeWith("multi_edit", "main.go", ".git/config"), want: policy.Deny, hard: true},
		{name: "a good path does not carry an outside one", mode: policy.ModeDefault, req: f.writeWith("multi_edit", "main.go", "../elsewhere/f.txt"), want: policy.Ask, reason: "outside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := policy.New(f.ws, tt.mode, tt.layers...)
			v := e.Evaluate(tt.req)
			if v.Decision != tt.want {
				t.Errorf("decision = %v, want %v (%s)", v.Decision, tt.want, v.Reason)
			}
			if tt.hard && !v.HardDeny {
				t.Errorf("want a hard deny, got %q", v.Reason)
			}
			if tt.reason != "" && !strings.Contains(v.Reason, tt.reason) {
				t.Errorf("reason = %q, want containing %q", v.Reason, tt.reason)
			}
		})
	}
}

// TestInstallGrantIsSavable pins the saved-rule half of the install grant.
// An interactive approval of an installing command mounts the package
// manager's prefixes read-write for that one call; before +install there was
// no way to write that down, so --allow 'bash(brew install *) +net' produced
// a call that was allowed and then failed on a read-only file system. That
// is a grant that cannot do its job, which is worse than a prompt.
func TestInstallGrantIsSavable(t *testing.T) {
	f := newFixture(t)
	req := f.bash("brew install fpc")

	t.Run("without +install the reason names it", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin(),
			rules(t, policy.Allow, policy.SourceUser, "bash(brew install *) +net"))
		v := e.Evaluate(req)
		if v.Decision != policy.Allow && !containsAny(v.Explain, "+install") {
			t.Errorf("nothing told the user which flag to add: %v", v.Explain)
		}
		if v.Installs {
			t.Error("a rule without +install granted the prefixes anyway")
		}
	})

	t.Run("the offer for an installing command carries it", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin())
		v := e.Evaluate(req)
		var texts []string
		for _, o := range v.Offers {
			texts = append(texts, o.Rule.String())
		}
		if !containsAny(texts, "+install") {
			t.Errorf("offers = %v, want one carrying +install", texts)
		}
		// +install implies the network, so the offer must not also say +net:
		// one grant, described once.
		for _, s := range texts {
			if strings.Contains(s, "+install") && strings.Contains(s, "+net") {
				t.Errorf("offer %q describes one grant as two", s)
			}
		}
	})
}

// containsAny reports whether any element of list contains sub.
func containsAny(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestPlanModeKeepsEveryToolItCanPermit is the invariant that would have
// caught web_fetch. Plan mode filters the toolset down to a list, and the
// system prompt describes tools regardless, so a tool the policy permits but
// the list omits comes back to the model as `tool not found` — which is
// exactly what web_fetch, web_search, bash, todo_write, ask_user and job did.
// The policy is the authority: whatever plan mode does not always deny, plan
// mode must still offer.
func TestPlanModeKeepsEveryToolItCanPermit(t *testing.T) {
	f := newFixture(t)
	e := policy.New(f.ws, policy.ModePlan, policy.Builtin())
	// One representative request per built-in tool, chosen to be the most
	// permissive thing that tool can ask for.
	reqs := map[string]policy.Request{
		"read_file":  f.read("main.go"),
		"glob":       {Tool: "glob", Paths: []string{f.abs("main.go")}},
		"grep":       {Tool: "grep", Paths: []string{f.abs("main.go")}},
		"list_dir":   {Tool: "list_dir", Paths: []string{f.abs(".")}},
		"bash":       f.bash("git log"),
		"web_fetch":  fetch("https://example.com/x"),
		"web_search": {Tool: "web_search"},
		"todo_write": {Tool: "todo_write"},
		"ask_user":   {Tool: "ask_user"},
		"job":        {Tool: "job"},
		"write_file": f.writeWith("write_file", "main.go"),
		"edit_file":  f.write("main.go"),
		"multi_edit": f.writeWith("multi_edit", "main.go"),
	}
	kept := tools.PlanNames()
	for _, name := range tools.Names() {
		req, ok := reqs[name]
		if !ok {
			t.Fatalf("no representative request for %s; add one when adding a tool", name)
		}
		permitted := e.Evaluate(req).Decision != policy.Deny
		if permitted && !slices.Contains(kept, name) {
			t.Errorf("plan mode permits %s but drops it from the toolset: the model is told it exists and gets \"tool not found\"", name)
		}
		if !permitted && slices.Contains(kept, name) {
			t.Errorf("plan mode always denies %s, so keeping it in the toolset only wastes a turn", name)
		}
	}
}

// TestPlanModeOffersNothing pins that plan mode proposes no saved rule.
// It never consults allow rules, so an offer there cannot take effect: the
// approval overlay would invite "always allow" and then keep asking, and a
// headless denial would name a --allow flag that changes nothing.
func TestPlanModeOffersNothing(t *testing.T) {
	f := newFixture(t)
	for _, mode := range []policy.Mode{policy.ModePlan, policy.ModeDefault} {
		e := policy.New(f.ws, mode, policy.Builtin())
		v := e.Evaluate(fetch("https://example.com/x"))
		if v.Decision != policy.Ask {
			t.Fatalf("%v: decision = %v, want Ask", mode, v.Decision)
		}
		switch mode {
		case policy.ModePlan:
			if len(v.Offers) != 0 {
				t.Errorf("plan mode offered %v, which no rule there can honour", v.Offers)
			}
		default:
			if len(v.Offers) == 0 {
				t.Error("default mode must still offer a rule that would work")
			}
		}
	}
}

// TestSavedRuleCoversARealBuildCommand is the acceptance test for the report
// behind this change: a user compiling Pascal approved the same command on
// every call, with no "always allow" offered, because fpc is not in the
// classifier's table.
//
// It pins the whole path end to end: the script asks and offers a rule naming
// the program, the offered rule is one a person would recognise, and once it
// is in the allow layer the same shape runs without a prompt — including the
// `cd` and `grep` the model wraps around it, which need no rule of their own.
func TestSavedRuleCoversARealBuildCommand(t *testing.T) {
	f := newFixture(t)
	script := func(src, grepFlag string) policy.Request {
		return f.bash("cd " + f.ws.Root() + " && fpc -Mobjfpc -Sh -FUbuild -Fusrc " + src + " 2>&1 | grep " + grepFlag + " warning")
	}

	t.Run("asks, and offers a rule naming the program", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin())
		v := e.Evaluate(script("src/A.pas", "-i"))
		if v.Decision != policy.Ask {
			t.Fatalf("decision = %v, want Ask", v.Decision)
		}
		var texts []string
		for _, o := range v.Offers {
			texts = append(texts, o.Rule.String())
		}
		if !slices.Contains(texts, "bash(fpc *)") {
			t.Errorf("offers = %v, want one naming the program as bash(fpc *)", texts)
		}
	})

	t.Run("the saved rule covers the same shape again", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin(),
			rules(t, policy.Allow, policy.SourceSession, "bash(fpc *)"))
		// A different source file and different grep flags: the model does
		// not repeat itself exactly, and a rule that only matched verbatim
		// would send the user straight back to approving every call.
		for _, req := range []policy.Request{script("src/A.pas", "-i"), script("src/B.pas", "-Ei")} {
			if v := e.Evaluate(req); v.Decision != policy.Allow {
				t.Errorf("decision = %v, want Allow (%s) explain=%v", v.Decision, v.Reason, v.Explain)
			}
		}
	})

	t.Run("without the rule it still asks", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin())
		if v := e.Evaluate(script("src/A.pas", "-i")); v.Decision != policy.Ask {
			t.Errorf("decision = %v, want Ask without a rule", v.Decision)
		}
	})

	t.Run("the rule does not extend past the program it names", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin(),
			rules(t, policy.Allow, policy.SourceSession, "bash(fpc *)"))
		for _, tc := range []struct{ name, cmd string }{
			{"another program", "frobnicate --all"},
			{"an opaque script", `fpc x.pas && eval "$CMD"`},
			{"the hard-deny floor", "fpc x.pas; rm -rf ~"},
		} {
			if v := e.Evaluate(f.bash(tc.cmd)); v.Decision == policy.Allow {
				t.Errorf("%s: allowed by a rule for fpc (%s)", tc.name, v.Reason)
			}
		}
	})

	// TestSavedRuleCoversARealBuildCommand's limit, stated rather than
	// implied: an unrecognised program's arguments mean nothing to the
	// analyser, so it declares no paths and the containment checks have
	// nothing to check. A rule naming it therefore covers it whatever
	// arguments it is given — only the sandbox confines where it writes.
	// That is the price of being able to allow a program at all, and it is
	// the same bargain the builtin bash(go build *) already makes. It is
	// pinned here so the day someone teaches the table about fpc, this
	// expectation flips deliberately rather than silently.
	t.Run("an unrecognised program's arguments are not understood", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin(),
			rules(t, policy.Allow, policy.SourceSession, "bash(fpc *)"))
		v := e.Evaluate(f.bash("fpc -o /etc/x src/A.pas"))
		if v.Decision != policy.Allow {
			t.Errorf("decision = %v; the analyser cannot read -o, so this is expected to be covered", v.Decision)
		}
	})
}

// TestOfferedRuleNamesTheProgramNotTheWrapper is the acceptance test for the
// second half of the same report. The agent writes its own timeout, so the
// rule offered — and accepted — was bash(timeout 120 *): both too broad (any
// command under that exact wrapper) and too narrow (timeout 30 … never
// matched it). The binary's own rule was never proposed.
func TestOfferedRuleNamesTheProgramNotTheWrapper(t *testing.T) {
	f := newFixture(t)
	// The shape from the session, `echo "exit=$?"` and all: the trailing echo
	// is what made this script opaque before the inert-argument change, and
	// the wrapper is what made the rule useless after it.
	script := func(secs, suite string) policy.Request {
		return f.bash("cd " + f.ws.Root() + " && timeout " + secs + " ./bin/llmkittests " + suite + " 2>&1 | tail -70; echo \"exit=$?\"")
	}

	t.Run("asks, and offers a rule naming the binary", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin())
		v := e.Evaluate(script("120", "--all"))
		if v.Decision != policy.Ask {
			t.Fatalf("decision = %v, want Ask (%s)", v.Decision, v.Reason)
		}
		var texts []string
		for _, o := range v.Offers {
			texts = append(texts, o.Rule.String())
		}
		if !slices.Contains(texts, "bash(./bin/llmkittests *)") {
			t.Errorf("offers = %v, want one naming the binary as bash(./bin/llmkittests *)", texts)
		}
		if slices.Contains(texts, "bash(timeout 120 *)") {
			t.Errorf("offers = %v, still offering the wrapper", texts)
		}
	})

	t.Run("the offered rule covers the wrapped command", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin(),
			rules(t, policy.Allow, policy.SourceSession, "bash(./bin/llmkittests *)"))
		// A different duration and a different suite: the variant that
		// defeated the wrapper rule in the real session.
		for _, req := range []policy.Request{script("120", "--all"), script("30", "--suite=parse")} {
			if v := e.Evaluate(req); v.Decision != policy.Allow {
				t.Errorf("decision = %v, want Allow (%s) explain=%v", v.Decision, v.Reason, v.Explain)
			}
		}
	})

	t.Run("a rule already saved for the wrapper keeps working", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin(),
			rules(t, policy.Allow, policy.SourceSession, "bash(timeout 120 *)"))
		if v := e.Evaluate(script("120", "--all")); v.Decision != policy.Allow {
			t.Errorf("decision = %v, want Allow for the literal form it was written against (%s)", v.Decision, v.Reason)
		}
	})

	// Matching the peeled argv widens what a rule covers, so the floors have
	// to be shown still standing in front of it.
	t.Run("the floors run before any of this", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin(),
			rules(t, policy.Allow, policy.SourceSession, "bash(rm *)", "bash(./bin/llmkittests *)"))
		for _, tc := range []struct{ name, cmd string }{
			{"hard deny under a wrapper", "timeout 120 rm -rf ~"},
			{"a dynamic wrapper argument is still opaque", "timeout $N ./bin/llmkittests --all"},
			{"the wrapper is not a licence for another program", "timeout 120 frobnicate"},
		} {
			if v := e.Evaluate(f.bash(tc.cmd)); v.Decision == policy.Allow {
				t.Errorf("%s: allowed (%s)", tc.name, v.Reason)
			}
		}
	})
}

// TestABroadGhRuleCannotReachTheFloor is the hole this closes, taken from a
// real settings file. The first gh command a session ran happened to be
// `gh --version`, whose second word is flag-shaped, so the offer was the
// broadest rule the grammar allows — and the only Destructive guard in the
// policy layer is in *offering* a rule, never in matching one. So that saved
// rule auto-allowed every gh command, deletions included.
func TestABroadGhRuleCannotReachTheFloor(t *testing.T) {
	f := newFixture(t)
	allow := rules(t, policy.Allow, policy.SourceProjectLocal, "bash(gh *) +net")

	t.Run("the rule still does its job", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin(), allow)
		for _, cmd := range []string{"gh pr list", "gh issue view 3", "gh pr create --title x"} {
			if v := e.Evaluate(f.bash(cmd)); v.Decision != policy.Allow {
				t.Errorf("%s: decision = %v, want Allow (%s)", cmd, v.Decision, v.Reason)
			}
		}
	})

	t.Run("but never past the floor", func(t *testing.T) {
		for _, mode := range []policy.Mode{policy.ModeDefault, policy.ModeAutoEdit, policy.ModeBypass} {
			e := policy.New(f.ws, mode, policy.Builtin(), allow)
			for _, cmd := range []string{
				"gh repo delete o/r --yes",
				"gh api -X DELETE repos/o/r",
				"gh auth token",
				"gh secret set NAME",
			} {
				v := e.Evaluate(f.bash(cmd))
				if v.Decision != policy.Deny {
					t.Errorf("%s in %s: decision = %v, want Deny (%s)", cmd, mode, v.Decision, v.Reason)
				}
			}
		}
	})

	// A deletion that is not on the floor still asks rather than being
	// silently covered — no rule is ever offered for it, and the prompt
	// focuses deny.
	t.Run("other deletions are destructive, and say they need the network", func(t *testing.T) {
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin())
		v := e.Evaluate(f.bash("gh release delete v1"))
		if v.Decision != policy.Ask {
			t.Errorf("decision = %v, want Ask (%s)", v.Decision, v.Reason)
		}
		if len(v.Offers) != 0 {
			t.Errorf("offers = %v, want none for a destructive command", v.Offers)
		}
	})
}

// TestGhOffersNameTheCommandGroup pins what a saved rule is *for*. One
// approval of `gh pr view` should cover `gh pr list`, and nothing broader:
// the classifier knows gh's shape, so the policy layer does not have to
// guess it from a word that may be a flag.
func TestGhOffersNameTheCommandGroup(t *testing.T) {
	f := newFixture(t)
	offerFor := func(t *testing.T, cmd string) []string {
		t.Helper()
		e := policy.New(f.ws, policy.ModeDefault, policy.Builtin())
		v := e.Evaluate(f.bash(cmd))
		var texts []string
		for _, o := range v.Offers {
			if !slices.Contains(texts, o.Rule.String()) {
				texts = append(texts, o.Rule.String())
			}
		}
		return texts
	}

	tests := []struct {
		name, cmd, want string
	}{
		{name: "a read", cmd: "gh pr view 3", want: "bash(gh pr *) +net"},
		{name: "another verb in the same group", cmd: "gh pr list", want: "bash(gh pr *) +net"},
		{name: "a global flag before the noun", cmd: "gh --repo o/r issue list", want: "bash(gh issue *) +net"},
		{name: "a write", cmd: "gh pr create --title x", want: "bash(gh pr *) +net"},
		// Nothing to name but the program. Acceptable only because the
		// dangerous verbs are on the floor, where no rule reaches them.
		{name: "no subcommand", cmd: "gh --version", want: "bash(gh *) +net"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := offerFor(t, tt.cmd)
			if !slices.Contains(got, tt.want) {
				t.Errorf("offers = %v, want one reading %s", got, tt.want)
			}
			// The rule must be for the group, not for one invocation.
			for _, g := range got {
				if strings.Contains(g, "--") {
					t.Errorf("an offer pins a flag: %q", g)
				}
			}
		})
	}
}
