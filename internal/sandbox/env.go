package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// envAllow is the allowlist of variable names (glob syntax, matched with
// path.Match semantics on the name) that reach sandboxed commands. Anything
// not listed — and not passed through by trusted settings — is dropped.
// The Go and Node variables are named one by one rather than globbed: a
// blanket GO* passed GOPROXY (which routinely carries https://user:pass@host
// credentials) and GOFLAGS, which can inject -toolexec or -ldflags into any
// build and so turn an allowed `go build` into arbitrary execution without
// the command line ever saying so. NODE_OPTIONS can inject --require the same
// way. They are locations and target selectors only.
var envAllow = []string{
	"PATH", "HOME", "LANG", "LC_*", "TERM", "XDG_*",
	"GOPATH", "GOMODCACHE", "GOCACHE", "GOOS", "GOARCH", "GOTOOLCHAIN", "GOROOT",
	"CARGO_HOME", "npm_config_cache",
	"PYTHONPATH", "VIRTUAL_ENV", "JAVA_HOME", "CI", "NO_COLOR",
	"USER", "LOGNAME", "SHELL", "TMPDIR", "TZ", "COLORTERM",
}

// envStrip are hard rules that win over the allowlist and over passthrough:
// a variable matching any of these is never forwarded, so a project cannot
// smuggle a provider key into the sandbox via sandbox.passEnv.
var envStrip = []*regexp.Regexp{
	regexp.MustCompile(`(_KEY|_TOKEN|_SECRET|_PASSWORD|_CREDENTIALS?)$`),
	regexp.MustCompile(`^AWS_`),
	regexp.MustCompile(`^(GITHUB|GH)_TOKEN$`),
	regexp.MustCompile(`^(ANTHROPIC|OPENAI|GOOGLE|GEMINI|VERTEX|XAI|DEEPSEEK|OPENROUTER|GROQ|MISTRAL|COHERE|AZURE_OPENAI|HUGGINGFACE|HF)_`),
	regexp.MustCompile(`^LLMKIT_`),
	regexp.MustCompile(`^DATABASE_URL$`),
	regexp.MustCompile(`^(NPM_TOKEN|PYPI_TOKEN|DOCKER_AUTH_CONFIG|KUBECONFIG|SSH_AUTH_SOCK|GPG_AGENT_INFO)$`),
	// Code injection through a build or runtime variable: GOFLAGS carries
	// -toolexec/-ldflags, NODE_OPTIONS carries --require, GOPROXY carries
	// credentials, GOPRIVATE/GONOSUMDB/GONOSUMCHECK/GOSUMDB turn module
	// checksum verification off. sandbox.passEnv must not reinstate them.
	regexp.MustCompile(`^(GOFLAGS|GOEXPERIMENT|GOPROXY|GOPRIVATE|GONOSUMDB|GONOSUMCHECK|GOSUMDB|GONOSUMVERIFY|GCCGO|CC|CXX)$`),
	regexp.MustCompile(`^(NODE_OPTIONS|BASH_ENV|ENV|LD_PRELOAD|LD_LIBRARY_PATH|LD_AUDIT|DYLD_INSERT_LIBRARIES|PERL5OPT|RUBYOPT|PYTHONSTARTUP)$`),
}

// Env builds the sandbox environment from the process environment: allowlist
// plus pass (trusted passthrough globs), minus the hard-strip set, plus extra.
func Env(pass []string, extra map[string]string) []string {
	return EnvFrom(os.Environ(), pass, extra)
}

// EnvFrom is Env over an explicit environ slice (for tests).
func EnvFrom(environ, pass []string, extra map[string]string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if stripped(name) {
			continue
		}
		if matchesAny(envAllow, name) || matchesAny(pass, name) {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if stripped(k) {
			continue
		}
		out = slices.DeleteFunc(out, func(kv string) bool { return strings.HasPrefix(kv, k+"=") })
		out = append(out, k+"="+extra[k])
	}
	return out
}

// stripped reports whether name matches a hard-strip rule.
func stripped(name string) bool {
	for _, re := range envStrip {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// matchesAny matches name against glob patterns ("LC_*", exact names).
func matchesAny(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
	}
	return false
}

// Caches returns the build/package caches that should stay writable inside
// the sandbox so builds are not cold every time: Go, pip, uv, pnpm, npm and
// cargo. Only directories that exist are returned.
func Caches() []string {
	home, _ := os.UserHomeDir()
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		cache = filepath.Join(home, ".cache")
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = filepath.Join(home, "go")
	}
	candidates := []string{
		envOr("GOCACHE", filepath.Join(cache, "go-build")),
		envOr("GOMODCACHE", filepath.Join(gopath, "pkg", "mod")),
		filepath.Join(cache, "pip"),
		filepath.Join(cache, "uv"),
		filepath.Join(cache, "pnpm"),
		filepath.Join(home, ".npm"),
		filepath.Join(home, ".cargo", "registry"),
		filepath.Join(home, ".cargo", "git"),
	}
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
