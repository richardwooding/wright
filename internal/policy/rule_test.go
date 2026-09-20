package policy_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/policy"
)

func TestParseRule(t *testing.T) {
	tests := []struct {
		text    string
		wantErr bool
		tool    string
		str     string
		net     bool
		install bool
	}{
		{text: "bash", tool: "bash", str: "bash"},
		{text: "bash(go test *)", tool: "bash", str: "bash(go test *)"},
		{text: "bash(git remote -v)", tool: "bash", str: "bash(git remote -v)"},
		{text: "  bash( go   test  * )  ", tool: "bash", str: "bash(go test *)"},
		{text: "bash(go mod download *) +net", tool: "bash", str: "bash(go mod download *) +net", net: true},
		{text: "bash(go mod download *)+net", tool: "bash", str: "bash(go mod download *) +net", net: true},
		// +install implies +net: a package manager that cannot reach its
		// index is a grant that cannot do its job.
		{text: "bash(brew install *) +install", tool: "bash", str: "bash(brew install *) +install", net: true, install: true},
		{text: "bash(brew install *)+install", tool: "bash", str: "bash(brew install *) +install", net: true, install: true},
		{text: "bash(brew install *) +net +install", tool: "bash", str: "bash(brew install *) +net +install", net: true, install: true},
		{text: "bash(brew install *) +install +net", tool: "bash", str: "bash(brew install *) +net +install", net: true, install: true},
		{text: "read_file(**) +install", wantErr: true},
		{text: "web_fetch +install", wantErr: true},
		{text: "bash(re:^go (test|build) )", tool: "bash", str: "bash(re:^go (test|build) )"},
		{text: "bash(re:()", wantErr: true},
		{text: "bash(*)", wantErr: true},
		{text: "bash(git * status)", wantErr: true},
		{text: "read_file($WORKSPACE/**)", tool: "read_file", str: "read_file($WORKSPACE/**)"},
		{text: "read_file(~/notes/**)", tool: "read_file", str: "read_file(~/notes/**)"},
		{text: "write_file(internal/**/*.go)", tool: "write_file", str: "write_file(internal/**/*.go)"},
		{text: "read_file([)", wantErr: true},
		{text: "read_file(**) +net", wantErr: true},
		{text: "web_fetch", tool: "web_fetch", str: "web_fetch"},
		{text: "web_fetch(domain:example.com)", tool: "web_fetch", str: "web_fetch(domain:example.com)"},
		{text: "web_fetch(*.example.com)", tool: "web_fetch", str: "web_fetch(domain:*.example.com)"},
		{text: "web_fetch(domain:)", wantErr: true},
		{text: "web_fetch(domain:a b)", wantErr: true},
		{text: "mcp:*", tool: "mcp:*", str: "mcp:*"},
		{text: "mcp:github", tool: "mcp:github", str: "mcp:github"},
		{text: "mcp:github:create_*", tool: "mcp:github:create_*", str: "mcp:github:create_*"},
		{text: "mcp:", wantErr: true},
		{text: "mcp:github(x)", wantErr: true},
		{text: "*", tool: "*", str: "*"},
		{text: "*(**/.env)", tool: "*", str: "*(**/.env)"},
		{text: "", wantErr: true},
		{text: "bash(", wantErr: true},
		{text: "bash()", wantErr: true},
		{text: "bash go", wantErr: true},
		{text: "Bash(ls)", wantErr: true},
		{text: "(ls)", wantErr: true},
		{text: "tool-name", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			r, err := policy.ParseRule(tt.text, policy.SourceUser)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %s", r)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Tool != tt.tool || r.String() != tt.str || r.Net() != tt.net || r.Installs() != tt.install || r.Source != policy.SourceUser {
				t.Errorf("got tool=%q str=%q net=%v install=%v src=%q", r.Tool, r, r.Net(), r.Installs(), r.Source)
			}
			again, err := policy.ParseRule(r.String(), policy.SourceUser)
			if err != nil || again.String() != r.String() {
				t.Errorf("String() does not round-trip: %q → %q (%v)", r, again, err)
			}
		})
	}
}

func TestParseRulesRefusesStarInAllow(t *testing.T) {
	if _, err := policy.ParseRules([]string{"bash", "*"}, policy.Allow, policy.SourceUser); err == nil {
		t.Error("bare * must be refused in allow")
	}
	if _, err := policy.ParseRules([]string{"*(**/*.go)"}, policy.Allow, policy.SourceUser); err == nil {
		t.Error("*(pattern) must be refused in allow")
	}
	rules, err := policy.ParseRules([]string{"*", "*(**/.env)"}, policy.Deny, policy.SourceProject)
	if err != nil || len(rules) != 2 || rules[0].Decision != policy.Deny || rules[1].Source != policy.SourceProject {
		t.Errorf("deny list: %v %v", rules, err)
	}
	if _, err := policy.ParseRules([]string{"bash(("}, policy.Ask, policy.SourceUser); err == nil {
		t.Error("bad rule in list must error")
	}
}

func TestRuleMatching(t *testing.T) {
	mustRule := func(s string) policy.Rule {
		t.Helper()
		r, err := policy.ParseRule(s, policy.SourceUser)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	t.Run("command", func(t *testing.T) {
		tests := []struct {
			rule string
			argv []string
			want bool
		}{
			{"bash(go test *)", []string{"go", "test", "./..."}, true},
			{"bash(go test *)", []string{"go", "test"}, true},
			{"bash(go test *)", []string{"go", "testx"}, false},
			{"bash(go test *)", []string{"go"}, false},
			{"bash(git remote -v)", []string{"git", "remote", "-v"}, true},
			{"bash(git remote -v)", []string{"git", "remote", "-v", "x"}, false},
			{"bash(re:^go (test|vet))", []string{"go", "vet", "./..."}, true},
			{"bash(re:^go (test|vet))", []string{"go", "build"}, false},
			{"bash", []string{"anything"}, true},
			{"read_file(x)", []string{"cat"}, false},
		}
		for _, tt := range tests {
			if got := mustRule(tt.rule).MatchesCommand(tt.argv); got != tt.want {
				t.Errorf("%s vs %v = %v, want %v", tt.rule, tt.argv, got, tt.want)
			}
		}
	})
	t.Run("path", func(t *testing.T) {
		const home, root = "/home/u", "/w"
		tests := []struct {
			rule string
			abs  string
			want bool
		}{
			{"read_file($WORKSPACE/**)", "/w/a/b.go", true},
			{"read_file($WORKSPACE/**)", "/w", true},
			{"read_file($WORKSPACE/**)", "/w2/a", false},
			{"read_file(internal/**)", "/w/internal/x.go", true},
			{"read_file(internal/**)", "/w/cmd/x.go", false},
			{"read_file(~/.ssh/**)", "/home/u/.ssh/id_rsa", true},
			{"read_file(~/.ssh/**)", "/home/u/.sshx", false},
			{"*(**/.env)", "/w/.env", true},
			{"*(**/.env)", "/w/deep/.env", true},
			{"*(**/.env)", "/w/.env.example", false},
			{"*(/etc/**)", "/etc/passwd", true},
			{"bash(ls *)", "/w/x", false},
		}
		for _, tt := range tests {
			if got := mustRule(tt.rule).MatchesPath(tt.abs, home, root); got != tt.want {
				t.Errorf("%s vs %s = %v, want %v", tt.rule, tt.abs, got, tt.want)
			}
		}
	})
	t.Run("host", func(t *testing.T) {
		tests := []struct {
			rule string
			host string
			want bool
		}{
			{"web_fetch(domain:example.com)", "example.com", true},
			{"web_fetch(domain:example.com)", "EXAMPLE.com", true},
			{"web_fetch(domain:example.com)", "api.example.com", false},
			{"web_fetch(*.example.com)", "api.example.com", true},
			{"web_fetch(*.example.com)", "example.com", true},
			{"web_fetch(*.example.com)", "notexample.com", false},
			{"web_fetch(domain:127.*)", "127.0.0.1", true},
			{"web_fetch(domain:127.*)", "128.0.0.1", false},
			{"web_fetch", "anything", true},
		}
		for _, tt := range tests {
			if got := mustRule(tt.rule).MatchesHost(tt.host); got != tt.want {
				t.Errorf("%s vs %s = %v, want %v", tt.rule, tt.host, got, tt.want)
			}
		}
	})
	t.Run("tool", func(t *testing.T) {
		tests := []struct {
			rule string
			tool string
			want bool
		}{
			{"bash", "bash", true},
			{"bash", "read_file", false},
			{"*", "anything", true},
			{"*(**/x)", "mcp:a:b", true},
			{"mcp:*", "mcp:github:create_issue", true},
			{"mcp:*", "bash", false},
			{"mcp:github", "mcp:github:create_issue", true},
			{"mcp:github", "mcp:gitlab:create_issue", false},
			{"mcp:github:create_*", "mcp:github:create_issue", true},
			{"mcp:github:create_*", "mcp:github:delete_issue", false},
		}
		for _, tt := range tests {
			if got := mustRule(tt.rule).MatchesTool(tt.tool); got != tt.want {
				t.Errorf("%s vs %s = %v, want %v", tt.rule, tt.tool, got, tt.want)
			}
		}
	})
}

func TestBuiltinMatchesConfigDefaults(t *testing.T) {
	allow, ask, deny := policy.BuiltinTexts()
	d := config.Defaults()
	if !slices.Equal(allow, d.Permissions.Allow) {
		t.Errorf("builtin allow differs from defaults.json:\n%v\n%v", allow, d.Permissions.Allow)
	}
	if !slices.Equal(ask, d.Permissions.Ask) {
		t.Errorf("builtin ask differs from defaults.json:\n%v\n%v", ask, d.Permissions.Ask)
	}
	if !slices.Equal(deny, d.Permissions.Deny) {
		t.Errorf("builtin deny differs from defaults.json:\n%v\n%v", deny, d.Permissions.Deny)
	}
	rules := policy.Builtin()
	if len(rules) != len(allow)+len(ask)+len(deny) {
		t.Errorf("Builtin() parsed %d rules, want %d", len(rules), len(allow)+len(ask)+len(deny))
	}
	for _, r := range rules {
		if r.Source != policy.SourceBuiltin {
			t.Errorf("rule %s has source %q", r, r.Source)
		}
	}
}

func TestParseMode(t *testing.T) {
	tests := []struct {
		in      string
		want    policy.Mode
		wantErr bool
	}{
		{"", policy.ModeDefault, false},
		{"default", policy.ModeDefault, false},
		{"Plan", policy.ModePlan, false},
		{"auto-edit", policy.ModeAutoEdit, false},
		{"autoedit", policy.ModeAutoEdit, false},
		{"bypass", policy.ModeBypass, false},
		{"yolo", policy.ModeDefault, true},
	}
	for _, tt := range tests {
		got, err := policy.ParseMode(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseMode(%q) = %v, %v", tt.in, got, err)
		}
	}
	if policy.ModeAutoEdit.String() != "auto-edit" || policy.Deny.String() != "deny" || policy.ScopeProjectLocal.String() != "project" {
		t.Error("String() names mismatch")
	}
}

func FuzzParseRule(f *testing.F) {
	for _, s := range []string{"bash", "bash(go test *)", "bash(re:^x)", "read_file(~/**)", "web_fetch(*.x.com)", "mcp:a:b*", "*(**/.env)", "bash(x) +net", "bash(x) +install", "(", ")", "mcp:", "a(b)c"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		r, err := policy.ParseRule(text, policy.SourceUser)
		if err != nil {
			return
		}
		s := r.String()
		again, err := policy.ParseRule(s, policy.SourceUser)
		if err != nil {
			t.Fatalf("String() of %q = %q does not re-parse: %v", text, s, err)
		}
		if again.String() != s {
			t.Fatalf("not idempotent: %q → %q → %q", text, s, again.String())
		}
		// Matching must never panic on arbitrary inputs.
		_ = r.MatchesTool(text)
		_ = r.MatchesPath("/"+strings.TrimLeft(text, "/"), "/home/u", "/w")
		_ = r.MatchesCommand(strings.Fields(text))
		_ = r.MatchesHost(text)
	})
}
