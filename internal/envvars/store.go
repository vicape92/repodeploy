// Package envvars owns per-project environment variables: storage outside
// Git, validation, .env parsing, change digests and Compose substitution.
// It depends only on the standard library.
package envvars

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Var is one stored variable. Build also exposes it to image builds.
type Var struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Build bool   `json:"build"`
}

// Update upserts one variable. A nil Build keeps the stored flag (false for
// a new variable).
type Update struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Build *bool  `json:"build,omitempty"`
}

const (
	// MaxVars is the most variables one project may store.
	MaxVars = 500
	// MaxValueLen is the largest accepted value, in bytes.
	MaxValueLen = 64 << 10
	maxNameLen  = 100
	fileSuffix  = ".env.json"
)

// ErrNotFound reports that a variable is not stored.
var ErrNotFound = errors.New("variable not found")

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidName reports whether name is an accepted variable name.
func ValidName(name string) bool { return len(name) <= maxNameLen && namePattern.MatchString(name) }

// IsStoreFile reports whether a file name in the store directory belongs to
// this module, so other readers of that directory can skip it.
func IsStoreFile(name string) bool { return strings.HasSuffix(name, fileSuffix) }

// Store keeps one 0600 JSON file per project in dir.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New returns a store whose files live in dir.
func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) path(projectID string) string { return filepath.Join(s.dir, projectID+fileSuffix) }

// List returns the project's variables sorted by name.
func (s *Store) List(projectID string) ([]Var, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(projectID)
}

func (s *Store) read(projectID string) ([]Var, error) {
	b, err := os.ReadFile(s.path(projectID))
	if errors.Is(err, os.ErrNotExist) {
		return []Var{}, nil
	}
	if err != nil {
		return nil, err
	}
	vars := []Var{}
	if err = json.Unmarshal(b, &vars); err != nil {
		return nil, errors.New("environment variables file is corrupt")
	}
	return vars, nil
}

// Set validates and upserts updates. Nothing is saved when any update is
// invalid.
func (s *Store) Set(projectID string, updates []Update) error {
	if len(updates) == 0 {
		return errors.New("no variables to save")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	vars, err := s.read(projectID)
	if err != nil {
		return err
	}
	index := make(map[string]int, len(vars))
	for i, v := range vars {
		index[v.Name] = i
	}
	for _, u := range updates {
		if !ValidName(u.Name) {
			return fmt.Errorf("invalid variable name %q; use letters, digits and '_', not starting with a digit (max 100)", u.Name)
		}
		if len(u.Value) > MaxValueLen || !utf8.ValidString(u.Value) {
			return fmt.Errorf("value of %s must be UTF-8 text of at most 64 KiB", u.Name)
		}
		i, ok := index[u.Name]
		if !ok {
			i = len(vars)
			index[u.Name] = i
			vars = append(vars, Var{Name: u.Name})
		}
		vars[i].Value = u.Value
		if u.Build != nil {
			vars[i].Build = *u.Build
		}
	}
	if len(vars) > MaxVars {
		return fmt.Errorf("a project can store at most %d variables", MaxVars)
	}
	return s.write(projectID, vars)
}

// Unset removes one variable.
func (s *Store) Unset(projectID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	vars, err := s.read(projectID)
	if err != nil {
		return err
	}
	for i, v := range vars {
		if v.Name == name {
			return s.write(projectID, append(vars[:i], vars[i+1:]...))
		}
	}
	return ErrNotFound
}

// DeleteProject removes every variable of a project.
func (s *Store) DeleteProject(projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(projectID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Values returns every stored value of every project, for log redaction.
func (s *Store) Values() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths, _ := filepath.Glob(filepath.Join(s.dir, "*"+fileSuffix))
	var out []string
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var vars []Var
		if json.Unmarshal(b, &vars) != nil {
			continue
		}
		for _, v := range vars {
			out = append(out, v.Value)
		}
	}
	return out
}

func (s *Store) write(projectID string, vars []Var) error {
	sort.Slice(vars, func(i, j int) bool { return vars[i].Name < vars[j].Name })
	if len(vars) == 0 {
		if err := os.Remove(s.path(projectID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(s.path(projectID), b)
}

// writePrivate replaces path atomically with a 0600 file.
func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Digest identifies a set of variables including values and build flags.
// Order does not matter, and nil and empty sets share one digest.
func Digest(vars []Var) string {
	sorted := append([]Var{}, vars...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	b, _ := json.Marshal(sorted)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
