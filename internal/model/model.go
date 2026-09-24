// Package model decides which LLM wright talks to. The choice comes from an
// explicit name (flag, WRIGHT_MODEL, settings) or, failing that, from which
// provider credentials are present in the environment, in a fixed order, with
// a local Ollama as the last resort. Every pick is a catalog row so the status
// bar can show context size and price; providers the catalog does not list
// fall back to a documented default name.
package model

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/richardwooding/llmkit"
	"github.com/richardwooding/llmkit/catalog"

	"github.com/richardwooding/wright/internal/config"
)

// Sources of a Choice, in precedence order.
const (
	SourceFlag     = "flag"
	SourceEnv      = "env"
	SourceSettings = "settings"
	SourceDetected = "detected"
	SourceFallback = "fallback"
)

// DefaultContextWindow is assumed when neither settings nor the catalog know
// the window. 128k is the floor of every current hosted coding model.
const DefaultContextWindow = 128_000

// ErrNoProvider is returned by Detect when nothing is configured and no
// credentials or local Ollama are found.
var ErrNoProvider = errors.New("model: no provider credentials found")

// Choice is the selected model plus everything the UI wants to show about it.
type Choice struct {
	// Model is the name to hand to llmkit; it carries a "provider/" prefix
	// whenever the bare ID would resolve elsewhere (groq, openrouter).
	Model    string
	Provider string
	Info     catalog.Model
	Source   string
}

// String renders "model (provider)" for logs and the picker.
func (c Choice) String() string {
	if c.Provider == "" {
		return c.Model
	}
	return c.Model + " (" + c.Provider + ")"
}

// credential names a provider and the environment variables that prove the
// user can call it. Order is detection precedence.
var credentials = []struct {
	provider string
	envs     []string
}{
	{"anthropic", []string{"ANTHROPIC_API_KEY"}},
	{"openai", []string{"OPENAI_API_KEY"}},
	{"vertex", []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_APPLICATION_CREDENTIALS"}},
	{"xai", []string{"XAI_API_KEY"}},
	{"deepseek", []string{"DEEPSEEK_API_KEY"}},
	{"openrouter", []string{"OPENROUTER_API_KEY"}},
	{"groq", []string{"GROQ_API_KEY"}},
}

// preferred breaks ties between catalog rows that share the largest context
// window. Earlier is better; IDs not listed sort after listed ones. This is
// the one table to edit when a provider ships a better coding model.
var preferred = map[string][]string{
	"anthropic":  {"claude-opus-5", "claude-sonnet-5", "claude-fable-5-1", "claude-opus-4-8", "claude-sonnet-4-6"},
	"openai":     {"gpt-5.5", "gpt-5.4", "gpt-5", "gpt-4.1"},
	"vertex":     {"gemini-3.1-pro-preview", "gemini-3.5-flash", "gemini-2.5-pro"},
	"xai":        {"grok-4.3", "grok-4.6", "grok-4.5"},
	"deepseek":   {"deepseek-v4-pro", "deepseek-flash"},
	"groq":       {"openai/gpt-oss-120b", "openai/gpt-oss-20b"},
	"openrouter": {"anthropic/claude-sonnet-4.6"},
}

// fallbacks are used when the catalog has no row at all for a provider (the
// catalog does not seed pass-through providers such as OpenRouter).
var fallbacks = map[string]string{
	"anthropic":  "claude-sonnet-4-6",
	"openai":     "gpt-5.4",
	"vertex":     "gemini-2.5-pro",
	"xai":        "grok-4.3",
	"deepseek":   "deepseek-v4-pro",
	"openrouter": "anthropic/claude-sonnet-4.6",
	"groq":       "openai/gpt-oss-120b",
}

// coderPreference orders local Ollama tags by how well they code; matched by
// prefix so "qwen2.5-coder:7b" and "qwen2.5-coder:32b" both qualify.
var coderPreference = []string{"qwen2.5-coder", "qwen3-coder", "devstral", "deepseek-coder-v2", "codellama", "llama3"}

// Detect picks the model: flag, then WRIGHT_MODEL, then settings.Default, then
// the first provider with credentials, then a local Ollama. env and
// probeOllama are injected so tests never touch the process environment or
// the network; a nil probeOllama skips Ollama.
func Detect(ctx context.Context, settings config.Model, flag string, env func(string) string, probeOllama func(context.Context) ([]string, error)) (Choice, error) {
	if env == nil {
		env = func(string) string { return "" }
	}
	for _, src := range []struct{ name, source string }{
		{flag, SourceFlag},
		{env("WRIGHT_MODEL"), SourceEnv},
		{settings.Default, SourceSettings},
	} {
		if src.name == "" {
			continue
		}
		return named(src.name, src.source)
	}
	for _, c := range credentials {
		if hasAny(env, c.envs) {
			return Best(c.provider), nil
		}
	}
	if probeOllama != nil {
		if tags, err := probeOllama(ctx); err == nil && len(tags) > 0 {
			return ollamaChoice(tags), nil
		}
	}
	if len(settings.Endpoints) > 0 {
		// An endpoint says where, never what: wright cannot know which model
		// a self-hosted server has loaded, so it cannot pick one for the
		// user. Naming the prefix is the useful half of the answer.
		return Choice{}, fmt.Errorf("%w; you have the %s endpoint configured — name a model on it, as in `-m %s/<model>` or \"model\": {\"default\": \"%s/<model>\"}",
			ErrNoProvider, strings.Join(sortedNames(settings.Endpoints), " and "), firstName(settings.Endpoints), firstName(settings.Endpoints))
	}
	return Choice{}, fmt.Errorf("%w (set one of %s, or run Ollama)", ErrNoProvider, strings.Join(credentialNames(), ", "))
}

// named resolves a user-supplied name; an unroutable name is an error rather
// than a silent Ollama fallback, since a typo should not hit localhost.
func named(name, source string) (Choice, error) {
	provider, ok := Resolve(name)
	if !ok {
		return Choice{}, fmt.Errorf("model: no provider claims %q", name)
	}
	return Choice{Model: name, Provider: provider, Info: catalog.Lookup(name), Source: source}, nil
}

func hasAny(env func(string) string, names []string) bool {
	for _, n := range names {
		if env(n) != "" {
			return true
		}
	}
	return false
}

func credentialNames() []string {
	var out []string
	for _, c := range credentials {
		out = append(out, c.envs...)
	}
	return out
}

// Best returns the provider's strongest coding model: the tools-capable
// catalog row with the largest context window, ties broken by preferred.
// Providers absent from the catalog get their fallback name.
func Best(provider string) Choice {
	rows := toolRows(provider)
	if len(rows) == 0 {
		name := fallbacks[provider]
		return Choice{Model: qualify(provider, name), Provider: provider, Info: catalog.Lookup(name), Source: SourceFallback}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].ContextWindow != rows[j].ContextWindow {
			return rows[i].ContextWindow > rows[j].ContextWindow
		}
		return rank(provider, rows[i].ID) < rank(provider, rows[j].ID)
	})
	return fromRow(provider, rows[0], SourceDetected)
}

// Fast returns the cheapest tools-capable model of c's provider (by output
// price) for summaries and sub-agents; c.Model when the catalog has nothing.
func Fast(c Choice) string {
	rows := toolRows(c.Provider)
	best := ""
	price := 0.0
	for _, r := range rows {
		if r.Pricing.Output <= 0 {
			continue
		}
		if best == "" || r.Pricing.Output < price {
			best, price = r.ID, r.Pricing.Output
		}
	}
	if best == "" {
		return c.Model
	}
	return qualify(c.Provider, best)
}

// List returns every catalog model whose provider has credentials, in
// catalog order (provider, then ID), for the /model picker.
func List(env func(string) string, eps map[string]config.Endpoint) []Choice {
	if env == nil {
		return nil
	}
	// Configured endpoints first: they are the user's own servers, and
	// nothing else in this list would ever mention them.
	out := endpointChoices(eps)
	for _, c := range credentials {
		if !hasAny(env, c.envs) {
			continue
		}
		for _, r := range catalog.All() {
			if r.Provider == c.provider {
				out = append(out, fromRow(c.provider, r, SourceDetected))
			}
		}
	}
	return out
}

// Resolve reports which llmkit provider claims name.
func Resolve(name string) (provider string, ok bool) {
	p, _, err := llmkit.ParseModel(name)
	if err != nil {
		return "", false
	}
	return p.ID(), true
}

// ContextWindow returns the window to plan compaction against: an explicit
// settings override, else the catalog, else DefaultContextWindow. known is
// false only in the last case.
func ContextWindow(c Choice, override int) (n int, known bool) {
	switch {
	case override > 0:
		return override, true
	case c.Info.Known && c.Info.ContextWindow > 0:
		return c.Info.ContextWindow, true
	default:
		return DefaultContextWindow, false
	}
}

func toolRows(provider string) []catalog.Model {
	var rows []catalog.Model
	for _, r := range catalog.All() {
		if r.Provider == provider && r.Capabilities.Tools {
			rows = append(rows, r)
		}
	}
	return rows
}

func rank(provider, id string) int {
	prefs := preferred[provider]
	for i, p := range prefs {
		if strings.EqualFold(p, id) {
			return i
		}
	}
	return len(prefs)
}

func fromRow(provider string, r catalog.Model, source string) Choice {
	return Choice{Model: qualify(provider, r.ID), Provider: provider, Info: r, Source: source}
}

// qualify prefixes id with "provider/" when llmkit would otherwise route the
// bare name somewhere else ("openai/gpt-oss-120b" on groq, anything on
// openrouter).
func qualify(provider, id string) string {
	if p, ok := Resolve(id); ok && p == provider {
		return id
	}
	return provider + "/" + id
}

func ollamaChoice(tags []string) Choice {
	pick := tags[0]
	for _, pref := range coderPreference {
		if i := indexPrefix(tags, pref); i >= 0 {
			pick = tags[i]
			break
		}
	}
	// A bare tag without ":" routes to the Ollama fallback anyway, but the
	// explicit prefix keeps the choice unambiguous if someone changes
	// LLMKIT_PROVIDER.
	return Choice{Model: qualify("ollama", pick), Provider: "ollama", Info: catalog.Lookup(pick), Source: SourceDetected}
}

func indexPrefix(tags []string, prefix string) int {
	for i, t := range tags {
		if strings.HasPrefix(strings.ToLower(t), prefix) {
			return i
		}
	}
	return -1
}

// firstName is the endpoint named in the "no model chosen" hint.
func firstName(eps map[string]config.Endpoint) string {
	if names := sortedNames(eps); len(names) > 0 {
		return names[0]
	}
	return ""
}
