package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeRejectsUnsafeOrUnsupportedFeaturesBeforeExecution(t *testing.T) {
	for name, body := range map[string]string{
		"privileged":       "services:\n  app:\n    image: alpine\n    privileged: true\n",
		"host network":     "services:\n  app:\n    image: alpine\n    network_mode: host\n",
		"fixed name":       "services:\n  app:\n    image: alpine\n    container_name: shared\n",
		"bind source":      "services:\n  app:\n    image: alpine\n    volumes: ['./src:/app']\n",
		"anonymous volume": "services:\n  app:\n    image: alpine\n    volumes: ['/data']\n",
		"env file":         "services:\n  app:\n    image: alpine\n    env_file: /etc/shadow\n",
		"include":          "include: /etc/other.yaml\nservices: {}\n",
		"external context": "services:\n  app:\n    build: ../../outside\n",
		"interpolation":    "services:\n  app:\n    image: ${PRIVATE_IMAGE}\n",
		"unknown feature":  "services:\n  app:\n    image: alpine\n    post_start: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(body), 0600)
			p := Project{ID: strings.Repeat("a", 32), ComposeFile: "compose.yaml", HealthService: "app"}
			if _, err := loadCompose(p, dir, t.TempDir(), t.TempDir()); err == nil {
				t.Fatal("unsafe Compose accepted")
			}
		})
	}
}

func TestComposeUsesStableVolumeNamesAndIsolatedBuildImages(t *testing.T) {
	source := t.TempDir()
	os.WriteFile(filepath.Join(source, "Dockerfile"), []byte("FROM scratch"), 0600)
	body := "services:\n  app:\n    build: .\n    ports: ['18080:8080']\n    volumes: ['data:/data']\n    healthcheck:\n      test: ['CMD', '/health']\nvolumes:\n  data: {}\n"
	os.WriteFile(filepath.Join(source, "compose.yaml"), []byte(body), 0600)
	p := Project{ID: strings.Repeat("a", 32), ComposeFile: "compose.yaml", HealthService: "app"}
	cfg, err := loadCompose(p, source, filepath.Join(t.TempDir(), "release-one"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	services := cfg["services"].(map[string]any)
	app := services["app"].(map[string]any)
	if !strings.Contains(app["image"].(string), "release-one") {
		t.Fatal("build tag does not identify the release")
	}
	vol := cfg["volumes"].(map[string]any)["data"].(map[string]any)
	if vol["name"] != "pv-"+p.ID+"-data" {
		t.Fatalf("unstable volume name: %v", vol)
	}
}

func TestComposeRejectsSymlinkEscapingCheckout(t *testing.T) {
	source := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM scratch"), 0600)
	os.Symlink(outside, filepath.Join(source, "escape"))
	os.WriteFile(filepath.Join(source, "compose.yaml"), []byte("services:\n  app:\n    build: ./escape\n"), 0600)
	if _, err := loadCompose(Project{ID: strings.Repeat("a", 32), ComposeFile: "compose.yaml", HealthURL: "http://127.0.0.1:8080/health"}, source, t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("escaped build context accepted")
	}
}

func TestDockerfileModeGeneratesExplicitHealthAndPortConfiguration(t *testing.T) {
	source := t.TempDir()
	os.WriteFile(filepath.Join(source, "Dockerfile"), []byte("FROM scratch"), 0600)
	p := Project{ID: strings.Repeat("a", 32), Dockerfile: "Dockerfile", Port: 18080, ContainerPort: 8080, HealthURL: "http://127.0.0.1:18080/health"}
	cfg, err := loadCompose(p, source, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := cfg["services"].(map[string]any)["app"].(map[string]any)
	if app["restart"] != "unless-stopped" || len(app["ports"].([]any)) != 1 {
		t.Fatalf("bad generated config: %v", app)
	}
}

func TestImageDeclaredVolumesRequireExplicitNamedMounts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		service   map[string]any
		wantError bool
	}{
		{"missing", map[string]any{}, true},
		{"wrong target", map[string]any{"volumes": []any{"data:/other"}}, true},
		{"named target", map[string]any{"volumes": []any{"data:/data"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImageVolumes(tc.service, []byte(`{"/data":{}}`))
			if (err != nil) != tc.wantError {
				t.Fatalf("implicit image volume validation: %v", err)
			}
		})
	}
}
