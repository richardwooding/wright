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
