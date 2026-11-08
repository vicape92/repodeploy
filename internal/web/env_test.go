package web

import (
	"encoding/json"
	"io"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/envvars"
)

func readBody(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	defer body.Close()
	b, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func activate(t *testing.T, server *Server, projectID, sha string) deploy.Job {
	t.Helper()
	j, err := server.Store.Enqueue(projectID, "manual", "", sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Store.Finish(j.ID, "succeeded", "", ""); err != nil {
		t.Fatal(err)
	}
	if err = server.Store.SetActive(projectID, j.ID); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestEnvAPIStoresValuesAndRevealsOnlyOnRequest(t *testing.T) {
	server, h, token := webFixture(t)
	p := savedProject(t, server)
	base := h.URL + "/api/projects/" + p.ID + "/env"
	resp := request(t, "PUT", base, token, []map[string]any{{"name": "DATABASE_URL", "value": "postgres://secret-pass@db"}, {"name": "PUBLIC_URL", "value": "https://x", "build": true}})
	if body := readBody(t, resp.Body); resp.StatusCode != 200 {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	resp = request(t, "GET", base, token, nil)
	body := readBody(t, resp.Body)
	if strings.Contains(body, "secret-pass") || !strings.Contains(body, `"name":"PUBLIC_URL","build":true`) || !strings.Contains(body, `"pending":false`) {
		t.Fatalf("list: %s", body)
	}
	resp = request(t, "GET", base+"?reveal=1", token, nil)
	if body = readBody(t, resp.Body); !strings.Contains(body, "postgres://secret-pass@db") {
		t.Fatalf("reveal: %s", body)
	}
	resp = request(t, "PUT", base, token, []map[string]any{{"name": "1BAD", "value": "x"}})
	if readBody(t, resp.Body); resp.StatusCode != 400 {
		t.Fatalf("invalid name: %d", resp.StatusCode)
	}
	resp = request(t, "DELETE", base+"/PUBLIC_URL", token, nil)
	if readBody(t, resp.Body); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp = request(t, "DELETE", base+"/PUBLIC_URL", token, nil)
	if readBody(t, resp.Body); resp.StatusCode != 404 {
		t.Fatalf("second delete: %d", resp.StatusCode)
	}
	resp = request(t, "GET", base, "", nil)
	if readBody(t, resp.Body); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
	if vars, _ := server.Store.Env.List(p.ID); len(vars) != 1 {
		t.Fatalf("stored: %+v", vars)
	}
}

func TestDeployChangesNeedsActiveReleaseAndPinsItsCommit(t *testing.T) {
	server, h, token := webFixture(t)
	p := savedProject(t, server)
	resp := request(t, "POST", h.URL+"/api/projects/"+p.ID+"/deploy-changes", token, nil)
	if body := readBody(t, resp.Body); resp.StatusCode != 409 || !strings.Contains(body, "nothing is deployed yet") {
		t.Fatalf("without release: %d %s", resp.StatusCode, body)
	}
	sha := strings.Repeat("a", 40)
	activate(t, server, p.ID, sha)
	resp = request(t, "POST", h.URL+"/api/projects/"+p.ID+"/deploy-changes", token, nil)
	var job deploy.Job
	json.NewDecoder(resp.Body).Decode(&job)
	resp.Body.Close()
	if resp.StatusCode != 202 || job.Commit != sha || job.Source != "manual" {
		t.Fatalf("deploy changes: %d %+v", resp.StatusCode, job)
	}
}

func TestEnvCanBeManagedFromTheProjectPage(t *testing.T) {
	server, h, _ := webFixture(t)
	p := savedProject(t, server)
	client, csrf := loggedBrowser(t, h.URL)
	project := h.URL + "/projects/" + p.ID
	resp := postForm(t, client, project+"/env", url.Values{"_csrf": {csrf}, "name": {"API_KEY"}, "value": {"hunter2-long-value"}, "build": {"on"}})
	if readBody(t, resp.Body); resp.StatusCode != 303 {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	resp = postForm(t, client, project+"/env/import", url.Values{"_csrf": {csrf}, "dotenv": {"export A=1\r\nB=\"two\"\r\n"}})
	if readBody(t, resp.Body); resp.StatusCode != 303 {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	vars, _ := server.Store.Env.List(p.ID)
	want := []envvars.Var{{Name: "A", Value: "1"}, {Name: "API_KEY", Value: "hunter2-long-value", Build: true}, {Name: "B", Value: "two"}}
	if !reflect.DeepEqual(vars, want) {
		t.Fatalf("stored: %+v", vars)
	}
	resp, _ = client.Get(project)
	page := readBody(t, resp.Body)
	if !strings.Contains(page, "<code>API_KEY</code>") || !strings.Contains(page, "Build and runtime") || strings.Contains(page, "hunter2") || strings.Contains(page, "Undeployed changes") {
		t.Fatal("variable list missing, leaking values or showing a banner without a release")
	}
	resp = postForm(t, client, project+"/env/API_KEY/reveal", url.Values{"_csrf": {csrf}})
	if body := readBody(t, resp.Body); resp.StatusCode != 200 || !strings.Contains(body, "hunter2-long-value") {
		t.Fatalf("reveal: %d", resp.StatusCode)
	}
	resp = postForm(t, client, project+"/env/API_KEY/reveal", url.Values{})
	if readBody(t, resp.Body); resp.StatusCode != 403 {
		t.Fatalf("reveal without CSRF: %d", resp.StatusCode)
	}
	resp = postForm(t, client, project+"/env/import", url.Values{"_csrf": {csrf}, "dotenv": {"NOEQUALS"}})
	if body := readBody(t, resp.Body); resp.StatusCode != 400 || !strings.Contains(body, "line 1") {
		t.Fatalf("bad import: %d", resp.StatusCode)
	}
	resp = postForm(t, client, project+"/env/A/delete", url.Values{"_csrf": {csrf}})
	if readBody(t, resp.Body); resp.StatusCode != 303 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	activate(t, server, p.ID, strings.Repeat("b", 40))
	resp, _ = client.Get(project)
	if page = readBody(t, resp.Body); !strings.Contains(page, "Undeployed changes") || !strings.Contains(page, `action="/projects/`+p.ID+`/deploy-changes"`) {
		t.Fatal("pending banner missing")
	}
}
