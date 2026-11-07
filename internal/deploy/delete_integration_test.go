package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Deploys the example app in Dockerfile mode with real Docker, checks the
// injected hardening, then deletes the project in two steps: first keeping
// and then removing its named volumes.
func TestDockerIntegrationHardeningAndProjectDeletion(t *testing.T) {
	if os.Getenv("REPODEPLOY_DOCKER_TEST") != "1" {
		t.Skip("set REPODEPLOY_DOCKER_TEST=1 to run isolated Docker integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := dockerOutput(ctx, "info"); err != nil {
		t.Fatal("Docker integration requested but Docker engine is unavailable:", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	s := testStore(t)
	p := testProject(t, s)
	p.Port, p.ContainerPort = port, 8080
	p.HealthURL = fmt.Sprintf("http://127.0.0.1:%d/health", port)
	p.HealthTimeout = 20
	repo, _ := sourceRepo(t)
	for _, name := range []string{"Dockerfile", "app.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "examples", "app", name))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(repo, name), b, 0600)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "add app")
	p.Repo = repo
	p, _ = s.SaveProject(p)
	d := Docker{Store: s}
	e := &Engine{Store: s, Runtime: d}
	volume := "pv-" + p.ID + "-orphan"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		d.Purge(cleanupCtx, p, "", true, io.Discard)
		images, _ := dockerOutput(cleanupCtx, "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "pushvori/"+p.ID+"-app")
		for _, image := range strings.Fields(string(images)) {
			dockerRun(cleanupCtx, io.Discard, "image", "rm", image)
		}
	})
	j := runDeployment(t, e, p)
	if j.Phase != "succeeded" {
		log, _ := s.Logs(j.ID)
		t.Fatalf("deployment: %+v\n%s", j, log)
	}
	ids, err := dockerOutput(ctx, "ps", "--quiet", "--filter", "label=io.pushvori.project="+p.ID)
	if err != nil || len(strings.Fields(string(ids))) != 1 {
		t.Fatalf("container lookup: %q %v", ids, err)
	}
	cid := strings.Fields(string(ids))[0]
	raw, err := dockerOutput(ctx, "inspect", "--format", "{{json .HostConfig}}", cid)
	if err != nil {
		t.Fatal(err)
	}
	var host struct {
		SecurityOpt []string
		LogConfig   struct {
			Type   string
			Config map[string]string
		}
		PortBindings map[string][]struct{ HostIp, HostPort string }
	}
	if err = json.Unmarshal(raw, &host); err != nil {
		t.Fatal(err)
	}
	if len(host.SecurityOpt) != 1 || host.SecurityOpt[0] != "no-new-privileges:true" {
		t.Fatalf("security options: %v", host.SecurityOpt)
	}
	if host.LogConfig.Type != "json-file" || host.LogConfig.Config["max-size"] != "10m" || host.LogConfig.Config["max-file"] != "3" {
		t.Fatalf("log config: %+v", host.LogConfig)
	}
	binding := host.PortBindings["8080/tcp"]
	if len(binding) != 1 || binding[0].HostIp != "127.0.0.1" || binding[0].HostPort != fmt.Sprint(port) {
		t.Fatalf("port binding: %+v", host.PortBindings)
	}

	// A project volume that must survive deletion without --volumes.
	if err = dockerRun(ctx, io.Discard, "volume", "create", volume); err != nil {
		t.Fatal(err)
	}
	if err = e.DeleteProject(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	if ids, _ = dockerOutput(ctx, "ps", "--all", "--quiet", "--filter", "label=io.pushvori.project="+p.ID); strings.TrimSpace(string(ids)) != "" {
		t.Fatalf("containers remain: %q", ids)
	}
	if images, _ := dockerOutput(ctx, "image", "ls", "--quiet", "pushvori/"+p.ID+"-app"); strings.TrimSpace(string(images)) != "" {
		t.Fatalf("built images remain: %q", images)
	}
	if _, err = dockerOutput(ctx, "volume", "inspect", volume); err != nil {
		t.Fatal("volume deleted without --volumes")
	}
	if _, err = s.Project(p.ID); err == nil {
		t.Fatal("project row remains")
	}
	if err = d.Purge(ctx, p, "", true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err = dockerOutput(ctx, "volume", "inspect", volume); err == nil {
		t.Fatal("volume survived --volumes purge")
	}
}
