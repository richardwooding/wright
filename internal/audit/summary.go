package audit

import (
	"fmt"
	"iter"
	"sort"
	"strings"
)

// Summary is the end-of-run digest shown in the TUI and printed by headless
// mode: what changed, what ran, what was refused, what it cost.
type Summary struct {
	Files        int
	Added        int
	Removed      int
	ChangedPaths []string
	Commands     int
	Failed       int // commands with a non-zero exit
	Allowed      int
	Asked        int
	Denied       int
	HardDenied   int
	Redactions   int
	Injections   int
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CostUSD      float64
	Errors       int
}

// Summarize folds an event stream into a Summary. A read error ends the
// fold and is returned with the partial summary.
func Summarize(events iter.Seq2[Event, error]) (Summary, error) {
	var s Summary
	seen := map[string]bool{}
	for ev, err := range events {
		if err != nil {
			return s, err
		}
		s.add(ev, seen)
	}
	sort.Strings(s.ChangedPaths)
	return s, nil
}

func (s *Summary) add(ev Event, seen map[string]bool) {
	for _, f := range ev.Files {
		if !seen[f.Path] {
			seen[f.Path] = true
			s.Files++
			s.ChangedPaths = append(s.ChangedPaths, f.Path)
		}
		s.Added += f.Added
		s.Removed += f.Removed
	}
	if ev.Command != nil {
		s.Commands++
		if ev.Command.Exit != 0 {
			s.Failed++
		}
	}
	if ev.Decision != nil {
		s.addDecision(ev.Decision)
	}
	if ev.Result != nil {
		s.Redactions += ev.Result.Redactions
		if ev.Result.Injection {
			s.Injections++
		}
	}
	if ev.Model != nil {
		s.InputTokens += ev.Model.InputTokens
		s.OutputTokens += ev.Model.OutputTokens
		s.CacheRead += ev.Model.CacheRead
		s.CostUSD += ev.Model.CostUSD
	}
	if ev.Kind == KindError || ev.Error != "" {
		s.Errors++
	}
}

func (s *Summary) addDecision(d *Decision) {
	// A decision made by the user was preceded by a prompt, and its recorded
	// outcome is the user's answer rather than the Ask verdict that raised
	// that prompt. Counting it here keeps Asked meaning "prompts shown" now
	// that an answered prompt lands in Allowed or Denied.
	if d.By == "user" {
		s.Asked++
	}
	switch d.Outcome {
	case "allow":
		s.Allowed++
	case "ask":
		s.Asked++
	case "deny":
		s.Denied++
		if d.HardDeny {
			s.HardDenied++
		}
	}
}

// String renders the summary in the compact end-of-run form:
//
//	Changed 3 files (+41 −7) · Ran 4 commands (1 failed) · Denied 1 (1 hard) · 12.3k in / 1.1k out tokens · $0.38
func (s Summary) String() string {
	var parts []string
	if s.Files > 0 {
		parts = append(parts, fmt.Sprintf("Changed %s (+%d −%d)", plural(s.Files, "file"), s.Added, s.Removed))
	}
	if s.Commands > 0 {
		p := fmt.Sprintf("Ran %s", plural(s.Commands, "command"))
		if s.Failed > 0 {
			p += fmt.Sprintf(" (%d failed)", s.Failed)
		}
		parts = append(parts, p)
	}
	if s.Denied > 0 {
		p := fmt.Sprintf("Denied %d", s.Denied)
		if s.HardDenied > 0 {
			p += fmt.Sprintf(" (%d hard)", s.HardDenied)
		}
		parts = append(parts, p)
	}
	if s.Redactions > 0 {
		parts = append(parts, fmt.Sprintf("Redacted %d", s.Redactions))
	}
	if s.Injections > 0 {
		parts = append(parts, fmt.Sprintf("Injection signals %d", s.Injections))
	}
	if s.InputTokens+s.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s in / %s out tokens", kilo(s.InputTokens), kilo(s.OutputTokens)))
	}
	if s.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", s.CostUSD))
	}
	if s.Errors > 0 {
		parts = append(parts, plural(s.Errors, "error"))
	}
	if len(parts) == 0 {
		return "Nothing changed"
	}
	return strings.Join(parts, " · ")
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// kilo formats a count as 12.3k above a thousand.
func kilo(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
