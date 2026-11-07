package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Docker struct{ Store *Store }

func validateImageVolumes(service map[string]any, raw []byte) error {
	var volumes map[string]json.RawMessage
	if err := json.Unmarshal(raw, &volumes); err != nil {
		return err
	}
	mounts, _ := service["volumes"].([]any)
	for target := range volumes {
		found := false
		for _, v := range mounts {
			mount, _ := v.(string)
			parts := strings.Split(mount, ":")
			if len(parts) >= 2 && filepath.Clean(parts[1]) == filepath.Clean(target) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("image declares VOLUME %s; configure an explicit named volume for that target", target)
		}
	}
	return nil
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

func within(root, path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	root = canonical
	if filepath.IsAbs(path) {
		return "", errors.New("path must be relative to the checkout")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, path))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the checkout")
	}
	return resolved, nil
}
func allowed(m map[string]any, keys string) error {
	for k := range m {
		if !strings.Contains(" "+keys+" ", " "+k+" ") {
			return fmt.Errorf("unsupported Compose field %q", k)
		}
	}
	return nil
}
func mapping(v any) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("expected a Compose mapping")
	}
	return m, nil
}

// noInterpolation rejects Compose variable substitution. "$$" is Compose's
// escape for a literal "$" (for example in a CMD-SHELL healthcheck) and is
// allowed; it stays escaped in the generated manifest, which Compose reads.
func noInterpolation(v any) bool {
	switch x := v.(type) {
	case string:
		return !strings.Contains(strings.ReplaceAll(x, "$$", ""), "$")
	case map[string]any:
		for k, v := range x {
			if strings.Contains(k, "$") || !noInterpolation(v) {
				return false
			}
		}
	case []any:
		for _, v := range x {
			if !noInterpolation(v) {
				return false
			}
		}
	}
	return true
}

func loadCompose(p Project, source, dir, secretsDir string) (map[string]any, error) {
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil, err
	}
	source = canonical
	var cfg map[string]any
	if p.ComposeFile == "" {
		dockerfile, err := within(source, p.Dockerfile)
		if err != nil {
			return nil, err
		}
		cfg = map[string]any{"services": map[string]any{"app": map[string]any{"build": map[string]any{"context": source, "dockerfile": dockerfile}, "ports": []any{publishedPort(p)}, "restart": "unless-stopped"}}}
	} else {
		path, err := within(source, p.ComposeFile)
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		decoder := yaml.NewDecoder(bytes.NewReader(b))
		if err = decoder.Decode(&cfg); err != nil {
			return nil, err
		}
		var extra any
		if err = decoder.Decode(&extra); err != io.EOF {
			return nil, errors.New("only one Compose document is supported")
		}
		if !noInterpolation(cfg) {
			return nil, errors.New("Compose interpolation is unsupported; use explicit values or managed secrets")
		}
		if err = allowed(cfg, "services volumes networks secrets configs"); err != nil {
			return nil, err
		}
	}
	services, err := mapping(cfg["services"])
	if err != nil || len(services) == 0 {
		return nil, errors.New("Compose must define services")
	}
	volumes, err := mapping(cfg["volumes"])
	if err != nil {
		return nil, err
	}
	for name, v := range volumes {
		if !namePattern.MatchString(name) {
			return nil, errors.New("invalid volume name")
		}
		m, err := mapping(v)
		if err != nil {
			return nil, err
		}
		if err = allowed(m, ""); err != nil {
			return nil, err
		}
		volumes[name] = map[string]any{"name": "pv-" + p.ID + "-" + name}
	}
	if len(volumes) > 0 {
		cfg["volumes"] = volumes
	}
	networks, err := mapping(cfg["networks"])
	if err != nil {
		return nil, err
	}
	for name, v := range networks {
		if !namePattern.MatchString(name) {
			return nil, errors.New("invalid network name")
		}
		m, err := mapping(v)
		if err != nil {
			return nil, err
		}
		if err = allowed(m, ""); err != nil {
			return nil, err
		}
	}
	for _, kind := range []string{"secrets", "configs"} {
		resources, err := mapping(cfg[kind])
		if err != nil {
			return nil, err
		}
		for name, v := range resources {
			m, err := mapping(v)
			if err != nil {
				return nil, err
			}
			if !namePattern.MatchString(name) || len(m) != 1 || m["external"] != true {
				return nil, fmt.Errorf("%s must declare external: true and be installed with the secret command", kind)
			}
			b, err := os.ReadFile(filepath.Join(secretsDir, p.ID+"-"+name))
			if err != nil {
				return nil, fmt.Errorf("managed secret/config %q is missing", name)
			}
			target := filepath.Join(dir, "secrets", name)
			if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return nil, err
			}
			if err = os.WriteFile(target, b, 0600); err != nil {
				return nil, err
			}
			resources[name] = map[string]any{"file": target}
		}
		if len(resources) > 0 {
			cfg[kind] = resources
		}
	}
	for name, v := range services {
		if !namePattern.MatchString(name) {
			return nil, errors.New("invalid service name")
		}
		service, err := mapping(v)
		if err != nil {
			return nil, err
		}
		if err = allowed(service, "build image command entrypoint environment ports expose healthcheck depends_on restart volumes networks secrets configs working_dir user init stop_grace_period read_only tmpfs cap_drop"); err != nil {
			return nil, err
		}
		if env, ok := service["environment"].([]any); ok {
			for _, entry := range env {
				s, ok := entry.(string)
				if !ok || !strings.Contains(s, "=") {
					return nil, errors.New("environment entries require explicit values")
				}
			}
		}
		if env, ok := service["environment"].(map[string]any); ok {
			for _, v := range env {
				if v == nil {
					return nil, errors.New("environment inheritance is unsupported")
				}
			}
		}
		if mounts, ok := service["volumes"]; ok {
			list, ok := mounts.([]any)
			if !ok {
				return nil, errors.New("volumes must be a list of named mounts")
			}
			for _, mount := range list {
				s, ok := mount.(string)
				if !ok {
					return nil, errors.New("only short named-volume syntax is supported")
				}
				parts := strings.Split(s, ":")
				if len(parts) < 2 || len(parts) > 3 || !strings.HasPrefix(parts[1], "/") {
					return nil, errors.New("anonymous and bind volumes are unsupported")
				}
				if _, ok = volumes[parts[0]]; !ok {
					return nil, errors.New("mount source must be a declared named volume")
				}
			}
		}
		if build, ok := service["build"]; ok {
			var b map[string]any
			switch x := build.(type) {
			case string:
				b = map[string]any{"context": x}
			case map[string]any:
				b = x
			default:
				return nil, errors.New("invalid build configuration")
			}
			if err = allowed(b, "context dockerfile args target"); err != nil {
				return nil, err
			}
			contextPath := "."
			if v, ok := b["context"].(string); ok {
				contextPath = v
			}
			if p.ComposeFile != "" {
				contextPath, err = within(source, contextPath)
				if err != nil {
					return nil, err
				}
			}
			dockerfile := "Dockerfile"
			if v, ok := b["dockerfile"].(string); ok {
				dockerfile = v
			}
			if !filepath.IsAbs(dockerfile) {
				dockerfile, err = within(contextPath, dockerfile)
				if err != nil {
					return nil, err
				}
			}
			if p.ComposeFile != "" {
				rel, _ := filepath.Rel(source, dockerfile)
				if _, err = within(source, rel); err != nil {
					return nil, err
				}
			}
			if args, ok := b["args"].(map[string]any); ok {
				for _, v := range args {
					if v == nil {
						return nil, errors.New("build arguments require explicit values")
					}
				}
			} else if b["args"] != nil {
				return nil, errors.New("build args must be an explicit mapping")
			}
			b["context"], b["dockerfile"] = contextPath, dockerfile
			service["build"] = b
			service["image"] = "pushvori/" + p.ID + "-" + strings.ToLower(name) + ":" + filepath.Base(dir)
		} else if image, ok := service["image"].(string); !ok || image == "" {
			return nil, errors.New("each service needs an image or build")
		}
		service["labels"] = map[string]any{"io.pushvori.project": p.ID, "io.pushvori.release": filepath.Base(dir)}
		if service["restart"] == nil {
			service["restart"] = "unless-stopped"
		}
		if !p.AllowPrivilegeEscalation {
			service["security_opt"] = []any{"no-new-privileges:true"}
		}
		// Docker's default json-file driver never rotates; cap each container.
		service["logging"] = map[string]any{"driver": "json-file", "options": map[string]any{"max-size": "10m", "max-file": "3"}}
		services[name] = service
	}
	if p.HealthURL == "" {
		svc, ok := services[p.HealthService].(map[string]any)
		if !ok {
			return nil, errors.New("health_service must select a service")
		}
		h, ok := svc["healthcheck"].(map[string]any)
		if !ok || h["test"] == nil || h["disable"] == true {
			return nil, errors.New("selected health service requires an explicit healthcheck")
		}
	}
	cfg["services"] = services
	return cfg, nil
}

func (d Docker) Build(ctx context.Context, p Project, source, dir string, log io.Writer) (string, error) {
	cfg, err := loadCompose(p, source, dir, filepath.Join(d.Store.Root, "secrets"))
	if err != nil {
		return "", err
	}
	services := cfg["services"].(map[string]any)
	current, err := d.Store.Project(p.ID)
	if err != nil {
		return "", err
	}
	if current.Active != "" {
		previous, err := d.Store.Job(current.Active)
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(previous.Manifest)
		if err != nil {
			return "", err
		}
		var old map[string]any
		if err = json.Unmarshal(b, &old); err != nil {
			return "", err
		}
		if !reflect.DeepEqual(mapKeys(old["services"]), mapKeys(cfg["services"])) || !reflect.DeepEqual(old["volumes"], cfg["volumes"]) {
			return "", errors.New("service topology and volume definitions must remain stable in the MVP")
		}
	}
	for _, warning := range allInterfacePorts(services) {
		fmt.Fprintf(log, "Warning: %s is published on all host interfaces and Docker bypasses host firewalls; prefix it with 127.0.0.1: to keep it behind your reverse proxy\n", warning)
	}
	buildPath := filepath.Join(dir, "build.json")
	if err = writeJSON(buildPath, cfg); err != nil {
		return "", err
	}
	if err = d.compose(ctx, p, buildPath, log, buildArgs(p)...); err != nil {
		return "", errors.New("Docker build failed; see deployment logs")
	}
	for _, v := range services {
		service := v.(map[string]any)
		image := service["image"].(string)
		if _, ok := service["build"]; !ok {
			_, inspectErr := dockerOutput(ctx, "image", "inspect", "--format", "{{.Id}}", image)
			if shouldPull(p, inspectErr == nil) {
				if err = dockerRun(ctx, log, "pull", image); err != nil {
					return "", errors.New("image pull failed; see deployment logs")
				}
			}
		}
		id, err := dockerOutput(ctx, "image", "inspect", "--format", "{{.Id}}", image)
		if err != nil {
			return "", errors.New("built image unavailable")
		}
		volumes, err := dockerOutput(ctx, "image", "inspect", "--format", "{{json .Config.Volumes}}", image)
		if err != nil {
			return "", errors.New("cannot inspect image volumes")
		}
		if err = validateImageVolumes(service, volumes); err != nil {
			return "", err
		}
		service["image"] = strings.TrimSpace(string(id))
		delete(service, "build")
	}
	manifest := filepath.Join(dir, "compose.json")
	return manifest, writeJSON(manifest, cfg)
}

// buildArgs selects the Compose build command; PullImages also refreshes the
// base images named in Dockerfiles.
func buildArgs(p Project) []string {
	if p.PullImages {
		return []string{"build", "--pull"}
	}
	return []string{"build"}
}

// shouldPull reports whether an image: service must be pulled. By default a
// locally present tag is reused, so moving tags such as :latest stay frozen
// until PullImages is enabled. Released manifests pin image IDs either way.
func shouldPull(p Project, present bool) bool { return p.PullImages || !present }

// publishedPort formats the Dockerfile-mode port mapping. It binds to loopback
// unless the project explicitly selects another interface.
func publishedPort(p Project) string {
	bind := p.BindAddress
	if bind == "" {
		bind = "127.0.0.1"
	}
	if strings.Contains(bind, ":") {
		bind = "[" + bind + "]"
	}
	return fmt.Sprintf("%s:%d:%d", bind, p.Port, p.ContainerPort)
}

// allInterfacePorts describes Compose port mappings that omit a host IP and
// are therefore published on every interface. An explicit 0.0.0.0 is treated
// as a deliberate choice and not reported.
func allInterfacePorts(services map[string]any) []string {
	var out []string
	for _, name := range mapKeys(services) {
		service, _ := services[name].(map[string]any)
		ports, _ := service["ports"].([]any)
		for _, port := range ports {
			implicit := false
			switch x := port.(type) {
			case string:
				implicit = !strings.HasPrefix(x, "[") && len(strings.Split(x, ":")) < 3
			case map[string]any:
				ip, _ := x["host_ip"].(string)
				implicit = ip == ""
			default:
				implicit = true
			}
			if implicit {
				out = append(out, fmt.Sprintf("service %s port %v", name, port))
			}
		}
	}
	return out
}

func mapKeys(v any) []string {
	m, _ := v.(map[string]any)
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0600)
}
func (d Docker) compose(ctx context.Context, p Project, path string, log io.Writer, args ...string) error {
	base := []string{"compose", "--ansi", "never", "--progress", "plain", "--env-file", "/dev/null", "--project-name", "pv-" + p.ID, "-f", path}
	return dockerRun(ctx, log, append(base, args...)...)
}
func (d Docker) Activate(ctx context.Context, p Project, path string, log io.Writer) error {
	if err := d.compose(ctx, p, path, log, "up", "--detach", "--no-build", "--pull", "never", "--remove-orphans"); err != nil {
		return errors.New("container activation failed; see deployment logs")
	}
	return nil
}
func (d Docker) Remove(ctx context.Context, p Project, path string, log io.Writer) error {
	return d.compose(ctx, p, path, log, "down", "--remove-orphans", "--timeout", "10")
}

func (d Docker) Forget(ctx context.Context, p Project, dir string, log io.Writer) error {
	b, err := os.ReadFile(filepath.Join(dir, "build.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err = json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	services, _ := cfg["services"].(map[string]any)
	for _, v := range services {
		service, _ := v.(map[string]any)
		image, _ := service["image"].(string)
		if !strings.HasPrefix(image, "pushvori/"+p.ID+"-") || !strings.HasSuffix(image, ":"+filepath.Base(dir)) {
			continue
		}
		if _, err = dockerOutput(ctx, "image", "inspect", image); err != nil {
			continue
		}
		if err = dockerRun(ctx, log, "image", "rm", image); err != nil {
			return err
		}
	}
	return nil
}

func (d Docker) Healthy(ctx context.Context, p Project, path string, log io.Writer) error {
	timeout := p.HealthTimeout
	if timeout <= 0 {
		timeout = 60
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err = json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	services := mapKeys(cfg["services"])
	for ctx.Err() == nil {
		ready := true
		for _, service := range services {
			var out bytes.Buffer
			if err = d.compose(ctx, p, path, &out, "ps", "--all", "--quiet", service); err != nil {
				ready = false
				break
			}
			ids := strings.Fields(out.String())
			if len(ids) == 0 {
				ready = false
				break
			}
			for _, id := range ids {
				state, err := dockerOutput(ctx, "inspect", "--format", "{{json .State}}", id)
				if err != nil {
					ready = false
					break
				}
				var st struct {
					Status string
					Health *struct{ Status string }
				}
				if json.Unmarshal(state, &st) != nil {
					ready = false
					break
				}
				if st.Status == "exited" || st.Status == "dead" {
					return fmt.Errorf("service %s exited before becoming healthy", service)
				}
				if st.Status != "running" || (st.Health != nil && st.Health.Status != "healthy") || (p.HealthURL == "" && service == p.HealthService && st.Health == nil) {
					ready = false
				}
			}
		}
		if ready && p.HealthURL != "" {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.HealthURL, nil)
			resp, err := client.Do(req)
			ready = err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300
			if resp != nil {
				resp.Body.Close()
			}
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("health check timed out")
}

func dockerCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	for _, key := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "XDG_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	return cmd
}
func dockerOutput(ctx context.Context, args ...string) ([]byte, error) {
	return dockerCommand(ctx, args...).Output()
}
func dockerRun(ctx context.Context, log io.Writer, args ...string) error {
	cmd := dockerCommand(ctx, args...)
	cmd.Stdout = log
	cmd.Stderr = log
	return cmd.Run()
}
