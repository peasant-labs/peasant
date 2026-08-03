package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/annotation_repository_scope.yaml
var annotationRepositoryScopeFixtureData []byte

const annotationRepositoryScopeFixturePath = "internal/push/testdata/annotation_repository_scope.yaml"

// annotationTarget is what a candidate annotation points at. The set is closed
// because each one is a different attribution question, and a repository-scoped
// push answers them differently.
type annotationTarget string

const (
	// targetSelectedSession points at a session the scope admits.
	targetSelectedSession annotationTarget = "selected-session"
	// targetOtherSession points at a session outside the scope.
	targetOtherSession annotationTarget = "other-session"
	// targetScopedProject points at a project identity inside the scope.
	targetScopedProject annotationTarget = "scoped-project"
	// targetOtherProject points at a project identity outside the scope.
	targetOtherProject annotationTarget = "other-project"
	// targetUnattributable points at another annotation: a valid target that is
	// neither a session nor a project, so nothing ties it to a repository.
	targetUnattributable annotationTarget = "unattributable"
)

var allAnnotationTargets = [...]annotationTarget{
	targetSelectedSession, targetOtherSession, targetScopedProject, targetOtherProject, targetUnattributable,
}

// scopeState is whether a repository scope is active for the push under test.
type scopeState string

const (
	scopeActive   scopeState = "repository-scoped"
	scopeInactive scopeState = "selection-only"
)

var allScopeStates = [...]scopeState{scopeActive, scopeInactive}

type annotationScopeDocument struct {
	ExpectedCaseCount int                   `yaml:"expectedCaseCount"`
	Cases             []annotationScopeCase `yaml:"cases"`
}

type annotationScopeCase struct {
	Name      string             `yaml:"name"`
	Scope     scopeState         `yaml:"scope"`
	Published []annotationTarget `yaml:"published"`
	Withheld  []annotationTarget `yaml:"withheld"`
}

func loadAnnotationScopeFixture(data []byte) (annotationScopeDocument, error) {
	var document annotationScopeDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf(
			"annotation repository scope fixture rule failed: typed YAML fields must match the document schema; unknown or "+
				"malformed data invalidates the attribution evidence; where=%s loader=first-document decode; when=test fixture loading; "+
				"impact=what a repository-scoped push publishes cannot be trusted; fix=match the typed schema: %w",
			annotationRepositoryScopeFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return document, fmt.Errorf(
			"annotation repository scope fixture rule failed: exactly one YAML document is allowed; trailing data is silently "+
				"ignored; where=%s loader=end-of-document check; when=test fixture loading; "+
				"impact=what a repository-scoped push publishes cannot be trusted; fix=remove the second document",
			annotationRepositoryScopeFixturePath)
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, fmt.Errorf(
			"annotation repository scope fixture rule failed: declared and actual case counts must match and be non-zero, got "+
				"expectedCaseCount=%d cases=%d; where=%s loader=case-count validation; when=test fixture loading; "+
				"impact=what a repository-scoped push publishes cannot be trusted; fix=set expectedCaseCount to the number of cases present",
			document.ExpectedCaseCount, len(document.Cases), annotationRepositoryScopeFixturePath)
	}
	seen := make(map[string]bool, len(document.Cases))
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, annotationScopeRuleError(index,
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name),
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		if !annotationScopeContains(allScopeStates[:], testCase.Scope) {
			return document, annotationScopeRuleError(index,
				fmt.Sprintf("unsupported scope %q", testCase.Scope),
				"fix=use repository-scoped or selection-only")
		}
		accounted := make(map[annotationTarget]bool, len(allAnnotationTargets))
		for _, group := range [][]annotationTarget{testCase.Published, testCase.Withheld} {
			for _, target := range group {
				if !annotationScopeContains(allAnnotationTargets[:], target) {
					return document, annotationScopeRuleError(index,
						fmt.Sprintf("unknown target %q", target),
						"fix=name one of the candidate annotations the corpus seeds")
				}
				if accounted[target] {
					return document, annotationScopeRuleError(index,
						fmt.Sprintf("target %q appears twice", target),
						"fix=an annotation is either published or withheld, never both and never listed twice")
				}
				accounted[target] = true
			}
		}
		for _, target := range allAnnotationTargets {
			if !accounted[target] {
				return document, annotationScopeRuleError(index,
					fmt.Sprintf("target %q is neither published nor withheld", target),
					"fix=account for every candidate; an unlisted one would let a silent publish pass unnoticed")
			}
		}
		if annotationScopeContains(testCase.Published, targetOtherSession) {
			return document, annotationScopeRuleError(index,
				"a session outside the scope can never be published",
				"fix=the session gate predates this change and must keep excluding it")
		}
	}
	return document, nil
}

func annotationScopeRuleError(index int, what, fix string) error {
	return fmt.Errorf(
		"annotation repository scope fixture rule failed: %s; a malformed case invalidates the attribution evidence; "+
			"where=%s case index %d; when=test fixture loading; impact=what a repository-scoped push publishes cannot be trusted; %s",
		what, annotationRepositoryScopeFixturePath, index, fix)
}

func annotationScopeContains[T comparable](values []T, want T) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// --- loader guards ----------------------------------------------------------

func TestLoadAnnotationScopeFixture_RejectsUnaccountedTarget(t *testing.T) {
	t.Parallel()
	_, err := loadAnnotationScopeFixture([]byte(`expectedCaseCount: 1
cases:
  - name: forgets one
    scope: repository-scoped
    published: [selected-session]
    withheld: [other-session, other-project, unattributable]
`))
	if err == nil || !strings.Contains(err.Error(), "neither published nor withheld") {
		t.Fatalf("error = %v, want rejection of a case that leaves a candidate unaccounted for", err)
	}
}

func TestLoadAnnotationScopeFixture_RejectsPublishingAnotherSession(t *testing.T) {
	t.Parallel()
	_, err := loadAnnotationScopeFixture([]byte(`expectedCaseCount: 1
cases:
  - name: publishes another session
    scope: repository-scoped
    published: [selected-session, other-session, scoped-project, other-project, unattributable]
    withheld: []
`))
	if err == nil || !strings.Contains(err.Error(), "can never be published") {
		t.Fatalf("error = %v, want rejection of a case that would encode a leak as correct", err)
	}
}

// --- the corpus -------------------------------------------------------------

const (
	scopedProjectHash = "1111111111111111111111111111111111111111111111111111111111111111"
	otherProjectHash  = "2222222222222222222222222222222222222222222222222222222222222222"
	selectedSessionID = "session-in-scope"
	otherSessionID    = "session-elsewhere"
	// annotatedAnnotationID is the annotation a review-of-a-review points at.
	annotatedAnnotationID = "33333333-3333-4333-8333-333333333333"
)

// TestPushAnnotationsSelected_RepositoryScopeGatesUnattributable drives the real
// annotation push over every kind of candidate a repository-scoped push can meet.
//
// The session gate alone leaves a hole: an annotation with no target session
// passes it unconditionally, so a hook installed in one repository would publish
// annotations belonging to every other one on each commit. Under a repository
// scope such an annotation is published only when it targets that repository's
// own project identity — the one case where Peasant can actually show it belongs
// to what the user consented to.
func TestPushAnnotationsSelected_RepositoryScopeGatesUnattributable(t *testing.T) {
	t.Parallel()
	document, err := loadAnnotationScopeFixture(annotationRepositoryScopeFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			store := &stubAnnotationStore{rows: annotationScopeCandidates()}

			var received schema.AnnotationPushRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/annotations/manifest" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
					return
				}
				if decodeErr := json.NewDecoder(r.Body).Decode(&received); decodeErr != nil {
					t.Errorf("decode request body: %v", decodeErr)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: len(received.Annotations)})
			}))
			defer srv.Close()

			// The session gate is always on: both cases run under a configured
			// selection, which is what makes the repository scope the only
			// difference between them.
			selection := push.AnnotationSelection{
				SessionIDs: map[string]bool{selectedSessionID: true},
			}
			if testCase.Scope == scopeActive {
				selection.RepositoryProjectHashes = map[string]bool{scopedProjectHash: true}
			}

			client := village.NewVillageClient(srv.URL, testAPIKey, nil)
			summary, pushErr := push.PushAnnotationsSelected(
				context.Background(), client, store, selection, false, push.DefaultConcurrency)
			if pushErr != nil {
				t.Fatalf("PushAnnotationsSelected: %v", pushErr)
			}
			if summary.Total != len(testCase.Published) {
				t.Errorf("published %d annotation(s), want %d", summary.Total, len(testCase.Published))
			}

			published := map[string]bool{}
			for _, annotation := range received.Annotations {
				published[annotation.TypeID] = true
			}
			for _, target := range testCase.Published {
				if !published[string(target)] {
					t.Errorf("the %s annotation must be published; published set: %v", target, sortedTypeIDs(published))
				}
			}
			for _, target := range testCase.Withheld {
				if published[string(target)] {
					t.Errorf("the %s annotation must be withheld; published set: %v", target, sortedTypeIDs(published))
				}
			}
		})
	}
}

// annotationScopeCandidates seeds one annotation per attribution kind, using the
// target's own name as the type id so an assertion failure names the case.
func annotationScopeCandidates() []ingest.AnnotationPushRow {
	selected := selectedSessionID
	other := otherSessionID
	scopedProject := scopedProjectHash
	otherProject := otherProjectHash
	annotated := annotatedAnnotationID
	return []ingest.AnnotationPushRow{
		{
			ID: "annotation-selected-session", TargetKind: schema.TargetSession, SessionID: &selected,
			TypeID: string(targetSelectedSession), Value: "ok", IsPrimary: true, AnnotatorName: "system",
		},
		{
			ID: "annotation-other-session", TargetKind: schema.TargetSession, SessionID: &other,
			TypeID: string(targetOtherSession), Value: "ok", IsPrimary: true, AnnotatorName: "system",
		},
		{
			ID: "annotation-scoped-project", TargetKind: schema.TargetProject, ProjectHash: &scopedProject,
			TypeID: string(targetScopedProject), Value: "ok", IsPrimary: true, AnnotatorName: "system",
		},
		{
			ID: "annotation-other-project", TargetKind: schema.TargetProject, ProjectHash: &otherProject,
			TypeID: string(targetOtherProject), Value: "ok", IsPrimary: true, AnnotatorName: "system",
		},
		{
			ID: "annotation-unattributable", TargetKind: schema.TargetAnnotation, AnnotationID: &annotated,
			TypeID: string(targetUnattributable), Value: "ok", IsPrimary: true, AnnotatorName: "system",
		},
	}
}

func sortedTypeIDs(set map[string]bool) []string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
