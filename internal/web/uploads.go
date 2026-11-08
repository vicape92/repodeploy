package web

import (
	"net/http"

	"github.com/vicape92/repodeploy/internal/deploy"
)

// newProjectPage shows the form for a new upload project. Git projects start
// from the connections page.
func (s *Server) newProjectPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("source") != "upload" {
		http.Redirect(w, r, "/connections", 303)
		return
	}
	s.render(w, r, page{View: "edit", Project: deploy.Project{Source: "upload", Dockerfile: "Dockerfile", Port: 8080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:8080/health", Retain: 5, HealthTimeout: 60}})
}
