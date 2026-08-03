package redactmock_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/redactmock"
)

// TestMockRedactionsFreshness_MatchesGenerator asserts the committed
// web/src/lib/session-detail/mock-redactions.ts is byte-identical to the
// generator output (the same GenerateMockRedactionsTS the cmd writer uses). It
// FAILS if the schema fixture changed without regen, or the TS was hand-edited —
// the web drift gate. Regenerate with `go run ./cmd/gen-mock-redactions`.
func TestMockRedactionsFreshness_MatchesGenerator(t *testing.T) {
	want, err := redactmock.GenerateMockRedactionsTS()
	if err != nil {
		t.Fatalf("generate mock-redactions.ts: %v", err)
	}

	root := moduleRoot(t)
	path := filepath.Join(root, redactmock.MockRedactionsRelPath)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed mock: %s\nRegenerate with `go run ./cmd/gen-mock-redactions`.\n%v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf(
			"STALE committed web mock: %s drifted from the schema redaction fixture.\n"+
				"  what: web/src/lib/session-detail/mock-redactions.ts is not what the generator emits.\n"+
				"  why:  the schema fixture changed without regen, or the file was hand-edited.\n"+
				"  fix:  run `go run ./cmd/gen-mock-redactions` and commit the result.",
			path)
	}
}

func TestMockRedactionsPresentation_UsesPublicModuleAttribution(t *testing.T) {
	generated, err := redactmock.GenerateMockRedactionsTS()
	if err != nil {
		t.Fatalf("generate mock-redactions.ts: %v", err)
	}
	if bytes.Contains(generated, []byte("pkg/redact rule:")) {
		t.Fatal("generated redaction presentation attributes a rule to the retired pkg/redact package; update internal/redactmock normalization and regenerate")
	}
	if !bytes.Contains(generated, []byte("github.com/peasant-labs/redact rule:")) {
		t.Fatal("generated redaction presentation does not identify the public redaction module as the rule owner")
	}
}

// moduleRoot walks up from the test working directory to the peasant module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found from test working directory upward")
		}
		dir = parent
	}
}
