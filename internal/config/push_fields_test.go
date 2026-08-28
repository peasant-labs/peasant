package config

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/push_fields.yaml
var pushFieldsYAML []byte

//go:embed testdata/push_fields.manifest.yaml
var pushFieldsManifestYAML []byte

// pushFieldsCase is one row of testdata/push_fields.yaml: a config document
// and the tri-state Resolve() outcome it must produce for GitRemote,
// ProjectPath, and ProjectName (D8).
type pushFieldsCase struct {
	Name            string `yaml:"name"`
	Why             string `yaml:"why"`
	YAML            string `yaml:"yaml"`
	WantGitRemote   bool   `yaml:"wantGitRemote"`
	WantProjectPath bool   `yaml:"wantProjectPath"`
	WantProjectName bool   `yaml:"wantProjectName"`
}

type pushFieldsFixture struct {
	Cases []pushFieldsCase `yaml:"cases"`
}

func loadPushFieldsFixture(t *testing.T) pushFieldsFixture {
	t.Helper()
	var fixture pushFieldsFixture
	decoder := yaml.NewDecoder(bytes.NewReader(pushFieldsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode push_fields.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("push_fields.yaml must contain exactly one YAML document: %v", err)
	}
	for i, c := range fixture.Cases {
		if c.Name == "" {
			t.Fatalf("push_fields.yaml case %d has no name", i)
		}
		if c.Why == "" {
			t.Fatalf("push_fields.yaml case %q has no why", c.Name)
		}
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pushFieldsManifestYAML, "push_fields")
	if err != nil {
		t.Fatalf("decode push_fields.manifest.yaml: %v", err)
	}
	names := make([]string, len(fixture.Cases))
	for i, c := range fixture.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "push_fields"); err != nil {
		t.Fatalf("push_fields.yaml fixture/manifest mismatch: %v", err)
	}
	return fixture
}

// TestPushFieldVisibility_TriStateResolve drives every row of
// testdata/push_fields.yaml through the real production path: Parse (the
// same YAML config loader the CLI uses) followed by Resolve.
func TestPushFieldVisibility_TriStateResolve(t *testing.T) {
	fixture := loadPushFieldsFixture(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			cfg, err := Parse([]byte(c.YAML))
			if err != nil {
				t.Fatalf("Parse: %v (why: %s)", err, c.Why)
			}
			resolved := cfg.Push.Fields.Resolve()
			if resolved.GitRemote != c.WantGitRemote {
				t.Errorf("Resolve().GitRemote = %v, want %v (why: %s)", resolved.GitRemote, c.WantGitRemote, c.Why)
			}
			if resolved.ProjectPath != c.WantProjectPath {
				t.Errorf("Resolve().ProjectPath = %v, want %v (why: %s)", resolved.ProjectPath, c.WantProjectPath, c.Why)
			}
			if resolved.ProjectName != c.WantProjectName {
				t.Errorf("Resolve().ProjectName = %v, want %v (why: %s)", resolved.ProjectName, c.WantProjectName, c.Why)
			}
		})
	}
}
