package app_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richardwooding/wright/internal/app"
)

// armedSession builds a session with a debug endpoint and arms its input,
// returning the URL and the token.
func armedSession(t *testing.T) (*app.Built, string, string) {
	t.Helper()
	ws := isolate(t)
	setScript(t, nil)
	o := baseOpts(ws)
	o.Print = false
	o.DebugAddr = "127.0.0.1:0"
	b, err := app.Build(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	out, err := b.Command(context.Background(), "debug", []string{"inject", "on"})
	if err != nil {
		t.Fatal(err)
	}
	token := tokenFrom(t, out)
	return b, b.DebugURL(), token
}

func tokenFrom(t *testing.T, out string) string {
	t.Helper()
	const marker = "X-Wright-Debug-Token: "
	_, after, ok := strings.Cut(out, marker)
	if !ok {
		t.Fatalf("no token in the arming output:\n%s", out)
	}
	token, _, _ := strings.Cut(after, "'")
	return strings.TrimSpace(token)
}

func postPrompt(t *testing.T, url, token, prompt string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"debug/input", strings.NewReader(`{"prompt":`+jsonQuote(prompt)+`}`)) //nolint:noctx // loopback test
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Wright-Debug-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// jsonQuote is enough quoting for the plain prompts these tests send.
func jsonQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// TestDebugInjectNeedsAnEndpoint: the command must not pretend.
func TestDebugInjectNeedsAnEndpoint(t *testing.T) {
	ws := isolate(t)
	setScript(t, nil)
	b, err := app.Build(context.Background(), baseOpts(ws))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if _, err := b.Command(context.Background(), "debug", []string{"inject", "on"}); err == nil {
		t.Fatal("arming succeeded with no endpoint")
	} else if !strings.Contains(err.Error(), "--debug-addr") {
		t.Errorf("the error does not name the flag: %v", err)
	}
}

// TestArmingIsRequiredAndReversible pins the consent, end to end through a
// built session rather than the package's own fake.
func TestArmingIsRequiredAndReversible(t *testing.T) {
	b, url, token := armedSession(t)
	ctx := context.Background()

	if resp := postPrompt(t, url, token, "hello"); resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("armed POST = %d: %s", resp.StatusCode, body)
	}
	if _, err := b.Command(ctx, "debug", []string{"inject", "off"}); err != nil {
		t.Fatal(err)
	}
	if resp := postPrompt(t, url, token, "hello again"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("after disarming, POST = %d, want 403", resp.StatusCode)
	}
	// /debug and the state report both say so, and neither prints the token.
	report, err := b.Command(ctx, "debug", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "prompts") || strings.Contains(report, token) {
		t.Errorf("/debug report is wrong or leaks the token:\n%s", report)
	}
}

// TestInjectedPromptIsMarkedEverywhere: a prompt that did not come from the
// terminal must not look like one that did — in the transcript the model
// sees, on screen, or in the audit log.
func TestInjectedPromptIsMarkedEverywhere(t *testing.T) {
	b, url, token := armedSession(t)
	const prompt = "summarise what you just did"
	if resp := postPrompt(t, url, token, prompt); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// The delivery is asynchronous by design; wait for it to land.
	deadline := time.Now().Add(10 * time.Second)
	var audited []byte
	for time.Now().Before(deadline) {
		audited, _ = os.ReadFile(b.Store.AuditPath(b.SessionID))
		if bytes.Contains(audited, []byte("endpoint_prompt")) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Contains(audited, []byte("endpoint_prompt")) {
		t.Fatalf("no endpoint_prompt in the audit log:\n%s", audited)
	}
	for _, want := range []string{"endpoint_input_armed", "debug endpoint", prompt} {
		if !bytes.Contains(audited, []byte(want)) {
			t.Errorf("the audit log does not record %q", want)
		}
	}

	// And the marker reaches the model: the session transcript's user turn
	// says where it came from.
	var transcript []byte
	for time.Now().Before(deadline) {
		transcript, _ = os.ReadFile(b.Store.Dir() + "/" + b.SessionID + ".jsonl")
		if bytes.Contains(transcript, []byte("local debug endpoint")) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Contains(transcript, []byte("local debug endpoint")) {
		t.Errorf("the prompt reached the session unmarked:\n%s", transcript)
	}
}
