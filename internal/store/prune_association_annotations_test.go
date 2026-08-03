package store_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/prune/association_annotations.yaml
var pruneAssociationAnnotationFixtureData []byte

type pruneAssociationAnnotationFixtures struct {
	Cases []pruneAssociationAnnotationFixture `yaml:"cases"`
}

type pruneAssociationAnnotationFixture struct {
	Name                    string             `yaml:"name"`
	SelectedSessionID       ingest.SessionID   `yaml:"selectedSessionID"`
	RetainedSessionID       ingest.SessionID   `yaml:"retainedSessionID"`
	SelectedProjectHash     ingest.ProjectHash `yaml:"selectedProjectHash"`
	RetainedProjectHash     ingest.ProjectHash `yaml:"retainedProjectHash"`
	SelectedCommitHash      string             `yaml:"selectedCommitHash"`
	RetainedCommitHash      string             `yaml:"retainedCommitHash"`
	SelectedAnnotationValue string             `yaml:"selectedAnnotationValue"`
	RetainedAnnotationValue string             `yaml:"retainedAnnotationValue"`
	SelectedMetaValue       string             `yaml:"selectedMetaValue"`
	SelectedNestedMetaValue string             `yaml:"selectedNestedMetaValue"`
	RetainedMetaValue       string             `yaml:"retainedMetaValue"`
	RetainedNestedMetaValue string             `yaml:"retainedNestedMetaValue"`
	ExpectedDeleted         int                `yaml:"expectedDeleted"`
}

func loadPruneAssociationAnnotationFixture(data []byte) (pruneAssociationAnnotationFixture, error) {
	var fixtures pruneAssociationAnnotationFixtures
	const fixturePath = "internal/store/testdata/prune/association_annotations.yaml"

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		return pruneAssociationAnnotationFixture{}, fmt.Errorf("decode committed fixture %s: %w; fix the YAML schema or remove unknown fields", fixturePath, err)
	}
	var trailing any
	switch err := decoder.Decode(&trailing); err {
	case io.EOF:
	case nil:
		return pruneAssociationAnnotationFixture{}, fmt.Errorf("committed fixture %s contains a trailing YAML document; remove the extra document so the fixture contains exactly one YAML document", fixturePath)
	default:
		return pruneAssociationAnnotationFixture{}, fmt.Errorf("decode trailing YAML content in committed fixture %s: %w; remove or repair the trailing YAML document", fixturePath, err)
	}
	if len(fixtures.Cases) != 1 {
		return pruneAssociationAnnotationFixture{}, fmt.Errorf("committed fixture %s defines %d cases, want exactly one pruning scenario; remove extra cases from the fixture", fixturePath, len(fixtures.Cases))
	}
	fixture := fixtures.Cases[0]
	if fixture.Name == "" || fixture.SelectedSessionID == "" || fixture.RetainedSessionID == "" ||
		fixture.SelectedProjectHash == "" || fixture.RetainedProjectHash == "" ||
		fixture.SelectedCommitHash == "" || fixture.RetainedCommitHash == "" ||
		fixture.SelectedAnnotationValue == "" || fixture.RetainedAnnotationValue == "" ||
		fixture.SelectedMetaValue == "" || fixture.SelectedNestedMetaValue == "" || fixture.RetainedMetaValue == "" || fixture.RetainedNestedMetaValue == "" {
		return pruneAssociationAnnotationFixture{}, fmt.Errorf("committed fixture %s has an incomplete scenario; populate all IDs, hashes, commit hashes, annotation values, and meta values", fixturePath)
	}
	if fixture.SelectedSessionID == fixture.RetainedSessionID || fixture.SelectedProjectHash == fixture.RetainedProjectHash || fixture.ExpectedDeleted != 1 {
		return pruneAssociationAnnotationFixture{}, fmt.Errorf("committed fixture %s must define distinct selected/retained records and expectedDeleted=1; update the fixture values", fixturePath)
	}
	return fixture, nil
}

func TestLoadPruneAssociationAnnotationFixture_RejectsUnknownField(t *testing.T) {
	t.Parallel()

	data := append([]byte(nil), pruneAssociationAnnotationFixtureData...)
	data = append(data, []byte("\nunknownFixtureField: true\n")...)

	_, err := loadPruneAssociationAnnotationFixture(data)
	if err == nil {
		t.Fatal("loadPruneAssociationAnnotationFixture accepted an unknown fixture field")
	}
	if !strings.Contains(err.Error(), "unknownFixtureField") {
		t.Fatalf("loadPruneAssociationAnnotationFixture error = %q, want unknown field name", err)
	}
}

func TestLoadPruneAssociationAnnotationFixture_RejectsTrailingDocument(t *testing.T) {
	t.Parallel()

	data := append([]byte(nil), pruneAssociationAnnotationFixtureData...)
	data = append(data, []byte("\n---\n{}\n")...)

	_, err := loadPruneAssociationAnnotationFixture(data)
	if err == nil {
		t.Fatal("loadPruneAssociationAnnotationFixture accepted a trailing YAML document")
	}
	if !strings.Contains(err.Error(), "trailing YAML document") {
		t.Fatalf("loadPruneAssociationAnnotationFixture error = %q, want trailing YAML document", err)
	}
}

func associationIDForPruneSession(t *testing.T, s *store.Store, sessionID ingest.SessionID) schema.AssociationID {
	t.Helper()
	associations, err := s.ListCurrentSessionCommitAssociations(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListCurrentSessionCommitAssociations(%s): %v", sessionID, err)
	}
	if len(associations) != 1 {
		t.Fatalf("ListCurrentSessionCommitAssociations(%s) returned %d rows, want 1", sessionID, len(associations))
	}
	return associations[0].ID
}

func TestStore_PruneSessions_AssociationAnnotationCleanup(t *testing.T) {
	t.Parallel()
	fixture, err := loadPruneAssociationAnnotationFixture(pruneAssociationAnnotationFixtureData)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	s := openTestStore(t)
	seedPruneSession(t, s, string(fixture.SelectedSessionID), string(fixture.SelectedProjectHash), defaults.HarnessClaudeCode, 1700000000000)
	seedPruneSession(t, s, string(fixture.RetainedSessionID), string(fixture.RetainedProjectHash), defaults.HarnessClaudeCode, 1700100000000)

	if err := s.UpsertSessionCommits(ctx, fixture.SelectedSessionID, []ingest.CommitInfo{{
		Hash: fixture.SelectedCommitHash,
	}}); err != nil {
		t.Fatalf("UpsertSessionCommits(selected): %v", err)
	}
	if err := s.UpsertSessionCommits(ctx, fixture.RetainedSessionID, []ingest.CommitInfo{{
		Hash: fixture.RetainedCommitHash,
	}}); err != nil {
		t.Fatalf("UpsertSessionCommits(retained): %v", err)
	}

	selectedAssociationID := associationIDForPruneSession(t, s, fixture.SelectedSessionID)
	retainedAssociationID := associationIDForPruneSession(t, s, fixture.RetainedSessionID)

	annotator, err := s.GetAnnotator(ctx, "outcome-classifier")
	if err != nil || annotator == nil {
		t.Fatalf("GetAnnotator(outcome-classifier): %v", err)
	}
	associationType, err := s.GetAnnotationTypeByTypeID(ctx, testutil.TestTypeIDSessionOutcome)
	if err != nil || associationType == nil {
		t.Fatalf("GetAnnotationTypeByTypeID(%s): %v", testutil.TestTypeIDSessionOutcome, err)
	}
	metaType, err := s.GetAnnotationTypeByTypeID(ctx, testutil.TestTypeIDSessionApproval)
	if err != nil || metaType == nil {
		t.Fatalf("GetAnnotationTypeByTypeID(%s): %v", testutil.TestTypeIDSessionApproval, err)
	}

	selectedAnnotationID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		AssociationID:    &selectedAssociationID,
		AnnotatorID:      annotator.ID,
		AnnotationTypeID: associationType.ID,
		Value:            fixture.SelectedAnnotationValue,
	})
	if err != nil {
		t.Fatalf("CreateAnnotation(selected association): %v", err)
	}
	selectedMetaAnnotationID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		AnnotationID:     &selectedAnnotationID,
		AnnotatorID:      annotator.ID,
		AnnotationTypeID: metaType.ID,
		Value:            fixture.SelectedMetaValue,
	})
	if err != nil {
		t.Fatalf("CreateAnnotation(selected meta): %v", err)
	}
	selectedNestedMetaAnnotationID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{AnnotationID: &selectedMetaAnnotationID, AnnotatorID: annotator.ID, AnnotationTypeID: metaType.ID, Value: fixture.SelectedNestedMetaValue})
	if err != nil {
		t.Fatalf("CreateAnnotation(selected nested meta): %v", err)
	}

	retainedAnnotationID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		AssociationID:    &retainedAssociationID,
		AnnotatorID:      annotator.ID,
		AnnotationTypeID: associationType.ID,
		Value:            fixture.RetainedAnnotationValue,
	})
	if err != nil {
		t.Fatalf("CreateAnnotation(retained association): %v", err)
	}
	retainedMetaAnnotationID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		AnnotationID:     &retainedAnnotationID,
		AnnotatorID:      annotator.ID,
		AnnotationTypeID: metaType.ID,
		Value:            fixture.RetainedMetaValue,
	})
	if err != nil {
		t.Fatalf("CreateAnnotation(retained meta): %v", err)
	}
	retainedNestedMetaAnnotationID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{AnnotationID: &retainedMetaAnnotationID, AnnotatorID: annotator.ID, AnnotationTypeID: metaType.ID, Value: fixture.RetainedNestedMetaValue})
	if err != nil {
		t.Fatalf("CreateAnnotation(retained nested meta): %v", err)
	}

	result, err := s.PruneSessions(ctx, []ingest.SessionID{fixture.SelectedSessionID})
	if err != nil {
		t.Fatalf("PruneSessions(selected association annotation): %v", err)
	}
	if result.Deleted != fixture.ExpectedDeleted {
		t.Fatalf("PruneSessions Deleted = %d, want %d", result.Deleted, fixture.ExpectedDeleted)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	assertCount := func(label, query string, want int, args ...any) {
		t.Helper()
		if got := queryInt(t, conn, query, args...); got != want {
			t.Errorf("%s: got %d rows, want %d", label, got, want)
		}
	}

	assertCount("selected session", `SELECT COUNT(*) FROM sessions WHERE session_id = ?`, 0, string(fixture.SelectedSessionID))
	assertCount("selected current commit", `SELECT COUNT(*) FROM session_commits WHERE session_id = ?`, 0, string(fixture.SelectedSessionID))
	assertCount("selected durable association", `SELECT COUNT(*) FROM session_commit_associations WHERE association_id = ?`, 0, selectedAssociationID.String())
	assertCount("selected association annotation", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 0, selectedAnnotationID)
	assertCount("selected association target", `SELECT COUNT(*) FROM annotation_target_associations WHERE annotation_id = ?`, 0, selectedAnnotationID)
	assertCount("selected meta-annotation", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 0, selectedMetaAnnotationID)
	assertCount("selected meta target", `SELECT COUNT(*) FROM annotation_target_annotations WHERE annotation_id = ?`, 0, selectedMetaAnnotationID)
	assertCount("selected nested meta", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 0, selectedNestedMetaAnnotationID)

	assertCount("retained session", `SELECT COUNT(*) FROM sessions WHERE session_id = ?`, 1, string(fixture.RetainedSessionID))
	assertCount("retained current commit", `SELECT COUNT(*) FROM session_commits WHERE session_id = ?`, 1, string(fixture.RetainedSessionID))
	assertCount("retained durable association", `SELECT COUNT(*) FROM session_commit_associations WHERE association_id = ?`, 1, retainedAssociationID.String())
	assertCount("retained association annotation", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, retainedAnnotationID)
	assertCount("retained association target", `SELECT COUNT(*) FROM annotation_target_associations WHERE annotation_id = ?`, 1, retainedAnnotationID)
	assertCount("retained meta-annotation", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, retainedMetaAnnotationID)
	assertCount("retained meta target", `SELECT COUNT(*) FROM annotation_target_annotations WHERE annotation_id = ?`, 1, retainedMetaAnnotationID)
	assertCount("retained nested meta", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, retainedNestedMetaAnnotationID)

	retainedRows, err := s.GetAssociationAnnotationsForSession(ctx, string(fixture.RetainedSessionID))
	if err != nil {
		t.Fatalf("GetAssociationAnnotationsForSession(retained): %v", err)
	}
	if len(retainedRows) != 1 || retainedRows[0].ID != retainedAnnotationID {
		t.Fatalf("retained association annotations = %+v, want annotation %s", retainedRows, retainedAnnotationID)
	}
}
