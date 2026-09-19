package tools

import (
	"strings"
	"testing"
)

// readRipgrep decodes a stream, so it is tested against one directly: the
// bug this pins depended on ripgrep's file order, which varies by version
// and cannot be arranged through the public API.
func TestReadRipgrepCapCountsOnlyVisibleMatches(t *testing.T) {
	// An ignored file arrives first, as ripgrep on a CI runner did. It must
	// not consume the limit, or the caller sees one match, concludes nothing
	// was truncated and drops the truncation note.
	const stream = `{"type":"match","data":{"path":{"text":"/w/build/out.go"},"lines":{"text":"Hello ignored\n"},"line_number":1}}
{"type":"match","data":{"path":{"text":"/w/main.go"},"lines":{"text":"func main() { Hello() }\n"},"line_number":3}}
{"type":"match","data":{"path":{"text":"/w/lib/hello.go"},"lines":{"text":"// Hello greets.\n"},"line_number":3}}
{"type":"match","data":{"path":{"text":"/w/lib/hello.go"},"lines":{"text":"func Hello() {}\n"},"line_number":4}}
`
	visible := func(p string) bool { return !strings.Contains(p, "/build/") }
	got := readRipgrep(strings.NewReader(stream), grepArgs{Mode: grepModeContent, Limit: 1}, visible)

	for _, l := range got {
		if strings.Contains(l.path, "/build/") {
			t.Fatalf("an ignored path survived the reader: %q", l.path)
		}
	}
	// One past the limit is what tells the formatter to report truncation.
	if len(got) != 2 {
		t.Fatalf("read %d visible lines, want 2 (the limit plus the one that proves there are more)", len(got))
	}
	if got[0].path != "/w/main.go" {
		t.Errorf("first visible match = %q, want /w/main.go", got[0].path)
	}
}
