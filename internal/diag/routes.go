package diag

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"strings"
)

// routes is the endpoint's whole surface. pprof's handlers are registered
// here by hand rather than by relying on net/http/pprof's init, which
// attaches them to http.DefaultServeMux: this server must expose exactly
// what is listed and nothing a library happened to register globally.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// Method-qualified patterns, so anything else gets a 405 with an Allow
	// header for free. They used to be bare paths, which meant every route
	// answered a POST as readily as a GET.
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /debug/state", s.state)
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	// pprof's symbol handler documents POST as well as GET; a blanket
	// GET-only rule would break `go tool pprof`.
	mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	if s.opts.Input != nil {
		// Registered now because a mux cannot gain a route once it is
		// serving; whether it *accepts* anything is decided per request by
		// the armed flag.
		mux.HandleFunc("POST /debug/input", s.input)
	}
	return s.guard(mux)
}

// endpoints is what the index lists, in the order a person needs them.
var endpoints = []struct{ path, what string }{
	{"/debug/state", "what this session is doing right now, with goroutine stacks"},
	{"/debug/pprof/", "the pprof index"},
	{"/debug/pprof/goroutine?debug=2", "every goroutine's stack, the readable form"},
	{"/debug/pprof/heap", "heap profile"},
	{"/debug/pprof/profile?seconds=10", "10 seconds of CPU profile"},
	{"/debug/pprof/trace?seconds=5", "5 seconds of execution trace"},
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	b.WriteString("wright diagnostics\n\n")
	for _, e := range endpoints {
		fmt.Fprintf(&b, "  %-32s %s\n", e.path, e.what)
	}
	if st := s.InputStatus(); st.Available {
		b.WriteString("\n")
		if st.Armed {
			fmt.Fprintf(&b, "  %-32s %s\n", "POST /debug/input", "send this session a prompt (needs the token /debug prints)")
		} else {
			fmt.Fprintf(&b, "  %-32s %s\n", "POST /debug/input", "refused: run `/debug inject on` in the session to allow prompts")
		}
	}
	b.WriteString("\nThis endpoint is local to this machine and serves this one session.\n")
	writeText(w, b.String())
}

// state serves the same report the signal dump writes. Stacks are included
// by default because the question being asked is nearly always "what is
// everything waiting on"; ?stacks=0 leaves them out.
func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	writeText(w, s.report(r.URL.Query().Get("stacks") != "0"))
}

func writeText(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(text))
}
