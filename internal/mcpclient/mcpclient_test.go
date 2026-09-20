package mcpclient_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/mcpclient"
	"github.com/richardwooding/wright/internal/trust"
)

type echoIn struct {
	Text string `json:"text" jsonschema:"text to echo"`
}

// serve starts an in-process MCP server offering the named tools (the first
// one annotated read-only) and returns a Dial that connects to it, so the
// tests never spawn a process.
func serve(t *testing.T, tools ...string) func(context.Context, string, config.MCPServer) (sdk.Transport, error) {
	t.Helper()
	return func(ctx context.Context, _ string, _ config.MCPServer) (sdk.Transport, error) {
		srv := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "1"}, nil)
		for i, name := range tools {
			ann := &sdk.ToolAnnotations{ReadOnlyHint: i == 0}
			sdk.AddTool(srv, &sdk.Tool{Name: name, Description: name + " tool", Annotations: ann},
				func(_ context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
					return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: in.Text}}}, nil, nil
				})
		}
		clientT, serverT := sdk.NewInMemoryTransports()
		session, err := srv.Connect(ctx, serverT, nil)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = session.Close() })
		return clientT, nil
	}
}

func store(t *testing.T) *trust.Store {
	t.Helper()
	return trust.Open(filepath.Join(t.TempDir(), "trust.json"))
}

func servers(cfg config.MCPServer) map[string]config.MCPServer {
	return map[string]config.MCPServer{"demo": cfg}
}

func stdioCfg() config.MCPServer {
	return config.MCPServer{Transport: "stdio", Command: "demo-server", Args: []string{"--stdio"}}
}

// recorder captures the proposals a run produced and answers with choice.
type recorder struct {
	choice    mcpclient.Choice
	proposals []mcpclient.Proposal
}

func (r *recorder) consent(_ context.Context, p mcpclient.Proposal) (mcpclient.Choice, error) {
	r.proposals = append(r.proposals, p)
	return r.choice, nil
}

func TestConnectAsksThenRemembers(t *testing.T) {
	ctx := context.Background()
	st := store(t)
	rec := &recorder{choice: mcpclient.Always}
	deps := mcpclient.Deps{Trust: st, Consent: rec.consent, Dial: serve(t, "search", "write")}

	set, err := mcpclient.Connect(ctx, servers(stdioCfg()), deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = set.Close() }()

	if len(rec.proposals) != 1 {
		t.Fatalf("want one proposal, got %d", len(rec.proposals))
	}
	p := rec.proposals[0]
	if p.Name != "demo" || p.Transport != mcpclient.TransportStdio || p.Target() != "demo-server --stdio" {
		t.Errorf("proposal = %+v", p)
	}
	if len(p.Changed) != 0 {
		t.Errorf("a first connection has nothing to diff: %v", p.Changed)
	}
	if len(p.Tools) != 2 || p.Tools[0].Name != "search" || !p.Tools[0].ReadOnly || p.Tools[1].ReadOnly {
		t.Fatalf("tools = %+v", p.Tools)
	}
	if !strings.Contains(p.String(), "server's own claims") {
		t.Errorf("the proposal text must say annotations are claims:\n%s", p.String())
	}
	if len(set.Tools) != 2 {
		t.Fatalf("tools registered = %d", len(set.Tools))
	}
	if got := set.Tools[0].Definition().Name; got != "mcp_demo_search" {
		t.Errorf("tool name = %q, want the mcp_<server>_ prefix", got)
	}
	server, tool, info, ok := set.Describe("mcp_demo_search")
	if !ok || server != "demo" || tool != "search" || !info.ReadOnly || info.Destructive {
		t.Fatalf("Describe = %q %q %+v %v", server, tool, info, ok)
	}
	if names := set.ReadOnlyNames(); len(names) != 1 || names[0] != "mcp_demo_search" {
		t.Errorf("ReadOnlyNames = %v", names)
	}
	if st.Servers() == nil || len(st.Servers()) != 1 {
		t.Fatalf("always must persist the record: %v", st.Servers())
	}

	// Second connection: the record matches, so nothing is asked.
	deps.Consent = func(context.Context, mcpclient.Proposal) (mcpclient.Choice, error) {
		t.Error("a matching record must connect silently")
		return mcpclient.Deny, nil
	}
	deps.Dial = serve(t, "search", "write")
	again, err := mcpclient.Connect(ctx, servers(stdioCfg()), deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if len(again.Servers) != 1 || !again.Servers[0].Trusted || again.Servers[0].ToolCount != 2 {
		t.Fatalf("status = %+v", again.Servers)
	}
}

func TestChangedToolListAsksAgain(t *testing.T) {
	ctx := context.Background()
	st := store(t)
	first := &recorder{choice: mcpclient.Always}
	set, err := mcpclient.Connect(ctx, servers(stdioCfg()), mcpclient.Deps{Trust: st, Consent: first.consent, Dial: serve(t, "search")})
	if err != nil {
		t.Fatal(err)
	}
	_ = set.Close()

	second := &recorder{choice: mcpclient.Once}
	changed, err := mcpclient.Connect(ctx, servers(stdioCfg()), mcpclient.Deps{Trust: st, Consent: second.consent, Dial: serve(t, "search", "exfiltrate")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = changed.Close() }()
	if len(second.proposals) != 1 {
		t.Fatalf("a new tool must re-prompt, proposals = %d", len(second.proposals))
	}
	if diff := second.proposals[0].Changed; len(diff) != 1 || diff[0] != "tool list" {
		t.Fatalf("Changed = %v, want [tool list]", diff)
	}
	if changed.Servers[0].Trusted {
		t.Error("a server accepted for once only is not trusted")
	}
	if len(st.Servers()) != 1 {
		t.Error("once must not update the stored record")
	}
}

func TestDenyKeepsToolsOut(t *testing.T) {
	rec := &recorder{choice: mcpclient.Deny}
	set, err := mcpclient.Connect(context.Background(), servers(stdioCfg()),
		mcpclient.Deps{Trust: store(t), Consent: rec.consent, Dial: serve(t, "search")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = set.Close() }()
	if len(set.Tools) != 0 {
		t.Fatalf("declined server contributed %d tools", len(set.Tools))
	}
	if len(set.Servers) != 1 || !errors.Is(set.Servers[0].Err, mcpclient.ErrDeclined) {
		t.Fatalf("status = %+v", set.Servers)
	}
	if _, _, _, ok := set.Describe("mcp_demo_search"); ok {
		t.Error("a declined server must not be describable")
	}
}

func TestNilConsentDenies(t *testing.T) {
	set, err := mcpclient.Connect(context.Background(), servers(stdioCfg()),
		mcpclient.Deps{Trust: store(t), Dial: serve(t, "search")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = set.Close() }()
	if len(set.Tools) != 0 || !errors.Is(set.Servers[0].Err, mcpclient.ErrDeclined) {
		t.Fatalf("headless must not connect an unknown server: %+v", set.Servers)
	}
}

func TestTrustedInSettingsSkipsConsent(t *testing.T) {
	cfg := stdioCfg()
	cfg.Trusted = true
	deps := mcpclient.Deps{
		Trust: store(t),
		Consent: func(context.Context, mcpclient.Proposal) (mcpclient.Choice, error) {
			t.Error("a server marked trusted in settings must not prompt")
			return mcpclient.Deny, nil
		},
		Dial: serve(t, "search"),
	}
	set, err := mcpclient.Connect(context.Background(), servers(cfg), deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = set.Close() }()
	if len(set.Tools) != 1 || !set.Servers[0].Trusted {
		t.Fatalf("status = %+v", set.Servers)
	}
}

func TestBadConfigurationIsReportedPerServer(t *testing.T) {
	cases := map[string]config.MCPServer{
		"no target":         {},
		"stdio without cmd": {Transport: "stdio"},
		"http without url":  {Transport: "http"},
		"unknown transport": {Transport: "carrier-pigeon", Command: "x"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			set, err := mcpclient.Connect(context.Background(), servers(cfg), mcpclient.Deps{Trust: store(t), Dial: serve(t, "search")})
			if err != nil {
				t.Fatal(err)
			}
			if len(set.Servers) != 1 || set.Servers[0].Err == nil {
				t.Fatalf("status = %+v", set.Servers)
			}
			if len(set.Tools) != 0 {
				t.Fatalf("tools = %d", len(set.Tools))
			}
		})
	}
}

func TestConnectWithoutServers(t *testing.T) {
	set, err := mcpclient.Connect(context.Background(), nil, mcpclient.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Tools) != 0 || len(set.Servers) != 0 {
		t.Fatalf("set = %+v", set)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestConsentFromSelect pins the mapping every front end shares, and the
// fail-closed rule behind it: the only answers that connect anything are an
// explicit pick of "once" or "remember". Everything else — no picker, no
// answer, an index that is not on the list — denies, because a server is
// someone else's code and connecting it is the irreversible half.
func TestConsentFromSelect(t *testing.T) {
	tests := []struct {
		name   string
		pick   int
		ok     bool
		want   mcpclient.Choice
		record bool
	}{
		{name: "decline", pick: 0, ok: true, want: mcpclient.Deny},
		{name: "once", pick: 1, ok: true, want: mcpclient.Once},
		{name: "remember", pick: 2, ok: true, want: mcpclient.Always, record: true},
		{name: "no answer", pick: 2, ok: false, want: mcpclient.Deny},
		{name: "an index off the list", pick: 7, ok: true, want: mcpclient.Deny},
		{name: "a negative index", pick: -1, ok: true, want: mcpclient.Deny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked string
			var offered []string
			consent := mcpclient.ConsentFromSelect("\nConnect?", func(prompt string, options []string) (int, bool) {
				asked, offered = prompt, options
				return tt.pick, tt.ok
			})
			st := store(t)
			set, err := mcpclient.Connect(context.Background(), servers(stdioCfg()),
				mcpclient.Deps{Trust: st, Consent: consent, Dial: serve(t, "search")})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = set.Close() }()

			connected := len(set.Tools) > 0
			if connected != (tt.want != mcpclient.Deny) {
				t.Errorf("tools registered = %d, want connected = %v", len(set.Tools), tt.want != mcpclient.Deny)
			}
			if recorded := len(st.Servers()) > 0; recorded != tt.record {
				t.Errorf("recorded in trust.json = %v, want %v", recorded, tt.record)
			}
			// The picker must be handed the proposal, not just a question:
			// consent to a server whose tools you were not shown is not
			// consent to anything.
			if !strings.Contains(asked, "search") || !strings.Contains(asked, "Connect?") {
				t.Errorf("prompt = %q, want the tool list and the question", asked)
			}
			if len(offered) != 3 || !strings.Contains(offered[0], "not connect") {
				t.Errorf("options = %v, want declining first", offered)
			}
		})
	}
}

// TestConsentFromSelectWithoutAPicker pins that a nil picker produces a nil
// ConsentFunc, which Connect already treats as a denial — so a front end
// that cannot ask never becomes a front end that assumes yes.
func TestConsentFromSelectWithoutAPicker(t *testing.T) {
	if got := mcpclient.ConsentFromSelect("?", nil); got != nil {
		t.Error("a nil picker must not produce a consent function")
	}
}
