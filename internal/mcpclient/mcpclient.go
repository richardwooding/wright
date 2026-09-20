// Package mcpclient connects the MCP servers a workspace configures and
// exposes their tools to the agent — but only after the user has seen what
// they are and said yes.
//
// The contract, in order: build the transport (a stdio server runs inside the
// OS sandbox with the filtered environment, an HTTP server goes through the
// guarded client), do the handshake, list the tools with their annotations,
// hash what was shown (server binary, arguments, URL, tool list) and compare
// that with the record in trust.json. A matching record connects silently; a
// missing or changed one asks, naming exactly what changed; a declined server
// contributes nothing.
//
// Tool names are prefixed "mcp_<server>_" and addressed by policy as
// "mcp:<server>:<tool>", which the builtin "mcp:*" ask rule covers. The
// annotations a server sends (readOnlyHint and friends) are its own claims:
// they are shown to the user and may *tighten* what plan mode allows, but
// they never allow anything on their own.
package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/richardwooding/agentkit"
	agentmcp "github.com/richardwooding/agentkit/mcp"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/sandbox"
	"github.com/richardwooding/wright/internal/trust"
	"github.com/richardwooding/wright/internal/workspace"
)

// Transport names used in records, status rows and prompts.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// ErrDeclined is the status error of a server the user did not accept.
var ErrDeclined = errors.New("mcpclient: connection declined")

// Choice is the answer to a connection proposal.
type Choice int

// The three answers. Deny is the zero value so a consent function that
// cannot ask fails closed.
const (
	Deny Choice = iota
	// Once connects for this session without recording anything.
	Once
	// Always connects and records the server in trust.json.
	Always
)

// String returns the choice's name.
func (c Choice) String() string {
	switch c {
	case Once:
		return "once"
	case Always:
		return "always"
	default:
		return "deny"
	}
}

// ToolInfo is one tool as the server describes it. The three flags are the
// server's own annotations: advisory, never a permission.
type ToolInfo struct {
	Name        string
	Description string
	ReadOnly    bool
	Destructive bool
	OpenWorld   bool
}

// Proposal is what the user is asked to accept: the exact command or URL
// that will run, every tool the server offers, and — when a record exists
// already — what changed since it was accepted.
type Proposal struct {
	Name      string
	Transport string
	Command   string
	Args      []string
	URL       string
	Tools     []ToolInfo
	Changed   []string
}

// Target renders the command line or URL the proposal is about.
func (p Proposal) Target() string {
	if p.Transport == TransportHTTP {
		return p.URL
	}
	return strings.TrimSpace(p.Command + " " + strings.Join(p.Args, " "))
}

// String renders the whole proposal as the text a prompt (or a warning about
// a server that was not connected) shows.
func (p Proposal) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "MCP server %q (%s): %s\n", p.Name, p.Transport, p.Target())
	if len(p.Changed) > 0 {
		fmt.Fprintf(&b, "  changed since you accepted it: %s\n", strings.Join(p.Changed, ", "))
	}
	fmt.Fprintf(&b, "  %d tool(s), as the server describes them:\n", len(p.Tools))
	for _, t := range p.Tools {
		fmt.Fprintf(&b, "    %s%s — %s\n", t.Name, annotationSuffix(t), firstLine(t.Description))
	}
	b.WriteString("  annotations are the server's own claims; every call is still checked by policy")
	return b.String()
}

func annotationSuffix(t ToolInfo) string {
	var tags []string
	if t.ReadOnly {
		tags = append(tags, "read-only")
	}
	if t.Destructive {
		tags = append(tags, "destructive")
	}
	if t.OpenWorld {
		tags = append(tags, "open-world")
	}
	if len(tags) == 0 {
		return ""
	}
	return " [" + strings.Join(tags, ", ") + "]"
}

// ConsentFunc asks the user about a server. A nil ConsentFunc denies, which
// is what headless runs use: nothing new is connected without a person.
type ConsentFunc func(ctx context.Context, p Proposal) (Choice, error)

// ConsentOptions are the three answers as a picker should list them, in
// Choice order. Declining is first because it is the answer that changes
// nothing: a server is someone else's code, and the tool list it just sent
// is the only thing describing it.
func ConsentOptions() []string {
	return []string{
		"do not connect",
		"connect for this session only",
		"connect and remember this server",
	}
}

// ConsentFromSelect turns a picker into a ConsentFunc. It is the mapping
// every front end needs and none of them should write twice.
//
// It fails closed at each step, because every way of not getting an answer
// means the same thing: a nil picker denies, a picker that reports no
// choice denies, and an index outside ConsentOptions denies. Only an
// explicit pick of "once" or "remember" connects anything.
//
// question is prepended to the proposal, which describes the server and
// every tool it offers.
func ConsentFromSelect(question string, sel func(prompt string, options []string) (int, bool)) ConsentFunc {
	if sel == nil {
		return nil
	}
	return func(_ context.Context, p Proposal) (Choice, error) {
		i, ok := sel(p.String()+question, ConsentOptions())
		if !ok {
			return Deny, nil
		}
		switch Choice(i) {
		case Once:
			return Once, nil
		case Always:
			return Always, nil
		default:
			return Deny, nil
		}
	}
}

// Deps is what Connect needs from the rest of wright.
type Deps struct {
	// Trust records accepted servers. nil means nothing is remembered and
	// every server that is not marked trusted in settings needs consent.
	Trust *trust.Store
	// Consent asks the user; nil denies.
	Consent ConsentFunc
	// Env is the environment a stdio server starts with, already filtered by
	// sandbox.Env. The server's own env entries are added on top.
	Env []string
	// Sandbox runs stdio servers. The none and container backends run them
	// directly (in a container the container is the boundary).
	Sandbox sandbox.Backend
	// Spec is the base sandbox spec (workspace read-write, caches, no
	// network); Connect sets Argv, Env and Network per server.
	Spec sandbox.Spec
	// Workspace is the working directory of a stdio server.
	Workspace *workspace.Workspace
	// HTTPClient is the guarded client HTTP transports use. nil uses the
	// SDK default, which is http.DefaultClient.
	HTTPClient *http.Client
	// Getenv resolves settings values that name a variable instead of
	// carrying it. nil uses os.Getenv.
	Getenv func(string) string
	// Dial overrides transport construction. It exists for tests, which
	// connect to an in-process server instead of spawning one.
	Dial func(ctx context.Context, name string, cfg config.MCPServer) (sdk.Transport, error)
}

// Status is one server's outcome, for `/mcp`, `wright mcp list` and warnings.
type Status struct {
	Name      string
	Transport string
	ToolCount int
	// Trusted is true when a stored record matched or settings marked the
	// server trusted: no question was asked.
	Trusted bool
	// Err is nil when the server's tools are in the set.
	Err error
}

// toolRef maps a registered tool name back to its server and annotations.
type toolRef struct {
	server string
	tool   string
	info   ToolInfo
}

// Set is the result of Connect: the tools to register, one status row per
// configured server, and the sessions to close at the end.
type Set struct {
	Tools   agentkit.Toolset
	Servers []Status

	sessions []*agentmcp.Server
	refs     map[string]toolRef
}

// Describe resolves a registered tool name (mcp_<server>_<tool>) to its
// server, its server-side name and its annotations. The app uses it to build
// the policy request "mcp:<server>:<tool>" with the read-only hint attached.
func (s *Set) Describe(name string) (server, tool string, info ToolInfo, ok bool) {
	if s == nil {
		return "", "", ToolInfo{}, false
	}
	r, ok := s.refs[name]
	if !ok {
		return "", "", ToolInfo{}, false
	}
	return r.server, r.tool, r.info, true
}

// ReadOnlyNames lists the registered tools annotated read-only. Plan mode
// keeps them (it still asks before each call) and drops the rest.
func (s *Set) ReadOnlyNames() []string {
	if s == nil {
		return nil
	}
	var out []string
	for name, r := range s.refs {
		if r.info.ReadOnly {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Close ends every session, terminating the stdio servers it started.
func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, srv := range s.sessions {
		if err := srv.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	s.sessions = nil
	return errors.Join(errs...)
}

// Connect connects every configured server, in name order so the status rows
// and the tool list are stable. A server that fails or is declined becomes a
// Status with Err set and contributes no tools; the error return is reserved
// for a caller mistake.
func Connect(ctx context.Context, servers map[string]config.MCPServer, deps Deps) (*Set, error) {
	set := &Set{refs: map[string]toolRef{}}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		set.add(ctx, name, servers[name], deps)
	}
	return set, nil
}

// add connects one server and records the outcome.
func (s *Set) add(ctx context.Context, name string, cfg config.MCPServer, deps Deps) {
	st := Status{Name: name}
	transport, err := transportOf(cfg)
	if err != nil {
		st.Err = err
		s.Servers = append(s.Servers, st)
		return
	}
	st.Transport = transport
	srv, tools, infos, trusted, err := connect(ctx, name, cfg, transport, deps)
	st.Trusted, st.Err = trusted, err
	if err != nil {
		s.Servers = append(s.Servers, st)
		return
	}
	st.ToolCount = len(tools)
	s.sessions = append(s.sessions, srv)
	s.Tools = append(s.Tools, tools...)
	for i, t := range tools {
		s.refs[t.Definition().Name] = toolRef{server: name, tool: infos[i].Name, info: infos[i]}
	}
	s.Servers = append(s.Servers, st)
}

// connect does the handshake, lists the tools and applies the trust rules.
// Everything it returns belongs to an accepted server; a declined or failed
// one closes the session before returning the error.
func connect(ctx context.Context, name string, cfg config.MCPServer, transport string, deps Deps) (*agentmcp.Server, agentkit.Toolset, []ToolInfo, bool, error) {
	record, err := newRecord(name, cfg, transport, deps)
	if err != nil {
		return nil, nil, nil, false, err
	}
	tr, err := dial(ctx, name, cfg, transport, deps)
	if err != nil {
		return nil, nil, nil, false, err
	}
	opts := []agentmcp.Option{agentmcp.WithPrefix(prefix(name, cfg)), agentmcp.WithImplementation("wright", "1")}
	if transport == TransportStdio {
		// One process behind one pipe: never call it from two tool calls at
		// once, whatever the model asks for.
		opts = append(opts, agentmcp.WithSequential())
	}
	srv, err := agentmcp.Connect(ctx, tr, opts...)
	if err != nil {
		return nil, nil, nil, false, err
	}
	tools, err := srv.Tools(ctx)
	if err != nil {
		return nil, nil, nil, false, closeWith(srv, err)
	}
	infos, err := serverTools(ctx, srv)
	if err != nil {
		return nil, nil, nil, false, closeWith(srv, err)
	}
	if len(infos) != len(tools) {
		return nil, nil, nil, false, closeWith(srv, fmt.Errorf("mcpclient: %s: tool list changed while connecting", name))
	}
	record.ToolsSHA256 = hashTools(infos)
	trusted, err := accept(ctx, name, cfg, record, infos, deps)
	if err != nil {
		return nil, nil, nil, false, closeWith(srv, err)
	}
	return srv, tools, infos, trusted, nil
}

// accept applies the trust rules and returns whether the server connected
// without a question.
func accept(ctx context.Context, name string, cfg config.MCPServer, record trust.ServerRecord, infos []ToolInfo, deps Deps) (bool, error) {
	stored, has := storedRecord(deps, name)
	if has && stored.Matches(record) {
		return true, nil
	}
	if cfg.Trusted {
		// Declared trusted in a settings layer that is itself trusted.
		return true, nil
	}
	proposal := Proposal{
		Name: name, Transport: record.Transport, Command: cfg.Command, Args: cfg.Args,
		URL: cfg.URL, Tools: infos,
	}
	if has {
		proposal.Changed = changes(stored, record)
	}
	if deps.Consent == nil {
		return false, fmt.Errorf("%w: %s", ErrDeclined, "no way to ask for consent")
	}
	choice, err := deps.Consent(ctx, proposal)
	if err != nil {
		return false, err
	}
	switch choice {
	case Always:
		if deps.Trust != nil {
			if err := deps.Trust.AcceptServer(record); err != nil {
				return false, err
			}
		}
		return false, nil
	case Once:
		return false, nil
	default:
		return false, ErrDeclined
	}
}

func storedRecord(deps Deps, name string) (trust.ServerRecord, bool) {
	if deps.Trust == nil {
		return trust.ServerRecord{}, false
	}
	return deps.Trust.Server(name)
}

// changes names the fields that differ from the accepted record, so the
// prompt can say what it is really asking about.
func changes(old, cur trust.ServerRecord) []string {
	var out []string
	for _, c := range []struct {
		what     string
		old, cur string
	}{
		{"transport", old.Transport, cur.Transport},
		{"server binary", old.CommandSHA256, cur.CommandSHA256},
		{"arguments", old.ArgsSHA256, cur.ArgsSHA256},
		{"url", old.URL, cur.URL},
		{"tool list", old.ToolsSHA256, cur.ToolsSHA256},
	} {
		if c.old != c.cur {
			out = append(out, c.what)
		}
	}
	return out
}

// serverTools lists the tools under the names the *server* uses, which is
// what the user is shown, what the trust record hashes and what policy
// addresses as "mcp:<server>:<tool>". The registered agentkit tools carry
// the "mcp_<server>_" prefix instead, and pair with these by position.
func serverTools(ctx context.Context, srv *agentmcp.Server) ([]ToolInfo, error) {
	var out []ToolInfo
	for t, err := range srv.Session().Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcpclient: list tools: %w", err)
		}
		out = append(out, info(t))
	}
	return out, nil
}

// info reads one tool's annotations. The MCP defaults apply: a tool is
// destructive and open-world unless it says otherwise, and not read-only.
// Nothing verifies any of it, which is why it only ever tightens.
func info(t *sdk.Tool) ToolInfo {
	out := ToolInfo{Name: t.Name, Description: t.Description, Destructive: true, OpenWorld: true}
	if ann := t.Annotations; ann != nil {
		out.ReadOnly = ann.ReadOnlyHint
		out.Destructive = ann.DestructiveHint == nil || *ann.DestructiveHint
		out.OpenWorld = ann.OpenWorldHint == nil || *ann.OpenWorldHint
		if out.Description == "" {
			out.Description = ann.Title
		}
	}
	if out.ReadOnly {
		out.Destructive = false
	}
	return out
}

// hashTools hashes the tool list exactly as it was shown, so a server that
// grows, loses or renames a tool prompts again.
func hashTools(infos []ToolInfo) string {
	parts := make([]string, 0, len(infos)*3)
	sorted := slices.Clone(infos)
	slices.SortFunc(sorted, func(a, b ToolInfo) int { return strings.Compare(a.Name, b.Name) })
	for _, t := range sorted {
		parts = append(parts, t.Name, t.Description, annotationSuffix(t))
	}
	return trust.HashStrings(parts...)
}

func closeWith(srv *agentmcp.Server, err error) error {
	if cerr := srv.Close(); cerr != nil {
		return errors.Join(err, cerr)
	}
	return err
}

// prefix is the tool-name prefix for a server: "mcp_<name>_" unless the
// settings override it.
func prefix(name string, cfg config.MCPServer) string {
	if cfg.Prefix != "" {
		return cfg.Prefix
	}
	return "mcp_" + name + "_"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	const max = 120
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
