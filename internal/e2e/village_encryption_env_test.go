package e2e

import (
	"strings"
	"testing"
)

func TestVillageProcessReceivesDeterministicEncryptionAuthority(t *testing.T) {
	assignments := villageEncryptionEnvAssignments()
	if len(assignments) != 2 {
		t.Fatalf("Village encryption environment assignments=%d want 2", len(assignments))
	}
	seenVersion, seenKeyring := false, false
	for _, assignment := range assignments {
		switch {
		case assignment == envAssignment(envTranscriptKEKActiveVersion, testTranscriptKEKVersion):
			seenVersion = true
		case assignment == envAssignment(envTranscriptKEKKeyring, testTranscriptKEKKeyring):
			seenKeyring = true
		case strings.HasPrefix(assignment, envTranscriptKEKActiveVersion.String()+"=") || strings.HasPrefix(assignment, envTranscriptKEKKeyring.String()+"="):
			t.Fatal("Village encryption environment contains an unexpected authority value")
		}
	}
	if !seenVersion || !seenKeyring {
		t.Fatalf("Village encryption environment is incomplete: version=%v keyring=%v", seenVersion, seenKeyring)
	}
}
