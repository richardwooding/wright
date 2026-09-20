package tools

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The bash timeout is stated in three places the model reads — the tool
// description, the Docs() line that reaches the system prompt, and the
// JSON-schema description of the timeout argument — and enforced in a fourth.
// Two of those are derived from the constants; the schema description is a
// struct tag, which cannot be, so it is pinned here. A model that does not
// know the bound exists writes `timeout 120 …` itself, which is how a rule
// came to be offered for the wrapper instead of the program.
func TestBashTimeoutIsStatedWhereverTheModelReads(t *testing.T) {
	def := strconv.Itoa(int(defaultBashTimeout.Seconds()))
	maxs := strconv.Itoa(int(maxBashTimeout.Seconds()))

	for _, n := range []string{def, maxs} {
		if !strings.Contains(bashTimeoutNote, n) {
			t.Errorf("the note does not state %s seconds: %q", n, bashTimeoutNote)
		}
	}
	if !strings.Contains(bashTimeoutNote, "timeout") {
		t.Errorf("the note does not tell the model not to wrap commands: %q", bashTimeoutNote)
	}

	var docs string
	for _, d := range Docs() {
		if d.Name == NameBash {
			docs = d.When
		}
	}
	if !strings.Contains(docs, bashTimeoutNote) {
		t.Errorf("the system-prompt guidance for bash does not carry the note:\n%s", docs)
	}

	f, ok := reflect.TypeFor[bashArgs]().FieldByName("Timeout")
	if !ok {
		t.Fatal("bashArgs has no Timeout field")
	}
	tag := f.Tag.Get("jsonschema")
	for _, n := range []string{def, maxs} {
		if !strings.Contains(tag, n) {
			t.Errorf("the timeout argument's schema says %q, which does not state %s seconds", tag, n)
		}
	}
}

// TestGitSandboxNoteReachesTheModel pins the guidance that would have saved a
// real session two wrong turns: `git remote add` failing on a read-only
// .git/config, and a hand-rolled -c credential.helper that wright had already
// configured and the policy engine refuses.
func TestGitSandboxNoteReachesTheModel(t *testing.T) {
	var docs string
	for _, d := range Docs() {
		if d.Name == NameBash {
			docs = d.When
		}
	}
	if docs == "" {
		t.Fatal("bash has no system-prompt guidance at all")
	}
	for _, want := range []string{
		".git/config",    // the file that cannot be written
		"read-only",      // and why it fails
		"git push <url>", // the way round it that needs no remote
		"credential.helper",
	} {
		if !strings.Contains(docs, want) {
			t.Errorf("the guidance does not mention %q:\n%s", want, docs)
		}
	}
	// The paths named here are the ones the sandbox actually protects, and
	// the ones the failure note recognises. If either list moves, this says
	// so rather than leaving the model with stale advice.
	for _, p := range protectedNames {
		if p == ".git/config.worktree" {
			continue // the rare spelling; the note names the common ones
		}
		if !strings.Contains(docs, p) {
			t.Errorf("the sandbox protects %s but the guidance does not mention it", p)
		}
	}
}
