package diag_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/diag"
)

// armed opens a server whose Input hook records what it is given.
func armed(t *testing.T, arm bool, hook func(diag.Prompt) error) (*diag.Server, string, *[]diag.Prompt) {
	t.Helper()
	var got []diag.Prompt
	if hook == nil {
		hook = func(p diag.Prompt) error { got = append(got, p); return nil }
	}
	s := open(t, diag.Options{Addr: "127.0.0.1:0", Input: hook})
	var token string
	if arm {
		token = s.ArmInput(true)
	}
	return s, token, &got
}

// post sends one prompt, with every header right unless overridden.
func post(t *testing.T, s *diag.Server, token, body string, mutate func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.URL()+"debug/input", strings.NewReader(body)) //nolint:noctx // loopback test
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Wright-Debug-Token", token)
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestInputIsRefusedUntilArmed pins the consent: the route exists so the
// caller is told *how* to enable it, and refuses until a human has.
func TestInputIsRefusedUntilArmed(t *testing.T) {
	s, token, got := armed(t, false, nil)
	resp := post(t, s, token, `{"prompt":"hello"}`, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/debug inject on") {
		t.Errorf("the refusal does not say how to allow it: %s", body)
	}
	if len(*got) != 0 {
		t.Errorf("a prompt reached the hook while disarmed: %v", *got)
	}
}

func TestInputAcceptsAnArmedPrompt(t *testing.T) {
	s, token, got := armed(t, true, nil)
	resp := post(t, s, token, `{"prompt":"  summarise what you did\n  "}`, nil)
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202: %s", resp.StatusCode, body)
	}
	if len(*got) != 1 || (*got)[0].Text != "summarise what you did" {
		t.Fatalf("hook got %+v, want the trimmed text", *got)
	}
	if (*got)[0].Remote == "" {
		t.Error("the peer address was not recorded")
	}
}

// Everything a caller can get wrong, answered specifically: the person at
// the other terminal cannot debug a bare 403.
func TestInputRefusals(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		mutate func(*http.Request)
		want   int
	}{
		{
			name: "no content type", body: `{"prompt":"x"}`, want: http.StatusUnsupportedMediaType,
			mutate: func(r *http.Request) { r.Header.Del("Content-Type") },
		},
		{
			name: "form content type", body: `prompt=x`, want: http.StatusUnsupportedMediaType,
			mutate: func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") },
		},
		{
			name: "text/plain, the other simple type", body: `{"prompt":"x"}`, want: http.StatusUnsupportedMediaType,
			mutate: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		},
		{
			name: "json with a charset is fine", body: `{"prompt":"x"}`, want: http.StatusAccepted,
			mutate: func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") },
		},
		{
			name: "no token", body: `{"prompt":"x"}`, want: http.StatusForbidden,
			mutate: func(r *http.Request) { r.Header.Del("X-Wright-Debug-Token") },
		},
		{
			name: "wrong token", body: `{"prompt":"x"}`, want: http.StatusForbidden,
			mutate: func(r *http.Request) { r.Header.Set("X-Wright-Debug-Token", "nope") },
		},
		// A browser page can POST cross-origin without a preflight for the
		// simple content types; these headers are what gives it away.
		{
			name: "an Origin header", body: `{"prompt":"x"}`, want: http.StatusForbidden,
			mutate: func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		},
		{
			name: "a cross-site fetch", body: `{"prompt":"x"}`, want: http.StatusForbidden,
			mutate: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		},
		{
			name: "a no-cors fetch", body: `{"prompt":"x"}`, want: http.StatusForbidden,
			mutate: func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "no-cors") },
		},
		// DNS rebinding: the connection is on loopback, but the name asked
		// for is not ours.
		{
			name: "a rebound host name", body: `{"prompt":"x"}`, want: http.StatusForbidden,
			mutate: func(r *http.Request) { r.Host = "evil.example" },
		},
		{name: "empty prompt", body: `{"prompt":"   "}`, want: http.StatusBadRequest},
		{name: "malformed json", body: `{"prompt":`, want: http.StatusBadRequest},
		{name: "an unknown field", body: `{"prompt":"x","run":true}`, want: http.StatusBadRequest},
		{name: "oversized", body: `{"prompt":"` + strings.Repeat("a", 32<<10) + `"}`, want: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, token, got := armed(t, true, nil)
			resp := post(t, s, token, tt.body, tt.mutate)
			if resp.StatusCode != tt.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tt.want, body)
			}
			if tt.want != http.StatusAccepted && len(*got) != 0 {
				t.Errorf("a refused request still reached the hook: %v", *got)
			}
		})
	}
}

// Disarming must invalidate the token, so one read off a screen stops
// working the moment the user says stop.
func TestDisarmingInvalidatesTheToken(t *testing.T) {
	s, token, got := armed(t, true, nil)
	s.ArmInput(false)
	if s.InputArmed() {
		t.Fatal("still armed after disarming")
	}
	if resp := post(t, s, token, `{"prompt":"x"}`, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 once disarmed", resp.StatusCode)
	}
	// Re-arming must mint a fresh token, so the old one stays dead.
	if again := s.ArmInput(true); again == token {
		t.Error("re-arming reused the old token")
	}
	if len(*got) != 0 {
		t.Errorf("a prompt got through with a stale token: %v", *got)
	}
}

// A hook that cannot take the prompt says so, and the caller is told which
// of the two it was.
func TestInputMapsHookErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want int
	}{
		{name: "busy", err: diag.ErrBusy, want: http.StatusServiceUnavailable},
		{name: "closed", err: diag.ErrClosed, want: http.StatusServiceUnavailable},
		{name: "anything else", err: errors.New("boom"), want: http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, token, _ := armed(t, true, func(diag.Prompt) error { return tt.err })
			resp := post(t, s, token, `{"prompt":"x"}`, nil)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			body, _ := io.ReadAll(resp.Body)
			if tt.want == http.StatusInternalServerError && strings.Contains(string(body), "boom") {
				t.Errorf("the error text leaked to the caller: %s", body)
			}
		})
	}
}

// The read routes are GET-only now; they answered a POST as readily as a GET
// before. pprof's symbol handler is the documented exception.
func TestMethodDiscipline(t *testing.T) {
	s, _, _ := armed(t, true, nil)
	do := func(t *testing.T, method, path string) int {
		t.Helper()
		req, err := http.NewRequest(method, s.URL()+strings.TrimPrefix(path, "/"), bytes.NewReader(nil)) //nolint:noctx // loopback test
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	for _, path := range []string{"/", "/debug/state", "/debug/pprof/"} {
		t.Run("POST "+path, func(t *testing.T) {
			if got := do(t, http.MethodPost, path); got != http.StatusMethodNotAllowed {
				t.Errorf("POST %s = %d, want 405", path, got)
			}
		})
	}
	t.Run("GET /debug/input is refused", func(t *testing.T) {
		if got := do(t, http.MethodGet, "/debug/input"); got != http.StatusMethodNotAllowed {
			t.Errorf("GET /debug/input = %d, want 405", got)
		}
	})
	t.Run("POST /debug/pprof/symbol still works", func(t *testing.T) {
		if got := do(t, http.MethodPost, "/debug/pprof/symbol"); got != http.StatusOK {
			t.Errorf("POST symbol = %d, want 200 — go tool pprof uses it", got)
		}
	})
}

// The guard covers what was already being served: a rebinding page could
// read this session's stacks and the commands it ran.
func TestTheGuardProtectsTheReadRoutesToo(t *testing.T) {
	s := open(t, diag.Options{Addr: "127.0.0.1:0"})
	for _, tt := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "a rebound host", mutate: func(r *http.Request) { r.Host = "evil.example" }},
		{name: "a browser origin", mutate: func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, s.URL()+"debug/state", nil) //nolint:noctx // loopback test
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(req)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403 — this is the DNS-rebinding hole", resp.StatusCode)
			}
		})
	}
}

// Without an Input hook there is no route at all: a session that cannot
// accept prompts should not advertise that it might.
func TestNoInputHookNoRoute(t *testing.T) {
	s := open(t, diag.Options{Addr: "127.0.0.1:0"})
	resp, err := http.Post(s.URL()+"debug/input", "application/json", strings.NewReader(`{"prompt":"x"}`)) //nolint:noctx // loopback test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// The token must never appear in anything the endpoint serves: the state
// page is fetched over the port and a dump of it gets pasted into issues.
func TestTheTokenIsNeverServed(t *testing.T) {
	s, token, _ := armed(t, true, nil)
	for _, path := range []string{"", "debug/state"} {
		resp, err := http.Get(s.URL() + path) //nolint:noctx // loopback test
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if bytes.Contains(body, []byte(token)) {
			t.Errorf("/%s serves the token", path)
		}
	}
	if st := s.InputStatus(); !st.Armed || !st.Available {
		t.Errorf("InputStatus = %+v", st)
	}
	path, err := s.Dump()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(token)) {
		t.Error("a dump carries the token")
	}
}
