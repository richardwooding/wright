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
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/debug/state", s.state)
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
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

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	var b strings.Builder
	b.WriteString("wright diagnostics\n\n")
	for _, e := range endpoints {
		fmt.Fprintf(&b, "  %-32s %s\n", e.path, e.what)
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
