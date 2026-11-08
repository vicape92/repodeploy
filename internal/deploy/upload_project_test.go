package deploy

import "testing"

func uploadProject() Project {
	return Project{Name: "site", Source: "upload", Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"}
}

func TestValidateUploadProject(t *testing.T) {
	if err := ValidateProject(uploadProject()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Project){
		"repo":   func(p *Project) { p.Repo = "https://github.com/a/b.git" },
		"branch": func(p *Project) { p.Branch = "main" },
		"ssh":    func(p *Project) { p.SSHKey = "/keys/app" },
		"ci":     func(p *Project) { p.WaitCI = true },
		"poll":   func(p *Project) { p.Mode = "poll" },
		"source": func(p *Project) { p.Source = "ftp" },
	} {
		p := uploadProject()
		mutate(&p)
		if err := ValidateProject(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	git := uploadProject()
	git.Source = ""
	if err := ValidateProject(git); ErrorField(err) != "repo" {
		t.Fatalf("Git project without repository: %v", err)
	}
}

func TestSaveProjectForcesManualModeForUploads(t *testing.T) {
	s := testStore(t)
	p := uploadProject()
	p.EnvDigest = "stale"
	saved, err := s.SaveProject(p)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Mode != "manual" || saved.EnvDigest != "" || !saved.IsUpload() {
		t.Fatalf("%+v", saved)
	}
}
