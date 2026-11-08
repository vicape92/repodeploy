# Environment Variables and Manual Upload Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-project environment variables (stored outside Git, injected at deploy time, restored on rollback) and manual-upload projects (zip/tar.gz from the panel or `repodeploy up`) to RepoDeploy.

**Architecture:** Modular monolith. Two new stdlib-only modules own their data: `internal/envvars` (`secrets/<id>.env.json`, `.env` parsing, digest, `${VAR}` substitution) and `internal/upload` (`uploads/`, safe extraction, normalised content-addressed archives, retention, client file selection and packing). `internal/deploy` consumes them through `Store.Env` / `Store.Uploads` fields and a `SourcePreparer` interface; `internal/web` gets `env.go` and `uploads.go`; the CLI gets `env` commands and `up.go`.

**Tech Stack:** Go 1.26, stdlib (`archive/zip`, `archive/tar`, `compress/gzip`, `crypto/sha256`), existing deps only (`modernc.org/sqlite`, `go.yaml.in/yaml/v3`, `golang.org/x/crypto`). Spec: `docs/superpowers/specs/2026-11-08-env-vars-and-upload-design.md`.

## Global Constraints

- Module path `github.com/vicape92/repodeploy`; **no new dependencies** (`go.mod` unchanged).
- `internal/envvars` and `internal/upload` import only the standard library; they never import `deploy`, `web` or `scm`.
- No SQLite schema change: `PRAGMA user_version` stays `1`. Upload hashes live in the existing `jobs.sha` column; `env_digest` lives in the job snapshot JSON.
- Variable names `^[A-Za-z_][A-Za-z0-9_]*$`, at most 100 characters; values UTF-8, at most 64 KiB; at most 500 variables per project.
- Upload limits: 200 MB per archive by default (`serve --max-upload`), expanded size at most 5× that limit, at most 50 000 entries, at most 2 uploads processed concurrently.
- State files are 0600, state directories 0700. Variable values never appear in argv, job snapshots, rendered pages (except the explicit **Show** response page) or logs.
- Compose interpolation is allowed only as `${NAME}` / `${NAME:-default}` inside `services.*.environment` values and `services.*.build.args` values; inserted values have `$` doubled.
- Panel copy is English, matching the existing UI.
- Commit messages carry **no** Claude / `Co-Authored-By` attribution (user preference).
- Required checks before finishing: `go test ./...`, `go test -race ./...`, `go vet ./...`, `gofmt -l cmd internal` (empty), `go mod verify`, `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...` and the same for `arm64`.

## File Structure

| File | Responsibility |
|---|---|
| `internal/envvars/store.go` (new) | `Var`, `Update`, `Store` (List/Set/Unset/DeleteProject/Values), `ValidName`, `Digest`, `IsStoreFile` |
| `internal/envvars/dotenv.go` (new) | `ParseDotEnv` |
| `internal/envvars/compose.go` (new) | `Substitute`, `ServiceEnv`, `InterpolationHelp` |
| `internal/upload/archive.go` (new) | Safe extraction (zip/tar.gz), `Pack`, `Limits`, `Error` |
| `internal/upload/store.go` (new) | `Store` (Save/Prepare/Prune/DeleteProject/SweepTemp/TempDir), `ValidHash` |
| `internal/upload/select.go` (new) | `SelectFiles`, `.repodeployignore` rules |
| `internal/deploy/store.go` | `Project.Source/EnvDigest/Env`, `IsUpload`, `Store.Env/Uploads`, `SetSnapshot`, `userSource` SQL |
| `internal/deploy/envchanges.go` (new) | `EnvPending`, `EnqueueConfigChange`, `ErrNoActive` |
| `internal/deploy/sources.go` (new) | `SourcePreparer`, `uploadSource`, `sourceFor`, `userSource`, `pruneUploads` |
| `internal/deploy/engine.go` | env loading + digest, source selection, upload retention, log redaction |
| `internal/deploy/docker.go` | variable injection in `loadCompose` |
| `internal/deploy/validate.go` | upload-project validation |
| `internal/deploy/delete.go`, `backup.go` | module cleanup, uploads in backups |
| `internal/web/env.go` (new) | env API, panel forms, reveal page, deploy-changes |
| `internal/web/uploads.go` (new) | new-project page, upload endpoint, size helpers |
| `internal/web/server.go`, `projects_admin.go`, `templates.go` | routes, middleware, page fields, templates |
| `cmd/repodeploy/main.go` | `env`, `deploy-changes`, `up` wiring, `--max-upload`, `streamLogs`, `parseSize`, help |
| `cmd/repodeploy/up.go` (new) | `repodeploy up` |
| `README.md`, `docs/operations.md`, `docs/spec.md`, `docs/progress.md` | documentation |

---

### Task 1: `envvars` module — storage, validation, `.env` parsing, digest

**Files:**
- Create: `internal/envvars/store.go`
- Create: `internal/envvars/dotenv.go`
- Test: `internal/envvars/store_test.go`, `internal/envvars/dotenv_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Var struct { Name string; Value string; Build bool }` (JSON `name`, `value`, `build`)
  - `type Update struct { Name string; Value string; Build *bool }` (JSON `name`, `value`, `build,omitempty`)
  - `const MaxVars = 500`, `const MaxValueLen = 64 << 10`
  - `var ErrNotFound error`
  - `func ValidName(name string) bool`
  - `func IsStoreFile(name string) bool`
  - `func New(dir string) *Store`
  - `func (s *Store) List(projectID string) ([]Var, error)` — sorted by name, never nil
  - `func (s *Store) Set(projectID string, updates []Update) error` — all-or-nothing upsert
  - `func (s *Store) Unset(projectID, name string) error`
  - `func (s *Store) DeleteProject(projectID string) error`
  - `func (s *Store) Values() []string` — every value of every project
  - `func Digest(vars []Var) string`
  - `func ParseDotEnv(text string) ([]Update, error)`

- [ ] **Step 1: Write the failing store tests**

Create `internal/envvars/store_test.go`:

```go
package envvars

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestSetListUnsetAndPermissions(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	build := true
	if err := s.Set("p1", []Update{{Name: "B_VAR", Value: "two", Build: &build}, {Name: "A_VAR", Value: "one"}}); err != nil {
		t.Fatal(err)
	}
	vars, err := s.List("p1")
	if err != nil {
		t.Fatal(err)
	}
	want := []Var{{Name: "A_VAR", Value: "one"}, {Name: "B_VAR", Value: "two", Build: true}}
	if !reflect.DeepEqual(vars, want) {
		t.Fatalf("got %+v", vars)
	}
	info, err := os.Stat(filepath.Join(dir, "p1.env.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file mode: %v %v", info, err)
	}
	// A nil Build keeps the stored flag while the value is replaced.
	if err := s.Set("p1", []Update{{Name: "B_VAR", Value: "three"}}); err != nil {
		t.Fatal(err)
	}
	vars, _ = s.List("p1")
	if vars[1].Value != "three" || !vars[1].Build {
		t.Fatalf("upsert lost the build flag: %+v", vars[1])
	}
	if err := s.Unset("p1", "A_VAR"); err != nil {
		t.Fatal(err)
	}
	if err := s.Unset("p1", "A_VAR"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second unset: %v", err)
	}
	if other, _ := s.List("p2"); len(other) != 0 {
		t.Fatal("projects share variables")
	}
	if err := s.DeleteProject("p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "p1.env.json")); !os.IsNotExist(err) {
		t.Fatal("variables survived project deletion")
	}
}

func TestSetRejectsInvalidInputWithoutSaving(t *testing.T) {
	s := New(t.TempDir())
	for name, u := range map[string]Update{
		"digit first": {Name: "1ABC", Value: "x"},
		"dash":        {Name: "MY-VAR", Value: "x"},
		"empty":       {Name: "", Value: "x"},
		"long name":   {Name: strings.Repeat("A", 101), Value: "x"},
		"long value":  {Name: "BIG", Value: strings.Repeat("x", MaxValueLen+1)},
		"bad utf8":    {Name: "BIN", Value: "\xff"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := s.Set("p", []Update{{Name: "OK", Value: "kept"}, u}); err == nil {
				t.Fatal("accepted")
			}
			if vars, _ := s.List("p"); len(vars) != 0 {
				t.Fatalf("partial save: %+v", vars)
			}
		})
	}
	many := make([]Update, MaxVars+1)
	for i := range many {
		many[i] = Update{Name: fmt.Sprintf("V%d", i), Value: "x"}
	}
	if err := s.Set("p", many); err == nil {
		t.Fatal("variable limit not enforced")
	}
	if err := s.Set("p", nil); err == nil {
		t.Fatal("empty update accepted")
	}
}

func TestValuesCoverEveryProjectAndIgnoreOtherFiles(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.Set("p1", []Update{{Name: "A", Value: "alpha-secret"}})
	s.Set("p2", []Update{{Name: "B", Value: "beta-secret"}})
	os.WriteFile(filepath.Join(dir, "p1-tls"), []byte("not a variable"), 0600)
	values := s.Values()
	sort.Strings(values)
	if !reflect.DeepEqual(values, []string{"alpha-secret", "beta-secret"}) {
		t.Fatalf("values: %v", values)
	}
	if !IsStoreFile("p1.env.json") || IsStoreFile("p1-tls") {
		t.Fatal("store file detection")
	}
}

func TestDigestIgnoresOrderButNotValuesOrFlags(t *testing.T) {
	a := []Var{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}}
	b := []Var{{Name: "B", Value: "2"}, {Name: "A", Value: "1"}}
	if Digest(a) != Digest(b) {
		t.Fatal("order changes the digest")
	}
	if Digest(nil) != Digest([]Var{}) {
		t.Fatal("nil and empty sets differ")
	}
	changed := []Var{{Name: "A", Value: "1"}, {Name: "B", Value: "3"}}
	flagged := []Var{{Name: "A", Value: "1"}, {Name: "B", Value: "2", Build: true}}
	if Digest(a) == Digest(changed) || Digest(a) == Digest(flagged) {
		t.Fatal("digest misses changes")
	}
}
```

- [ ] **Step 2: Write the failing `.env` parser test**

Create `internal/envvars/dotenv_test.go`:

```go
package envvars

import (
	"reflect"
	"testing"
)

func TestParseDotEnv(t *testing.T) {
	text := "# comment\r\n\nexport API_URL=https://api.example.org\nPLAIN = value with spaces # trailing comment\nHASH=abc#def\nSINGLE='literal $HOME \\n'\nDOUBLE=\"line1\\nline2 \\\"quoted\\\"\"\nEMPTY=\n"
	got, err := ParseDotEnv(text)
	if err != nil {
		t.Fatal(err)
	}
	want := []Update{
		{Name: "API_URL", Value: "https://api.example.org"},
		{Name: "PLAIN", Value: "value with spaces"},
		{Name: "HASH", Value: "abc#def"},
		{Name: "SINGLE", Value: `literal $HOME \n`},
		{Name: "DOUBLE", Value: "line1\nline2 \"quoted\""},
		{Name: "EMPTY", Value: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	for _, bad := range []string{"NOEQUALS", "1BAD=x", `Q="open`, "S='open", `E="bad \q"`, "# only comments\n"} {
		if _, err := ParseDotEnv(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/envvars/`
Expected: FAIL — `undefined: New`, `undefined: ParseDotEnv`, etc.

- [ ] **Step 4: Implement the store**

Create `internal/envvars/store.go`:

```go
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
```

- [ ] **Step 5: Implement the parser**

Create `internal/envvars/dotenv.go`:

```go
package envvars

import (
	"errors"
	"fmt"
	"strings"
)

// ParseDotEnv reads KEY=value lines in the common .env format. Blank lines
// and # comments are skipped and an "export " prefix is allowed. Single-quoted
// values are literal; double-quoted values understand \n, \r, \t, \", \\ and
// \$. Unquoted values end at " #". Build flags are left unset.
func ParseDotEnv(text string) ([]Update, error) {
	var out []Update
	for n, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !ValidName(name) {
			return nil, fmt.Errorf("line %d: expected NAME=value with a valid name", n+1)
		}
		value = strings.TrimSpace(value)
		switch {
		case strings.HasPrefix(value, `"`):
			if len(value) < 2 || !strings.HasSuffix(value, `"`) {
				return nil, fmt.Errorf("line %d: unterminated double quote", n+1)
			}
			unquoted, err := unescape(value[1 : len(value)-1])
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", n+1, err)
			}
			value = unquoted
		case strings.HasPrefix(value, "'"):
			if len(value) < 2 || !strings.HasSuffix(value, "'") {
				return nil, fmt.Errorf("line %d: unterminated single quote", n+1)
			}
			value = value[1 : len(value)-1]
		default:
			if i := strings.Index(value, " #"); i >= 0 {
				value = strings.TrimSpace(value[:i])
			}
		}
		out = append(out, Update{Name: name, Value: value})
	}
	if len(out) == 0 {
		return nil, errors.New("no variables found")
	}
	return out, nil
}

func unescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i == len(s) {
			return "", errors.New("trailing backslash")
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '"', '\\', '$':
			b.WriteByte(s[i])
		default:
			return "", fmt.Errorf("unsupported escape \\%c", s[i])
		}
	}
	return b.String(), nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/envvars/ -v`
Expected: PASS for all four store tests and `TestParseDotEnv`.

- [ ] **Step 7: Commit**

```bash
git add internal/envvars
git commit -m "Add envvars module: per-project variable storage, .env parsing and digests"
```

---

### Task 2: `envvars` Compose substitution and Dockerfile injection

**Files:**
- Create: `internal/envvars/compose.go`
- Test: `internal/envvars/compose_test.go`

**Interfaces:**
- Consumes: `Var`, `ValidName` (Task 1).
- Produces:
  - `const InterpolationHelp string`
  - `func Substitute(cfg map[string]any, vars []Var) error` — mutates `cfg` in place
  - `func ServiceEnv(vars []Var) (env, args map[string]any)`

- [ ] **Step 1: Write the failing tests**

Create `internal/envvars/compose_test.go`:

```go
package envvars

import (
	"strings"
	"testing"
)

func TestSubstituteExpandsOnlyEnvironmentAndBuildArgs(t *testing.T) {
	cfg := map[string]any{"services": map[string]any{
		"web": map[string]any{
			"image":       "${IMAGE}",
			"environment": map[string]any{"DB": "${DATABASE_URL}", "PORT": "${PORT:-3000}", "LITERAL": "cost $$5", "NUM": 3},
			"build":       map[string]any{"context": ".", "args": map[string]any{"PUBLIC": "${PUBLIC_URL}", "MODE": "${MODE:-prod}"}},
		},
		"worker": map[string]any{"environment": []any{"DB=${DATABASE_URL}", "EMPTY=${BLANK:-fallback}"}},
	}}
	vars := []Var{{Name: "DATABASE_URL", Value: "postgres://u:pa$$@db/app"}, {Name: "PUBLIC_URL", Value: "https://x", Build: true}, {Name: "BLANK", Value: ""}}
	if err := Substitute(cfg, vars); err != nil {
		t.Fatal(err)
	}
	services := cfg["services"].(map[string]any)
	web := services["web"].(map[string]any)
	env := web["environment"].(map[string]any)
	if env["DB"] != "postgres://u:pa$$$$@db/app" || env["PORT"] != "3000" || env["LITERAL"] != "cost $$5" || env["NUM"] != 3 {
		t.Fatalf("environment: %#v", env)
	}
	args := web["build"].(map[string]any)["args"].(map[string]any)
	if args["PUBLIC"] != "https://x" || args["MODE"] != "prod" {
		t.Fatalf("args: %#v", args)
	}
	if web["image"] != "${IMAGE}" {
		t.Fatal("image was expanded")
	}
	worker := services["worker"].(map[string]any)["environment"].([]any)
	if worker[0] != "DB=postgres://u:pa$$$$@db/app" || worker[1] != "EMPTY=fallback" {
		t.Fatalf("list environment: %#v", worker)
	}
}

func TestSubstituteReportsMissingNonBuildAndUnsupported(t *testing.T) {
	compose := func(env, args map[string]any) map[string]any {
		service := map[string]any{"environment": env}
		if args != nil {
			service["build"] = map[string]any{"args": args}
		}
		return map[string]any{"services": map[string]any{"app": service}}
	}
	err := Substitute(compose(map[string]any{"A": "${ZETA}", "B": "${ALPHA}"}, nil), nil)
	if err == nil || !strings.Contains(err.Error(), "undefined environment variables: ALPHA, ZETA") {
		t.Fatalf("missing: %v", err)
	}
	err = Substitute(compose(map[string]any{}, map[string]any{"S": "${SECRET}"}), []Var{{Name: "SECRET", Value: "x"}})
	if err == nil || !strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "image history") {
		t.Fatalf("non-build arg: %v", err)
	}
	for _, bad := range []string{"$NAME", "${NAME?required}", "${NAME-x}", "${NAME:-$OTHER}", "${unterminated", "${}"} {
		if err := Substitute(compose(map[string]any{"X": bad}, nil), nil); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestServiceEnvEscapesAndLimitsBuildArgs(t *testing.T) {
	env, args := ServiceEnv([]Var{{Name: "RUNTIME", Value: "a$b"}, {Name: "PUBLIC", Value: "p", Build: true}})
	if env["RUNTIME"] != "a$$b" || env["PUBLIC"] != "p" || len(args) != 1 || args["PUBLIC"] != "p" {
		t.Fatalf("env=%v args=%v", env, args)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/envvars/ -run 'Substitute|ServiceEnv'`
Expected: FAIL — `undefined: Substitute`, `undefined: ServiceEnv`.

- [ ] **Step 3: Implement**

Create `internal/envvars/compose.go`:

```go
package envvars

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// InterpolationHelp names the only accepted Compose interpolation.
const InterpolationHelp = "Compose interpolation is only supported as ${NAME} or ${NAME:-default} inside environment values and build args"

// Substitute expands variables in a decoded Compose file in place. Only the
// values of services.*.environment and services.*.build.args are expanded;
// every other "$" is left for the caller's validation to reject, so variables
// cannot change images, ports, volumes or privileges. Inserted values have
// "$" doubled so Compose keeps them literal.
func Substitute(cfg map[string]any, vars []Var) error {
	byName := make(map[string]Var, len(vars))
	for _, v := range vars {
		byName[v.Name] = v
	}
	r := resolver{vars: byName, missing: map[string]bool{}, notBuild: map[string]bool{}}
	services, _ := cfg["services"].(map[string]any)
	for _, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch env := service["environment"].(type) {
		case map[string]any:
			for key, value := range env {
				if s, ok := value.(string); ok {
					out, err := r.expand(s, false)
					if err != nil {
						return err
					}
					env[key] = out
				}
			}
		case []any:
			for i, entry := range env {
				s, ok := entry.(string)
				if !ok {
					continue
				}
				key, value, found := strings.Cut(s, "=")
				if !found {
					continue
				}
				out, err := r.expand(value, false)
				if err != nil {
					return err
				}
				env[i] = key + "=" + out
			}
		}
		if build, ok := service["build"].(map[string]any); ok {
			if args, ok := build["args"].(map[string]any); ok {
				for key, value := range args {
					if s, ok := value.(string); ok {
						out, err := r.expand(s, true)
						if err != nil {
							return err
						}
						args[key] = out
					}
				}
			}
		}
	}
	if len(r.notBuild) > 0 {
		return fmt.Errorf("build args use %s, which are not available at build time; build arguments are stored in the image history, so enable the build-time flag for them on the project page or with repodeploy env set --build", sortedNames(r.notBuild))
	}
	if len(r.missing) > 0 {
		return fmt.Errorf("undefined environment variables: %s; define them on the project page or with repodeploy env set", sortedNames(r.missing))
	}
	return nil
}

// ServiceEnv returns the environment and build args for Dockerfile mode:
// every variable at runtime, build-flagged ones also as build args.
func ServiceEnv(vars []Var) (env, args map[string]any) {
	env, args = map[string]any{}, map[string]any{}
	for _, v := range vars {
		env[v.Name] = escape(v.Value)
		if v.Build {
			args[v.Name] = escape(v.Value)
		}
	}
	return env, args
}

type resolver struct {
	vars              map[string]Var
	missing, notBuild map[string]bool
}

func (r resolver) expand(s string, build bool) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			out.WriteByte(s[i])
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "$$") {
			out.WriteString("$$")
			i += 2
			continue
		}
		end := strings.IndexByte(s[i:], '}')
		if !strings.HasPrefix(s[i:], "${") || end < 0 {
			return "", errors.New(InterpolationHelp)
		}
		expr := s[i+2 : i+end]
		name, def, hasDefault := strings.Cut(expr, ":-")
		if !ValidName(name) || strings.ContainsAny(def, "${") {
			return "", fmt.Errorf("unsupported interpolation ${%s}; %s", expr, InterpolationHelp)
		}
		v, ok := r.vars[name]
		switch {
		case ok && v.Value == "" && hasDefault:
			out.WriteString(def)
		case ok && build && !v.Build:
			r.notBuild[name] = true
		case ok:
			out.WriteString(escape(v.Value))
		case hasDefault:
			out.WriteString(def)
		default:
			r.missing[name] = true
		}
		i += end + 1
	}
	return out.String(), nil
}

func escape(s string) string { return strings.ReplaceAll(s, "$", "$$") }

func sortedNames(set map[string]bool) string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/envvars/ -v`
Expected: PASS (all envvars tests).

- [ ] **Step 5: Commit**

```bash
git add internal/envvars/compose.go internal/envvars/compose_test.go
git commit -m "Add restricted Compose substitution and Dockerfile-mode variables to envvars"
```

---

### Task 3: Deploy engine uses variables — injection, digest, pending changes, redaction, deletion

**Files:**
- Modify: `internal/deploy/store.go` (Project fields, Store field, OpenStore, SaveProject, new `SetSnapshot`)
- Create: `internal/deploy/envchanges.go`
- Modify: `internal/deploy/engine.go` (rollback snapshot, env loading, LogWriter)
- Modify: `internal/deploy/docker.go` (`loadCompose`)
- Modify: `internal/deploy/delete.go`
- Test: `internal/deploy/envchanges_test.go`, modify `internal/deploy/engine_test.go` (testRuntime), `internal/deploy/docker_test.go`

**Interfaces:**
- Consumes: `envvars.Store`, `envvars.Var`, `envvars.Update`, `envvars.Digest`, `envvars.Substitute`, `envvars.ServiceEnv`, `envvars.InterpolationHelp`, `envvars.IsStoreFile`, `envvars.New` (Tasks 1–2).
- Produces:
  - `Project.EnvDigest string` (JSON `env_digest,omitempty`; only meaningful in job snapshots)
  - `Project.Env []envvars.Var` (JSON `-`; carries variables into one build)
  - `Store.Env *envvars.Store`
  - `func (s *Store) SetSnapshot(id string, p Project) error`
  - `var ErrNoActive error` — message `nothing is deployed yet; deploy the project first`
  - `func (s *Store) EnvPending(p Project) (bool, error)`
  - `func (s *Store) EnqueueConfigChange(projectID string) (Job, error)`

- [ ] **Step 1: Let the test runtime record the variables it receives**

In `internal/deploy/engine_test.go`, add the import `"github.com/vicape92/repodeploy/internal/envvars"` and change `testRuntime`:

```go
type testRuntime struct {
	active       string
	failBuild    bool
	failStart    bool
	failHealth   bool
	failRecovery bool
	beforeBuild  func()
	env          []envvars.Var
}
```

and the first lines of its `Build` method:

```go
func (r *testRuntime) Build(ctx context.Context, p Project, checkout, dir string, w io.Writer) (string, error) {
	r.env = p.Env
	if r.beforeBuild != nil {
		r.beforeBuild()
	}
```

- [ ] **Step 2: Write the failing engine tests**

Create `internal/deploy/envchanges_test.go`:

```go
package deploy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/envvars"
)

func stepOnce(t *testing.T, e *Engine, j Job) Job {
	t.Helper()
	did, err := e.Step(context.Background())
	if err != nil || !did {
		t.Fatalf("step=%v err=%v", did, err)
	}
	j, err = e.Store.Job(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestEngineBuildsWithVariablesAndRecordsTheirDigest(t *testing.T) {
	e, r, p, _ := engineFixture(t)
	if err := e.Store.Env.Set(p.ID, []envvars.Update{{Name: "API_KEY", Value: "very-secret-value"}}); err != nil {
		t.Fatal(err)
	}
	j := runDeployment(t, e, p)
	vars, _ := e.Store.Env.List(p.ID)
	if j.Phase != "succeeded" || !reflect.DeepEqual(r.env, vars) || j.Project.EnvDigest != envvars.Digest(vars) {
		t.Fatalf("job=%+v runtime env=%+v", j, r.env)
	}
	var snapshot string
	if err := e.Store.db.QueryRow(`SELECT snapshot FROM jobs WHERE id=?`, j.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot, "very-secret-value") {
		t.Fatal("variable value persisted in the job snapshot")
	}
}

func TestConfigChangeRedeploysActiveCommitAndRollbackRestoresDigest(t *testing.T) {
	e, r, p, repo := engineFixture(t)
	if _, err := e.Store.EnqueueConfigChange(p.ID); !errors.Is(err, ErrNoActive) {
		t.Fatalf("config change without release: %v", err)
	}
	first := runDeployment(t, e, p)
	current, _ := e.Store.Project(p.ID)
	if pending, err := e.Store.EnvPending(current); err != nil || pending {
		t.Fatalf("pending without changes: %v %v", pending, err)
	}
	e.Store.Env.Set(p.ID, []envvars.Update{{Name: "MODE", Value: "blue"}})
	if pending, _ := e.Store.EnvPending(current); !pending {
		t.Fatal("variable change not detected")
	}
	secondCommit(t, repo) // the branch moves; deploy changes must keep the active commit
	j, err := e.Store.EnqueueConfigChange(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	j = stepOnce(t, e, j)
	if j.Phase != "succeeded" || j.Commit != first.Commit || r.active != "first" || len(r.env) != 1 {
		t.Fatalf("config change: %+v active=%s env=%v", j, r.active, r.env)
	}
	current, _ = e.Store.Project(p.ID)
	if pending, _ := e.Store.EnvPending(current); pending {
		t.Fatal("still pending after deploying changes")
	}
	rollback, err := e.Store.Enqueue(p.ID, "manual", "", "", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	rollback = stepOnce(t, e, rollback)
	current, _ = e.Store.Project(p.ID)
	if rollback.Phase != "succeeded" || rollback.Project.EnvDigest != envvars.Digest(nil) {
		t.Fatalf("rollback: %+v", rollback)
	}
	if pending, _ := e.Store.EnvPending(current); !pending {
		t.Fatal("restoring the old release should restore its variables")
	}
}

func TestLogWriterRedactsEnvironmentValues(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	s.Env.Set(p.ID, []envvars.Update{{Name: "TOKEN", Value: "tok-123456"}})
	j, err := s.Enqueue(p.ID, "manual", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	w := s.LogWriter(j.ID)
	fmt.Fprintln(w, "using tok-123456 now")
	w.Close()
	logs, _ := s.Logs(j.ID)
	if strings.Contains(logs, "tok-123456") || !strings.Contains(logs, "[REDACTED]") {
		t.Fatalf("value not redacted: %q", logs)
	}
}

func TestDeleteProjectRemovesOnlyItsVariables(t *testing.T) {
	e, _, p, _ := engineFixture(t)
	e.Runtime = &purgeRuntime{testRuntime: &testRuntime{}}
	keep := testProject(t, e.Store)
	e.Store.Env.Set(p.ID, []envvars.Update{{Name: "A", Value: "1"}})
	e.Store.Env.Set(keep.ID, []envvars.Update{{Name: "B", Value: "2"}})
	if err := e.DeleteProject(context.Background(), p.ID, false); err != nil {
		t.Fatal(err)
	}
	if vars, _ := e.Store.Env.List(p.ID); len(vars) != 0 {
		t.Fatal("deleted project's variables remain")
	}
	if vars, _ := e.Store.Env.List(keep.ID); len(vars) != 1 {
		t.Fatal("another project's variables were deleted")
	}
}
```

Append to `internal/deploy/docker_test.go` (add the import `"github.com/vicape92/repodeploy/internal/envvars"`):

```go
func TestComposeSubstitutesProjectVariables(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  app:\n    image: alpine\n    environment:\n      DB: ${DATABASE_URL}\n    healthcheck:\n      test: ['CMD', 'true']\n"), 0600)
	p := Project{ID: strings.Repeat("a", 32), ComposeFile: "compose.yaml", HealthService: "app", Env: []envvars.Var{{Name: "DATABASE_URL", Value: "pg://x$y"}}}
	cfg, err := loadCompose(p, dir, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := cfg["services"].(map[string]any)["app"].(map[string]any)["environment"].(map[string]any)
	if env["DB"] != "pg://x$$y" {
		t.Fatalf("environment: %v", env)
	}
	p.Env = nil
	if _, err := loadCompose(p, dir, t.TempDir(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("missing variable accepted: %v", err)
	}
}

func TestDockerfileModeInjectsVariables(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch"), 0600)
	p := Project{ID: strings.Repeat("a", 32), Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health", Env: []envvars.Var{{Name: "RUNTIME", Value: "r"}, {Name: "PUBLIC", Value: "p", Build: true}}}
	cfg, err := loadCompose(p, dir, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := cfg["services"].(map[string]any)["app"].(map[string]any)
	env := app["environment"].(map[string]any)
	args := app["build"].(map[string]any)["args"].(map[string]any)
	if env["RUNTIME"] != "r" || env["PUBLIC"] != "p" || len(args) != 1 || args["PUBLIC"] != "p" {
		t.Fatalf("env=%v args=%v", env, args)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/deploy/ -run 'Variables|ConfigChange|Redacts|OnlyItsVariables|Substitutes|InjectsVariables'`
Expected: FAIL to compile — `p.Env undefined`, `e.Store.Env undefined`, `undefined: ErrNoActive`.

- [ ] **Step 4: Extend `Project` and `Store`**

In `internal/deploy/store.go`, add the import `"github.com/vicape92/repodeploy/internal/envvars"`. Add to `Project` after `PollError`:

```go
	// EnvDigest records, in job snapshots only, the variables a release was
	// built with. Project settings never store it.
	EnvDigest string `json:"env_digest,omitempty"`
	// Env carries the stored variables into the runtime for one build. It is
	// never persisted.
	Env []envvars.Var `json:"-"`
```

Change `Store`:

```go
type Store struct {
	Root    string
	Sources *scm.Manager
	Env     *envvars.Store
	db      *sql.DB
	logMu   sync.Mutex
}
```

In `OpenStore` replace

```go
	s := &Store{Root: root, db: db, Sources: scm.New(root)}
```

with

```go
	s := &Store{Root: root, db: db, Sources: scm.New(root), Env: envvars.New(filepath.Join(root, "secrets"))}
```

At the top of `SaveProject` add:

```go
	p.EnvDigest, p.Env = "", nil
```

After `SetArtifact` add:

```go
// SetSnapshot replaces the project settings recorded for a job.
func (s *Store) SetSnapshot(id string, p Project) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE jobs SET snapshot=? WHERE id=?`, string(b), id)
	return err
}
```

- [ ] **Step 5: Add pending detection and config-change enqueueing**

Create `internal/deploy/envchanges.go`:

```go
package deploy

import (
	"errors"

	"github.com/vicape92/repodeploy/internal/envvars"
)

// ErrNoActive reports that a project has no release to redeploy.
var ErrNoActive = errors.New("nothing is deployed yet; deploy the project first")

// EnvPending reports whether the stored variables differ from those the
// active release was built with. Without an active release nothing is
// pending: the first deployment uses the stored values.
func (s *Store) EnvPending(p Project) (bool, error) {
	if p.Active == "" {
		return false, nil
	}
	active, err := s.Job(p.Active)
	if err != nil {
		return false, err
	}
	vars, err := s.Env.List(p.ID)
	if err != nil {
		return false, err
	}
	built := active.Project.EnvDigest
	if built == "" {
		built = envvars.Digest(nil)
	}
	return envvars.Digest(vars) != built, nil
}

// EnqueueConfigChange queues a manual deployment pinned to the active
// release's commit or upload, so it rebuilds with the current variables.
func (s *Store) EnqueueConfigChange(projectID string) (Job, error) {
	p, err := s.Project(projectID)
	if err != nil {
		return Job{}, err
	}
	if p.Active == "" {
		return Job{}, ErrNoActive
	}
	active, err := s.Job(p.Active)
	if err != nil {
		return Job{}, err
	}
	return s.Enqueue(projectID, "manual", "", active.Commit, "")
}
```

- [ ] **Step 6: Load variables in the engine and redact them in logs**

In `internal/deploy/engine.go`, add the import `"github.com/vicape92/repodeploy/internal/envvars"`.

Replace in `execute` (rollback branch):

```go
		j.Commit = target.Commit
		j.Project = target.Project
		snapshot, err := marshal(j.Project)
		if err != nil {
			return err
		}
		if _, err = e.Store.db.Exec(`UPDATE jobs SET snapshot=? WHERE id=?`, snapshot, j.ID); err != nil {
			return err
		}
```

with

```go
		j.Commit = target.Commit
		j.Project = target.Project
		if err = e.Store.SetSnapshot(j.ID, j.Project); err != nil {
			return err
		}
```

Replace

```go
		fmt.Fprintln(log, "Building; the active release is unchanged")
		j.Manifest, err = e.Runtime.Build(ctx, j.Project, checkout, dir, log)
```

with

```go
		vars, err := e.Store.Env.List(j.ProjectID)
		if err != nil {
			return err
		}
		j.Project.Env, j.Project.EnvDigest = vars, envvars.Digest(vars)
		if err = e.Store.SetSnapshot(j.ID, j.Project); err != nil {
			return err
		}
		fmt.Fprintln(log, "Building; the active release is unchanged")
		j.Manifest, err = e.Runtime.Build(ctx, j.Project, checkout, dir, log)
```

In `LogWriter`, change the loop condition

```go
		if !entry.IsDir() {
```

to

```go
		if !entry.IsDir() && !envvars.IsStoreFile(entry.Name()) {
```

and insert, just before `sort.Slice(masks, ...)`:

```go
	for _, value := range s.Env.Values() {
		if value = strings.TrimSpace(value); len(value) >= minWholeMask {
			masks = append(masks, value)
		}
	}
```

- [ ] **Step 7: Inject variables in `loadCompose`**

In `internal/deploy/docker.go`, add the import `"github.com/vicape92/repodeploy/internal/envvars"`. Replace

```go
		cfg = map[string]any{"services": map[string]any{"app": map[string]any{"build": map[string]any{"context": source, "dockerfile": dockerfile}, "ports": []any{publishedPort(p)}, "restart": "unless-stopped"}}}
```

with

```go
		build := map[string]any{"context": source, "dockerfile": dockerfile}
		app := map[string]any{"build": build, "ports": []any{publishedPort(p)}, "restart": "unless-stopped"}
		env, args := envvars.ServiceEnv(p.Env)
		if len(env) > 0 {
			app["environment"] = env
		}
		if len(args) > 0 {
			build["args"] = args
		}
		cfg = map[string]any{"services": map[string]any{"app": app}}
```

Replace

```go
		if !noInterpolation(cfg) {
			return nil, errors.New("Compose interpolation is unsupported; use explicit values or managed secrets")
		}
```

with

```go
		if err = envvars.Substitute(cfg, p.Env); err != nil {
			return nil, err
		}
		if !noInterpolation(cfg) {
			return nil, errors.New(envvars.InterpolationHelp)
		}
```

- [ ] **Step 8: Delete a project's variables with the project**

In `internal/deploy/delete.go`, insert just before `return e.Store.deleteProjectRows(id)`:

```go
	if err = e.Store.Env.DeleteProject(p.ID); err != nil {
		return err
	}
```

- [ ] **Step 9: Run the deploy tests**

Run: `go test ./internal/deploy/`
Expected: PASS, including the six new tests and the existing `TestComposeRejectsUnsafeOrUnsupportedFeaturesBeforeExecution` (its `${PRIVATE_IMAGE}` case is still rejected).

- [ ] **Step 10: Commit**

```bash
git add internal/deploy
git commit -m "Inject project variables at build time, record their digest and support deploy changes"
```

---

### Task 4: Web — variables API, panel section, reveal page, deploy changes

**Files:**
- Create: `internal/web/env.go`
- Modify: `internal/web/server.go` (routes, `page` fields)
- Modify: `internal/web/projects_admin.go` (`projectView`)
- Modify: `internal/web/templates.go`
- Test: `internal/web/env_test.go`

**Interfaces:**
- Consumes: `Store.Env`, `Store.EnvPending`, `Store.EnqueueConfigChange`, `deploy.ErrNoActive` (Task 3); `envvars.Update`, `envvars.ParseDotEnv`, `envvars.ErrNotFound` (Task 1).
- Produces:
  - Routes: `GET/PUT /api/projects/{id}/env`, `DELETE /api/projects/{id}/env/{name}`, `POST /api/projects/{id}/deploy-changes`, `POST /projects/{id}/env`, `POST /projects/{id}/env/import`, `POST /projects/{id}/env/{name}/reveal`, `POST /projects/{id}/env/{name}/delete`, `POST /projects/{id}/deploy-changes`.
  - `GET /api/projects/{id}/env` response: `{"variables":[{"name":"…","build":false,"value":"…"}],"pending":false}` (`value` only with `?reveal=1`).
  - `PUT /api/projects/{id}/env` body: JSON list of `envvars.Update`; response `{"saved": N}`.
  - `page` fields `EnvRows []envRow`, `EnvPending bool`, `EnvError string`, `EnvName string`.

- [ ] **Step 1: Write the failing tests**

Create `internal/web/env_test.go`:

```go
package web

import (
	"encoding/json"
	"io"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/envvars"
)

func readBody(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	defer body.Close()
	b, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func activate(t *testing.T, server *Server, projectID, sha string) deploy.Job {
	t.Helper()
	j, err := server.Store.Enqueue(projectID, "manual", "", sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Store.Finish(j.ID, "succeeded", "", ""); err != nil {
		t.Fatal(err)
	}
	if err = server.Store.SetActive(projectID, j.ID); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestEnvAPIStoresValuesAndRevealsOnlyOnRequest(t *testing.T) {
	server, h, token := webFixture(t)
	p := savedProject(t, server)
	base := h.URL + "/api/projects/" + p.ID + "/env"
	resp := request(t, "PUT", base, token, []map[string]any{{"name": "DATABASE_URL", "value": "postgres://secret-pass@db"}, {"name": "PUBLIC_URL", "value": "https://x", "build": true}})
	if body := readBody(t, resp.Body); resp.StatusCode != 200 {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	resp = request(t, "GET", base, token, nil)
	body := readBody(t, resp.Body)
	if strings.Contains(body, "secret-pass") || !strings.Contains(body, `"name":"PUBLIC_URL","build":true`) || !strings.Contains(body, `"pending":false`) {
		t.Fatalf("list: %s", body)
	}
	resp = request(t, "GET", base+"?reveal=1", token, nil)
	if body = readBody(t, resp.Body); !strings.Contains(body, "postgres://secret-pass@db") {
		t.Fatalf("reveal: %s", body)
	}
	resp = request(t, "PUT", base, token, []map[string]any{{"name": "1BAD", "value": "x"}})
	if readBody(t, resp.Body); resp.StatusCode != 400 {
		t.Fatalf("invalid name: %d", resp.StatusCode)
	}
	resp = request(t, "DELETE", base+"/PUBLIC_URL", token, nil)
	if readBody(t, resp.Body); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp = request(t, "DELETE", base+"/PUBLIC_URL", token, nil)
	if readBody(t, resp.Body); resp.StatusCode != 404 {
		t.Fatalf("second delete: %d", resp.StatusCode)
	}
	resp = request(t, "GET", base, "", nil)
	if readBody(t, resp.Body); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
	if vars, _ := server.Store.Env.List(p.ID); len(vars) != 1 {
		t.Fatalf("stored: %+v", vars)
	}
}

func TestDeployChangesNeedsActiveReleaseAndPinsItsCommit(t *testing.T) {
	server, h, token := webFixture(t)
	p := savedProject(t, server)
	resp := request(t, "POST", h.URL+"/api/projects/"+p.ID+"/deploy-changes", token, nil)
	if body := readBody(t, resp.Body); resp.StatusCode != 409 || !strings.Contains(body, "nothing is deployed yet") {
		t.Fatalf("without release: %d %s", resp.StatusCode, body)
	}
	sha := strings.Repeat("a", 40)
	activate(t, server, p.ID, sha)
	resp = request(t, "POST", h.URL+"/api/projects/"+p.ID+"/deploy-changes", token, nil)
	var job deploy.Job
	json.NewDecoder(resp.Body).Decode(&job)
	resp.Body.Close()
	if resp.StatusCode != 202 || job.Commit != sha || job.Source != "manual" {
		t.Fatalf("deploy changes: %d %+v", resp.StatusCode, job)
	}
}

func TestEnvCanBeManagedFromTheProjectPage(t *testing.T) {
	server, h, _ := webFixture(t)
	p := savedProject(t, server)
	client, csrf := loggedBrowser(t, h.URL)
	project := h.URL + "/projects/" + p.ID
	resp := postForm(t, client, project+"/env", url.Values{"_csrf": {csrf}, "name": {"API_KEY"}, "value": {"hunter2-long-value"}, "build": {"on"}})
	if readBody(t, resp.Body); resp.StatusCode != 303 {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	resp = postForm(t, client, project+"/env/import", url.Values{"_csrf": {csrf}, "dotenv": {"export A=1\r\nB=\"two\"\r\n"}})
	if readBody(t, resp.Body); resp.StatusCode != 303 {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	vars, _ := server.Store.Env.List(p.ID)
	want := []envvars.Var{{Name: "A", Value: "1"}, {Name: "API_KEY", Value: "hunter2-long-value", Build: true}, {Name: "B", Value: "two"}}
	if !reflect.DeepEqual(vars, want) {
		t.Fatalf("stored: %+v", vars)
	}
	resp, _ = client.Get(project)
	page := readBody(t, resp.Body)
	if !strings.Contains(page, "<code>API_KEY</code>") || !strings.Contains(page, "Build and runtime") || strings.Contains(page, "hunter2") || strings.Contains(page, "Undeployed changes") {
		t.Fatal("variable list missing, leaking values or showing a banner without a release")
	}
	resp = postForm(t, client, project+"/env/API_KEY/reveal", url.Values{"_csrf": {csrf}})
	if body := readBody(t, resp.Body); resp.StatusCode != 200 || !strings.Contains(body, "hunter2-long-value") {
		t.Fatalf("reveal: %d", resp.StatusCode)
	}
	resp = postForm(t, client, project+"/env/API_KEY/reveal", url.Values{})
	if readBody(t, resp.Body); resp.StatusCode != 403 {
		t.Fatalf("reveal without CSRF: %d", resp.StatusCode)
	}
	resp = postForm(t, client, project+"/env/import", url.Values{"_csrf": {csrf}, "dotenv": {"NOEQUALS"}})
	if body := readBody(t, resp.Body); resp.StatusCode != 400 || !strings.Contains(body, "line 1") {
		t.Fatalf("bad import: %d", resp.StatusCode)
	}
	resp = postForm(t, client, project+"/env/A/delete", url.Values{"_csrf": {csrf}})
	if readBody(t, resp.Body); resp.StatusCode != 303 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	activate(t, server, p.ID, strings.Repeat("b", 40))
	resp, _ = client.Get(project)
	if page = readBody(t, resp.Body); !strings.Contains(page, "Undeployed changes") || !strings.Contains(page, `action="/projects/`+p.ID+`/deploy-changes"`) {
		t.Fatal("pending banner missing")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/web/ -run 'Env|DeployChanges'`
Expected: FAIL — routes return 404/405 (handlers do not exist).

- [ ] **Step 3: Implement the handlers**

Create `internal/web/env.go`:

```go
package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/envvars"
)

// envRow is what the panel lists for a variable: never its value.
type envRow struct {
	Name  string
	Build bool
}

type envItem struct {
	Name  string  `json:"name"`
	Build bool    `json:"build"`
	Value *string `json:"value,omitempty"`
}

type envResponse struct {
	Variables []envItem `json:"variables"`
	Pending   bool      `json:"pending"`
}

func (s *Server) envList(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vars, err := s.Store.Env.List(p.ID)
	if err != nil {
		http.Error(w, "cannot read variables", 500)
		return
	}
	pending, err := s.Store.EnvPending(p)
	if err != nil {
		http.Error(w, "cannot compare variables with the active release", 500)
		return
	}
	reveal := r.URL.Query().Get("reveal") == "1"
	out := envResponse{Variables: []envItem{}, Pending: pending}
	for _, v := range vars {
		item := envItem{Name: v.Name, Build: v.Build}
		if reveal {
			value := v.Value
			item.Value = &value
		}
		out.Variables = append(out.Variables, item)
	}
	jsonResponse(w, 200, out)
}

func (s *Server) envSet(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var updates []envvars.Update
	if err = decode(r, &updates); err != nil {
		http.Error(w, "send a JSON list of {name, value, build}", 400)
		return
	}
	if err = s.Store.Env.Set(p.ID, updates); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	jsonResponse(w, 200, map[string]int{"saved": len(updates)})
}

func (s *Server) envDelete(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.Store.Env.Unset(p.ID, r.PathValue("name"))
	if api(r) {
		switch {
		case errors.Is(err, envvars.ErrNotFound):
			http.Error(w, err.Error(), 404)
		case err != nil:
			http.Error(w, "cannot delete variable", 500)
		default:
			jsonResponse(w, 200, map[string]bool{"deleted": true})
		}
		return
	}
	if err != nil {
		s.renderEnvError(w, r, p, err.Error())
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID+"#env", 303)
}

// envSaveForm adds or replaces one variable from the project page.
func (s *Server) envSaveForm(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	build := r.FormValue("build") == "on"
	value := strings.ReplaceAll(r.FormValue("value"), "\r\n", "\n")
	if err = s.Store.Env.Set(p.ID, []envvars.Update{{Name: strings.TrimSpace(r.FormValue("name")), Value: value, Build: &build}}); err != nil {
		s.renderEnvError(w, r, p, err.Error())
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID+"#env", 303)
}

// envImportForm stores every variable of pasted .env contents.
func (s *Server) envImportForm(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	updates, err := envvars.ParseDotEnv(r.FormValue("dotenv"))
	if err == nil {
		err = s.Store.Env.Set(p.ID, updates)
	}
	if err != nil {
		s.renderEnvError(w, r, p, err.Error())
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID+"#env", 303)
}

// envReveal shows one value on its own response page.
func (s *Server) envReveal(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vars, err := s.Store.Env.List(p.ID)
	if err != nil {
		http.Error(w, "cannot read variables", 500)
		return
	}
	for _, v := range vars {
		if v.Name == r.PathValue("name") {
			s.render(w, r, page{View: "env-value", Project: p, EnvName: v.Name, Secret: v.Value})
			return
		}
	}
	http.NotFound(w, r)
}

// deployChanges redeploys the active release with the current variables.
func (s *Server) deployChanges(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.Project(r.PathValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	j, err := s.Store.EnqueueConfigChange(r.PathValue("id"))
	respondJob(w, r, j, err)
}

func (s *Server) renderEnvError(w http.ResponseWriter, r *http.Request, p deploy.Project, message string) {
	view, err := s.projectView(p)
	if err != nil {
		http.Error(w, message, 400)
		return
	}
	view.EnvError = message
	s.renderStatus(w, r, 400, view)
}
```

- [ ] **Step 4: Register routes and page fields**

In `internal/web/server.go`, add after `mux.HandleFunc("DELETE /api/projects/{id}/secrets/{name}", s.deleteSecret)`:

```go
	mux.HandleFunc("GET /api/projects/{id}/env", s.envList)
	mux.HandleFunc("PUT /api/projects/{id}/env", s.envSet)
	mux.HandleFunc("DELETE /api/projects/{id}/env/{name}", s.envDelete)
	mux.HandleFunc("POST /api/projects/{id}/deploy-changes", s.deployChanges)
```

and after `mux.HandleFunc("POST /projects/{id}/secrets/{name}/delete", s.deleteSecret)`:

```go
	mux.HandleFunc("POST /projects/{id}/env", s.envSaveForm)
	mux.HandleFunc("POST /projects/{id}/env/import", s.envImportForm)
	mux.HandleFunc("POST /projects/{id}/env/{name}/reveal", s.envReveal)
	mux.HandleFunc("POST /projects/{id}/env/{name}/delete", s.envDelete)
	mux.HandleFunc("POST /projects/{id}/deploy-changes", s.deployChanges)
```

Add to `type page struct` after `Refresh bool`:

```go
	EnvRows    []envRow
	EnvPending bool
	EnvError   string
	EnvName    string
```

- [ ] **Step 5: Fill the project view**

In `internal/web/projects_admin.go` replace the end of `projectView`:

```go
	return page{View: "project", Project: p, Jobs: jobs, Secrets: secrets}, nil
```

with

```go
	vars, err := s.Store.Env.List(p.ID)
	if err != nil {
		return page{}, errors.New("cannot read variables")
	}
	pending, err := s.Store.EnvPending(p)
	if err != nil {
		return page{}, errors.New("cannot compare variables with the active release")
	}
	rows := make([]envRow, 0, len(vars))
	for _, v := range vars {
		rows = append(rows, envRow{Name: v.Name, Build: v.Build})
	}
	return page{View: "project", Project: p, Jobs: jobs, Secrets: secrets, EnvRows: rows, EnvPending: pending}, nil
```

- [ ] **Step 6: Add the templates**

In `internal/web/templates.go`:

1. Replace `"secret" "confirm-rollback")}}` with `"secret" "env-value" "confirm-rollback")}}` (navigation highlight).
2. Replace `<section id="secrets">` with the environment section followed by the original tag:

```html
<section id="env"><h2>Environment variables</h2><p class="muted">Stored on this host, never in Git. Dockerfile mode passes every variable to the container; Compose mode uses them where <code>environment:</code> or <code>build.args:</code> reference <code>${NAME}</code> or <code>${NAME:-default}</code>. Changes apply when you deploy them; restoring a release restores its values.</p>{{if .EnvPending}}<div class="notice" role="status"><p><strong>Undeployed changes.</strong> The active release runs with different values.</p><form method="post" action="/projects/{{.Project.ID}}/deploy-changes"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button>{{template "i-rocket"}}Deploy changes</button></form></div>{{end}}{{if .EnvError}}<p class="notice" role="alert">{{.EnvError}}</p>{{end}}{{if .EnvRows}}<div class="scroll"><table><thead><tr><th>Name</th><th>Value</th><th>Available</th><th><span class="sr-only">Actions</span></th></tr></thead><tbody>{{range .EnvRows}}<tr><td><code>{{.Name}}</code></td><td><span class="muted">••••••••</span></td><td>{{if .Build}}Build and runtime{{else}}Runtime{{end}}</td><td class="row-action"><form method="post" action="/projects/{{$.Project.ID}}/env/{{.Name}}/reveal"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary small">Show</button></form> <form method="post" action="/projects/{{$.Project.ID}}/env/{{.Name}}/delete"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary small" data-confirm="Delete {{.Name}}? Deploy changes afterwards to remove it from the running application.">Delete</button></form></td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No variables yet.</p>{{end}}
<form method="post" action="/projects/{{.Project.ID}}/env" class="subform"><input type="hidden" name="_csrf" value="{{.CSRF}}"><div class="grid"><label>Name<input name="name" required maxlength="100" pattern="[A-Za-z_][A-Za-z0-9_]*" placeholder="DATABASE_URL" autocomplete="off"></label><label class="inline"><input type="checkbox" name="build"> Also available at build time (stored in the image history)</label></div><label>Value<textarea name="value" rows="2" autocomplete="off" spellcheck="false"></textarea></label><div class="form-foot"><button>Save variable</button><small>Saving an existing name replaces its value and build setting.</small></div></form>
<details><summary>Paste a .env file</summary><form method="post" action="/projects/{{.Project.ID}}/env/import" class="subform"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>.env contents<textarea name="dotenv" rows="6" autocomplete="off" spellcheck="false" placeholder="DATABASE_URL=postgres://..."></textarea></label><div class="form-foot"><button>Import variables</button><small>Existing names are replaced; their build settings are kept.</small></div></form></details></section>
<section id="secrets">
```

3. Replace `{{if eq .View "secret"}}` with the reveal view followed by the original text:

```html
{{if eq .View "env-value"}}<section class="narrow-section reveal"><h1><code>{{.EnvName}}</code></h1><p>Current value of this environment variable. It is only displayed in this response.</p><div class="secret-box"><span id="secret-value" class="secret-value"><code>{{.Secret}}</code></span><button type="button" class="secondary small" data-copy="secret-value">Copy</button></div><p><a href="/projects/{{.Project.ID}}#env">← Back to project</a></p></section>{{end}}
{{if eq .View "secret"}}
```

- [ ] **Step 7: Run the web tests**

Run: `go test ./internal/web/`
Expected: PASS (new and existing tests).

- [ ] **Step 8: Commit**

```bash
git add internal/web
git commit -m "Manage environment variables and deploy changes from the API and panel"
```

---

### Task 5: CLI — `env` commands and `deploy-changes`

**Files:**
- Modify: `cmd/repodeploy/main.go`
- Test: `cmd/repodeploy/env_test.go`

**Interfaces:**
- Consumes: API from Task 4; `envvars.Update`, `envvars.ParseDotEnv`, `envvars.MaxValueLen`.
- Produces (used by Task 10 tests): test helpers `cliFixture(t) (root string, s *deploy.Store, h *httptest.Server)` and `cli(t, root, url, stdin string, args ...string) (string, error)` in `cmd/repodeploy/env_test.go`.

- [ ] **Step 1: Write the failing test**

Create `cmd/repodeploy/env_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/envvars"
	"github.com/vicape92/repodeploy/internal/web"
)

func cliFixture(t *testing.T) (string, *deploy.Store, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	if err := web.Initialize(root, "correct-horse-battery-staple"); err != nil {
		t.Fatal(err)
	}
	s, err := deploy.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	server, err := web.New(s, false)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(server.Handler())
	t.Cleanup(h.Close)
	return root, s, h
}

func cli(t *testing.T, root, url, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), append([]string{"--state", root, "--url", url}, args...), strings.NewReader(stdin), &out)
	return out.String(), err
}

func TestCLIManagesEnvironmentVariables(t *testing.T) {
	root, s, h := cliFixture(t)
	p, err := s.SaveProject(deploy.Project{Name: "app", Repo: "https://github.com/team/app.git", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli(t, root, h.URL, "s3cr3t-value\n", "env", "set", p.ID, "API_KEY", "--build"); err != nil {
		t.Fatal(err)
	}
	dotenv := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(dotenv, []byte("A=1\nB='two'\n"), 0600)
	if _, err := cli(t, root, h.URL, "", "env", "import", p.ID, dotenv); err != nil {
		t.Fatal(err)
	}
	vars, _ := s.Env.List(p.ID)
	want := []envvars.Var{{Name: "A", Value: "1"}, {Name: "API_KEY", Value: "s3cr3t-value", Build: true}, {Name: "B", Value: "two"}}
	if !reflect.DeepEqual(vars, want) {
		t.Fatalf("stored: %+v", vars)
	}
	out, err := cli(t, root, h.URL, "", "env", "list", p.ID)
	if err != nil || strings.Contains(out, "s3cr3t") || !strings.Contains(out, "API_KEY") {
		t.Fatalf("list: %v %s", err, out)
	}
	if out, _ = cli(t, root, h.URL, "", "env", "list", p.ID, "--reveal"); !strings.Contains(out, "s3cr3t-value") {
		t.Fatalf("reveal: %s", out)
	}
	if _, err := cli(t, root, h.URL, "", "env", "unset", p.ID, "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := cli(t, root, h.URL, "", "env", "unset", p.ID, "A"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("second unset: %v", err)
	}
	if _, err := cli(t, root, h.URL, "", "env", "frobnicate", p.ID); err == nil {
		t.Fatal("unknown env command accepted")
	}
	if _, err := cli(t, root, h.URL, "", "deploy-changes", p.ID); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("deploy-changes without release: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/repodeploy/ -run TestCLIManagesEnvironmentVariables`
Expected: FAIL — `unknown command "env"`.

- [ ] **Step 3: Implement the commands**

In `cmd/repodeploy/main.go`, add the import `"github.com/vicape92/repodeploy/internal/envvars"`.

Change

```go
	case "deploy", "resume", "webhook-secret":
```

to

```go
	case "deploy", "resume", "webhook-secret", "deploy-changes":
```

Insert a new case before `case "secret":`:

```go
	case "env":
		if err = need(3); err != nil {
			return err
		}
		path = "/api/projects/" + url.PathEscape(args[2]) + "/env"
		switch args[1] {
		case "list":
			for _, option := range args[3:] {
				if option != "--reveal" {
					return fmt.Errorf("unknown env list option %q", option)
				}
				path += "?reveal=1"
			}
		case "set":
			if err = need(4); err != nil {
				return err
			}
			update := envvars.Update{Name: args[3]}
			for _, option := range args[4:] {
				build := option == "--build"
				if !build && option != "--no-build" {
					return fmt.Errorf("unknown env set option %q", option)
				}
				update.Build = &build
			}
			value, err := io.ReadAll(io.LimitReader(in, envvars.MaxValueLen+2))
			if err != nil {
				return err
			}
			update.Value = strings.TrimSuffix(strings.TrimSuffix(string(value), "\n"), "\r")
			body, _ = json.Marshal([]envvars.Update{update})
			method = http.MethodPut
		case "unset":
			if err = need(4); err != nil {
				return err
			}
			path += "/" + url.PathEscape(args[3])
			method = http.MethodDelete
		case "import":
			if err = need(4); err != nil {
				return err
			}
			text, err := readFile(args[3], in)
			if err != nil {
				return err
			}
			updates, err := envvars.ParseDotEnv(string(text))
			if err != nil {
				return err
			}
			body, _ = json.Marshal(updates)
			method = http.MethodPut
		default:
			return fmt.Errorf("unknown env command %q; use list, set, unset or import", args[1])
		}
```

In the `help` constant, insert after the `deploy ID` line:

```
  deploy-changes ID                Redeploy the active release with current variables
  env list ID [--reveal]           List environment variables (values with --reveal)
  env set ID NAME [--build|--no-build]
                                   Store a variable; the value is read from stdin
  env unset ID NAME                Delete a variable
  env import ID FILE|-             Store every variable of a .env file
```

- [ ] **Step 4: Run the CLI tests**

Run: `go test ./cmd/repodeploy/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/repodeploy
git commit -m "Add env and deploy-changes CLI commands"
```

---

### Task 6: `upload` module — safe extraction, normalisation, storage and retention

**Files:**
- Create: `internal/upload/archive.go`
- Create: `internal/upload/store.go`
- Test: `internal/upload/archive_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `const DefaultMaxBytes int64 = 200 << 20`
  - `type Limits struct { MaxExpanded int64; MaxEntries int }`, `func DefaultLimits(maxBytes int64) Limits`
  - `type Error struct { Message string }` (client error, HTTP 400)
  - `func Pack(root string, files []string, w io.Writer) error`
  - `func New(root string) *Store`
  - `func (s *Store) TempDir() (string, error)`
  - `func (s *Store) Save(projectID, archive string, lim Limits) (string, error)` — returns the SHA-256 hex
  - `func (s *Store) Prepare(projectID, hash, dest string) error`
  - `func (s *Store) Prune(projectID string, keep map[string]bool, minAge time.Duration) error`
  - `func (s *Store) DeleteProject(projectID string) error`
  - `func (s *Store) SweepTemp(age time.Duration) error`
  - `func ValidHash(ref string) bool`

- [ ] **Step 1: Write the failing tests**

Create `internal/upload/archive_test.go`:

```go
package upload

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type entry struct {
	name, body, link string
	mode             int64
	kind             byte
}

func tgz(t *testing.T, entries ...entry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		kind, mode := e.kind, e.mode
		if kind == 0 {
			kind = tar.TypeReg
		}
		if mode == 0 {
			mode = 0644
		}
		h := &tar.Header{Name: e.name, Typeflag: kind, Mode: mode, Linkname: e.link, ModTime: time.Now()}
		if kind == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	return path
}

func zipFile(t *testing.T, entries ...entry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := fs.FileMode(e.mode)
		if mode == 0 {
			mode = 0644
		}
		body := e.body
		switch e.kind {
		case tar.TypeSymlink:
			mode |= fs.ModeSymlink
			body = e.link
		case tar.TypeDir:
			mode |= fs.ModeDir
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
	return path
}

func plainFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	os.WriteFile(path, []byte(body), 0600)
	return path
}

func TestSaveNormalisesZipAndTarToTheSameHash(t *testing.T) {
	s := New(t.TempDir())
	entries := []entry{
		{name: "app/Dockerfile", body: "FROM scratch"},
		{name: "app/run.sh", body: "#!/bin/sh", mode: 0755},
		{name: "app/link", link: "run.sh", kind: tar.TypeSymlink},
		{name: "__MACOSX/app/._run.sh", body: "junk"},
		{name: "app/.DS_Store", body: "junk"},
	}
	fromTar, err := s.Save("p1", tgz(t, entries...), DefaultLimits(DefaultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	fromZip, err := s.Save("p1", zipFile(t, entries...), DefaultLimits(DefaultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	if fromTar != fromZip || !ValidHash(fromTar) {
		t.Fatalf("hashes differ: %s %s", fromTar, fromZip)
	}
	dest := filepath.Join(t.TempDir(), "src")
	if err = s.Prepare("p1", fromTar, dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "Dockerfile")); string(b) != "FROM scratch" {
		t.Fatalf("single root not flattened: %q", b)
	}
	if info, _ := os.Stat(filepath.Join(dest, "run.sh")); info.Mode().Perm() != 0755 {
		t.Fatalf("executable bit lost: %v", info.Mode())
	}
	if target, _ := os.Readlink(filepath.Join(dest, "link")); target != "run.sh" {
		t.Fatalf("symlink: %q", target)
	}
	for _, junk := range []string{"__MACOSX", ".DS_Store"} {
		if _, err := os.Stat(filepath.Join(dest, junk)); !os.IsNotExist(err) {
			t.Fatalf("%s extracted", junk)
		}
	}
	if stored, _ := os.Stat(filepath.Join(s.root, "p1", fromTar+".tar.gz")); stored.Mode().Perm() != 0600 {
		t.Fatalf("archive mode %v", stored.Mode())
	}
}

func TestSaveRejectsUnsafeArchivesAndLeavesNothing(t *testing.T) {
	lim := Limits{MaxExpanded: 1024, MaxEntries: 5}
	cases := map[string]string{
		"traversal":      tgz(t, entry{name: "../evil", body: "x"}),
		"absolute":       tgz(t, entry{name: "/etc/evil", body: "x"}),
		"backslash":      zipFile(t, entry{name: `..\evil`, body: "x"}),
		"escaping link":  tgz(t, entry{name: "link", link: "../../etc", kind: tar.TypeSymlink}),
		"absolute link":  tgz(t, entry{name: "link", link: "/etc/passwd", kind: tar.TypeSymlink}),
		"through link":   tgz(t, entry{name: "a/", kind: tar.TypeDir}, entry{name: "a/up", link: "..", kind: tar.TypeSymlink}, entry{name: "a/up/x", body: "x"}),
		"hardlink":       tgz(t, entry{name: "f", body: "x"}, entry{name: "h", link: "f", kind: tar.TypeLink}),
		"fifo":           tgz(t, entry{name: "pipe", kind: tar.TypeFifo}),
		"duplicate":      tgz(t, entry{name: "f", body: "x"}, entry{name: "f", body: "y"}),
		"too large":      tgz(t, entry{name: "big", body: strings.Repeat("x", 2048)}),
		"too many":       tgz(t, entry{name: "1", body: "x"}, entry{name: "2", body: "x"}, entry{name: "3", body: "x"}, entry{name: "4", body: "x"}, entry{name: "5", body: "x"}, entry{name: "6", body: "x"}),
		"empty":          tgz(t),
		"not an archive": plainFile(t, "plain text"),
	}
	for name, archive := range cases {
		t.Run(name, func(t *testing.T) {
			s := New(t.TempDir())
			_, err := s.Save("p1", archive, lim)
			var invalid *Error
			if !errors.As(err, &invalid) {
				t.Fatalf("expected *upload.Error, got %v", err)
			}
			if left, _ := filepath.Glob(filepath.Join(s.root, "*", "*")); len(left) != 0 {
				t.Fatalf("files left behind: %v", left)
			}
		})
	}
}

func TestPrepareRetentionAndDeletion(t *testing.T) {
	s := New(t.TempDir())
	a, err := s.Save("p1", tgz(t, entry{name: "v", body: "a"}), DefaultLimits(DefaultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Save("p1", tgz(t, entry{name: "v", body: "b"}), DefaultLimits(DefaultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare("p1", "not-a-hash", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("invalid hash accepted")
	}
	if err := s.Prune("p1", map[string]bool{a: true}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare("p1", b, filepath.Join(t.TempDir(), "x")); err != nil {
		t.Fatal("recent archive was pruned")
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, h := range []string{a, b} {
		os.Chtimes(filepath.Join(s.root, "p1", h+".tar.gz"), old, old)
	}
	if err := s.Prune("p1", map[string]bool{a: true}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare("p1", a, filepath.Join(t.TempDir(), "x")); err != nil {
		t.Fatal("kept archive was pruned")
	}
	if err := s.Prepare("p1", b, filepath.Join(t.TempDir(), "x")); err == nil || !strings.Contains(err.Error(), "upload it again") {
		t.Fatalf("pruned archive: %v", err)
	}
	if err := s.DeleteProject("p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.root, "p1")); !os.IsNotExist(err) {
		t.Fatal("project archives remain")
	}
	if err := s.DeleteProject(""); err == nil {
		t.Fatal("empty project ID accepted")
	}
}

func TestSweepTempRemovesOnlyOldFiles(t *testing.T) {
	s := New(t.TempDir())
	dir, err := s.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "old"), []byte("x"), 0600)
	os.WriteFile(filepath.Join(dir, "new"), []byte("x"), 0600)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(dir, "old"), old, old)
	if err := s.SweepTemp(24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old")); !os.IsNotExist(err) {
		t.Fatal("old temp file kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); err != nil {
		t.Fatal("recent temp file removed")
	}
}

func TestPackIsDeterministicAndRejectsOutsideSymlinks(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a"), []byte("x"), 0600)
	var one, two bytes.Buffer
	if err := Pack(root, []string{"a"}, &one); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(root, "a"), later, later)
	if err := Pack(root, []string{"a"}, &two); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one.Bytes(), two.Bytes()) {
		t.Fatal("pack depends on modification times")
	}
	os.Symlink("../outside", filepath.Join(root, "bad"))
	if err := Pack(root, []string{"a", "bad"}, io.Discard); err == nil {
		t.Fatal("escaping symlink packed")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/upload/`
Expected: FAIL — `undefined: New`, `undefined: Pack`, etc.

- [ ] **Step 3: Implement extraction and packing**

Create `internal/upload/archive.go`:

```go
// Package upload owns manually uploaded source archives: safe extraction,
// normalisation into content-addressed tar.gz files, retention, and the
// client-side file selection and packing used by "repodeploy up". It depends
// only on the standard library.
package upload

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultMaxBytes is the default largest accepted upload archive.
const DefaultMaxBytes int64 = 200 << 20

// Limits bounds what an archive may expand to.
type Limits struct {
	MaxExpanded int64
	MaxEntries  int
}

// DefaultLimits allows five times the upload size and 50 000 entries.
func DefaultLimits(maxBytes int64) Limits {
	return Limits{MaxExpanded: 5 * maxBytes, MaxEntries: 50000}
}

// unlimited re-extracts archives that were validated when they were stored.
var unlimited = Limits{MaxExpanded: 1 << 62, MaxEntries: 1 << 30}

// Error is a problem with the uploaded archive itself.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func invalid(format string, a ...any) error { return &Error{Message: fmt.Sprintf(format, a...)} }

type extractor struct {
	root    string
	lim     Limits
	written int64
	entries int
}

// extract unpacks a zip or gzip-compressed tar archive into dest, which must
// not exist yet. Unsafe entries abort the extraction.
func extract(archive, dest string, lim Limits) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, 4)
	n, _ := io.ReadFull(f, magic)
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err = os.Mkdir(dest, 0755); err != nil {
		return err
	}
	x := &extractor{root: dest, lim: lim}
	switch {
	case n == 4 && string(magic) == "PK\x03\x04":
		info, err := f.Stat()
		if err != nil {
			return err
		}
		return x.zip(f, info.Size())
	case n >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		return x.tarGz(f)
	default:
		return invalid("upload a .zip or .tar.gz archive")
	}
}

func (x *extractor) zip(r io.ReaderAt, size int64) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return invalid("the zip archive is damaged: %v", err)
	}
	for _, f := range zr.File {
		mode := f.Mode()
		switch {
		case mode.IsDir():
			err = x.dir(f.Name)
		case mode&fs.ModeSymlink != 0:
			err = x.zipSymlink(f)
		case mode.IsRegular():
			var rc io.ReadCloser
			if rc, err = f.Open(); err != nil {
				return invalid("cannot read %s: %v", f.Name, err)
			}
			err = x.file(f.Name, rc, mode)
			rc.Close()
		default:
			err = invalid("%s is not a regular file, directory or symlink", f.Name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (x *extractor) zipSymlink(f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return invalid("cannot read %s: %v", f.Name, err)
	}
	defer rc.Close()
	target, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil {
		return invalid("cannot read %s: %v", f.Name, err)
	}
	return x.symlink(f.Name, string(target))
}

func (x *extractor) tarGz(r io.Reader) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return invalid("the tar.gz archive is damaged: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return invalid("the tar.gz archive is damaged: %v", err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = x.dir(h.Name)
		case tar.TypeReg:
			err = x.file(h.Name, tr, fs.FileMode(h.Mode))
		case tar.TypeSymlink:
			err = x.symlink(h.Name, h.Linkname)
		case tar.TypeXGlobalHeader:
			continue
		default:
			err = invalid("%s is not a regular file, directory or symlink", h.Name)
		}
		if err != nil {
			return err
		}
	}
}

// clean validates an entry name and returns its slash-separated relative
// path, or "" for entries that are skipped.
func clean(name string) (string, error) {
	name = strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(name, "/") || (len(name) > 1 && name[1] == ':') {
		return "", invalid("%s uses an absolute path", name)
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", invalid("%s points outside the archive", name)
	}
	if cleaned == "." || strings.Split(cleaned, "/")[0] == "__MACOSX" || path.Base(cleaned) == ".DS_Store" {
		return "", nil
	}
	return cleaned, nil
}

// prepare counts the entry, creates missing parent directories and refuses
// to write through an existing symlink, so a link created by an earlier
// entry can never redirect a later one outside the root.
func (x *extractor) prepare(rel string) (string, error) {
	x.entries++
	if x.entries > x.lim.MaxEntries {
		return "", invalid("the archive has more than %d entries", x.lim.MaxEntries)
	}
	parts := strings.Split(rel, "/")
	current := x.root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err = os.Mkdir(current, 0755); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", invalid("%s writes through the symlink %s", rel, part)
		}
		if !info.IsDir() {
			return "", invalid("%s is inside %s, which is not a directory", rel, part)
		}
	}
	return filepath.Join(x.root, filepath.FromSlash(rel)), nil
}

func (x *extractor) dir(name string) error {
	rel, err := clean(name)
	if err != nil || rel == "" {
		return err
	}
	target, err := x.prepare(rel)
	if err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if err == nil {
		if info.IsDir() {
			return nil
		}
		return invalid("%s appears twice in the archive", rel)
	}
	return os.Mkdir(target, 0755)
}

func (x *extractor) file(name string, r io.Reader, mode fs.FileMode) error {
	rel, err := clean(name)
	if err != nil || rel == "" {
		return err
	}
	target, err := x.prepare(rel)
	if err != nil {
		return err
	}
	perm := fs.FileMode(0644)
	if mode&0111 != 0 {
		perm = 0755
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, fs.ErrExist) {
		return invalid("%s appears twice in the archive", rel)
	}
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, x.lim.MaxExpanded-x.written+1))
	x.written += n
	closeErr := f.Close()
	if err != nil {
		return invalid("cannot extract %s: %v", rel, err)
	}
	if closeErr != nil {
		return closeErr
	}
	if x.written > x.lim.MaxExpanded {
		return invalid("the archive expands to more than %d MB", x.lim.MaxExpanded>>20)
	}
	return os.Chmod(target, perm)
}

func (x *extractor) symlink(name, target string) error {
	rel, err := clean(name)
	if err != nil || rel == "" {
		return err
	}
	if target == "" || strings.HasPrefix(target, "/") || strings.Contains(target, `\`) {
		return invalid("symlink %s must use a relative target", rel)
	}
	resolved := path.Clean(path.Join(path.Dir(rel), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return invalid("symlink %s points outside the archive", rel)
	}
	dest, err := x.prepare(rel)
	if err != nil {
		return err
	}
	if err = os.Symlink(target, dest); errors.Is(err, fs.ErrExist) {
		return invalid("%s appears twice in the archive", rel)
	}
	return err
}

// Pack writes a deterministic tar.gz of the listed paths (slash-separated,
// relative to root): sorted entries, zero timestamps and owners, and modes
// reduced to 0755/0644. Symlinks must stay inside root.
func Pack(root string, files []string, w io.Writer) error {
	sorted := append([]string{}, files...)
	sort.Strings(sorted)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, rel := range sorted {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		h := &tar.Header{Name: rel, ModTime: time.Unix(0, 0), Mode: 0644}
		switch {
		case info.IsDir():
			h.Typeflag, h.Name, h.Mode = tar.TypeDir, rel+"/", 0755
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			target = filepath.ToSlash(target)
			resolved := path.Clean(path.Join(path.Dir(rel), target))
			if path.IsAbs(target) || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("symlink %s points outside the project", rel)
			}
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, target, 0777
		case info.Mode().IsRegular():
			h.Typeflag, h.Size = tar.TypeReg, info.Size()
			if info.Mode()&0111 != 0 {
				h.Mode = 0755
			}
		default:
			return fmt.Errorf("%s is not a regular file, directory or symlink", rel)
		}
		if err = tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			f, err := os.Open(full)
			if err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, h.Size)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
```

- [ ] **Step 4: Implement the store**

Create `internal/upload/store.go`:

```go
package upload

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Store keeps normalised archives at root/<project>/<sha256>.tar.gz and
// in-flight files under root/tmp.
type Store struct{ root string }

// New returns a store rooted at root (the state's uploads directory).
func New(root string) *Store { return &Store{root: root} }

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidHash reports whether ref can name a stored archive.
func ValidHash(ref string) bool { return hashPattern.MatchString(ref) }

// TempDir returns the private directory for in-flight uploads.
func (s *Store) TempDir() (string, error) {
	dir := filepath.Join(s.root, "tmp")
	return dir, os.MkdirAll(dir, 0700)
}

func (s *Store) path(projectID, hash string) string {
	return filepath.Join(s.root, projectID, hash+".tar.gz")
}

// Save validates the archive, normalises it and stores it, returning the
// SHA-256 of the normalised archive. Nothing remains on disk on error.
func (s *Store) Save(projectID, archive string, lim Limits) (string, error) {
	tmp, err := s.TempDir()
	if err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(tmp, "extract-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	tree := filepath.Join(work, "tree")
	if err = extract(archive, tree, lim); err != nil {
		return "", err
	}
	if tree, err = singleRoot(tree); err != nil {
		return "", err
	}
	files, err := listTree(tree)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", invalid("the archive is empty")
	}
	packed, err := os.CreateTemp(tmp, "packed-")
	if err != nil {
		return "", err
	}
	defer os.Remove(packed.Name())
	sum := sha256.New()
	err = Pack(tree, files, io.MultiWriter(packed, sum))
	if err == nil {
		err = packed.Sync()
	}
	if closeErr := packed.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	hash := hex.EncodeToString(sum.Sum(nil))
	if err = os.MkdirAll(filepath.Join(s.root, projectID), 0700); err != nil {
		return "", err
	}
	if err = os.Rename(packed.Name(), s.path(projectID, hash)); err != nil {
		return "", err
	}
	return hash, nil
}

// singleRoot descends into the only top-level directory, as produced by
// "compress folder" tools.
func singleRoot(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(dir, entries[0].Name()), nil
	}
	return dir, nil
}

// listTree returns every path below root, directories included, so empty
// directories survive normalisation.
func listTree(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// Prepare extracts a stored archive into dest, replacing anything a previous
// interrupted attempt left there.
func (s *Store) Prepare(projectID, hash, dest string) error {
	if !ValidHash(hash) {
		return errors.New("invalid upload identifier")
	}
	archive := s.path(projectID, hash)
	if _, err := os.Stat(archive); err != nil {
		return errors.New("upload archive unavailable; upload it again")
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return extract(archive, dest, unlimited)
}

// Prune removes the project's archives that keep does not list, except those
// modified within minAge: their deployment may not be queued yet.
func (s *Store) Prune(projectID string, keep map[string]bool, minAge time.Duration) error {
	paths, err := filepath.Glob(filepath.Join(s.root, projectID, "*.tar.gz"))
	if err != nil {
		return err
	}
	for _, p := range paths {
		hash := strings.TrimSuffix(filepath.Base(p), ".tar.gz")
		info, err := os.Stat(p)
		if err != nil || keep[hash] || time.Since(info.ModTime()) < minAge {
			continue
		}
		if err = os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// DeleteProject removes every archive of a project.
func (s *Store) DeleteProject(projectID string) error {
	if projectID == "" || projectID == "tmp" || strings.ContainsAny(projectID, `/\.`) {
		return errors.New("invalid project ID")
	}
	return os.RemoveAll(filepath.Join(s.root, projectID))
}

// SweepTemp removes in-flight files older than age, left by interrupted
// uploads or daemon restarts.
func (s *Store) SweepTemp(age time.Duration) error {
	dir, err := s.TempDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) <= age {
			continue
		}
		if err = os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/upload/ -v`
Expected: PASS (all five tests and every unsafe-archive subtest).

- [ ] **Step 6: Commit**

```bash
git add internal/upload
git commit -m "Add upload module: safe extraction, normalised archives and retention"
```

---

### Task 7: Upload projects — model, validation and panel

**Files:**
- Modify: `internal/deploy/store.go` (`Project.Source`, `IsUpload`, `SaveProject`)
- Modify: `internal/deploy/validate.go`
- Create: `internal/web/uploads.go` (new-project page)
- Modify: `internal/web/server.go` (`projectFromForm`, `saveProject`, `deploy`, `webhookSecret`, route)
- Modify: `internal/web/templates.go`
- Test: `internal/deploy/upload_project_test.go`, `internal/web/uploads_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `Project.Source string` (JSON `source,omitempty`; `""`/`"git"` or `"upload"`)
  - `func (p Project) IsUpload() bool`
  - `GET /projects/new?source=upload` page
  - web test helper `savedUploadProject(t, server) deploy.Project` (used by Task 9)

- [ ] **Step 1: Write the failing deploy tests**

Create `internal/deploy/upload_project_test.go`:

```go
package deploy

import "testing"

func uploadProject() Project {
	return Project{Name: "site", Source: "upload", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"}
}

func TestValidateUploadProject(t *testing.T) {
	if err := ValidateProject(uploadProject()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Project){
		"repo":   func(p *Project) { p.Repo = "https://github.com/a/b.git" },
		"branch": func(p *Project) { p.Branch = "main" },
		"ssh":    func(p *Project) { p.SSHKey = "/keys/app" },
		"ci":     func(p *Project) { p.WaitCI = true },
		"poll":   func(p *Project) { p.Mode = "poll" },
		"source": func(p *Project) { p.Source = "ftp" },
	} {
		p := uploadProject()
		mutate(&p)
		if err := ValidateProject(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	git := uploadProject()
	git.Source = ""
	if err := ValidateProject(git); ErrorField(err) != "repo" {
		t.Fatalf("Git project without repository: %v", err)
	}
}

func TestSaveProjectForcesManualModeForUploads(t *testing.T) {
	s := testStore(t)
	p := uploadProject()
	p.EnvDigest = "stale"
	saved, err := s.SaveProject(p)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Mode != "manual" || saved.EnvDigest != "" || !saved.IsUpload() {
		t.Fatalf("%+v", saved)
	}
}
```

- [ ] **Step 2: Write the failing web tests**

Create `internal/web/uploads_test.go`:

```go
package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
)

func savedUploadProject(t *testing.T, server *Server) deploy.Project {
	t.Helper()
	p, err := server.Store.SaveProject(deploy.Project{Name: "site", Source: "upload", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUploadProjectsKeepTheirSourceAndRefuseGitActions(t *testing.T) {
	server, h, token := webFixture(t)
	p := savedUploadProject(t, server)
	git := savedProject(t, server)
	toGit := p
	toGit.Source, toGit.Repo, toGit.Branch, toGit.Mode = "", "https://github.com/a/b.git", "main", "manual"
	resp := request(t, "PUT", h.URL+"/api/projects/"+p.ID, token, toGit)
	if body := readBody(t, resp.Body); resp.StatusCode != 400 || !strings.Contains(body, "cannot change") {
		t.Fatalf("upload to Git: %d %s", resp.StatusCode, body)
	}
	toUpload := git
	toUpload.Source = "upload"
	resp = request(t, "PUT", h.URL+"/api/projects/"+git.ID, token, toUpload)
	if readBody(t, resp.Body); resp.StatusCode != 400 {
		t.Fatalf("Git to upload: %d", resp.StatusCode)
	}
	for _, action := range []string{"deploy", "webhook-secret"} {
		resp = request(t, "POST", h.URL+"/api/projects/"+p.ID+"/"+action, token, nil)
		if readBody(t, resp.Body); resp.StatusCode != 409 {
			t.Fatalf("%s on upload project: %d", action, resp.StatusCode)
		}
	}
}

func TestBrowserCreatesUploadProject(t *testing.T) {
	server, h, _ := webFixture(t)
	client, csrf := loggedBrowser(t, h.URL)
	resp, _ := client.Get(h.URL + "/")
	if page := readBody(t, resp.Body); !strings.Contains(page, `href="/projects/new?source=upload"`) {
		t.Fatal("dashboard has no upload entry point")
	}
	resp, _ = client.Get(h.URL + "/projects/new?source=upload")
	page := readBody(t, resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(page, `name="source" value="upload"`) || strings.Contains(page, `name="repo"`) || strings.Contains(page, `name="ssh_key"`) {
		t.Fatal("upload project form shows Git fields")
	}
	resp = postForm(t, client, h.URL+"/projects", url.Values{"_csrf": {csrf}, "source": {"upload"}, "name": {"site"}, "build_type": {"dockerfile"}, "dockerfile": {"Dockerfile"}, "port": {"18080"}, "container_port": {"8080"}, "bind_address": {"127.0.0.1"}, "health_url": {"http://127.0.0.1:18080/health"}, "health_timeout": {"60"}, "retain": {"5"}})
	readBody(t, resp.Body)
	if resp.StatusCode != 303 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	projects, _ := server.Store.Projects()
	if len(projects) != 1 || !projects[0].IsUpload() || projects[0].Mode != "manual" {
		t.Fatalf("%+v", projects)
	}
	resp, _ = client.Get(h.URL + resp.Header.Get("Location"))
	page = readBody(t, resp.Body)
	if !strings.Contains(page, "Manual upload") || strings.Contains(page, "Deploy branch") || strings.Contains(page, "GitHub webhook") {
		t.Fatal("project page shows Git controls for an upload project")
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/deploy/ ./internal/web/ -run 'Upload'`
Expected: FAIL to compile — `unknown field Source`, `IsUpload undefined`.

- [ ] **Step 4: Add `Source` to `Project`**

In `internal/deploy/store.go`, add to `Project` after `Name`:

```go
	// Source is "git" (or empty) for repository projects and "upload" for
	// projects that deploy uploaded archives.
	Source string `json:"source,omitempty"`
```

Add after the `Job` type:

```go
// IsUpload reports whether the project deploys uploaded archives.
func (p Project) IsUpload() bool { return p.Source == "upload" }
```

In `SaveProject`, after `p.EnvDigest, p.Env = "", nil`, add:

```go
	if p.IsUpload() {
		p.Mode = "manual"
	}
```

- [ ] **Step 5: Validate upload projects**

In `internal/deploy/validate.go`, replace the whole `ValidateProject` function with:

```go
func ValidateProject(p Project) error {
	if p.Source != "" && p.Source != "git" && p.Source != "upload" {
		return fieldErr("source", "source must be git or upload")
	}
	if p.IsUpload() {
		if p.Repo != "" || p.Branch != "" || p.Connection != "" || p.RepositoryID != "" || p.SSHKey != "" || p.KnownHosts != "" || p.WaitCI || p.CIProvider != "" {
			return fieldErr("source", "upload projects have no Git repository, branch, SSH key or CI/CD settings")
		}
		if p.Mode != "" && p.Mode != "manual" {
			return fieldErr("mode", "upload projects deploy when you upload; detection must be manual")
		}
	}
	if p.Connection != "" && p.Connection != "github" && p.Connection != "bitbucket" {
		return fieldErr("connection", "unsupported repository connection")
	}
	if p.Connection != "" && p.RepositoryID == "" {
		return fieldErr("repo", "select a repository from the connected provider")
	}
	if p.CITimeout < 0 || (p.CITimeout > 0 && p.CITimeout < 30) || p.CITimeout > 86400 {
		return fieldErr("ci_timeout", "CI/CD timeout must be 30–86400 seconds (zero selects 1800)")
	}
	if p.WaitCI && ((p.CIProvider != "github" && p.CIProvider != "bitbucket") || p.RepositoryID == "") {
		return fieldErr("wait_ci", "CI/CD requires browser authorization for the repository provider")
	}
	if strings.TrimSpace(p.Name) == "" || len(p.Name) > 100 {
		return fieldErr("name", "a project name of at most 100 characters is required")
	}
	if !p.IsUpload() {
		if err := validateGitSource(p); err != nil {
			return err
		}
	}
	if (p.Dockerfile == "") == (p.ComposeFile == "") {
		return fieldErr("build_type", "configure exactly one Dockerfile or Compose file")
	}
	for field, path := range map[string]string{"dockerfile": p.Dockerfile, "compose_file": p.ComposeFile} {
		if path != "" && (filepath.IsAbs(path) || filepath.Clean(path) == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator))) {
			return fieldErr(field, "Docker configuration paths must stay inside the repository")
		}
	}
	if p.ComposeFile == "" && (p.Port < 1 || p.Port > 65535) {
		return fieldErr("port", "the host port must be between 1 and 65535")
	}
	if p.ComposeFile == "" && (p.ContainerPort < 1 || p.ContainerPort > 65535) {
		return fieldErr("container_port", "the container port must be between 1 and 65535")
	}
	if p.BindAddress != "" {
		ip := net.ParseIP(p.BindAddress)
		if ip == nil {
			return fieldErr("bind_address", "bind address must be an IP address such as 127.0.0.1 or 0.0.0.0")
		}
		if !ip.IsLoopback() && !ip.IsUnspecified() {
			return fieldErr("bind_address", "bind address must be a loopback address or 0.0.0.0/:: so the loopback health check can reach the application")
		}
	}
	if p.ComposeFile == "" && p.HealthURL == "" {
		return fieldErr("health_url", "Dockerfile mode requires an HTTP health URL")
	}
	if p.HealthURL == "" && p.HealthService == "" {
		return fieldErr("health_service", "an HTTP health URL or Compose health service is required")
	}
	if p.HealthURL != "" {
		u, err := url.Parse(p.HealthURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || (u.Hostname() != "localhost" && (net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback())) {
			return fieldErr("health_url", "health URL must use HTTP(S) on a loopback address")
		}
	}
	if p.HealthTimeout < 0 || p.HealthTimeout > 300 {
		return fieldErr("health_timeout", "health timeout must be 1–300 seconds (zero selects 60)")
	}
	if p.PollSeconds < 0 || (p.PollSeconds > 0 && p.PollSeconds < 10) || p.PollSeconds > 86400 {
		return fieldErr("poll_seconds", "polling interval must be 10–86400 seconds (zero selects 60)")
	}
	if p.Retain < 0 || p.Retain > 50 {
		return fieldErr("retain", "versions to retain must be 1–50 (zero selects 5)")
	}
	if p.SSHKey != "" {
		for field, path := range map[string]string{"ssh_key": p.SSHKey, "known_hosts": p.KnownHosts} {
			if !filepath.IsAbs(path) {
				return fieldErr(field, "SSH paths must be absolute")
			}
			stat, err := os.Stat(path)
			if err != nil || !stat.Mode().IsRegular() {
				return fieldErr(field, "SSH key and known-hosts file must exist and be regular files")
			}
		}
		stat, _ := os.Stat(p.SSHKey)
		if stat.Mode().Perm()&0077 != 0 {
			return fieldErr("ssh_key", "SSH private key must not be accessible to group or others")
		}
	}
	return nil
}

// validateGitSource checks the repository, branch and detection settings of
// Git projects.
func validateGitSource(p Project) error {
	if strings.TrimSpace(p.Repo) == "" || len(p.Repo) > 2048 {
		return fieldErr("repo", "a Git URL of at most 2048 characters is required")
	}
	if err := gitInputs(p); err != nil {
		return err
	}
	u, err := url.Parse(p.Repo)
	scp := strings.HasPrefix(p.Repo, "git@") && strings.Contains(p.Repo, ":")
	if !scp && (err != nil || (u.Scheme != "https" && u.Scheme != "ssh") || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.User != nil && u.Scheme != "ssh")) {
		return fieldErr("repo", "use an HTTPS or SSH Git URL without embedded credentials")
	}
	if err := exec.Command("git", "check-ref-format", "--branch", p.Branch).Run(); err != nil {
		return fieldErr("branch", "invalid deployment branch")
	}
	if p.Mode != "manual" && p.Mode != "poll" && p.Mode != "webhook" && p.Mode != "both" {
		return fieldErr("mode", "mode must be manual, poll, webhook or both")
	}
	if (p.Mode == "webhook" || p.Mode == "both") && githubRepo(p.Repo) == "" {
		return fieldErr("mode", "webhook mode currently supports github.com repositories only")
	}
	return nil
}
```

- [ ] **Step 6: Web handlers for upload projects**

Create `internal/web/uploads.go`:

```go
package web

import (
	"net/http"

	"github.com/vicape92/repodeploy/internal/deploy"
)

// newProjectPage shows the form for a new upload project. Git projects start
// from the connections page.
func (s *Server) newProjectPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("source") != "upload" {
		http.Redirect(w, r, "/connections", 303)
		return
	}
	s.render(w, r, page{View: "edit", Project: deploy.Project{Source: "upload", Dockerfile: "Dockerfile", Port: 8080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:8080/health", Retain: 5, HealthTimeout: 60}})
}
```

In `internal/web/server.go`:

1. Register after `mux.HandleFunc("GET /{$}", s.dashboard)`:

```go
	mux.HandleFunc("GET /projects/new", s.newProjectPage)
```

2. In `projectFromForm`, after `p.PullImages = r.FormValue("pull_images") == "on"` add:

```go
	p.Source = r.FormValue("source")
	if p.IsUpload() {
		p.Repo, p.Branch, p.Mode, p.SSHKey, p.KnownHosts = "", "", "", "", ""
		p.Connection, p.RepositoryID, p.WaitCI, p.PollSeconds, p.CITimeout = "", "", false, 0, 0
	}
```

3. In `saveProject`, replace

```go
	if p.ID != "" {
		if _, err := s.Store.Project(p.ID); err != nil {
			http.NotFound(w, r)
			return
		}
	}
```

with

```go
	if p.ID != "" {
		existing, err := s.Store.Project(p.ID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if existing.IsUpload() != p.IsUpload() {
			fail(400, &deploy.FieldError{Field: "source", Message: "a project's source cannot change; create a new project instead"})
			return
		}
	}
```

4. Replace `deploy`:

```go
func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if p.IsUpload() {
		http.Error(w, "upload projects deploy when you upload a new version; use deploy-changes to apply new variables", 409)
		return
	}
	j, err := s.Store.Enqueue(p.ID, "manual", "", "", "")
	respondJob(w, r, j, err)
}
```

5. In `webhookSecret`, after the `NotFound` block add:

```go
	if p.IsUpload() {
		http.Error(w, "upload projects have no webhook", 409)
		return
	}
```

- [ ] **Step 7: Templates for upload projects**

In `internal/web/templates.go` make these replacements (each old string occurs once):

| Old | New |
|---|---|
| `Deploy Git commits on this host.` | `Deploy Git repositories or uploaded projects on this host.` |
| `{{template "i-plus"}}Connect a repository</a></div></div>` | `{{template "i-plus"}}Connect a repository</a><a class="button secondary" href="/projects/new?source=upload">{{template "i-box"}}Upload a project</a></div></div>` |
| `<p class="repo" title="{{.Repo}}">{{.Repo}}</p><ul class="chips"><li>{{template "i-branch"}}{{.Branch}}</li><li>{{template "i-pulse"}}{{.Mode}}</li>` | `{{if eq .Source "upload"}}<p class="repo">Manual upload</p><ul class="chips"><li>{{template "i-box"}}upload</li>{{else}}<p class="repo" title="{{.Repo}}">{{.Repo}}</p><ul class="chips"><li>{{template "i-branch"}}{{.Branch}}</li><li>{{template "i-pulse"}}{{.Mode}}</li>{{end}}` |
| `<p class="lede repo">{{.Project.Repo}}</p>` | `<p class="lede repo">{{if eq .Project.Source "upload"}}Manual upload{{else}}{{.Project.Repo}}{{end}}</p>` |
| `<div class="actions"><form method="post" action="/projects/{{.Project.ID}}/deploy">` | `<div class="actions">{{if ne .Project.Source "upload"}}<form method="post" action="/projects/{{.Project.ID}}/deploy">` |
| `Deploy branch</button></form>` | `Deploy branch</button></form>{{end}}` |
| `<dl class="stats"><div><dt>Branch</dt><dd>{{template "i-branch"}}<strong>{{.Project.Branch}}</strong></dd></div><div><dt>Detection</dt><dd>{{.Project.Mode}}</dd></div>` | `<dl class="stats">{{if eq .Project.Source "upload"}}<div><dt>Source</dt><dd>Manual upload</dd></div>{{else}}<div><dt>Branch</dt><dd>{{template "i-branch"}}<strong>{{.Project.Branch}}</strong></dd></div><div><dt>Detection</dt><dd>{{.Project.Mode}}</dd></div>{{end}}` |
| `<th>Commit</th>` | `<th>{{if eq .Project.Source "upload"}}Upload{{else}}Commit{{end}}</th>` |
| `class="mono">{{if .Commit}}{{short .Commit}}{{else}}Pending{{end}}</a>` | `class="mono">{{if .Commit}}{{if eq $.Project.Source "upload"}}upload {{end}}{{short .Commit}}{{else}}Pending{{end}}</a>` |
| `<details><summary>GitHub webhook</summary>` | `{{if ne .Project.Source "upload"}}<details><summary>GitHub webhook</summary>` |
| `Rotating invalidates the previous secret immediately.</small></p></details>` | `Rotating invalidates the previous secret immediately.</small></p></details>{{end}}` |
| `<dt>Commit</dt>` | `<dt>{{if eq .Job.Project.Source "upload"}}Upload{{else}}Commit{{end}}</dt>` |
| `<p class="muted small-text">Commit <code>{{.Job.Commit}}</code></p>` | `<p class="muted small-text">{{if eq .Job.Project.Source "upload"}}Upload{{else}}Commit{{end}} <code>{{.Job.Commit}}</code></p>` |
| `{{if .Wizard}}<input type="hidden" name="ssh_key"` | `{{if eq .Project.Source "upload"}}{{else if .Wizard}}<input type="hidden" name="ssh_key"` |
| `including when Git uses SSH.</p></fieldset>` | `including when Git uses SSH.</p></fieldset>{{end}}` |

Then replace `<fieldset><legend>Source and automation</legend>` with:

```html
{{if eq .Project.Source "upload"}}<input type="hidden" name="source" value="upload"><fieldset><legend>Project</legend><div class="grid"><label>Name<input {{field .ErrorField "name"}} name="name" value="{{.Project.Name}}" required maxlength="100"></label></div><p class="hint">Upload projects deploy the archive you upload from the project page or with <code>repodeploy up</code>. They have no repository, polling, webhook or CI/CD gate.</p></fieldset>{{else}}<fieldset><legend>Source and automation</legend>
```

- [ ] **Step 8: Run the tests**

Run: `go test ./internal/deploy/ ./internal/web/`
Expected: PASS (new and existing tests).

- [ ] **Step 9: Commit**

```bash
git add internal/deploy internal/web
git commit -m "Add upload projects: source field, validation and panel"
```

---

### Task 8: Engine deploys uploads — source interface, queue rules, retention, cleanup, backup

**Files:**
- Create: `internal/deploy/sources.go`
- Modify: `internal/deploy/store.go` (`Store.Uploads`, OpenStore, Enqueue/Claim/Pause SQL)
- Modify: `internal/deploy/engine.go` (`execute`, `prune`)
- Modify: `internal/deploy/delete.go`, `internal/deploy/backup.go`
- Modify: `cmd/repodeploy/main.go` (`serve` sweeps upload temp files)
- Test: `internal/deploy/upload_engine_test.go`

**Interfaces:**
- Consumes: `upload.Store`, `upload.New`, `upload.Pack`, `upload.DefaultLimits`, `upload.DefaultMaxBytes` (Task 6); `Project.IsUpload` (Task 7); `stepOnce` test helper (Task 3).
- Produces:
  - `Store.Uploads *upload.Store`
  - `type SourcePreparer interface { Prepare(ctx context.Context, p Project, ref, dest string) error }`
  - Job source `"upload"` behaves like `"manual"` for deduplication and paused automation.
  - test helpers `uploadFixture(t) (*Engine, *testRuntime, Project)` and `storeUpload(t, s, p, version string) string`.

- [ ] **Step 1: Write the failing tests**

Create `internal/deploy/upload_engine_test.go`:

```go
package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vicape92/repodeploy/internal/envvars"
	"github.com/vicape92/repodeploy/internal/upload"
)

func uploadFixture(t *testing.T) (*Engine, *testRuntime, Project) {
	t.Helper()
	s := testStore(t)
	p, err := s.SaveProject(uploadProject())
	if err != nil {
		t.Fatal(err)
	}
	r := &testRuntime{}
	return &Engine{Store: s, Runtime: r}, r, p
}

func storeUpload(t *testing.T, s *Store, p Project, version string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "version.txt"), []byte(version), 0600)
	archive := filepath.Join(t.TempDir(), "src.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err = upload.Pack(dir, []string{"version.txt"}, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	hash, err := s.Uploads.Save(p.ID, archive, upload.DefaultLimits(upload.DefaultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func archivePath(s *Store, p Project, hash string) string {
	return filepath.Join(s.Root, "uploads", p.ID, hash+".tar.gz")
}

func TestUploadDeploysArchiveAndRestartsFromAFreshExtraction(t *testing.T) {
	e, r, p := uploadFixture(t)
	hash := storeUpload(t, e.Store, p, "first")
	j, err := e.Store.Enqueue(p.ID, "upload", "", hash, "")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := e.Store.Claim()
	if err != nil || claimed.ID != j.ID {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	leftover := filepath.Join(e.Store.Root, "releases", j.ID, "source")
	os.MkdirAll(leftover, 0700)
	os.WriteFile(filepath.Join(leftover, "partial"), []byte("x"), 0600)
	if err = e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	j = stepOnce(t, e, j)
	logs, _ := e.Store.Logs(j.ID)
	if j.Phase != "succeeded" || j.Commit != hash || r.active != "first" || !strings.Contains(logs, "Upload "+hash) {
		t.Fatalf("job=%+v active=%s\n%s", j, r.active, logs)
	}
	if _, err := os.Stat(filepath.Join(leftover, "partial")); !os.IsNotExist(err) {
		t.Fatal("partial extraction reused")
	}
}

func TestUploadsRunWhilePausedAndManualDeployNeedsAnArchive(t *testing.T) {
	e, r, p := uploadFixture(t)
	first, _ := e.Store.Enqueue(p.ID, "upload", "", storeUpload(t, e.Store, p, "first"), "")
	if first = stepOnce(t, e, first); first.Phase != "succeeded" {
		t.Fatalf("first: %+v", first)
	}
	if err := e.Store.Pause(p.ID, false); err != nil {
		t.Fatal(err)
	}
	second, err := e.Store.Enqueue(p.ID, "upload", "", storeUpload(t, e.Store, p, "second"), "")
	if err != nil {
		t.Fatalf("paused project refused an upload: %v", err)
	}
	if second = stepOnce(t, e, second); second.Phase != "succeeded" || r.active != "second" {
		t.Fatalf("second: %+v active=%s", second, r.active)
	}
	manual, _ := e.Store.Enqueue(p.ID, "manual", "", "", "")
	if manual = stepOnce(t, e, manual); manual.Phase != "failed" || !strings.Contains(manual.Error, "upload a new version") {
		t.Fatalf("manual without archive: %+v", manual)
	}
}

func TestUploadRetentionKeepsReferencedArchivesOnly(t *testing.T) {
	e, _, p := uploadFixture(t)
	p.Retain = 1
	p, _ = e.Store.SaveProject(p)
	var hashes []string
	for _, version := range []string{"one", "two", "three"} {
		hash := storeUpload(t, e.Store, p, version)
		old := time.Now().Add(-2 * time.Hour)
		os.Chtimes(archivePath(e.Store, p, hash), old, old)
		j, _ := e.Store.Enqueue(p.ID, "upload", "", hash, "")
		if j = stepOnce(t, e, j); j.Phase != "succeeded" {
			t.Fatalf("%s: %+v", version, j)
		}
		hashes = append(hashes, hash)
	}
	exists := func(hash string) bool { _, err := os.Stat(archivePath(e.Store, p, hash)); return err == nil }
	if exists(hashes[0]) || !exists(hashes[1]) || !exists(hashes[2]) {
		t.Fatalf("retention: one=%v two=%v three=%v", exists(hashes[0]), exists(hashes[1]), exists(hashes[2]))
	}
}

func TestConfigChangeRebuildsTheActiveUpload(t *testing.T) {
	e, r, p := uploadFixture(t)
	hash := storeUpload(t, e.Store, p, "first")
	j, _ := e.Store.Enqueue(p.ID, "upload", "", hash, "")
	stepOnce(t, e, j)
	e.Store.Env.Set(p.ID, []envvars.Update{{Name: "COLOR", Value: "blue"}})
	change, err := e.Store.EnqueueConfigChange(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if change = stepOnce(t, e, change); change.Phase != "succeeded" || change.Commit != hash || len(r.env) != 1 {
		t.Fatalf("config change: %+v env=%v", change, r.env)
	}
}

func TestDeleteProjectRemovesUploads(t *testing.T) {
	e, _, p := uploadFixture(t)
	e.Runtime = &purgeRuntime{testRuntime: &testRuntime{}}
	j, _ := e.Store.Enqueue(p.ID, "upload", "", storeUpload(t, e.Store, p, "first"), "")
	stepOnce(t, e, j)
	if err := e.DeleteProject(context.Background(), p.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.Store.Root, "uploads", p.ID)); !os.IsNotExist(err) {
		t.Fatal("uploads remain after deletion")
	}
}

func TestBackupIncludesUploadsButNotTemporaryFiles(t *testing.T) {
	s := testStore(t)
	os.MkdirAll(filepath.Join(s.Root, "uploads", "p1"), 0700)
	os.WriteFile(filepath.Join(s.Root, "uploads", "p1", "x.tar.gz"), []byte("a"), 0600)
	os.MkdirAll(filepath.Join(s.Root, "uploads", "tmp"), 0700)
	os.WriteFile(filepath.Join(s.Root, "uploads", "tmp", "upload-1"), []byte("b"), 0600)
	var buf bytes.Buffer
	if err := Backup(s.Root, &buf); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
	if !names["uploads/p1/x.tar.gz"] || names["uploads/tmp/upload-1"] {
		t.Fatalf("backup entries: %v", names)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/deploy/ -run 'Upload|Uploads|Backup'`
Expected: FAIL to compile — `s.Uploads undefined`.

- [ ] **Step 3: Wire the upload store and queue rules**

In `internal/deploy/store.go`, add the import `"github.com/vicape92/repodeploy/internal/upload"`. Add to `Store` after `Env`:

```go
	Uploads *upload.Store
```

In `OpenStore`, change

```go
	for _, sub := range []string{"releases", "logs", "secrets"} {
```

to

```go
	for _, sub := range []string{"releases", "logs", "secrets", "uploads"} {
```

and the store literal to

```go
	s := &Store{Root: root, db: db, Sources: scm.New(root), Env: envvars.New(filepath.Join(root, "secrets")), Uploads: upload.New(filepath.Join(root, "uploads"))}
```

In `Enqueue`, replace every occurrence of `source != "manual"` (four occurrences) with `!userSource(source)`.

In `Claim`, replace `(p.paused=0 OR j.source='manual')` with `(p.paused=0 OR j.source IN ('manual','upload'))`.

In `Pause`, replace `(source!='manual' AND phase='queued')` with `(source NOT IN ('manual','upload') AND phase='queued')`.

- [ ] **Step 4: Add the source interface and upload retention**

Create `internal/deploy/sources.go`:

```go
package deploy

import (
	"context"
	"time"

	"github.com/vicape92/repodeploy/internal/upload"
)

// SourcePreparer materialises a project's source at ref (a commit SHA or an
// upload hash) into dest, replacing anything an earlier attempt left there.
type SourcePreparer interface {
	Prepare(ctx context.Context, p Project, ref, dest string) error
}

// uploadSource adapts the upload module to the engine.
type uploadSource struct{ uploads *upload.Store }

func (u uploadSource) Prepare(ctx context.Context, p Project, ref, dest string) error {
	return u.uploads.Prepare(p.ID, ref, dest)
}

// sourceFor selects how a project's code is prepared.
func (e *Engine) sourceFor(p Project) SourcePreparer {
	if p.IsUpload() {
		return uploadSource{uploads: e.Store.Uploads}
	}
	return Git{Sources: e.Store.Sources}
}

// userSource reports job sources started by the administrator. They bypass
// automatic deduplication and run while automation is paused.
func userSource(source string) bool { return source == "manual" || source == "upload" }

// uploadGrace protects freshly stored archives whose job may not be queued yet.
const uploadGrace = time.Hour

// pruneUploads removes archives that no retained job references.
func (s *Store) pruneUploads(projectID string) error {
	rows, err := s.db.Query(`SELECT DISTINCT sha FROM jobs WHERE project_id=? AND retained=1 AND sha!=''`, projectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	keep := map[string]bool{}
	for rows.Next() {
		var sha string
		if err = rows.Scan(&sha); err != nil {
			return err
		}
		keep[sha] = true
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return s.Uploads.Prune(projectID, keep, uploadGrace)
}
```

- [ ] **Step 5: Use the source interface in the engine**

In `internal/deploy/engine.go`, `execute`:

Replace `if j.Source != "manual" {` with `if !userSource(j.Source) {`.

Replace

```go
		if j.Commit == "" {
			j.Commit, err = (Git{Sources: e.Store.Sources}).Resolve(ctx, j.Project)
```

with

```go
		if j.Commit == "" {
			if j.Project.IsUpload() {
				return errors.New("upload projects deploy uploaded archives; upload a new version")
			}
			j.Commit, err = (Git{Sources: e.Store.Sources}).Resolve(ctx, j.Project)
```

Replace

```go
		fmt.Fprintf(log, "Commit %s\n", j.Commit)
```

with

```go
		if j.Project.IsUpload() {
			fmt.Fprintf(log, "Upload %s\n", j.Commit)
		} else {
			fmt.Fprintf(log, "Commit %s\n", j.Commit)
		}
```

Replace

```go
		if err = (Git{Sources: e.Store.Sources}).Prepare(ctx, j.Project, j.Commit, checkout); err != nil {
```

with

```go
		if err = e.sourceFor(j.Project).Prepare(ctx, j.Project, j.Commit, checkout); err != nil {
```

At the end of `prune`, replace the final `return nil` with:

```go
	if p.IsUpload() {
		return e.Store.pruneUploads(p.ID)
	}
	return nil
```

- [ ] **Step 6: Cleanup and backup**

In `internal/deploy/delete.go`, after the `e.Store.Env.DeleteProject` block (Task 3) add:

```go
	if err = e.Store.Uploads.DeleteProject(p.ID); err != nil {
		return err
	}
```

In `internal/deploy/backup.go`, inside the `switch parts[0]`, add before `case "releases":`:

```go
		case "uploads":
			// Archives let "deploy changes" rebuild after a restore; in-flight
			// files are not worth keeping.
			if len(parts) >= 2 && parts[1] == "tmp" {
				return filepath.SkipDir
			}
```

In `cmd/repodeploy/main.go`, `serve`, right after `defer store.Close()` add:

```go
	if err = store.Uploads.SweepTemp(24 * time.Hour); err != nil {
		return err
	}
```

- [ ] **Step 7: Run all tests**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/deploy cmd/repodeploy
git commit -m "Deploy uploaded archives through a source interface with retention and backups"
```

---

### Task 9: Web — upload endpoint, limits and panel upload form

**Files:**
- Modify: `internal/web/uploads.go`
- Modify: `internal/web/server.go` (`Server` fields, `New`, middleware, routes, `page`, `projectPage`)
- Modify: `internal/web/projects_admin.go` (`projectView`)
- Modify: `internal/web/templates.go`
- Modify: `cmd/repodeploy/main.go` (`--max-upload`, `parseSize`, help)
- Test: `internal/web/uploads_test.go` (append), `cmd/repodeploy/env_test.go` (append `TestParseSize`)

**Interfaces:**
- Consumes: `Store.Uploads`, `upload.Error`, `upload.DefaultLimits`, `upload.DefaultMaxBytes`, `upload.ValidHash`, `upload.Pack`; `savedUploadProject`, `activate`, `readBody` test helpers.
- Produces:
  - `Server.MaxUpload int64` (0 selects `upload.DefaultMaxBytes`)
  - `POST /api/projects/{id}/uploads` (raw archive body; `?force=1`): `202` + job, `200` + active job when identical, `400` invalid, `409` queue refusal, `413` too large, `429` busy.
  - `POST /projects/{id}/uploads` (multipart `file`, optional `force=on`): redirect to `/jobs/ID` or `/projects/ID?unchanged=1#upload`.
  - `func parseSize(s string) (int64, error)` in `cmd/repodeploy`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/web/uploads_test.go` (extend imports with `"bytes"`, `"encoding/json"`, `"mime/multipart"`, `"net/http"`, `"net/http/httptest"`, `"os"`, `"path/filepath"`, and `"github.com/vicape92/repodeploy/internal/upload"`):

```go
func archiveBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	dir := t.TempDir()
	var names []string
	for name, body := range files {
		path := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(path), 0755)
		os.WriteFile(path, []byte(body), 0644)
		names = append(names, name)
	}
	var buf bytes.Buffer
	if err := upload.Pack(dir, names, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func rawUpload(t *testing.T, target, token string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", target, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAPIUploadStoresArchiveQueuesAndDeduplicates(t *testing.T) {
	server, h, token := webFixture(t)
	p := savedUploadProject(t, server)
	target := h.URL + "/api/projects/" + p.ID + "/uploads"
	archive := archiveBytes(t, map[string]string{"Dockerfile": "FROM scratch", "version.txt": "one"})
	resp := rawUpload(t, target, token, archive)
	var job deploy.Job
	json.NewDecoder(resp.Body).Decode(&job)
	resp.Body.Close()
	if resp.StatusCode != 202 || job.Source != "upload" || !upload.ValidHash(job.Commit) {
		t.Fatalf("upload: %d %+v", resp.StatusCode, job)
	}
	if _, err := os.Stat(filepath.Join(server.Store.Root, "uploads", p.ID, job.Commit+".tar.gz")); err != nil {
		t.Fatal("archive not stored")
	}
	server.Store.Finish(job.ID, "succeeded", "", "")
	server.Store.SetActive(p.ID, job.ID)
	resp = rawUpload(t, target, token, archive)
	var same deploy.Job
	json.NewDecoder(resp.Body).Decode(&same)
	resp.Body.Close()
	if resp.StatusCode != 200 || same.ID != job.ID {
		t.Fatalf("identical upload: %d %+v", resp.StatusCode, same)
	}
	resp = rawUpload(t, target+"?force=1", token, archive)
	var forced deploy.Job
	json.NewDecoder(resp.Body).Decode(&forced)
	resp.Body.Close()
	if resp.StatusCode != 202 || forced.ID == job.ID {
		t.Fatalf("forced upload: %d %+v", resp.StatusCode, forced)
	}
}

func TestAPIUploadRejectsBadRequests(t *testing.T) {
	server, h, token := webFixture(t)
	p := savedUploadProject(t, server)
	git := savedProject(t, server)
	target := h.URL + "/api/projects/" + p.ID + "/uploads"
	archive := archiveBytes(t, map[string]string{"Dockerfile": "FROM scratch"})
	if resp := rawUpload(t, h.URL+"/api/projects/"+git.ID+"/uploads", token, archive); readBody(t, resp.Body) == "" || resp.StatusCode != 400 {
		t.Fatalf("Git project upload: %d", resp.StatusCode)
	}
	resp := rawUpload(t, target, token, []byte("not an archive"))
	if body := readBody(t, resp.Body); resp.StatusCode != 400 || !strings.Contains(body, ".zip or .tar.gz") {
		t.Fatalf("invalid archive: %d %s", resp.StatusCode, body)
	}
	server.MaxUpload = 1 << 20
	resp = rawUpload(t, target, token, bytes.Repeat([]byte("x"), (1<<20)+(512<<10)))
	if body := readBody(t, resp.Body); resp.StatusCode != 413 || !strings.Contains(body, "1 MB") {
		t.Fatalf("oversized: %d %s", resp.StatusCode, body)
	}
	server.uploadSlots <- struct{}{}
	server.uploadSlots <- struct{}{}
	resp = rawUpload(t, target, token, archive)
	if readBody(t, resp.Body); resp.StatusCode != 429 {
		t.Fatalf("busy: %d", resp.StatusCode)
	}
	<-server.uploadSlots
	<-server.uploadSlots
	if resp = rawUpload(t, target, "", archive); readBody(t, resp.Body) == "" || resp.StatusCode != 401 {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
	if left, _ := filepath.Glob(filepath.Join(server.Store.Root, "uploads", p.ID, "*")); len(left) != 0 {
		t.Fatalf("rejected uploads stored: %v", left)
	}
}

func TestBrowserUploadNeedsCSRFAndShowsTheForm(t *testing.T) {
	server, h, _ := webFixture(t)
	p := savedUploadProject(t, server)
	client, csrf := loggedBrowser(t, h.URL)
	resp, _ := client.Get(h.URL + "/projects/" + p.ID)
	if page := readBody(t, resp.Body); !strings.Contains(page, `action="/projects/`+p.ID+`/uploads"`) || !strings.Contains(page, "repodeploy up --project "+p.ID) {
		t.Fatal("upload form missing")
	}
	form := func(withCSRF bool, file []byte) (*bytes.Buffer, string) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		if withCSRF {
			mw.WriteField("_csrf", csrf)
		}
		fw, _ := mw.CreateFormFile("file", "site.tar.gz")
		fw.Write(file)
		mw.Close()
		return &body, mw.FormDataContentType()
	}
	send := func(body *bytes.Buffer, contentType string) *http.Response {
		req, _ := http.NewRequest("POST", h.URL+"/projects/"+p.ID+"/uploads", body)
		req.Header.Set("Content-Type", contentType)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	archive := archiveBytes(t, map[string]string{"Dockerfile": "FROM scratch"})
	if resp = send(form(false, archive)); readBody(t, resp.Body) == "" || resp.StatusCode != 403 {
		t.Fatalf("without CSRF: %d", resp.StatusCode)
	}
	resp = send(form(true, archive))
	readBody(t, resp.Body)
	if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/jobs/") {
		t.Fatalf("browser upload: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = client.Get(h.URL + "/projects/" + p.ID)
	if page := readBody(t, resp.Body); !strings.Contains(page, "upload ") {
		t.Fatal("history does not label uploads")
	}
	// An oversized multipart body is refused with 413 before the handler runs.
	server.MaxUpload = 1 << 20
	body, contentType := form(true, bytes.Repeat([]byte("x"), 3<<20))
	req := httptest.NewRequest("POST", "/projects/"+p.ID+"/uploads", body)
	req.Host = strings.TrimPrefix(h.URL, "http://")
	req.Header.Set("Content-Type", contentType)
	u, _ := url.Parse(h.URL)
	for _, c := range client.Jar.Cookies(u) {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("oversized browser upload: %d", rec.Code)
	}
}
```

Append to `cmd/repodeploy/env_test.go`:

```go
func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"200MB": 200 << 20, "1gb": 1 << 30, "2048KB": 2 << 20, "1048576": 1 << 20} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "abc", "-1MB", "512KB", "10TB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/web/ ./cmd/repodeploy/ -run 'Upload|ParseSize'`
Expected: FAIL — `server.MaxUpload undefined`, `server.uploadSlots undefined`, `undefined: parseSize`.

- [ ] **Step 3: Server fields, middleware and routes**

In `internal/web/server.go`:

Add to `Server` after `Deleter ProjectDeleter`:

```go
	// MaxUpload is the largest accepted upload archive; zero selects
	// upload.DefaultMaxBytes.
	MaxUpload int64
	// uploadSlots bounds concurrent upload processing.
	uploadSlots chan struct{}
```

In `New`, add `uploadSlots: make(chan struct{}, 2),` to the `&Server{...}` literal.

In the middleware replace

```go
		limit := int64(maxRequestBody)
		if strings.HasPrefix(r.URL.Path, "/hooks/github/") {
			limit = maxWebhookBody
		}
```

with

```go
		limit := int64(maxRequestBody)
		if strings.HasPrefix(r.URL.Path, "/hooks/github/") {
			limit = maxWebhookBody
		} else if r.Method == http.MethodPost && isUploadPath(r.URL.Path) {
			// Room for the archive plus multipart framing.
			limit = s.maxUpload() + maxRequestBody
		}
```

and replace

```go
			csrf := r.Header.Get("X-CSRF-Token")
			if csrf == "" {
				csrf = r.FormValue("_csrf")
			}
```

with

```go
			csrf := r.Header.Get("X-CSRF-Token")
			if csrf == "" {
				if err := r.ParseMultipartForm(32 << 20); err != nil {
					var tooLarge *http.MaxBytesError
					if errors.As(err, &tooLarge) {
						http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
						return
					}
				}
				csrf = r.FormValue("_csrf")
			}
```

Register routes after `mux.HandleFunc("POST /api/projects/{id}/deploy-changes", s.deployChanges)`:

```go
	mux.HandleFunc("POST /api/projects/{id}/uploads", s.upload)
```

and after `mux.HandleFunc("POST /projects/{id}/deploy-changes", s.deployChanges)`:

```go
	mux.HandleFunc("POST /projects/{id}/uploads", s.upload)
```

Add to `type page struct` after `EnvName string`:

```go
	MaxUpload   string
	UploadError string
```

In `projectPage`, after the `projectView` error check add:

```go
	if r.URL.Query().Get("unchanged") == "1" {
		view.Notice = "This archive matches the active release, so nothing was deployed. Tick \"Redeploy even if identical\" to deploy it again."
	}
```

In `internal/web/projects_admin.go`, change the `projectView` return to:

```go
	return page{View: "project", Project: p, Jobs: jobs, Secrets: secrets, EnvRows: rows, EnvPending: pending, MaxUpload: humanBytes(s.maxUpload())}, nil
```

- [ ] **Step 4: Implement the upload handler**

Replace `internal/web/uploads.go` with:

```go
package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/upload"
)

func (s *Server) maxUpload() int64 {
	if s.MaxUpload > 0 {
		return s.MaxUpload
	}
	return upload.DefaultMaxBytes
}

func isUploadPath(path string) bool {
	return strings.HasSuffix(path, "/uploads") && (strings.HasPrefix(path, "/api/projects/") || strings.HasPrefix(path, "/projects/"))
}

// humanBytes renders a size limit for messages, for example "200 MB".
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// newProjectPage shows the form for a new upload project. Git projects start
// from the connections page.
func (s *Server) newProjectPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("source") != "upload" {
		http.Redirect(w, r, "/connections", 303)
		return
	}
	s.render(w, r, page{View: "edit", Project: deploy.Project{Source: "upload", Dockerfile: "Dockerfile", Port: 8080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:8080/health", Retain: 5, HealthTimeout: 60}})
}

// upload stores an archive for an upload project and queues its deployment.
// The API takes the raw archive as the body; the panel sends a multipart form.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	browser := !api(r)
	fail := func(status int, message string) {
		if browser {
			s.renderUploadError(w, r, p, status, message)
		} else {
			http.Error(w, message, status)
		}
	}
	if !p.IsUpload() {
		fail(400, "this project deploys from Git; create an upload project for manual uploads")
		return
	}
	select {
	case s.uploadSlots <- struct{}{}:
		defer func() { <-s.uploadSlots }()
	default:
		w.Header().Set("Retry-After", "10")
		fail(429, "another upload is being processed; retry shortly")
		return
	}
	body, force := io.Reader(r.Body), r.URL.Query().Get("force") == "1"
	if browser {
		file, _, err := r.FormFile("file")
		if err != nil {
			fail(400, "choose a .zip or .tar.gz file to upload")
			return
		}
		defer file.Close()
		body, force = file, r.FormValue("force") == "on"
	}
	dir, err := s.Store.Uploads.TempDir()
	if err != nil {
		fail(500, "cannot store the upload")
		return
	}
	tmp, err := os.CreateTemp(dir, "upload-")
	if err != nil {
		fail(500, "cannot store the upload")
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(body, s.maxUpload()+1))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	var tooLarge *http.MaxBytesError
	if n > s.maxUpload() || errors.As(err, &tooLarge) {
		fail(413, "the archive exceeds the "+humanBytes(s.maxUpload())+" upload limit")
		return
	}
	if err != nil {
		fail(400, "the upload was interrupted")
		return
	}
	hash, err := s.Store.Uploads.Save(p.ID, tmp.Name(), upload.DefaultLimits(s.maxUpload()))
	var invalid *upload.Error
	if errors.As(err, &invalid) {
		fail(400, invalid.Message)
		return
	}
	if err != nil {
		fail(500, "cannot store the upload")
		return
	}
	if !force && p.Active != "" {
		if active, err := s.Store.Job(p.Active); err == nil && active.Commit == hash {
			if browser {
				http.Redirect(w, r, "/projects/"+p.ID+"?unchanged=1#upload", 303)
			} else {
				jsonResponse(w, 200, active)
			}
			return
		}
	}
	j, err := s.Store.Enqueue(p.ID, "upload", "", hash, "")
	respondJob(w, r, j, err)
}

func (s *Server) renderUploadError(w http.ResponseWriter, r *http.Request, p deploy.Project, status int, message string) {
	view, err := s.projectView(p)
	if err != nil {
		http.Error(w, message, status)
		return
	}
	view.UploadError = message
	s.renderStatus(w, r, status, view)
}
```

- [ ] **Step 5: Upload form template**

In `internal/web/templates.go`, replace `<section><h2>Deployment history</h2>` with:

```html
{{if eq .Project.Source "upload"}}<section id="upload"><h2>Upload a new version</h2><p class="muted">A .zip or .tar.gz of the project folder, up to {{.MaxUpload}}. The archive replaces the whole source. Keep secrets out of it: <code>.env</code> values belong in Environment variables.</p>{{if .UploadError}}<p class="notice" role="alert">{{.UploadError}}</p>{{end}}{{if .Notice}}<p class="success" role="status">{{.Notice}}</p>{{end}}<form method="post" action="/projects/{{.Project.ID}}/uploads" enctype="multipart/form-data" class="subform"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Archive<input name="file" type="file" accept=".zip,.tar.gz,.tgz,application/zip,application/gzip" required></label><label class="inline"><input type="checkbox" name="force"> Redeploy even if identical to the active release</label><div class="form-foot"><button>{{template "i-rocket"}}Upload and deploy</button><small>From a terminal: <code>repodeploy up --project {{.Project.ID}}</code></small></div></form></section>{{end}}
<section><h2>Deployment history</h2>
```

- [ ] **Step 6: `--max-upload` flag**

In `cmd/repodeploy/main.go`, add `"strconv"` to the imports. In `serve`, after the `allowedHosts` flag add:

```go
	maxUpload := fs.String("max-upload", "200MB", "largest accepted upload archive, e.g. 200MB or 1GB")
```

after `fs.Parse(args)` succeeds add:

```go
	uploadLimit, err := parseSize(*maxUpload)
	if err != nil {
		return err
	}
```

(the later `host, _, err := net.SplitHostPort(*listen)` still compiles because `host` is new). After `handler.AllowedHosts = extraHosts` add:

```go
	handler.MaxUpload = uploadLimit
```

Add at the end of the file:

```go
// parseSize reads sizes such as 200MB, 1GB, 2048KB or a plain byte count.
func parseSize(s string) (int64, error) {
	text := strings.ToUpper(strings.TrimSpace(s))
	factor := int64(1)
	for _, unit := range []struct {
		suffix string
		factor int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(text, unit.suffix) {
			text, factor = strings.TrimSpace(strings.TrimSuffix(text, unit.suffix)), unit.factor
			break
		}
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n <= 0 || n > (1<<40)/factor {
		return 0, fmt.Errorf("invalid size %q; use a value such as 200MB or 1GB", s)
	}
	if n*factor < 1<<20 {
		return 0, errors.New("--max-upload must be at least 1MB")
	}
	return n * factor, nil
}
```

In the `help` constant change the `serve` lines to:

```
  serve [--listen ADDR] [--public-url HTTPS_URL] [--trust-proxy]
        [--allowed-hosts HOST,...] [--max-upload SIZE]
```

- [ ] **Step 7: Run all tests**

Run: `go test ./...`
Expected: PASS. If an existing web test that posts an oversized urlencoded form now gets 413 instead of another status, update its expectation to 413 (the new behaviour is intentional and documented).

- [ ] **Step 8: Commit**

```bash
git add internal/web cmd/repodeploy
git commit -m "Accept project uploads from the API and panel with size and concurrency limits"
```

---

### Task 10: `repodeploy up` — file selection, packing, upload and log following

**Files:**
- Create: `internal/upload/select.go`
- Create: `cmd/repodeploy/up.go`
- Modify: `cmd/repodeploy/main.go` (`streamLogs`, `up` dispatch, help)
- Test: `internal/upload/select_test.go`, `cmd/repodeploy/up_test.go`

**Interfaces:**
- Consumes: `upload.Pack` (Task 6); API from Task 9; `cliFixture`, `cli` (Task 5).
- Produces:
  - `func SelectFiles(dir string) (files, envFiles []string, err error)` in `upload`
  - `func streamLogs(r io.Reader, out io.Writer) error`, `type apiClient`, `func up(ctx, apiClient, []string, io.Writer) error`, `func parseUpArgs([]string) (upOptions, error)` in `cmd/repodeploy`

- [ ] **Step 1: Write the failing selection tests**

Create `internal/upload/select_test.go`:

```go
package upload

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestIgnoreRules(t *testing.T) {
	rules := parseIgnore("# comment\nnode_modules/\n*.log\n!keep.log\n/build\ndocs/**/*.md\n")
	for rel, want := range map[string]bool{
		"node_modules/a/b.js": true,
		"src/node_modules/x":  true,
		"node_modules":        false,
		"app.log":             true,
		"logs/x.log":          true,
		"keep.log":            false,
		"build/out.js":        true,
		"src/build/out.js":    false,
		"docs/a/b/c.md":       true,
		"docs/c.md":           true,
		"docs/c.txt":          false,
		"README.md":           false,
	} {
		if got := rules.ignored(rel); got != want {
			t.Errorf("%s: got %v want %v", rel, got, want)
		}
	}
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(path), 0755)
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSelectFilesInGitRepository(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.org", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.org")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	writeTree(t, dir, map[string]string{
		".gitignore":             "dist/\n",
		"Dockerfile":             "FROM scratch",
		"src/app.js":             "x",
		"dist/bundle.js":         "ignored by git",
		".env":                   "SECRET=1",
		"config/.env.production": "SECRET=2",
		"notes.tmp":              "ignored by .repodeployignore",
		".repodeployignore":      "*.tmp\n",
		".repodeploy/project":    "abc",
	})
	git("add", "Dockerfile", ".gitignore")
	git("commit", "-qm", "init")
	files, envFiles, err := SelectFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".gitignore", ".repodeployignore", "Dockerfile", "src/app.js"}; !reflect.DeepEqual(files, want) {
		t.Fatalf("files %v", files)
	}
	if want := []string{".env", "config/.env.production"}; !reflect.DeepEqual(envFiles, want) {
		t.Fatalf("env files %v", envFiles)
	}
}

func TestSelectFilesWithoutGit(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".git/HEAD": "not a repository", "index.html": "x", "sub/a.txt": "y"})
	files, envFiles, err := SelectFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"index.html", "sub/a.txt"}; !reflect.DeepEqual(files, want) || len(envFiles) != 0 {
		t.Fatalf("files %v env %v", files, envFiles)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/upload/ -run 'Ignore|SelectFiles'`
Expected: FAIL — `undefined: parseIgnore`, `undefined: SelectFiles`.

- [ ] **Step 3: Implement selection**

Create `internal/upload/select.go`:

```go
package upload

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SelectFiles lists the files "repodeploy up" sends from dir as sorted,
// slash-separated paths. Inside a Git work tree it uses git ls-files, which
// honours .gitignore; elsewhere it walks the tree. .repodeployignore applies
// in both cases. .git/, .repodeploy/ and .env files are never sent; skipped
// .env files are returned separately so the caller can suggest env import.
func SelectFiles(dir string) (files, envFiles []string, err error) {
	candidates, err := gitFiles(dir)
	if err != nil {
		if candidates, err = walkFiles(dir); err != nil {
			return nil, nil, err
		}
	}
	rules, err := loadIgnore(filepath.Join(dir, ".repodeployignore"))
	if err != nil {
		return nil, nil, err
	}
	for _, rel := range candidates {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || info.IsDir() {
			continue
		}
		if top := strings.SplitN(rel, "/", 2)[0]; top == ".git" || top == ".repodeploy" {
			continue
		}
		if base := path.Base(rel); base == ".env" || strings.HasPrefix(base, ".env.") {
			envFiles = append(envFiles, rel)
			continue
		}
		if rules.ignored(rel) {
			continue
		}
		files = append(files, rel)
	}
	sort.Strings(files)
	sort.Strings(envFiles)
	return files, envFiles, nil
}

func gitFiles(dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" && !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	return files, nil
}

func walkFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// ignoreRule is one .repodeployignore line, a subset of gitignore syntax.
type ignoreRule struct {
	pattern  []string
	negate   bool
	dirOnly  bool
	anchored bool
}

type ignoreRules []ignoreRule

func loadIgnore(file string) (ignoreRules, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseIgnore(string(b)), nil
}

// parseIgnore understands # comments, globs (*, ?, [..], **), a trailing /
// for directories, a leading / (or any inner /) to anchor at the root, and !
// to re-include.
func parseIgnore(text string) ignoreRules {
	var rules ignoreRules
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var r ignoreRule
		if strings.HasPrefix(line, "!") {
			r.negate, line = true, line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly, line = true, strings.TrimSuffix(line, "/")
		}
		if strings.HasPrefix(line, "/") {
			r.anchored, line = true, line[1:]
		} else if strings.Contains(line, "/") {
			r.anchored = true
		}
		if line == "" {
			continue
		}
		r.pattern = strings.Split(line, "/")
		rules = append(rules, r)
	}
	return rules
}

// ignored reports whether the file rel is excluded; the last matching rule
// wins.
func (rules ignoreRules) ignored(rel string) bool {
	parts := strings.Split(rel, "/")
	ignored := false
	for _, r := range rules {
		if r.matches(parts) {
			ignored = !r.negate
		}
	}
	return ignored
}

// matches tests the rule against every parent directory of the file and,
// unless the rule names directories only, the file itself.
func (r ignoreRule) matches(parts []string) bool {
	for end := 1; end <= len(parts); end++ {
		if end == len(parts) && r.dirOnly {
			return false
		}
		prefix := parts[:end]
		if r.anchored {
			if matchSegments(r.pattern, prefix) {
				return true
			}
		} else if ok, _ := path.Match(r.pattern[0], prefix[end-1]); ok {
			return true
		}
	}
	return false
}

func matchSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchSegments(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, _ := path.Match(pattern[0], name[0])
	return ok && matchSegments(pattern[1:], name[1:])
}
```

- [ ] **Step 4: Run the selection tests**

Run: `go test ./internal/upload/ -v`
Expected: PASS.

- [ ] **Step 5: Write the failing CLI tests**

Create `cmd/repodeploy/up_test.go`:

```go
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
)

func TestParseUpArgs(t *testing.T) {
	got, err := parseUpArgs([]string{"site", "--project=abc", "--force", "--detach"})
	if err != nil || !reflect.DeepEqual(got, upOptions{project: "abc", dir: "site", force: true, detach: true}) {
		t.Fatalf("%+v %v", got, err)
	}
	if got, _ = parseUpArgs([]string{"--project", "abc"}); got.dir != "." || got.project != "abc" {
		t.Fatalf("defaults: %+v", got)
	}
	for _, bad := range [][]string{{"--project"}, {"--unknown"}, {"a", "b"}} {
		if _, err := parseUpArgs(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestUpUploadsDirectoryAndRemembersProject(t *testing.T) {
	root, s, h := cliFixture(t)
	p, err := s.SaveProject(deploy.Project{Name: "site", Source: "upload", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch"), 0644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0644)
	out, err := cli(t, root, h.URL, "", "up", "--project", p.ID, "--detach", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Skipped .env") || !strings.Contains(out, "queued") {
		t.Fatalf("output: %s", out)
	}
	if saved, _ := os.ReadFile(filepath.Join(dir, ".repodeploy", "project")); strings.TrimSpace(string(saved)) != p.ID {
		t.Fatalf("project ID not saved: %q", saved)
	}
	jobs, _ := s.Jobs(p.ID)
	if len(jobs) != 1 || jobs[0].Source != "upload" {
		t.Fatalf("jobs: %+v", jobs)
	}
	dest := filepath.Join(t.TempDir(), "src")
	if err := s.Uploads.Prepare(p.ID, jobs[0].Commit, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".env")); !os.IsNotExist(err) {
		t.Fatal(".env was uploaded")
	}
	if _, err := cli(t, root, h.URL, "", "up", "--detach", dir); err != nil {
		t.Fatalf("saved project ID not used: %v", err)
	}
	if jobs, _ = s.Jobs(p.ID); len(jobs) != 2 {
		t.Fatalf("second upload not queued: %d", len(jobs))
	}
}

func TestUpFollowsLogsAndFailsWhenTheDeploymentFails(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "admin.token"), []byte("secret-token"), 0600)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/uploads"):
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			w.Write([]byte(`{"id":"job1","phase":"queued"}`))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"log\":\"Building\\n\"}\n\nevent: done\ndata: {}\n\n"))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"job1","phase":"failed","error":"Docker build failed"}`))
		}
	}))
	defer h.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch"), 0644)
	out, err := cli(t, root, h.URL, "", "up", "--project", "p1", dir)
	if err == nil || !strings.Contains(err.Error(), "Docker build failed") || !strings.Contains(out, "Building") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./cmd/repodeploy/ -run 'Up'`
Expected: FAIL — `undefined: parseUpArgs`, `undefined: upOptions`.

- [ ] **Step 7: Implement `up`**

Create `cmd/repodeploy/up.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/upload"
)

// projectFile remembers the upload project of a directory.
const projectFile = ".repodeploy/project"

type upOptions struct {
	project string
	dir     string
	force   bool
	detach  bool
}

func parseUpArgs(args []string) (upOptions, error) {
	o := upOptions{dir: "."}
	dirSet := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--force":
			o.force = true
		case a == "--detach":
			o.detach = true
		case a == "--project":
			if i+1 >= len(args) {
				return o, errors.New("--project needs a project ID")
			}
			i++
			o.project = args[i]
		case strings.HasPrefix(a, "--project="):
			o.project = strings.TrimPrefix(a, "--project=")
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown up option %q", a)
		case dirSet:
			return o, errors.New("up accepts a single directory")
		default:
			o.dir, dirSet = a, true
		}
	}
	return o, nil
}

// apiClient sends authenticated requests to the daemon.
type apiClient struct {
	base  string
	token string
	http  *http.Client
}

func (c apiClient) do(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.base, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (c apiClient) job(ctx context.Context, id string) (deploy.Job, error) {
	var j deploy.Job
	resp, err := c.do(ctx, http.MethodGet, "/api/jobs/"+url.PathEscape(id), "", nil)
	if err != nil {
		return j, err
	}
	defer resp.Body.Close()
	return j, json.NewDecoder(resp.Body).Decode(&j)
}

// up packs a directory, uploads it to an upload project and follows the
// deployment log unless --detach is given.
func up(ctx context.Context, c apiClient, args []string, out io.Writer) error {
	o, err := parseUpArgs(args)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(o.dir)
	if err != nil {
		return err
	}
	remembered := false
	if o.project == "" {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(projectFile)))
		if err != nil {
			return errors.New("pass --project ID; after the first upload the ID is saved in .repodeploy/project")
		}
		o.project, remembered = strings.TrimSpace(string(b)), true
	}
	files, envFiles, err := upload.SelectFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no files to upload in %s", dir)
	}
	archive, err := os.CreateTemp("", "repodeploy-up-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if err = upload.Pack(dir, files, archive); err != nil {
		return err
	}
	size, err := archive.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = archive.Seek(0, io.SeekStart)
	}
	if err != nil {
		return err
	}
	if len(envFiles) > 0 {
		fmt.Fprintf(out, "Skipped %s; store their values with: repodeploy env import %s FILE\n", strings.Join(envFiles, ", "), o.project)
	}
	fmt.Fprintf(out, "Uploading %d files (%s)\n", len(files), formatSize(size))
	path := "/api/projects/" + url.PathEscape(o.project) + "/uploads"
	if o.force {
		path += "?force=1"
	}
	resp, err := c.do(ctx, http.MethodPost, path, "application/octet-stream", archive)
	if err != nil {
		return err
	}
	var j deploy.Job
	err = json.NewDecoder(resp.Body).Decode(&j)
	status := resp.StatusCode
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("unexpected daemon response: %w", err)
	}
	if !remembered {
		err = os.MkdirAll(filepath.Join(dir, ".repodeploy"), 0755)
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, filepath.FromSlash(projectFile)), []byte(o.project+"\n"), 0644)
		}
		if err != nil {
			fmt.Fprintf(out, "Warning: cannot save the project ID: %v\n", err)
		}
	}
	if status == http.StatusOK {
		_, err = fmt.Fprintf(out, "This version is already active (deployment %s); use --force to deploy it again\n", j.ID)
		return err
	}
	fmt.Fprintf(out, "Deployment %s queued\n", j.ID)
	if o.detach {
		return nil
	}
	resp, err = c.do(ctx, http.MethodGet, "/api/jobs/"+url.PathEscape(j.ID)+"/logs?follow=1", "", nil)
	if err != nil {
		return err
	}
	err = streamLogs(resp.Body, out)
	resp.Body.Close()
	if err != nil {
		return err
	}
	if j, err = c.job(ctx, j.ID); err != nil {
		return err
	}
	if j.Phase != "succeeded" {
		return fmt.Errorf("deployment %s %s: %s", j.ID, j.Phase, j.Error)
	}
	return nil
}

func formatSize(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
}
```

- [ ] **Step 8: Wire `up` and share log streaming**

In `cmd/repodeploy/main.go`:

Add at the end of the file:

```go
// streamLogs prints the log text of a server-sent event stream.
func streamLogs(r io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Log string `json:"log"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			return err
		}
		if _, err := io.WriteString(out, event.Log); err != nil {
			return err
		}
	}
	return scanner.Err()
}
```

Replace the inline follow block in `run`:

```go
	if follow {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				var event struct {
					Log string `json:"log"`
				}
				if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					return err
				}
				if _, err = io.WriteString(out, event.Log); err != nil {
					return err
				}
			}
		}
		return scanner.Err()
	}
```

with

```go
	if follow {
		return streamLogs(resp.Body, out)
	}
```

Right after the token is read (after the `token, err := readToken(*tokenFile)` error check) add:

```go
	if args[0] == "up" {
		client := apiClient{base: *apiURL, token: strings.TrimSpace(string(token)), http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
		return up(ctx, client, args[1:], out)
	}
```

In the `help` constant, insert after the `env import` line:

```
  up [--project ID] [--force] [--detach] [DIR]
                                   Upload DIR (default .) to an upload project and deploy
```

- [ ] **Step 9: Run all tests**

Run: `go test ./...`
Expected: PASS, including the existing `TestCLIFollowLogsPrintsDecodedLogEvents`.

- [ ] **Step 10: Commit**

```bash
git add internal/upload cmd/repodeploy
git commit -m "Add repodeploy up: select, pack and upload a folder, then follow the deployment"
```

---

### Task 11: Docker integration, documentation and final verification

**Files:**
- Create: `internal/deploy/env_upload_integration_test.go`
- Modify: `README.md`, `docs/operations.md`, `docs/spec.md`, `docs/progress.md`

**Interfaces:**
- Consumes: everything above; `stepOnce` (Task 3).
- Produces: documentation and verified build.

- [ ] **Step 1: Write the Docker integration test**

Create `internal/deploy/env_upload_integration_test.go`:

```go
package deploy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vicape92/repodeploy/internal/envvars"
	"github.com/vicape92/repodeploy/internal/upload"
)

func TestDockerIntegrationEnvironmentAndUpload(t *testing.T) {
	if os.Getenv("REPODEPLOY_DOCKER_TEST") != "1" {
		t.Skip("set REPODEPLOY_DOCKER_TEST=1 to run isolated Docker integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if _, err := dockerOutput(ctx, "info"); err != nil {
		t.Fatal("Docker integration requested but Docker engine is unavailable:", err)
	}
	for _, mode := range []string{"dockerfile", "compose"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			s := testStore(t)
			p := Project{Name: "env-" + mode, Source: "upload", HealthURL: fmt.Sprintf("http://127.0.0.1:%d/health", port), HealthTimeout: 20, Retain: 5}
			src := t.TempDir()
			os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM busybox:1.37.0\nARG BUILD_LABEL=none\nRUN echo \"$BUILD_LABEL\" > /label\nCOPY app.sh /app.sh\nRUN chmod 755 /app.sh\nENTRYPOINT [\"/app.sh\"]\n"), 0644)
			os.WriteFile(filepath.Join(src, "app.sh"), []byte("#!/bin/sh\nset -eu\nmkdir -p /www\nprintf '%s|%s' \"$GREETING\" \"$(cat /label)\" > /www/index.html\nprintf ok > /www/health\nexec httpd -f -p 8080 -h /www\n"), 0755)
			files := []string{"Dockerfile", "app.sh"}
			if mode == "dockerfile" {
				p.Dockerfile, p.Port, p.ContainerPort = "Dockerfile", port, 8080
			} else {
				p.ComposeFile = "compose.yaml"
				os.WriteFile(filepath.Join(src, "compose.yaml"), []byte(fmt.Sprintf("services:\n  app:\n    build:\n      context: .\n      args:\n        BUILD_LABEL: ${BUILD_LABEL}\n    ports:\n      - \"127.0.0.1:%d:8080\"\n    environment:\n      GREETING: ${GREETING}\n    stop_grace_period: 2s\n", port)), 0644)
				files = append(files, "compose.yaml")
			}
			p, err = s.SaveProject(p)
			if err != nil {
				t.Fatal(err)
			}
			e := &Engine{Store: s, Runtime: Docker{Store: s}}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				e.DeleteProject(cleanup, p.ID, true)
			})
			deployUpload := func() Job {
				t.Helper()
				archive := filepath.Join(t.TempDir(), "src.tar.gz")
				f, _ := os.Create(archive)
				if err := upload.Pack(src, files, f); err != nil {
					t.Fatal(err)
				}
				f.Close()
				hash, err := s.Uploads.Save(p.ID, archive, upload.DefaultLimits(upload.DefaultMaxBytes))
				if err != nil {
					t.Fatal(err)
				}
				j, err := s.Enqueue(p.ID, "upload", "", hash, "")
				if err != nil {
					t.Fatal(err)
				}
				return stepOnce(t, e, j)
			}
			get := func() string {
				t.Helper()
				resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return strings.TrimSpace(string(b))
			}
			build := true
			s.Env.Set(p.ID, []envvars.Update{{Name: "GREETING", Value: "hello $world"}, {Name: "BUILD_LABEL", Value: "v1", Build: &build}})
			first := deployUpload()
			if first.Phase != "succeeded" {
				logs, _ := s.Logs(first.ID)
				t.Fatalf("first: %+v\n%s", first, logs)
			}
			if got := get(); got != "hello $world|v1" {
				t.Fatalf("first response %q", got)
			}
			s.Env.Set(p.ID, []envvars.Update{{Name: "GREETING", Value: "second"}})
			change, err := s.EnqueueConfigChange(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if change = stepOnce(t, e, change); change.Phase != "succeeded" || get() != "second|v1" {
				logs, _ := s.Logs(change.ID)
				t.Fatalf("deploy changes: %+v\n%s", change, logs)
			}
			rollback, _ := s.Enqueue(p.ID, "manual", "", "", first.ID)
			if rollback = stepOnce(t, e, rollback); rollback.Phase != "succeeded" || get() != "hello $world|v1" {
				t.Fatalf("rollback: %+v %q", rollback, get())
			}
			os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM busybox:1.37.0\nRUN exit 1\n"), 0644)
			if broken := deployUpload(); broken.Phase != "failed" || get() != "hello $world|v1" {
				t.Fatalf("broken upload: %+v %q", broken, get())
			}
		})
	}
}
```

- [ ] **Step 2: Run the integration test where Docker is available**

Run: `REPODEPLOY_DOCKER_TEST=1 go test ./internal/deploy/ -run TestDockerIntegrationEnvironmentAndUpload -v -timeout 10m`
Expected: PASS for both subtests. Without Docker the test reports SKIP; record which in `docs/progress.md`.

- [ ] **Step 3: README — manual upload**

In `README.md`, insert before the line `Details: [browser and SSH connections](docs/installation.md#browser-and-ssh-connections).`:

~~~markdown
### Manual upload (no Git)

A project can deploy code you upload instead of a Git repository: useful for
prototypes, code that is not on a Git host, or a quick test.

1. On the dashboard choose **Upload a project**, give it a name and the same
   build and health settings as any project, and save.
2. Upload a `.zip` or `.tar.gz` of the project folder from the project page, or
   run this from the folder:

   ```sh
   repodeploy up --project PROJECT_ID
   ```

   The first upload saves the ID in `.repodeploy/project`, so later runs need
   only `repodeploy up`. The CLI follows the deployment log; `--detach` returns
   immediately and `--force` redeploys an archive identical to the active
   release.

What is sent: inside a Git work tree, the files `git ls-files` reports (tracked
files and untracked files not ignored by `.gitignore`); elsewhere every file
except `.git/`. Patterns in `.repodeployignore` (gitignore subset: `*`, `?`,
`**`, `dir/`, `/anchored`, `!negation`) are excluded too. `.env` and `.env.*`
files are never sent; store their values as
[environment variables](#environment-variables).

Upload projects have no polling, webhooks or CI/CD gate. Each archive is
validated (no absolute paths, `..`, links leaving the archive, devices or hard
links), limited to 200 MB by default (`serve --max-upload`) and stored under
its SHA-256, so history, rollback and **Deploy changes** work as for Git
projects. If the archive holds a single top-level folder, its contents become
the project root.

~~~

- [ ] **Step 4: README — environment variables**

In `README.md`, insert immediately before `## 13. CLI reference`:

~~~markdown
### Environment variables

Values such as `DATABASE_URL` are stored per project on this host
(`STATE/secrets/<project>.env.json`, mode 0600) and never belong in Git.

- **Dockerfile mode:** every variable reaches the container environment.
- **Compose mode:** reference them as `${NAME}` or `${NAME:-default}` in a
  service's `environment:` values or `build.args:`. Interpolation anywhere else
  is still rejected, so variables cannot change images, ports or volumes. An
  undefined variable without a default fails the deployment before the build.
- **Build time:** a variable reaches the image build only when it is marked
  *available at build* (`--build`). Build arguments are stored in the image
  history, so mark only values that are safe to embed, such as
  `NEXT_PUBLIC_*`.

Saving variables restarts nothing. When the active release was built with
other values, the project page shows **Deploy changes**, which rebuilds the
active commit or upload with the current values. Each release keeps the values
it was built with, so restoring an old release restores its variables too.
Values are redacted from deployment logs.

```sh
repodeploy env set PROJECT_ID DATABASE_URL < db-url.txt
printf '%s' "$TOKEN" | repodeploy env set PROJECT_ID API_TOKEN
printf '%s' 'https://api.example.org' | repodeploy env set PROJECT_ID NEXT_PUBLIC_API_URL --build
repodeploy env import PROJECT_ID .env
repodeploy env list PROJECT_ID          # names only; --reveal shows values
repodeploy env unset PROJECT_ID API_TOKEN
repodeploy deploy-changes PROJECT_ID
```

~~~

- [ ] **Step 5: README — Compose rules, CLI, configuration and API tables**

In `README.md`:

1. Replace `- **Not allowed:** includes, \`extends\`, variable interpolation, env files,` with `- **Not allowed:** includes, \`extends\`, env files,` and replace

```
- Environment values must be explicit. Write a literal dollar sign as `$$`; any
  other `$` is rejected as interpolation.
```

with

```
- Environment values must be explicit or reference project variables as
  `${NAME}` / `${NAME:-default}` (also allowed in `build.args`). Write a literal
  dollar sign as `$$`; any other `$` is rejected as interpolation.
```

2. In the CLI table, add after the `deploy ID` row:

```
| `deploy-changes ID` | Redeploy the active release with the current variables |
| `env list ID [--reveal]` | List variables; values only with `--reveal` |
| `env set ID NAME [--build\|--no-build]` | Store a variable read from standard input |
| `env unset ID NAME` | Delete a variable |
| `env import ID FILE\|-` | Store every variable of a `.env` file |
| `up [--project ID] [--force] [--detach] [DIR]` | Upload a folder to an upload project and deploy it |
```

change the `serve` row to `serve [--listen ADDR] [--public-url URL] [--trust-proxy] [--allowed-hosts H,...] [--max-upload SIZE]`, and add to the `serve` flags table:

```
| `--max-upload` | Largest accepted upload archive, e.g. `200MB` (default) or `1GB` |
```

3. In section 14, replace `Every project needs \`name\`, \`repo\`, \`branch\` and \`mode\`, plus the fields of one` with `Git projects need \`name\`, \`repo\`, \`branch\` and \`mode\`; upload projects need \`name\` and \`"source": "upload"\`. Both need the fields of one`, and add as the first table row after the header:

```
| `source` | `git` or `upload`; cannot change after creation | `git` |
```

4. In the HTTP API table, add after the `DELETE /api/projects/{id}/secrets/{name}` row:

```
| `GET /api/projects/{id}/env[?reveal=1]` | Variable names, build flags and pending state; values only with `reveal=1` |
| `PUT /api/projects/{id}/env` | Upsert variables: `[{"name": "…", "value": "…", "build": true}]` |
| `DELETE /api/projects/{id}/env/{name}` | Delete a variable |
| `POST /api/projects/{id}/deploy-changes` | Redeploy the active release with current variables (`202`; `409` when nothing is deployed) |
| `POST /api/projects/{id}/uploads[?force=1]` | Upload an archive (raw body, `application/octet-stream`): `202` queued, `200` already active, `413` too large |
```

- [ ] **Step 6: Operations and spec**

In `docs/operations.md`:

1. Replace `\`configs\`. No includes, extends, variable interpolation, env files, profiles, remote build` with `\`configs\`. No includes, extends, env files, profiles, remote build`, and replace `any other \`$\` is rejected as interpolation.` with:

```
project variables are interpolated only as `${NAME}` or `${NAME:-default}` inside
`environment` values and `build.args` (inserted values keep `$` literal); any
other `$` is rejected as interpolation.
```

2. Replace `administrator credentials, \`secrets/\`, \`releases/JOB_ID/\` and \`logs/JOB_ID.log\`.` with:

```
administrator credentials, `secrets/` (including `<project>.env.json` variable
files), `releases/JOB_ID/`, `logs/JOB_ID.log` and `uploads/<project>/<sha256>.tar.gz`
(normalised upload archives; `uploads/tmp/` holds in-flight files, swept after
24 hours). An upload archive is kept while any retained release references it.
`serve --max-upload` (default 200MB) caps archives; extraction is capped at five
times that size and 50 000 entries. Backups include `uploads/` except `tmp/`.
```

In `docs/spec.md`, append:

```markdown
## Environment variables and manual upload

Approved 2026-11-08; full design in
`docs/superpowers/specs/2026-11-08-env-vars-and-upload-design.md`.

- Modular monolith: `internal/envvars` and `internal/upload` own their data and
  import only the standard library; `deploy` uses them through `Store.Env`,
  `Store.Uploads` and the `SourcePreparer` interface.
- Per-project variables live outside Git, reach Dockerfile services directly
  and Compose services through `${NAME}`/`${NAME:-default}` in `environment`
  and `build.args` only. Build-time exposure is opt-in per variable. Each
  release records a digest; "deploy changes" rebuilds the active source;
  rollback restores the release's values.
- Upload projects (`source: upload`) deploy validated, normalised,
  SHA-256-addressed archives from the panel or `repodeploy up`; they have no
  polling, webhooks or CI. No schema change.
```

- [ ] **Step 7: Full verification**

Run each command and confirm the expected result:

```bash
gofmt -l cmd internal                       # expected: no output
go vet ./...                                # expected: no output
go mod verify                               # expected: all modules verified
go test ./... -count=1                      # expected: ok for every package
go test -race ./... -count=1                # expected: ok for every package
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/repodeploy
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/repodeploy
sh -n scripts/install.sh                    # expected: no output
```

If Docker is available, also run `REPODEPLOY_DOCKER_TEST=1 go test ./internal/deploy/ -count=1 -timeout 15m`.

- [ ] **Step 8: Record the outcome**

Append to `docs/progress.md`:

```markdown
## Environment variables and manual upload — 2026-11-08

- Modular monolith: new `internal/envvars` and `internal/upload` modules
  (stdlib only); engine selects sources through `SourcePreparer`.
- Variables: storage, `.env` import, restricted Compose substitution,
  build-time opt-in, digest per release, deploy changes, rollback restores
  values, log redaction; API, panel and CLI.
- Uploads: validated and normalised archives, retention with a one-hour grace,
  backups, panel form, `POST /api/projects/{id}/uploads`, `repodeploy up`.
- Verification: <paste the results of Task 11 Step 7, and whether the Docker
  integration ran or was skipped>.
```

Replace the placeholder line with the actual command results before committing.

- [ ] **Step 9: Commit**

```bash
git add internal/deploy/env_upload_integration_test.go README.md docs/operations.md docs/spec.md docs/progress.md
git commit -m "Document environment variables and manual upload; add Docker integration test"
```
