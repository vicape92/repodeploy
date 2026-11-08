package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/envvars"
	"github.com/vicape92/repodeploy/internal/web"
)

const help = `RepoDeploy — self-hosted Git deployments

Usage: repodeploy [--state DIR] [--url URL] [--token-file FILE] COMMAND

  init --password-file FILE|-       Initialize administrator credentials
  passwd --password-file FILE|-     Change the password; signs out browsers
  rotate-token                     Issue a new CLI token; revokes the old one
  serve [--listen ADDR] [--public-url HTTPS_URL] [--trust-proxy]
        [--allowed-hosts HOST,...] [--max-upload SIZE]
  projects                         List projects
  project ID                       Show project settings
  add --file FILE                   Create a project from JSON
  update ID --file FILE             Update project settings from JSON
  deploy ID                        Queue deployment of the configured branch
  deploy-changes ID                Redeploy the active release with current variables
  env list ID [--reveal]           List environment variables (values with --reveal)
  env set ID NAME [--build|--no-build]
                                   Store a variable; the value is read from stdin
  env unset ID NAME                Delete a variable
  env import ID FILE|-             Store every variable of a .env file
  up [--project ID] [--force] [--detach] [DIR]
                                   Upload DIR (default .) to an upload project and deploy
  rollback ID RELEASE_ID           Restore a retained release; pause automation
  resume ID                        Resume automation after rollback
  history ID                       Show deployment history
  logs JOB_ID [--follow]            Read or stream deployment logs
  secret ID NAME --file FILE|-      Store a managed Compose secret/config
  webhook-secret ID                Rotate and display a GitHub webhook secret
  delete ID --yes [--volumes]      Delete a project; --volumes also deletes data
  backup --output FILE|-            Write a tar.gz backup of the state directory

Global flags precede COMMAND. Default state: /var/lib/repodeploy.
Existing /var/lib/pushvori installations retain their state directory.
Default API: $REPODEPLOY_URL or http://127.0.0.1:8081. Remote API access
requires HTTPS. Token: --token-file, $REPODEPLOY_TOKEN_FILE, the user's
config directory (~/.config/repodeploy/token on Linux), then STATE/admin.token.
Token files must be mode 0600.
`

func main() {
	syscall.Umask(0077)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func defaultStateDirectory(base string) string {
	current := filepath.Join(base, "repodeploy")
	if _, err := os.Stat(current); !os.IsNotExist(err) {
		return current
	}
	legacy := filepath.Join(base, "pushvori")
	if info, err := os.Stat(legacy); err == nil && info.IsDir() {
		return legacy
	}
	return current
}

func run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("repodeploy", flag.ContinueOnError)
	fs.SetOutput(out)
	state := fs.String("state", defaultStateDirectory("/var/lib"), "state directory")
	defaultURL := os.Getenv("REPODEPLOY_URL")
	if defaultURL == "" {
		defaultURL = "http://127.0.0.1:8081"
	}
	apiURL := fs.String("url", defaultURL, "daemon API URL (default $REPODEPLOY_URL)")
	tokenFile := fs.String("token-file", os.Getenv("REPODEPLOY_TOKEN_FILE"), "CLI token file (default $REPODEPLOY_TOKEN_FILE)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) == 0 || args[0] == "help" {
		_, err := io.WriteString(out, help)
		return err
	}
	root, err := filepath.Abs(*state)
	if err != nil {
		return err
	}
	if args[0] == "init" {
		file, err := fileFlag(args[1:])
		if err != nil {
			return err
		}
		b, err := readFile(file, in)
		if err != nil {
			return err
		}
		if err = web.Initialize(root, strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Administrator initialized. CLI token stored at %s\n", filepath.Join(root, "admin.token"))
		return err
	}
	if args[0] == "passwd" {
		file, err := fileFlag(args[1:])
		if err != nil {
			return err
		}
		b, err := readFile(file, in)
		if err != nil {
			return err
		}
		if err = web.ChangePassword(root, strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")); err != nil {
			return err
		}
		_, err = io.WriteString(out, "Administrator password changed. Browser sessions are signed out.\n")
		return err
	}
	if args[0] == "rotate-token" {
		if len(args) != 1 {
			return errors.New("rotate-token takes no arguments")
		}
		if _, err := web.RotateToken(root); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "CLI token rotated; the previous token is revoked. New token stored at %s\n", filepath.Join(root, "admin.token"))
		return err
	}
	if args[0] == "backup" {
		return backup(root, args[1:], out)
	}
	if args[0] == "serve" {
		return serve(ctx, root, args[1:], out)
	}
	u, err := url.Parse(*apiURL)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return errors.New("invalid daemon URL")
	}
	loopback := u.Hostname() == "localhost" || (net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return errors.New("remote daemon access requires HTTPS")
	}
	if *tokenFile == "" {
		*tokenFile = defaultTokenFile(root)
	}
	token, err := readToken(*tokenFile)
	if err != nil {
		return err
	}
	if args[0] == "up" {
		client := apiClient{base: *apiURL, token: strings.TrimSpace(string(token)), http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
		return up(ctx, client, args[1:], out)
	}
	method, path := http.MethodGet, ""
	var body []byte
	follow := false
	need := func(n int) error {
		if len(args) < n {
			return errors.New("missing arguments; run repodeploy help")
		}
		return nil
	}
	switch args[0] {
	case "projects":
		path = "/api/projects"
	case "project":
		if err = need(2); err != nil {
			return err
		}
		path = "/api/projects/" + url.PathEscape(args[1])
	case "add", "update":
		tail := args[1:]
		path = "/api/projects"
		method = http.MethodPost
		if args[0] == "update" {
			if err = need(2); err != nil {
				return err
			}
			path += "/" + url.PathEscape(args[1])
			method = http.MethodPut
			tail = args[2:]
		}
		file, err := fileFlag(tail)
		if err != nil {
			return err
		}
		body, err = readFile(file, in)
		if err != nil {
			return err
		}
	case "deploy", "resume", "webhook-secret", "deploy-changes":
		if err = need(2); err != nil {
			return err
		}
		path = "/api/projects/" + url.PathEscape(args[1]) + "/" + args[0]
		method = http.MethodPost
	case "rollback":
		if err = need(3); err != nil {
			return err
		}
		path = "/api/projects/" + url.PathEscape(args[1]) + "/rollback"
		method = http.MethodPost
		body, _ = json.Marshal(map[string]string{"release_id": args[2]})
	case "history":
		if err = need(2); err != nil {
			return err
		}
		path = "/api/projects/" + url.PathEscape(args[1]) + "/jobs"
	case "logs":
		if err = need(2); err != nil {
			return err
		}
		path = "/api/jobs/" + url.PathEscape(args[1]) + "/logs"
		follow = len(args) > 2 && args[2] == "--follow"
		if follow {
			path += "?follow=1"
		}
	case "delete":
		if err = need(2); err != nil {
			return err
		}
		volumes, confirmed := false, false
		for _, flag := range args[2:] {
			switch flag {
			case "--volumes":
				volumes = true
			case "--yes":
				confirmed = true
			default:
				return fmt.Errorf("unknown delete option %q", flag)
			}
		}
		if !confirmed {
			return errors.New("delete removes the project's containers, images, history and secrets; repeat with --yes to confirm (add --volumes to also delete application data)")
		}
		path = "/api/projects/" + url.PathEscape(args[1])
		if volumes {
			path += "?volumes=1"
		}
		method = http.MethodDelete
	case "env":
		if err = need(3); err != nil {
			return err
		}
		path = "/api/projects/" + url.PathEscape(args[2]) + "/env"
		switch args[1] {
		case "list":
			for _, option := range args[3:] {
				if option != "--reveal" {
					return fmt.Errorf("unknown env list option %q", option)
				}
				path += "?reveal=1"
			}
		case "set":
			if err = need(4); err != nil {
				return err
			}
			update := envvars.Update{Name: args[3]}
			for _, option := range args[4:] {
				build := option == "--build"
				if !build && option != "--no-build" {
					return fmt.Errorf("unknown env set option %q", option)
				}
				update.Build = &build
			}
			value, err := io.ReadAll(io.LimitReader(in, envvars.MaxValueLen+2))
			if err != nil {
				return err
			}
			update.Value = strings.TrimSuffix(strings.TrimSuffix(string(value), "\n"), "\r")
			body, _ = json.Marshal([]envvars.Update{update})
			method = http.MethodPut
		case "unset":
			if err = need(4); err != nil {
				return err
			}
			path += "/" + url.PathEscape(args[3])
			method = http.MethodDelete
		case "import":
			if err = need(4); err != nil {
				return err
			}
			text, err := readFile(args[3], in)
			if err != nil {
				return err
			}
			updates, err := envvars.ParseDotEnv(string(text))
			if err != nil {
				return err
			}
			body, _ = json.Marshal(updates)
			method = http.MethodPut
		default:
			return fmt.Errorf("unknown env command %q; use list, set, unset or import", args[1])
		}
	case "secret":
		if err = need(3); err != nil {
			return err
		}
		file, err := fileFlag(args[3:])
		if err != nil {
			return err
		}
		body, err = readFile(file, in)
		if err != nil {
			return err
		}
		path = "/api/projects/" + url.PathEscape(args[1]) + "/secrets/" + url.PathEscape(args[2])
		method = http.MethodPut
	default:
		return fmt.Errorf("unknown command %q; run repodeploy help", args[0])
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(*apiURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if follow {
		client.Timeout = 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if follow {
		return streamLogs(resp.Body, out)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, b, "", "  ") == nil {
			pretty.WriteByte('\n')
			_, err = out.Write(pretty.Bytes())
			return err
		}
		_, err = out.Write(b)
		return err
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

// defaultTokenFile prefers a per-user copy of the token, so administrators can
// use the CLI without sudo, and falls back to the state directory.
func defaultTokenFile(root string) string {
	if dir, err := os.UserConfigDir(); err == nil {
		candidate := filepath.Join(dir, "repodeploy", "token")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join(root, "admin.token")
}

// readToken refuses token files other users can read, like ssh does for keys.
func readToken(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read CLI token: %w", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("CLI token file %s is accessible to group or others; run chmod 600 %s", path, path)
	}
	token, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CLI token: %w", err)
	}
	return token, nil
}

func fileFlag(args []string) (string, error) {
	if len(args) != 2 || args[0] != "--file" && args[0] != "--password-file" {
		return "", errors.New("expected --file FILE (or --password-file FILE for init)")
	}
	return args[1], nil
}
func readFile(path string, in io.Reader) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(io.LimitReader(in, (1<<20)+1))
	}
	return os.ReadFile(path)
}

// backup writes a consistent archive of the state directory. It runs locally
// as the service user and does not require the daemon.
func backup(root string, args []string, out io.Writer) (err error) {
	if len(args) != 2 || args[0] != "--output" {
		return errors.New("expected backup --output FILE (or - for standard output)")
	}
	if args[1] == "-" {
		return deploy.Backup(root, out)
	}
	f, err := os.OpenFile(args[1], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("cannot create backup file (it must not exist yet): %w", err)
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(args[1])
		}
	}()
	if err = deploy.Backup(root, f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Backup written to %s. It contains credentials and secrets; store it securely.\n", args[1])
	return err
}

func serve(ctx context.Context, root string, args []string, out io.Writer) error {
	if runtime.GOOS != "linux" {
		return errors.New("the daemon supports Linux only; use a Linux host or VM")
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(out)
	listen := fs.String("listen", "127.0.0.1:8081", "HTTP listen address")
	public := fs.String("public-url", "", "external HTTPS URL behind a reverse proxy")
	trustProxy := fs.Bool("trust-proxy", false, "identify clients by X-Forwarded-For from a loopback reverse proxy")
	allowedHosts := fs.String("allowed-hosts", "", "comma-separated extra Host names accepted by the panel")
	maxUpload := fs.String("max-upload", "200MB", "largest accepted upload archive, e.g. 200MB or 1GB")
	if err := fs.Parse(args); err != nil {
		return err
	}
	uploadLimit, err := parseSize(*maxUpload)
	if err != nil {
		return err
	}
	var extraHosts []string
	for _, host := range strings.Split(*allowedHosts, ",") {
		if host = strings.TrimSpace(host); host != "" {
			if strings.ContainsAny(host, "/:@ ") {
				return fmt.Errorf("invalid allowed host %q; use bare host names", host)
			}
			extraHosts = append(extraHosts, host)
		}
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return err
	}
	loopback := host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
	secure := false
	if *public != "" {
		u, err := url.Parse(*public)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return errors.New("public URL must use HTTPS")
		}
		secure = true
	}
	if !loopback && !secure {
		return errors.New("non-loopback listen requires --public-url with HTTPS and a TLS reverse proxy")
	}
	for _, command := range [][]string{{"git", "--version"}, {"ssh", "-V"}, {"docker", "info"}, {"docker", "compose", "version"}} {
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := exec.CommandContext(checkCtx, command[0], command[1:]...).Run()
		cancel()
		if err != nil {
			return fmt.Errorf("prerequisite unavailable: %s", strings.Join(command, " "))
		}
	}
	release, err := deploy.Lock(root)
	if err != nil {
		return err
	}
	defer release()
	store, err := deploy.OpenStore(root)
	if err != nil {
		return err
	}
	defer store.Close()
	if err = store.Uploads.SweepTemp(24 * time.Hour); err != nil {
		return err
	}
	handler, err := web.New(store, secure)
	if err != nil {
		return err
	}
	handler.BaseURL = *public
	handler.TrustProxy = *trustProxy
	handler.AllowedHosts = extraHosts
	handler.MaxUpload = uploadLimit
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Addr: *listen, Handler: handler.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return child }}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	engine := &deploy.Engine{Store: store, Runtime: deploy.Docker{Store: store}}
	handler.Deleter = engine
	if err = engine.Recover(child); err != nil {
		listener.Close()
		return err
	}
	errorsCh := make(chan error, 2)
	workerDone := make(chan struct{})
	pollDone := make(chan struct{})
	go func() { defer close(workerDone); errorsCh <- engine.Run(child) }()
	go func() { defer close(pollDone); deploy.NewPoller(store).Run(child) }()
	go func() { errorsCh <- server.Serve(listener) }()
	fmt.Fprintf(out, "RepoDeploy listening on %s\n", *listen)
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errorsCh:
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	server.Shutdown(shutdownCtx)
	<-workerDone
	<-pollDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// parseSize reads sizes such as 200MB, 1GB, 2048KB or a plain byte count.
func parseSize(s string) (int64, error) {
	text := strings.ToUpper(strings.TrimSpace(s))
	factor := int64(1)
	for _, unit := range []struct {
		suffix string
		factor int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(text, unit.suffix) {
			text, factor = strings.TrimSpace(strings.TrimSuffix(text, unit.suffix)), unit.factor
			break
		}
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n <= 0 || n > (1<<40)/factor {
		return 0, fmt.Errorf("invalid size %q; use a value such as 200MB or 1GB", s)
	}
	if n*factor < 1<<20 {
		return 0, errors.New("--max-upload must be at least 1MB")
	}
	return n * factor, nil
}

// streamLogs prints the log text of a server-sent event stream.
func streamLogs(r io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Log string `json:"log"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			return err
		}
		if _, err := io.WriteString(out, event.Log); err != nil {
			return err
		}
	}
	return scanner.Err()
}
