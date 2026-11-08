package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/upload"
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

func archiveBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	dir := t.TempDir()
	var names []string
	for name, body := range files {
		path := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(path), 0755)
		os.WriteFile(path, []byte(body), 0644)
		names = append(names, name)
	}
	var buf bytes.Buffer
	if err := upload.Pack(dir, names, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func rawUpload(t *testing.T, target, token string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", target, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAPIUploadStoresArchiveQueuesAndDeduplicates(t *testing.T) {
	server, h, token := webFixture(t)
	p := savedUploadProject(t, server)
	target := h.URL + "/api/projects/" + p.ID + "/uploads"
	archive := archiveBytes(t, map[string]string{"Dockerfile": "FROM scratch", "version.txt": "one"})
	resp := rawUpload(t, target, token, archive)
	var job deploy.Job
	json.NewDecoder(resp.Body).Decode(&job)
	resp.Body.Close()
	if resp.StatusCode != 202 || job.Source != "upload" || !upload.ValidHash(job.Commit) {
		t.Fatalf("upload: %d %+v", resp.StatusCode, job)
	}
	if _, err := os.Stat(filepath.Join(server.Store.Root, "uploads", p.ID, job.Commit+".tar.gz")); err != nil {
		t.Fatal("archive not stored")
	}
	server.Store.Finish(job.ID, "succeeded", "", "")
	server.Store.SetActive(p.ID, job.ID)
	resp = rawUpload(t, target, token, archive)
	var same deploy.Job
	json.NewDecoder(resp.Body).Decode(&same)
	resp.Body.Close()
	if resp.StatusCode != 200 || same.ID != job.ID {
		t.Fatalf("identical upload: %d %+v", resp.StatusCode, same)
	}
	resp = rawUpload(t, target+"?force=1", token, archive)
	var forced deploy.Job
	json.NewDecoder(resp.Body).Decode(&forced)
	resp.Body.Close()
	if resp.StatusCode != 202 || forced.ID == job.ID {
		t.Fatalf("forced upload: %d %+v", resp.StatusCode, forced)
	}
}

func TestAPIUploadRejectsBadRequests(t *testing.T) {
	server, h, token := webFixture(t)
	p := savedUploadProject(t, server)
	git := savedProject(t, server)
	target := h.URL + "/api/projects/" + p.ID + "/uploads"
	archive := archiveBytes(t, map[string]string{"Dockerfile": "FROM scratch"})
	if resp := rawUpload(t, h.URL+"/api/projects/"+git.ID+"/uploads", token, archive); readBody(t, resp.Body) == "" || resp.StatusCode != 400 {
		t.Fatalf("Git project upload: %d", resp.StatusCode)
	}
	resp := rawUpload(t, target, token, []byte("not an archive"))
	if body := readBody(t, resp.Body); resp.StatusCode != 400 || !strings.Contains(body, ".zip or .tar.gz") {
		t.Fatalf("invalid archive: %d %s", resp.StatusCode, body)
	}
	server.MaxUpload = 1 << 20
	resp = rawUpload(t, target, token, bytes.Repeat([]byte("x"), (1<<20)+(512<<10)))
	if body := readBody(t, resp.Body); resp.StatusCode != 413 || !strings.Contains(body, "1 MB") {
		t.Fatalf("oversized: %d %s", resp.StatusCode, body)
	}
	server.uploadSlots <- struct{}{}
	server.uploadSlots <- struct{}{}
	resp = rawUpload(t, target, token, archive)
	if readBody(t, resp.Body); resp.StatusCode != 429 {
		t.Fatalf("busy: %d", resp.StatusCode)
	}
	<-server.uploadSlots
	<-server.uploadSlots
	if resp = rawUpload(t, target, "", archive); readBody(t, resp.Body) == "" || resp.StatusCode != 401 {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
	if left, _ := filepath.Glob(filepath.Join(server.Store.Root, "uploads", p.ID, "*")); len(left) != 0 {
		t.Fatalf("rejected uploads stored: %v", left)
	}
}

func TestBrowserUploadNeedsCSRFAndShowsTheForm(t *testing.T) {
	server, h, _ := webFixture(t)
	p := savedUploadProject(t, server)
	client, csrf := loggedBrowser(t, h.URL)
	resp, _ := client.Get(h.URL + "/projects/" + p.ID)
	if page := readBody(t, resp.Body); !strings.Contains(page, `action="/projects/`+p.ID+`/uploads"`) || !strings.Contains(page, "repodeploy up --project "+p.ID) {
		t.Fatal("upload form missing")
	}
	form := func(withCSRF bool, file []byte) (*bytes.Buffer, string) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		if withCSRF {
			mw.WriteField("_csrf", csrf)
		}
		fw, _ := mw.CreateFormFile("file", "site.tar.gz")
		fw.Write(file)
		mw.Close()
		return &body, mw.FormDataContentType()
	}
	send := func(body *bytes.Buffer, contentType string) *http.Response {
		req, _ := http.NewRequest("POST", h.URL+"/projects/"+p.ID+"/uploads", body)
		req.Header.Set("Content-Type", contentType)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	archive := archiveBytes(t, map[string]string{"Dockerfile": "FROM scratch"})
	if resp = send(form(false, archive)); readBody(t, resp.Body) == "" || resp.StatusCode != 403 {
		t.Fatalf("without CSRF: %d", resp.StatusCode)
	}
	resp = send(form(true, archive))
	readBody(t, resp.Body)
	if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/jobs/") {
		t.Fatalf("browser upload: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = client.Get(h.URL + "/projects/" + p.ID)
	if page := readBody(t, resp.Body); !strings.Contains(page, "upload ") {
		t.Fatal("history does not label uploads")
	}
	// An oversized multipart body is refused with 413 before the handler runs.
	server.MaxUpload = 1 << 20
	body, contentType := form(true, bytes.Repeat([]byte("x"), 3<<20))
	req := httptest.NewRequest("POST", "/projects/"+p.ID+"/uploads", body)
	req.Host = strings.TrimPrefix(h.URL, "http://")
	req.Header.Set("Content-Type", contentType)
	u, _ := url.Parse(h.URL)
	for _, c := range client.Jar.Cookies(u) {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("oversized browser upload: %d", rec.Code)
	}
}

func TestIsUploadPathMatchesExactly(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/projects/abc/uploads":   true,
		"/projects/abc/uploads":       true,
		"/api/projects//uploads":      false,
		"/projects//uploads":          false,
		"/projects/a/b/uploads":       false,
		"/api/projects/a/b/uploads":   false,
		"/projects/abc/uploads/extra": false,
		"/other/abc/uploads":          false,
		"/projects/uploads":           false,
	} {
		if got := isUploadPath(path); got != want {
			t.Errorf("%s: got %v want %v", path, got, want)
		}
	}
}

func TestProjectPageHasOneHistoryHeadingAndCSRFFirst(t *testing.T) {
	server, h, _ := webFixture(t)
	up := savedUploadProject(t, server)
	git := savedProject(t, server)
	client, _ := loggedBrowser(t, h.URL)
	for _, id := range []string{up.ID, git.ID} {
		resp, _ := client.Get(h.URL + "/projects/" + id)
		page := readBody(t, resp.Body)
		if n := strings.Count(page, "Deployment history"); n != 1 {
			t.Fatalf("%s: heading appears %d times", id, n)
		}
	}
	resp, _ := client.Get(h.URL + "/projects/" + up.ID)
	page := readBody(t, resp.Body)
	form := page[strings.Index(page, `action="/projects/`+up.ID+`/uploads"`):]
	if c, f := strings.Index(form, `name="_csrf"`), strings.Index(form, `name="file"`); c < 0 || f < 0 || c > f {
		t.Fatal("_csrf must precede the file input")
	}
}

func TestBrowserUploadForceAfterFileAndFileFirstIs403(t *testing.T) {
	server, h, _ := webFixture(t)
	p := savedUploadProject(t, server)
	client, csrf := loggedBrowser(t, h.URL)
	archive := archiveBytes(t, map[string]string{"Dockerfile": "FROM scratch"})
	send := func(csrfFirst, force bool) *http.Response {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		if csrfFirst {
			mw.WriteField("_csrf", csrf)
		}
		fw, _ := mw.CreateFormFile("file", "site.tar.gz")
		fw.Write(archive)
		if force {
			mw.WriteField("force", "on")
		}
		mw.Close()
		req, _ := http.NewRequest("POST", h.URL+"/projects/"+p.ID+"/uploads", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		readBody(t, resp.Body)
		return resp
	}
	if resp := send(false, false); resp.StatusCode != 403 {
		t.Fatalf("file first: %d", resp.StatusCode)
	}
	first := send(true, false)
	if first.StatusCode != 303 || !strings.HasPrefix(first.Header.Get("Location"), "/jobs/") {
		t.Fatalf("first: %d %s", first.StatusCode, first.Header.Get("Location"))
	}
	jobID := strings.TrimPrefix(first.Header.Get("Location"), "/jobs/")
	server.Store.Finish(jobID, "succeeded", "", "")
	server.Store.SetActive(p.ID, jobID)
	if same := send(true, false); !strings.Contains(same.Header.Get("Location"), "unchanged=1") {
		t.Fatalf("identical: %d %s", same.StatusCode, same.Header.Get("Location"))
	}
	forced := send(true, true)
	loc := forced.Header.Get("Location")
	if forced.StatusCode != 303 || !strings.HasPrefix(loc, "/jobs/") || loc == "/jobs/"+jobID {
		t.Fatalf("forced: %d %s", forced.StatusCode, loc)
	}
}
