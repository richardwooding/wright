// Package session stores conversations per workspace: the lossless transcript
// lives in an agentkit FileStore (append-only JSONL) and wright's own facts
// about a session (title, model, usage, cost, mode) live in a meta/<id>.meta.json
// sidecar beside it, so the transcript format stays agentkit's and the sidecar
// can be rewritten freely.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/richardwooding/agentkit"
	"github.com/richardwooding/llmkit/core"

	"github.com/richardwooding/wright/internal/config"
	"github.com/richardwooding/wright/internal/workspace"
)

// ErrBadID rejects IDs that could escape the sessions directory.
var ErrBadID = errors.New("session: invalid session id")

// idPattern is the shape NewID produces plus enough slack for hand-typed or
// future IDs; the point is to exclude separators and dot-dot.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Meta is the sidecar record for one session.
type Meta struct {
	ID        string     `json:"id"`
	Title     string     `json:"title,omitempty"`
	Model     string     `json:"model,omitempty"`
	Workspace string     `json:"workspace,omitempty"`
	Created   time.Time  `json:"created"`
	Updated   time.Time  `json:"updated"`
	Turns     int        `json:"turns,omitempty"`
	Usage     core.Usage `json:"usage,omitzero"`
	CostUSD   float64    `json:"cost_usd,omitempty"`
	Mode      string     `json:"mode,omitempty"`
}

// metaSuffix names sidecars. They live in a "meta" subdirectory rather than
// beside the transcripts because FileStore.List reads every *.json in its
// directory as a legacy transcript (and skips subdirectories).
const metaSuffix = ".meta.json"

// Store is the per-workspace session directory. It embeds the agentkit
// FileStore so it satisfies agentkit.Store and Lister directly.
type Store struct {
	*agentkit.FileStore
	project string // <dataDir>/projects/<hash>
	dir     string // <project>/sessions
	meta    string // <dir>/meta
}

// Open prepares <dataDir>/projects/<ws.Hash()>/sessions (0700) and returns
// the store. Nothing else is read until a method is called.
func Open(dataDir string, ws *workspace.Workspace) (*Store, error) {
	project := filepath.Join(dataDir, "projects", ws.Hash())
	dir := filepath.Join(project, "sessions")
	meta := filepath.Join(dir, "meta")
	if err := os.MkdirAll(meta, 0o700); err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	return &Store{FileStore: agentkit.NewFileStore(dir), project: project, dir: dir, meta: meta}, nil
}

// Dir is the sessions directory (transcripts).
func (s *Store) Dir() string { return s.dir }

// MetaDir is where the <id>.meta.json sidecars live.
func (s *Store) MetaDir() string { return s.meta }

// AuditPath is where the audit log for id lives (the audit package writes it).
func (s *Store) AuditPath(id string) string {
	return filepath.Join(s.project, "audit", id+".jsonl")
}

// SnapshotDir is where pre-edit snapshots for id live (the snapshot package
// writes them).
func (s *Store) SnapshotDir(id string) string {
	return filepath.Join(s.project, "snapshots", id)
}

// NewID returns a sortable, human-readable ID: "20260919-153012-a1b2". The
// timestamp makes listings chronological without reading files; the random
// suffix keeps two sessions started in the same second apart.
func NewID(now time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return now.Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// ValidID reports whether id is safe to use as a file name component.
func ValidID(id string) bool { return idPattern.MatchString(id) }

func (s *Store) metaPath(id string) string { return filepath.Join(s.meta, id+metaSuffix) }

// List merges the FileStore's view (one entry per transcript) with the
// sidecars: a sidecar wins for Title, Model, Usage, Cost, Turns and Mode,
// the transcript supplies Created/Updated and a fallback title. Sessions with
// a sidecar but no transcript yet are included too. Most recent first.
func (s *Store) List(ctx context.Context) ([]Meta, error) {
	infos, err := s.FileStore.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	metas := map[string]Meta{}
	for _, info := range infos {
		metas[info.ID] = Meta{ID: info.ID, Title: info.Title, Created: info.Created, Updated: info.Updated}
	}
	sidecars, err := s.readSidecars()
	if err != nil {
		return nil, err
	}
	for id, m := range sidecars {
		metas[id] = merge(metas[id], m)
	}
	out := make([]Meta, 0, len(metas))
	for _, m := range metas {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.After(out[j].Updated)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// merge lays sidecar over the transcript-derived base. Timestamps take the
// later Updated and the earlier non-zero Created so neither source can make a
// session look newer or older than it is.
func merge(base, side Meta) Meta {
	m := side
	m.ID = base.ID
	if m.ID == "" {
		m.ID = side.ID
	}
	if m.Title == "" {
		m.Title = base.Title
	}
	if m.Created.IsZero() || (!base.Created.IsZero() && base.Created.Before(m.Created)) {
		m.Created = base.Created
	}
	if base.Updated.After(m.Updated) {
		m.Updated = base.Updated
	}
	return m
}

func (s *Store) readSidecars() (map[string]Meta, error) {
	entries, err := os.ReadDir(s.meta)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	out := map[string]Meta{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), metaSuffix)
		if !ok || e.IsDir() {
			continue
		}
		m, found, err := s.readMeta(id)
		if err != nil {
			return nil, err
		}
		if found {
			out[id] = m
		}
	}
	return out, nil
}

func (s *Store) readMeta(id string) (Meta, bool, error) {
	data, err := os.ReadFile(s.metaPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, false, nil
	}
	if err != nil {
		return Meta{}, false, fmt.Errorf("session: %w", err)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, false, fmt.Errorf("session: %s: %w", s.metaPath(id), err)
	}
	m.ID = id
	return m, true, nil
}

// Get returns the merged Meta for id; found is false when neither a
// transcript nor a sidecar exists.
func (s *Store) Get(ctx context.Context, id string) (Meta, bool, error) {
	if !ValidID(id) {
		return Meta{}, false, ErrBadID
	}
	all, err := s.List(ctx)
	if err != nil {
		return Meta{}, false, err
	}
	for _, m := range all {
		if m.ID == id {
			return m, true, nil
		}
	}
	return Meta{}, false, nil
}

// Touch writes m's sidecar atomically (0600). A zero Updated is set to now
// so callers can pass a Meta straight from Get.
func (s *Store) Touch(_ context.Context, m Meta) error {
	if !ValidID(m.ID) {
		return ErrBadID
	}
	if m.Updated.IsZero() {
		m.Updated = time.Now()
	}
	if m.Created.IsZero() {
		m.Created = m.Updated
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := config.WriteAtomic(s.metaPath(m.ID), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	return nil
}

// Latest returns the most recently updated session, for --continue.
func (s *Store) Latest(ctx context.Context) (Meta, bool, error) {
	all, err := s.List(ctx)
	if err != nil || len(all) == 0 {
		return Meta{}, false, err
	}
	return all[0], true, nil
}

// Delete removes the transcript, the sidecar, the session's snapshots and its
// audit log. Missing pieces are not errors so a half-deleted session can be
// deleted again.
func (s *Store) Delete(ctx context.Context, id string) error {
	if !ValidID(id) {
		return ErrBadID
	}
	if err := s.FileStore.Delete(ctx, id); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: %w", err)
	}
	for _, rm := range []func() error{
		func() error { return os.Remove(s.metaPath(id)) },
		func() error { return os.Remove(s.AuditPath(id)) },
		func() error { return os.RemoveAll(s.SnapshotDir(id)) },
	} {
		if err := rm(); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session: %w", err)
		}
	}
	return nil
}

// Purge deletes every session whose Updated is older than olderThan and
// returns how many were removed.
func (s *Store) Purge(ctx context.Context, olderThan time.Duration) (int, error) {
	all, err := s.List(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	n := 0
	for _, m := range all {
		if !m.Updated.Before(cutoff) {
			continue
		}
		if err := s.Delete(ctx, m.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
