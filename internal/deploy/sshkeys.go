package deploy

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// SSHKeyUsage maps cleaned private-key paths to the names of projects that use
// them. Unfinished jobs count too: they execute with their queued snapshot.
func (s *Store) SSHKeyUsage() (map[string][]string, error) {
	usage := map[string][]string{}
	projects, err := s.Projects()
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.SSHKey != "" {
			path := filepath.Clean(p.SSHKey)
			usage[path] = append(usage[path], p.Name)
		}
	}
	rows, err := s.db.Query(`SELECT snapshot FROM jobs WHERE phase NOT IN ('succeeded','failed')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var snapshot string
		if err = rows.Scan(&snapshot); err != nil {
			return nil, err
		}
		var p Project
		if json.Unmarshal([]byte(snapshot), &p) == nil && p.SSHKey != "" {
			path := filepath.Clean(p.SSHKey)
			if len(usage[path]) == 0 {
				usage[path] = append(usage[path], p.Name+" (pending deployment)")
			}
		}
	}
	return usage, rows.Err()
}

// PruneSSHKeys deletes generated keys older than maxAge that no project or
// pending deployment uses, so abandoned private keys do not accumulate.
func (s *Store) PruneSSHKeys(maxAge time.Duration, now time.Time) (int, error) {
	if s.Sources == nil {
		return 0, nil
	}
	release, err := lockFile(s.Root, "artifacts.lock", false)
	if err != nil {
		return 0, err
	}
	defer release()
	keys, err := s.Sources.SSHKeys()
	if err != nil {
		return 0, err
	}
	usage, err := s.SSHKeyUsage()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, key := range keys {
		created, err := time.Parse(time.RFC3339, key.Created)
		if err != nil || now.Sub(created) < maxAge || len(usage[filepath.Clean(key.PrivatePath)]) > 0 {
			continue
		}
		if err = s.Sources.DeleteSSH(key.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (s *Store) DeleteSSHKey(id string) error {
	release, err := lockFile(s.Root, "artifacts.lock", false)
	if err != nil {
		return err
	}
	defer release()
	key, err := s.Sources.SSHKey(id)
	if err != nil {
		return err
	}
	usage, err := s.SSHKeyUsage()
	if err != nil {
		return err
	}
	if users := usage[filepath.Clean(key.PrivatePath)]; len(users) > 0 {
		return fmt.Errorf("this key is still used by %s; switch those projects to another key or delete them first", strings.Join(users, ", "))
	}
	return s.Sources.DeleteSSH(id)
}
