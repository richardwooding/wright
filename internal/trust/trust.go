// Package trust remembers what the user has accepted: a project's settings
// (by hash, so a changed .wright/settings.json prompts again) and MCP
// servers (by command, arguments, URL and tool list). The file lives in the
// user's config directory, never in the project, so a repository cannot
// vouch for itself.
package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record is an accepted project.
type Record struct {
	Root         string    `json:"root"`
	SettingsHash string    `json:"settingsHash"`
	Accepted     time.Time `json:"accepted"`
}

// ServerRecord is an accepted MCP server. The hashes pin exactly what was
// shown to the user at acceptance; any change re-prompts.
type ServerRecord struct {
	Name          string    `json:"name"`
	Transport     string    `json:"transport"`
	CommandSHA256 string    `json:"commandSha256,omitempty"`
	ArgsSHA256    string    `json:"argsSha256,omitempty"`
	URL           string    `json:"url,omitempty"`
	ToolsSHA256   string    `json:"toolsSha256,omitempty"`
	Accepted      time.Time `json:"accepted"`
}

// Matches reports whether other describes the same server configuration.
func (r ServerRecord) Matches(other ServerRecord) bool {
	return r.Name == other.Name && r.Transport == other.Transport &&
		r.CommandSHA256 == other.CommandSHA256 && r.ArgsSHA256 == other.ArgsSHA256 &&
		r.URL == other.URL && r.ToolsSHA256 == other.ToolsSHA256
}

// file is the on-disk document.
type file struct {
	Version  int                     `json:"version"`
	Projects map[string]Record       `json:"projects"`
	Servers  map[string]ServerRecord `json:"servers"`
}

// Store reads and writes trust.json.
type Store struct {
	mu   sync.Mutex
	path string
}

// Open returns a store backed by path (normally Paths.TrustFile()). The
// file is created on first write.
func Open(path string) *Store { return &Store{path: path} }

// Path returns the backing file.
func (s *Store) Path() string { return s.path }

func (s *Store) load() (file, error) {
	f := file{Version: 1, Projects: map[string]Record{}, Servers: map[string]ServerRecord{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, err
	}
	if f.Projects == nil {
		f.Projects = map[string]Record{}
	}
	if f.Servers == nil {
		f.Servers = map[string]ServerRecord{}
	}
	return f, nil
}

// save writes atomically with mode 0600.
func (s *Store) save(f file) error {
	f.Version = 1
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".trust-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := writeClose(tmp, append(data, '\n')); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func writeClose(f *os.File, data []byte) error {
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Project returns the accepted record for root, if any.
func (s *Store) Project(root string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return Record{}, false
	}
	r, ok := f.Projects[root]
	return r, ok
}

// ProjectTrusted reports whether root's settings at settingsHash were accepted.
func (s *Store) ProjectTrusted(root, settingsHash string) bool {
	r, ok := s.Project(root)
	return ok && r.SettingsHash == settingsHash
}

// AcceptProject records root's settings hash as accepted.
func (s *Store) AcceptProject(root, settingsHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	f.Projects[root] = Record{Root: root, SettingsHash: settingsHash, Accepted: time.Now().UTC()}
	return s.save(f)
}

// ForgetProject removes root's record.
func (s *Store) ForgetProject(root string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	delete(f.Projects, root)
	return s.save(f)
}

// Server returns the accepted record for an MCP server name, if any.
func (s *Store) Server(name string) (ServerRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return ServerRecord{}, false
	}
	r, ok := f.Servers[name]
	return r, ok
}

// AcceptServer records an MCP server configuration as accepted.
func (s *Store) AcceptServer(r ServerRecord) error {
	if r.Name == "" {
		return errors.New("trust: server name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	r.Accepted = time.Now().UTC()
	f.Servers[r.Name] = r
	return s.save(f)
}

// ForgetServer removes a server's record.
func (s *Store) ForgetServer(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	delete(f.Servers, name)
	return s.save(f)
}

// Servers lists accepted server names, sorted.
func (s *Store) Servers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(f.Servers))
	for n := range f.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// HashFile returns the hex SHA-256 of a file's content. A missing file
// hashes as the empty string so "no settings file" is a stable state.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashStrings hashes a sequence of strings with length prefixes, so
// ("ab","c") and ("a","bc") differ.
func HashStrings(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		var n [8]byte
		l := uint64(len(p))
		for i := range 8 {
			n[i] = byte(l >> (8 * i))
		}
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
