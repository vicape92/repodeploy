package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/envvars"
)

// envRow is what the panel lists for a variable: never its value.
type envRow struct {
	Name  string
	Build bool
}

type envItem struct {
	Name  string  `json:"name"`
	Build bool    `json:"build"`
	Value *string `json:"value,omitempty"`
}

type envResponse struct {
	Variables []envItem `json:"variables"`
	Pending   bool      `json:"pending"`
}

func (s *Server) envList(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vars, err := s.Store.Env.List(p.ID)
	if err != nil {
		http.Error(w, "cannot read variables", 500)
		return
	}
	pending, err := s.Store.EnvPending(p)
	if err != nil {
		http.Error(w, "cannot compare variables with the active release", 500)
		return
	}
	reveal := r.URL.Query().Get("reveal") == "1"
	out := envResponse{Variables: []envItem{}, Pending: pending}
	for _, v := range vars {
		item := envItem{Name: v.Name, Build: v.Build}
		if reveal {
			value := v.Value
			item.Value = &value
		}
		out.Variables = append(out.Variables, item)
	}
	jsonResponse(w, 200, out)
}

func (s *Server) envSet(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var updates []envvars.Update
	if err = decode(r, &updates); err != nil {
		http.Error(w, "send a JSON list of {name, value, build}", 400)
		return
	}
	if err = s.Store.Env.Set(p.ID, updates); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	jsonResponse(w, 200, map[string]int{"saved": len(updates)})
}

func (s *Server) envDelete(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.Store.Env.Unset(p.ID, r.PathValue("name"))
	if api(r) {
		switch {
		case errors.Is(err, envvars.ErrNotFound):
			http.Error(w, err.Error(), 404)
		case err != nil:
			http.Error(w, "cannot delete variable", 500)
		default:
			jsonResponse(w, 200, map[string]bool{"deleted": true})
		}
		return
	}
	if err != nil {
		s.renderEnvError(w, r, p, err.Error())
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID+"#env", 303)
}

// envSaveForm adds or replaces one variable from the project page.
func (s *Server) envSaveForm(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	build := r.FormValue("build") == "on"
	value := strings.ReplaceAll(r.FormValue("value"), "\r\n", "\n")
	if err = s.Store.Env.Set(p.ID, []envvars.Update{{Name: strings.TrimSpace(r.FormValue("name")), Value: value, Build: &build}}); err != nil {
		s.renderEnvError(w, r, p, err.Error())
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID+"#env", 303)
}

// envImportForm stores every variable of pasted .env contents.
func (s *Server) envImportForm(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	updates, err := envvars.ParseDotEnv(r.FormValue("dotenv"))
	if err == nil {
		err = s.Store.Env.Set(p.ID, updates)
	}
	if err != nil {
		s.renderEnvError(w, r, p, err.Error())
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID+"#env", 303)
}

// envReveal shows one value on its own response page.
func (s *Server) envReveal(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vars, err := s.Store.Env.List(p.ID)
	if err != nil {
		http.Error(w, "cannot read variables", 500)
		return
	}
	for _, v := range vars {
		if v.Name == r.PathValue("name") {
			s.render(w, r, page{View: "env-value", Project: p, EnvName: v.Name, Secret: v.Value})
			return
		}
	}
	http.NotFound(w, r)
}

// deployChanges redeploys the active release with the current variables.
func (s *Server) deployChanges(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.Project(r.PathValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	j, err := s.Store.EnqueueConfigChange(r.PathValue("id"))
	respondJob(w, r, j, err)
}

func (s *Server) renderEnvError(w http.ResponseWriter, r *http.Request, p deploy.Project, message string) {
	view, err := s.projectView(p)
	if err != nil {
		http.Error(w, message, 400)
		return
	}
	view.EnvError = message
	s.renderStatus(w, r, 400, view)
}
