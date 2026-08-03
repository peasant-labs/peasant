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
	"gopkg.in/yaml.v3"
)

//go:embed testdata/annotation_prune/association_scoped.yaml
var annotationPruneAssociationScopedFixtureData []byte

const annotationPruneAssociationScopedFixturePath = "internal/store/testdata/annotation_prune/association_scoped.yaml"

type annotationPruneAssociationScopedFixtures struct {
	Cases []annotationPruneAssociationScopedFixture `yaml:"cases"`
}

type annotationPruneAssociationScopedFixture struct {
	Name                    string             `yaml:"name"`
	SelectedSessionID       ingest.SessionID   `yaml:"selectedSessionID"`
	RetainedSessionID       ingest.SessionID   `yaml:"retainedSessionID"`
	SelectedProjectHash     ingest.ProjectHash `yaml:"selectedProjectHash"`
	RetainedProjectHash     ingest.ProjectHash `yaml:"retainedProjectHash"`
	SelectedCommitHash      string             `yaml:"selectedCommitHash"`
	RetainedCommitHash      string             `yaml:"retainedCommitHash"`
	SelectedAnnotationValue string             `yaml:"selectedAnnotationValue"`
	RetainedAnnotationValue string             `yaml:"retainedAnnotationValue"`
	SelectedSessionValue    string             `yaml:"selectedSessionValue"`
	SelectedEntryValue      string             `yaml:"selectedEntryValue"`
	SelectedMetaValue       string             `yaml:"selectedMetaValue"`
	SelectedNestedMetaValue string             `yaml:"selectedNestedMetaValue"`
	RetainedMetaValue       string             `yaml:"retainedMetaValue"`
	RetainedNestedMetaValue string             `yaml:"retainedNestedMetaValue"`
	ExpectedCount           int                `yaml:"expectedCount"`
}

func loadAnnotationPruneAssociationScopedFixture(data []byte) (annotationPruneAssociationScopedFixture, error) {
	var fixtures annotationPruneAssociationScopedFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		return annotationPruneAssociationScopedFixture{}, fmt.Errorf("decode committed fixture %s: %w; fix the YAML schema or remove unknown fields", annotationPruneAssociationScopedFixturePath, err)
	}
	var trailing any
	switch err := decoder.Decode(&trailing); err {
	case io.EOF:
	case nil:
		return annotationPruneAssociationScopedFixture{}, fmt.Errorf("committed fixture %s contains a trailing YAML document; remove the extra document so the fixture contains exactly one YAML document", annotationPruneAssociationScopedFixturePath)
	default:
		return annotationPruneAssociationScopedFixture{}, fmt.Errorf("decode trailing YAML content in committed fixture %s: %w; remove or repair the trailing YAML document", annotationPruneAssociationScopedFixturePath, err)
	}
	if len(fixtures.Cases) != 1 {
		return annotationPruneAssociationScopedFixture{}, fmt.Errorf("committed fixture %s defines %d cases, want exactly one annotation-pruning scenario; remove extra cases from the fixture", annotationPruneAssociationScopedFixturePath, len(fixtures.Cases))
	}
	fixture := fixtures.Cases[0]
	if fixture.Name == "" || fixture.SelectedSessionID == "" || fixture.RetainedSessionID == "" ||
		fixture.SelectedProjectHash == "" || fixture.RetainedProjectHash == "" ||
		fixture.SelectedCommitHash == "" || fixture.RetainedCommitHash == "" ||
		fixture.SelectedAnnotationValue == "" || fixture.RetainedAnnotationValue == "" ||
		fixture.SelectedSessionValue == "" || fixture.SelectedEntryValue == "" ||
		fixture.SelectedMetaValue == "" || fixture.SelectedNestedMetaValue == "" ||
		fixture.RetainedMetaValue == "" || fixture.RetainedNestedMetaValue == "" {
		return annotationPruneAssociationScopedFixture{}, fmt.Errorf("committed fixture %s has an incomplete scenario; populate all IDs, hashes, commit hashes, annotation values, and the meta value", annotationPruneAssociationScopedFixturePath)
	}
	if fixture.SelectedSessionID == fixture.RetainedSessionID ||
		fixture.SelectedProjectHash == fixture.RetainedProjectHash ||
		fixture.SelectedCommitHash == fixture.RetainedCommitHash ||
		fixture.ExpectedCount != 3 {
		return annotationPruneAssociationScopedFixture{}, fmt.Errorf("committed fixture %s must define distinct selected/retained records and expectedCount=3; update the fixture values", annotationPruneAssociationScopedFixturePath)
	}
	return fixture, nil
}

func TestLoadAnnotationPruneAssociationScopedFixture_RejectsUnknownField(t *testing.T) {
	t.Parallel()

	data := append([]byte(nil), annotationPruneAssociationScopedFixtureData...)
	data = append(data, []byte("\nunknownFixtureField: true\n")...)

	_, err := loadAnnotationPruneAssociationScopedFixture(data)
	if err == nil {
		t.Fatal("loadAnnotationPruneAssociationScopedFixture accepted an unknown fixture field")
	}
	if !strings.Contains(err.Error(), "unknownFixtureField") {
		t.Fatalf("loadAnnotationPruneAssociationScopedFixture error = %q, want unknown field name", err)
	}
}

func TestLoadAnnotationPruneAssociationScopedFixture_RejectsTrailingDocument(t *testing.T) {
	t.Parallel()

	data := append([]byte(nil), annotationPruneAssociationScopedFixtureData...)
	data = append(data, []byte("\n---\n{}\n")...)

	_, err := loadAnnotationPruneAssociationScopedFixture(data)
	if err == nil {
		t.Fatal("loadAnnotationPruneAssociationScopedFixture accepted a trailing YAML document")
	}
	if !strings.Contains(err.Error(), "trailing YAML document") {
		t.Fatalf("loadAnnotationPruneAssociationScopedFixture error = %q, want trailing YAML document", err)
	}
}

func TestStore_DeleteAnnotationsByAnnotator_AssociationScoped(t *testing.T) {
	t.Parallel()
	fixture, err := loadAnnotationPruneAssociationScopedFixture(annotationPruneAssociationScopedFixtureData)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	s := openTestStore(t)
	seedPruneSession(t, s, string(fixture.SelectedSessionID), string(fixture.SelectedProjectHash), defaults.HarnessClaudeCode, 1700000000000)
	seedPruneSession(t, s, string(fixture.RetainedSessionID), string(fixture.RetainedProjectHash), defaults.HarnessClaudeCode, 1700100000000)
	seedPruneEntries(t, s, string(fixture.SelectedSessionID), 1)

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

	annotator, err := s.GetAnnotator(ctx, pruneAnnotatorName)
	if err != nil || annotator == nil {
		t.Fatalf("GetAnnotator(%s): %v", pruneAnnotatorName, err)
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
	selectedNestedMetaID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{AnnotationID: &selectedMetaAnnotationID, AnnotatorID: annotator.ID, AnnotationTypeID: metaType.ID, Value: fixture.SelectedNestedMetaValue})
	if err != nil {
		t.Fatalf("CreateAnnotation(selected nested meta): %v", err)
	}
	selectedSessionID := string(fixture.SelectedSessionID)
	selectedSessionAnnotationID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{SessionID: &selectedSessionID, AnnotatorID: annotator.ID, AnnotationTypeID: associationType.ID, Value: fixture.SelectedSessionValue})
	if err != nil {
		t.Fatalf("CreateAnnotation(selected session): %v", err)
	}
	selectedEntryAnnotationID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{EntryTarget: &store.EntryTarget{SessionID: selectedSessionID, EntryIndex: 0}, AnnotatorID: annotator.ID, AnnotationTypeID: associationType.ID, Value: fixture.SelectedEntryValue})
	if err != nil {
		t.Fatalf("CreateAnnotation(selected entry): %v", err)
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
	retainedMetaID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{AnnotationID: &retainedAnnotationID, AnnotatorID: annotator.ID, AnnotationTypeID: metaType.ID, Value: fixture.RetainedMetaValue})
	if err != nil {
		t.Fatalf("CreateAnnotation(retained meta): %v", err)
	}
	retainedNestedMetaID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{AnnotationID: &retainedMetaID, AnnotatorID: annotator.ID, AnnotationTypeID: metaType.ID, Value: fixture.RetainedNestedMetaValue})
	if err != nil {
		t.Fatalf("CreateAnnotation(retained nested meta): %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	assertCount := func(label, query string, want int, args ...any) {
		t.Helper()
		if got := queryInt(t, conn, query, args...); got != want {
			t.Errorf("%s: got %d rows, want %d", label, got, want)
		}
	}
	assertRowsBeforePrune := func(prefix string) {
		t.Helper()
		assertCount(prefix+" selected association annotation", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, selectedAnnotationID)
		assertCount(prefix+" selected association target", `SELECT COUNT(*) FROM annotation_target_associations WHERE annotation_id = ?`, 1, selectedAnnotationID)
		assertCount(prefix+" selected meta-annotation", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, selectedMetaAnnotationID)
		assertCount(prefix+" selected meta target", `SELECT COUNT(*) FROM annotation_target_annotations WHERE annotation_id = ?`, 1, selectedMetaAnnotationID)
		assertCount(prefix+" selected nested meta", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, selectedNestedMetaID)
		assertCount(prefix+" selected session root", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, selectedSessionAnnotationID)
		assertCount(prefix+" selected entry root", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, selectedEntryAnnotationID)
		assertCount(prefix+" selected durable association", `SELECT COUNT(*) FROM session_commit_associations WHERE association_id = ?`, 1, selectedAssociationID.String())
		assertCount(prefix+" retained association annotation", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, retainedAnnotationID)
		assertCount(prefix+" retained association target", `SELECT COUNT(*) FROM annotation_target_associations WHERE annotation_id = ?`, 1, retainedAnnotationID)
		assertCount(prefix+" retained durable association", `SELECT COUNT(*) FROM session_commit_associations WHERE association_id = ?`, 1, retainedAssociationID.String())
		assertCount(prefix+" retained meta", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, retainedMetaID)
		assertCount(prefix+" retained nested meta", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, retainedNestedMetaID)
	}

	assertRowsBeforePrune("before dry-run")
	count, err := s.CountAnnotationsByAnnotator(ctx, pruneAnnotatorName, []string{string(fixture.SelectedSessionID)})
	if err != nil {
		t.Fatalf("CountAnnotationsByAnnotator(scoped): %v", err)
	}
	if count != int64(fixture.ExpectedCount) {
		t.Fatalf("CountAnnotationsByAnnotator(scoped) = %d, want %d", count, fixture.ExpectedCount)
	}
	assertRowsBeforePrune("after dry-run")

	deleted, err := s.DeleteAnnotationsByAnnotator(ctx, pruneAnnotatorName, []string{string(fixture.SelectedSessionID)})
	if err != nil {
		t.Fatalf("DeleteAnnotationsByAnnotator(scoped): %v", err)
	}
	if deleted != int64(fixture.ExpectedCount) {
		t.Fatalf("DeleteAnnotationsByAnnotator(scoped) = %d, want %d", deleted, fixture.ExpectedCount)
	}

	assertCount("selected association annotation after delete", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 0, selectedAnnotationID)
	assertCount("selected association target after delete", `SELECT COUNT(*) FROM annotation_target_associations WHERE annotation_id = ?`, 0, selectedAnnotationID)
	assertCount("selected meta-annotation after delete", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 0, selectedMetaAnnotationID)
	assertCount("selected meta target after delete", `SELECT COUNT(*) FROM annotation_target_annotations WHERE annotation_id = ?`, 0, selectedMetaAnnotationID)
	assertCount("selected nested meta after delete", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 0, selectedNestedMetaID)
	assertCount("selected session root after delete", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 0, selectedSessionAnnotationID)
	assertCount("selected entry root after delete", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 0, selectedEntryAnnotationID)
	assertCount("selected durable association after delete", `SELECT COUNT(*) FROM session_commit_associations WHERE association_id = ?`, 1, selectedAssociationID.String())
	assertCount("retained association annotation after delete", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, retainedAnnotationID)
	assertCount("retained association target after delete", `SELECT COUNT(*) FROM annotation_target_associations WHERE annotation_id = ?`, 1, retainedAnnotationID)
	assertCount("retained durable association after delete", `SELECT COUNT(*) FROM session_commit_associations WHERE association_id = ?`, 1, retainedAssociationID.String())
	assertCount("retained nested meta after delete", `SELECT COUNT(*) FROM annotations WHERE id = ?`, 1, retainedNestedMetaID)
}
