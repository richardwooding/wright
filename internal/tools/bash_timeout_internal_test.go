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
