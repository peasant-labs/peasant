package e2e

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/project-identity.yaml
var projectIdentityYAML []byte

func TestProjectIdentityFixture(t *testing.T) {
	if _, err := loadProjectIdentityFixtures(); err != nil {
		t.Fatal(err)
	}
}

// projectIdentityDocument is the fixture for
// TestPushProjectIdentityDefaultsRenderOnVillage: named session identifiers
// for a repository with a recognizable git remote and one without, plus the
// label the village is expected to render for the remote.
type projectIdentityDocument struct {
	Cases []projectIdentityCase `yaml:"cases"`
}

type projectIdentityCase struct {
	Name                        string `yaml:"name"`
	RemoteRootSessionID         string `yaml:"remoteRootSessionID"`
	RemoteSubagentSessionID     string `yaml:"remoteSubagentSessionID"`
	NoRemoteRootSessionID       string `yaml:"noRemoteRootSessionID"`
	NoRemoteSubagentSessionID   string `yaml:"noRemoteSubagentSessionID"`
	NoRemoteRecordedCWD         string `yaml:"noRemoteRecordedCWD"`
	Remote                      string `yaml:"remote"`
	ExpectedRemoteLabel         string `yaml:"expectedRemoteLabel"`
	ExpectedNoRemoteDisplayName string `yaml:"expectedNoRemoteDisplayName"`
	ExpectedNoRemoteNameSource  string `yaml:"expectedNoRemoteNameSource"`
}

func loadProjectIdentityFixtures() (projectIdentityDocument, error) {
	var document projectIdentityDocument
	decoder := yaml.NewDecoder(bytes.NewReader(projectIdentityYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode strict project identity corpus: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return document, fmt.Errorf("project identity corpus must contain exactly one YAML document")
	}
	if len(document.Cases) == 0 {
		return document, fmt.Errorf("project identity corpus declares no cases")
	}
	seen := map[string]bool{}
	for index, c := range document.Cases {
		fields := []string{
			c.Name, c.RemoteRootSessionID, c.RemoteSubagentSessionID,
			c.NoRemoteRootSessionID, c.NoRemoteSubagentSessionID, c.NoRemoteRecordedCWD,
			c.Remote, c.ExpectedRemoteLabel,
			c.ExpectedNoRemoteDisplayName, c.ExpectedNoRemoteNameSource,
		}
		for _, value := range fields {
			if strings.TrimSpace(value) == "" {
				return document, fmt.Errorf("project identity case %d has a blank required field", index)
			}
		}
		if seen[c.Name] {
			return document, fmt.Errorf("project identity corpus repeats case name %q", c.Name)
		}
		seen[c.Name] = true
	}
	if !seen["default-fields-render-label-or-canonical-path"] {
		return document, fmt.Errorf("project identity corpus is missing required case default-fields-render-label-or-canonical-path; restore the label-versus-path regression fixture")
	}
	return document, nil
}
