package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/annotations"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// executeAnnotateImportCmd runs the annotate command under a test root with
// --data-dir=dir (parallel-safe; no t.Setenv), capturing stdout and stderr
// SEPARATELY (the import path writes warnings to stderr that must be asserted
// apart from the success line on stdout, so it cannot use the combined-buffer
// executeWithDataDir helper). args are the sub-args; the "annotate" subcommand
// name is inserted automatically by the root.
func executeAnnotateImportCmd(t *testing.T, dir string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	root := newTestRoot()
	sub := BuildAnnotateCommand()
	root.AddCommand(sub)
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, sub.Name()}, args...))
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// openTestDB opens the store at the --data-dir-resolved path (dir is the
// t.TempDir() passed as --data-dir to the command, so the DB lives at
// dir/<AppName>/peasant.db). Parallel-safe: no reliance on process env.
func openTestDB(t *testing.T, dir string) *store.Store {
	t.Helper()
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("openTestDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// writeJSONLFile writes a slice of ExportedAnnotation records to a JSONL file.
func writeJSONLFile(t *testing.T, path string, records []export.ExportedAnnotation) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("writeJSONLFile: create %s: %v", path, err)
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			f.Close()
			t.Fatalf("writeJSONLFile: encode: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("writeJSONLFile: close: %v", err)
	}
}

// TestCLI_AnnotateImportSubcommandExists verifies the import subcommand is registered.
func TestCLI_AnnotateImportSubcommandExists(t *testing.T) {
	t.Parallel()
	cmd := BuildAnnotateCommand()
	found := false
	for _, c := range cmd.Commands() {
		if c.Name() == "import" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("subcommand 'import' not found; available: %v", subcommandNames(cmd))
	}
}

// TestCLI_AnnotateImportMutualExclusion verifies flag validation.
func TestCLI_AnnotateImportMutualExclusion(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	t.Run("neither flag set", func(t *testing.T) {
		_, _, err := executeAnnotateImportCmd(t, dataHome, []string{"import"})
		if err == nil {
			t.Fatal("expected error when neither --from-file nor --from-dir is set")
		}
		if !strings.Contains(err.Error(), "neither --from-file nor --from-dir") {
			t.Errorf("expected 'neither --from-file nor --from-dir' in error; got: %v", err)
		}
	})

	t.Run("both flags set", func(t *testing.T) {
		_, _, err := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-file", "a.jsonl", "--from-dir", "/tmp"})
		if err == nil {
			t.Fatal("expected error when both --from-file and --from-dir are set")
		}
		if !strings.Contains(err.Error(), "both --from-file and --from-dir") {
			t.Errorf("expected 'both --from-file and --from-dir' in error; got: %v", err)
		}
	})
}

// TestCLI_AnnotateImportRoundTrip tests the full export-import round-trip:
// seed annotations, export them, then import and verify.
func TestCLI_AnnotateImportRoundTrip(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	db := openTestDB(t, dataHome)
	ctx := context.Background()

	// Create the human-cli annotator.
	annotatorID, err := db.CreateAnnotator(ctx, store.CreateAnnotatorParams{
		Kind:        schema.AnnotatorHuman,
		Name:        humanCLIAnnotatorName,
		DisplayName: "Human (CLI)",
		Description: "Test annotator",
	})
	if err != nil {
		t.Fatalf("create annotator: %v", err)
	}

	// Get the type UUID for the annotation type.
	registry := annotations.NewTypeReader(db)
	typeSummary, err := registry.GetType(ctx, testutil.TestTypeIDSessionApproval)
	if err != nil {
		t.Fatalf("get type: %v", err)
	}

	// Create a source annotation.
	sid := testSessionUUID
	_, err = db.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sid,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeSummary.ID,
		Value:            "approve",
	})
	if err != nil {
		t.Fatalf("create annotation: %v", err)
	}

	// Export to JSONL.
	exported, err := export.ExportAnnotations(ctx, db, testSessionUUID)
	if err != nil {
		t.Fatalf("export annotations: %v", err)
	}
	if len(exported) == 0 {
		t.Fatal("expected at least one exported annotation")
	}

	// Write to temp file.
	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, testSessionUUID+"--annotations.jsonl")
	writeJSONLFile(t, jsonlPath, exported)

	// Now create a fresh DB for import (to prove the import creates new annotations).
	dataHome2 := t.TempDir()
	seedTestSessionInto(t, dataHome2, testSessionUUID)

	// Import from the JSONL file.
	stdout, stderr, err := executeAnnotateImportCmd(t, dataHome2, []string{"import", "--from-file", jsonlPath})
	if err != nil {
		t.Fatalf("annotate import: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	if !strings.Contains(stdout, "imported 1 of 1 annotations (0 duplicates skipped, 0 errors)") {
		t.Errorf("expected 'imported 1 of 1 annotations (0 duplicates skipped, 0 errors)'; got stdout: %s", stdout)
	}

	// Verify annotation exists in the new DB.
	db2 := openTestDB(t, dataHome2)
	rows, err := db2.GetAnnotationsForSession(ctx, testSessionUUID)
	if err != nil {
		t.Fatalf("get annotations after import: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one annotation after import")
	}

	// Verify the imported annotation matches.
	found := false
	for _, row := range rows {
		if row.TypeID == testutil.TestTypeIDSessionApproval && row.Value == "approve" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected annotation with type_id=%s value=approve; got rows: %+v",
			testutil.TestTypeIDSessionApproval, rows)
	}
}

// TestCLI_AnnotateImportMalformedJSONL verifies that malformed lines are skipped
// with warnings while valid lines are imported.
func TestCLI_AnnotateImportMalformedJSONL(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	// Build a JSONL file with one bad line and one valid line.
	validRecord := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "approve",
		Annotator:     "import-test-annotator",
		AnnotatorKind: schema.AnnotatorHuman.String(),
		CreatedAt:     1000,
	}

	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, "mixed.jsonl")
	f, err := os.Create(jsonlPath)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	// Write a malformed line.
	f.WriteString("{not valid json\n")
	// Write a valid line.
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(validRecord); err != nil {
		t.Fatalf("encode: %v", err)
	}
	f.Close()

	stdout, stderr, err := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-file", jsonlPath})
	if err != nil {
		t.Fatalf("annotate import: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// Should import 1, skip 1.
	if !strings.Contains(stdout, "imported 1 of 2 annotations (0 duplicates skipped, 1 errors)") {
		t.Errorf("expected 'imported 1 of 2 annotations (0 duplicates skipped, 1 errors)'; got stdout: %s", stdout)
	}

	// Warning should appear on stderr.
	if !strings.Contains(stderr, "malformed JSON") {
		t.Errorf("expected 'malformed JSON' warning on stderr; got stderr: %s", stderr)
	}
}

// TestCLI_AnnotateImportInvalidTypeID verifies that records with a non-existent
// type_id are skipped with a warning while valid records are still imported.
func TestCLI_AnnotateImportInvalidTypeID(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	validRecord := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "deny",
		Annotator:     "import-test-annotator",
		AnnotatorKind: schema.AnnotatorHuman.String(),
		CreatedAt:     2000,
	}
	invalidTypeRecord := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        "nonexistent.type.id",
		Value:         "whatever",
		Annotator:     "import-test-annotator",
		AnnotatorKind: schema.AnnotatorHuman.String(),
		CreatedAt:     3000,
	}

	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, "invalid-type.jsonl")
	writeJSONLFile(t, jsonlPath, []export.ExportedAnnotation{invalidTypeRecord, validRecord})

	stdout, stderr, err := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-file", jsonlPath})
	if err != nil {
		t.Fatalf("annotate import: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// Should import 1 valid, skip 1 invalid.
	if !strings.Contains(stdout, "imported 1 of 2 annotations (0 duplicates skipped, 1 errors)") {
		t.Errorf("expected 'imported 1 of 2 annotations (0 duplicates skipped, 1 errors)'; got stdout: %s", stdout)
	}

	// Warning about unknown type_id on stderr.
	if !strings.Contains(stderr, "unknown type_id") {
		t.Errorf("expected 'unknown type_id' warning on stderr; got stderr: %s", stderr)
	}
}

// TestCLI_AnnotateImportFromDir verifies importing from a directory with multiple JSONL files.
func TestCLI_AnnotateImportFromDir(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	record1 := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "approve",
		Annotator:     "dir-import-annotator",
		AnnotatorKind: schema.AnnotatorHuman.String(),
		CreatedAt:     4000,
	}
	record2 := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "deny",
		Annotator:     "dir-import-annotator",
		AnnotatorKind: schema.AnnotatorHuman.String(),
		CreatedAt:     5000,
	}

	// Files are processed in lexicographic order (Glob sorts): session-a (approve) first,
	// then session-b (deny) supersedes it — deny must be the surviving value.
	importDir := t.TempDir()
	writeJSONLFile(t, filepath.Join(importDir, "session-a--annotations.jsonl"), []export.ExportedAnnotation{record1})
	writeJSONLFile(t, filepath.Join(importDir, "session-b--annotations.jsonl"), []export.ExportedAnnotation{record2})

	stdout, stderr, err := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-dir", importDir})
	if err != nil {
		t.Fatalf("annotate import from dir: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// Both records imported: second supersedes first (same type+annotator+session, different value).
	if !strings.Contains(stdout, "imported 2 of 2 annotations (0 duplicates skipped, 0 errors)") {
		t.Errorf("expected 'imported 2 of 2 annotations (0 duplicates skipped, 0 errors)'; got stdout: %s", stdout)
	}

	// Only the non-superseded annotation should be visible.
	db := openTestDB(t, dataHome)
	ctx := context.Background()
	rows, err := db.GetAnnotationsForSession(ctx, testSessionUUID)
	if err != nil {
		t.Fatalf("get annotations after dir import: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 non-superseded annotation after dir import; got %d", len(rows))
	}
	if len(rows) == 1 && rows[0].Value != "deny" {
		t.Errorf("expected surviving annotation value %q; got %q", "deny", rows[0].Value)
	}
}

// TestCLI_AnnotateImportFromDirNoFiles verifies that importing from a directory
// with no matching JSONL files shows an informative message.
func TestCLI_AnnotateImportFromDirNoFiles(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	emptyDir := t.TempDir()

	stdout, _, err := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-dir", emptyDir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout, "no *--annotations.jsonl files found") {
		t.Errorf("expected 'no *--annotations.jsonl files found'; got stdout: %s", stdout)
	}
}

// TestCLI_AnnotateImportInvalidAnnotatorKind verifies that records with an invalid
// annotator_kind are skipped with a warning.
func TestCLI_AnnotateImportInvalidAnnotatorKind(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	record := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "approve",
		Annotator:     "bad-kind-annotator",
		AnnotatorKind: "invalid-kind",
		CreatedAt:     6000,
	}

	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, "bad-kind.jsonl")
	writeJSONLFile(t, jsonlPath, []export.ExportedAnnotation{record})

	stdout, stderr, err := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-file", jsonlPath})
	if err != nil {
		t.Fatalf("annotate import: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	if !strings.Contains(stdout, "imported 0 of 1 annotations (0 duplicates skipped, 1 errors)") {
		t.Errorf("expected 'imported 0 of 1 annotations (0 duplicates skipped, 1 errors)'; got stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "invalid annotator_kind") {
		t.Errorf("expected 'invalid annotator_kind' warning on stderr; got stderr: %s", stderr)
	}
}

// TestCLI_AnnotateImport_AgentNoModel_Skipped verifies that an agent annotation record
// without model info (no JSONL fields, no CLI flags) is skipped with an actionable warning.
func TestCLI_AnnotateImport_AgentNoModel_Skipped(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	record := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "approve",
		Annotator:     "llm-judge",
		AnnotatorKind: schema.AnnotatorAgent.String(),
		Confidence:    ptrFloat64(0.9),
		CreatedAt:     7000,
		// No ModelID, no ProviderKey
	}

	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, "agent-no-model.jsonl")
	writeJSONLFile(t, jsonlPath, []export.ExportedAnnotation{record})

	// No --model-id or --provider flags.
	stdout, stderr, err := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-file", jsonlPath})
	if err != nil {
		t.Fatalf("annotate import: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	if !strings.Contains(stdout, "imported 0 of 1 annotations (0 duplicates skipped, 1 errors)") {
		t.Errorf("expected 'imported 0 of 1 annotations (0 duplicates skipped, 1 errors)'; got stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "requires model info") {
		t.Errorf("expected 'requires model info' warning on stderr; got stderr: %s", stderr)
	}
}

// TestCLI_AnnotateImport_AgentCLIFlags_Creates verifies that an agent annotation
// without JSONL model fields falls back to --model-id and --provider CLI flags,
// creating an annotator named "baseName:modelID".
func TestCLI_AnnotateImport_AgentCLIFlags_Creates(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	record := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "approve",
		Annotator:     "llm-judge",
		AnnotatorKind: schema.AnnotatorAgent.String(),
		Confidence:    ptrFloat64(0.85),
		CreatedAt:     8000,
		// No ModelID or ProviderKey in JSONL.
	}

	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, "agent-cli-flags.jsonl")
	writeJSONLFile(t, jsonlPath, []export.ExportedAnnotation{record})

	stdout, stderr, err := executeAnnotateImportCmd(t, dataHome, []string{
		"import", "--from-file", jsonlPath,
		"--model-id", "claude-opus-4-6",
		"--provider", "anthropic",
	})
	if err != nil {
		t.Fatalf("annotate import: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	if !strings.Contains(stdout, "imported 1 of 1 annotations (0 duplicates skipped, 0 errors)") {
		t.Errorf("expected 'imported 1 of 1 annotations (0 duplicates skipped, 0 errors)'; got stdout: %s", stdout)
	}

	// Verify the annotator was created with the composite name.
	db := openTestDB(t, dataHome)
	ctx := context.Background()
	annotator, err := db.GetAnnotator(ctx, "llm-judge:claude-opus-4-6")
	if err != nil {
		t.Fatalf("get annotator: %v", err)
	}
	if annotator == nil {
		t.Fatal("expected annotator 'llm-judge:claude-opus-4-6' to exist in DB")
	}
	if annotator.Kind != schema.AnnotatorAgent {
		t.Errorf("expected kind %s; got %s", schema.AnnotatorAgent, annotator.Kind)
	}
	if annotator.DisplayName != "llm-judge (claude-opus-4-6)" {
		t.Errorf("expected display_name 'llm-judge (claude-opus-4-6)'; got %q", annotator.DisplayName)
	}
	if annotator.ModelID == nil || *annotator.ModelID != "claude-opus-4-6" {
		t.Errorf("expected model_id 'claude-opus-4-6'; got %v", annotator.ModelID)
	}
	if annotator.ProviderKey == nil || *annotator.ProviderKey != "anthropic" {
		t.Errorf("expected provider_key 'anthropic'; got %v", annotator.ProviderKey)
	}

	_ = stderr
}

// TestCLI_AnnotateImport_AgentJSONLWins verifies that JSONL model fields take
// priority over CLI flags.
func TestCLI_AnnotateImport_AgentJSONLWins(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	record := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "approve",
		Annotator:     "llm-judge",
		AnnotatorKind: schema.AnnotatorAgent.String(),
		ModelID:       "claude-sonnet-4-6",
		ProviderKey:   "anthropic",
		Confidence:    ptrFloat64(0.9),
		CreatedAt:     9000,
	}

	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, "agent-jsonl-wins.jsonl")
	writeJSONLFile(t, jsonlPath, []export.ExportedAnnotation{record})

	// CLI says opus, JSONL says sonnet. JSONL should win.
	stdout, stderr, err := executeAnnotateImportCmd(t, dataHome, []string{
		"import", "--from-file", jsonlPath,
		"--model-id", "claude-opus-4-6",
		"--provider", "anthropic",
	})
	if err != nil {
		t.Fatalf("annotate import: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	if !strings.Contains(stdout, "imported 1 of 1 annotations (0 duplicates skipped, 0 errors)") {
		t.Errorf("expected 'imported 1 of 1 annotations (0 duplicates skipped, 0 errors)'; got stdout: %s", stdout)
	}

	// Verify the annotator uses JSONL's model, not CLI's.
	db := openTestDB(t, dataHome)
	ctx := context.Background()
	annotator, err := db.GetAnnotator(ctx, "llm-judge:claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("get annotator: %v", err)
	}
	if annotator == nil {
		t.Fatal("expected annotator 'llm-judge:claude-sonnet-4-6' to exist (JSONL wins over CLI)")
	}
	if annotator.ModelID == nil || *annotator.ModelID != "claude-sonnet-4-6" {
		t.Errorf("expected model_id 'claude-sonnet-4-6' (from JSONL); got %v", annotator.ModelID)
	}

	// The opus annotator should NOT exist.
	opusAnnotator, err := db.GetAnnotator(ctx, "llm-judge:claude-opus-4-6")
	if err != nil {
		t.Fatalf("get opus annotator: %v", err)
	}
	if opusAnnotator != nil {
		t.Error("expected annotator 'llm-judge:claude-opus-4-6' to NOT exist (JSONL should win)")
	}
}

// TestCLI_AnnotateImport_HumanNoModel_Works verifies that human annotator records
// import successfully without any model_id or provider_key fields (backward compat).
func TestCLI_AnnotateImport_HumanNoModel_Works(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	record := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "deny",
		Annotator:     "human-reviewer",
		AnnotatorKind: schema.AnnotatorHuman.String(),
		CreatedAt:     10000,
		// No model fields.
	}

	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, "human-no-model.jsonl")
	writeJSONLFile(t, jsonlPath, []export.ExportedAnnotation{record})

	stdout, stderr, err := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-file", jsonlPath})
	if err != nil {
		t.Fatalf("annotate import: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	if !strings.Contains(stdout, "imported 1 of 1 annotations (0 duplicates skipped, 0 errors)") {
		t.Errorf("expected 'imported 1 of 1 annotations (0 duplicates skipped, 0 errors)'; got stdout: %s", stdout)
	}

	// Verify the annotator was created with the base name (no model suffix).
	db := openTestDB(t, dataHome)
	ctx := context.Background()
	annotator, err := db.GetAnnotator(ctx, "human-reviewer")
	if err != nil {
		t.Fatalf("get annotator: %v", err)
	}
	if annotator == nil {
		t.Fatal("expected annotator 'human-reviewer' to exist")
	}
	if annotator.Kind != schema.AnnotatorHuman {
		t.Errorf("expected kind %s; got %s", schema.AnnotatorHuman, annotator.Kind)
	}
	if annotator.ModelID != nil {
		t.Errorf("expected nil model_id for human annotator; got %v", annotator.ModelID)
	}
}

// TestCLI_AnnotateImport_TwoModels_TwoAnnotators verifies that two JSONL records
// with the same base annotator name but different model_ids produce two distinct
// annotator rows in the database.
func TestCLI_AnnotateImport_TwoModels_TwoAnnotators(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	records := []export.ExportedAnnotation{
		{
			SessionID:     testSessionUUID,
			TypeID:        testutil.TestTypeIDSessionApproval,
			Value:         "approve",
			Annotator:     "llm-judge",
			AnnotatorKind: schema.AnnotatorAgent.String(),
			ModelID:       "claude-opus-4-6",
			ProviderKey:   "anthropic",
			Confidence:    ptrFloat64(0.9),
			CreatedAt:     11000,
		},
		{
			SessionID:     testSessionUUID,
			TypeID:        testutil.TestTypeIDSessionApproval,
			Value:         "deny",
			Annotator:     "llm-judge",
			AnnotatorKind: schema.AnnotatorAgent.String(),
			ModelID:       "claude-sonnet-4-6",
			ProviderKey:   "anthropic",
			Confidence:    ptrFloat64(0.85),
			CreatedAt:     12000,
		},
	}

	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, "two-models.jsonl")
	writeJSONLFile(t, jsonlPath, records)

	stdout, stderr, err := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-file", jsonlPath})
	if err != nil {
		t.Fatalf("annotate import: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	if !strings.Contains(stdout, "imported 2 of 2 annotations (0 duplicates skipped, 0 errors)") {
		t.Errorf("expected 'imported 2 of 2 annotations (0 duplicates skipped, 0 errors)'; got stdout: %s", stdout)
	}

	// Verify two distinct annotator rows exist.
	db := openTestDB(t, dataHome)
	ctx := context.Background()

	opusAnnotator, err := db.GetAnnotator(ctx, "llm-judge:claude-opus-4-6")
	if err != nil {
		t.Fatalf("get opus annotator: %v", err)
	}
	if opusAnnotator == nil {
		t.Fatal("expected annotator 'llm-judge:claude-opus-4-6' to exist")
	}
	if opusAnnotator.ModelID == nil || *opusAnnotator.ModelID != "claude-opus-4-6" {
		t.Errorf("expected model_id 'claude-opus-4-6'; got %v", opusAnnotator.ModelID)
	}

	sonnetAnnotator, err := db.GetAnnotator(ctx, "llm-judge:claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("get sonnet annotator: %v", err)
	}
	if sonnetAnnotator == nil {
		t.Fatal("expected annotator 'llm-judge:claude-sonnet-4-6' to exist")
	}
	if sonnetAnnotator.ModelID == nil || *sonnetAnnotator.ModelID != "claude-sonnet-4-6" {
		t.Errorf("expected model_id 'claude-sonnet-4-6'; got %v", sonnetAnnotator.ModelID)
	}

	// They should be different annotator IDs.
	if opusAnnotator.ID == sonnetAnnotator.ID {
		t.Errorf("expected distinct annotator IDs; both have ID %s", opusAnnotator.ID)
	}
}

// TestCLI_AnnotateImportIdempotent verifies that importing the same JSONL file twice
// results in exactly one annotation in the DB (second import is a true duplicate).
func TestCLI_AnnotateImportIdempotent(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	record := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "approve",
		Annotator:     "idempotent-test-annotator",
		AnnotatorKind: schema.AnnotatorHuman.String(),
		Confidence:    ptrFloat64(0.8),
		CreatedAt:     20000,
	}

	tmpDir := t.TempDir()
	jsonlPath := filepath.Join(tmpDir, testSessionUUID+"--annotations.jsonl")
	writeJSONLFile(t, jsonlPath, []export.ExportedAnnotation{record})

	// First import.
	stdout, stderr, err := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-file", jsonlPath})
	if err != nil {
		t.Fatalf("first import: unexpected error: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "imported 1 of 1 annotations (0 duplicates skipped, 0 errors)") {
		t.Errorf("first import: expected 'imported 1 of 1 annotations (0 duplicates skipped, 0 errors)'; got: %s", stdout)
	}

	// Second import (same file, same --data-dir).
	stdout2, stderr2, err2 := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-file", jsonlPath})
	if err2 != nil {
		t.Fatalf("second import: unexpected error: %v\nstdout: %s\nstderr: %s", err2, stdout2, stderr2)
	}
	if !strings.Contains(stdout2, "imported 0 of 1 annotations (1 duplicates skipped, 0 errors)") {
		t.Errorf("second import: expected 'imported 0 of 1 annotations (1 duplicates skipped, 0 errors)'; got: %s", stdout2)
	}

	// Exactly 1 non-superseded annotation should exist.
	db := openTestDB(t, dataHome)
	ctx := context.Background()
	rows, err := db.GetAnnotationsForSession(ctx, testSessionUUID)
	if err != nil {
		t.Fatalf("get annotations: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected exactly 1 annotation after two idempotent imports; got %d", len(rows))
	}

	// Verify content_hash is non-empty via direct pool query (TPT: session_id is in annotation_target_sessions).
	conn, connErr := db.Pool().Take(ctx)
	if connErr != nil {
		t.Fatalf("take connection: %v", connErr)
	}
	defer db.Pool().Put(conn)
	var hash string
	if qErr := sqlitex.ExecuteTransient(conn,
		`SELECT COALESCE(a.content_hash,'')
		 FROM annotations a
		 JOIN annotation_target_sessions ts ON ts.annotation_id = a.id
		 WHERE ts.session_id = ? AND a.superseded_by IS NULL LIMIT 1`,
		&sqlitex.ExecOptions{
			Args: []any{testSessionUUID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				hash = stmt.ColumnText(0)
				return nil
			},
		},
	); qErr != nil {
		t.Fatalf("query content_hash: %v", qErr)
	}
	if hash == "" {
		t.Error("expected non-empty content_hash on imported annotation")
	}
}

// TestCLI_AnnotateImportSupersede verifies that importing a second annotation for the
// same target+type+annotator with a different value supersedes the old one, resulting
// in exactly one non-superseded annotation with the new value.
func TestCLI_AnnotateImportSupersede(t *testing.T) {
	t.Parallel()
	dataHome := t.TempDir()
	seedTestSessionInto(t, dataHome, testSessionUUID)

	// First import: value="approve", confidence=0.8.
	record1 := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "approve",
		Annotator:     "supersede-test-annotator",
		AnnotatorKind: schema.AnnotatorHuman.String(),
		Confidence:    ptrFloat64(0.8),
		CreatedAt:     21000,
	}
	tmpDir := t.TempDir()
	jsonlPath1 := filepath.Join(tmpDir, "first.jsonl")
	writeJSONLFile(t, jsonlPath1, []export.ExportedAnnotation{record1})

	stdout1, stderr1, err1 := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-file", jsonlPath1})
	if err1 != nil {
		t.Fatalf("first import: unexpected error: %v\nstdout: %s\nstderr: %s", err1, stdout1, stderr1)
	}
	if !strings.Contains(stdout1, "imported 1 of 1 annotations (0 duplicates skipped, 0 errors)") {
		t.Errorf("first import: expected 'imported 1 of 1 annotations (0 duplicates skipped, 0 errors)'; got: %s", stdout1)
	}

	// Second import: same type+annotator+session, different value → hash mismatch → supersede.
	record2 := export.ExportedAnnotation{
		SessionID:     testSessionUUID,
		TypeID:        testutil.TestTypeIDSessionApproval,
		Value:         "deny",
		Annotator:     "supersede-test-annotator",
		AnnotatorKind: schema.AnnotatorHuman.String(),
		Confidence:    ptrFloat64(0.9),
		CreatedAt:     22000,
	}
	jsonlPath2 := filepath.Join(tmpDir, "second.jsonl")
	writeJSONLFile(t, jsonlPath2, []export.ExportedAnnotation{record2})

	stdout2, stderr2, err2 := executeAnnotateImportCmd(t, dataHome, []string{"import", "--from-file", jsonlPath2})
	if err2 != nil {
		t.Fatalf("second import: unexpected error: %v\nstdout: %s\nstderr: %s", err2, stdout2, stderr2)
	}
	if !strings.Contains(stdout2, "imported 1 of 1 annotations (0 duplicates skipped, 0 errors)") {
		t.Errorf("second import: expected 'imported 1 of 1 annotations (0 duplicates skipped, 0 errors)'; got: %s", stdout2)
	}

	// Exactly 1 non-superseded annotation should remain, with value="deny".
	db := openTestDB(t, dataHome)
	ctx := context.Background()
	rows, err := db.GetAnnotationsForSession(ctx, testSessionUUID)
	if err != nil {
		t.Fatalf("get annotations: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected exactly 1 non-superseded annotation after supersede; got %d", len(rows))
	}
	if len(rows) == 1 && rows[0].Value != "deny" {
		t.Errorf("expected value 'deny' on surviving annotation; got %q", rows[0].Value)
	}

	// Verify content_hash is non-empty on the new annotation.
	conn, connErr := db.Pool().Take(ctx)
	if connErr != nil {
		t.Fatalf("take connection: %v", connErr)
	}
	defer db.Pool().Put(conn)
	var hash string
	if qErr := sqlitex.ExecuteTransient(conn,
		`SELECT COALESCE(a.content_hash,'')
		 FROM annotations a
		 JOIN annotation_target_sessions ts ON ts.annotation_id = a.id
		 WHERE ts.session_id = ? AND a.superseded_by IS NULL LIMIT 1`,
		&sqlitex.ExecOptions{
			Args: []any{testSessionUUID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				hash = stmt.ColumnText(0)
				return nil
			},
		},
	); qErr != nil {
		t.Fatalf("query content_hash: %v", qErr)
	}
	if hash == "" {
		t.Error("expected non-empty content_hash on superseding annotation")
	}
}

// ptrFloat64 returns a pointer to the given float64. Test helper.
func ptrFloat64(v float64) *float64 { return &v }
