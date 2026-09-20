// Package diag is wright's self-inspection: a signal-triggered dump of what
// the harness is doing, and an optional loopback HTTP endpoint serving the
// same report plus net/http/pprof.
//
// It exists because the failure that matters most is the one where the UI
// itself is stuck: a tool call that never returns, an approval waiting with
// nobody to answer it, a run parked on a channel. None of that reaches the
// transcript, so the only way to see it was to read the audit log and poke at
// /proc. The signal dump always works — it needs no socket, no port and no
// working event loop — and the HTTP endpoint is the opt-in way to watch a
// live session from another terminal.
//
// This package listens; it never dials. That distinction is the whole reason
// it can hold an http import in a project whose claim is "no telemetry, no
// phone-home": a server bound to loopback at the user's explicit request
// sends nothing anywhere.
package diag

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Section is one titled block of the report. The caller builds these, so
// this package needs to know nothing about engines, policies or sessions.
type Section struct {
	Title string
	Lines []string
}

// Source produces the report at the moment it is asked. It is called from a
// signal handler and from HTTP requests, so it must be safe to call at any
// time and from any goroutine, and it must not block: a source that waits on
// the thing being debugged turns the debugger into part of the hang.
type Source func() []Section

// Options configure Open.
type Options struct {
	// Addr is the loopback address to serve on ("127.0.0.1:6060", or
	// "127.0.0.1:0" to let the kernel pick). Empty means no server: the
	// signal dump still works.
	Addr string
	// Source is the report. Required.
	Source Source
	// DumpDir is where signal dumps are written. Empty disables them.
	DumpDir string
	// Redact rewrites the report before it is written or served. A dump
	// quotes commands and arguments, which is exactly where a secret would
	// be. nil means no redaction, which is only right in tests.
	Redact func(string) string
	// Now is the clock, for tests. nil means time.Now.
	Now func() time.Time
	// OnDump is called with the path of each dump the signal handler wrote,
	// or with the error, so the UI can say so. It runs on the handler's
	// goroutine and must not block.
	OnDump func(path string, err error)
}

// Server is a running diagnostics endpoint plus its signal handler.
type Server struct {
	opts Options
	ln   net.Listener
	srv  *http.Server
	stop func() // stops the signal handler

	// mu guards last and the two fields Close clears. Addr is read from the
	// UI while Close can run from the session shutting down.
	mu   sync.Mutex
	last string // path of the most recent dump
}

// ErrNotLoopback is returned for a debug address that is not a loopback IP.
// Anything else would expose the process's stacks, its goroutine dump and a
// CPU profiler to the network.
var ErrNotLoopback = errors.New("diag: debug address must be a loopback IP")

// Open starts the endpoint if Addr is set, and installs the dump signal
// handler either way. Binding happens here, synchronously, so a port already
// in use fails the session's startup instead of vanishing into a goroutine.
func Open(o Options) (*Server, error) {
	if o.Source == nil {
		return nil, errors.New("diag: no source")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Server{opts: o}
	if o.Addr != "" {
		if err := CheckAddr(o.Addr); err != nil {
			return nil, err
		}
		ln, err := net.Listen("tcp", o.Addr)
		if err != nil {
			return nil, fmt.Errorf("diag: listen on %s: %w", o.Addr, err)
		}
		srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
		s.ln, s.srv = ln, srv
		// srv and ln are captured, not read from s: Close clears those
		// fields, and a Close that lands before this goroutine is first
		// scheduled would otherwise call Serve on a nil server.
		go func() { _ = srv.Serve(ln) }()
	}
	s.stop = notifyDump(func() {
		path, err := s.Dump()
		if o.OnDump != nil {
			o.OnDump(path, err)
		}
	})
	return s, nil
}

// CheckAddr reports whether addr is a host:port this package will bind.
//
// A bare port or an empty host would listen on every interface, and
// "localhost" depends on whatever /etc/hosts says it means, so both are
// refused in favour of writing the loopback address out.
func CheckAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("diag: %q is not host:port (try 127.0.0.1:6060): %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("diag: %q has no port (try 127.0.0.1:6060)", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%w, not %q (try 127.0.0.1:%s)", ErrNotLoopback, host, port)
	}
	return nil
}

// Addr is the address being served, with the port the kernel chose when the
// request was for port 0. Empty when no server is running.
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// URL is the address as something a user can paste into a browser.
func (s *Server) URL() string {
	if a := s.Addr(); a != "" {
		return "http://" + a + "/"
	}
	return ""
}

// LastDump is the path of the most recent dump this process wrote, or "".
func (s *Server) LastDump() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// DumpDir is where dumps are written, or "" when they are disabled.
func (s *Server) DumpDir() string {
	if s == nil {
		return ""
	}
	return s.opts.DumpDir
}

// Dump writes the report to a new file in DumpDir and returns its path.
func (s *Server) Dump() (string, error) {
	if s == nil || s.opts.DumpDir == "" {
		return "", errors.New("diag: no dump directory")
	}
	if err := os.MkdirAll(s.opts.DumpDir, 0o700); err != nil {
		return "", fmt.Errorf("diag: %w", err)
	}
	// 0600: a dump holds the commands a session ran and the stacks of the
	// process that ran them. O_EXCL, because two dumps are usually being
	// compared and the second must never quietly replace the first — two
	// signals in the same millisecond take the next free suffix instead.
	base := filepath.Join(s.opts.DumpDir, s.opts.Now().UTC().Format("20060102-150405.000"))
	path, f, err := createUnique(base)
	if err != nil {
		return "", fmt.Errorf("diag: %w", err)
	}
	_, werr := f.WriteString(s.report(true))
	cerr := f.Close()
	if werr != nil {
		return "", fmt.Errorf("diag: %w", werr)
	}
	if cerr != nil {
		return "", fmt.Errorf("diag: %w", cerr)
	}
	s.mu.Lock()
	s.last = path
	s.mu.Unlock()
	return path, nil
}

// createUnique opens base.txt, or base-2.txt … base-64.txt if that name is
// taken, never overwriting a dump that is already there.
func createUnique(base string) (string, *os.File, error) {
	for i := 1; i <= 64; i++ {
		path := base + ".txt"
		if i > 1 {
			path = fmt.Sprintf("%s-%d.txt", base, i)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		switch {
		case err == nil:
			return path, f, nil
		case !errors.Is(err, os.ErrExist):
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("%s.txt and its 63 alternatives all exist", base)
}

// Close stops the signal handler and the server.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
	s.mu.Lock()
	srv := s.srv
	s.srv, s.ln = nil, nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// report renders the current state, optionally with goroutine stacks.
func (s *Server) report(stacks bool) string {
	text := render(s.opts.Now(), s.opts.Source(), stacks)
	if s.opts.Redact != nil {
		text = s.opts.Redact(text)
	}
	return text
}
