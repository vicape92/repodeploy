package deploy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SecretInfo describes a managed Compose secret or config without its value.
type SecretInfo struct {
	Name     string `json:"name"`
	Stored   bool   `json:"stored"`
	Size     int64  `json:"size,omitempty"`
	Modified string `json:"modified,omitempty"`
	// Required means the active release's Compose file declares it.
	Required bool `json:"required"`
}

const secretNameChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"

// ValidSecretName matches the names the secret command and Compose accept.
func ValidSecretName(name string) bool {
	return name != "" && len(name) <= 100 && strings.Trim(name, secretNameChars) == ""
}

func (s *Store) secretPath(projectID, name string) string {
	return filepath.Join(s.Root, "secrets", projectID+"-"+name)
}

// Secrets lists stored values and the names the active release requires.
func (s *Store) Secrets(p Project) ([]SecretInfo, error) {
	byName := map[string]*SecretInfo{}
	paths, err := filepath.Glob(filepath.Join(s.Root, "secrets", p.ID+"-*"))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		name := strings.TrimPrefix(filepath.Base(path), p.ID+"-")
		if err != nil || !info.Mode().IsRegular() || !ValidSecretName(name) {
			continue
		}
		byName[name] = &SecretInfo{Name: name, Stored: true, Size: info.Size(), Modified: info.ModTime().UTC().Format(time.RFC3339)}
	}
	if p.Active != "" {
		if active, err := s.Job(p.Active); err == nil && active.Manifest != "" {
			if b, err := os.ReadFile(active.Manifest); err == nil {
				var cfg map[string]any
				if json.Unmarshal(b, &cfg) == nil {
					for _, kind := range []string{"secrets", "configs"} {
						resources, _ := cfg[kind].(map[string]any)
						for name := range resources {
							if !ValidSecretName(name) {
								continue
							}
							if byName[name] == nil {
								byName[name] = &SecretInfo{Name: name}
							}
							byName[name].Required = true
						}
					}
				}
			}
		}
	}
	out := make([]SecretInfo, 0, len(byName))
	for _, info := range byName {
		out = append(out, *info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SaveSecret atomically stores a value used by the next deployment.
func (s *Store) SaveSecret(projectID, name string, value []byte) error {
	if !ValidSecretName(name) {
		return errors.New("invalid secret name; use letters, digits, '_' or '-' (max 100)")
	}
	if len(value) == 0 {
		return errors.New("secret must contain 1–1048576 bytes")
	}
	return WritePrivate(s.secretPath(projectID, name), value)
}

// DeleteSecret removes a stored value. Retained releases keep their snapshot.
func (s *Store) DeleteSecret(projectID, name string) error {
	if !ValidSecretName(name) {
		return errors.New("invalid secret name")
	}
	if err := os.Remove(s.secretPath(projectID, name)); err != nil {
		if os.IsNotExist(err) {
			return errors.New("secret not found")
		}
		return err
	}
	return nil
}

// WritePrivate replaces path atomically with a 0600 file.
func WritePrivate(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".secret-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LatestJobs returns each project's most recent visible deployment.
func (s *Store) LatestJobs() (map[string]Job, error) {
	rows, err := s.db.Query(`SELECT ` + jobColumns + ` FROM jobs WHERE seq IN (SELECT MAX(seq) FROM jobs WHERE retained=1 GROUP BY project_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out[j.ProjectID] = j
	}
	return out, rows.Err()
}
