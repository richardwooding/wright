package diag_test

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/wright/internal/diag"
)

func sections() []diag.Section {
	return []diag.Section{
		{Title: "session", Lines: []string{"session:   20260920-1", "token:     ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"}},
		{Title: "approvals waiting for an answer"},
	}
}

func open(t *testing.T, o diag.Options) *diag.Server {
	t.Helper()
	if o.Source == nil {
		o.Source = sections
	}
	if o.DumpDir == "" {
		o.DumpDir = filepath.Join(t.TempDir(), "debug")
	}
	s, err := diag.Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestCheckAddrRefusesAnythingButLoopback is the security boundary of the
// whole package: everything it serves — goroutine stacks, the commands a
// session ran, a CPU profiler — is for the person at this machine.
func TestCheckAddrRefusesAnythingButLoopback(t *testing.T) {
	tests := []struct {
		name string
		addr string
		ok   bool
	}{
		{name: "loopback v4", addr: "127.0.0.1:6060", ok: true},
		{name: "another loopback v4", addr: "127.0.0.2:6060", ok: true},
		{name: "loopback v6", addr: "[::1]:6060", ok: true},
		{name: "kernel picks the port", addr: "127.0.0.1:0", ok: true},
		{name: "every interface", addr: "0.0.0.0:6060"},
		{name: "a bare port listens everywhere", addr: ":6060"},
		{name: "a routable address", addr: "192.168.1.10:6060"},
		{name: "unspecified v6", addr: "[::]:6060"},
		// localhost is whatever /etc/hosts says it is, and the point of
		// this check is not to depend on that.
		{name: "localhost is not an IP", addr: "localhost:6060"},
		{name: "no port", addr: "127.0.0.1"},
		{name: "nonsense", addr: "not an address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := diag.CheckAddr(tt.addr)
			if tt.ok {
				if err != nil {
					t.Fatalf("CheckAddr(%q) = %v, want nil", tt.addr, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckAddr(%q) = nil; it would listen beyond this machine", tt.addr)
			}
		})
	}
}

func TestOpenRefusesANonLoopbackAddress(t *testing.T) {
	_, err := diag.Open(diag.Options{Addr: "0.0.0.0:0", Source: sections})
	if !errors.Is(err, diag.ErrNotLoopback) {
		t.Fatalf("err = %v, want ErrNotLoopback", err)
	}
}

// A port already in use has to fail the caller, not a goroutine: a user who
// asked for an endpoint and silently did not get one is worse off than one
// who was told.
func TestOpenReportsABindFailure(t *testing.T) {
	first := open(t, diag.Options{Addr: "127.0.0.1:0"})
	if _, err := diag.Open(diag.Options{Addr: first.Addr(), Source: sections}); err == nil {
		t.Fatal("binding an address already in use returned no error")
	}
}

func TestServesStateAndPprof(t *testing.T) {
	s := open(t, diag.Options{Addr: "127.0.0.1:0"})
	if !strings.HasPrefix(s.URL(), "http://127.0.0.1:") {
		t.Fatalf("URL = %q", s.URL())
	}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(s.URL() + strings.TrimPrefix(path, "/")) //nolint:noctx // a loopback test request
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}

	t.Run("the index lists what is there", func(t *testing.T) {
		code, body := get("/")
		if code != http.StatusOK || !strings.Contains(body, "/debug/state") || !strings.Contains(body, "/debug/pprof/") {
			t.Fatalf("index %d:\n%s", code, body)
		}
	})

	t.Run("state carries the report and the stacks", func(t *testing.T) {
		code, body := get("/debug/state")
		for _, want := range []string{"wright diagnostics", "== session ==", "20260920-1", "== goroutines ==", "goroutine "} {
			if !strings.Contains(body, want) {
				t.Errorf("state (%d) missing %q:\n%s", code, want, body)
			}
		}
		// An empty section says so rather than looking like a missing one.
		if !strings.Contains(body, "(none)") {
			t.Errorf("an empty section is not marked:\n%s", body)
		}
	})

	t.Run("stacks can be left out", func(t *testing.T) {
		if _, body := get("/debug/state?stacks=0"); strings.Contains(body, "== goroutines ==") {
			t.Errorf("stacks=0 still included them:\n%s", body)
		}
	})

	t.Run("pprof is served", func(t *testing.T) {
		if code, body := get("/debug/pprof/"); code != http.StatusOK || !strings.Contains(body, "goroutine") {
			t.Errorf("pprof index %d:\n%s", code, body)
		}
	})

	t.Run("nothing else is", func(t *testing.T) {
		if code, _ := get("/etc/passwd"); code != http.StatusNotFound {
			t.Errorf("unknown path returned %d, want 404", code)
		}
	})
}

// A dump quotes the commands a session ran and the arguments it passed,
// which is exactly where a secret would be.
func TestDumpIsWrittenRedactedAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "debug")
	s := open(t, diag.Options{
		DumpDir: dir,
		Redact: func(in string) string {
			return strings.ReplaceAll(in, "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij", "[redacted]")
		},
	})
	path, err := s.Dump()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.LastDump(); got != path {
		t.Errorf("LastDump = %q, want %q", got, path)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("dump landed in %s, want %s", filepath.Dir(path), dir)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("dump mode = %o, want 600", perm)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "ghp_ABCDEF") {
		t.Error("the dump was written unredacted")
	}
	for _, want := range []string{"[redacted]", "== goroutines ==", "pid "} {
		if !strings.Contains(text, want) {
			t.Errorf("dump missing %q:\n%s", want, text)
		}
	}

	// A second dump in the same session must not overwrite the first: the
	// two are usually being compared.
	second, err := s.Dump()
	if err != nil {
		t.Fatal(err)
	}
	if second == path {
		t.Fatal("the second dump reused the first path")
	}
}

func TestDumpWithoutADirectoryIsAnError(t *testing.T) {
	s, err := diag.Open(diag.Options{Source: sections})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Dump(); err == nil {
		t.Fatal("dumping with no directory returned no error")
	}
}

// Nothing listens and nothing is exposed unless an address was asked for;
// the dump still works, which is the half that needs no port.
func TestNoAddrMeansNoServer(t *testing.T) {
	s := open(t, diag.Options{})
	if s.Addr() != "" || s.URL() != "" {
		t.Fatalf("Addr = %q, URL = %q, want empty", s.Addr(), s.URL())
	}
	if _, err := s.Dump(); err != nil {
		t.Fatalf("dump without a server: %v", err)
	}
}

func TestCloseStopsServing(t *testing.T) {
	s := open(t, diag.Options{Addr: "127.0.0.1:0"})
	url := s.URL()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url) //nolint:noctx // a loopback test request
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the endpoint still answers after Close")
	}
}
