package cli

import (
	"fmt"
	"os"

	"github.com/richardwooding/wright/internal/config"
)

// ConfigPathsCmd prints the resolved settings, data and cache locations.
type ConfigPathsCmd struct{}

// Run prints the paths and whether each settings layer exists on disk.
func (c *ConfigPathsCmd) Run(g *Globals) error {
	cwd, err := workingDir(g.CLI.Cwd)
	if err != nil {
		return err
	}
	layered, err := config.Load(cwd)
	if err != nil {
		return err
	}
	p := layered.Paths
	rows := []struct{ label, path string }{
		{"user config", p.UserConfigFile()},
		{"user data", p.UserData},
		{"user cache", p.UserCache},
		{"project settings", p.ProjectSettingsFile()},
		{"project local", p.ProjectLocalFile()},
		// trust.json decides whether this directory's edits prompt and
		// whether its settings apply at all, so it belongs among the files
		// that decide what wright may do here.
		{"trust", p.TrustFile()},
	}
	for _, r := range rows {
		state := "absent"
		if _, statErr := os.Stat(r.path); statErr == nil {
			state = "present"
		}
		fmt.Fprintf(g.Stdout, "%-17s %s (%s)\n", r.label, r.path, state)
	}
	return nil
}

// workingDir resolves the --cwd flag (or the process cwd) to an absolute path.
func workingDir(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	return os.Getwd()
}
