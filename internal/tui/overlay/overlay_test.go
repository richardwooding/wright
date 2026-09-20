package overlay_test

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/richardwooding/wright/internal/engine"
	"github.com/richardwooding/wright/internal/policy"
	"github.com/richardwooding/wright/internal/policy/shellclass"
	"github.com/richardwooding/wright/internal/theme"
	"github.com/richardwooding/wright/internal/tui/overlay"
)

var th = theme.New(true)

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "ctrl+s":
		return tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	case "ctrl+a":
		return tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl}
	case "ctrl+k":
		return tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}
	}
	r := []rune(s)
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

func press(o overlay.Overlay, keys ...string) (overlay.Overlay, bool) {
	var done bool
	for _, k := range keys {
		o, _, done = o.Update(key(k))
	}
	return o, done
}

func typeText(o overlay.Overlay, s string) overlay.Overlay {
	for _, r := range s {
		o, _, _ = o.Update(key(string(r)))
	}
	return o
}

func plain(s string) string { return ansi.Strip(s) }

func offer(t *testing.T, rule string, scope policy.Scope) policy.GrantOffer {
	t.Helper()
	r, err := policy.ParseRule(rule, policy.SourceSession)
	if err != nil {
		t.Fatal(err)
	}
	return policy.GrantOffer{Rule: r, Scope: scope, Label: "allow " + rule}
}

func approval(t *testing.T, sev engine.Severity, offers ...policy.GrantOffer) engine.Approval {
	t.Helper()
	return engine.Approval{
		ID:   "ap-1",
		Tool: "edit_file",
		Args: json.RawMessage(`{"path":"internal/x.go","content":"new"}`),
		Request: policy.Request{
			Tool:   "edit_file",
			Paths:  []string{"/ws/internal/x.go"},
			Writes: []string{"/ws/internal/x.go"},
		},
		Verdict: policy.Verdict{
			Decision: policy.Ask,
			Reason:   "writes inside the workspace ask in default mode",
			Explain:  []string{"hard-deny: no match", "rules: no match", "mode default: edit_file → ask"},
		},
		Preview:  engine.Preview{Title: "internal/x.go", Diff: "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n"},
		Offers:   offers,
		Severity: sev,
	}
}

func TestApprovalAllowOnce(t *testing.T) {
	var got *engine.Decision
	var o overlay.Overlay = overlay.NewApproval(approval(t, engine.SeverityCaution), th, func(d engine.Decision) { got = &d })
	view := plain(o.View(80, 30))
	for _, want := range []string{"⚠ caution", "edit_file", "internal/x.go", "+new", "-old", "[y] allow once", "[n] deny", "[e] edit arguments", "[x] explain"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q in view:\n%s", want, view)
		}
	}
	if strings.Contains(view, "[a] allow") {
		t.Error("allow… offered with no offers")
	}
	if strings.Contains(view, "[w] allow with network") {
		t.Error("network option offered for an edit")
	}
	_, done := press(o, "y")
	if !done || got == nil || !got.Allow || got.By != "user" || len(got.Grants) != 0 {
		t.Fatalf("done=%v decision=%+v", done, got)
	}
}

func TestApprovalDenyKeys(t *testing.T) {
	for _, k := range []string{"n", "esc"} {
		var got *engine.Decision
		var o overlay.Overlay = overlay.NewApproval(approval(t, engine.SeverityInfo), th, func(d engine.Decision) { got = &d })
		_, done := press(o, k)
		if !done || got == nil || got.Allow || got.Reason == "" {
			t.Errorf("%s: done=%v decision=%+v", k, done, got)
		}
	}
}

func TestApprovalDestructiveFocusesDeny(t *testing.T) {
	var got *engine.Decision
	var o overlay.Overlay = overlay.NewApproval(approval(t, engine.SeverityDestructive), th, func(d engine.Decision) { got = &d })
	view := plain(o.View(80, 30))
	if !strings.Contains(view, "⛔ destructive") {
		t.Fatalf("severity not shown:\n%s", view)
	}
	if !strings.Contains(view, "› [n] deny") {
		t.Fatalf("deny not focused:\n%s", view)
	}
	_, done := press(o, "enter")
	if !done || got == nil || got.Allow {
		t.Fatalf("enter on destructive allowed: %+v", got)
	}
}

func TestApprovalInfoFocusesAllowAndArrowsMove(t *testing.T) {
	var got *engine.Decision
	var o overlay.Overlay = overlay.NewApproval(approval(t, engine.SeverityInfo), th, func(d engine.Decision) { got = &d })
	if v := plain(o.View(80, 30)); !strings.Contains(v, "› [y] allow once") {
		t.Fatalf("allow not focused:\n%s", v)
	}
	o, _ = press(o, "down")
	if v := plain(o.View(80, 30)); !strings.Contains(v, "› [e] edit arguments") {
		t.Fatalf("down did not move focus:\n%s", v)
	}
	_, done := press(o, "up", "enter")
	if !done || got == nil || !got.Allow {
		t.Fatalf("enter did not allow: %+v", got)
	}
}

func TestApprovalGrantList(t *testing.T) {
	var got *engine.Decision
	offers := []policy.GrantOffer{offer(t, "edit_file(internal/**)", policy.ScopeSession), offer(t, "edit_file", policy.ScopeProjectLocal)}
	var o overlay.Overlay = overlay.NewApproval(approval(t, engine.SeverityCaution, offers...), th, func(d engine.Decision) { got = &d })
	o, done := press(o, "a")
	if done {
		t.Fatal("a closed the prompt")
	}
	view := plain(o.View(80, 30))
	for _, want := range []string{"allow…", "[1]", "rule edit_file(internal/**) · scope session", "[2]", "rule edit_file · scope project"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q in grants view:\n%s", want, view)
		}
	}
	// esc goes back rather than denying.
	o, done = press(o, "esc")
	if done || got != nil {
		t.Fatal("esc on the grants page decided")
	}
	o, _ = press(o, "a")
	_, done = press(o, "2")
	if !done || got == nil || !got.Allow || len(got.Grants) != 1 || got.Grants[0].Scope != policy.ScopeProjectLocal {
		t.Fatalf("grant not sent: done=%v %+v", done, got)
	}
}

func TestApprovalEditArguments(t *testing.T) {
	var got *engine.Decision
	var o overlay.Overlay = overlay.NewApproval(approval(t, engine.SeverityCaution), th, func(d engine.Decision) { got = &d })
	o, _ = press(o, "e")
	if v := plain(o.View(80, 30)); !strings.Contains(v, `"path": "internal/x.go"`) {
		t.Fatalf("editor not prefilled:\n%s", v)
	}
	// Corrupt the JSON: jump to the start and type a brace.
	o, _ = press(o, "ctrl+a")
	o = typeText(o, "{")
	o, done := press(o, "ctrl+s")
	if done || got != nil {
		t.Fatal("invalid JSON accepted")
	}
	if v := plain(o.View(80, 30)); !strings.Contains(v, "not valid JSON") {
		t.Fatalf("no validation message:\n%s", v)
	}
	// Fix it by removing the extra brace.
	o, _ = press(o, "ctrl+a")
	o, _, _ = o.Update(tea.KeyPressMsg{Code: tea.KeyDelete})
	_, done = press(o, "ctrl+s")
	if !done || got == nil || !got.Allow || !json.Valid(got.Args) {
		t.Fatalf("valid JSON rejected: done=%v %+v", done, got)
	}
	var args map[string]string
	if err := json.Unmarshal(got.Args, &args); err != nil || args["path"] != "internal/x.go" {
		t.Fatalf("edited args %s: %v", got.Args, err)
	}
}

func TestApprovalExplain(t *testing.T) {
	var o overlay.Overlay = overlay.NewApproval(approval(t, engine.SeverityInfo), th, nil)
	o, done := press(o, "x")
	if done {
		t.Fatal("x closed the prompt")
	}
	if v := plain(o.View(80, 30)); !strings.Contains(v, "mode default: edit_file → ask") {
		t.Fatalf("explain trace missing:\n%s", v)
	}
	o, _ = press(o, "esc")
	if v := plain(o.View(80, 30)); !strings.Contains(v, "[y] allow once") {
		t.Fatalf("esc did not return to the options:\n%s", v)
	}
}

// TestApprovalStatesWhatItGrants: allowing a command that needs the network
// grants it, so the prompt says so and there is no separate "allow with
// network" option to miss. An install also names the paths it will make
// writable — consent has to be to something specific.
func TestApprovalStatesWhatItGrants(t *testing.T) {
	var got *engine.Decision
	a := engine.Approval{
		ID: "ap-2", Tool: "bash", Args: json.RawMessage(`{"command":"brew install fpc"}`),
		Request: policy.Request{Tool: "bash", Shell: &shellclass.Analysis{
			Raw: "brew install fpc", NeedsNetwork: true, Installs: true,
			Reasons: []string{"brew install"},
		}},
		Verdict: policy.Verdict{Decision: policy.Ask},
		Grants:  engine.CallGrant{Network: true, Writable: []string{"/opt/homebrew"}},
	}
	var o overlay.Overlay = overlay.NewApproval(a, th, func(d engine.Decision) { got = &d })
	view := plain(o.View(80, 30))
	for _, want := range []string{
		"allow once (with network and the writes below)",
		"allowing runs this call with network access",
		"/opt/homebrew",
		"brew install fpc",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "[w]") {
		t.Errorf("the separate network option is a trap and must be gone:\n%s", view)
	}
	_, done := press(o, "y")
	if !done || got == nil || !got.Allow {
		t.Fatalf("plain allow not sent: %+v", got)
	}
	// A call that needs nothing extra says nothing extra.
	a.Grants = engine.CallGrant{}
	v := plain(overlay.NewApproval(a, th, nil).View(80, 30))
	if !strings.Contains(v, "[y] allow once") || strings.Contains(v, "network access") {
		t.Errorf("a call with no grant must not advertise one:\n%s", v)
	}
}

func TestApprovalViewFitsWidth(t *testing.T) {
	long := approval(t, engine.SeverityCaution, offer(t, "edit_file(internal/**)", policy.ScopeSession))
	long.Preview.Diff = strings.Repeat("+"+strings.Repeat("x", 150)+"\n", 60)
	for _, w := range []int{40, 80, 200} {
		for _, h := range []int{10, 24} {
			var o overlay.Overlay = overlay.NewApproval(long, th, nil)
			for _, page := range []string{"", "a", "esc", "x", "esc", "e"} {
				if page != "" {
					o, _ = press(o, page)
				}
				view := o.View(w, h)
				for line := range strings.SplitSeq(view, "\n") {
					if got := lipgloss.Width(line); got > w {
						t.Errorf("w=%d h=%d page %q: line %d wide: %q", w, h, page, got, plain(line))
					}
				}
				if got := lipgloss.Height(view); got > h {
					t.Errorf("w=%d h=%d page %q: view %d tall", w, h, page, got)
				}
			}
		}
	}
}

func TestQuestionOptionsAndFreeText(t *testing.T) {
	var got *engine.Answer
	q := engine.QuestionEvent{ID: "q1", Text: "Which one?", Options: []string{"alpha", "beta"}, FreeText: true}
	var o overlay.Overlay = overlay.NewQuestion(q, th, func(a engine.Answer) { got = &a })
	if v := plain(o.View(60, 20)); !strings.Contains(v, "Which one?") || !strings.Contains(v, "[2] beta") {
		t.Fatalf("view:\n%s", v)
	}
	_, done := press(o, "2")
	if !done || got == nil || got.Index != 1 || got.Text != "beta" {
		t.Fatalf("option answer: done=%v %+v", done, got)
	}
	got = nil
	o = overlay.NewQuestion(q, th, func(a engine.Answer) { got = &a })
	o, _ = press(o, "tab")
	o = typeText(o, "gamma")
	_, done = press(o, "enter")
	if !done || got == nil || got.Index != -1 || got.Text != "gamma" {
		t.Fatalf("free-text answer: done=%v %+v", done, got)
	}
	got = nil
	_, done = press(overlay.NewQuestion(q, th, func(a engine.Answer) { got = &a }), "esc")
	if !done || got == nil || got.Index != -1 || got.Text != "" {
		t.Fatalf("esc answer: done=%v %+v", done, got)
	}
}

func TestPickerFiltersAndPicks(t *testing.T) {
	var got *overlay.Item
	items := []overlay.Item{{Label: "claude-opus", Value: "opus"}, {Label: "claude-sonnet", Value: "sonnet"}, {Label: "gpt-5", Value: "gpt"}}
	var o overlay.Overlay = overlay.NewPicker("model", items, th, func(it overlay.Item) tea.Cmd { got = &it; return nil })
	o = typeText(o, "son")
	view := plain(o.View(60, 20))
	if !strings.Contains(view, "claude-sonnet") || strings.Contains(view, "gpt-5") {
		t.Fatalf("filter failed:\n%s", view)
	}
	_, done := press(o, "enter")
	if !done || got == nil || got.Value != "sonnet" {
		t.Fatalf("pick: done=%v %+v", done, got)
	}
	got = nil
	_, done = press(overlay.NewPicker("model", items, th, func(it overlay.Item) tea.Cmd { got = &it; return nil }), "esc")
	if !done || got != nil {
		t.Fatal("esc picked something")
	}
}

func TestConfirmRequiresExactWord(t *testing.T) {
	confirmed := false
	var o overlay.Overlay = overlay.NewConfirm("bypass permissions", "This disables approval prompts.", "yes", th, func() tea.Cmd { confirmed = true; return nil })
	o = typeText(o, "y")
	o, done := press(o, "enter")
	if done || confirmed {
		t.Fatal("partial word confirmed")
	}
	if v := plain(o.View(60, 12)); !strings.Contains(v, "not \"yes\"") {
		t.Fatalf("no rejection shown:\n%s", v)
	}
	o = typeText(o, "yes")
	_, done = press(o, "enter")
	if !done || !confirmed {
		t.Fatal("exact word not confirmed")
	}
	confirmed = false
	_, done = press(overlay.NewConfirm("t", "p", "yes", th, func() tea.Cmd { confirmed = true; return nil }), "esc")
	if !done || confirmed {
		t.Fatal("esc confirmed")
	}
}

func TestInputSubmits(t *testing.T) {
	var got string
	var o overlay.Overlay = overlay.NewInput("export", "Path to write:", "out.md", th, func(s string) tea.Cmd { got = s; return nil })
	o, _ = press(o, "backspace", "backspace")
	o = typeText(o, "txt")
	o, _, _ = o.Update(tea.PasteMsg{Content: "-x"})
	_, done := press(o, "enter")
	if !done || got != "out.txt-x" {
		t.Fatalf("done=%v got=%q", done, got)
	}
}

func TestHelpAndTodosRender(t *testing.T) {
	h := overlay.NewHelp(th, []overlay.Entry{{Name: "ctrl+c ctrl+c", Desc: "quit"}}, []overlay.Entry{{Name: "/mode", Desc: "switch mode"}})
	v := plain(h.View(60, 20))
	if !strings.Contains(v, "ctrl+c ctrl+c") || !strings.Contains(v, "/mode") {
		t.Fatalf("help view:\n%s", v)
	}
	if _, done := press(h, "esc"); !done {
		t.Fatal("esc did not close help")
	}
	td := overlay.NewTodos([]engine.Todo{{Content: "write tests", Status: "completed"}, {Content: "ship", Status: "in_progress"}, {Content: "rest", Status: "pending"}}, th)
	v = plain(td.View(60, 20))
	for _, want := range []string{"1/3 done", "✓ done write tests", "● doing ship", "○ todo rest"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in todos view:\n%s", want, v)
		}
	}
	if _, done := press(td, "esc"); !done {
		t.Fatal("esc did not close todos")
	}
	if v := plain(overlay.NewTodos(nil, th).View(40, 10)); !strings.Contains(v, "no todos") {
		t.Fatalf("empty todos:\n%s", v)
	}
}

func TestSmallOverlaysFitWidth(t *testing.T) {
	q := engine.QuestionEvent{Text: strings.Repeat("long question ", 20), Options: []string{strings.Repeat("option ", 20)}, FreeText: true}
	items := []overlay.Item{{Label: strings.Repeat("label ", 30), Desc: strings.Repeat("desc ", 30)}}
	overlays := []overlay.Overlay{
		overlay.NewQuestion(q, th, nil),
		overlay.NewPicker("p", items, th, nil),
		overlay.NewHelp(th, []overlay.Entry{{Name: strings.Repeat("k", 50), Desc: strings.Repeat("d", 100)}}, nil),
		overlay.NewTodos([]engine.Todo{{Content: strings.Repeat("todo ", 40)}}, th),
		overlay.NewConfirm("t", strings.Repeat("prompt ", 30), "yes", th, nil),
		overlay.NewInput("t", strings.Repeat("prompt ", 30), strings.Repeat("v", 80), th, nil),
	}
	for i, o := range overlays {
		for _, w := range []int{40, 200} {
			for line := range strings.SplitSeq(o.View(w, 20), "\n") {
				if got := lipgloss.Width(line); got > w {
					t.Errorf("overlay %d w=%d: line %d wide: %q", i, w, got, plain(line))
				}
			}
		}
	}
}

// TestApprovalNamesAnUnrecognisedProgram pins what the prompt says about a
// program the classifier has no description of. The script is perfectly
// readable — saying it "contains constructs the analyser cannot see through"
// would be wrong, and reporting it as destructive was worse. It should say
// which program is unknown, and offer a rule for it.
func TestApprovalNamesAnUnrecognisedProgram(t *testing.T) {
	a := engine.Approval{
		ID:   "ap-1",
		Tool: "bash",
		Request: policy.Request{
			Tool: "bash",
			Shell: &shellclass.Analysis{
				Raw:          "fpc -Mobjfpc src/X.pas",
				Class:        shellclass.MutatingWorkspace,
				Reasons:      []string{"unknown command fpc"},
				Unrecognised: []string{"fpc"},
			},
		},
		Verdict:  policy.Verdict{Decision: policy.Ask, Reason: "mutating command"},
		Preview:  engine.Preview{Title: "compile"},
		Offers:   []policy.GrantOffer{offer(t, "bash(fpc *)", policy.ScopeSession)},
		Severity: engine.SeverityCaution,
	}
	p := overlay.NewApproval(a, th, func(engine.Decision) {})
	v := plain(p.View(80, 24))

	if !strings.Contains(v, "wright has no description of fpc") {
		t.Errorf("the prompt does not say which program is unknown:\n%s", v)
	}
	if strings.Contains(v, "cannot see through") {
		t.Errorf("the prompt calls a readable script opaque:\n%s", v)
	}
	if !strings.Contains(v, "[a] allow") {
		t.Errorf("no way to remember a rule:\n%s", v)
	}
	if !strings.Contains(v, "⚠ caution") {
		t.Errorf("compiling a file is not destructive:\n%s", v)
	}
}

// TestApprovalGrantMarking covers the multi-select half of the grants page. A
// script needs a rule per command, so being able to accept only one per
// prompt means being asked again on the very next call — the user's own
// complaint about a session that asked for the same compiler forty times.
func TestApprovalGrantMarking(t *testing.T) {
	offers := []policy.GrantOffer{
		offer(t, "bash(fpc *)", policy.ScopeSession),
		offer(t, "bash(./bin/t *)", policy.ScopeSession),
		offer(t, "bash(./bin/t *)", policy.ScopeProjectLocal),
	}
	open := func(t *testing.T) (overlay.Overlay, func() *engine.Decision) {
		t.Helper()
		var got *engine.Decision
		var o overlay.Overlay = overlay.NewApproval(approval(t, engine.SeverityCaution, offers...), th, func(d engine.Decision) { got = &d })
		o, _ = press(o, "a")
		return o, func() *engine.Decision { return got }
	}

	t.Run("space marks and unmarks", func(t *testing.T) {
		o, got := open(t)
		if v := plain(o.View(80, 30)); !strings.Contains(v, "[ ] [1]") {
			t.Fatalf("rows are not drawn unmarked:\n%s", v)
		}
		o, done := press(o, " ")
		if done || got() != nil {
			t.Fatal("space decided")
		}
		if v := plain(o.View(80, 30)); !strings.Contains(v, "[x] [1]") {
			t.Errorf("space did not mark the focused row:\n%s", v)
		}
		o, _ = press(o, " ")
		if v := plain(o.View(80, 30)); strings.Contains(v, "[x]") {
			t.Errorf("space did not unmark:\n%s", v)
		}
	})

	t.Run("enter applies every marked row", func(t *testing.T) {
		o, got := open(t)
		_, done := press(o, " ", "down", " ", "enter")
		d := got()
		if !done || d == nil || !d.Allow || len(d.Grants) != 2 {
			t.Fatalf("done=%v decision=%+v", done, d)
		}
		for i, want := range []string{"bash(fpc *)", "bash(./bin/t *)"} {
			if d.Grants[i].Rule.String() != want {
				t.Errorf("grant %d = %s, want %s", i, d.Grants[i].Rule.String(), want)
			}
		}
	})

	t.Run("enter with nothing marked applies the focused row", func(t *testing.T) {
		o, got := open(t)
		_, done := press(o, "down", "enter")
		d := got()
		if !done || d == nil || len(d.Grants) != 1 || d.Grants[0].Rule.String() != "bash(./bin/t *)" {
			t.Fatalf("done=%v decision=%+v", done, d)
		}
	})

	// The fast path: one keystroke still answers, marks or no marks. Anything
	// slower would make the common single-rule case worse than before.
	t.Run("a digit still applies that row alone", func(t *testing.T) {
		o, got := open(t)
		_, done := press(o, " ", "1")
		d := got()
		if !done || d == nil || len(d.Grants) != 1 || d.Grants[0].Rule.String() != "bash(fpc *)" {
			t.Fatalf("done=%v decision=%+v", done, d)
		}
	})
}

// TestApprovalOffersEveryProjectScopeLast pins the third scope's placement.
// It writes to the user's config and so applies to every workspace, which is
// worth one deliberate press rather than a reflex: it is last and never
// focused, and its label names the file it writes.
func TestApprovalOffersEveryProjectScopeLast(t *testing.T) {
	offers := []policy.GrantOffer{
		offer(t, "bash(gh pr *)", policy.ScopeSession),
		offer(t, "bash(gh pr *)", policy.ScopeProjectLocal),
		offer(t, "bash(gh pr *)", policy.ScopeUser),
	}
	var got *engine.Decision
	var o overlay.Overlay = overlay.NewApproval(approval(t, engine.SeverityCaution, offers...), th, func(d engine.Decision) { got = &d })
	o, _ = press(o, "a")
	view := plain(o.View(90, 30))
	if !strings.Contains(view, "scope every project") {
		t.Errorf("the widest scope is not shown:\n%s", view)
	}
	// Focus starts on the first row, so enter alone can never take it.
	if i := strings.Index(view, "›"); i < 0 || strings.Contains(view[i:i+60], "every project") {
		t.Errorf("the every-project row is focused by default:\n%s", view)
	}
	_, done := press(o, "3")
	if !done || got == nil || len(got.Grants) != 1 || got.Grants[0].Scope != policy.ScopeUser {
		t.Fatalf("done=%v decision=%+v", done, got)
	}
}
