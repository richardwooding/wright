package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/richardwooding/wright/internal/app"
	"github.com/richardwooding/wright/internal/model"
)

// Run lists every catalog model whose provider has credentials and marks the
// one a session would pick by default.
func (c *ModelsCmd) Run(g *Globals) error {
	cwd, err := workingDir(g.CLI.Cwd)
	if err != nil {
		return err
	}
	eff, err := app.LoadEffective(cwd, os.Getenv)
	if err != nil {
		return err
	}
	choice, detectErr := model.Detect(g.Ctx, eff.Settings.Model, g.CLI.Model, os.Getenv, func(ctx context.Context) ([]string, error) {
		return model.ProbeOllama(ctx, "")
	})
	list := model.List(os.Getenv)
	if detectErr == nil && !contains(list, choice.Model) {
		list = append([]model.Choice{choice}, list...)
	}
	if len(list) == 0 {
		fmt.Fprintln(g.Stdout, "no models: no provider credentials found (see `wright doctor`)")
		return nil
	}
	tw := tabwriter.NewWriter(g.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, " \tMODEL\tPROVIDER\tCONTEXT\tNAME")
	for _, m := range list {
		mark := " "
		if detectErr == nil && m.Model == choice.Model {
			mark = "*"
		}
		ctxWin, _ := model.ContextWindow(m, 0)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%dk\t%s\n", mark, m.Model, m.Provider, ctxWin/1000, m.Info.DisplayName)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if detectErr != nil {
		fmt.Fprintln(g.Stderr, "default: "+detectErr.Error())
	} else {
		fmt.Fprintf(g.Stdout, "\n* default (%s)\n", choice.Source)
	}
	return nil
}

func contains(list []model.Choice, name string) bool {
	for _, m := range list {
		if m.Model == name {
			return true
		}
	}
	return false
}

// ConfigShowCmd prints merged settings.
type ConfigShowCmd struct{}

// Run prints the effective settings as JSON, exactly as a session would use
// them: an untrusted project's allow rules are absent, and stderr says so.
func (c *ConfigShowCmd) Run(g *Globals) error {
	cwd, err := workingDir(g.CLI.Cwd)
	if err != nil {
		return err
	}
	eff, err := app.LoadEffective(cwd, os.Getenv)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(eff.Settings, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(g.Stdout, string(data))
	if !eff.Trusted {
		fmt.Fprintln(g.Stderr, "note: .wright/settings.json is not trusted; its allow rules, extra directories, env passthrough and MCP servers are not included")
	}
	return nil
}
