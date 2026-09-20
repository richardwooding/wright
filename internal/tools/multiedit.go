package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/richardwooding/agentkit"

	"github.com/richardwooding/wright/internal/policy"
)

// maxMultiEdits caps one call. A refactor that touches more places than this
// is better done as several calls the user can read and approve one at a
// time, because the approval prompt has to stay readable to mean anything.
const maxMultiEdits = 50

type multiEditArgs struct {
	Edits []editArgs `json:"edits" jsonschema:"the edits to apply, in order; all of them are applied or none are"`
}

// multiEditPlan is every edit of one call, computed and validated but not yet
// written.
type multiEditPlan struct {
	files []plannedFile
	count int // replacements across every file
}

// plannedFile is one file's final content plus what it took to get there.
// Several edits may target the same file, and each sees the previous one's
// result, so a file appears once here however many edits touched it.
type plannedFile struct {
	abs    string
	mode   os.FileMode
	before string
	after  string
	edits  int
	count  int
}

func (d *Deps) multiEdit() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameMultiEdit,
			"Apply several edits in one call, in order. Each edit is an edit_file: an exact old_string, unique unless replace_all. Edits may target the same file and each sees the previous one's result. Every edit is validated first, so either all of them apply or none do. Snapshots are taken first so the change can be undone.",
			d.runMultiEdit),
		describe: d.describeMultiEdit,
	}
}

func (d *Deps) describeMultiEdit(args json.RawMessage) (policy.Request, Preview, error) {
	var a multiEditArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	paths, err := d.multiEditPaths(a)
	if err != nil {
		return policy.Request{}, Preview{}, err
	}
	// The request carries every path before the plan is computed, so a call
	// that cannot be planned is still evaluated against the files it names
	// rather than escaping the policy engine on its way to an error.
	req := policy.Request{Tool: NameMultiEdit, Args: args, Paths: paths, Writes: paths}
	plan, err := d.planMultiEdit(a)
	if err != nil {
		return req, Preview{}, err
	}
	return req, Preview{Title: plan.title(), Diff: plan.diff(d)}, nil
}

func (d *Deps) runMultiEdit(ctx context.Context, a multiEditArgs) (agentkit.Output, error) {
	paths, err := d.multiEditPaths(a)
	if err != nil {
		return agentkit.Output{}, err
	}
	for i, abs := range paths {
		_, inside, err := d.resolve(a.Edits[i].Path)
		if err != nil {
			return agentkit.Output{}, err
		}
		if err := d.refuseWrite(abs, inside); err != nil {
			return agentkit.Output{}, err
		}
	}
	plan, err := d.planMultiEdit(a)
	if err != nil {
		return agentkit.Output{}, err
	}
	for _, f := range plan.files {
		if err := d.snapshot(ctx, f.abs); err != nil {
			return agentkit.Output{}, err
		}
	}
	if err := writeAll(plan.files); err != nil {
		return agentkit.Output{}, err
	}
	return agentkit.Text(plan.summary(d)), nil
}

// multiEditPaths resolves every edit's target, in order, with one resolved
// path per edit (so an edit and its path share an index).
func (d *Deps) multiEditPaths(a multiEditArgs) ([]string, error) {
	switch {
	case len(a.Edits) == 0:
		return nil, errors.New("edits must not be empty; use edit_file for a single edit")
	case len(a.Edits) > maxMultiEdits:
		return nil, fmt.Errorf("%d edits is more than the %d one call may carry; split them up", len(a.Edits), maxMultiEdits)
	}
	paths := make([]string, len(a.Edits))
	for i, e := range a.Edits {
		abs, _, err := d.resolve(e.Path)
		if err != nil {
			return nil, fmt.Errorf("edit %d: %w", i+1, err)
		}
		paths[i] = abs
	}
	return paths, nil
}

// planMultiEdit computes every edit against the content the edits before it
// produced, without touching the disk. Validating the whole call before any
// of it is written is what makes the tool atomic: the common failure — an
// old_string that does not match, or matches twice — is found while nothing
// has changed yet.
func (d *Deps) planMultiEdit(a multiEditArgs) (multiEditPlan, error) {
	paths, err := d.multiEditPaths(a)
	if err != nil {
		return multiEditPlan{}, err
	}
	var plan multiEditPlan
	for i, e := range a.Edits {
		abs := paths[i]
		f := plan.find(abs)
		if f == nil {
			info, err := os.Stat(abs)
			if err != nil {
				return multiEditPlan{}, fmt.Errorf("edit %d: %w", i+1, err)
			}
			if info.IsDir() {
				return multiEditPlan{}, fmt.Errorf("edit %d: %s is a directory", i+1, d.rel(abs))
			}
			raw, err := os.ReadFile(abs)
			if err != nil {
				return multiEditPlan{}, fmt.Errorf("edit %d: %w", i+1, err)
			}
			plan.files = append(plan.files, plannedFile{abs: abs, mode: info.Mode().Perm(), before: string(raw), after: string(raw)})
			f = &plan.files[len(plan.files)-1]
		}
		after, lines, err := d.applyOne(f.after, abs, e)
		if err != nil {
			return multiEditPlan{}, fmt.Errorf("edit %d: %w", i+1, err)
		}
		f.after, f.edits, f.count = after, f.edits+1, f.count+len(lines)
		plan.count += len(lines)
	}
	for _, f := range plan.files {
		if f.after == f.before {
			return multiEditPlan{}, fmt.Errorf("%s would be unchanged", d.rel(f.abs))
		}
	}
	return plan, nil
}

// applyOne is computeEdit's validation against an in-memory buffer rather
// than the file, so an edit later in the call is checked against what the
// edits before it produced instead of against stale bytes on disk.
func (d *Deps) applyOne(before, abs string, a editArgs) (after string, lines []int, err error) {
	switch a.OldString {
	case "":
		return "", nil, errors.New("old_string must not be empty; use write_file to create content")
	case a.NewString:
		return "", nil, errors.New("old_string and new_string are identical")
	}
	count := strings.Count(before, a.OldString)
	switch {
	case count == 0:
		return "", nil, fmt.Errorf("old_string not found in %s%s", d.rel(abs), closestLine(before, a.OldString))
	case count > 1 && !a.ReplaceAll:
		return "", nil, fmt.Errorf("old_string occurs %d times in %s (lines %s); add context to make it unique or set replace_all",
			count, d.rel(abs), joinInts(occurrenceLines(before, a.OldString)))
	}
	after, lines = replace(before, a.OldString, a.NewString, a.ReplaceAll)
	return after, lines, nil
}

// find returns the file already planned for abs, or nil.
func (p *multiEditPlan) find(abs string) *plannedFile {
	for i := range p.files {
		if p.files[i].abs == abs {
			return &p.files[i]
		}
	}
	return nil
}

func (p multiEditPlan) title() string {
	return fmt.Sprintf("%s %s across %s", NameMultiEdit,
		countOf(p.count, "replacement"), countOf(len(p.files), "file"))
}

// diff renders every file's change as one unified diff, so the approval
// prompt shows the whole call rather than a summary of it.
func (p multiEditPlan) diff(d *Deps) string {
	parts := make([]string, 0, len(p.files))
	for _, f := range p.files {
		rel := d.rel(f.abs)
		parts = append(parts, unified(rel, f.before, f.after))
	}
	return strings.Join(parts, "\n")
}

func (p multiEditPlan) summary(d *Deps) string {
	names := make([]string, 0, len(p.files))
	for _, f := range p.files {
		names = append(names, d.rel(f.abs))
	}
	return fmt.Sprintf("Applied %s (%s) in %s: %s",
		countOf(p.countEdits(), "edit"), countOf(p.count, "replacement"),
		countOf(len(p.files), "file"), strings.Join(names, ", "))
}

// countEdits is how many edits the call carried, which is not the number of
// replacements: one replace_all edit can make many.
func (p multiEditPlan) countEdits() int {
	n := 0
	for _, f := range p.files {
		n += f.edits
	}
	return n
}

// writeAll writes every planned file, and puts back what it already wrote if
// one of them fails. Renaming N files is not one atomic operation, so a
// failure part-way through — a disk filling up, a file turned read-only
// between the plan and the write — would otherwise leave the call half
// applied, which is the state the tool exists to avoid. The snapshots taken
// beforehand are the backstop if even the rollback cannot be written.
func writeAll(files []plannedFile) error {
	done := make([]plannedFile, 0, len(files))
	for _, f := range files {
		if err := os.WriteFile(f.abs, []byte(f.after), f.mode); err != nil {
			return fmt.Errorf("%w%s", err, rollback(done))
		}
		done = append(done, f)
	}
	return nil
}

// rollback restores the files already written, and describes what it could
// not put back. The message matters more than the error: a half-applied call
// the user is not told about is worse than one that failed loudly.
func rollback(done []plannedFile) string {
	var failed []string
	for _, f := range slices.Backward(done) {
		if err := os.WriteFile(f.abs, []byte(f.before), f.mode); err != nil {
			failed = append(failed, f.abs)
		}
	}
	switch {
	case len(done) == 0:
		return " (nothing had been written yet)"
	case len(failed) > 0:
		return fmt.Sprintf(" (could not restore %s; use /undo)", strings.Join(failed, ", "))
	}
	return fmt.Sprintf(" (rolled back %s)", countOf(len(done), "file"))
}

// countOf renders "1 file" / "3 files".
func countOf(n int, noun string) string {
	return fmt.Sprintf("%d %s%s", n, noun, plural(n))
}
