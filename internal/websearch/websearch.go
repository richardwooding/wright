// Package websearch backs the web_search tool. A provider is constructed only
// when the user has configured one, because searching means sending the
// query to a third party: with nothing configured the tool is never
// registered and wright makes no search requests at all.
package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxBody bounds a provider response; a search API answering with megabytes
// is misbehaving and the model only ever sees a handful of results.
const maxBody = 1 << 20

// Result is one hit. It mirrors what the tool renders, so the app adapts it
// to the tool's own type rather than either package importing the other.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Provider queries a search API.
type Provider interface {
	Search(ctx context.Context, query string, limit int) ([]Result, error)
}

// Config selects and configures a provider. Name is the provider id from
// settings; Key is its credential, read from the environment by the caller so
// this package never touches the process environment itself.
type Config struct {
	Name    string
	Key     string
	BaseURL string // overridden by tests; empty means the provider's own
	Client  *http.Client
}

// Errors returned by New.
var (
	ErrNoProvider = errors.New("websearch: no provider configured")
	ErrNoKey      = errors.New("websearch: provider configured but its API key is not set")
	ErrUnknown    = errors.New("websearch: unknown provider")
)

// Providers are the names New accepts.
func Providers() []string { return []string{"brave"} }

// New builds the configured provider. ErrNoProvider means the user has not
// asked for search, which is not a failure: the caller leaves the tool
// unregistered.
func New(cfg Config) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Name)) {
	case "":
		return nil, ErrNoProvider
	case "brave":
		if cfg.Key == "" {
			return nil, fmt.Errorf("%w (BRAVE_API_KEY)", ErrNoKey)
		}
		return &brave{cfg: cfg}, nil
	default:
		return nil, fmt.Errorf("%w %q (known: %s)", ErrUnknown, cfg.Name, strings.Join(Providers(), ", "))
	}
}

// KeyEnv names the environment variable a provider's credential comes from.
func KeyEnv(name string) string {
	if strings.EqualFold(strings.TrimSpace(name), "brave") {
		return "BRAVE_API_KEY"
	}
	return ""
}

const braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

type brave struct{ cfg Config }

// braveResponse is the subset of the Brave Search response wright reads.
type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

func (b *brave) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	endpoint := b.cfg.BaseURL
	if endpoint == "" {
		endpoint = braveEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("websearch: %w", err)
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", strconv.Itoa(limit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("websearch: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.cfg.Key)

	client := b.cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("websearch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The body may carry the provider's own reason; keep it short and
		// never echo the key back.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("websearch: brave returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	var out braveResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("websearch: decoding brave response: %w", err)
	}
	results := make([]Result, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		if len(results) >= limit {
			break
		}
		results = append(results, Result{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return results, nil
}
