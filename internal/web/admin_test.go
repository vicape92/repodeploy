package web

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"repodeploy/internal/deploy"
)

type fakeDeleter struct {
	store   *deploy.Store
	calls   int
	volumes bool
	err     error
}

func (d *fakeDeleter) DeleteProject(ctx context.Context, id string, volumes bool) error {
	d.calls++
	d.volumes = volumes
	return d.err
}

func savedProject(t *testing.T, server *Server) deploy.Project {
	t.Helper()
	p, err := server.Store.SaveProject(deploy.Project{Name: "shop", Repo: "https://github.com/team/shop.git", Branch: "main", Mode: "manual", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBrowserDeleteRequiresTypedProjectName(t *testing.T) {
	server, h, token := webFixture(t)
	deleter := &fakeDeleter{}
	server.Deleter = deleter
	p := savedProject(t, server)
	client, csrf := loggedBrowser(t, h.URL)
	resp, _ := client.Get(h.URL + "/projects/" + p.ID + "/delete")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `name="confirm_name"`) {
		t.Fatalf("no deletion page: %d", resp.StatusCode)
	}
	resp = postForm(t, client, h.URL+"/projects/"+p.ID+"/delete", url.Values{"_csrf": {csrf}, "confirm_name": {"sho"}, "volumes": {"on"}})
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || deleter.calls != 0 || !strings.Contains(string(body), "Type the project name") {
		t.Fatalf("deleted without exact confirmation: %d calls=%d", resp.StatusCode, deleter.calls)
	}
	resp = postForm(t, client, h.URL+"/projects/"+p.ID+"/delete", url.Values{"_csrf": {csrf}, "confirm_name": {"shop"}})
	resp.Body.Close()
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/?deleted=1" || deleter.calls != 1 || deleter.volumes {
		t.Fatalf("confirmed deletion: %d calls=%d volumes=%v", resp.StatusCode, deleter.calls, deleter.volumes)
	}
	deleter.err = deploy.ErrBusy
	resp = request(t, "DELETE", h.URL+"/api/projects/"+p.ID+"?volumes=1", token, nil)
	resp.Body.Close()
	if resp.StatusCode != 409 || !deleter.volumes {
		t.Fatalf("busy API deletion: %d volumes=%v", resp.StatusCode, deleter.volumes)
	}
}

func TestSecretsCanBeManagedFromTheProjectPage(t *testing.T) {
	server, h, token := webFixture(t)
	p := savedProject(t, server)
	client, csrf := loggedBrowser(t, h.URL)
	resp := postForm(t, client, h.URL+"/projects/"+p.ID+"/secrets", url.Values{"_csrf": {csrf}, "name": {"env"}, "value": {"USER=app\r\nPASSWORD=hunter2-long\r\n"}})
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("paste save: %d", resp.StatusCode)
	}
	stored, _ := os.ReadFile(filepath.Join(server.Store.Root, "secrets", p.ID+"-env"))
	if string(stored) != "USER=app\nPASSWORD=hunter2-long\n" {
		t.Fatalf("pasted value not normalised: %q", stored)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("_csrf", csrf)
	mw.WriteField("name", "tls_key")
	fw, _ := mw.CreateFormFile("file", "key.pem")
	fw.Write([]byte("binary\r\ncontent"))
	mw.Close()
	req, _ := http.NewRequest("POST", h.URL+"/projects/"+p.ID+"/secrets", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	stored, _ = os.ReadFile(filepath.Join(server.Store.Root, "secrets", p.ID+"-tls_key"))
	if resp.StatusCode != 303 || string(stored) != "binary\r\ncontent" {
		t.Fatalf("upload save: %d %q", resp.StatusCode, stored)
	}
	resp, _ = client.Get(h.URL + "/projects/" + p.ID)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "<code>env</code>") || !strings.Contains(string(page), "<code>tls_key</code>") || strings.Contains(string(page), "hunter2") {
		t.Fatal("secret list missing or leaks values")
	}
	resp = postForm(t, client, h.URL+"/projects/"+p.ID+"/secrets", url.Values{"_csrf": {csrf}, "name": {"bad name"}, "value": {"x"}})
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(page), "invalid secret name") {
		t.Fatalf("invalid name: %d", resp.StatusCode)
	}
	resp = postForm(t, client, h.URL+"/projects/"+p.ID+"/secrets/env/delete", url.Values{"_csrf": {csrf}})
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(server.Store.Root, "secrets", p.ID+"-env")); resp.StatusCode != 303 || !os.IsNotExist(err) {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp = request(t, "GET", h.URL+"/api/projects/"+p.ID+"/secrets", token, nil)
	list, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(list), `"name":"tls_key"`) || strings.Contains(string(list), `"name":"env"`) {
		t.Fatalf("API list: %s", list)
	}
}

func TestDashboardShowsLatestDeploymentState(t *testing.T) {
	server, h, _ := webFixture(t)
	p := savedProject(t, server)
	j, _ := server.Store.Enqueue(p.ID, "manual", "", "", "")
	server.Store.Finish(j.ID, "failed", "build failed", "")
	client, _ := loggedBrowser(t, h.URL)
	resp, _ := client.Get(h.URL + "/")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "✗ Failed") || !strings.Contains(string(body), `href="/jobs/`+j.ID+`"`) || strings.Contains(string(body), `data-refresh`) {
		t.Fatal("dashboard lacks failed state or refreshes needlessly")
	}
	server.Store.Enqueue(p.ID, "manual", "", "", "")
	resp, _ = client.Get(h.URL + "/")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Queued") || !strings.Contains(string(body), `data-refresh="1"`) {
		t.Fatal("dashboard does not show and follow a pending deployment")
	}
}

func TestPagesRunUnderStrictCSPWithoutInlineCode(t *testing.T) {
	server, h, _ := webFixture(t)
	p := savedProject(t, server)
	j, _ := server.Store.Enqueue(p.ID, "manual", "", "", "")
	client, _ := loggedBrowser(t, h.URL)
	inline := regexp.MustCompile(`<script>|<script [^>]*>[^<]+</script>|<style|\sstyle="|\son[a-z]+="`)
	for _, path := range []string{"/login", "/", "/projects/" + p.ID, "/jobs/" + j.ID, "/settings", "/connections", "/connections/ssh", "/projects/" + p.ID + "/delete"} {
		resp, err := client.Get(h.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		csp := resp.Header.Get("Content-Security-Policy")
		if strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "script-src 'self'") {
			t.Fatalf("%s: weak CSP %q", path, csp)
		}
		if loc := inline.FindString(string(body)); loc != "" {
			t.Fatalf("%s: inline code %q", path, loc)
		}
	}
	for file, kind := range map[string]string{"/static/app.js": "javascript", "/static/app.css": "text/css"} {
		resp, err := http.Get(h.URL + file)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), kind) {
			t.Fatalf("%s: %d %s", file, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	resp, _ := http.Get(h.URL + "/static/")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("static directory listing exposed: %d", resp.StatusCode)
	}
}
