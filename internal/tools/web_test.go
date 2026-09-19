package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/redact"
	"github.com/richardwooding/wright/internal/tools"
)

// fakeSite serves the pages web_fetch is tested against.
func fakeSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var agents []string
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		agents = append(agents, r.UserAgent())
		_, _ = w.Write([]byte("User-agent: other\nDisallow: /\n\nUser-agent: *\nDisallow: /private\nAllow: /private/ok\n"))
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Hello  Page</title><style>p{}</style><script>var x=1;</script></head>
<body><h1>Heading</h1><p>Some   <b>bold</b> text and <a href="/x">a link</a>.</p><script>alert(1)</script>
<ul><li>one</li><li>two</li></ul></body></html>`))
	})
	mux.HandleFunc("/private/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("allowed")) })
	mux.HandleFunc("/private/no", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("forbidden")) })
	mux.HandleFunc("/plain", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ua=" + r.UserAgent()))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("a", 2<<20)))
	})
	mux.HandleFunc("/secret", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ghp_" + strings.Repeat("E", 36) + "\n" + strings.Repeat("padding line\n", 20000)))
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0, 1, 2})
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestWebFetch(t *testing.T) {
	srv := fakeSite(t)
	f := newFixture(t, func(d *tools.Deps) {
		d.Fetch = srv.Client()
		d.Version = "1.2.3"
	})
	tests := []struct {
		name    string
		path    string
		want    []string
		absent  []string
		wantErr string
	}{
		{name: "html to text", path: "/page", want: []string{"Title: Hello  Page", "URL: " + srv.URL + "/page", "Heading\n", "Some bold text and a link.", "one\ntwo"}, absent: []string{"var x", "alert", "p{}", "<b>"}},
		{name: "plain with honest ua", path: "/plain", want: []string{"ua=wright/1.2.3 (+https://github.com/richardwooding/wright)"}},
		{name: "size cap", path: "/big", want: []string{"[body truncated at 1048576 bytes]", "[output truncated:"}},
		{name: "robots disallow", path: "/private/no", wantErr: "robots.txt"},
		{name: "robots allow override", path: "/private/ok", want: []string{"allowed"}},
		{name: "binary refused", path: "/bin", wantErr: "unsupported content type"},
		{name: "http error", path: "/missing", wantErr: "HTTP 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.text(tools.NameWebFetch, jsonArgs(map[string]any{"url": srv.URL + tt.path}))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("unexpected %q in:\n%s", a, got)
				}
			}
		})
	}
	for _, bad := range []string{"", "ftp://x/y", "/relative", "not a url"} {
		if _, err := f.text(tools.NameWebFetch, jsonArgs(map[string]any{"url": bad})); err == nil {
			t.Errorf("url %q accepted", bad)
		}
	}
	d, _ := tools.Lookup(f.ts, tools.NameWebFetch)
	req, p, err := d.Describe(json.RawMessage(`{"url":"https://example.com/a?b=1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.URL == nil || req.URL.Host != "example.com" || p.Body != "https://example.com/a?b=1" {
		t.Errorf("describe = %+v %+v", req, p)
	}
}

func TestWebFetchRobotsFailOpen(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	mux.HandleFunc("/x", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := newFixture(t, func(d *tools.Deps) { d.Fetch = srv.Client() })
	got, err := f.text(tools.NameWebFetch, jsonArgs(map[string]any{"url": srv.URL + "/x"}))
	if err != nil || !strings.Contains(got, "ok") {
		t.Errorf("got %q, %v", got, err)
	}
}

type fakeSearch struct {
	results []tools.SearchResult
	err     error
	gotQ    string
	gotN    int
}

func (s *fakeSearch) Search(_ context.Context, q string, n int) ([]tools.SearchResult, error) {
	s.gotQ, s.gotN = q, n
	return s.results, s.err
}

func TestWebSearch(t *testing.T) {
	fs := &fakeSearch{results: []tools.SearchResult{
		{Title: "One", URL: "https://a/1", Snippet: "first"},
		{Title: "Two", URL: "https://a/2"},
	}}
	f := newFixture(t, func(d *tools.Deps) { d.Search = fs })
	got, err := f.text(tools.NameWebSearch, `{"query":"go generics","limit":50}`)
	if err != nil {
		t.Fatal(err)
	}
	if fs.gotQ != "go generics" || fs.gotN != 20 {
		t.Errorf("provider got %q/%d", fs.gotQ, fs.gotN)
	}
	if want := "1. One\n   https://a/1\n   first\n2. Two\n   https://a/2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	fs.err = errors.New("quota")
	if _, err := f.text(tools.NameWebSearch, `{"query":"x"}`); err == nil {
		t.Error("provider error not surfaced")
	}
	if _, err := f.text(tools.NameWebSearch, `{"query":" "}`); err == nil {
		t.Error("empty query accepted")
	}
}

func TestOptionalToolsUnregistered(t *testing.T) {
	f := newFixture(t, nil)
	for _, name := range []string{tools.NameWebFetch, tools.NameWebSearch, tools.NameTodoWrite, tools.NameAskUser} {
		if _, ok := f.ts.Lookup(name); ok {
			t.Errorf("%s registered without its dependency", name)
		}
	}
	if _, ok := f.ts.Lookup(tools.NameBash); !ok {
		t.Error("bash missing")
	}
	f = newFixture(t, func(d *tools.Deps) { d.Sandbox = nil })
	if _, ok := f.ts.Lookup(tools.NameBash); ok {
		t.Error("bash registered without a sandbox backend")
	}
}

// TestWebFetchSpillIsRedacted pins M3 for web_fetch: the spill file Clip
// writes must hold the redacted text, not the page as it arrived.
func TestWebFetchSpillIsRedacted(t *testing.T) {
	srv := fakeSite(t)
	f := newFixture(t, func(d *tools.Deps) {
		d.Fetch = srv.Client()
		d.Redactor = redact.New()
	})
	got, err := f.text(tools.NameWebFetch, jsonArgs(map[string]any{"url": srv.URL + "/secret"}))
	if err != nil {
		t.Fatal(err)
	}
	token := "ghp_" + strings.Repeat("E", 36)
	if strings.Contains(got, token) {
		t.Errorf("result holds the unredacted secret:\n%.200s", got)
	}
	path := spillPath(t, got)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Errorf("spill file %s holds the unredacted secret", path)
	}
	if !strings.Contains(string(data), "[redacted: github") {
		t.Errorf("spill file %s has no redaction marker", path)
	}
}

// TestWebFetchRefusesLocalHosts pins the tool-side host check. The guarded
// client refuses these at dial time too; this is the cheap, early half of
// the defence, and it covers the spellings a text glob misses.
func TestWebFetchRefusesLocalHosts(t *testing.T) {
	f := newFixture(t, func(d *tools.Deps) {
		d.Fetch = http.DefaultClient
		d.AllowLocalFetch = false
	})
	d, ok := tools.Lookup(f.ts, tools.NameWebFetch)
	if !ok {
		t.Fatal("web_fetch has no Describer")
	}
	blocked := []string{
		"http://localhost:8080/x",
		"http://LOCALHOST./x",
		"http://app.localhost/x",
		"http://127.0.0.1/x",
		"http://127.1.2.3/x",
		"http://[::1]/x",
		"http://[::ffff:127.0.0.1]/x",
		"http://0.0.0.0/x",
		"http://[::]/x",
		"http://2130706433/x",
		"http://0x7f000001/x",
		"http://0177.0.0.1/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://10.0.0.5/x",
		"http://192.168.1.1/x",
		"http://172.16.9.9/x",
		"http://100.100.100.200/x",
	}
	// Describe is where the refusal has to land: the approval prompt must
	// never show a URL the tool would refuse, and it makes no request, so
	// the whole table runs offline.
	for _, raw := range blocked {
		_, _, err := d.Describe(json.RawMessage(jsonArgs(map[string]any{"url": raw})))
		if err == nil {
			t.Errorf("%s described without a refusal", raw)
		} else if !errors.Is(err, tools.ErrRefused) {
			t.Errorf("%s: err = %v, want a refusal", raw, err)
		}
	}
	// The run path refuses too, before any request is made. Port 9 on the
	// loopback is the cheap case: without the check this fails to connect
	// rather than being refused.
	if _, err := f.text(tools.NameWebFetch, jsonArgs(map[string]any{"url": "http://127.0.0.1:9/x"})); !errors.Is(err, tools.ErrRefused) {
		t.Errorf("run path err = %v, want a refusal", err)
	}
	for _, raw := range []string{"https://example.com/x", "https://8.8.8.8/x", "https://not-localhost.example/x"} {
		if _, _, err := d.Describe(json.RawMessage(jsonArgs(map[string]any{"url": raw}))); err != nil {
			t.Errorf("public url %s refused: %v", raw, err)
		}
	}
}

// TestWebFetchRobotsProductTokenIsExact pins that a group naming another
// crawler whose token merely starts with ours does not bind us.
func TestWebFetchRobotsProductTokenIsExact(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: wrightbot\nDisallow: /\n\nUser-agent: *\nDisallow: /nope\n"))
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := newFixture(t, func(d *tools.Deps) { d.Fetch = srv.Client() })
	got, err := f.text(tools.NameWebFetch, jsonArgs(map[string]any{"url": srv.URL + "/page"}))
	if err != nil || !strings.Contains(got, "ok") {
		t.Errorf("wrightbot's group captured wright: got %q, %v", got, err)
	}
	if _, err := f.text(tools.NameWebFetch, jsonArgs(map[string]any{"url": srv.URL + "/nope"})); err == nil {
		t.Error("the * group's disallow was not applied")
	}
}
