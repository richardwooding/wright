package tools_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/tools"
)

const multiSrcA = "package main\n\nfunc a() {}\n\nfunc b() {}\n\nfunc b() {}\n"

const multiSrcB = "package other\n\nfunc a() {}\n"

// describeCall returns the policy request as well as the preview, which
// f.describe drops.
func describeCall(t *testing.T, f *fixture, name, args string) (policy.Request, tools.Preview, error) {
	t.Helper()
	d, ok := tools.Lookup(f.ts, name)
	if !ok {
		t.Fatalf("tool %s has no Describer", name)
	}
	return d.Describe(json.RawMessage(args))
}

// edit builds one entry of a multi_edit call.
func edit(path, old, new string, all ...bool) map[string]any {
	m := map[string]any{"path": path, "old_string": old, "new_string": new}
	if len(all) > 0 && all[0] {
		m["replace_all"] = true
	}
	return m
}

func TestMultiEdit(t *testing.T) {
	tests := []struct {
		name    string
		edits   []map[string]any
		wantA   string // "" means unchanged
		wantB   string
		want    string
		wantErr string
	}{
		{
			name:  "several files in one call",
			edits: []map[string]any{edit("a.go", "func a() {}", "func a() int { return 1 }"), edit("b.go", "package other", "package renamed")},
			wantA: strings.Replace(multiSrcA, "func a() {}", "func a() int { return 1 }", 1),
			wantB: strings.Replace(multiSrcB, "package other", "package renamed", 1),
			want:  "Applied 2 edits (2 replacements) in 2 files: a.go, b.go",
		},
		{
			// The second edit's old_string exists only after the first has
			// been applied, so this fails unless each edit sees the last
			// one's result rather than the bytes on disk.
			name:  "a later edit sees an earlier one's result",
			edits: []map[string]any{edit("a.go", "func a() {}", "func renamed() {}"), edit("a.go", "func renamed() {}", "func final() {}")},
			wantA: strings.Replace(multiSrcA, "func a() {}", "func final() {}", 1),
			want:  "Applied 2 edits (2 replacements) in 1 file: a.go",
		},
		{
			name:  "replace_all counts every replacement",
			edits: []map[string]any{edit("a.go", "func b() {}", "func c() {}", true)},
			wantA: strings.ReplaceAll(multiSrcA, "func b() {}", "func c() {}"),
			want:  "Applied 1 edit (2 replacements) in 1 file: a.go",
		},
		{
			// The failing edit is the last one and targets a file the earlier
			// edits already changed in memory: nothing may reach the disk.
			name:    "one bad edit abandons the whole call",
			edits:   []map[string]any{edit("a.go", "func a() {}", "func x() {}"), edit("b.go", "package other", "package renamed"), edit("a.go", "nowhere", "x")},
			wantErr: "edit 3: old_string not found in a.go",
		},
		{
			name:    "an ambiguous edit names its lines",
			edits:   []map[string]any{edit("a.go", "func b() {}", "x")},
			wantErr: "edit 1: old_string occurs 2 times in a.go (lines 5, 7)",
		},
		{
			name:    "a missing file stops the call",
			edits:   []map[string]any{edit("a.go", "func a() {}", "func x() {}"), edit("none.go", "a", "b")},
			wantErr: "edit 2:",
		},
		{
			name:    "a credential file is refused",
			edits:   []map[string]any{edit("id_rsa", "a", "b")},
			wantErr: "credential file",
		},
		{
			name:    "no edits",
			edits:   []map[string]any{},
			wantErr: "must not be empty",
		},
		{
			// Two edits that undo each other leave the file byte-identical;
			// reporting a write that changed nothing would be a lie.
			name:    "a call that changes nothing is refused",
			edits:   []map[string]any{edit("a.go", "func a() {}", "func x() {}"), edit("a.go", "func x() {}", "func a() {}")},
			wantErr: "a.go would be unchanged",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.write("a.go", multiSrcA)
			f.write("b.go", multiSrcB)
			got, err := f.text(tools.NameMultiEdit, jsonArgs(map[string]any{"edits": tt.edits}))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				// Atomicity: a call that failed wrote nothing at all.
				if f.read("a.go") != multiSrcA || f.read("b.go") != multiSrcB {
					t.Error("a failed multi_edit left changes on disk")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			wantA, wantB := tt.wantA, tt.wantB
			if wantA == "" {
				wantA = multiSrcA
			}
			if wantB == "" {
				wantB = multiSrcB
			}
			if a := f.read("a.go"); a != wantA {
				t.Errorf("a.go =\n%s\nwant\n%s", a, wantA)
			}
			if b := f.read("b.go"); b != wantB {
				t.Errorf("b.go =\n%s\nwant\n%s", b, wantB)
			}
		})
	}
}

// TestMultiEditTooManyEdits pins the cap. The limit exists so the approval
// prompt stays readable, so exceeding it must be an error and not a silent
// truncation.
func TestMultiEditTooManyEdits(t *testing.T) {
	f := newFixture(t, nil)
	f.write("a.go", multiSrcA)
	edits := make([]map[string]any, 51)
	for i := range edits {
		edits[i] = edit("a.go", "func a() {}", "func x() {}")
	}
	_, err := f.text(tools.NameMultiEdit, jsonArgs(map[string]any{"edits": edits}))
	if err == nil || !strings.Contains(err.Error(), "51 edits is more than the 50") {
		t.Fatalf("err = %v, want the cap", err)
	}
	if f.read("a.go") != multiSrcA {
		t.Error("an over-long call still wrote")
	}
}

// TestMultiEditDescribe pins what the policy engine and the approval prompt
// see: every file the call writes, and a diff of the whole call rather than a
// count of it.
func TestMultiEditDescribe(t *testing.T) {
	f := newFixture(t, nil)
	f.write("a.go", multiSrcA)
	f.write("b.go", multiSrcB)
	args := jsonArgs(map[string]any{"edits": []map[string]any{
		edit("a.go", "func a() {}", "func x() {}"),
		edit("b.go", "package other", "package renamed"),
	}})

	req, _, err := describeCall(t, f, tools.NameMultiEdit, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Writes) != 2 || len(req.Paths) != 2 {
		t.Errorf("request writes %v paths %v, want both files", req.Writes, req.Paths)
	}

	p, err := f.describe(tools.NameMultiEdit, args)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a/a.go", "a/b.go", "func x() {}", "package renamed"} {
		if !strings.Contains(p.Diff, want) {
			t.Errorf("diff does not mention %q:\n%s", want, p.Diff)
		}
	}
	if !strings.Contains(p.Title, "2 files") {
		t.Errorf("title = %q", p.Title)
	}
}

// TestMultiEditDescribesAnUnplannableCall pins that a call which cannot be
// planned still names the files it would write. A request with no Writes
// would be evaluated as if it touched nothing, so a tool whose arguments are
// wrong must not also be a tool that skipped the permission check.
func TestMultiEditDescribesAnUnplannableCall(t *testing.T) {
	f := newFixture(t, nil)
	f.write("a.go", multiSrcA)
	args := jsonArgs(map[string]any{"edits": []map[string]any{edit("a.go", "nowhere", "x")}})
	req, _, err := describeCall(t, f, tools.NameMultiEdit, args)
	if err == nil {
		t.Fatal("expected the unplannable edit to error")
	}
	if len(req.Writes) != 1 {
		t.Errorf("writes = %v, want a.go even though the call cannot be planned", req.Writes)
	}
}

// TestMultiEditRollsBackAPartialWrite covers the one failure the plan cannot
// prevent: every edit validated, but a write failing part-way through.
// Renaming N files is not one atomic operation, so the tool has to put back
// what it already wrote — otherwise "all or none" holds only for the easy
// case.
func TestMultiEditRollsBackAPartialWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only bit that makes the second write fail")
	}
	f := newFixture(t, nil)
	f.write("a.go", multiSrcA)
	b := f.write("b.go", multiSrcB)
	if err := os.Chmod(b, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(b, 0o644) })

	_, err := f.text(tools.NameMultiEdit, jsonArgs(map[string]any{"edits": []map[string]any{
		edit("a.go", "func a() {}", "func x() {}"),
		edit("b.go", "package other", "package renamed"),
	}}))
	if err == nil {
		t.Fatal("expected the read-only file to fail the call")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("err = %v, want it to say what it put back", err)
	}
	if got := f.read("a.go"); got != multiSrcA {
		t.Errorf("a.go was left changed by a failed call:\n%s", got)
	}
	if got := f.read("b.go"); got != multiSrcB {
		t.Errorf("b.go =\n%s\nwant unchanged", got)
	}
}
