package app_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/app"
	"github.com/richardwooding/wright/internal/diag"
)

// TestDebugEndpointIsOffUnlessAsked pins the default. Everything the
// endpoint serves is for the person at this machine, so it exists only when
// a flag on this one run asked for it.
func TestDebugEndpointIsOffUnlessAsked(t *testing.T) {
	ws := isolate(t)
	setScript(t, nil)
	b, err := app.Build(context.Background(), baseOpts(ws))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if url := b.DebugURL(); url != "" {
		t.Fatalf("an endpoint is serving on %s with no --debug-addr", url)
	}
	report, err := b.Command(context.Background(), "debug", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "--debug-addr") {
		t.Errorf("/debug does not say how to turn one on:\n%s", report)
	}
}

// TestDebugEndpointServesThisSession is the feature: a live session watched
// from another terminal.
func TestDebugEndpointServesThisSession(t *testing.T) {
	ws := isolate(t)
	setScript(t, nil)
	o := baseOpts(ws)
	o.DebugAddr = "127.0.0.1:0" // the kernel picks, so the test never collides
	b, err := app.Build(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	url := b.DebugURL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("DebugURL = %q", url)
	}

	resp, err := http.Get(url + "debug/state") //nolint:noctx // a loopback test request
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	state := string(body)
	// The sections a person reads first when a session has gone quiet.
	for _, want := range []string{
		"== session ==", b.SessionID, ws,
		"== engine ==", "== approvals waiting for an answer ==",
		"== tool calls that have not returned ==", "== background jobs ==",
		"== goroutines ==",
	} {
		if !strings.Contains(state, want) {
			t.Errorf("/debug/state is missing %q:\n%s", want, state)
		}
	}

	// A listening endpoint exposes this process to anything running as this
	// user, so the user is told at startup rather than left to discover it.
	var warned bool
	for _, w := range b.Warnings {
		if strings.Contains(w, url) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no startup warning names the endpoint: %v", b.Warnings)
	}

	// And /debug names it too: the status bar has the address, this has the
	// pid to signal and where the dumps go.
	report, err := b.Command(context.Background(), "debug", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{url, "debug/state", "debug/pprof/", diag.DumpSignal} {
		if want != "" && !strings.Contains(report, want) {
			t.Errorf("/debug is missing %q:\n%s", want, report)
		}
	}
}

// TestDebugDumpLandsWithTheSession pins where a dump goes: beside the
// session it describes, so deleting that session takes the dump with it.
func TestDebugDumpLandsWithTheSession(t *testing.T) {
	ws := isolate(t)
	setScript(t, nil)
	b, err := app.Build(context.Background(), baseOpts(ws))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	out, err := b.Command(context.Background(), "debug", []string{"dump"})
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimPrefix(strings.TrimSpace(out), "Dump written to ")
	if want := b.Store.DebugDir(b.SessionID); filepath.Dir(path) != want {
		t.Fatalf("dump at %s, want it under %s", path, want)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), b.SessionID) {
		t.Errorf("the dump does not name its own session:\n%s", body)
	}
	if err := b.Store.Delete(context.Background(), b.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("deleting the session left its dump behind (%v)", err)
	}
}

// A debug address that would listen beyond this machine fails the session at
// startup: a user who asked for an endpoint and silently got a different one
// is the worst outcome.
func TestBuildRefusesANonLoopbackDebugAddress(t *testing.T) {
	ws := isolate(t)
	setScript(t, nil)
	for _, addr := range []string{"0.0.0.0:0", ":6060", "192.168.1.10:6060"} {
		o := baseOpts(ws)
		o.DebugAddr = addr
		b, err := app.Build(context.Background(), o)
		if err == nil {
			_ = b.Close()
			t.Errorf("--debug-addr %s was accepted", addr)
		}
	}
}
