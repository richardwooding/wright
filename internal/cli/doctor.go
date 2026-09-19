package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"

	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/theme"
)

// providerEnvKeys are the credential variables doctor reports as present or
// absent. Only the names are ever printed; values never leave the process.
var providerEnvKeys = []string{
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"GOOGLE_CLOUD_PROJECT",
	"XAI_API_KEY",
	"DEEPSEEK_API_KEY",
	"OPENROUTER_API_KEY",
	"GROQ_API_KEY",
	"OLLAMA_HOST",
}

// Report section headings.
const (
	sectionSandbox = "Sandbox"
	sectionTools   = "Tools"
	sectionCreds   = "Credentials (names only)"
)

// DoctorCmd reports what the machine offers: sandbox backends, external tools
// and which provider credentials are configured.
type DoctorCmd struct{}

// doctorRow is one line of the report; status selects the colour and glyph.
type doctorRow struct {
	section, name, detail string
	status                rowStatus
}

type rowStatus int

const (
	statusInfo rowStatus = iota
	statusOK
	statusWarn
	statusBad
)

// Run prints the report. Styles are applied only when stdout is a terminal.
func (c *DoctorCmd) Run(g *Globals) error {
	ctx, cancel := context.WithTimeout(g.Ctx, 10*time.Second)
	defer cancel()

	rows := append(sandboxRows(ctx, g.CLI.Sandbox), toolRows(ctx)...)
	rows = append(rows, credentialRows()...)

	styled := isTTY(g.Stdout)
	var th theme.Theme
	if styled {
		th = theme.New(lipgloss.HasDarkBackground(os.Stdin, os.Stdout))
	}
	fmt.Fprintln(g.Stdout, render(th, styled, g.Build.String(), statusInfo))
	section := ""
	for _, r := range rows {
		if r.section != section {
			section = r.section
			fmt.Fprintln(g.Stdout)
			fmt.Fprintln(g.Stdout, render(th, styled, section, statusInfo))
		}
		fmt.Fprintf(g.Stdout, "  %s %-12s %s\n", glyph(th, styled, r.status), r.name, render(th, styled, r.detail, r.status))
	}
	return nil
}

// sandboxRows probes every backend and reports which one Detect would pick.
func sandboxRows(ctx context.Context, want string) []doctorRow {
	rows := make([]doctorRow, 0, 8)
	for _, st := range sandbox.Probe(ctx) {
		status := statusOK
		if !st.Available {
			status = statusWarn
		}
		rows = append(rows, doctorRow{section: sectionSandbox, name: st.Name, detail: st.Detail, status: status})
	}
	if abi, err := sandbox.LandlockABI(); err == nil {
		rows = append(rows, doctorRow{section: sectionSandbox, name: "landlock ABI", detail: fmt.Sprintf("v%d (network restriction needs v4+)", abi), status: statusInfo})
	}
	backend, warnings := sandbox.Detect(ctx, want)
	status := statusOK
	if backend.Name() == "none" {
		status = statusBad
	}
	rows = append(rows, doctorRow{section: sectionSandbox, name: "selected", detail: backend.Name(), status: status})
	for _, w := range warnings {
		rows = append(rows, doctorRow{section: sectionSandbox, name: "warning", detail: w.String(), status: statusWarn})
	}
	return rows
}

// toolRows reports the external tools wright shells out to.
func toolRows(ctx context.Context) []doctorRow {
	rows := []doctorRow{}
	for _, t := range []struct{ name, flag string }{{"git", "--version"}, {"rg", "--version"}, {"bwrap", "--version"}} {
		detail, err := toolVersion(ctx, t.name, t.flag)
		status := statusOK
		if err != nil {
			detail, status = "not found", statusWarn
			if t.name == "git" {
				status = statusBad
			}
		}
		rows = append(rows, doctorRow{section: sectionTools, name: t.name, detail: detail, status: status})
	}
	return rows
}

// credentialRows lists provider variables by presence only.
func credentialRows() []doctorRow {
	rows := make([]doctorRow, 0, len(providerEnvKeys))
	for _, k := range providerEnvKeys {
		if _, ok := os.LookupEnv(k); ok {
			rows = append(rows, doctorRow{section: sectionCreds, name: k, detail: "present", status: statusOK})
		} else {
			rows = append(rows, doctorRow{section: sectionCreds, name: k, detail: "absent", status: statusInfo})
		}
	}
	return rows
}

// toolVersion runs `name flag` and returns its first output line.
func toolVersion(ctx context.Context, name, flag string) (string, error) {
	out, err := exec.CommandContext(ctx, name, flag).Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line, nil
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// render applies the status colour when styling is on; otherwise plain text.
func render(th theme.Theme, styled bool, s string, st rowStatus) string {
	if !styled {
		return s
	}
	switch st {
	case statusOK:
		return th.Good.Render(s)
	case statusWarn:
		return th.Warm.Render(s)
	case statusBad:
		return th.Hot.Render(s)
	default:
		return th.Title.Render(s)
	}
}

// glyph pairs a word-free symbol with the colour so the state is legible
// without colour too (the symbols differ per status).
func glyph(th theme.Theme, styled bool, st rowStatus) string {
	var g string
	switch st {
	case statusOK:
		g = "✓"
	case statusWarn:
		g = "!"
	case statusBad:
		g = "✗"
	default:
		g = "·"
	}
	return render(th, styled, g, st)
}
