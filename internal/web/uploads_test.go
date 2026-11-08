package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
)

func savedUploadProject(t *testing.T, server *Server) deploy.Project {
	t.Helper()
	p, err := server.Store.SaveProject(deploy.Project{Name: "site", Source: "upload", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUploadProjectsKeepTheirSourceAndRefuseGitActions(t *testing.T) {
	server, h, token := webFixture(t)
	p := savedUploadProject(t, server)
	git := savedProject(t, server)
	toGit := p
	toGit.Source, toGit.Repo, toGit.Branch, toGit.Mode = "", "https://github.com/a/b.git", "main", "manual"
	resp := request(t, "PUT", h.URL+"/api/projects/"+p.ID, token, toGit)
	if body := readBody(t, resp.Body); resp.StatusCode != 400 || !strings.Contains(body, "cannot change") {
		t.Fatalf("upload to Git: %d %s", resp.StatusCode, body)
	}
	toUpload := git
	toUpload.Source = "upload"
	resp = request(t, "PUT", h.URL+"/api/projects/"+git.ID, token, toUpload)
	if readBody(t, resp.Body); resp.StatusCode != 400 {
		t.Fatalf("Git to upload: %d", resp.StatusCode)
	}
	for _, action := range []string{"deploy", "webhook-secret"} {
		resp = request(t, "POST", h.URL+"/api/projects/"+p.ID+"/"+action, token, nil)
		if readBody(t, resp.Body); resp.StatusCode != 409 {
			t.Fatalf("%s on upload project: %d", action, resp.StatusCode)
		}
	}
}

func TestBrowserCreatesUploadProject(t *testing.T) {
	server, h, _ := webFixture(t)
	client, csrf := loggedBrowser(t, h.URL)
	resp, _ := client.Get(h.URL + "/")
	if page := readBody(t, resp.Body); !strings.Contains(page, `href="/projects/new?source=upload"`) {
		t.Fatal("dashboard has no upload entry point")
	}
	resp, _ = client.Get(h.URL + "/projects/new?source=upload")
	page := readBody(t, resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(page, `name="source" value="upload"`) || strings.Contains(page, `name="repo"`) || strings.Contains(page, `name="ssh_key"`) {
		t.Fatal("upload project form shows Git fields")
	}
	resp = postForm(t, client, h.URL+"/projects", url.Values{"_csrf": {csrf}, "source": {"upload"}, "name": {"site"}, "build_type": {"dockerfile"}, "dockerfile": {"Dockerfile"}, "port": {"18080"}, "container_port": {"8080"}, "bind_address": {"127.0.0.1"}, "health_url": {"http://127.0.0.1:18080/health"}, "health_timeout": {"60"}, "retain": {"5"}})
	readBody(t, resp.Body)
	if resp.StatusCode != 303 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	projects, _ := server.Store.Projects()
	if len(projects) != 1 || !projects[0].IsUpload() || projects[0].Mode != "manual" {
		t.Fatalf("%+v", projects)
	}
	resp, _ = client.Get(h.URL + resp.Header.Get("Location"))
	page = readBody(t, resp.Body)
	if !strings.Contains(page, "Manual upload") || strings.Contains(page, "Deploy branch") || strings.Contains(page, "GitHub webhook") {
		t.Fatal("project page shows Git controls for an upload project")
	}
}
