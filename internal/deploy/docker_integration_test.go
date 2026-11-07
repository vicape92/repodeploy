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
)

func TestDockerIntegrationBuildStartupHealthRollbackAndVolumes(t *testing.T) {
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
	p.ComposeFile = "compose.yaml"
	p.Dockerfile = ""
	p.HealthURL = ""
	p.HealthService = "app"
	p.HealthTimeout = 8
	p.Retain = 3
	repo, firstSHA := sourceRepo(t)
	_ = firstSHA
	for _, name := range []string{"Dockerfile", "app.sh", "compose.yaml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "examples", "app", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "compose.yaml" {
			b = []byte(strings.ReplaceAll(string(b), "18080:8080", fmt.Sprintf("%d:8080", port)))
		}
		if err = os.WriteFile(filepath.Join(repo, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "add app")
	p.Repo = repo
	p, _ = s.SaveProject(p)
	d := Docker{Store: s}
	e := &Engine{Store: s, Runtime: d}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		jobs, _ := s.Jobs(p.ID)
		for _, j := range jobs {
			if j.Manifest != "" {
				d.Remove(cleanupCtx, p, j.Manifest, io.Discard)
				break
			}
		}
		// This volume was created by this test's random project, never a user volume.
		dockerRun(cleanupCtx, io.Discard, "volume", "rm", "pv-"+p.ID+"-data")
		images, _ := dockerOutput(cleanupCtx, "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "pushvori/"+p.ID+"-app")
		for _, image := range strings.Fields(string(images)) {
			dockerRun(cleanupCtx, io.Discard, "image", "rm", image)
		}
	})
	first := runDeployment(t, e, p)
	if first.Phase != "succeeded" {
		log, _ := s.Logs(first.ID)
		t.Fatalf("first deployment: %+v\n%s", first, log)
	}
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if strings.TrimSpace(string(b)) != "first" {
		t.Fatalf("wrong active source: %q", b)
	}
	var container strings.Builder
	if err = d.compose(ctx, p, first.Manifest, &container, "ps", "--quiet", "app"); err != nil {
		t.Fatal(err)
	}
	cid := strings.TrimSpace(container.String())
	if err = dockerRun(ctx, io.Discard, "exec", cid, "sh", "-c", "printf 'keep-me' > /data/marker"); err != nil {
		t.Fatal(err)
	}
	originalDockerfile, _ := os.ReadFile(filepath.Join(repo, "Dockerfile"))
	originalCompose, _ := os.ReadFile(filepath.Join(repo, "compose.yaml"))
	for _, stage := range []string{"build", "startup", "health", "implicit-volume"} {
		t.Run(stage, func(t *testing.T) {
			os.WriteFile(filepath.Join(repo, "version.txt"), []byte("second"), 0600)
			os.WriteFile(filepath.Join(repo, "Dockerfile"), originalDockerfile, 0600)
			os.WriteFile(filepath.Join(repo, "compose.yaml"), originalCompose, 0600)
			switch stage {
			case "build":
				os.WriteFile(filepath.Join(repo, "Dockerfile"), append(append([]byte{}, originalDockerfile...), []byte("\nRUN exit 1\n")...), 0600)
			case "startup":
				os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte(strings.ReplaceAll(string(originalCompose), `FAIL_START: "0"`, `FAIL_START: "1"`)), 0600)
			case "health":
				os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte(strings.ReplaceAll(string(originalCompose), `FAIL_HEALTH: "0"`, `FAIL_HEALTH: "1"`)), 0600)
			case "implicit-volume":
				os.WriteFile(filepath.Join(repo, "Dockerfile"), append(append([]byte{}, originalDockerfile...), []byte("\nVOLUME [\"/implicit\"]\n")...), 0600)
			}
			gitTest(t, repo, "add", ".")
			gitTest(t, repo, "commit", "-m", stage)
			job := runDeployment(t, e, p)
			if job.Phase != "failed" {
				t.Fatalf("expected failed %s: %+v", stage, job)
			}
			if (stage == "startup" || stage == "health") && job.Recovery != "restored" {
				log, _ := s.Logs(job.ID)
				t.Fatalf("recovery: %+v\n%s", job, log)
			}
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.TrimSpace(string(body)) != "first" {
				t.Fatalf("active version changed after failure: %q", body)
			}
		})
	}
	container.Reset()
	d.compose(ctx, p, first.Manifest, &container, "ps", "--quiet", "app")
	cid = strings.TrimSpace(container.String())
	marker, err := dockerOutput(ctx, "exec", cid, "cat", "/data/marker")
	if err != nil || string(marker) != "keep-me" {
		t.Fatalf("volume lost: %s %v", marker, err)
	}
}

func TestDockerIntegrationCIWaitsBeforeBuildingAndActivatesExactCommit(t *testing.T) {
	if os.Getenv("REPODEPLOY_DOCKER_TEST") != "1" {
		t.Skip("set REPODEPLOY_DOCKER_TEST=1 to run isolated Docker integration")
	}
	e, _, p, status := ciFixture(t)
	repo := p.Repo
	for _, name := range []string{"Dockerfile", "app.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "examples", "app", name))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(repo, name), b, 0600)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "add CI deployment fixture")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.Port = listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	p.HealthURL = fmt.Sprintf("http://127.0.0.1:%d/health", p.Port)
	p, _ = e.Store.SaveProject(p)
	docker := Docker{Store: e.Store}
	e.Runtime = docker
	waiting := runDeployment(t, e, p)
	if waiting.Phase != "waiting_ci" {
		t.Fatalf("did not wait for CI: %+v", waiting)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		job, _ := e.Store.Job(waiting.ID)
		if job.Manifest != "" {
			docker.Remove(clean, p, job.Manifest, io.Discard)
		}
		dockerRun(clean, io.Discard, "image", "rm", "pushvori/"+p.ID+"-app:"+waiting.ID)
	})
	images, err := dockerOutput(ctx, "image", "ls", "--format", "{{.Repository}}", "pushvori/"+p.ID+"-app")
	if err != nil || strings.TrimSpace(string(images)) != "" {
		t.Fatalf("image built before CI passed: %s %v", images, err)
	}
	*status = "SUCCESSFUL"
	restarted := &Engine{Store: e.Store, Runtime: docker}
	if _, err = restarted.Step(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ := e.Store.Job(waiting.ID)
	if job.Phase != "succeeded" || job.Commit != waiting.Commit {
		logs, _ := e.Store.Logs(job.ID)
		t.Fatalf("CI deployment: %+v\n%s", job, logs)
	}
	response, err := http.Get(p.HealthURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("health %d", response.StatusCode)
	}
}
