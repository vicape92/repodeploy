package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vicape92/repodeploy/internal/deploy"
)

func TestLinuxDaemonIntegrationCLIWebAndRestart(t *testing.T) {
	if os.Getenv("REPODEPLOY_DAEMON_TEST") != "1" {
		t.Skip("set REPODEPLOY_DAEMON_TEST=1 and REPODEPLOY_LINUX_BINARY to test the Linux daemon in Docker")
	}
	binary := os.Getenv("REPODEPLOY_LINUX_BINARY")
	if binary == "" {
		t.Fatal("REPODEPLOY_LINUX_BINARY is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	build := t.TempDir()
	executable, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(build, "repodeploy"), executable, 0755)
	os.WriteFile(filepath.Join(build, "Dockerfile"), []byte("FROM docker:29-cli\nRUN apk add --no-cache git\nCOPY repodeploy /usr/local/bin/repodeploy\nENTRYPOINT [\"/usr/local/bin/repodeploy\"]\n"), 0600)
	tag := fmt.Sprintf("repodeploy/daemon-test:%d", time.Now().UnixNano())
	name := fmt.Sprintf("pv-daemon-test-%d", time.Now().UnixNano())
	docker := func(input string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdin = strings.NewReader(input)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, b)
		}
		return string(b)
	}
	docker("", "build", "-t", tag, build)
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		exec.CommandContext(clean, "docker", "rm", "-f", name).Run()
		exec.CommandContext(clean, "docker", "image", "rm", tag).Run()
	})
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mount := "type=bind,source=" + state + ",target=" + state
	docker("test-password-long-enough\n", "run", "--rm", "-i", "--mount", mount, tag, "--state", state, "init", "--password-file", "-")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	docker("", "run", "--detach", "--name", name, "--mount", mount, "-v", "/var/run/docker.sock:/var/run/docker.sock", "-p", fmt.Sprintf("127.0.0.1:%d:8081", port), tag, "--state", state, "serve", "--listen", "0.0.0.0:8081", "--public-url", "https://fixture.invalid")
	apiURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	cli := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		all := append([]string{"--state", state, "--url", apiURL}, args...)
		if err := run(ctx, all, strings.NewReader(""), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	ready := func() {
		t.Helper()
		for range 100 {
			resp, err := http.Get(apiURL + "/login")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("daemon did not become ready: " + docker("", "logs", name))
	}
	ready()
	project := deploy.Project{Name: "linux-smoke", Repo: "https://example.invalid/app.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"}
	b, _ := json.Marshal(project)
	config := filepath.Join(t.TempDir(), "project.json")
	os.WriteFile(config, b, 0600)
	created := cli("add", "--file", config)
	if err = json.Unmarshal([]byte(created), &project); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cli("projects"), project.ID) {
		t.Fatal("CLI did not persist the project")
	}
	resp, err := http.Get(apiURL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(html), "Administrator password") {
		t.Fatal("web login missing")
	}
	docker("", "restart", name)
	ready()
	if !strings.Contains(cli("projects"), project.ID) {
		t.Fatal("configuration lost after restart")
	}
	result := cli("deploy", project.ID)
	var job deploy.Job
	json.Unmarshal([]byte(result), &job)
	for range 100 {
		history := cli("history", project.ID)
		var jobs []deploy.Job
		json.Unmarshal([]byte(history), &jobs)
		if len(jobs) > 0 && jobs[0].Phase == "failed" {
			if !strings.Contains(cli("logs", job.ID), "Failed") {
				t.Fatal("failed Git deployment has no logs")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("worker did not finish the deliberately unreachable Git deployment")
}
