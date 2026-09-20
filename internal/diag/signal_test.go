//go:build unix

package diag_test

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/richardwooding/wright/internal/diag"
)

// The signal dump is the half that works when nothing else does: no port, no
// event loop, no UI. A session whose terminal is frozen is exactly the case
// this exists for, so it has to be exercised by actually signalling the
// process rather than by calling Dump directly.
func TestSignalWritesADump(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "debug")
	got := make(chan string, 4)
	s, err := diag.Open(diag.Options{
		Source:  sections,
		DumpDir: dir,
		OnDump:  func(path string, err error) { got <- path },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	var path string
	select {
	case path = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no dump within 5s of SIGUSR1")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("dump reported at %s: %v", path, err)
	}

	// After Close the handler is gone and a later signal writes nothing.
	// The test has to catch that signal itself: with no handler at all the
	// default disposition for SIGUSR1 terminates the process, which is
	// exactly what should happen to a wright that has finished, and is not
	// something a test can demonstrate by dying.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	swallow := make(chan os.Signal, 1)
	signal.Notify(swallow, syscall.SIGUSR1)
	defer signal.Stop(swallow)
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-swallow:
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal was never delivered")
	}
	select {
	case p := <-got:
		t.Fatalf("a dump (%s) was written after Close", p)
	case <-time.After(200 * time.Millisecond):
	}
}
