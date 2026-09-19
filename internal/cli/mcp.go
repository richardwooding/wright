package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/richardwooding/wright/internal/app"
	"github.com/richardwooding/wright/internal/config"
)

// Run lists the configured MCP servers and whether each has been accepted.
func (c *MCPListCmd) Run(g *Globals) error {
	cwd, err := workingDir(g.CLI.Cwd)
	if err != nil {
		return err
	}
	servers, err := app.ListMCPServers(cwd, os.Getenv)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		fmt.Fprintln(g.Stdout, "no MCP servers configured")
		return nil
	}
	for _, s := range servers {
		state := "needs acceptance on first use"
		if s.Trusted {
			state = "accepted"
		}
		if !s.Reachable {
			state = "ignored: .wright/settings.json is not trusted yet (run /trust)"
		}
		fmt.Fprintf(g.Stdout, "%-16s %-6s %s\n  %s\n", s.Name, s.Transport, s.Target, state)
	}
	return nil
}

// Run adds a server to the project settings file.
func (c *MCPAddCmd) Run(g *Globals) error {
	cwd, err := workingDir(g.CLI.Cwd)
	if err != nil {
		return err
	}
	server, err := c.server()
	if err != nil {
		return err
	}
	path, err := app.AddMCPServer(cwd, c.Name, server, os.Getenv)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.Stdout, "added MCP server %q to %s\n", c.Name, path)
	fmt.Fprintln(g.Stdout, "it is connected — and asked about — the next time wright starts")
	return nil
}

// server turns the flags into a settings entry. Environment variables are
// named, never valued: the value is read from wright's own environment when
// the server starts, so a committed settings file carries no secrets.
func (c *MCPAddCmd) server() (config.MCPServer, error) {
	s := config.MCPServer{
		Transport: c.Type,
		Command:   c.Command,
		Args:      c.Arg,
		URL:       c.URL,
		Network:   c.Network,
	}
	if len(c.Env) > 0 {
		s.Env = map[string]string{}
		for _, name := range c.Env {
			if strings.ContainsRune(name, '=') {
				return config.MCPServer{}, fmt.Errorf("--env takes a variable name, not a value: %q", name)
			}
			s.Env[name] = "" // empty = read it from the environment at startup
		}
	}
	switch {
	case s.Transport == "http" && s.URL == "":
		return config.MCPServer{}, fmt.Errorf("--type http needs --url")
	case s.Transport == "stdio" && s.Command == "":
		return config.MCPServer{}, fmt.Errorf("--type stdio needs --command")
	case s.Transport == "" && s.Command == "" && s.URL == "":
		return config.MCPServer{}, fmt.Errorf("mcp add needs --command (stdio) or --url (http)")
	}
	return s, nil
}

// Run removes a server from the project settings file and forgets it.
func (c *MCPRemoveCmd) Run(g *Globals) error {
	cwd, err := workingDir(g.CLI.Cwd)
	if err != nil {
		return err
	}
	path, err := app.RemoveMCPServer(cwd, c.Name, os.Getenv)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.Stdout, "removed MCP server %q from %s\n", c.Name, path)
	return nil
}

// Run lists the skills a session in this workspace would load.
func (c *SkillsCmd) Run(g *Globals) error {
	cwd, err := workingDir(g.CLI.Cwd)
	if err != nil {
		return err
	}
	rows, problems, err := app.ListSkills(cwd, os.Getenv)
	if err != nil {
		return err
	}
	if len(rows) == 0 && len(problems) == 0 {
		fmt.Fprintln(g.Stdout, "no skills found")
		return nil
	}
	for _, r := range rows {
		fmt.Fprintf(g.Stdout, "%-24s %s\n  %s\n", r.Name, oneLine(r.Description), r.Source)
	}
	for _, p := range problems {
		fmt.Fprintf(g.Stderr, "skipped %s: %v\n", p.Path, p.Err)
	}
	return nil
}

// oneLine keeps a listing to one row per entry.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	const max = 100
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
