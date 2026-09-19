package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Anchor is the head of an audit log as wright last wrote it: the sequence
// number of the final line and the SHA-256 of that line.
type Anchor struct {
	Log     string    `json:"log"`
	Seq     int       `json:"seq"`
	Hash    string    `json:"hash"`
	Updated time.Time `json:"updated"`
}

// Anchors records those heads outside the log's own directory — in the
// user's config/state directory, mode 0600, like trust.json.
//
// The hash chain alone only proves a log is self-consistent. Whoever can
// write the file can recompute every Prev and hand back a shorter or
// different history that verifies perfectly, so a truncated log used to
// verify clean. The anchor is the witness from outside the file: it is what
// makes "this is not the log wright wrote" detectable at all, which is why
// it never lives beside the log.
type Anchors struct {
	mu  sync.Mutex
	dir string
}

// OpenAnchors returns the anchor store kept in dir. Nothing is created until
// the first Record.
func OpenAnchors(dir string) *Anchors { return &Anchors{dir: dir} }

// Dir returns the directory the anchors live in.
func (a *Anchors) Dir() string { return a.dir }

// file is the anchor path for a log: named by the digest of the log's
// absolute path, so two workspaces cannot collide and no path component
// needs escaping.
func (a *Anchors) file(logPath string) string {
	abs, err := filepath.Abs(logPath)
	if err != nil {
		abs = logPath
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(a.dir, hex.EncodeToString(sum[:])+".json")
}

// Record stores the head of the log at logPath.
func (a *Anchors) Record(logPath string, seq int, hash string) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	abs, err := filepath.Abs(logPath)
	if err != nil {
		abs = logPath
	}
	data, err := json.Marshal(Anchor{Log: abs, Seq: seq, Hash: hash, Updated: time.Now().UTC()})
	if err != nil {
		return err
	}
	return writeAtomic(a.file(logPath), append(data, '\n'))
}

// Head returns the recorded head for logPath, if there is one.
func (a *Anchors) Head(logPath string) (Anchor, bool) {
	if a == nil {
		return Anchor{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	data, err := os.ReadFile(a.file(logPath))
	if err != nil {
		return Anchor{}, false
	}
	var an Anchor
	if err := json.Unmarshal(data, &an); err != nil {
		return Anchor{}, false
	}
	return an, true
}

// Forget drops the anchor for logPath, for a deleted session.
func (a *Anchors) Forget(logPath string) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.Remove(a.file(logPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// writeAtomic publishes data at path through a same-directory temp file, so
// a reader never sees a torn anchor and the mode is set before the rename.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".anchor-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
