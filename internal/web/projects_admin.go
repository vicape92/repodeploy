package web

import (
	"context"
	"embed"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"repodeploy/internal/deploy"
)

// ProjectDeleter removes a project and the resources it created.
type ProjectDeleter interface {
	DeleteProject(ctx context.Context, id string, volumes bool) error
}

//go:embed static
var staticFiles embed.FS

// staticHandler serves the embedded stylesheet and script. Keeping them out of
// the HTML lets the Content-Security-Policy forbid inline code.
func staticHandler() http.Handler {
	files := http.FileServerFS(staticFiles)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		files.ServeHTTP(w, r)
	})
}

func (s *Server) projectView(p deploy.Project) (page, error) {
	jobs, err := s.Store.Jobs(p.ID)
	if err != nil {
		return page{}, errors.New("cannot read history")
	}
	secrets, err := s.Store.Secrets(p)
	if err != nil {
		return page{}, errors.New("cannot read secrets")
	}
	return page{View: "project", Project: p, Jobs: jobs, Secrets: secrets}, nil
}

func (s *Server) deletePage(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, page{View: "delete", Project: p})
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	browser := !api(r)
	volumes := r.URL.Query().Get("volumes") == "1"
	if browser {
		volumes = r.FormValue("volumes") == "on"
	}
	fail := func(status int, message string) {
		if browser {
			s.renderStatus(w, r, status, page{View: "delete", Project: p, Error: message})
		} else {
			http.Error(w, message, status)
		}
	}
	if browser && strings.TrimSpace(r.FormValue("confirm_name")) != p.Name {
		fail(400, "Type the project name exactly as shown to confirm.")
		return
	}
	if s.Deleter == nil {
		fail(503, "project deletion is unavailable in this server")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err = s.Deleter.DeleteProject(ctx, p.ID, volumes); err != nil {
		status := 500
		if errors.Is(err, deploy.ErrBusy) {
			status = 409
		}
		fail(status, err.Error())
		return
	}
	if browser {
		http.Redirect(w, r, "/?deleted=1", 303)
	} else {
		jsonResponse(w, 200, map[string]bool{"deleted": true})
	}
}

func (s *Server) secretsList(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	secrets, err := s.Store.Secrets(p)
	if err != nil {
		http.Error(w, "cannot read secrets", 500)
		return
	}
	jsonResponse(w, 200, secrets)
}

// saveSecretForm stores a value from the project page, either pasted or
// uploaded. Pasted text is normalised from the browser's CRLF line endings.
func (s *Server) saveSecretForm(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var value []byte
	if file, header, err := r.FormFile("file"); err == nil {
		defer file.Close()
		if header.Size > 0 {
			value, err = io.ReadAll(file)
			if err != nil {
				s.renderSecretError(w, r, p, "Cannot read the uploaded file.")
				return
			}
		}
	}
	if len(value) == 0 {
		value = []byte(strings.ReplaceAll(r.FormValue("value"), "\r\n", "\n"))
	}
	if err = s.Store.SaveSecret(p.ID, strings.TrimSpace(r.FormValue("name")), value); err != nil {
		s.renderSecretError(w, r, p, err.Error())
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID+"#secrets", 303)
}

func (s *Server) renderSecretError(w http.ResponseWriter, r *http.Request, p deploy.Project, message string) {
	view, err := s.projectView(p)
	if err != nil {
		http.Error(w, message, 400)
		return
	}
	view.SecretError = message
	s.renderStatus(w, r, 400, view)
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err = s.Store.DeleteSecret(p.ID, r.PathValue("name")); err != nil {
		if api(r) {
			http.Error(w, err.Error(), 404)
		} else {
			s.renderSecretError(w, r, p, err.Error())
		}
		return
	}
	if api(r) {
		jsonResponse(w, 200, map[string]bool{"deleted": true})
	} else {
		http.Redirect(w, r, "/projects/"+p.ID+"#secrets", 303)
	}
}
