package e2e

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/publication-convergence.yaml
var publicationConvergenceYAML []byte

type publicationConvergenceDocument struct {
	ExpectedCaseCount int                          `yaml:"expectedCaseCount"`
	Cases             []publicationConvergenceCase `yaml:"cases"`
}

type publicationConvergenceCase struct {
	Name                       string                        `yaml:"name"`
	ControlRootSessionID       string                        `yaml:"controlRootSessionID"`
	ControlSubagentSessionID   string                        `yaml:"controlSubagentSessionID"`
	TargetSessionID            string                        `yaml:"targetSessionID"`
	InitialLicense             schema.License                `yaml:"initialLicense"`
	ChangedLicense             schema.License                `yaml:"changedLicense"`
	Visibility                 schema.Visibility             `yaml:"visibility"`
	OriginalContent            string                        `yaml:"originalContent"`
	ChangedContent             string                        `yaml:"changedContent"`
	OriginalMetadata           string                        `yaml:"originalMetadata"`
	ChangedMetadata            string                        `yaml:"changedMetadata"`
	ExpectedProjects           int                           `yaml:"expectedProjects"`
	ExpectedSessionsPerProject int                           `yaml:"expectedSessionsPerProject"`
	ExpectedFailureStage       store.PublicationAttemptStage `yaml:"expectedFailureStage"`
	FailureOutputContains      []string                      `yaml:"failureOutputContains"`
	DiagnosticContains         []string                      `yaml:"diagnosticContains"`
	ExpectedAssociations       []schema.PublishedAssociation `yaml:"expectedAssociations"`
}

func loadPublicationConvergenceFixtures() (publicationConvergenceDocument, error) {
	var document publicationConvergenceDocument
	decoder := yaml.NewDecoder(bytes.NewReader(publicationConvergenceYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode strict publication convergence corpus: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return document, fmt.Errorf("publication convergence corpus must contain exactly one YAML document")
	}
	if document.ExpectedCaseCount < 1 || len(document.Cases) != document.ExpectedCaseCount {
		return document, fmt.Errorf("publication convergence corpus row count = %d, want declared non-zero count %d", len(document.Cases), document.ExpectedCaseCount)
	}
	seen := map[string]bool{}
	for index, c := range document.Cases {
		fields := []string{c.Name, c.ControlRootSessionID, c.ControlSubagentSessionID, c.TargetSessionID, c.InitialLicense.String(), c.ChangedLicense.String(), c.Visibility.String(), c.OriginalContent, c.ChangedContent, c.OriginalMetadata, c.ChangedMetadata, string(c.ExpectedFailureStage)}
		for _, value := range fields {
			if strings.TrimSpace(value) == "" {
				return document, fmt.Errorf("publication convergence case %d has a blank required field", index)
			}
		}
		if seen[c.Name] {
			return document, fmt.Errorf("publication convergence case name %q is duplicated", c.Name)
		}
		seen[c.Name] = true
		if !c.InitialLicense.IsValid() || !c.ChangedLicense.IsValid() || c.InitialLicense == c.ChangedLicense {
			return document, fmt.Errorf("publication convergence case %q needs two distinct canonical licenses", c.Name)
		}
		if c.Visibility != schema.VisibilityPublic || c.ExpectedFailureStage != store.PublicationAttemptStagePersistence {
			return document, fmt.Errorf("publication convergence case %q must exercise public visibility and the typed persistence-failure arm", c.Name)
		}
		if c.ExpectedProjects < 2 || c.ExpectedSessionsPerProject < 1 || len(c.FailureOutputContains) < 3 || len(c.DiagnosticContains) < 2 || len(c.ExpectedAssociations) < 2 {
			return document, fmt.Errorf("publication convergence case %q does not activate every convergence arm", c.Name)
		}
		associationIDs, hashes := map[string]bool{}, map[string]bool{}
		for associationIndex, association := range c.ExpectedAssociations {
			if err := association.Validate(); err != nil {
				return document, fmt.Errorf("publication convergence case %q association: %w", c.Name, err)
			}
			if associationIDs[association.ID.String()] || hashes[association.ObservedCommitHash] || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(association.ObservedCommitHash) {
				return document, fmt.Errorf("publication convergence case %q association identities/hashes must be unique valid commit bindings", c.Name)
			}
			associationIDs[association.ID.String()], hashes[association.ObservedCommitHash] = true, true
			if associationIndex > 0 {
				previous := c.ExpectedAssociations[associationIndex-1]
				if previous.ID.String() > association.ID.String() || (previous.ID == association.ID && previous.ObservedCommitHash > association.ObservedCommitHash) {
					return document, fmt.Errorf("publication convergence case %q associations are not in deterministic identity/binding order", c.Name)
				}
			}
		}
		for _, values := range [][]string{c.FailureOutputContains, c.DiagnosticContains} {
			for _, value := range values {
				if strings.TrimSpace(value) == "" {
					return document, fmt.Errorf("publication convergence case %q contains a blank assertion needle", c.Name)
				}
			}
		}
	}
	return document, nil
}

func TestPublicationConvergenceCorpus(t *testing.T) {
	document, err := loadPublicationConvergenceFixtures()
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Cases) != 1 {
		t.Fatalf("focused convergence corpus has %d rows, want exactly one", len(document.Cases))
	}
}

func TestPublicationConvergenceMarkersMatchClaudeFixture(t *testing.T) {
	document, err := loadPublicationConvergenceFixtures()
	if err != nil {
		t.Fatal(err)
	}
	want := document.Cases[0]
	var source strings.Builder
	err = filepath.WalkDir("testdata/claude-fixture", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return walkErr
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		source.Write(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("read synthetic Claude fixture: %v", err)
	}
	assertConvergenceMarkerCount(t, source.String(), want.OriginalContent, 1)
	assertConvergenceMarkerCount(t, source.String(), want.OriginalMetadata, 1)
	assertConvergenceMarkerCount(t, source.String(), want.ChangedContent, 0)
	assertConvergenceMarkerCount(t, source.String(), want.ChangedMetadata, 0)
}

func assertConvergenceMarkerCount(t *testing.T, source, marker string, expected int) {
	t.Helper()
	if count := strings.Count(source, marker); count != expected {
		t.Fatalf("synthetic Claude fixture marker %q count = %d, want %d", marker, count, expected)
	}
}
