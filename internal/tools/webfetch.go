package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/richardwooding/agentkit"
	"golang.org/x/net/html"
	"golang.org/x/time/rate"

	"github.com/richardwooding/wright/internal/policy"
)

const (
	// maxFetchBody caps the response body read from the network.
	maxFetchBody = 1 << 20
	// maxFetchText caps the text handed to the model after conversion.
	maxFetchText = 100 * 1024
	// fetchTimeout bounds one request.
	fetchTimeout = 30 * time.Second
	// fetchPerMinute is the token-bucket rate for web_fetch.
	fetchPerMinute = 10
	// userAgentName is the robots.txt product token.
	userAgentName = "wright"
)

type fetchArgs struct {
	URL    string `json:"url" jsonschema:"http or https URL to fetch"`
	Prompt string `json:"prompt,omitempty" jsonschema:"what to look for in the page (recorded for the transcript; the full text is returned)"`
}

// fetcher holds web_fetch state: the rate limiter and the robots cache.
type fetcher struct {
	limiter *rate.Limiter
	mu      sync.Mutex
	robots  map[string]*robotsRules // keyed by scheme://host
}

func (d *Deps) webFetch() agentkit.Tool {
	f := &fetcher{
		limiter: rate.NewLimiter(rate.Every(time.Minute/fetchPerMinute), fetchPerMinute),
		robots:  map[string]*robotsRules{},
	}
	return &tool{
		Tool: agentkit.Func(NameWebFetch,
			"Fetch a public URL and return its text (HTML is converted; 1 MiB limit). Honors robots.txt and is rate limited to 10 requests per minute.",
			func(ctx context.Context, a fetchArgs) (agentkit.Output, error) { return d.runFetch(ctx, f, a) }),
		describe: d.describeFetch,
	}
}

// parseFetchURL accepts absolute http(s) URLs only.
func parseFetchURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("url must be absolute http or https, got %q", raw)
	}
	return u, nil
}

func (d *Deps) describeFetch(args json.RawMessage) (policy.Request, Preview, error) {
	var a fetchArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	u, err := parseFetchURL(a.URL)
	if err != nil {
		return policy.Request{}, Preview{}, err
	}
	req := policy.Request{Tool: NameWebFetch, Args: args, URL: u}
	return req, Preview{Title: NameWebFetch + " " + u.Host, Body: u.String()}, nil
}

func (d *Deps) runFetch(ctx context.Context, f *fetcher, a fetchArgs) (agentkit.Output, error) {
	u, err := parseFetchURL(a.URL)
	if err != nil {
		return agentkit.Output{}, err
	}
	if err := f.limiter.Wait(ctx); err != nil {
		return agentkit.Output{}, err
	}
	if !f.allowed(ctx, d.Fetch, d.userAgent(), u) {
		return agentkit.Output{}, fmt.Errorf("%s disallows fetching %s for this user agent (robots.txt)", u.Host, u.Path)
	}
	body, ctype, truncated, err := d.get(ctx, u)
	if err != nil {
		return agentkit.Output{}, err
	}
	title, text, err := toText(body, ctype)
	if err != nil {
		return agentkit.Output{}, err
	}
	var b strings.Builder
	if title != "" {
		b.WriteString("Title: " + title + "\n")
	}
	b.WriteString("URL: " + u.String() + "\n")
	if truncated {
		fmt.Fprintf(&b, "[body truncated at %d bytes]\n", maxFetchBody)
	}
	b.WriteString("\n")
	b.WriteString(text)
	// Redact before Clip: Clip spills the full text to a file, so anything
	// still secret at that point is written to disk unredacted.
	out, _ := Clip(d.redact(ctx, NameWebFetch, b.String()), maxFetchText, d.SpillDir, spillID(ctx))
	return agentkit.Text(out), nil
}

// userAgent is honest about who is asking.
func (d *Deps) userAgent() string {
	v := d.Version
	if v == "" {
		v = "dev"
	}
	return userAgentName + "/" + v + " (+https://github.com/richardwooding/wright)"
}

// get performs the request with the body cap.
func (d *Deps) get(ctx context.Context, u *url.URL) (body []byte, ctype string, truncated bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", false, err
	}
	req.Header.Set("User-Agent", d.userAgent())
	req.Header.Set("Accept", "text/html, text/plain, application/json, text/*;q=0.8, */*;q=0.1")
	resp, err := d.Fetch.Do(req)
	if err != nil {
		return nil, "", false, fmt.Errorf("fetch %s: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", false, fmt.Errorf("fetch %s: HTTP %s", u.Host, resp.Status)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxFetchBody+1))
	if err != nil {
		return nil, "", false, err
	}
	if len(body) > maxFetchBody {
		body, truncated = body[:maxFetchBody], true
	}
	return body, resp.Header.Get("Content-Type"), truncated, nil
}

// toText converts the body by content type: HTML to text, text and JSON as
// they are, anything else refused.
func toText(body []byte, ctype string) (title, text string, err error) {
	mime, _, _ := strings.Cut(strings.ToLower(ctype), ";")
	mime = strings.TrimSpace(mime)
	switch {
	case mime == "text/html" || mime == "application/xhtml+xml":
		return htmlToText(body)
	case mime == "" || strings.HasPrefix(mime, "text/"), strings.HasSuffix(mime, "json"), strings.HasSuffix(mime, "xml"):
		return "", string(body), nil
	}
	return "", "", fmt.Errorf("unsupported content type %q; only text and HTML are fetched", ctype)
}

// skipElements are dropped with their content when converting HTML.
var skipElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "svg": true,
}

// blockElements are set off by a blank line; lineElements only end a line.
var (
	blockElements = map[string]bool{
		"p": true, "div": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"ul": true, "ol": true, "pre": true, "blockquote": true, "section": true, "article": true,
		"header": true, "footer": true, "nav": true, "table": true, "main": true, "aside": true, "form": true,
	}
	lineElements = map[string]bool{"br": true, "li": true, "tr": true, "hr": true, "dt": true, "dd": true}
)

var (
	spaceRun   = regexp.MustCompile(`[ \t\r\f\v]+`)
	newlineRun = regexp.MustCompile(`\n{3,}`)
)

// htmlToText keeps the visible text (link text included), drops scripts and
// styles and collapses whitespace.
func htmlToText(body []byte) (title, text string, err error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return "", "", fmt.Errorf("parse html: %w", err)
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.ElementNode:
			if n.Data == "title" && title == "" && n.FirstChild != nil {
				title = strings.TrimSpace(n.FirstChild.Data)
				return
			}
			if skipElements[n.Data] {
				return
			}
			if blockElements[n.Data] {
				b.WriteByte('\n')
			}
		case html.TextNode:
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && (blockElements[n.Data] || lineElements[n.Data]) {
			b.WriteByte('\n')
		}
	}
	walk(doc)
	return title, collapse(b.String()), nil
}

// collapse normalizes whitespace: single spaces within lines, at most one
// blank line between paragraphs, no trailing space.
func collapse(s string) string {
	s = spaceRun.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = newlineRun.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// robotsRules is the parsed robots.txt group that applies to us.
type robotsRules struct {
	disallow []string
	allow    []string
}

// allowed consults robots.txt for u's host, fetching and caching it once.
// Any failure to fetch or parse fails open: robots.txt is etiquette, and a
// missing file means "no restrictions".
func (f *fetcher) allowed(ctx context.Context, client *http.Client, ua string, u *url.URL) bool {
	key := u.Scheme + "://" + u.Host
	f.mu.Lock()
	rules, ok := f.robots[key]
	f.mu.Unlock()
	if !ok {
		rules = fetchRobots(ctx, client, ua, key)
		f.mu.Lock()
		f.robots[key] = rules
		f.mu.Unlock()
	}
	if rules == nil {
		return true
	}
	return rules.permits(u.EscapedPath())
}

// fetchRobots downloads and parses /robots.txt; nil means no rules apply.
func fetchRobots(ctx context.Context, client *http.Client, ua, origin string) *robotsRules {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/robots.txt", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBody))
	if err != nil {
		return nil
	}
	return parseRobots(string(body), userAgentName)
}

// parseRobots picks the group naming our product token, else the "*" group.
func parseRobots(text, product string) *robotsRules {
	var ours, star *robotsRules
	var current []*robotsRules
	inAgents := false
	for line := range strings.SplitSeq(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		key, val = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(val)
		switch key {
		case "user-agent":
			if !inAgents {
				current = nil
			}
			inAgents = true
			r := &robotsRules{}
			current = append(current, r)
			switch {
			case strings.EqualFold(val, product) || strings.HasPrefix(strings.ToLower(val), strings.ToLower(product)):
				ours = r
			case val == "*":
				star = r
			}
		case "disallow", "allow":
			inAgents = false
			for _, r := range current {
				if key == "allow" {
					r.allow = append(r.allow, val)
				} else if val != "" {
					r.disallow = append(r.disallow, val)
				}
			}
		default:
			inAgents = false
		}
	}
	if ours != nil {
		return ours
	}
	return star
}

// permits applies the longest-match rule: the most specific matching
// directive wins, allow on ties.
func (r *robotsRules) permits(path string) bool {
	if path == "" {
		path = "/"
	}
	best, allowed := -1, true
	for _, p := range r.disallow {
		if n := robotsMatch(p, path); n > best {
			best, allowed = n, false
		}
	}
	for _, p := range r.allow {
		if n := robotsMatch(p, path); n >= best {
			best, allowed = n, true
		}
	}
	return allowed
}

// robotsMatch reports the pattern's length when it matches path (with `*`
// wildcards and a `$` end anchor), or -1.
func robotsMatch(pattern, path string) int {
	anchored := strings.HasSuffix(pattern, "$")
	pattern = strings.TrimSuffix(pattern, "$")
	parts := strings.Split(pattern, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	expr := "^" + strings.Join(parts, ".*")
	if anchored {
		expr += "$"
	}
	re, err := regexp.Compile(expr)
	if err != nil || !re.MatchString(path) {
		return -1
	}
	return len(pattern)
}
