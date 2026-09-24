package model_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/richardwooding/llmkit/catalog"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/model"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func probe(tags []string, err error) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return tags, err }
}

func TestDetectPrecedence(t *testing.T) {
	tests := []struct {
		name         string
		settings     config.Model
		flag         string
		env          map[string]string
		probe        func(context.Context) ([]string, error)
		wantModel    string
		wantProvider string
		wantSource   string
		wantErr      error
	}{
		{
			name: "flag beats everything", flag: "claude-haiku-4-5",
			env:       map[string]string{"WRIGHT_MODEL": "gpt-5", "OPENAI_API_KEY": "x"},
			settings:  config.Model{Default: "grok-4.3"},
			wantModel: "claude-haiku-4-5", wantProvider: "anthropic", wantSource: model.SourceFlag,
		},
		{
			name:      "WRIGHT_MODEL beats settings",
			env:       map[string]string{"WRIGHT_MODEL": "gpt-5", "ANTHROPIC_API_KEY": "x"},
			settings:  config.Model{Default: "grok-4.3"},
			wantModel: "gpt-5", wantProvider: "openai", wantSource: model.SourceEnv,
		},
		{
			name:      "settings beat detection",
			env:       map[string]string{"ANTHROPIC_API_KEY": "x"},
			settings:  config.Model{Default: "grok-4.3"},
			wantModel: "grok-4.3", wantProvider: "xai", wantSource: model.SourceSettings,
		},
		{
			name: "provider-qualified name", flag: "groq/openai/gpt-oss-120b",
			wantModel: "groq/openai/gpt-oss-120b", wantProvider: "groq", wantSource: model.SourceFlag,
		},
		{
			name:         "anthropic detected first",
			env:          map[string]string{"ANTHROPIC_API_KEY": "x", "OPENAI_API_KEY": "y"},
			wantProvider: "anthropic", wantSource: model.SourceDetected,
		},
		{
			name:         "openai before vertex",
			env:          map[string]string{"OPENAI_API_KEY": "y", "GOOGLE_CLOUD_PROJECT": "p"},
			wantProvider: "openai", wantSource: model.SourceDetected,
		},
		{
			name:         "vertex via application credentials",
			env:          map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": "/x.json"},
			wantProvider: "vertex", wantSource: model.SourceDetected,
		},
		{
			name:         "xai then deepseek then openrouter then groq",
			env:          map[string]string{"GROQ_API_KEY": "1", "OPENROUTER_API_KEY": "1", "DEEPSEEK_API_KEY": "1"},
			wantProvider: "deepseek", wantSource: model.SourceDetected,
		},
		{
			name:      "openrouter falls back to a documented name",
			env:       map[string]string{"OPENROUTER_API_KEY": "1"},
			wantModel: "openrouter/anthropic/claude-sonnet-4.6", wantProvider: "openrouter", wantSource: model.SourceFallback,
		},
		{
			name:      "groq name is qualified so it does not route to openai",
			env:       map[string]string{"GROQ_API_KEY": "1"},
			wantModel: "groq/openai/gpt-oss-120b", wantProvider: "groq", wantSource: model.SourceDetected,
		},
		{
			name:      "ollama prefers coder tags",
			probe:     probe([]string{"llama3:8b", "qwen2.5-coder:7b", "devstral:latest"}, nil),
			wantModel: "qwen2.5-coder:7b", wantProvider: "ollama", wantSource: model.SourceDetected,
		},
		{
			name:      "ollama takes first tag when nothing is preferred",
			probe:     probe([]string{"mistral:7b", "phi3:mini"}, nil),
			wantModel: "mistral:7b", wantProvider: "ollama", wantSource: model.SourceDetected,
		},
		{
			name: "credentials beat ollama", env: map[string]string{"XAI_API_KEY": "1"},
			probe:        probe([]string{"llama3:8b"}, nil),
			wantProvider: "xai", wantSource: model.SourceDetected,
		},
		{name: "probe error is no provider", probe: probe(nil, errors.New("down")), wantErr: model.ErrNoProvider},
		{name: "empty probe is no provider", probe: probe(nil, nil), wantErr: model.ErrNoProvider},
		{name: "nothing at all", wantErr: model.ErrNoProvider},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := model.Detect(context.Background(), tt.settings, tt.flag, envOf(tt.env), tt.probe)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantModel != "" && c.Model != tt.wantModel {
				t.Errorf("model = %q, want %q", c.Model, tt.wantModel)
			}
			if c.Provider != tt.wantProvider || c.Source != tt.wantSource {
				t.Errorf("provider/source = %s/%s, want %s/%s", c.Provider, c.Source, tt.wantProvider, tt.wantSource)
			}
			if c.Source == model.SourceDetected && c.Provider != "ollama" && !c.Info.Known {
				t.Errorf("detected %s is not a catalog row", c.Model)
			}
		})
	}
}

// llmkit routes any bare name nobody claims to Ollama; Detect keeps that so a
// pulled tag such as "llama3" works without a prefix.
func TestDetectBareNameRoutesToOllama(t *testing.T) {
	c, err := model.Detect(context.Background(), config.Model{}, "llama3", nil, nil)
	if err != nil || c.Provider != "ollama" || c.Info.Known {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestBestIsLargestToolsCapable(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai", "vertex", "xai", "deepseek", "groq"} {
		t.Run(provider, func(t *testing.T) {
			c := model.Best(provider)
			if c.Provider != provider || !c.Info.Known || !c.Info.Capabilities.Tools {
				t.Fatalf("Best(%s) = %+v", provider, c)
			}
			for _, r := range catalog.All() {
				if r.Provider == provider && r.Capabilities.Tools && r.ContextWindow > c.Info.ContextWindow {
					t.Errorf("%s has a larger window than the pick %s", r.ID, c.Info.ID)
				}
			}
			if p, ok := model.Resolve(c.Model); !ok || p != provider {
				t.Errorf("Resolve(%q) = %s/%v, want %s", c.Model, p, ok, provider)
			}
		})
	}
}

func TestFast(t *testing.T) {
	tests := []struct {
		name string
		in   model.Choice
		want func(got string) bool
	}{
		{
			name: "anthropic picks the cheapest output price",
			in:   model.Best("anthropic"),
			want: func(got string) bool { return strings.HasPrefix(got, "claude-haiku") },
		},
		{
			name: "groq stays qualified",
			in:   model.Best("groq"),
			want: func(got string) bool { return got == "groq/openai/gpt-oss-20b" },
		},
		{
			name: "unknown provider keeps the model",
			in:   model.Choice{Model: "qwen2.5-coder:7b", Provider: "ollama"},
			want: func(got string) bool { return got == "qwen2.5-coder:7b" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := model.Fast(tt.in); !tt.want(got) {
				t.Errorf("Fast = %q", got)
			}
		})
	}
}

func TestList(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		providers []string
	}{
		{"nothing", nil, nil},
		{"one provider", map[string]string{"XAI_API_KEY": "1"}, []string{"xai"}},
		{"two providers in detection order", map[string]string{"GROQ_API_KEY": "1", "ANTHROPIC_API_KEY": "1"}, []string{"anthropic", "groq"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := model.List(envOf(tt.env), nil)
			var seen []string
			for _, c := range got {
				if !c.Info.Known {
					t.Errorf("%s is not a catalog row", c.Model)
				}
				if len(seen) == 0 || seen[len(seen)-1] != c.Provider {
					seen = append(seen, c.Provider)
				}
			}
			if strings.Join(seen, ",") != strings.Join(tt.providers, ",") {
				t.Errorf("providers = %v, want %v", seen, tt.providers)
			}
		})
	}
}

func TestContextWindow(t *testing.T) {
	known := model.Choice{Info: catalog.Lookup("claude-sonnet-4-5")}
	tests := []struct {
		name      string
		c         model.Choice
		override  int
		want      int
		wantKnown bool
	}{
		{"override wins", known, 50_000, 50_000, true},
		{"catalog", known, 0, known.Info.ContextWindow, true},
		{"default for unknown", model.Choice{Info: catalog.Unknown("x")}, 0, model.DefaultContextWindow, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, ok := model.ContextWindow(tt.c, tt.override)
			if n != tt.want || ok != tt.wantKnown {
				t.Errorf("= %d/%v, want %d/%v", n, ok, tt.want, tt.wantKnown)
			}
		})
	}
}

func TestProbeOllama(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen2.5-coder:7b"},{"name":"llama3:8b"}]}`))
	}))
	defer srv.Close()

	tests := []struct {
		name    string
		host    string
		want    []string
		wantErr error
	}{
		{"loopback server", srv.URL, []string{"qwen2.5-coder:7b", "llama3:8b"}, nil},
		{"non-loopback refused without OLLAMA_HOST", "http://ollama.example.invalid:11434", nil, model.ErrNotLoopback},
		{"scheme-less non-loopback refused", "10.0.0.5:11434", nil, model.ErrNotLoopback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := model.ProbeOllama(context.Background(), tt.host)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("tags = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProbeOllamaHonoursExplicitHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"devstral:latest"}]}`))
	}))
	defer srv.Close()
	// OLLAMA_HOST set means the user chose the host; the loopback guard is
	// lifted for whatever is passed or read from the variable.
	t.Setenv("OLLAMA_HOST", srv.URL)
	got, err := model.ProbeOllama(context.Background(), "")
	if err != nil || len(got) != 1 || got[0] != "devstral:latest" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestChoiceString(t *testing.T) {
	if s := (model.Choice{Model: "gpt-5", Provider: "openai"}).String(); s != "gpt-5 (openai)" {
		t.Errorf("String = %q", s)
	}
	if s := (model.Choice{Model: "x"}).String(); s != "x" {
		t.Errorf("String = %q", s)
	}
}
