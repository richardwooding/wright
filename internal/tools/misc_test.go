package tools_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/tools"
)

func TestTodoWrite(t *testing.T) {
	list := &tools.TodoList{}
	f := newFixture(t, func(d *tools.Deps) { d.Todos = list })
	got, err := f.text(tools.NameTodoWrite, `{"todos":[{"id":"1","content":"a","status":"completed"},{"id":"2","content":"b","status":"in_progress"},{"id":"3","content":"c","status":"pending"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "3 todos: 1 pending, 1 in progress, 1 completed" {
		t.Errorf("summary = %q", got)
	}
	snap := list.Snapshot()
	if len(snap) != 3 || snap[1].Content != "b" {
		t.Errorf("snapshot = %+v", snap)
	}
	snap[0].Content = "mutated"
	if list.Snapshot()[0].Content == "mutated" {
		t.Error("Snapshot must copy")
	}
	for name, args := range map[string]string{
		"bad status":   `{"todos":[{"id":"1","content":"a","status":"done"}]}`,
		"missing id":   `{"todos":[{"content":"a","status":"pending"}]}`,
		"duplicate id": `{"todos":[{"id":"1","content":"a","status":"pending"},{"id":"1","content":"b","status":"pending"}]}`,
		"no content":   `{"todos":[{"id":"1","status":"pending"}]}`,
	} {
		if _, err := f.text(tools.NameTodoWrite, args); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if len(list.Snapshot()) != 3 {
		t.Error("invalid write changed the list")
	}
	if got, _ := f.text(tools.NameTodoWrite, `{"todos":[]}`); got != "0 todos: 0 pending, 0 in progress, 0 completed" {
		t.Errorf("empty = %q", got)
	}
}

type fakeAsker struct {
	got tools.Question
	ans tools.Answer
	err error
}

func (a *fakeAsker) Ask(_ context.Context, q tools.Question) (tools.Answer, error) {
	a.got = q
	return a.ans, a.err
}

func TestAskUser(t *testing.T) {
	asker := &fakeAsker{ans: tools.Answer{Index: 1}}
	f := newFixture(t, func(d *tools.Deps) { d.Asker = asker })
	got, err := f.text(tools.NameAskUser, `{"question":"Which?","options":["a","b"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "b" || asker.got.FreeText || asker.got.Text != "Which?" {
		t.Errorf("got %q, question %+v", got, asker.got)
	}
	asker.ans = tools.Answer{Text: "free form", Index: -1}
	got, err = f.text(tools.NameAskUser, `{"question":"Why?"}`)
	if err != nil || got != "free form" || !asker.got.FreeText {
		t.Errorf("got %q %v, question %+v", got, err, asker.got)
	}
	asker.err = errors.New("declined")
	if _, err := f.text(tools.NameAskUser, `{"question":"?"}`); err == nil {
		t.Error("asker error not surfaced")
	}
	if _, err := f.text(tools.NameAskUser, `{"question":""}`); err == nil {
		t.Error("empty question accepted")
	}
	d, _ := tools.Lookup(f.ts, tools.NameAskUser)
	if _, p, err := d.Describe([]byte(`{"question":"Q"}`)); err != nil || p.Body != "Q" {
		t.Errorf("describe = %+v %v", p, err)
	}
}

func TestClip(t *testing.T) {
	dir := t.TempDir()
	short := "abc"
	if got, spilled := tools.Clip(short, 10, dir, "id"); got != short || spilled {
		t.Errorf("short clipped: %q %v", got, spilled)
	}
	var b strings.Builder
	for i := range 1000 {
		b.WriteString("line-" + strings.Repeat("x", 20) + "-" + string(rune('a'+i%26)) + "\n")
	}
	long := b.String()
	got, spilled := tools.Clip(long, 3000, dir, "call/1")
	if !spilled {
		t.Fatal("expected spill")
	}
	if len(got) > 3200 {
		t.Errorf("clipped length %d", len(got))
	}
	if !strings.HasPrefix(got, "line-") || !strings.HasSuffix(got, "\n") {
		t.Errorf("head/tail not on line boundaries: %q … %q", got[:20], got[len(got)-20:])
	}
	if !strings.Contains(got, "[output truncated: "+strconv.Itoa(len(long))+" bytes; full output saved to ") {
		t.Errorf("note missing:\n%s", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out-call_1.txt"))
	if err != nil || string(data) != long {
		t.Errorf("spill file: %v", err)
	}
	got, spilled = tools.Clip(long, 3000, "", "id")
	if spilled || strings.Contains(got, "saved to") {
		t.Error("spilled without a directory")
	}
}

func TestNamesAndFilters(t *testing.T) {
	list := &tools.TodoList{}
	f := newFixture(t, func(d *tools.Deps) {
		d.Todos = list
		d.Asker = &fakeAsker{}
		d.Search = &fakeSearch{}
		d.Fetch = &http.Client{}
	})
	if got := f.ts.Names(); !slices.Equal(got, tools.Names()) {
		t.Errorf("registered %v, Names() %v", got, tools.Names())
	}
	ro := tools.ReadOnly(f.ts)
	if got := ro.Names(); !slices.Equal(got, tools.ReadOnlyNames()) {
		t.Errorf("ReadOnly = %v", got)
	}
	docs := tools.Docs()
	if len(docs) != len(tools.Names()) {
		t.Fatalf("Docs has %d entries", len(docs))
	}
	for i, d := range docs {
		if d.Name != tools.Names()[i] || d.When == "" {
			t.Errorf("doc %d = %+v", i, d)
		}
	}
	for _, name := range tools.Names() {
		if _, ok := tools.Lookup(f.ts, name); !ok {
			t.Errorf("%s has no Describer", name)
		}
	}
	if _, ok := tools.Lookup(f.ts, "nope"); ok {
		t.Error("unknown tool found")
	}
}

func TestOnRedacted(t *testing.T) {
	var seen []string
	f := newFixture(t, func(d *tools.Deps) {
		d.Redactor = redact.New()
		d.OnRedacted = func(_ context.Context, tool string, hits []redact.Hit) {
			seen = append(seen, tool+":"+hits[0].Name)
		}
	})
	f.write("t.txt", "sk-ant-"+strings.Repeat("a", 90)+"\n")
	if _, err := f.text(tools.NameReadFile, `{"path":"t.txt"}`); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "read_file:") {
		t.Errorf("OnRedacted calls = %v", seen)
	}
}
