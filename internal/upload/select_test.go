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
