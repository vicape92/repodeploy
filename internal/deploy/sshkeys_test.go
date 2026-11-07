package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSSHKey writes a generated-key directory without contacting a provider.
func fakeSSHKey(t *testing.T, s *Store, id string, created time.Time) string {
	t.Helper()
	dir := filepath.Join(s.Root, "ssh", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"key", "known_hosts"} {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600)
	}
	meta, _ := json.Marshal(map[string]string{"id": id, "repo": "git@github.com:team/app.git", "created": created.UTC().Format(time.RFC3339)})
	os.WriteFile(filepath.Join(dir, "metadata.json"), meta, 0600)
	return filepath.Join(dir, "key")
}

func TestPruneSSHKeysRemovesOnlyOldUnusedKeys(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	oldUnused := strings.Repeat("a", 32)
	recentUnused := strings.Repeat("b", 32)
	oldUsed := strings.Repeat("c", 32)
	oldPending := strings.Repeat("d", 32)
	fakeSSHKey(t, s, oldUnused, now.Add(-48*time.Hour))
	fakeSSHKey(t, s, recentUnused, now.Add(-time.Hour))
	usedPath := fakeSSHKey(t, s, oldUsed, now.Add(-48*time.Hour))
	pendingPath := fakeSSHKey(t, s, oldPending, now.Add(-48*time.Hour))

	p := testProject(t, s)
	p.SSHKey, p.KnownHosts = usedPath, filepath.Join(filepath.Dir(usedPath), "known_hosts")
	if _, err := s.SaveProject(p); err != nil {
		t.Fatal(err)
	}
	// A queued job keeps its snapshot even if the project later switches keys.
	other := testProject(t, s)
	other.SSHKey, other.KnownHosts = pendingPath, filepath.Join(filepath.Dir(pendingPath), "known_hosts")
	other, _ = s.SaveProject(other)
	if _, err := s.Enqueue(other.ID, "manual", "", "", ""); err != nil {
		t.Fatal(err)
	}
	other.SSHKey, other.KnownHosts = "", ""
	s.SaveProject(other)

	removed, err := s.PruneSSHKeys(UnusedSSHKeyAge, now)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	for id, wantPresent := range map[string]bool{oldUnused: false, recentUnused: true, oldUsed: true, oldPending: true} {
		_, err := os.Stat(filepath.Join(s.Root, "ssh", id))
		if (err == nil) != wantPresent {
			t.Fatalf("key %s present=%v, want %v", id[:1], err == nil, wantPresent)
		}
	}
}
