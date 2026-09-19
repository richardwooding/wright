// Package snapshot keeps a copy of every file before a run's tools change
// it, so /undo can put the workspace back. Content lives in a blob store
// keyed by SHA-256; each run has a JSONL index of (path → blob) entries in
// the order they were taken, and Restore replays them newest first.
package snapshot

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry records one pre-change snapshot.
type Entry struct {
	Seq    int         `json:"seq"`
	RunID  string      `json:"run"`
	Path   string      `json:"path"`
	Blob   string      `json:"blob,omitempty"` // sha256 of the content; empty when Absent
	Absent bool        `json:"absent,omitempty"`
	Mode   os.FileMode `json:"mode,omitempty"`
	Time   time.Time   `json:"time"`
	Size   int64       `json:"size,omitempty"`
}

// Store is a snapshot directory: <dir>/blobs/<sha256> and <dir>/runs/<run>.jsonl.
type Store struct {
	mu  sync.Mutex
	dir string
	seq map[string]int
}

// Open creates the store layout under dir (mode 0700).
func Open(dir string) (*Store, error) {
	for _, sub := range []string{"blobs", "runs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir, seq: map[string]int{}}, nil
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) blobPath(sum string) string  { return filepath.Join(s.dir, "blobs", sum) }
func (s *Store) runPath(runID string) string { return filepath.Join(s.dir, "runs", runID+".jsonl") }

// Save snapshots path for runID before it is modified. A missing file is
// recorded as Absent so a later Restore removes what the tool created.
func (s *Store) Save(runID, path string) (Entry, error) {
	if runID == "" || strings.ContainsAny(runID, "/\\") {
		return Entry{}, errors.New("snapshot: invalid run id")
	}
	if !filepath.IsAbs(path) {
		return Entry{}, errors.New("snapshot: path must be absolute")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := Entry{RunID: runID, Path: path, Time: time.Now().UTC()}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		e.Absent = true
	case err != nil:
		return Entry{}, err
	case !info.Mode().IsRegular():
		return Entry{}, errors.New("snapshot: not a regular file: " + path)
	default:
		sum, err := s.storeBlob(path)
		if err != nil {
			return Entry{}, err
		}
		e.Blob, e.Mode, e.Size = sum, info.Mode().Perm(), info.Size()
	}
	seq, err := s.nextSeq(runID)
	if err != nil {
		return Entry{}, err
	}
	e.Seq = seq
	if err := s.appendIndex(e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// storeBlob copies the file into the blob store (content-addressed, so a
// file snapshotted twice with the same content is stored once).
func (s *Store) storeBlob(path string) (string, error) {
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "blobs"), ".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	dst := s.blobPath(sum)
	if _, err := os.Stat(dst); err == nil {
		_ = os.Remove(tmpName)
		return sum, nil
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return sum, nil
}

// nextSeq continues a run's sequence across process restarts.
func (s *Store) nextSeq(runID string) (int, error) {
	if n, ok := s.seq[runID]; ok {
		s.seq[runID] = n + 1
		return n + 1, nil
	}
	entries, err := s.readIndex(runID)
	if err != nil {
		return 0, err
	}
	n := 0
	if len(entries) > 0 {
		n = entries[len(entries)-1].Seq
	}
	s.seq[runID] = n + 1
	return n + 1, nil
}

func (s *Store) appendIndex(e Entry) error {
	f, err := os.OpenFile(s.runPath(e.RunID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (s *Store) readIndex(runID string) ([]Entry, error) {
	f, err := os.Open(s.runPath(runID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Entries returns a run's snapshots in the order they were taken.
func (s *Store) Entries(runID string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readIndex(runID)
}

// Runs lists run IDs with snapshots, newest first (by index mtime).
func (s *Store) Runs() []string {
	entries, err := os.ReadDir(filepath.Join(s.dir, "runs"))
	if err != nil {
		return nil
	}
	type run struct {
		id string
		t  time.Time
	}
	runs := make([]run, 0, len(entries))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		runs = append(runs, run{id: strings.TrimSuffix(e.Name(), ".jsonl"), t: info.ModTime()})
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].t.Equal(runs[j].t) {
			return runs[i].id > runs[j].id
		}
		return runs[i].t.After(runs[j].t)
	})
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.id)
	}
	return out
}

// Restore puts one entry back: rewrites the content (and mode) or removes
// the file when it did not exist before.
func (s *Store) Restore(e Entry) error {
	if e.Absent {
		err := os.Remove(e.Path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	data, err := os.ReadFile(s.blobPath(e.Blob))
	if err != nil {
		return err
	}
	mode := e.Mode
	if mode == 0 {
		mode = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(e.Path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(e.Path), "."+filepath.Base(e.Path)+".undo-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, e.Path)
}

// RestoreRun undoes every snapshot of a run, newest first, so a file
// touched several times ends at its very first state. It returns the
// entries restored; on error the earlier ones stay restored.
func (s *Store) RestoreRun(runID string) ([]Entry, error) {
	entries, err := s.Entries(runID)
	if err != nil {
		return nil, err
	}
	var done []Entry
	for i := len(entries) - 1; i >= 0; i-- {
		if err := s.Restore(entries[i]); err != nil {
			return done, err
		}
		done = append(done, entries[i])
	}
	return done, nil
}

// Size reports the bytes held in the blob store.
func (s *Store) Size() int64 {
	var total int64
	entries, err := os.ReadDir(filepath.Join(s.dir, "blobs"))
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

// Prune drops the oldest runs (and the blobs only they reference) until the
// store is within maxBytes. Runs are removed whole so an undo never finds a
// half-present history.
func (s *Store) Prune(maxBytes int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := s.Runs()
	for s.Size() > maxBytes && len(runs) > 0 {
		oldest := runs[len(runs)-1]
		runs = runs[:len(runs)-1]
		if err := s.dropRun(oldest, runs); err != nil {
			return err
		}
	}
	return nil
}

// dropRun removes a run's index and any blob no remaining run references.
func (s *Store) dropRun(runID string, remaining []string) error {
	victims, err := s.readIndex(runID)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, r := range remaining {
		entries, err := s.readIndex(r)
		if err != nil {
			return err
		}
		for _, e := range entries {
			live[e.Blob] = true
		}
	}
	for _, e := range victims {
		if e.Blob != "" && !live[e.Blob] {
			if err := os.Remove(s.blobPath(e.Blob)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	delete(s.seq, runID)
	return os.Remove(s.runPath(runID))
}
