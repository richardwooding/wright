package model

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/richardwooding/llmkit"
	"github.com/richardwooding/llmkit/catalog"
	"github.com/richardwooding/llmkit/openaicompat"

	"github.com/richardwooding/wright/internal/config"
)

// RamaLamaEndpoint is the name and default URL of a RamaLama server, which is
// the common self-hosted case: `ramalama serve` exposes an OpenAI-compatible
// API here unless told otherwise. A user entry of the same name replaces it.
const (
	RamaLamaEndpoint = "ramalama"
	RamaLamaBaseURL  = "http://127.0.0.1:8080/v1"
)

// Endpoints registers the user's OpenAI-compatible servers so "<name>/<model>"
// resolves, and returns any warnings worth showing at startup.
//
// It must run before Detect: an unroutable name is a hard error there (a typo
// should not quietly hit localhost), so a model named after an endpoint fails
// unless that endpoint exists first.
//
// Registration is process-global — llmkit keeps one default registry, and
// wright opens its client through agentkit, which consults exactly that — so
// this is the only seam that reaches both the main client and the fast one.
func Endpoints(eps map[string]config.Endpoint) (warnings []string, err error) {
	// Validate every endpoint before registering any, so a bad one later in
	// the map cannot leave an earlier one half-applied to a global registry.
	names := sortedNames(eps)
	for _, name := range names {
		if err := validEndpoint(name, eps[name]); err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		if w := endpointWarning(name, eps[name]); w != "" {
			warnings = append(warnings, w)
		}
		register(name, eps[name])
	}
	if _, ok := eps[RamaLamaEndpoint]; !ok {
		register(RamaLamaEndpoint, config.Endpoint{BaseURL: RamaLamaBaseURL, KeyOptional: true})
	}
	return warnings, nil
}

// ours are the endpoint names this process has registered, so registering the
// same configuration twice — `wright models` after LoadEffective, a test
// calling Endpoints per case — is idempotent rather than a collision with
// "itself". Without it the built-in check below rejects wright's own work.
var (
	oursMu sync.Mutex
	ours   = map[string]bool{}
)

func registered(name string) bool {
	oursMu.Lock()
	defer oursMu.Unlock()
	return ours[strings.ToLower(name)]
}

func register(name string, ep config.Endpoint) {
	oursMu.Lock()
	ours[strings.ToLower(name)] = true
	oursMu.Unlock()
	llmkit.Register(openaicompat.NewProvider(openaicompat.Config{
		ID:        name,
		BaseURL:   strings.TrimRight(ep.BaseURL, "/"),
		APIKeyEnv: ep.APIKeyEnv,
		// A server on loopback usually wants no key at all, and demanding one
		// would make the common case fail with a message about a variable the
		// user has no reason to set.
		KeyOptional: ep.KeyOptional || ep.APIKeyEnv == "",
		Quirks:      openaicompat.Quirks{StreamUsage: true},
	}))
}

// validEndpoint refuses what would otherwise fail confusingly much later.
func validEndpoint(name string, ep config.Endpoint) error {
	// Asked of llmkit rather than kept as a list here: Lookup consults the
	// registry's own ID *and alias* map, case-insensitively, so this cannot
	// drift when llmkit adds a provider — and the drift would be silent,
	// because Register replaces a duplicate ID rather than refusing it. An
	// endpoint named "anthropic" would send every Claude call to the user's
	// own box, which is one typo away.
	if _, taken := llmkit.Default.Lookup(name); taken && !registered(name) {
		return fmt.Errorf("model: endpoint %q is the name of a built-in provider and would replace it; pick another name", name)
	}
	if strings.ContainsAny(name, "/ \t") || name == "" {
		return fmt.Errorf("model: endpoint name %q cannot be empty or contain a slash or space: it is the prefix in %q", name, name+"/<model>")
	}
	u, err := url.Parse(ep.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// openaicompat has no host normalisation, so a bare "localhost:8080"
		// becomes a malformed request URL with no useful error at all.
		return fmt.Errorf("model: endpoint %q needs an absolute URL like %s, not %q", name, RamaLamaBaseURL, ep.BaseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("model: endpoint %q has scheme %q; only http and https are supported", name, u.Scheme)
	}
	return nil
}

// endpointWarning names what is worth saying but not worth refusing.
func endpointWarning(name string, ep config.Endpoint) string {
	u, err := url.Parse(ep.BaseURL)
	if err != nil {
		return ""
	}
	if !isLoopback(u.Hostname()) {
		// The same posture as the GitHub-auth warning: this is where the
		// prompt goes, and the prompt carries the user's code.
		return fmt.Sprintf("model endpoint %q is %s, which is not on this machine: every prompt, and so every file the agent reads, is sent there", name, u.Host)
	}
	if strings.Trim(u.Path, "/") == "" {
		// Requests go to "<base>/chat/completions" with nothing inserted.
		return fmt.Sprintf("model endpoint %q has no path; these servers are usually addressed at %s/v1, and requests will go to %s/chat/completions as written", name, u.Scheme+"://"+u.Host, ep.BaseURL)
	}
	return ""
}

// endpointChoices lists the configured endpoints for the model picker. They
// have no credential variable and no catalog row, so List's provider walk
// cannot see them and they would otherwise be usable but never offered.
//
// Only endpoints the user actually configured. The built-in ramalama default
// is registered so "ramalama/<model>" resolves, but listing it unasked would
// advertise a server that is probably not running — the same reason Ollama is
// listed only when the probe answers.
func endpointChoices(eps map[string]config.Endpoint) []Choice {
	out := make([]Choice, 0, len(eps))
	for _, name := range sortedNames(eps) {
		// The model half is the user's to supply — wright cannot know what a
		// self-hosted server has loaded — so the row names the prefix and the
		// URL, which together say exactly what to type and where it goes.
		out = append(out, Choice{
			Model:    name + "/<model>",
			Provider: name,
			Info:     catalog.Model{DisplayName: eps[name].BaseURL},
			Source:   SourceSettings,
		})
	}
	return out
}

func sortedNames(eps map[string]config.Endpoint) []string {
	names := make([]string, 0, len(eps))
	for n := range eps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
