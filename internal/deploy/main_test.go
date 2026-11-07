package deploy

import (
	"os"
	"testing"
)

// productionGitProtocols keeps the shipped value so tests can assert it.
var productionGitProtocols = gitAllowProtocol

func TestMain(m *testing.M) {
	// Fixture repositories are local directories, which Git treats as the
	// file transport. Production keeps HTTPS and SSH only.
	gitAllowProtocol = "https:ssh:file"
	os.Exit(m.Run())
}
