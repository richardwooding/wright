package app

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/richardwooding/wright/internal/agents"
	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/mcpclient"
	"github.com/richardwooding/wright/internal/skillsdir"
	"github.com/richardwooding/wright/internal/trust"
)

// ErrNoSuchServer is returned when a named MCP server is not configured.
var ErrNoSuchServer = errors.New("app: no such MCP server")

// SkillRow is one discovered skill, for `wright skills` and `/skills`.
type SkillRow = skillsdir.Row

// SkillProblem is a skill that could not be loaded.
type SkillProblem = skillsdir.Problem

// ListSkills reports the skills a session in cwd would load, without
// starting one.
func ListSkills(cwd string, env func(string) string) ([]SkillRow, []SkillProblem, error) {
	eff, err := LoadEffective(cwd, env)
	if err != nil {
		return nil, nil, err
	}
	ws, err := openWorkspace(cwd)
	if err != nil {
		return nil, nil, err
	}
	set, problems, err := skillsdir.Load(ws, eff.Layered.Paths.UserConfig, eff.Settings.Skills)
	if err != nil {
		return nil, nil, err
	}
	return skillsdir.Describe(set), problems, nil
}

// MCPServerInfo is one configured server as the CLI reports it: what the
// settings say plus whether it has been accepted.
type MCPServerInfo struct {
	Name      string
	Transport string
	Target    string
	Trusted   bool
	// Reachable is false when the settings themselves are not trusted, in
	// which case the server is inert whatever trust.json says.
	Reachable bool
}

// ListMCPServers reports the configured servers for cwd. Servers from an
// untrusted project settings file are listed (so the user can see what the
// project wants) but marked unreachable.
func ListMCPServers(cwd string, env func(string) string) ([]MCPServerInfo, error) {
	eff, err := LoadEffective(cwd, env)
	if err != nil {
		return nil, err
	}
	store := trust.Open(eff.Layered.Paths.TrustFile())
	configured := eff.Settings.MCPServers
	untrusted := map[string]config.MCPServer{}
	if !eff.Trusted {
		for name, s := range eff.Layered.Project.MCPServers {
			if _, ok := configured[name]; !ok {
				untrusted[name] = s
			}
		}
	}
	out := make([]MCPServerInfo, 0, len(configured)+len(untrusted))
	for name, s := range configured {
		out = append(out, serverInfo(name, s, store, true))
	}
	for name, s := range untrusted {
		out = append(out, serverInfo(name, s, store, false))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func serverInfo(name string, s config.MCPServer, store *trust.Store, reachable bool) MCPServerInfo {
	transport := s.Transport
	if transport == "" {
		transport = mcpclient.TransportStdio
		if s.Command == "" && s.URL != "" {
			transport = mcpclient.TransportHTTP
		}
	}
	target := s.URL
	if target == "" {
		target = strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
	}
	_, accepted := store.Server(name)
	return MCPServerInfo{
		Name: name, Transport: transport, Target: target,
		Trusted: s.Trusted || accepted, Reachable: reachable,
	}
}

// AddMCPServer writes a server into .wright/settings.json and re-records the
// project's trust, because the user who ran the command is the one vouching
// for the change. It returns the path it wrote.
func AddMCPServer(cwd, name string, server config.MCPServer, env func(string) string) (string, error) {
	eff, err := LoadEffective(cwd, env)
	if err != nil {
		return "", err
	}
	err = eff.Layered.SaveProject(func(s *config.Settings) {
		if s.MCPServers == nil {
			s.MCPServers = map[string]config.MCPServer{}
		}
		s.MCPServers[name] = server
	})
	if err != nil {
		return "", err
	}
	return eff.Layered.Paths.ProjectSettingsFile(), retrust(eff)
}

// RemoveMCPServer deletes a server from .wright/settings.json and forgets any
// trust record for it.
func RemoveMCPServer(cwd, name string, env func(string) string) (string, error) {
	eff, err := LoadEffective(cwd, env)
	if err != nil {
		return "", err
	}
	found := false
	err = eff.Layered.SaveProject(func(s *config.Settings) {
		if _, ok := s.MCPServers[name]; ok {
			found = true
			delete(s.MCPServers, name)
		}
	})
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: %s", ErrNoSuchServer, name)
	}
	store := trust.Open(eff.Layered.Paths.TrustFile())
	if _, ok := store.Server(name); ok {
		if err := store.ForgetServer(name); err != nil {
			return "", err
		}
	}
	return eff.Layered.Paths.ProjectSettingsFile(), retrust(eff)
}

// retrust re-accepts the project settings file after wright itself wrote it,
// but only when it was already trusted: a command must not silently turn an
// untrusted project into a trusted one.
func retrust(eff Effective) error {
	if !eff.Trusted {
		return nil
	}
	hash, err := ProjectHash(eff.Layered.Paths)
	if err != nil || hash == "" {
		return err
	}
	return trust.Open(eff.Layered.Paths.TrustFile()).AcceptProject(eff.Root, hash)
}

// ListAgents reports the custom sub-agents a session in cwd would load.
func ListAgents(cwd string, env func(string) string) ([]agents.Definition, []agents.Problem, error) {
	eff, err := LoadEffective(cwd, env)
	if err != nil {
		return nil, nil, err
	}
	ws, err := openWorkspace(cwd)
	if err != nil {
		return nil, nil, err
	}
	return agents.LoadCustom(ws, eff.Layered.Paths.UserConfig)
}
