package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/policy"
)

const (
	// defaultReadLimit is the page size when limit is omitted.
	defaultReadLimit = 2000
	// maxLineChars truncates pathological lines (minified bundles).
	maxLineChars = 2000
	// sniffBytes is how much of a file decides binary vs text.
	sniffBytes = 8 * 1024
	// maxImageBytes bounds an image handed to the model inline.
	maxImageBytes = 5 << 20
)

// imageMIME maps the image extensions read_file returns as parts.
var imageMIME = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

type readArgs struct {
	Path   string `json:"path" jsonschema:"file to read, absolute or workspace-relative"`
	Offset int    `json:"offset,omitempty" jsonschema:"1-based line to start from (default 1)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum lines to return (default 2000)"`
}

func (d *Deps) readFile() agentkit.Tool {
	return &tool{
		Tool: agentkit.Func(NameReadFile,
			"Read a file with cat -n style line numbers. Pages with offset/limit; refuses secret and binary files; returns images inline.",
			d.runReadFile),
		describe: d.describeRead,
	}
}

func (d *Deps) describeRead(args json.RawMessage) (policy.Request, Preview, error) {
	var a readArgs
	if err := decode(args, &a); err != nil {
		return policy.Request{}, Preview{}, err
	}
	abs, _, err := d.resolve(a.Path)
	if err != nil {
		return policy.Request{}, Preview{}, err
	}
	req := policy.Request{Tool: NameReadFile, Args: args, Paths: []string{abs}}
	return req, Preview{Title: NameReadFile + " " + d.rel(abs), Body: abs}, nil
}

func (d *Deps) runReadFile(ctx context.Context, a readArgs) (agentkit.Output, error) {
	abs, _, err := d.resolve(a.Path)
	if err != nil {
		return agentkit.Output{}, err
	}
	if err := d.refuseRead(abs); err != nil {
		return agentkit.Output{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return agentkit.Output{}, err
	}
	if info.IsDir() {
		return agentkit.Output{}, fmt.Errorf("%s is a directory; use list_dir", d.rel(abs))
	}
	f, err := os.Open(abs)
	if err != nil {
		return agentkit.Output{}, err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, sniffBytes)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if mime, ok := imageMIME[strings.ToLower(filepath.Ext(abs))]; ok {
		return d.readImage(f, head, info.Size(), mime, abs)
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return agentkit.Output{}, fmt.Errorf("%s is binary (%d bytes, %s); read_file only shows text and images",
			d.rel(abs), info.Size(), http.DetectContentType(head))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return agentkit.Output{}, err
	}
	text, err := d.readLines(f, a.Offset, a.Limit, info.Size())
	if err != nil {
		return agentkit.Output{}, err
	}
	return agentkit.Text(d.redact(ctx, NameReadFile, text)), nil
}

// readImage returns the file as an inline image part with a caption.
func (d *Deps) readImage(f *os.File, head []byte, size int64, mime, abs string) (agentkit.Output, error) {
	if size > maxImageBytes {
		return agentkit.Output{}, fmt.Errorf("%s is %d bytes; images over %d bytes are not sent to the model", d.rel(abs), size, maxImageBytes)
	}
	rest, err := io.ReadAll(f)
	if err != nil {
		return agentkit.Output{}, err
	}
	data := append(head, rest...)
	return agentkit.Output{Content: []core.Part{
		core.Text(fmt.Sprintf("%s (%s, %d bytes)", d.rel(abs), mime, len(data))),
		core.Image(data, mime),
	}}, nil
}

// readLines renders lines [offset, offset+limit) with numbers and notes
// when more remain.
func (d *Deps) readLines(r io.Reader, offset, limit int, size int64) (string, error) {
	if offset < 1 {
		offset = 1
	}
	if limit < 1 {
		limit = defaultReadLimit
	}
	if size == 0 {
		return "(empty file)", nil
	}
	br := bufio.NewReader(r)
	var b strings.Builder
	line := 0
	shown := 0
	for {
		text, err := br.ReadString('\n')
		if text == "" && err != nil {
			break
		}
		line++
		if line >= offset {
			if shown == limit {
				fmt.Fprintf(&b, "\n[truncated: %d lines shown; continue with offset=%d]", shown, line)
				return b.String(), nil
			}
			writeNumbered(&b, line, text)
			shown++
		}
		if err != nil {
			break
		}
	}
	if err := errIfShort(line, offset); err != nil {
		return "", err
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// errIfShort rejects an offset past the end of the file.
func errIfShort(lines, offset int) error {
	if offset > lines {
		return fmt.Errorf("offset %d is past the end of the file (%d lines)", offset, lines)
	}
	return nil
}

// writeNumbered appends one cat -n style line, clipping very long lines.
func writeNumbered(b *strings.Builder, n int, text string) {
	text = strings.TrimRight(text, "\n")
	if len(text) > maxLineChars {
		text = text[:maxLineChars] + "… [line truncated]"
	}
	fmt.Fprintf(b, "%6d\t%s\n", n, text)
}
