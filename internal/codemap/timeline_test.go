package codemap_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/timeline_bindings.yaml
var timelineBindingsYAML []byte

type timelineBindingsFixture struct {
	Cases []struct {
		Name                     string          `yaml:"name"`
		ExpectedSessionOrder     []string        `yaml:"expectedSessionOrder"`
		ExpectedHasCommitBinding map[string]bool `yaml:"expectedHasCommitBinding"`
		Commits                  []struct {
			Hash     string   `yaml:"hash"`
			Sessions []string `yaml:"sessions"`
		} `yaml:"commits"`
		UnattachedSessions []string `yaml:"unattachedSessions"`
	} `yaml:"cases"`
}

func decodeTimelineBindingsFixture(data []byte) (timelineBindingsFixture, error) {
	var fixture timelineBindingsFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return timelineBindingsFixture{}, fmt.Errorf("decode timeline binding fixture first document: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return timelineBindingsFixture{}, fmt.Errorf("timeline binding fixture must contain exactly one YAML document: %v", err)
	}
	if len(fixture.Cases) == 0 {
		return timelineBindingsFixture{}, errors.New("timeline binding fixture cases is empty; add at least one authoritative relationship case")
	}
	names := make(map[string]struct{}, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || len(testCase.Commits) == 0 || len(testCase.ExpectedSessionOrder) == 0 {
			return timelineBindingsFixture{}, fmt.Errorf("timeline binding fixture case %q is incomplete", testCase.Name)
		}
		if len(testCase.ExpectedHasCommitBinding) != len(testCase.ExpectedSessionOrder) {
			return timelineBindingsFixture{}, fmt.Errorf("timeline binding fixture case %q must state binding completeness for every normalized session", testCase.Name)
		}
		if _, duplicate := names[testCase.Name]; duplicate {
			return timelineBindingsFixture{}, fmt.Errorf("timeline binding fixture repeats case name %q", testCase.Name)
		}
		names[testCase.Name] = struct{}{}
	}
	return fixture, nil
}

func loadTimelineBindingsFixture(t *testing.T) timelineBindingsFixture {
	t.Helper()
	fixture, err := decodeTimelineBindingsFixture(timelineBindingsYAML)
	if err != nil {
		t.Fatalf("load timeline binding fixture: %v", err)
	}
	return fixture
}

func TestTimelineBindingFixtureLoaderRejectsStructuralDrift(t *testing.T) {
	unknownField := bytes.Replace(timelineBindingsYAML, []byte("expectedSessionOrder:"), []byte("unexpectedField: true\n    expectedSessionOrder:"), 1)
	if _, err := decodeTimelineBindingsFixture(unknownField); err == nil || !strings.Contains(err.Error(), "field unexpectedField not found") {
		t.Fatalf("unknown-field mutation error = %v, want strict known-field rejection", err)
	}
	duplicateName := bytes.Replace(timelineBindingsYAML, []byte("name: authoritative_many_to_many_and_unattached"), []byte("name: duplicate\n  - name: duplicate"), 1)
	if _, err := decodeTimelineBindingsFixture(duplicateName); err == nil {
		t.Fatal("duplicate-name mutation unexpectedly loaded")
	}
	trailingDocument := append(append([]byte{}, timelineBindingsYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeTimelineBindingsFixture(trailingDocument); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document mutation error = %v, want single-document rejection", err)
	}
}

func TestReviewChanges_TimelineBindings(t *testing.T) {
	t.Parallel()
	fixture := loadTimelineBindingsFixture(t)
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			repo := fxStubRepo()
			repo.CommitsByRef[schemaDefaultBranch()] = []gitops.Commit{
				{Hash: fxMergeBase, Subject: "base commit", TimeMs: fxBase()},
				{Hash: fxHashA, Subject: "feature commit", TimeMs: fxBase() - 1},
			}
			svc, store := newFixtureService(t, repo)
			if err := store.UpsertSessionCommits(context.Background(), schema.SessionID(fxSession1), []ingest.CommitInfo{
				{Hash: fxMergeBase, Message: "base commit", CommitTime: fxBase()},
				{Hash: fxHashA, Message: "feature commit", CommitTime: fxBase() - 1},
			}); err != nil {
				t.Fatalf("seed session-one commits: %v", err)
			}
			// Include both a visible binding and an authoritative binding outside
			// the bounded default-branch commit window. The latter marks the
			// normalized session as bound without inventing a RecentCommits edge.
			if err := store.UpsertSessionCommits(context.Background(), schema.SessionID(fxSession2), []ingest.CommitInfo{
				{Hash: fxMergeBase, Message: "base commit", CommitTime: fxBase()},
				{Hash: "bbbb222222222222222222222222222222222222", Message: "older branch commit", CommitTime: fxBase() - 9000},
			}); err != nil {
				t.Fatalf("seed session-two commits: %v", err)
			}
			seedSession(t, store, fxSession3, "", fxBase()-3000, fxBase()-2000)
			seedMetrics(t, store, fxSession3, "Explore without committing", schema.OutcomeResolved, 10, 0, nil)

			payload, err := svc.ReviewChanges(context.Background(), fxProjectHash)
			if err != nil {
				t.Fatalf("ReviewChanges: %v", err)
			}
			if err := payload.Validate(); err != nil {
				t.Fatalf("ReviewListPayload.Validate: %v", err)
			}
			assertCommitRefCoherent(t, payload.RecentCommits)

			commitOrder := make([]string, 0, len(payload.RecentCommits))
			byHash := make(map[string][]string, len(payload.RecentCommits))
			linked := make(map[string]bool)
			for _, commit := range payload.RecentCommits {
				commitOrder = append(commitOrder, commit.Hash)
				ids := make([]string, len(commit.SessionIDs))
				for i, id := range commit.SessionIDs {
					ids[i] = string(id)
					linked[string(id)] = true
				}
				byHash[commit.Hash] = ids
			}
			expectedCommitOrder := make([]string, 0, len(testCase.Commits))
			for _, expected := range testCase.Commits {
				expectedCommitOrder = append(expectedCommitOrder, expected.Hash)
				if got := byHash[expected.Hash]; !reflect.DeepEqual(got, expected.Sessions) {
					t.Errorf("commit %s sessions = %v, want %v", expected.Hash, got, expected.Sessions)
				}
			}
			if !reflect.DeepEqual(commitOrder, expectedCommitOrder) {
				t.Errorf("commit order = %v, want %v", commitOrder, expectedCommitOrder)
			}

			sessionOrder := make([]string, 0, len(payload.Sessions))
			sessionIDs := make(map[string]bool, len(payload.Sessions))
			for _, session := range payload.Sessions {
				id := string(session.SessionID)
				sessionOrder = append(sessionOrder, id)
				sessionIDs[id] = true
				if got, exists := testCase.ExpectedHasCommitBinding[id]; !exists || got != session.HasCommitBinding {
					t.Errorf("session %s hasCommitBinding = %t, want %t (declared=%t)", id, session.HasCommitBinding, got, exists)
				}
			}
			if !reflect.DeepEqual(sessionOrder, testCase.ExpectedSessionOrder) {
				t.Errorf("session order = %v, want %v", sessionOrder, testCase.ExpectedSessionOrder)
			}
			for _, sessionID := range testCase.UnattachedSessions {
				if !sessionIDs[sessionID] {
					t.Errorf("unattached session %s missing from normalized sessions", sessionID)
				}
				if linked[sessionID] {
					t.Errorf("unattached session %s unexpectedly linked to a timeline commit", sessionID)
				}
			}
		})
	}
}

// TestReviewChanges_RewriteLedgerPreservesOriginalAssociation verifies a
// re-ingest changes only the current commit projection. The historical ledger
// still exposes the original opaque ID and observed hash as unresolved, rather
// than silently retargeting it to the replacement commit.
func TestReviewChanges_RewriteLedgerPreservesOriginalAssociation(t *testing.T) {
	t.Parallel()
	svc, store := newFixtureService(t, fxStubRepo())
	ctx := context.Background()
	if err := store.UpsertSessionCommits(ctx, schema.SessionID(fxSession1), []ingest.CommitInfo{{
		Hash:       fxHashA,
		Message:    "original observation",
		AuthorTime: fxBase(),
	}}); err != nil {
		t.Fatalf("seed original commit association: %v", err)
	}
	original, err := store.ListCurrentSessionCommitAssociations(ctx, schema.SessionID(fxSession1))
	if err != nil {
		t.Fatalf("list original associations: %v", err)
	}
	if len(original) != 1 {
		t.Fatalf("original associations = %d, want 1", len(original))
	}
	originalID := original[0].ID

	if err := store.UpsertSessionCommits(ctx, schema.SessionID(fxSession1), []ingest.CommitInfo{{
		Hash:       fxMergeBase,
		Message:    "current observation",
		AuthorTime: fxBase() + 1,
	}}); err != nil {
		t.Fatalf("replace current commit association: %v", err)
	}
	payload, err := svc.ReviewChanges(ctx, fxProjectHash)
	if err != nil {
		t.Fatalf("ReviewChanges: %v", err)
	}
	if err := payload.Validate(); err != nil {
		t.Fatalf("ReviewListPayload.Validate: %v", err)
	}

	var historical *schema.RewrittenCommit
	for index := range payload.RewrittenCommits {
		if payload.RewrittenCommits[index].GhostHash == fxHashA {
			historical = &payload.RewrittenCommits[index]
			break
		}
	}
	if historical == nil {
		t.Fatalf("rewrite ledger omitted original hash %q: %+v", fxHashA, payload.RewrittenCommits)
	}
	if historical.Resolution != schema.RewriteResolutionUnresolved || historical.Method != schema.RewriteMethodNone {
		t.Errorf("historical rewrite resolution/method = %q/%q, want unresolved/none", historical.Resolution, historical.Method)
	}
	if len(historical.Associations) != 1 {
		t.Fatalf("historical associations = %d, want 1", len(historical.Associations))
	}
	association := historical.Associations[0]
	if association.ID != originalID {
		t.Errorf("historical association ID = %q, want original %q", association.ID, originalID)
	}
	if len(association.Evidence) != 1 || association.Evidence[0].RecordedCommitHash == nil || *association.Evidence[0].RecordedCommitHash != fxHashA {
		t.Errorf("historical association evidence = %+v, want original observed hash %q", association.Evidence, fxHashA)
	}
}

func schemaDefaultBranch() string { return "main" }
