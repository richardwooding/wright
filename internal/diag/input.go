package diag

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// maxInputBytes bounds a prompt. A prompt is something a person types; this
// is generous for that and small enough that a full queue costs nothing.
const maxInputBytes = 16 << 10

// Prompt is text that arrived on the endpoint for the session to act on.
type Prompt struct {
	Text   string // trimmed, with control characters removed
	Remote string // the peer address, for the audit record
}

// Errors an Input hook may return, mapped to a status the caller can act on.
var (
	// ErrBusy means the session has not taken the last prompts yet.
	ErrBusy = errors.New("diag: the session is not keeping up")
	// ErrClosed means the session is shutting down.
	ErrClosed = errors.New("diag: the session is shutting down")
)

// ArmInput permits prompts for the rest of the session and returns the token
// a caller must present. Disarming clears the token, so one that was read off
// a screen stops working the moment the user says stop.
func (s *Server) ArmInput(on bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !on {
		s.armed, s.token = false, ""
		return ""
	}
	if !s.armed {
		var b [16]byte
		_, _ = rand.Read(b[:])
		s.token = base64.RawURLEncoding.EncodeToString(b[:])
	}
	s.armed = true
	return s.token
}

// InputArmed reports whether prompts are currently permitted.
func (s *Server) InputArmed() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.armed
}

// InputStatus is what /debug and the dump report. It never carries the token:
// the dump is written to a file people paste into issues.
type InputStatus struct {
	Available bool // an Input hook exists at all
	Armed     bool
	Accepted  int
	Refused   int
	Last      time.Time
	LastError string
}

// InputStatus returns the counters.
func (s *Server) InputStatus() InputStatus {
	if s == nil {
		return InputStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return InputStatus{
		Available: s.opts.Input != nil, Armed: s.armed,
		Accepted: s.accepted, Refused: s.refused, Last: s.lastAt, LastError: s.lastErr,
	}
}

func (s *Server) countRefused(reason string) {
	s.mu.Lock()
	s.refused++
	s.lastErr = reason
	s.mu.Unlock()
}

// input accepts one prompt. Every refusal is specific, because the caller is
// a person at another terminal and a bare 403 tells them nothing.
func (s *Server) input(w http.ResponseWriter, r *http.Request) {
	if !s.InputArmed() {
		s.countRefused("not armed")
		http.Error(w, "diag: this session does not accept prompts. Run `/debug inject on` in the session to allow it.", http.StatusForbidden)
		return
	}
	// application/json is the load-bearing check: it is not a CORS-simple
	// content type, so a cross-origin request must pass a preflight first,
	// and this endpoint answers no CORS headers. It also rules out a plain
	// <form> submission, which cannot produce it.
	if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || ct != "application/json" {
		s.countRefused("content type")
		http.Error(w, "diag: send Content-Type: application/json", http.StatusUnsupportedMediaType)
		return
	}
	if !s.tokenOK(r.Header.Get("X-Wright-Debug-Token")) {
		s.countRefused("token")
		http.Error(w, "diag: wrong or missing X-Wright-Debug-Token (/debug in the session prints it)", http.StatusForbidden)
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxInputBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.countRefused("too large")
			http.Error(w, "diag: a prompt may be at most 16 KiB", http.StatusRequestEntityTooLarge)
			return
		}
		s.countRefused("malformed")
		http.Error(w, `diag: send {"prompt":"…"}`, http.StatusBadRequest)
		return
	}
	text := sanitise(body.Prompt)
	if text == "" {
		s.countRefused("empty")
		http.Error(w, "diag: the prompt is empty", http.StatusBadRequest)
		return
	}
	switch err := s.opts.Input(Prompt{Text: text, Remote: r.RemoteAddr}); {
	case err == nil:
		s.mu.Lock()
		s.accepted++
		s.lastAt = s.opts.Now()
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		writeBody(w, "accepted for delivery into this session; GET /debug/state to see what became of it\n")
	case errors.Is(err, ErrBusy), errors.Is(err, ErrClosed):
		s.countRefused(err.Error())
		w.Header().Set("Retry-After", "1")
		http.Error(w, "diag: "+err.Error(), http.StatusServiceUnavailable)
	default:
		s.countRefused("delivery failed")
		http.Error(w, "diag: the prompt could not be delivered", http.StatusInternalServerError)
	}
}

func (s *Server) tokenOK(given string) bool {
	s.mu.Lock()
	want := s.token
	s.mu.Unlock()
	return want != "" && subtle.ConstantTimeCompare([]byte(given), []byte(want)) == 1
}

// sanitise trims the text and drops control characters other than newline
// and tab: this becomes a line in a transcript and a terminal renders what
// it is given.
func sanitise(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(cleaned)
}

func writeBody(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(text))
}
