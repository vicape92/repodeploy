package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSSHIntegrationReadOnlyKeyAndStrictHostVerification(t *testing.T) {
	if os.Getenv("REPODEPLOY_SSH_TEST") != "1" {
		t.Skip("set REPODEPLOY_SSH_TEST=1 to run isolated SSH integration in Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.Mkdir(repo, 0700)
	source, sha := sourceRepo(t)
	if b, err := exec.Command("cp", "-R", source+"/.", repo).CombinedOutput(); err != nil {
		t.Fatalf("copy fixture: %v %s", err, b)
	}
	key := filepath.Join(root, "client-key")
	if b, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("keygen: %v %s", err, b)
	}
	pub, _ := os.ReadFile(key + ".pub")
	os.WriteFile(filepath.Join(root, "authorized_keys"), []byte("restrict,command=\"git-upload-pack '/repo'\" "+string(pub)), 0600)
	dockerfile := `FROM alpine:3.22
RUN apk add --no-cache openssh git && adduser -D git && passwd -d git && ssh-keygen -A
COPY repo /repo
COPY authorized_keys /home/git/.ssh/authorized_keys
RUN chown -R git:git /repo /home/git/.ssh && chmod 700 /home/git/.ssh && chmod 600 /home/git/.ssh/authorized_keys
EXPOSE 22
CMD ["/usr/sbin/sshd","-D","-e","-o","PasswordAuthentication=no","-o","KbdInteractiveAuthentication=no","-o","PermitRootLogin=no"]
`
	os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(dockerfile), 0600)
	tag := "pushvori/ssh-test:" + newID()
	var build bytes.Buffer
	if err := dockerRun(ctx, &build, "build", "-t", tag, root); err != nil {
		t.Fatalf("SSH fixture build: %v %s", err, build.String())
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	name := "pv-ssh-test-" + newID()
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		dockerRun(clean, io.Discard, "rm", "-f", name)
		dockerRun(clean, io.Discard, "image", "rm", tag)
	})
	if err = dockerRun(ctx, io.Discard, "run", "--detach", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:22", port), tag); err != nil {
		t.Fatal(err)
	}
	hosts := filepath.Join(root, "known_hosts")
	var known []byte
	for range 30 {
		cmd := exec.CommandContext(ctx, "ssh-keyscan", "-T", "1", "-p", fmt.Sprint(port), "127.0.0.1")
		known, _ = cmd.Output()
		if len(known) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(known) == 0 {
		logs, _ := dockerOutput(ctx, "logs", name)
		t.Fatalf("SSH fixture not ready: %s", logs)
	}
	os.WriteFile(hosts, known, 0600)
	p := Project{Repo: fmt.Sprintf("ssh://git@127.0.0.1:%d/repo", port), Branch: "main", SSHKey: key, KnownHosts: hosts}
	resolved, err := (Git{}).Resolve(ctx, p)
	if err != nil || resolved != sha {
		t.Fatalf("private SSH resolution: %s %v", resolved, err)
	}
	if err = (Git{}).Prepare(ctx, p, sha, filepath.Join(t.TempDir(), "checkout")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(hosts, []byte(""), 0600)
	if _, err = (Git{}).Resolve(ctx, p); err == nil {
		t.Fatal("unknown SSH host was accepted")
	}
	os.WriteFile(hosts, known, 0600)
	wrong := filepath.Join(root, "wrong-key")
	exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", wrong).Run()
	p.SSHKey = wrong
	if _, err = (Git{}).Resolve(ctx, p); err == nil || strings.Contains(err.Error(), string(pub)) {
		t.Fatalf("unauthorized SSH key accepted or disclosed: %v", err)
	}
}
