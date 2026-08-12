// Package testfixture loads the shared effective-project projection corpus.
package testfixture

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"gopkg.in/yaml.v3"
)

const (
	expectedProjectionRows = 16
	expectedGateRows       = 12
)

//go:embed testdata/effective_projects.yaml
var effectiveProjectsYAML []byte

// EffectiveProjectFixtures is the complete projection and commit-gate corpus.
type EffectiveProjectFixtures struct {
	DeclaredProjectionRows int              `yaml:"declared_projection_rows"`
	DeclaredGateRows       int              `yaml:"declared_gate_rows"`
	ProjectionCases        []ProjectionCase `yaml:"projection_cases"`
	GateCases              []GateCase       `yaml:"gate_cases"`
}

// ProjectionCase is one complete selection, candidate cohort, and projection
// expectation.
type ProjectionCase struct {
	Name       string                   `yaml:"name"`
	Selection  config.SelectionConfig   `yaml:"selection"`
	Candidates []ProjectCandidate       `yaml:"candidates"`
	Expected   []EffectiveProjectResult `yaml:"expected"`
}

// ProjectCandidate is the fixture spelling of a production project candidate.
type ProjectCandidate struct {
	ParentProjectID     string             `yaml:"parent_project_id"`
	Harness             string             `yaml:"harness"`
	GitRemote           string             `yaml:"git_remote"`
	ProjectName         string             `yaml:"project_name"`
	ClonePath           string             `yaml:"clone_path"`
	RepositoryCohortKey string             `yaml:"repository_cohort_key"`
	Descendants         []SessionCandidate `yaml:"descendants"`
}

// SessionCandidate is the fixture spelling of one available descendant.
type SessionCandidate struct {
	SessionID           string `yaml:"session_id"`
	Branch              string `yaml:"branch"`
	ParentSessionID     string `yaml:"parent_session_id"`
	ClonePath           string `yaml:"clone_path"`
	RepositoryCohortKey string `yaml:"repository_cohort_key"`
}

// ProjectAdmission is the closed fixture spelling of a production admission.
type ProjectAdmission string

const (
	ProjectAdmissionWholeProject     ProjectAdmission = "whole_project"
	ProjectAdmissionBranch           ProjectAdmission = "branch"
	ProjectAdmissionExplicitSession  ProjectAdmission = "explicit_session"
	ProjectAdmissionNestedDescendant ProjectAdmission = "nested_descendant"
)

// EffectiveProjectResult is one expected output row.
type EffectiveProjectResult struct {
	ParentProjectID         string           `yaml:"parent_project_id"`
	Admission               ProjectAdmission `yaml:"admission"`
	SelectedDescendantCount int              `yaml:"selected_descendant_count"`
}

// GateExpectation is the closed fixture spelling of the later commit-gate
// decision. It deliberately does not depend on that consumer's package.
type GateExpectation string

const (
	GateExpectationNone              GateExpectation = "none"
	GateExpectationConfirmNoProjects GateExpectation = "confirm_no_projects"
)

// GateCase reuses one projection case as commit-gate input.
type GateCase struct {
	Name           string          `yaml:"name"`
	ProjectionCase string          `yaml:"projection_case"`
	Expected       GateExpectation `yaml:"expected"`
}

// LoadEffectiveProjectFixtures strictly decodes and validates the shared
// effective-project fixture corpus.
func LoadEffectiveProjectFixtures() (EffectiveProjectFixtures, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(effectiveProjectsYAML))
	decoder.KnownFields(true)

	var fixtures EffectiveProjectFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		return EffectiveProjectFixtures{}, fmt.Errorf("decode effective-project fixtures: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return EffectiveProjectFixtures{}, fmt.Errorf("decode effective-project fixtures: expected exactly one YAML document: %w", err)
	}
	if fixtures.DeclaredProjectionRows != expectedProjectionRows || len(fixtures.ProjectionCases) != expectedProjectionRows {
		return EffectiveProjectFixtures{}, fmt.Errorf(
			"validate effective-project fixture row guard: declared projection rows=%d, actual=%d, required=%d",
			fixtures.DeclaredProjectionRows,
			len(fixtures.ProjectionCases),
			expectedProjectionRows,
		)
	}
	if fixtures.DeclaredGateRows != expectedGateRows || len(fixtures.GateCases) != expectedGateRows {
		return EffectiveProjectFixtures{}, fmt.Errorf(
			"validate effective-project fixture row guard: declared gate rows=%d, actual=%d, required=%d",
			fixtures.DeclaredGateRows,
			len(fixtures.GateCases),
			expectedGateRows,
		)
	}
	if err := fixtures.validate(); err != nil {
		return EffectiveProjectFixtures{}, err
	}
	return fixtures, nil
}

// ProjectionCaseByName returns the projection case with the stable fixture
// name. Commit-gate tests use this to share the same input rows.
func (f EffectiveProjectFixtures) ProjectionCaseByName(name string) (ProjectionCase, bool) {
	for _, candidate := range f.ProjectionCases {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return ProjectionCase{}, false
}

// ProductionCandidates converts trusted, validated fixture rows to the exact
// projection carrier.
func (c ProjectionCase) ProductionCandidates() ([]selectionprojection.ProjectCandidate, error) {
	result := make([]selectionprojection.ProjectCandidate, len(c.Candidates))
	for projectIndex, project := range c.Candidates {
		descendants := make([]selectionprojection.SessionCandidate, len(project.Descendants))
		for sessionIndex, session := range project.Descendants {
			sessionID, err := ingest.NewSessionID(session.SessionID)
			if err != nil {
				return nil, fmt.Errorf("projection case %q candidate %q has invalid session_id %q: %w", c.Name, project.ParentProjectID, session.SessionID, err)
			}
			var parentSessionID ingest.SessionID
			if session.ParentSessionID != "" {
				parentSessionID, err = ingest.NewSessionID(session.ParentSessionID)
				if err != nil {
					return nil, fmt.Errorf("projection case %q candidate %q has invalid parent_session_id %q: %w", c.Name, project.ParentProjectID, session.ParentSessionID, err)
				}
			}
			descendants[sessionIndex] = selectionprojection.SessionCandidate{
				SessionID:           sessionID,
				Branch:              session.Branch,
				ParentSessionID:     parentSessionID,
				ClonePath:           ingest.ClonePath(session.ClonePath),
				RepositoryCohortKey: ingest.RepositoryCohortKey(session.RepositoryCohortKey),
			}
		}
		result[projectIndex] = selectionprojection.ProjectCandidate{
			ParentProjectID:     selectionprojection.ParentProjectID(project.ParentProjectID),
			Harness:             ingest.Harness(project.Harness),
			GitRemote:           project.GitRemote,
			ProjectName:         project.ProjectName,
			ClonePath:           ingest.ClonePath(project.ClonePath),
			RepositoryCohortKey: ingest.RepositoryCohortKey(project.RepositoryCohortKey),
			Descendants:         descendants,
		}
	}
	return result, nil
}

// ExpectedProjects expands expected fixture rows into full production results
// by attaching the input candidate with the matching parent-project identity.
func (c ProjectionCase) ExpectedProjects() ([]selectionprojection.EffectiveProject, error) {
	candidates, err := c.ProductionCandidates()
	if err != nil {
		return nil, err
	}
	byParent := make(map[selectionprojection.ParentProjectID]selectionprojection.ProjectCandidate, len(candidates))
	for _, candidate := range candidates {
		byParent[candidate.ParentProjectID] = candidate
	}
	result := make([]selectionprojection.EffectiveProject, len(c.Expected))
	for index, expected := range c.Expected {
		parentID := selectionprojection.ParentProjectID(expected.ParentProjectID)
		candidate, ok := byParent[parentID]
		if !ok {
			return nil, fmt.Errorf("projection case %q expects unknown parent_project_id %q", c.Name, expected.ParentProjectID)
		}
		admission, err := expected.Admission.ProductionAdmission()
		if err != nil {
			return nil, fmt.Errorf("projection case %q: %w", c.Name, err)
		}
		result[index] = selectionprojection.EffectiveProject{
			Candidate:               candidate,
			Admission:               admission,
			SelectedDescendantCount: expected.SelectedDescendantCount,
		}
	}
	return result, nil
}

// ProductionAdmission converts a validated fixture token to the production
// enum.
func (a ProjectAdmission) ProductionAdmission() (selectionprojection.ProjectAdmission, error) {
	switch a {
	case ProjectAdmissionWholeProject:
		return selectionprojection.ProjectEffectiveWholeProject, nil
	case ProjectAdmissionBranch:
		return selectionprojection.ProjectEffectiveBranch, nil
	case ProjectAdmissionExplicitSession:
		return selectionprojection.ProjectEffectiveExplicitSession, nil
	case ProjectAdmissionNestedDescendant:
		return selectionprojection.ProjectEffectiveNestedDescendant, nil
	default:
		return selectionprojection.ProjectNotEffective, fmt.Errorf("unknown project admission %q", a)
	}
}

func (f EffectiveProjectFixtures) validate() error {
	projectionCases := make(map[string]ProjectionCase, len(f.ProjectionCases))
	for _, projectionCase := range f.ProjectionCases {
		if strings.TrimSpace(projectionCase.Name) == "" {
			return errors.New("validate effective-project fixtures: projection case name is empty")
		}
		if _, duplicate := projectionCases[projectionCase.Name]; duplicate {
			return fmt.Errorf("validate effective-project fixtures: duplicate projection case %q", projectionCase.Name)
		}
		projectionCases[projectionCase.Name] = projectionCase
		if !projectionCase.Selection.Mode.IsValid() {
			return fmt.Errorf("validate effective-project fixture %q: unknown selection mode %q", projectionCase.Name, projectionCase.Selection.Mode)
		}
		if err := projectionCase.validate(); err != nil {
			return err
		}
	}

	gateNames := make(map[string]struct{}, len(f.GateCases))
	gateProjectionCases := make(map[string]string, len(f.GateCases))
	for _, gateCase := range f.GateCases {
		if strings.TrimSpace(gateCase.Name) == "" {
			return errors.New("validate effective-project fixtures: gate case name is empty")
		}
		if _, duplicate := gateNames[gateCase.Name]; duplicate {
			return fmt.Errorf("validate effective-project fixtures: duplicate gate case %q", gateCase.Name)
		}
		gateNames[gateCase.Name] = struct{}{}
		projectionCase, ok := projectionCases[gateCase.ProjectionCase]
		if !ok {
			return fmt.Errorf("validate effective-project gate fixture %q: unknown projection_case %q", gateCase.Name, gateCase.ProjectionCase)
		}
		if firstGate, duplicate := gateProjectionCases[gateCase.ProjectionCase]; duplicate {
			return fmt.Errorf(
				"validate effective-project gate fixture %q: projection_case %q is already used by gate case %q",
				gateCase.Name,
				gateCase.ProjectionCase,
				firstGate,
			)
		}
		gateProjectionCases[gateCase.ProjectionCase] = gateCase.Name
		switch gateCase.Expected {
		case GateExpectationNone:
			if len(projectionCase.Expected) == 0 {
				return fmt.Errorf(
					"validate effective-project gate fixture %q: expected %q but projection_case %q has no effective projects",
					gateCase.Name,
					gateCase.Expected,
					gateCase.ProjectionCase,
				)
			}
		case GateExpectationConfirmNoProjects:
			if len(projectionCase.Expected) != 0 {
				return fmt.Errorf(
					"validate effective-project gate fixture %q: expected %q but projection_case %q has %d effective projects",
					gateCase.Name,
					gateCase.Expected,
					gateCase.ProjectionCase,
					len(projectionCase.Expected),
				)
			}
		default:
			return fmt.Errorf("validate effective-project gate fixture %q: unknown expected decision %q", gateCase.Name, gateCase.Expected)
		}
	}
	return nil
}

func (c ProjectionCase) validate() error {
	if err := c.validateSelection(); err != nil {
		return err
	}
	candidateIDs := make(map[string]int, len(c.Candidates))
	allSessionIDs := make(map[string]string)
	for index, candidate := range c.Candidates {
		if strings.TrimSpace(candidate.ParentProjectID) == "" {
			return fmt.Errorf("validate effective-project fixture %q: candidate %d has empty parent_project_id", c.Name, index)
		}
		if _, duplicate := candidateIDs[candidate.ParentProjectID]; duplicate {
			return fmt.Errorf("validate effective-project fixture %q: duplicate parent_project_id %q", c.Name, candidate.ParentProjectID)
		}
		candidateIDs[candidate.ParentProjectID] = len(candidate.Descendants)
		if !knownHarness(candidate.Harness) {
			return fmt.Errorf("validate effective-project fixture %q candidate %q: unknown harness %q", c.Name, candidate.ParentProjectID, candidate.Harness)
		}
		if err := validateClonePath(c.Name, candidate.ParentProjectID, "clone_path", candidate.ClonePath); err != nil {
			return err
		}
		if strings.TrimSpace(candidate.RepositoryCohortKey) != candidate.RepositoryCohortKey {
			return fmt.Errorf("validate effective-project fixture %q candidate %q: repository_cohort_key has surrounding whitespace", c.Name, candidate.ParentProjectID)
		}
		projectSessionIDs := make(map[string]struct{}, len(candidate.Descendants))
		for sessionIndex, session := range candidate.Descendants {
			if strings.TrimSpace(session.SessionID) == "" {
				return fmt.Errorf("validate effective-project fixture %q candidate %q: descendant %d has empty session_id", c.Name, candidate.ParentProjectID, sessionIndex)
			}
			if owner, duplicate := allSessionIDs[session.SessionID]; duplicate {
				return fmt.Errorf("validate effective-project fixture %q: session_id %q appears under both %q and %q", c.Name, session.SessionID, owner, candidate.ParentProjectID)
			}
			allSessionIDs[session.SessionID] = candidate.ParentProjectID
			projectSessionIDs[session.SessionID] = struct{}{}
			if err := validateClonePath(c.Name, candidate.ParentProjectID, "descendant clone_path", session.ClonePath); err != nil {
				return err
			}
			if strings.TrimSpace(session.RepositoryCohortKey) != session.RepositoryCohortKey {
				return fmt.Errorf("validate effective-project fixture %q candidate %q: descendant repository_cohort_key has surrounding whitespace", c.Name, candidate.ParentProjectID)
			}
		}
		for _, session := range candidate.Descendants {
			if session.ParentSessionID == "" {
				continue
			}
			if _, ok := projectSessionIDs[session.ParentSessionID]; !ok {
				return fmt.Errorf(
					"validate effective-project fixture %q candidate %q: nested session %q names unavailable parent_session_id %q; include the stored parent in the complete project cohort",
					c.Name,
					candidate.ParentProjectID,
					session.SessionID,
					session.ParentSessionID,
				)
			}
		}
	}
	if _, err := c.ProductionCandidates(); err != nil {
		return err
	}

	expectedIDs := make(map[string]struct{}, len(c.Expected))
	for _, expected := range c.Expected {
		descendantCount, ok := candidateIDs[expected.ParentProjectID]
		if !ok {
			return fmt.Errorf("validate effective-project fixture %q: expected row names unknown parent_project_id %q", c.Name, expected.ParentProjectID)
		}
		if _, duplicate := expectedIDs[expected.ParentProjectID]; duplicate {
			return fmt.Errorf("validate effective-project fixture %q: duplicate expected parent_project_id %q", c.Name, expected.ParentProjectID)
		}
		expectedIDs[expected.ParentProjectID] = struct{}{}
		if _, err := expected.Admission.ProductionAdmission(); err != nil {
			return fmt.Errorf("validate effective-project fixture %q: %w", c.Name, err)
		}
		if expected.SelectedDescendantCount < 0 || expected.SelectedDescendantCount > descendantCount {
			return fmt.Errorf(
				"validate effective-project fixture %q expected project %q: selected_descendant_count=%d is outside available range 0..%d",
				c.Name,
				expected.ParentProjectID,
				expected.SelectedDescendantCount,
				descendantCount,
			)
		}
	}
	return nil
}

func (c ProjectionCase) validateSelection() error {
	for harness, selected := range c.Selection.Harnesses {
		if !knownHarness(harness) {
			return fmt.Errorf("validate effective-project fixture %q selection: unknown harness %q", c.Name, harness)
		}
		for _, sessionID := range selected.Sessions {
			if _, err := ingest.NewSessionID(sessionID); err != nil {
				return fmt.Errorf("validate effective-project fixture %q selection: invalid session ID %q for harness %q: %w", c.Name, sessionID, harness, err)
			}
		}
		for projectIndex, project := range selected.Projects {
			if strings.TrimSpace(project.GitRemote) == "" && strings.TrimSpace(project.Name) == "" && len(project.ClonePaths) == 0 {
				return fmt.Errorf("validate effective-project fixture %q selection: project %d for harness %q has no remote, name, or clone path", c.Name, projectIndex, harness)
			}
			for _, clonePath := range project.ClonePaths {
				if !filepath.IsAbs(clonePath) || filepath.Clean(clonePath) != clonePath {
					return fmt.Errorf("validate effective-project fixture %q selection: clone path %q for harness %q is not a clean absolute physical path", c.Name, clonePath, harness)
				}
			}
		}
	}
	return nil
}

func knownHarness(raw string) bool {
	for _, harness := range ingest.AllHarnesses {
		if harness.String() == raw {
			return true
		}
	}
	return false
}

func validateClonePath(caseName, parentID, field, raw string) error {
	if raw == "" {
		return nil
	}
	if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return fmt.Errorf("validate effective-project fixture %q candidate %q: %s %q is not a clean absolute physical path", caseName, parentID, field, raw)
	}
	return nil
}
