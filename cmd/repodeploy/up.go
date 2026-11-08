package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/vicape92/repodeploy/internal/deploy"
	"github.com/vicape92/repodeploy/internal/upload"
)

// projectFile remembers the upload project of a directory.
const projectFile = ".repodeploy/project"

type upOptions struct {
	project string
	dir     string
	force   bool
	detach  bool
}

func parseUpArgs(args []string) (upOptions, error) {
	o := upOptions{dir: "."}
	dirSet := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--force":
			o.force = true
		case a == "--detach":
			o.detach = true
		case a == "--project":
			if i+1 >= len(args) {
				return o, errors.New("--project needs a project ID")
			}
			i++
			o.project = args[i]
		case strings.HasPrefix(a, "--project="):
			o.project = strings.TrimPrefix(a, "--project=")
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown up option %q", a)
		case dirSet:
			return o, errors.New("up accepts a single directory")
		default:
			o.dir, dirSet = a, true
		}
	}
	return o, nil
}

// apiClient sends authenticated requests to the daemon.
type apiClient struct {
	base  string
	token string
	http  *http.Client
}

func (c apiClient) do(ctx context.Context, method, path, contentType string, body io.Reader, contentLength int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.base, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if contentLength > 0 {
		req.ContentLength = contentLength
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (c apiClient) job(ctx context.Context, id string) (deploy.Job, error) {
	var j deploy.Job
	resp, err := c.do(ctx, http.MethodGet, "/api/jobs/"+url.PathEscape(id), "", nil, 0)
	if err != nil {
		return j, err
	}
	defer resp.Body.Close()
	return j, json.NewDecoder(resp.Body).Decode(&j)
}

// up packs a directory, uploads it to an upload project and follows the
// deployment log unless --detach is given.
func up(ctx context.Context, c apiClient, args []string, out io.Writer) error {
	o, err := parseUpArgs(args)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(o.dir)
	if err != nil {
		return err
	}
	remembered := false
	if o.project == "" {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(projectFile)))
		if err != nil {
			return errors.New("pass --project ID; after the first upload the ID is saved in .repodeploy/project")
		}
		o.project, remembered = strings.TrimSpace(string(b)), true
	}
	files, envFiles, err := upload.SelectFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no files to upload in %s", dir)
	}
	archive, err := os.CreateTemp("", "repodeploy-up-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if err = upload.Pack(dir, files, archive); err != nil {
		return err
	}
	size, err := archive.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = archive.Seek(0, io.SeekStart)
	}
	if err != nil {
		return err
	}
	if len(envFiles) > 0 {
		fmt.Fprintf(out, "Skipped %s; store their values with: repodeploy env import %s FILE\n", strings.Join(envFiles, ", "), o.project)
	}
	fmt.Fprintf(out, "Uploading %d files (%s)\n", len(files), formatSize(size))
	path := "/api/projects/" + url.PathEscape(o.project) + "/uploads"
	if o.force {
		path += "?force=1"
	}
	resp, err := c.do(ctx, http.MethodPost, path, "application/octet-stream", archive, size)
	if err != nil {
		return err
	}
	var j deploy.Job
	err = json.NewDecoder(resp.Body).Decode(&j)
	status := resp.StatusCode
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("unexpected daemon response: %w", err)
	}
	if !remembered {
		err = os.MkdirAll(filepath.Join(dir, ".repodeploy"), 0755)
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, filepath.FromSlash(projectFile)), []byte(o.project+"\n"), 0644)
		}
		if err != nil {
			fmt.Fprintf(out, "Warning: cannot save the project ID: %v\n", err)
		}
	}
	if status == http.StatusOK {
		_, err = fmt.Fprintf(out, "This version is already active (deployment %s); use --force to deploy it again\n", j.ID)
		return err
	}
	fmt.Fprintf(out, "Deployment %s queued\n", j.ID)
	if o.detach {
		return nil
	}
	resp, err = c.do(ctx, http.MethodGet, "/api/jobs/"+url.PathEscape(j.ID)+"/logs?follow=1", "", nil, 0)
	if err != nil {
		return err
	}
	err = streamLogs(resp.Body, out)
	resp.Body.Close()
	if err != nil {
		return err
	}
	if j, err = c.job(ctx, j.ID); err != nil {
		return err
	}
	if j.Phase != "succeeded" {
		return fmt.Errorf("deployment %s %s: %s", j.ID, j.Phase, j.Error)
	}
	return nil
}

func formatSize(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
}
