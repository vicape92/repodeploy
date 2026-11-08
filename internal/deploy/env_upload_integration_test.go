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

	"github.com/vicape92/repodeploy/internal/envvars"
	"github.com/vicape92/repodeploy/internal/upload"
)

func TestDockerIntegrationEnvironmentAndUpload(t *testing.T) {
	if os.Getenv("REPODEPLOY_DOCKER_TEST") != "1" {
		t.Skip("set REPODEPLOY_DOCKER_TEST=1 to run isolated Docker integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if _, err := dockerOutput(ctx, "info"); err != nil {
		t.Fatal("Docker integration requested but Docker engine is unavailable:", err)
	}
	for _, mode := range []string{"dockerfile", "compose"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			s := testStore(t)
			p := Project{Name: "env-" + mode, Source: "upload", HealthURL: fmt.Sprintf("http://127.0.0.1:%d/health", port), HealthTimeout: 20, Retain: 5}
			src := t.TempDir()
			os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM busybox:1.37.0\nARG BUILD_LABEL=none\nRUN echo \"$BUILD_LABEL\" > /label\nCOPY app.sh /app.sh\nRUN chmod 755 /app.sh\nENTRYPOINT [\"/app.sh\"]\n"), 0644)
			os.WriteFile(filepath.Join(src, "app.sh"), []byte("#!/bin/sh\nset -eu\nmkdir -p /www\nprintf '%s|%s' \"$GREETING\" \"$(cat /label)\" > /www/index.html\nprintf ok > /www/health\nexec httpd -f -p 8080 -h /www\n"), 0755)
			files := []string{"Dockerfile", "app.sh"}
			if mode == "dockerfile" {
				p.Dockerfile, p.Port, p.ContainerPort = "Dockerfile", port, 8080
			} else {
				p.ComposeFile = "compose.yaml"
				os.WriteFile(filepath.Join(src, "compose.yaml"), []byte(fmt.Sprintf("services:\n  app:\n    build:\n      context: .\n      args:\n        BUILD_LABEL: ${BUILD_LABEL}\n    ports:\n      - \"127.0.0.1:%d:8080\"\n    environment:\n      GREETING: ${GREETING}\n    stop_grace_period: 2s\n", port)), 0644)
				files = append(files, "compose.yaml")
			}
			p, err = s.SaveProject(p)
			if err != nil {
				t.Fatal(err)
			}
			e := &Engine{Store: s, Runtime: Docker{Store: s}}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				e.DeleteProject(cleanup, p.ID, true)
			})
			deployUpload := func() Job {
				t.Helper()
				archive := filepath.Join(t.TempDir(), "src.tar.gz")
				f, _ := os.Create(archive)
				if err := upload.Pack(src, files, f); err != nil {
					t.Fatal(err)
				}
				f.Close()
				hash, err := s.Uploads.Save(p.ID, archive, upload.DefaultLimits(upload.DefaultMaxBytes))
				if err != nil {
					t.Fatal(err)
				}
				j, err := s.Enqueue(p.ID, "upload", "", hash, "")
				if err != nil {
					t.Fatal(err)
				}
				return stepOnce(t, e, j)
			}
			get := func() string {
				t.Helper()
				resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return strings.TrimSpace(string(b))
			}
			build := true
			s.Env.Set(p.ID, []envvars.Update{{Name: "GREETING", Value: "hello $world"}, {Name: "BUILD_LABEL", Value: "v1", Build: &build}})
			first := deployUpload()
			if first.Phase != "succeeded" {
				logs, _ := s.Logs(first.ID)
				t.Fatalf("first: %+v\n%s", first, logs)
			}
			if got := get(); got != "hello $world|v1" {
				t.Fatalf("first response %q", got)
			}
			s.Env.Set(p.ID, []envvars.Update{{Name: "GREETING", Value: "second"}})
			change, err := s.EnqueueConfigChange(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if change = stepOnce(t, e, change); change.Phase != "succeeded" || get() != "second|v1" {
				logs, _ := s.Logs(change.ID)
				t.Fatalf("deploy changes: %+v\n%s", change, logs)
			}
			rollback, _ := s.Enqueue(p.ID, "manual", "", "", first.ID)
			if rollback = stepOnce(t, e, rollback); rollback.Phase != "succeeded" || get() != "hello $world|v1" {
				t.Fatalf("rollback: %+v %q", rollback, get())
			}
			os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM busybox:1.37.0\nRUN exit 1\n"), 0644)
			if broken := deployUpload(); broken.Phase != "failed" || get() != "hello $world|v1" {
				t.Fatalf("broken upload: %+v %q", broken, get())
			}
		})
	}
}
