package mcpclient

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	agentmcp "github.com/richardwooding/agentkit/mcp"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/trust"
)

// transportOf resolves the transport name and checks the settings make sense
// for it, so a typo is reported before anything is started.
func transportOf(cfg config.MCPServer) (string, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Transport)) {
	case TransportStdio:
		if cfg.Command == "" {
			return "", fmt.Errorf("mcpclient: stdio server needs a command")
		}
		return TransportStdio, nil
	case TransportHTTP, "streamable-http", "sse":
		if cfg.URL == "" {
			return "", fmt.Errorf("mcpclient: http server needs a url")
		}
		return TransportHTTP, nil
	case "":
		switch {
		case cfg.Command != "":
			return TransportStdio, nil
		case cfg.URL != "":
			return TransportHTTP, nil
		}
		return "", fmt.Errorf("mcpclient: server needs a command or a url")
	default:
		return "", fmt.Errorf("mcpclient: unknown transport %q (stdio, http)", cfg.Transport)
	}
}

// newRecord builds the trust record for what is about to be connected. For a
// stdio server the *binary* is hashed, not its name, so replacing the
// executable on PATH re-prompts.
func newRecord(name string, cfg config.MCPServer, transport string, deps Deps) (trust.ServerRecord, error) {
	rec := trust.ServerRecord{Name: name, Transport: transport}
	if transport == TransportHTTP {
		rec.URL = cfg.URL
		return rec, nil
	}
	rec.ArgsSHA256 = trust.HashStrings(cfg.Args...)
	if deps.Dial != nil {
		// A test (or an embedded server) supplies the transport itself;
		// there is no binary on disk to hash.
		rec.CommandSHA256 = trust.HashStrings(cfg.Command)
		return rec, nil
	}
	path, err := exec.LookPath(cfg.Command)
	if err != nil {
		return rec, fmt.Errorf("mcpclient: %s: %w", name, err)
	}
	hash, err := trust.HashFile(path)
	if err != nil {
		return rec, fmt.Errorf("mcpclient: %s: hash %s: %w", name, path, err)
	}
	rec.CommandSHA256 = hash
	return rec, nil
}

// dial builds the transport for one server.
func dial(ctx context.Context, name string, cfg config.MCPServer, transport string, deps Deps) (sdk.Transport, error) {
	if deps.Dial != nil {
		return deps.Dial(ctx, name, cfg)
	}
	if transport == TransportHTTP {
		return agentmcp.HTTP(cfg.URL, agentmcp.WithHTTPClient(httpClient(cfg, deps))), nil
	}
	return stdio(ctx, cfg, deps)
}

// stdio starts a server process. It runs inside the OS sandbox unless the
// backend confines nothing anyway (none, or a container that already is the
// boundary), with the filtered environment plus whatever the settings
// declare, the workspace as its working directory and no network unless the
// server is configured with "network": true.
//
// The process must outlive the context that connected it, so the command is
// built with a context that cannot be cancelled: the session's Close is what
// ends it (stdin, then SIGTERM, then SIGKILL, per the MCP spec).
func stdio(ctx context.Context, cfg config.MCPServer, deps Deps) (sdk.Transport, error) {
	argv := append([]string{cfg.Command}, cfg.Args...)
	env := serverEnv(cfg, deps)
	dir := deps.Spec.Dir
	if deps.Workspace != nil {
		dir = deps.Workspace.Root()
	}
	runCtx := context.WithoutCancel(ctx)
	if b := deps.Sandbox; b != nil && b.Name() != sandbox.NameNone && b.Name() != sandbox.NameContainer {
		spec := deps.Spec
		spec.Argv, spec.Env, spec.Dir, spec.Network = argv, env, dir, cfg.Network
		spec.Timeout, spec.Stdin = 0, nil
		cmd, err := b.Command(runCtx, spec)
		if err != nil {
			return nil, err
		}
		return &sdk.CommandTransport{Command: cmd}, nil
	}
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	return &sdk.CommandTransport{Command: cmd}, nil
}

// serverEnv is the filtered environment plus the server's declared entries.
// The declared ones are applied last and are not filtered: the user named
// them in a settings file that is itself trusted, which is the only way a
// credential reaches an MCP server.
func serverEnv(cfg config.MCPServer, deps Deps) []string {
	out := slices.Clone(deps.Env)
	names := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		v := resolve(k, cfg.Env[k], deps.Getenv)
		out = slices.DeleteFunc(out, func(kv string) bool { return strings.HasPrefix(kv, k+"=") })
		out = append(out, k+"="+v)
	}
	return out
}

// httpClient adds the configured headers to the guarded client, leaving the
// client itself (and its SSRF guard) alone.
func httpClient(cfg config.MCPServer, deps Deps) *http.Client {
	base := deps.HTTPClient
	if len(cfg.Headers) == 0 {
		return base
	}
	clone := &http.Client{}
	if base != nil {
		*clone = *base
	}
	inner := clone.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = resolve(k, v, deps.Getenv)
	}
	clone.Transport = &headerTransport{inner: inner, headers: headers}
	return clone
}

// headerTransport adds fixed headers to every request.
type headerTransport struct {
	inner   http.RoundTripper
	headers map[string]string
}

// RoundTrip implements http.RoundTripper. The request is cloned because a
// RoundTripper may not modify the one it is given.
func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		if v != "" {
			clone.Header.Set(k, v)
		}
	}
	return t.inner.RoundTrip(clone)
}

// resolve expands a settings value against wright's own environment, so a
// committed settings file can name a credential without carrying it: an
// empty value means the variable called key, and "$NAME" or "${NAME}" means
// the variable NAME. Anything else is used verbatim.
func resolve(key, value string, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	switch {
	case value == "":
		return getenv(key)
	case strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}"):
		return getenv(value[2 : len(value)-1])
	case strings.HasPrefix(value, "$"):
		return getenv(value[1:])
	default:
		return value
	}
}
