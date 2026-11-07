package scm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestSSHAssistantGeneratesPrivateKeyAndTrustsOfficialHostSources(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, _ := ssh.NewPublicKey(public)
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))
	for _, provider := range []string{"github", "bitbucket"} {
		t.Run(provider, func(t *testing.T) {
			m := testManager(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/meta":
					json.NewEncoder(w).Encode(map[string]any{"ssh_keys": []string{authorized}})
				case "/site/ssh":
					fmt.Fprintln(w, "bitbucket.org "+authorized)
				default:
					t.Error("unexpected host key source:", r.URL)
					http.NotFound(w, r)
				}
			})
			repo := "git@github.com:team/app.git"
			host := "github.com"
			if provider == "bitbucket" {
				repo = "git@bitbucket.org:team/app.git"
				host = "bitbucket.org"
			}
			key, err := m.NewSSH(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			if key.ID == "" || !strings.HasPrefix(key.PublicKey, "ssh-ed25519 ") || key.Repo != repo {
				t.Fatalf("key result: %+v", key)
			}
			private, err := os.ReadFile(key.PrivatePath)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := ssh.ParsePrivateKey(private)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Fields(key.PublicKey)[1] != strings.Fields(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))[1] {
				t.Fatal("public and private keys do not match")
			}
			for _, path := range []string{key.PrivatePath, key.KnownHostsPath} {
				stat, err := os.Stat(path)
				if err != nil || stat.Mode().Perm() != 0600 {
					t.Fatalf("permissions: %v %v", stat, err)
				}
			}
			known, _ := os.ReadFile(key.KnownHostsPath)
			if !strings.Contains(string(known), host+" "+authorized) {
				t.Fatalf("host trust: %s", known)
			}
			reopened := New(m.root)
			loaded, err := reopened.SSHKey(key.ID)
			if err != nil || loaded.PublicKey != key.PublicKey {
				t.Fatalf("SSH restart persistence: %+v %v", loaded, err)
			}
			visible, _ := json.Marshal(key)
			if strings.Contains(string(visible), "PRIVATE KEY") {
				t.Fatal("private key exposed")
			}
		})
	}
}

func TestSSHAssistantRejectsUntrustedHostsPathsAndHostKeyRedirects(t *testing.T) {
	calls := 0
	m := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "https://attacker.invalid/keys", 302)
	})
	if _, err := m.NewSSH(context.Background(), "git@attacker.invalid:team/app.git"); err == nil || calls != 0 {
		t.Fatal("untrusted SSH provider accepted")
	}
	if _, err := m.NewSSH(context.Background(), "git@github.com:team/app.git"); err == nil || calls != 1 {
		t.Fatal("host key redirect accepted")
	}
	if _, err := m.SSHKey("../../admin"); err == nil {
		t.Fatal("key traversal accepted")
	}
}

func TestBitbucketOfficialHostKeySourceMayAlsoContainSSHDomain(t *testing.T) {
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(public)
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	m := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "bitbucket.org "+authorized)
		fmt.Fprintln(w, "ssh.bitbucket.org "+authorized)
	})
	generated, err := m.NewSSH(context.Background(), "git@bitbucket.org:team/app.git")
	if err != nil {
		t.Fatal(err)
	}
	known, _ := os.ReadFile(generated.KnownHostsPath)
	if !strings.HasPrefix(string(known), "bitbucket.org ") || strings.Contains(string(known), "ssh.bitbucket.org") {
		t.Fatalf("unexpected host trust scope: %s", known)
	}
}
