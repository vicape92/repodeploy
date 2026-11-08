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
	os.WriteFile(filepath.Join(dir, "p1.env.json.tmp-123"), []byte(`[{"name":"X","value":"stray-secret"}]`), 0600)
	if got := s.Values(); len(got) != 2 {
		t.Fatalf("temp file values leaked: %v", got)
	}
	if !IsStoreFile("p1.env.json.tmp-123") {
		t.Fatal("temp files must be recognised as store files")
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
