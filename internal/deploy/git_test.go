package deploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"repodeploy/internal/scm"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func sourceRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-b", "main")
	os.WriteFile(filepath.Join(dir, "version.txt"), []byte("first"), 0600)
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "first")
	return dir, gitTest(t, dir, "rev-parse", "HEAD")
}

func TestGitPreparesPinnedCommitInFreshDirectory(t *testing.T) {
	repo, first := sourceRepo(t)
	g := Git{}
	p := Project{Repo: repo, Branch: "main"}
	sha, err := g.Resolve(context.Background(), p)
	if err != nil || sha != first {
		t.Fatalf("resolve %s %v", sha, err)
	}
	os.WriteFile(filepath.Join(repo, "version.txt"), []byte("second"), 0600)
	gitTest(t, repo, "commit", "-am", "second")
	dest := filepath.Join(t.TempDir(), "checkout")
	if err := g.Prepare(context.Background(), p, sha, dest); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dest, "version.txt"))
	if string(b) != "first" {
		t.Fatalf("wrong commit: %s", b)
	}
	os.WriteFile(filepath.Join(dest, "leftover"), []byte("dirty"), 0600)
	if err := g.Prepare(context.Background(), p, sha, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "leftover")); !os.IsNotExist(err) {
		t.Fatal("reused dirty checkout")
	}
	other := filepath.Join(t.TempDir(), "checkout")
	if err := g.Prepare(context.Background(), p, sha, other); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dest, "version.txt"), []byte("changed"), 0600)
	b, _ = os.ReadFile(filepath.Join(other, "version.txt"))
	if string(b) != "first" {
		t.Fatal("checkouts share mutable files")
	}
}

func TestGitFailsSafelyForMissingBranchOrCommit(t *testing.T) {
	repo, _ := sourceRepo(t)
	g := Git{}
	p := Project{Repo: repo, Branch: "absent"}
	if _, err := g.Resolve(context.Background(), p); err == nil {
		t.Fatal("missing branch accepted")
	}
	p.Branch = "main"
	if err := g.Prepare(context.Background(), p, strings.Repeat("a", 40), filepath.Join(t.TempDir(), "checkout")); err == nil {
		t.Fatal("missing commit accepted")
	}
	p.Repo = "https://user:supersecret@127.0.0.1:1/private.git"
	_, err := g.Resolve(context.Background(), p)
	if err == nil || strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("credential disclosure: %v", err)
	}
}

func TestGitRejectsExecutableTransportsAndMalformedRefs(t *testing.T) {
	g := Git{}
	for _, p := range []Project{{Repo: "ext::sh -c evil", Branch: "main"}, {Repo: "https://example.org/app", Branch: "--upload-pack=evil"}, {Repo: "https://example.org/app", Branch: "main\nother"}} {
		if _, err := g.Resolve(context.Background(), p); err == nil {
			t.Fatalf("accepted unsafe input: %v", p)
		}
	}
}

func TestGitCredentialsAreScopedToOneURLAndNeverPersisted(t *testing.T) {
	repo, _ := sourceRepo(t)
	p := Project{Repo: "https://github.com/team/app.git", Branch: "main"}
	credential := scm.Credential{Username: "x-access-token", Token: "private-token"}
	b, err := gitRunCredential(context.Background(), p, repo, credential, "config", "--get-urlmatch", "http.extraHeader", p.Repo)
	if err != nil || strings.TrimSpace(string(b)) != "Authorization: Basic eC1hY2Nlc3MtdG9rZW46cHJpdmF0ZS10b2tlbg==" {
		t.Fatalf("scoped credential not consumed by Git: %q %v", b, err)
	}
	if _, err = gitRunCredential(context.Background(), p, repo, credential, "config", "--get-urlmatch", "http.extraHeader", "https://attacker.invalid/team/app.git"); err == nil {
		t.Fatal("authorization header available to another host")
	}
	config, _ := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if strings.Contains(string(config), "private-token") || strings.Contains(string(config), "Authorization") {
		t.Fatal("credential persisted in checkout")
	}
	value, err := gitRunCredential(context.Background(), p, repo, credential, "config", "--get", "http.followRedirects")
	if err != nil || strings.TrimSpace(string(value)) != "false" {
		t.Fatalf("Git credential redirect enabled: %s %v", value, err)
	}
}

func TestGitListsBranchesForSSHAssistant(t *testing.T) {
	repo, _ := sourceRepo(t)
	gitTest(t, repo, "branch", "release/one")
	branches, err := (Git{}).Branches(context.Background(), Project{Repo: repo, Branch: "main"})
	if err != nil || len(branches) != 2 || branches[0] != "main" || branches[1] != "release/one" {
		t.Fatalf("branches: %v %v", branches, err)
	}
}

func TestProjectRejectsEmbeddedSSHPasswordsBeforePersistence(t *testing.T) {
	p := Project{Name: "app", Repo: "ssh://git:private-password@github.com/team/app.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", Port: 18082, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18082/health"}
	if err := ValidateProject(p); err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatalf("unsafe SSH credentials validation: %v", err)
	}
}
