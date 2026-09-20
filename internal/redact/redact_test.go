package redact_test

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/richardwooding/wright/internal/redact"
)

const (
	anthropicKey = "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdefgh"
	openaiKey    = "sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz012345"
	ghpToken     = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"
	awsKey       = "AKIAIOSFODNN7EXAMPLE"
	jwt          = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	privateKey   = "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\nabc\n-----END RSA PRIVATE KEY-----"
)

func TestRedactTable(t *testing.T) {
	r := redact.New()
	tests := []struct {
		name      string
		in        string
		wantHits  map[string]int
		wantGone  []string // substrings that must not survive
		wantKept  []string // substrings that must survive
		wantExact string   // full expected output when deterministic
	}{
		{
			name:      "anthropic key keeps last4",
			in:        "export ANTHROPIC_API_KEY=" + anthropicKey,
			wantHits:  map[string]int{redact.NameAnthropic: 1},
			wantExact: "export ANTHROPIC_API_KEY=[redacted: anthropic…efgh]",
		},
		{
			name:     "openai key",
			in:       "key: " + openaiKey + " done",
			wantHits: map[string]int{redact.NameOpenAI: 1},
			wantGone: []string{openaiKey},
			wantKept: []string{"key: ", " done"},
		},
		{
			name:     "github token",
			in:       "token=" + ghpToken,
			wantHits: map[string]int{redact.NameGitHub: 1},
			wantGone: []string{ghpToken},
		},
		{
			name:      "github pat",
			in:        "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz",
			wantHits:  map[string]int{redact.NameGitHubPAT: 1},
			wantExact: "[redacted: github-pat…wxyz]",
		},
		{
			name:      "aws access key",
			in:        "aws_access_key_id = " + awsKey,
			wantHits:  map[string]int{redact.NameAWSAccessKey: 1},
			wantExact: "aws_access_key_id = [redacted: aws-access-key…MPLE]",
		},
		{
			name:     "aws secret key assignment",
			in:       "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			wantHits: map[string]int{redact.NameAWSSecretKey: 1},
			wantGone: []string{"wJalrXUtnFEMI"},
			wantKept: []string{"AWS_SECRET_ACCESS_KEY="},
		},
		{
			name:     "google api key",
			in:       "AIzaSyA-1234567890abcdefghijklmnopqrstu",
			wantHits: map[string]int{redact.NameGoogleAPIKey: 1},
			wantGone: []string{"AIzaSyA-1234"},
		},
		{
			name:     "gcp private_key_id",
			in:       `{"private_key_id": "0123456789abcdef0123456789abcdef01234567", "x": 1}`,
			wantHits: map[string]int{redact.NameGCPPrivateKeyID: 1},
			wantGone: []string{"0123456789abcdef0123456789abcdef01234567"},
			wantKept: []string{`"private_key_id": "`, `"x": 1`},
		},
		{
			name:     "slack token",
			in:       "xoxb-123456789012-abcdefghijkl",
			wantHits: map[string]int{redact.NameSlack: 1},
			wantGone: []string{"xoxb-1234"},
		},
		{
			name:     "stripe live key",
			in:       "sk_" + "live_abcdefghijklmnopqrstuvwxyz", // split so the fixture is not a contiguous token literal
			wantHits: map[string]int{redact.NameStripe: 1},
			wantGone: []string{"sk_" + "live_abc"}, // split so the fixture is not a contiguous token literal
		},
		{
			name:     "npm token",
			in:       "//registry.npmjs.org/:_authToken=npm_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
			wantHits: map[string]int{redact.NameNPM: 1},
			wantGone: []string{"npm_ABC"},
		},
		{
			name:     "pypi token",
			in:       "pypi-AgEIcHlwaS5vcmcCJDEyMzQ1Njc4LWFiY2QtZWZnaC1pamts",
			wantHits: map[string]int{redact.NamePyPI: 1},
			wantGone: []string{"AgEIcHlwaS5vcmcCJ"},
		},
		{
			name:     "huggingface token",
			in:       "hf" + "_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh", // split so the fixture is not a contiguous token literal
			wantHits: map[string]int{redact.NameHuggingFace: 1},
			wantGone: []string{"hf" + "_ABCDE"}, // split so the fixture is not a contiguous token literal
		},
		{
			name:     "jwt",
			in:       "Cookie: session=" + jwt,
			wantHits: map[string]int{redact.NameJWT: 1},
			wantGone: []string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"},
			wantKept: []string{"Cookie: session="},
		},
		{
			name:      "authorization bearer",
			in:        "Authorization: Bearer abcdefgh.ijklmnop",
			wantHits:  map[string]int{redact.NameAuthHeader: 1},
			wantExact: "Authorization: Bearer [redacted: auth-header…mnop]",
		},
		{
			name:     "authorization basic case-insensitive",
			in:       "authorization: basic dXNlcjpwYXNz",
			wantHits: map[string]int{redact.NameAuthHeader: 1},
			wantGone: []string{"dXNlcjpwYXNz"},
		},
		{
			name:      "url password",
			in:        "postgres://admin:s3cr3t@db.example.com:5432/app",
			wantHits:  map[string]int{redact.NameURLPassword: 1},
			wantExact: "postgres://admin:[redacted: url-password]@db.example.com:5432/app",
		},
		{
			name:      "private key block",
			in:        "cert:\n" + privateKey + "\nend",
			wantHits:  map[string]int{redact.NamePrivateKey: 1},
			wantExact: "cert:\n[redacted: private-key]\nend",
		},
		{
			name:     "multiple hits counted",
			in:       ghpToken + " " + ghpToken + " " + awsKey,
			wantHits: map[string]int{redact.NameGitHub: 2, redact.NameAWSAccessKey: 1},
		},
		// --- negatives
		{name: "git sha", in: "commit 3f9a1b2c4d5e6f708192a3b4c5d6e7f8091a2b3c", wantKept: []string{"3f9a1b2c4d5e6f708192a3b4c5d6e7f8091a2b3c"}},
		{name: "sk- in prose", in: "the task-list and risk-assessment are ready", wantKept: []string{"risk-assessment"}},
		{name: "sk- short", in: "sk-abc", wantKept: []string{"sk-abc"}},
		{name: "AKIA inside longer word", in: "AKIAABCDEFGHIJKLMNOPQRSTUVWXYZ", wantKept: []string{"AKIAABCDEFGHIJKLMNOPQRSTUVWXYZ"}},
		{name: "AKIA lowercase", in: "akiaiosfodnn7example", wantKept: []string{"akiaiosfodnn7example"}},
		{name: "non-jwt base64", in: "eyJhbGciOiJIUzI1NiJ9 alone", wantKept: []string{"eyJhbGciOiJIUzI1NiJ9"}},
		{name: "two-part dotted", in: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0", wantKept: []string{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0"}},
		{name: "url without password", in: "https://user@example.com/x", wantKept: []string{"https://user@example.com/x"}},
		{name: "url no userinfo", in: "https://example.com:8080/x?y=1", wantKept: []string{"https://example.com:8080/x?y=1"}},
		{name: "ghp too short", in: "ghp_short", wantKept: []string{"ghp_short"}},
		{name: "public key block", in: "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----", wantKept: []string{"BEGIN PUBLIC KEY"}},
		{name: "authorization word alone", in: "Authorization: Bearer", wantKept: []string{"Authorization: Bearer"}},
		{name: "generic off by default", in: "API_TOKEN=Zx9Qw2Er4Ty6Ui8Op0As1Df3Gh5Jk7Lz", wantKept: []string{"Zx9Qw2Er4Ty6Ui8Op0As1Df3Gh5Jk7Lz"}},
		{name: "empty", in: "", wantExact: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, hits := r.Redact(tt.in)
			if tt.wantExact != "" || tt.in == "" {
				if out != tt.wantExact {
					t.Errorf("Redact = %q, want %q", out, tt.wantExact)
				}
			}
			for _, g := range tt.wantGone {
				if strings.Contains(out, g) {
					t.Errorf("secret survived: %q in %q", g, out)
				}
			}
			for _, k := range tt.wantKept {
				if !strings.Contains(out, k) {
					t.Errorf("text lost: %q from %q", k, out)
				}
			}
			got := map[string]int{}
			for _, h := range hits {
				got[h.Name] = h.Count
			}
			if len(tt.wantHits) == 0 && len(got) != 0 {
				t.Errorf("unexpected hits %v (out=%q)", got, out)
			}
			for name, n := range tt.wantHits {
				if got[name] != n {
					t.Errorf("hits[%s] = %d, want %d (all: %v)", name, got[name], n, got)
				}
			}
			again, _ := r.Redact(out)
			if again != out {
				t.Errorf("not idempotent: %q → %q", out, again)
			}
		})
	}
}

func TestOptions(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		r := redact.New(redact.WithDisabled(redact.NameGitHub))
		out, hits := r.Redact(ghpToken)
		if out != ghpToken || len(hits) != 0 {
			t.Errorf("disabled pattern fired: %q %v", out, hits)
		}
		for _, n := range r.Patterns() {
			if n == redact.NameGitHub {
				t.Error("disabled pattern still listed")
			}
		}
	})
	t.Run("extra", func(t *testing.T) {
		r := redact.New(redact.WithExtra(redact.Pattern{Name: "acme", Re: regexp.MustCompile(`\bacme_[a-z0-9]{10}\b`), Keep: 2}))
		out, hits := r.Redact("id acme_0123456789 x")
		if out != "id [redacted: acme…89] x" || len(hits) != 1 || hits[0].Name != "acme" {
			t.Errorf("extra pattern: %q %v", out, hits)
		}
	})
	t.Run("generic", func(t *testing.T) {
		r := redact.New(redact.WithGeneric())
		high := "API_TOKEN=Zx9Qw2Er4Ty6Ui8Op0As1Df3Gh5Jk7Lz"
		out, hits := r.Redact(high)
		if strings.Contains(out, "Zx9Qw2Er4Ty6") || len(hits) != 1 || hits[0].Name != redact.NameGeneric {
			t.Errorf("generic high entropy: %q %v", out, hits)
		}
		low := "DB_PASSWORD=aaaaaaaaaaaaaaaaaaaaaaaa"
		if out, hits := r.Redact(low); out != low || len(hits) != 0 {
			t.Errorf("generic low entropy should be kept: %q %v", out, hits)
		}
		word := "SESSION_TOKEN = changeme_changeme_changeme"
		if out, _ := r.Redact(word); out != word {
			t.Errorf("generic word should be kept: %q", out)
		}
	})
}

func TestMarker(t *testing.T) {
	if got := redact.Marker("x", "abcdef", 4); got != "[redacted: x…cdef]" {
		t.Errorf("Marker = %q", got)
	}
	if got := redact.Marker("x", "abc", 4); got != "[redacted: x]" {
		t.Errorf("Marker short secret = %q", got)
	}
	if got := redact.Marker("x", "abcdef", 0); got != "[redacted: x]" {
		t.Errorf("Marker keep 0 = %q", got)
	}
}

func TestWriter(t *testing.T) {
	var buf bytes.Buffer
	w := redact.New().Writer(&buf)
	// Split a token across writes: the line buffer must reassemble it.
	half := len(ghpToken) / 2
	for _, chunk := range []string{"a " + ghpToken[:half], ghpToken[half:] + " b\nplain line\n", "tail " + awsKey} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(buf.String(), "tail") {
		t.Error("partial line flushed before Close")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, ghpToken) || strings.Contains(got, awsKey) {
		t.Errorf("secret leaked through writer: %q", got)
	}
	if !strings.Contains(got, "a [redacted: github…ghij] b\nplain line\ntail [redacted: aws-access-key…MPLE]") {
		t.Errorf("unexpected output: %q", got)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func FuzzRedact(f *testing.F) {
	for _, s := range []string{anthropicKey, openaiKey, ghpToken, awsKey, jwt, privateKey, "Authorization: Bearer abcdefghijkl", "://u:p@h", "[redacted: x…abcd]", "sk-", "", "eyJ.eyJ."} {
		f.Add(s)
		f.Add("prefix " + s + " suffix")
	}
	r := redact.New(redact.WithGeneric())
	f.Fuzz(func(t *testing.T, s string) {
		once, hits := r.Redact(s)
		twice, _ := r.Redact(once)
		if twice != once {
			t.Fatalf("not idempotent:\n in: %q\n 1: %q\n 2: %q", s, once, twice)
		}
		for _, h := range hits {
			if h.Count <= 0 || h.Name == "" {
				t.Fatalf("bad hit %+v", h)
			}
		}
		if len(hits) == 0 && once != s {
			t.Fatalf("text changed without hits: %q → %q", s, once)
		}
	})
}

// TestWriterMultiLineKey pins M2: the streaming writer the user's screen is
// fed from must not leak a key that the whole-buffer path redacts. It is
// line-buffered, so the multi-line private-key pattern can only be caught by
// the bounded lookbehind.
func TestWriterMultiLineKey(t *testing.T) {
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\nQyNTUxOQAAACDb\n-----END OPENSSH PRIVATE KEY-----\n"
	tests := []struct {
		name     string
		in       string
		bytewise bool
		// whole says the whole-buffer path catches this input too, so the
		// two must agree; an unterminated or over-long block is exactly the
		// case where only the writer can do anything.
		whole  bool
		want   []string
		absent []string
	}{
		{
			name:   "whole key in one write",
			in:     "before\n" + key + "after\n",
			whole:  true,
			want:   []string{"before\n", "[redacted: private-key]", "after\n"},
			absent: []string{"b3BlbnNzaC", "BEGIN OPENSSH"},
		},
		{
			name:     "key split across many tiny writes",
			in:       "before\n" + key + "after\n",
			bytewise: true,
			whole:    true,
			want:     []string{"before\n", "[redacted: private-key]", "after\n"},
			absent:   []string{"b3BlbnNzaC", "BEGIN OPENSSH"},
		},
		{
			name:   "unterminated key is redacted at Close",
			in:     "before\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n",
			want:   []string{"before\n", "[redacted: private-key]"},
			absent: []string{"b3BlbnNzaC", "BEGIN OPENSSH"},
		},
		{
			name:   "line before the marker still streams",
			in:     "tail " + ghpToken + "\n" + key,
			whole:  true,
			want:   []string{"tail [redacted: github…ghij]\n", "[redacted: private-key]"},
			absent: []string{ghpToken, "b3BlbnNzaC"},
		},
		{
			name:   "oversized block is cut at the cap and the rest dropped until END",
			in:     "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=\n", 2000) + "-----END RSA PRIVATE KEY-----\nafter\n",
			want:   []string{"[redacted: private-key]", "after\n"},
			absent: []string{"QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := redact.New().Writer(&buf)
			if tt.bytewise {
				for i := 0; i < len(tt.in); i++ {
					if _, err := w.Write([]byte{tt.in[i]}); err != nil {
						t.Fatal(err)
					}
				}
			} else if _, err := w.Write([]byte(tt.in)); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			got := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%q", want, got)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(got, absent) {
					t.Errorf("leaked %q in:\n%q", absent, got)
				}
			}
			if !tt.whole {
				return
			}
			// The streamed result must agree with the whole-buffer one on
			// the thing that matters: no key material either way.
			whole, _ := redact.New().Redact(tt.in)
			for _, absent := range tt.absent {
				if strings.Contains(whole, absent) {
					t.Errorf("whole-buffer redaction leaked %q", absent)
				}
			}
		})
	}
}

// TestWriterKeepsStreamingLive pins that the lookbehind only engages on a
// key: ordinary output is still forwarded line by line, not buffered.
func TestWriterKeepsStreamingLive(t *testing.T) {
	var buf bytes.Buffer
	w := redact.New().Writer(&buf)
	if _, err := w.Write([]byte("one\ntwo\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "one\ntwo\n" {
		t.Errorf("lines were held back: %q", got)
	}
	if _, err := w.Write([]byte("-----BEGIN EC PRIVATE KEY-----\nsecretmaterial\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "one\ntwo\n" {
		t.Errorf("key block was forwarded before its end: %q", got)
	}
	if _, err := w.Write([]byte("-----END EC PRIVATE KEY-----\nthree\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "one\ntwo\n[redacted: private-key]\nthree\n" {
		t.Errorf("unexpected output: %q", got)
	}
}

// TestWriterWithoutPrivateKeyPattern pins that disabling the pattern also
// disables the lookbehind: nothing is held back that will not be redacted.
func TestWriterWithoutPrivateKeyPattern(t *testing.T) {
	var buf bytes.Buffer
	w := redact.New(redact.WithDisabled(redact.NamePrivateKey)).Writer(&buf)
	in := "-----BEGIN RSA PRIVATE KEY-----\nabc\n"
	if _, err := w.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != in {
		t.Errorf("output held back with the pattern disabled: %q", got)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestLiteralSecret covers the pattern for a token wright was handed rather
// than one it recognised by shape. The middle two rows are the point: they
// are secrets the default patterns do not match, which is why a literal is
// not redundant with them.
func TestLiteralSecret(t *testing.T) {
	tests := []struct {
		name      string
		secret    string
		input     string
		byDefault bool // whether the shape-based defaults already catch it
	}{
		{
			name:      "a modern gh token is caught twice over",
			secret:    "gho_" + strings.Repeat("a", 36),
			input:     "token is gho_" + strings.Repeat("a", 36) + " ok",
			byDefault: true,
		},
		{
			// No default pattern matches a bare 40-character hex PAT: the
			// github pattern needs a gh?_ prefix and github-pat needs
			// github_pat_.
			name:   "a legacy hex PAT is caught only by the literal",
			secret: "0123456789abcdef0123456789abcdef01234567",
			input:  "GH_TOKEN=0123456789abcdef0123456789abcdef01234567",
		},
		{
			// \b in the default pattern means a token abutting a word
			// character does not match it; the literal has no boundary.
			name:   "a token abutting a word character",
			secret: "gho_" + strings.Repeat("b", 36),
			input:  "X" + "gho_" + strings.Repeat("b", 36),
		},
		{
			name:   "metacharacters cannot corrupt the pattern",
			secret: `a.b*c+d(e)[f]|g`,
			input:  `value=a.b*c+d(e)[f]|g end`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := redact.New(redact.WithExtra(redact.Literal("github-token", tt.secret, 4)))
			got, hits := r.Redact(tt.input)
			if strings.Contains(got, tt.secret) {
				t.Fatalf("the secret survived: %q", got)
			}
			if len(hits) == 0 {
				t.Fatalf("no pattern fired on %q", tt.input)
			}
			if !strings.Contains(got, "[redacted:") {
				t.Errorf("no marker in %q", got)
			}
			// Idempotence: the marker keeps only the last four characters,
			// so no pattern can match inside it.
			if again, _ := r.Redact(got); again != got {
				t.Errorf("not idempotent:\n first %q\nsecond %q", got, again)
			}

			// The claim that the literal is doing the work, checked rather
			// than assumed.
			plain, _ := redact.New().Redact(tt.input)
			if caught := !strings.Contains(plain, tt.secret); caught != tt.byDefault {
				t.Errorf("defaults caught it = %v, want %v (%q)", caught, tt.byDefault, plain)
			}
		})
	}
}

// A secret shorter than the marker's tail would otherwise be shown in full.
func TestLiteralKeepsNothingOfAShortSecret(t *testing.T) {
	r := redact.New(redact.WithExtra(redact.Literal("tiny", "abc", 4)))
	got, _ := r.Redact("x=abc")
	if strings.Contains(got, "abc") {
		t.Fatalf("the whole secret is in the marker: %q", got)
	}
}
