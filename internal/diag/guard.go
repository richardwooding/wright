package diag

import (
	"net"
	"net/http"
	"strings"
)

// guard wraps the whole endpoint with the checks that decide *who* is asking.
//
// The threat here is not the network — the listener is loopback — it is the
// user's own browser. A page from anywhere can make a cross-origin request to
// 127.0.0.1, and with a name whose DNS rebinds to 127.0.0.1 it can even do so
// same-origin. That matters for what is already served: a rebinding page can
// read this session's goroutine stacks and the commands it ran.
//
// Two checks answer that, and they run on every route, not only the one that
// accepts input.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never emit an Access-Control-Allow-* header. Their absence is the
		// control that makes the preflight on /debug/input fail; adding one
		// "to make curl easier" would undo it.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if !s.hostIsOurs(r.Host) {
			// DNS rebinding: the connection really did arrive on loopback,
			// but the name the client asked for is not ours.
			http.Error(w, "diag: this endpoint answers only to its own loopback address", http.StatusForbidden)
			return
		}
		if why, browser := fromBrowser(r); browser {
			http.Error(w, "diag: refused a request that came from a browser ("+why+")", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostIsOurs reports whether the Host header names the address being served.
// An IP literal is required; a name is exactly what a rebinding attack has.
func (s *Server) hostIsOurs(host string) bool {
	if host == "" {
		return true // HTTP/2 and some clients omit it; the listener is still loopback
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host // no port given
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

// fromBrowser reports whether the request carries a header only a browser
// sends. curl, wget and Go's client send none of them, so this costs an
// ordinary caller nothing and stops a page driving the endpoint even where
// the content-type check would not (a no-cors POST, whose response the page
// cannot read but whose *effect* has already happened).
func fromBrowser(r *http.Request) (string, bool) {
	if r.Header.Get("Origin") != "" {
		return "Origin", true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "none":
	default:
		return "Sec-Fetch-Site", true
	}
	switch r.Header.Get("Sec-Fetch-Mode") {
	case "cors", "no-cors", "websocket":
		return "Sec-Fetch-Mode", true
	}
	return "", false
}
