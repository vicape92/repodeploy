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
// in both cases. .git/, .repodeploy/, .env files and .envrc are never sent; skipped
// env files are returned separately so the caller can suggest env import.
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
		if base := path.Base(rel); base == ".env" || base == ".envrc" || strings.HasPrefix(base, ".env.") {
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
