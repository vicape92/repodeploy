package web

import (
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
)

func baseForm(csrf string) url.Values {
	return url.Values{"_csrf": {csrf}, "name": {"my-app"}, "repo": {"https://github.com/team/app.git"}, "branch": {"main"}, "mode": {"manual"}, "build_type": {"dockerfile"}, "dockerfile": {"Dockerfile"}, "port": {"18080"}, "container_port": {"8080"}, "bind_address": {"127.0.0.1"}, "health_url": {"http://127.0.0.1:18080/health"}, "compose_file": {"compose.yaml"}, "health_service": {"app"}, "compose_health_url": {""}, "health_timeout": {"60"}, "poll_seconds": {"60"}, "retain": {"5"}, "ci_timeout": {"1800"}}
}

func TestInvalidBrowserFormKeepsInputAndHighlightsField(t *testing.T) {
	server, h, _ := webFixture(t)
	client, csrf := loggedBrowser(t, h.URL)
	form := baseForm(csrf)
	form.Set("branch", "bad..branch")
	form.Set("name", "Keep <me>")
	resp := postForm(t, client, h.URL+"/projects", form)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(body)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(html, `value="Keep &lt;me&gt;"`) || !strings.Contains(html, `value="bad..branch"`) {
		t.Fatal("submitted values were lost or not escaped")
	}
	if !regexp.MustCompile(`id="f-branch" aria-invalid="true"[^>]*autofocus`).MatchString(html) || !strings.Contains(html, `id="form-error"`) || !strings.Contains(html, "invalid deployment branch") {
		t.Fatal("error not attributed to the branch field")
	}
	if projects, _ := server.Store.Projects(); len(projects) != 0 {
		t.Fatal("invalid project persisted")
	}
}

func TestBuildTypeDiscardsFieldsOfTheOtherMode(t *testing.T) {
	server, h, _ := webFixture(t)
	client, csrf := loggedBrowser(t, h.URL)
	form := baseForm(csrf)
	form.Set("build_type", "compose")
	form.Set("compose_health_url", "")
	resp := postForm(t, client, h.URL+"/projects", form)
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("compose project rejected: %d", resp.StatusCode)
	}
	projects, _ := server.Store.Projects()
	p := projects[0]
	if p.ComposeFile != "compose.yaml" || p.HealthService != "app" || p.Dockerfile != "" || p.Port != 0 || p.ContainerPort != 0 || p.HealthURL != "" || p.BindAddress != "" {
		t.Fatalf("compose mode kept Dockerfile fields: %+v", p)
	}
	form = baseForm(csrf)
	form.Set("bind_address", "0.0.0.0")
	resp = postForm(t, client, h.URL+"/projects/"+p.ID, form)
	resp.Body.Close()
	p, _ = server.Store.Project(p.ID)
	if p.ComposeFile != "" || p.HealthService != "" || p.Dockerfile != "Dockerfile" || p.Port != 18080 || p.BindAddress != "0.0.0.0" || p.HealthURL != "http://127.0.0.1:18080/health" {
		t.Fatalf("Dockerfile mode kept Compose fields: %+v", p)
	}
}

func TestComposeHealthURLErrorPointsAtComposeField(t *testing.T) {
	_, h, _ := webFixture(t)
	client, csrf := loggedBrowser(t, h.URL)
	form := baseForm(csrf)
	form.Set("build_type", "compose")
	form.Set("compose_health_url", "http://example.org/health")
	resp := postForm(t, client, h.URL+"/projects", form)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(body), `id="f-compose_health_url" aria-invalid="true"`) {
		t.Fatalf("compose health URL error not attributed: %d", resp.StatusCode)
	}
}

func TestBrowserRollbackRequiresConfirmation(t *testing.T) {
	server, h, _ := webFixture(t)
	p, err := server.Store.SaveProject(deploy.Project{Name: "app", Repo: "https://github.com/team/app.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := server.Store.Enqueue(p.ID, "manual", "", "", "")
	manifest := filepath.Join(server.Store.Root, "releases", j.ID, "compose.json")
	os.MkdirAll(filepath.Dir(manifest), 0700)
	os.WriteFile(manifest, []byte("{}"), 0600)
	server.Store.SetArtifact(j.ID, manifest, "")
	server.Store.Finish(j.ID, "succeeded", "", "")
	client, csrf := loggedBrowser(t, h.URL)
	resp := postForm(t, client, h.URL+"/projects/"+p.ID+"/rollback", url.Values{"_csrf": {csrf}, "release_id": {j.ID}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "paused") || !strings.Contains(string(body), `name="confirm" value="1"`) {
		t.Fatalf("no confirmation page: %d", resp.StatusCode)
	}
	if jobs, _ := server.Store.Jobs(p.ID); len(jobs) != 1 {
		t.Fatal("rollback queued without confirmation")
	}
	resp = postForm(t, client, h.URL+"/projects/"+p.ID+"/rollback", url.Values{"_csrf": {csrf}, "release_id": {j.ID}, "confirm": {"1"}})
	resp.Body.Close()
	if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/jobs/") {
		t.Fatalf("confirmed rollback not queued: %d", resp.StatusCode)
	}
}
