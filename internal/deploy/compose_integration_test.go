package deploy

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Verifies with real Compose that an escaped "$$" reaches the container as a
// literal "$" (the shell then expands it) and that PullImages pulls an image
// that is already present.
func TestDockerIntegrationEscapedDollarAndPullImages(t *testing.T) {
	if os.Getenv("REPODEPLOY_DOCKER_TEST") != "1" {
		t.Skip("set REPODEPLOY_DOCKER_TEST=1 to run isolated Docker integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := dockerOutput(ctx, "info"); err != nil {
		t.Fatal("Docker integration requested but Docker engine is unavailable:", err)
	}
	s := testStore(t)
	p := testProject(t, s)
	p.Dockerfile, p.Port, p.ContainerPort, p.HealthURL = "", 0, 0, ""
	p.ComposeFile, p.HealthService, p.HealthTimeout, p.PullImages = "compose.yaml", "app", 30, true
	repo, _ := sourceRepo(t)
	compose := "services:\n  app:\n    image: busybox:1.37.0\n    command: ['sh', '-c', 'echo \"$$0\" > /tmp/shell; sleep 3600']\n    healthcheck:\n      test: ['CMD-SHELL', 'test \"$$(cat /tmp/shell)\" = sh']\n      interval: 1s\n      retries: 20\n"
	os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte(compose), 0600)
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "compose")
	p.Repo = repo
	p, _ = s.SaveProject(p)
	d := Docker{Store: s}
	e := &Engine{Store: s, Runtime: d}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		d.Purge(cleanupCtx, p, "", true, io.Discard)
	})
	j := runDeployment(t, e, p)
	log, _ := s.Logs(j.ID)
	if j.Phase != "succeeded" {
		t.Fatalf("deployment: %+v\n%s", j, log)
	}
	if !strings.Contains(log, "busybox") {
		t.Fatalf("PullImages did not pull the present image:\n%s", log)
	}
}
