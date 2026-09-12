package ingest

import (
	"bytes"
	_ "embed"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/artifact_owned_names.yaml
var artifactOwnedNamesYAML []byte

type artifactOwnedNameCase struct {
	Name     string            `yaml:"name"`
	Relative string            `yaml:"relative"`
	Kind     artifactOwnedKind `yaml:"kind"`
}

// artifactOwnedKinds is the closed set of families a publication owns. A case
// naming anything outside it fails: the set decides what may be pruned, so a
// new family has to be declared here before a fixture can rely on it.
var artifactOwnedKinds = []artifactOwnedKind{artifactOwnedMetadata, artifactOwnedTranscript, artifactOwnedSourceCapture, artifactOwnedDebug}

func loadArtifactOwnedNameCases(t *testing.T) []artifactOwnedNameCase {
	t.Helper()
	var document struct {
		RequiredNames []string                `yaml:"requiredNames"`
		Cases         []artifactOwnedNameCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(artifactOwnedNamesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode the owned-name fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("the owned-name fixture must hold exactly one YAML document: %v", err)
	}
	present := make(map[string]bool, len(document.Cases))
	covered := make(map[artifactOwnedKind]bool, len(artifactOwnedKinds))
	refused := false
	for _, row := range document.Cases {
		if row.Name == "" || present[row.Name] {
			t.Fatalf("the owned-name fixture has an empty or repeated case name %q", row.Name)
		}
		present[row.Name] = true
		if row.Relative == "" {
			t.Fatalf("fixture case %q names no path", row.Name)
		}
		if row.Kind == "" {
			refused = true
			continue
		}
		known := false
		for _, kind := range artifactOwnedKinds {
			if kind == row.Kind {
				known = true
			}
		}
		if !known {
			t.Fatalf("fixture case %q claims the unknown owned family %q; declare the family in artifactOwnedKind before a fixture relies on it", row.Name, row.Kind)
		}
		covered[row.Kind] = true
	}
	for _, kind := range artifactOwnedKinds {
		if !covered[kind] {
			t.Fatalf("no fixture case exercises the %q family, so nothing checks what a publication may write or retire there", kind)
		}
	}
	if !refused {
		t.Fatal("no fixture case is refused; with every case owned, a rule that claimed the whole directory would pass and the user's own files would be prunable")
	}
	for _, required := range document.RequiredNames {
		if !present[required] {
			t.Fatalf("required fixture case %q is missing; the rule it pins would stop being tested", required)
		}
	}
	return document.Cases
}

// TestArtifactOwnershipIsDecidedByPeasantsOwnNaming pins which members of a
// session directory a publication may write, replace or retire. Ownership is
// read from the NAME, so an output an earlier run wrote and this one no longer
// produces is still the session's own and can be retired, while anything a user
// or another tool left beside the artifact is never touched.
func TestArtifactOwnershipIsDecidedByPeasantsOwnNaming(t *testing.T) {
	const (
		sessionID = SessionID("11111111-1111-4111-8111-111111111111")
		otherID   = "22222222-2222-4222-8222-222222222222"
		directory = "test-host/11111111-1111-4111-8111-111111111111"
	)
	for _, row := range loadArtifactOwnedNameCases(t) {
		t.Run(row.Name, func(t *testing.T) {
			relative := strings.NewReplacer("SESSION_ID", string(sessionID), "OTHER_SESSION_ID", otherID).Replace(row.Relative)
			owned, err := newArtifactOwnedFile(filepath.Join(directory, relative), directory, sessionID)
			if row.Kind == "" {
				if err == nil {
					t.Fatalf("case %q claimed %q as the session's own file (family %q); a publication that owns it may retire it, and this path is not peasant's to remove", row.Name, relative, owned.Kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("case %q was refused as %q's own file: %v; a file peasant itself names must stay prunable, or a retired artifact lives beside the current one forever", row.Name, sessionID, err)
			}
			if owned.Kind != row.Kind {
				t.Fatalf("case %q was classified as the %q family, want %q", row.Name, owned.Kind, row.Kind)
			}
			if owned.Relative != filepath.Clean(relative) {
				t.Fatalf("case %q reported the relative path %q, want %q", row.Name, owned.Relative, filepath.Clean(relative))
			}
		})
	}
}
