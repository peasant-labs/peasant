package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/annotate_prune.yaml
var annotatePruneFixtureData []byte

//go:embed testdata/annotate_prune_unknown_field.yaml
var annotatePruneUnknownFieldFixtureData []byte

//go:embed testdata/annotate_prune_second_document.yaml
var annotatePruneSecondDocumentFixtureData []byte

//go:embed testdata/annotate_prune_multiple_cases.yaml
var annotatePruneMultipleCasesFixtureData []byte

type annotatePruneFixtureDocument struct {
	ExpectedCaseCount int                    `yaml:"expectedCaseCount"`
	Cases             []annotatePruneFixture `yaml:"cases"`
}

type annotatePruneFixture struct {
	SelectedSessionID  ingest.SessionID `yaml:"selectedSessionID"`
	RetainedSessionID  ingest.SessionID `yaml:"retainedSessionID"`
	SelectedCommitHash string           `yaml:"selectedCommitHash"`
	RetainedCommitHash string           `yaml:"retainedCommitHash"`
	SelectedValue      string           `yaml:"selectedValue"`
	RetainedValue      string           `yaml:"retainedValue"`
	AnnotatorName      string           `yaml:"annotatorName"`
	ExpectedCount      int              `yaml:"expectedCount"`
}

func loadAnnotatePruneFixture(data []byte) (annotatePruneFixture, error) {
	const fixturePath = "cmd/peasant/testdata/annotate_prune.yaml"
	var document annotatePruneFixtureDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return annotatePruneFixture{}, fmt.Errorf("annotate prune fixture rule failed: typed YAML fields must match the document schema; unknown or malformed data invalidates mounted CLI evidence; where=%s loader=first-document decode; when=test fixture loading; impact=dry-run/delete coverage cannot be trusted; fix=remove unknown fields and provide expectedCaseCount plus typed cases: %w", fixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return annotatePruneFixture{}, fmt.Errorf("annotate prune fixture rule failed: exactly one YAML document is allowed; trailing data can be silently ignored and invalidates mounted CLI evidence; where=%s loader=end-of-document check; when=test fixture loading; impact=dry-run/delete coverage cannot be trusted; fix=remove the second document so the next decode returns EOF: %w", fixturePath, err)
	}
	if document.ExpectedCaseCount != 1 || len(document.Cases) != 1 {
		return annotatePruneFixture{}, fmt.Errorf("annotate prune fixture rule failed: declared and actual case counts must each equal one, got expectedCaseCount=%d cases=%d; a different corpus invalidates the sole mounted CLI scenario; where=%s loader=case-count validation; when=test fixture loading; impact=dry-run/delete coverage cannot be trusted; fix=set expectedCaseCount: 1 and retain exactly one typed cases entry", document.ExpectedCaseCount, len(document.Cases), fixturePath)
	}
	fixture := document.Cases[0]
	if fixture.SelectedSessionID == "" || fixture.RetainedSessionID == "" || fixture.SelectedCommitHash == "" || fixture.RetainedCommitHash == "" || fixture.SelectedValue == "" || fixture.RetainedValue == "" || fixture.AnnotatorName == "" || fixture.ExpectedCount != 1 {
		return fixture, fmt.Errorf("annotate prune fixture rule failed: the sole case must define every selected/retained field and expectedCount=1; incomplete expectations invalidate mounted CLI evidence; where=%s loader=case-field validation; when=test fixture loading; impact=dry-run/delete coverage cannot be trusted; fix=populate all case fields and set expectedCount: 1", fixturePath)
	}
	return fixture, nil
}

func TestLoadAnnotatePruneFixture_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	_, err := loadAnnotatePruneFixture(annotatePruneUnknownFieldFixtureData)
	if err == nil || !strings.Contains(err.Error(), "field unexpectedEvidence not found") {
		t.Fatalf("error = %v, want unknown-field rejection", err)
	}
}

func TestLoadAnnotatePruneFixture_RejectsSecondDocument(t *testing.T) {
	t.Parallel()
	_, err := loadAnnotatePruneFixture(annotatePruneSecondDocumentFixtureData)
	if err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("error = %v, want second-document rejection", err)
	}
}

func TestLoadAnnotatePruneFixture_RejectsMultipleCases(t *testing.T) {
	t.Parallel()
	_, err := loadAnnotatePruneFixture(annotatePruneMultipleCasesFixtureData)
	if err == nil || !strings.Contains(err.Error(), "expectedCaseCount=2 cases=2") {
		t.Fatalf("error = %v, want multi-case corpus rejection", err)
	}
}

func TestAnnotatePrune_RejectsExplicitEmptySessionFileBeforeDatabaseAccess(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	emptyFile := filepath.Join(t.TempDir(), "empty-sessions.txt")
	if err := os.WriteFile(emptyFile, []byte("\n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := executeAnnotateCmd(t, dataHome, []string{"prune", "outcome-classifier", "--session-from-file", emptyFile})
	if err == nil || !strings.Contains(err.Error(), "empty session scope") {
		t.Fatalf("error = %v, want empty session scope rejection", err)
	}
	if strings.Contains(output, "annotation(s) by") {
		t.Fatalf("output = %q, want no deletion summary", output)
	}
	if _, statErr := os.Stat(defaults.ResolveDBFilePathWith(dataHome).String()); !os.IsNotExist(statErr) {
		t.Fatalf("database was accessed before scope rejection: %v", statErr)
	}
}

func TestAnnotatePrune_AssociationScopeDryRunAndDelete(t *testing.T) {
	t.Parallel()
	fixture, err := loadAnnotatePruneFixture(annotatePruneFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, string(fixture.SelectedSessionID))
	db, err := store.Open(defaults.ResolveDBFilePathWith(dataHome).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UnixMilli()
	entry := ingest.StoreEntry{Metadata: &ingest.UnifiedMetadata{SchemaVersion: 1, SessionID: fixture.RetainedSessionID, ModelHarness: defaults.HarnessClaudeCode, Model: ingest.ModelID("claude-opus-4-6"), HostSlug: ingest.HostSlug("github.com--test--repo"), Timestamp: ingest.TimestampInfo{Start: now, End: now, Ingested: &now}, Project: ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "test-project", FilePath: "/test/path"}, Source: ingest.SourceInfo{Format: ingest.SourceFormatJSONL}}}
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.UpsertSessionCommits(ctx, fixture.SelectedSessionID, []ingest.CommitInfo{{Hash: fixture.SelectedCommitHash}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSessionCommits(ctx, fixture.RetainedSessionID, []ingest.CommitInfo{{Hash: fixture.RetainedCommitHash}}); err != nil {
		t.Fatal(err)
	}
	annotator, _ := db.GetAnnotator(ctx, fixture.AnnotatorName)
	typeRow, _ := db.GetAnnotationTypeByTypeID(ctx, testutil.TestTypeIDSessionOutcome)
	selectedAssociations, _ := db.ListCurrentSessionCommitAssociations(ctx, fixture.SelectedSessionID)
	retainedAssociations, _ := db.ListCurrentSessionCommitAssociations(ctx, fixture.RetainedSessionID)
	selectedID, err := db.CreateAnnotation(ctx, store.CreateAnnotationParams{AssociationID: &selectedAssociations[0].ID, AnnotatorID: annotator.ID, AnnotationTypeID: typeRow.ID, Value: fixture.SelectedValue})
	if err != nil {
		t.Fatal(err)
	}
	retainedID, err := db.CreateAnnotation(ctx, store.CreateAnnotationParams{AssociationID: &retainedAssociations[0].ID, AnnotatorID: annotator.ID, AnnotationTypeID: typeRow.ID, Value: fixture.RetainedValue})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	output, err := executeAnnotateCmd(t, dataHome, []string{"prune", fixture.AnnotatorName, "--session", string(fixture.SelectedSessionID), "--dry-run"})
	if err != nil || !strings.Contains(output, "would delete 1 annotation(s)") {
		t.Fatalf("dry run: output=%q err=%v", output, err)
	}
	db, err = store.Open(defaults.ResolveDBFilePathWith(dataHome).String())
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := db.ListSystemAnnotations(ctx)
	if len(rows) < 2 {
		t.Fatalf("dry run mutated annotations: %+v", rows)
	}
	db.Close()
	output, err = executeAnnotateCmd(t, dataHome, []string{"prune", fixture.AnnotatorName, "--session", string(fixture.SelectedSessionID)})
	if err != nil || !strings.Contains(output, "deleted 1 annotation(s)") {
		t.Fatalf("delete: output=%q err=%v", output, err)
	}
	db, err = store.Open(defaults.ResolveDBFilePathWith(dataHome).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err = db.ListSystemAnnotations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == selectedID {
			t.Fatal("selected annotation retained")
		}
	}
	foundRetained := false
	for _, row := range rows {
		foundRetained = foundRetained || row.ID == retainedID
	}
	if !foundRetained {
		t.Fatal("retained annotation deleted")
	}
}
