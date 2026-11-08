package web

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/upload"
)

func (s *Server) maxUpload() int64 {
	if s.MaxUpload > 0 {
		return s.MaxUpload
	}
	return upload.DefaultMaxBytes
}

func isUploadPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/projects/")
	if !ok {
		rest, ok = strings.CutPrefix(path, "/projects/")
	}
	id, ok2 := strings.CutSuffix(rest, "/uploads")
	return ok && ok2 && id != "" && !strings.Contains(id, "/")
}

// humanBytes renders a size limit for messages, for example "200 MB".
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// newProjectPage shows the form for a new upload project. Git projects start
// from the connections page.
func (s *Server) newProjectPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("source") != "upload" {
		http.Redirect(w, r, "/connections", 303)
		return
	}
	s.render(w, r, page{View: "edit", Project: deploy.Project{Source: "upload", Dockerfile: "Dockerfile", Port: 8080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:8080/health", Retain: 5, HealthTimeout: 60}})
}

// upload stores an archive for an upload project and queues its deployment.
// The API takes the raw archive as the body; the panel sends a multipart form.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.Project(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	browser := !api(r)
	fail := func(status int, message string) {
		if browser {
			s.renderUploadError(w, r, p, status, message)
		} else {
			http.Error(w, message, status)
		}
	}
	if !p.IsUpload() {
		fail(400, "this project deploys from Git; create an upload project for manual uploads")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	var mr *multipart.Reader
	if browser {
		var ok bool
		if mr, ok = r.Context().Value(multipartKey{}).(*multipart.Reader); !ok {
			fail(400, "choose a .zip or .tar.gz file to upload")
			return
		}
	}
	dir, err := s.Store.Uploads.TempDir()
	if err != nil {
		fail(500, "cannot store the upload")
		return
	}
	tmp, err := os.CreateTemp(dir, "upload-")
	if err != nil {
		fail(500, "cannot store the upload")
		return
	}
	defer os.Remove(tmp.Name())
	var n int64
	if browser {
		n, force, err = s.copyFormFile(tmp, mr)
	} else {
		n, err = io.Copy(tmp, io.LimitReader(r.Body, s.maxUpload()+1))
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	var tooLarge *http.MaxBytesError
	if errors.Is(err, errNoFile) {
		fail(400, "choose a .zip or .tar.gz file to upload")
		return
	}
	if n > s.maxUpload() || errors.As(err, &tooLarge) {
		fail(413, "the archive exceeds the "+humanBytes(s.maxUpload())+" upload limit")
		return
	}
	if err != nil {
		fail(400, "the upload was interrupted")
		return
	}
	hash, err := s.Store.Uploads.Save(p.ID, tmp.Name(), upload.DefaultLimits(s.maxUpload()))
	var invalid *upload.Error
	if errors.As(err, &invalid) {
		fail(400, invalid.Message)
		return
	}
	if err != nil {
		fail(500, "cannot store the upload")
		return
	}
	if !force && p.Active != "" {
		if active, err := s.Store.Job(p.Active); err == nil && active.Commit == hash {
			if browser {
				http.Redirect(w, r, "/projects/"+p.ID+"?unchanged=1#upload", 303)
			} else {
				jsonResponse(w, 200, active)
			}
			return
		}
	}
	j, err := s.Store.Enqueue(p.ID, "upload", "", hash, "")
	respondJob(w, r, j, err)
}

func (s *Server) renderUploadError(w http.ResponseWriter, r *http.Request, p deploy.Project, status int, message string) {
	view, err := s.projectView(p)
	if err != nil {
		http.Error(w, message, status)
		return
	}
	view.UploadError = message
	s.renderStatus(w, r, status, view)
}

var errNoFile = errors.New("no file part")

// copyFormFile streams the "file" part of a multipart form into dst and
// reads the "force" flag, which may follow the file. Other parts are ignored
// with a bounded read.
func (s *Server) copyFormFile(dst io.Writer, mr *multipart.Reader) (n int64, force bool, err error) {
	got := false
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return n, force, perr
		}
		switch {
		case part.FormName() == "file" && !got:
			got = true
			if n, err = io.Copy(dst, io.LimitReader(part, s.maxUpload()+1)); err != nil || n > s.maxUpload() {
				return n, force, err
			}
		case part.FormName() == "force":
			b, _ := io.ReadAll(io.LimitReader(part, 1024))
			force = string(b) == "on"
		default:
			io.Copy(io.Discard, io.LimitReader(part, 1024))
		}
	}
	if !got {
		return 0, force, errNoFile
	}
	return n, force, nil
}
