package model_test

import (
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/model"
)

// TestEndpointsRefusesWhatWouldFailConfusingly. Each of these is silent today
// and surfaces much later as a malformed request or, worse, as traffic going
// somewhere the user did not choose.
func TestEndpointsRefusesWhatWouldFailConfusingly(t *testing.T) {
	tests := []struct {
		name string
		eps  map[string]config.Endpoint
		want string // substring of the error; "" means it must succeed
	}{
		{
			// The sharp one: llmkit's Register replaces an existing ID, so
			// this would silently send every Claude call to a local box.
			name: "a built-in provider's name is refused",
			eps:  map[string]config.Endpoint{"anthropic": {BaseURL: "http://127.0.0.1:8080/v1"}},
			want: "built-in provider",
		},
		{
			name: "an alias is refused too",
			eps:  map[string]config.Endpoint{"hf": {BaseURL: "http://127.0.0.1:8080/v1"}},
			want: "built-in provider",
		},
		{
			name: "case does not evade it",
			eps:  map[string]config.Endpoint{"OpenAI": {BaseURL: "http://127.0.0.1:8080/v1"}},
			want: "built-in provider",
		},
		{
			// openaicompat has no host normalisation: this becomes a
			// malformed request URL with no useful error.
			name: "a scheme-less URL is refused",
			eps:  map[string]config.Endpoint{"lab": {BaseURL: "localhost:8080/v1"}},
			want: "absolute URL",
		},
		{
			name: "an empty URL is refused",
			eps:  map[string]config.Endpoint{"lab": {BaseURL: ""}},
			want: "absolute URL",
		},
		{
			name: "a non-http scheme is refused",
			eps:  map[string]config.Endpoint{"lab": {BaseURL: "ftp://box/v1"}},
			want: "only http and https",
		},
		{
			name: "a name with a slash is refused",
			eps:  map[string]config.Endpoint{"a/b": {BaseURL: "http://127.0.0.1:8080/v1"}},
			want: "cannot be empty or contain",
		},
		{name: "a good endpoint is accepted", eps: map[string]config.Endpoint{"lab": {BaseURL: "http://127.0.0.1:8080/v1"}}},
		{name: "no endpoints at all is fine", eps: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := model.Endpoints(tt.eps)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.want == "":
			case err == nil:
				t.Fatalf("expected an error containing %q", tt.want)
			case !strings.Contains(err.Error(), tt.want):
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestEndpointsWarnsWithoutRefusing: the two cases worth saying out loud but
// not worth blocking, since wright has not seen the server in question.
func TestEndpointsWarnsWithoutRefusing(t *testing.T) {
	tests := []struct {
		name, url, want string
	}{
		{"off-machine endpoints are named", "http://gpu.example.test:8000/v1", "not on this machine"},
		{"a missing version segment is flagged", "http://127.0.0.1:8080", "no path"},
		{"loopback with a path says nothing", "http://127.0.0.1:8080/v1", ""},
		{"localhost counts as loopback", "http://localhost:8080/v1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := model.Endpoints(map[string]config.Endpoint{"lab": {BaseURL: tt.url}})
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(warnings, "\n")
			if tt.want == "" {
				if joined != "" {
					t.Errorf("unexpected warning: %s", joined)
				}
				return
			}
			if !strings.Contains(joined, tt.want) {
				t.Errorf("warnings = %q, want one mentioning %q", joined, tt.want)
			}
		})
	}
}

// TestRamaLamaResolvesWithoutConfiguration: the common case needs no URL. The
// name must be routable *and* usable as a model prefix, which is what Detect
// requires — an unroutable name is a hard error there, not a fallback.
func TestRamaLamaResolvesWithoutConfiguration(t *testing.T) {
	if _, err := model.Endpoints(nil); err != nil {
		t.Fatal(err)
	}
	provider, ok := model.Resolve("ramalama/gpt-oss:20b")
	if !ok || provider != "ramalama" {
		t.Fatalf("Resolve = %q, %v; want the ramalama endpoint to be routable", provider, ok)
	}
	// The colon in the model name is not a routing character: ParseModel cuts
	// on the first slash only, so "gpt-oss:20b" reaches the server as written.
	if _, ok := model.Resolve("ramalama/qwen2.5-coder"); !ok {
		t.Error("a second model on the same endpoint must route too")
	}
	// And the built-ins still route as they did.
	for name, want := range map[string]string{
		"claude-opus-4-5": "anthropic", "gpt-4o": "openai", "groq/llama-3.3-70b-versatile": "groq",
	} {
		if got, ok := model.Resolve(name); !ok || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
}

// TestConfiguredEndpointsAreListed: they have no credential variable and no
// catalog row, so List's provider walk cannot see them — without this they
// would be usable with -m but never offered by /model.
func TestConfiguredEndpointsAreListed(t *testing.T) {
	eps := map[string]config.Endpoint{"lab": {BaseURL: "http://127.0.0.1:9000/v1"}}
	got := model.List(func(string) string { return "" }, eps)
	if len(got) != 1 || got[0].Provider != "lab" {
		t.Fatalf("List = %+v, want the configured endpoint", got)
	}
	// The unconfigured ramalama default is registered but not advertised:
	// listing a server that is probably not running is noise, which is why
	// Ollama is listed only when its probe answers.
	if plain := model.List(func(string) string { return "" }, nil); len(plain) != 0 {
		t.Errorf("List with no endpoints = %+v, want nothing", plain)
	}
}
