package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/opencode_metadata_half.yaml
var openCodeMetadataHalfYAML []byte

//go:embed testdata/opencode_metadata_half.manifest.yaml
var openCodeMetadataHalfManifestYAML []byte

//go:embed testdata/opencode_fixtures.manifest.yaml
var openCodeFixturesManifestYAML []byte

//go:embed testdata/opencode_erroring_branches.manifest.yaml
var openCodeErroringBranchesManifestYAML []byte

//go:embed testdata/opencode_metadata_half_mutations.yaml
var openCodeMetadataHalfMutationsYAML []byte

const openCodeSQLiteFixturePath = "internal/ingest/testfixture/testdata/opencode_sqlite.yaml"

type openCodeNonCorpusDocument struct {
	Path   string `yaml:"path"`
	Reason string `yaml:"reason"`
}

type openCodeFixturesManifest struct {
	Directories        []string                    `yaml:"directories"`
	Documents          []string                    `yaml:"documents"`
	NonCorpusDocuments []openCodeNonCorpusDocument `yaml:"nonCorpusDocuments"`
}

type openCodeErroringBranchesManifest struct {
	ErroringBranches []struct {
		Name   string `yaml:"name"`
		Reason string `yaml:"reason"`
	} `yaml:"erroringBranches"`
}

type openCodeHalfRelationshipSpec struct {
	Kind          string `yaml:"kind"`
	TargetState   string `yaml:"targetState"`
	TargetLocalID string `yaml:"targetLocalId"`
	Evidence      string `yaml:"evidence"`
}

type openCodeHalfRevertSpec struct {
	TargetSeq int64 `yaml:"targetSeq"`
	Committed bool  `yaml:"committed"`
}

type openCodeMetadataHalfRow struct {
	Name                      string                         `yaml:"name"`
	SessionID                 string                         `yaml:"sessionId"`
	ParentID                  string                         `yaml:"parentId"`
	SkipSeed                  bool                           `yaml:"skipSeed"`
	Rows                      []ocProvRow                    `yaml:"rows"`
	Fork                      *ocFlowFork                    `yaml:"fork"`
	NativeForkSourceID        string                         `yaml:"nativeForkSourceId"`
	Revert                    *openCodeHalfRevertSpec        `yaml:"revert"`
	RecordedRelationships     []openCodeHalfRelationshipSpec `yaml:"recordedRelationships"`
	SchemaVersion             *int                           `yaml:"schemaVersion"`
	ModelHarness              string                         `yaml:"modelHarness"`
	RootSessionID             *string                        `yaml:"rootSessionId"`
	Purpose                   string                         `yaml:"purpose"`
	HasCompleteGeneration     bool                           `yaml:"hasCompleteGeneration"`
	HasCaptureAuthority       bool                           `yaml:"hasCaptureAuthority"`
	HasCapturedPrefix         bool                           `yaml:"hasCapturedPrefix"`
	NilOpenCode               bool                           `yaml:"nilOpenCode"`
	NilRecordedMetadata       bool                           `yaml:"nilRecordedMetadata"`
	MismatchSnapshotSessionID string                         `yaml:"mismatchSnapshotSessionId"`
	CancelCtx                 bool                           `yaml:"cancelCtx"`
	PriorReadError            bool                           `yaml:"priorReadError"`
	InvalidAllocator          bool                           `yaml:"invalidAllocator"`
}

type openCodeMetadataHalfDocument struct {
	Rows             []openCodeMetadataHalfRow `yaml:"rows"`
	Branches         map[string]string         `yaml:"branches"`
	Outcomes         map[string]string         `yaml:"outcomes"`
	AfterHalfClasses map[string]string         `yaml:"afterHalfClasses"`
}

type openCodeHalfCorpusRow struct {
	name  string
	build func(t *testing.T) (context.Context, *ingest.OpenCodeIndexer, ingest.GenerationBuildInput)
}

var openCodeFixturePathMarker = string(schema.HarnessOpenCode)

func loadOpenCodeFixturesManifest(t *testing.T) openCodeFixturesManifest {
	t.Helper()
	var manifest openCodeFixturesManifest
	if err := testutil.DecodeFixtureYAML(openCodeFixturesManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Documents) == 0 {
		t.Fatal("the OpenCode fixture manifest names no documents")
	}
	return manifest
}

func loadOpenCodeMetadataHalfDocument(t *testing.T) openCodeMetadataHalfDocument {
	t.Helper()
	var document openCodeMetadataHalfDocument
	if err := testutil.DecodeFixtureYAML(openCodeMetadataHalfYAML, &document); err != nil {
		t.Fatalf("decode OpenCode metadata-half fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(openCodeMetadataHalfManifestYAML, "opencode metadata half")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(document.Rows))
	for _, row := range document.Rows {
		names = append(names, row.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "opencode metadata half"); err != nil {
		t.Fatal(err)
	}
	return document
}

// TestOpenCodeFixtureCorpusIsComplete fails when an OpenCode fixture exists
// under /testdata/ outside the partitioned directories/documents/nonCorpusDocuments
// manifest, or when a listed path is missing or lacks a non-corpus reason.
func TestOpenCodeFixtureCorpusIsComplete(t *testing.T) {
	manifest := loadOpenCodeFixturesManifest(t)
	docSet := make(map[string]bool, len(manifest.Documents))
	for _, doc := range manifest.Documents {
		if docSet[doc] {
			t.Errorf("duplicate corpus document %s", doc)
		}
		docSet[doc] = true
		if _, err := os.Stat(filepath.Join(moduleRootFromIngest, doc)); err != nil {
			t.Errorf("listed OpenCode corpus document %s: %v", doc, err)
		}
	}
	nonCorpusSet := make(map[string]bool, len(manifest.NonCorpusDocuments))
	for _, entry := range manifest.NonCorpusDocuments {
		if strings.TrimSpace(entry.Reason) == "" {
			t.Errorf("non-corpus OpenCode document %s has empty reason", entry.Path)
		}
		if nonCorpusSet[entry.Path] {
			t.Errorf("duplicate non-corpus OpenCode document %s", entry.Path)
		}
		if docSet[entry.Path] {
			t.Errorf("OpenCode document %s is listed in both documents and nonCorpusDocuments", entry.Path)
		}
		nonCorpusSet[entry.Path] = true
		if _, err := os.Stat(filepath.Join(moduleRootFromIngest, entry.Path)); err != nil {
			t.Errorf("listed non-corpus OpenCode document %s: %v", entry.Path, err)
		}
	}
	for _, dir := range manifest.Directories {
		if _, err := os.Stat(filepath.Join(moduleRootFromIngest, dir)); err != nil {
			t.Errorf("listed OpenCode fixture directory %s: %v", dir, err)
		}
	}

	var unlistedRaw []string
	var unlistedYAML []string
	err := filepath.WalkDir(moduleRootFromIngest, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(moduleRootFromIngest, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if slices.Contains(corpusWalkSkippedDirs, entry.Name()) {
				return filepath.SkipDir
			}
			if rel != "." {
				if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.Contains(rel, "/testdata/") {
			return nil
		}
		lower := strings.ToLower(rel)
		if !strings.Contains(lower, openCodeFixturePathMarker) {
			return nil
		}
		if strings.HasSuffix(lower, ".db") || strings.HasSuffix(lower, ".sqlite") || strings.HasSuffix(lower, ".json") {
			listed := false
			for _, dir := range manifest.Directories {
				if strings.HasPrefix(rel, dir+"/") {
					listed = true
					break
				}
			}
			if !listed {
				unlistedRaw = append(unlistedRaw, rel)
			}
			return nil
		}
		if (strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")) && !strings.HasSuffix(lower, ".manifest.yaml") {
			if !docSet[rel] && !nonCorpusSet[rel] {
				unlistedYAML = append(unlistedYAML, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(unlistedRaw) != 0 {
		t.Fatalf("raw OpenCode fixtures outside opencode_fixtures.manifest.yaml directories: %v", unlistedRaw)
	}
	if len(unlistedYAML) != 0 {
		t.Fatalf("OpenCode YAML fixtures outside documents/nonCorpusDocuments: %v", unlistedYAML)
	}
}

func openCodeHalfCorpus(t *testing.T) []openCodeHalfCorpusRow {
	t.Helper()
	var corpus []openCodeHalfCorpusRow

	sqlitePath := filepath.Join(moduleRootFromIngest, openCodeSQLiteFixturePath)
	sqliteYAML, err := os.ReadFile(sqlitePath)
	if err != nil {
		t.Fatalf("read opencode_sqlite.yaml: %v", err)
	}
	sqliteNames, err := testfixture.CaseNames(sqliteYAML)
	if err != nil {
		t.Fatalf("list opencode_sqlite.yaml case names: %v", err)
	}
	for _, name := range sqliteNames {
		caseName := name
		corpus = append(corpus, openCodeHalfCorpusRow{
			name: "sqlite/" + caseName,
			build: func(t *testing.T) (context.Context, *ingest.OpenCodeIndexer, ingest.GenerationBuildInput) {
				source := testfixture.MaterializeByName(t, caseName)
				sid := firstSQLiteSessionID(t, source.Path)
				adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
				metadata := schema.UnifiedMetadata{
					SchemaVersion: ingest.CurrentSchemaVersion,
					SessionID:     schema.SessionID(sid),
					ModelHarness:  schema.HarnessOpenCode,
					Source:        schema.SourceInfo{FilePath: source.Path},
				}
				return t.Context(), ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}), ingest.GenerationBuildInput{
					Session: ingest.DiscoveredSession{
						SessionID:        ingest.SessionID(sid),
						Harness:          ingest.HarnessOpenCode,
						TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
						SourcePath:       ingest.ResolvedPath(source.Path),
					},
					GenerationID:     "gen-sqlite-" + caseName,
					RecordedMetadata: &metadata,
					Prior:            generationTestPriorSource{},
					OpenCode: generationTestOpenCodeSource{snapshot: func(ctx context.Context, locator string) (ingest.OpenCodeHistorySnapshot, error) {
						return adapter.SnapshotOpenCodeProvenance(ctx, locator, sid, ingest.OpenCodeSnapshotOptions{})
					}},
				}
			},
		})
	}

	for _, row := range loadOpenCodeProvenanceCases(t) {
		provRow := row
		corpus = append(corpus, openCodeHalfCorpusRow{
			name: "provenance/" + provRow.Name,
			build: func(t *testing.T) (context.Context, *ingest.OpenCodeIndexer, ingest.GenerationBuildInput) {
				source := testfixture.MaterializeByName(t, "native-current-rows")
				seedOpenCodeProvenanceCase(t, source, provRow)
				options := ingest.OpenCodeSnapshotOptions{}
				if provRow.AgentDeliver {
					delivered := make(map[string]bool, len(provRow.Rows))
					for _, native := range provRow.Rows {
						delivered[native.ID] = true
					}
					options.AgentDeliveredIDs = delivered
				}
				adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
				metadata := schema.UnifiedMetadata{
					SchemaVersion: ingest.CurrentSchemaVersion,
					SessionID:     schema.SessionID(provRow.Scope.SessionID),
					ModelHarness:  schema.HarnessOpenCode,
					Source:        schema.SourceInfo{FilePath: source.Path},
				}
				return t.Context(), ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}), ingest.GenerationBuildInput{
					Session: ingest.DiscoveredSession{
						SessionID:        ingest.SessionID(provRow.Scope.SessionID),
						Harness:          ingest.HarnessOpenCode,
						TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
						SourcePath:       ingest.ResolvedPath(source.Path),
					},
					GenerationID:     "gen-prov-" + provRow.Name,
					RecordedMetadata: &metadata,
					Prior:            generationTestPriorSource{},
					OpenCode: generationTestOpenCodeSource{snapshot: func(ctx context.Context, locator string) (ingest.OpenCodeHistorySnapshot, error) {
						return adapter.SnapshotOpenCodeProvenance(ctx, locator, provRow.Scope.SessionID, options)
					}},
				}
			},
		})
	}

	for _, row := range loadOpenCodeProvenanceFlowDocument(t).Cases {
		flowRow := row
		corpus = append(corpus, openCodeHalfCorpusRow{
			name: "flow/" + flowRow.Name,
			build: func(t *testing.T) (context.Context, *ingest.OpenCodeIndexer, ingest.GenerationBuildInput) {
				sourceName := flowRow.Source
				if sourceName == "" {
					sourceName = "native-current-rows"
				}
				source := testfixture.MaterializeByName(t, sourceName)
				session := openCodeFlowSession(flowRow)
				var fork *ingest.OpenCodeForkProof
				if flowRow.ForkParent != nil {
					seedOpenCodeFlowForkParent(t, source, flowRow)
					fork = openCodeFlowFork(flowRow.Fork)
				} else if flowRow.Fork != nil {
					fork = openCodeFlowFork(flowRow.Fork)
				}
				prior := ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()}
				if flowRow.Kind == "authority-bridge" {
					prior.CaptureAuthority = &ingest.StoredCaptureAuthority{
						Status:           ingest.ContentCaptureIncomplete,
						CaptureFormat:    ingest.ContentCaptureFormatFull,
						TranscriptOrigin: openCodeFlowOrigin(t, flowRow.AuthorityOrigin),
						SourceAuthority:  ingest.ContentSourceAuthority(flowRow.AuthoritySource),
						FailureCode:      ingest.ContentCaptureFailureCode(flowRow.AuthorityCode),
					}
				}
				input := openCodeFlowBuildInput(t, source, session, prior, fork)
				return t.Context(), ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}), input
			},
		})
	}

	for _, row := range loadOpenCodeMetadataHalfDocument(t).Rows {
		halfRow := row
		corpus = append(corpus, openCodeHalfCorpusRow{
			name: "half/" + halfRow.Name,
			build: func(t *testing.T) (context.Context, *ingest.OpenCodeIndexer, ingest.GenerationBuildInput) {
				return buildOpenCodeMetadataHalfRow(t, halfRow)
			},
		})
	}

	return corpus
}

func firstSQLiteSessionID(t *testing.T, dbPath string) string {
	t.Helper()
	const fallback = "ses_3cd91f52effeXd3QAJ54jCtrl1"
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadOnly)
	if err != nil {
		return fallback
	}
	defer func() { _ = conn.Close() }()
	for _, table := range []string{"session_v2", "session"} {
		var sid string
		err := sqlitex.ExecuteTransient(conn, "SELECT id FROM "+table+" ORDER BY id LIMIT 1", &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				sid = stmt.ColumnText(0)
				return nil
			},
		})
		if err == nil && strings.TrimSpace(sid) != "" {
			return sid
		}
	}
	return fallback
}

type errPriorSource struct {
	err error
}

func (s errPriorSource) ReadGenerationPrior(context.Context) (*ingest.GenerationPrior, error) {
	return nil, s.err
}

func (s errPriorSource) ReadStoredCaptureAuthority(context.Context) (*ingest.StoredCaptureAuthority, error) {
	return nil, s.err
}

func buildOpenCodeMetadataHalfRow(t *testing.T, row openCodeMetadataHalfRow) (context.Context, *ingest.OpenCodeIndexer, ingest.GenerationBuildInput) {
	t.Helper()
	source := testfixture.MaterializeByName(t, "native-current-rows")
	if !row.SkipSeed {
		seedOpenCodeProvenanceCase(t, source, ocProvCase{
			Scope: ocProvScope{
				SessionID: row.SessionID,
				Shape:     "current",
				HasParent: row.ParentID != "",
			},
			Rows: row.Rows,
		})
		if row.ParentID != "" {
			conn := openOpenCodeFlowConnection(t, source)
			if err := sqlitex.Execute(conn, "UPDATE session SET parent_id = ?1 WHERE id = ?2", &sqlitex.ExecOptions{Args: []any{row.ParentID, row.SessionID}}); err != nil {
				_ = conn.Close()
				t.Fatalf("update parent_id for %s: %v", row.Name, err)
			}
			_ = conn.Close()
		}
	}

	ctx := t.Context()
	if row.CancelCtx {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		ctx = cancelled
	}

	schemaVersion := ingest.CurrentSchemaVersion
	if row.SchemaVersion != nil {
		schemaVersion = *row.SchemaVersion
	}
	harness := schema.HarnessOpenCode
	if row.ModelHarness != "" {
		harness = schema.Harness(row.ModelHarness)
	}
	metadata := schema.UnifiedMetadata{
		SchemaVersion: schemaVersion,
		SessionID:     schema.SessionID(row.SessionID),
		ModelHarness:  harness,
		Purpose:       schema.SessionPurpose(row.Purpose),
		Source:        schema.SourceInfo{FilePath: source.Path},
	}
	if row.RootSessionID != nil {
		rootID := schema.SessionID(*row.RootSessionID)
		metadata.RootSessionID = &rootID
	}
	for _, rel := range row.RecordedRelationships {
		var targetPtr *schema.SessionID
		if rel.TargetLocalID != "" {
			tid := schema.SessionID(rel.TargetLocalID)
			targetPtr = &tid
		}
		metadata.Relationships = append(metadata.Relationships, schema.SessionRelationship{
			Kind:          schema.SessionRelationshipKind(rel.Kind),
			TargetState:   schema.RelationshipTargetState(rel.TargetState),
			TargetLocalID: targetPtr,
			Evidence:      schema.EvidenceKind(rel.Evidence),
		})
	}

	var recordedPtr *schema.UnifiedMetadata
	if !row.NilRecordedMetadata {
		recordedPtr = &metadata
	}

	priorDoc := ingest.OpenCodeProvenancePrior{
		Aliases:               ingest.NewProjectionPriorState(),
		HasCompleteGeneration: row.HasCompleteGeneration,
	}
	if row.HasCaptureAuthority {
		priorDoc.CaptureAuthority = &ingest.StoredCaptureAuthority{
			Status:           ingest.ContentCaptureIncomplete,
			CaptureFormat:    ingest.ContentCaptureFormatFull,
			TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
			SourceAuthority:  ingest.ContentSourceProviderSource,
			FailureCode:      "source_records_omitted",
		}
	}
	if row.HasCapturedPrefix {
		priorDoc.HasCapturedPrefix = true
		priorDoc.CapturedPrefix = []ingest.OpenCodeHistoryRow{{
			MessageID:  "copy-msg_parent_retained_1",
			Shape:      ingest.OpenCodeProvenanceCurrent,
			Seq:        0,
			HasSeq:     true,
			NativeType: "user",
			CreatedMs:  500,
			UpdatedMs:  500,
			Settled:    true,
			Message: ingest.OpenCodeProvenanceMessage{
				MessageID:   "copy-msg_parent_retained_1",
				SessionID:   row.SessionID,
				Shape:       ingest.OpenCodeProvenanceCurrent,
				NativeType:  "user",
				Text:        "retained parent prompt",
				TimeCreated: 500,
			},
		}}
	}

	var priorSource ingest.GenerationPriorSource = generationTestPriorFromOpenCode(t, priorDoc)
	if row.PriorReadError {
		priorSource = errPriorSource{err: errors.New("simulated prior read failure")}
	}

	var openCodeAccess ingest.OpenCodeSourceAccess
	if !row.NilOpenCode {
		adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
		opts := ingest.OpenCodeSnapshotOptions{Fork: openCodeFlowFork(row.Fork)}
		if row.NativeForkSourceID != "" {
			var linkID ingest.OpenCodeSessionLinkID
			if row.NativeForkSourceID != "<empty>" {
				var err error
				linkID, err = ingest.NewOpenCodeSessionLinkID(row.NativeForkSourceID)
				if err != nil {
					t.Fatalf("NewOpenCodeSessionLinkID(%q): %v", row.NativeForkSourceID, err)
				}
			}
			opts.NativeFork = &ingest.OpenCodeForkEvidence{
				SourceSessionID:   linkID,
				BoundaryType:      ingest.OpenCodeForkBoundaryThrough,
				BoundaryMessageID: "msg_boundary",
			}
		}
		if row.Revert != nil {
			opts.Revert = &ingest.OpenCodeRevertState{
				TargetSeq: row.Revert.TargetSeq,
				Committed: row.Revert.Committed,
			}
		}
		openCodeAccess = generationTestOpenCodeSource{snapshot: func(snapCtx context.Context, locator string) (ingest.OpenCodeHistorySnapshot, error) {
			snap, err := adapter.SnapshotOpenCodeProvenance(snapCtx, locator, row.SessionID, opts)
			if err != nil {
				return ingest.OpenCodeHistorySnapshot{}, err
			}
			if row.MismatchSnapshotSessionID != "" {
				snap.SessionID = row.MismatchSnapshotSessionID
			}
			return snap, nil
		}}
	}

	var allocator ingest.RefAllocator
	if row.InvalidAllocator {
		allocator = openCodeInvalidRefAllocator{}
	}

	return ctx, ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}), ingest.GenerationBuildInput{
		Session: ingest.DiscoveredSession{
			SessionID:        ingest.SessionID(row.SessionID),
			Harness:          ingest.HarnessOpenCode,
			TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
			SourcePath:       ingest.ResolvedPath(source.Path),
		},
		GenerationID:     "gen-half-" + row.Name,
		RecordedMetadata: recordedPtr,
		Prior:            priorSource,
		Allocator:        allocator,
		OpenCode:         openCodeAccess,
	}
}

type openCodeHalfObservation struct {
	half     ingest.GenerationMetadataCandidate
	halfErr  error
	build    ingest.GenerationCandidate
	buildErr error
}

func cloneUnifiedMetadataForTest(src *schema.UnifiedMetadata) *schema.UnifiedMetadata {
	if src == nil {
		return nil
	}
	cp := *src
	if src.RootSessionID != nil {
		v := *src.RootSessionID
		cp.RootSessionID = &v
	}
	if src.ParentUUID != nil {
		v := *src.ParentUUID
		cp.ParentUUID = &v
	}
	if src.Relationships != nil {
		cp.Relationships = append([]schema.SessionRelationship(nil), src.Relationships...)
	}
	if src.Diagnostics.Warnings != nil {
		cp.Diagnostics.Warnings = append([]schema.DiagnosticEntry(nil), src.Diagnostics.Warnings...)
	}
	return &cp
}

func observeOpenCodeHalf(t *testing.T, row openCodeHalfCorpusRow) openCodeHalfObservation {
	t.Helper()
	ctx, idx, input := row.build(t)
	before := cloneUnifiedMetadataForTest(input.RecordedMetadata)
	var observed openCodeHalfObservation
	observed.half, observed.halfErr = idx.BuildGenerationMetadata(ctx, input)
	observed.build, observed.buildErr = idx.BuildGeneration(ctx, input)
	if !reflect.DeepEqual(before, input.RecordedMetadata) {
		t.Fatalf("the half and the builder changed the recorded metadata:\nbefore %+v\nafter  %+v", before, input.RecordedMetadata)
	}
	return observed
}

func classifyOpenCodeHalf(observed openCodeHalfObservation) string {
	switch {
	case observed.halfErr == nil && observed.buildErr == nil:
		return halfOutcomeEqual
	case observed.halfErr != nil && observed.buildErr != nil:
		return halfOutcomeBothRefuse
	case observed.halfErr == nil:
		return halfOutcomeBuilderRefusesAfterHalf
	default:
		return halfOutcomeHalfErrors
	}
}

func openCodeHalfDifference(observed openCodeHalfObservation) string {
	generation := observed.build.Result.Generation
	if !reflect.DeepEqual(metadataWithoutStats(observed.half.Metadata), metadataWithoutStats(generation.Metadata)) {
		half, _ := json.Marshal(metadataWithoutStats(observed.half.Metadata))
		build, _ := json.Marshal(metadataWithoutStats(generation.Metadata))
		return "metadata differs:\nhalf    " + string(half) + "\nbuilder " + string(build)
	}
	if !slices.EqualFunc(observed.half.Metadata.Diagnostics.Warnings, generation.Metadata.Diagnostics.Warnings, func(a, b schema.DiagnosticEntry) bool { return reflect.DeepEqual(a, b) }) {
		return "warnings differ in content or order"
	}
	if observed.half.Completeness != generation.Completeness {
		return "completeness " + string(observed.half.Completeness) + " != " + string(generation.Completeness)
	}
	if observed.half.SourceEvidenceDigest == "" || observed.half.SourceEvidenceDigest != generation.SourceEvidenceDigest {
		return "source evidence digest differs or is empty"
	}
	return ""
}

func sameOpenCodeRefusal(halfErr, buildErr error) bool {
	if halfErr == nil || buildErr == nil {
		return false
	}
	var halfSnap, buildSnap *ingest.OpenCodeSnapshotError
	if errors.As(halfErr, &halfSnap) || errors.As(buildErr, &buildSnap) {
		if !errors.As(halfErr, &halfSnap) || !errors.As(buildErr, &buildSnap) {
			return false
		}
		if halfSnap.Step != buildSnap.Step || halfSnap.Reason != buildSnap.Reason || halfSnap.Recovery != buildSnap.Recovery {
			return false
		}
		if errors.Is(halfErr, context.Canceled) != errors.Is(buildErr, context.Canceled) {
			return false
		}
		if errors.Is(halfErr, context.DeadlineExceeded) != errors.Is(buildErr, context.DeadlineExceeded) {
			return false
		}
		return true
	}
	var halfInc, buildInc *ingest.OpenCodeIncompleteProvenanceError
	if errors.As(halfErr, &halfInc) || errors.As(buildErr, &buildInc) {
		if !errors.As(halfErr, &halfInc) || !errors.As(buildErr, &buildInc) {
			return false
		}
		return halfInc.Completeness == buildInc.Completeness && slices.Equal(halfInc.Diagnostics, buildInc.Diagnostics)
	}
	return halfErr.Error() == buildErr.Error()
}

// TestOpenCodeMetadataHalfMatchesTheBuilder runs BuildGenerationMetadata and
// BuildGeneration over every OpenCode corpus row and asserts the three-way
// outcome, parity, and exact sanitized refusal equality.
func TestOpenCodeMetadataHalfMatchesTheBuilder(t *testing.T) {
	document := loadOpenCodeMetadataHalfDocument(t)
	var erroring openCodeErroringBranchesManifest
	if err := testutil.DecodeFixtureYAML(openCodeErroringBranchesManifestYAML, &erroring); err != nil {
		t.Fatal(err)
	}
	if len(erroring.ErroringBranches) != 0 {
		t.Fatalf("opencode_erroring_branches.manifest.yaml must be empty, got %d entries", len(erroring.ErroringBranches))
	}
	corpus := openCodeHalfCorpus(t)
	names := map[string]bool{}
	for _, row := range corpus {
		names[row.name] = true
	}
	for branch, row := range document.Branches {
		if !names[row] {
			t.Errorf("branch %s names corpus row %s, which does not exist", branch, row)
		}
	}
	for row := range document.Outcomes {
		if !names[row] {
			t.Errorf("an outcome names corpus row %s, which does not exist", row)
		}
	}
	for _, row := range corpus {
		t.Run(row.name, func(t *testing.T) {
			observed := observeOpenCodeHalf(t, row)
			got := classifyOpenCodeHalf(observed)
			want := document.Outcomes[row.name]
			if want == "" {
				want = halfOutcomeEqual
			}
			if got != want {
				t.Fatalf("outcome %s, want %s (half error %v, builder error %v)", got, want, observed.halfErr, observed.buildErr)
			}
			switch got {
			case halfOutcomeEqual:
				if difference := openCodeHalfDifference(observed); difference != "" {
					t.Fatal(difference)
				}
			case halfOutcomeBothRefuse:
				if !sameOpenCodeRefusal(observed.halfErr, observed.buildErr) {
					t.Fatalf("the half and the builder refuse differently:\nhalf    %v\nbuilder %v", observed.halfErr, observed.buildErr)
				}
			case halfOutcomeBuilderRefusesAfterHalf:
				if document.AfterHalfClasses[row.name] == "" {
					t.Fatalf("row %s refuses after the half but names no refusal class", row.name)
				}
			}
		})
	}
}

// TestOpenCodeMetadataHalfAcceptsEmptyGenerationID verifies BuildGenerationMetadata
// succeeds with an empty GenerationID and matches BuildGeneration with a
// non-empty GenerationID, while BuildGeneration still refuses empty GenerationID.
func TestOpenCodeMetadataHalfAcceptsEmptyGenerationID(t *testing.T) {
	row := openCodeHalfCorpus(t)
	var target *openCodeHalfCorpusRow
	for i := range row {
		if row[i].name == "half/ordinary-current-session" {
			target = &row[i]
			break
		}
	}
	if target == nil {
		t.Fatal("missing half/ordinary-current-session row")
	}
	ctx, idx, input := target.build(t)
	full, err := idx.BuildGeneration(ctx, input)
	if err != nil {
		t.Fatalf("BuildGeneration with non-empty GenerationID: %v", err)
	}
	emptyInput := input
	emptyInput.GenerationID = ""
	half, err := idx.BuildGenerationMetadata(ctx, emptyInput)
	if err != nil {
		t.Fatalf("BuildGenerationMetadata with empty GenerationID: %v", err)
	}
	if diff := openCodeHalfDifference(openCodeHalfObservation{half: half, build: full}); diff != "" {
		t.Fatal(diff)
	}
	if _, err := idx.BuildGeneration(ctx, emptyInput); err == nil || !strings.Contains(err.Error(), "misses its generation identity") {
		t.Fatalf("BuildGeneration with empty GenerationID = %v, want generation identity refusal", err)
	}
}

// TestOpenCodeHalfThenBuildLeavesInputsUnchanged deep-copies RecordedMetadata
// and the snapshot before running BuildGenerationMetadata then BuildGeneration
// and asserts both inputs are byte-identical afterward.
func TestOpenCodeHalfThenBuildLeavesInputsUnchanged(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")
	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	forkLinkID, err := ingest.NewOpenCodeSessionLinkID("ses_opencodeForkSource")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := adapter.SnapshotOpenCodeProvenance(t.Context(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{
		NativeFork: &ingest.OpenCodeForkEvidence{
			SourceSessionID:   forkLinkID,
			BoundaryType:      ingest.OpenCodeForkBoundaryThrough,
			BoundaryMessageID: "msg_boundary",
		},
	})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
	}
	snapBefore, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	metadata := schema.UnifiedMetadata{
		SchemaVersion: ingest.CurrentSchemaVersion,
		SessionID:     schema.SessionID(openCodeNativeChild),
		ModelHarness:  schema.HarnessOpenCode,
		Source:        schema.SourceInfo{FilePath: source.Path},
	}
	metaBefore, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	input := ingest.GenerationBuildInput{
		Session: ingest.DiscoveredSession{
			SessionID:        ingest.SessionID(openCodeNativeChild),
			Harness:          ingest.HarnessOpenCode,
			TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
			SourcePath:       ingest.ResolvedPath(source.Path),
		},
		GenerationID:     "gen-immutability",
		RecordedMetadata: &metadata,
		Prior:            generationTestPriorSource{},
		OpenCode: generationTestOpenCodeSource{snapshot: func(context.Context, string) (ingest.OpenCodeHistorySnapshot, error) {
			return snap, nil
		}},
	}
	idx := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{})
	if _, err := idx.BuildGenerationMetadata(t.Context(), input); err != nil {
		t.Fatalf("BuildGenerationMetadata: %v", err)
	}
	if _, err := idx.BuildGeneration(t.Context(), input); err != nil {
		t.Fatalf("BuildGeneration: %v", err)
	}
	snapAfter, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	metaAfter, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if string(snapBefore) != string(snapAfter) {
		t.Fatalf("snapshot mutated:\nbefore %s\nafter  %s", snapBefore, snapAfter)
	}
	if string(metaBefore) != string(metaAfter) {
		t.Fatalf("RecordedMetadata mutated:\nbefore %s\nafter  %s", metaBefore, metaAfter)
	}
}

// TestOpenCodeRefusalAfterTheHalfLeavesNoGeneration runs every corpus row
// classified as builder-refuses-after-half: the metadata half succeeds and the
// full builder refuses, so a refresh that validated would still parse and
// refuse. The pipeline consequence (the active generation is kept) is asserted
// on a real store by the incomplete-over-complete refresh route.
func TestOpenCodeRefusalAfterTheHalfLeavesNoGeneration(t *testing.T) {
	document := loadOpenCodeMetadataHalfDocument(t)
	rows := 0
	for _, row := range openCodeHalfCorpus(t) {
		if document.Outcomes[row.name] != halfOutcomeBuilderRefusesAfterHalf {
			continue
		}
		rows++
		t.Run(row.name, func(t *testing.T) {
			ctx, idx, input := row.build(t)
			if _, err := idx.BuildGenerationMetadata(ctx, input); err != nil {
				t.Fatalf("half refused unexpectedly: %v", err)
			}
			if _, err := idx.BuildGeneration(ctx, input); err == nil {
				t.Fatal("BuildGeneration succeeded, want post-half refusal")
			}
		})
	}
	if rows == 0 {
		t.Fatal("no corpus row refuses after the half; the fixture lost its builder-refuses-after-half case")
	}
}

func openCodeBuilderCandidateJSON(t *testing.T, ctx context.Context, idx *ingest.OpenCodeIndexer, input ingest.GenerationBuildInput) []byte {
	t.Helper()
	candidate, err := idx.BuildGeneration(ctx, input)
	if err != nil {
		return []byte("refused: " + err.Error())
	}
	encoded, err := json.Marshal(candidate.Result.Generation.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestOpenCodeBuilderIgnoresTheHalfMutation sets the metadata-half mutation
// hook and shows BuildGeneration's output does not move while the half mutates.
func TestOpenCodeBuilderIgnoresTheHalfMutation(t *testing.T) {
	type built struct {
		name     string
		ctx      context.Context
		idx      *ingest.OpenCodeIndexer
		input    ingest.GenerationBuildInput
		baseline []byte
	}
	var rows []built
	for _, row := range openCodeHalfCorpus(t) {
		ctx, idx, input := row.build(t)
		rows = append(rows, built{
			name:     row.name,
			ctx:      ctx,
			idx:      idx,
			input:    input,
			baseline: openCodeBuilderCandidateJSON(t, ctx, idx, input),
		})
	}
	ingest.SetOpenCodeMetadataHalfMutationForTest(t, func(_ ingest.GenerationBuildInput, candidate *ingest.GenerationMetadataCandidate, _ *error) {
		candidate.Metadata.Project.Name = "mutated by the half hook"
		candidate.Metadata.Relationships = nil
		candidate.Metadata.Diagnostics.Warnings = append(candidate.Metadata.Diagnostics.Warnings, schema.DiagnosticEntry{ErrorType: "half-hook"})
	})
	mutatedHalf := 0
	for _, row := range rows {
		if got := openCodeBuilderCandidateJSON(t, row.ctx, row.idx, row.input); string(got) != string(row.baseline) {
			t.Fatalf("%s: BuildGeneration changed under the half's mutation hook:\nwithout %s\nwith    %s", row.name, row.baseline, got)
		}
		if half, err := row.idx.BuildGenerationMetadata(row.ctx, row.input); err == nil && half.Metadata.Project.Name == "mutated by the half hook" {
			mutatedHalf++
		}
	}
	if mutatedHalf == 0 {
		t.Fatal("the half's mutation hook changed no half output; the comparison above proves nothing")
	}
}

type openCodeMetadataHalfMutation struct {
	Name string `yaml:"name"`
}

func loadOpenCodeMetadataHalfMutations(t *testing.T) []openCodeMetadataHalfMutation {
	t.Helper()
	var fixture struct {
		Mutations []openCodeMetadataHalfMutation `yaml:"mutations"`
	}
	if err := testutil.DecodeFixtureYAML(openCodeMetadataHalfMutationsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range fixture.Mutations {
		names = append(names, m.Name)
	}
	want := []string{"skip-native-fork-context-from", "skip-retained-prefix-digest", "skip-incomplete-refusal"}
	if !slices.Equal(names, want) {
		t.Fatalf("metadata-half mutations = %v, want exactly %v", names, want)
	}
	return fixture.Mutations
}

func openCodeHalfMutationHook(t *testing.T, name string) func(ingest.GenerationBuildInput, *ingest.GenerationMetadataCandidate, *error) {
	t.Helper()
	switch name {
	case "skip-native-fork-context-from":
		return func(in ingest.GenerationBuildInput, candidate *ingest.GenerationMetadataCandidate, _ *error) {
			recordedContextFrom := false
			if in.RecordedMetadata != nil {
				for _, rel := range in.RecordedMetadata.Relationships {
					if rel.Kind == schema.SessionRelationshipContextFrom {
						recordedContextFrom = true
						break
					}
				}
			}
			if recordedContextFrom {
				return
			}
			kept := candidate.Metadata.Relationships[:0:0]
			for _, rel := range candidate.Metadata.Relationships {
				if rel.Kind == schema.SessionRelationshipContextFrom && rel.Evidence == schema.EvidenceNativeTyped {
					continue
				}
				kept = append(kept, rel)
			}
			candidate.Metadata.Relationships = kept
		}
	case "skip-retained-prefix-digest":
		return func(_ ingest.GenerationBuildInput, candidate *ingest.GenerationMetadataCandidate, _ *error) {
			candidate.SourceEvidenceDigest = ""
		}
	case "skip-incomplete-refusal":
		return func(_ ingest.GenerationBuildInput, _ *ingest.GenerationMetadataCandidate, incompleteErr *error) {
			if *incompleteErr != nil {
				*incompleteErr = nil
			}
		}
	}
	t.Fatalf("unknown metadata-half mutation %q", name)
	return nil
}

// TestOpenCodeMetadataHalfMutationsFailTheDifferential applies each named
// mutation through the half's hook and treats any outcome mismatch, equal
// diff, or refusal detail mismatch as caught.
func TestOpenCodeMetadataHalfMutationsFailTheDifferential(t *testing.T) {
	document := loadOpenCodeMetadataHalfDocument(t)
	corpus := openCodeHalfCorpus(t)
	for _, mutation := range loadOpenCodeMetadataHalfMutations(t) {
		t.Run(mutation.Name, func(t *testing.T) {
			ingest.SetOpenCodeMetadataHalfMutationForTest(t, openCodeHalfMutationHook(t, mutation.Name))
			var caught []string
			for _, row := range corpus {
				observed := observeOpenCodeHalf(t, row)
				got := classifyOpenCodeHalf(observed)
				want := document.Outcomes[row.name]
				if want == "" {
					want = halfOutcomeEqual
				}
				if got != want {
					caught = append(caught, row.name+" (outcome "+got+" != "+want+")")
					continue
				}
				if got == halfOutcomeEqual && openCodeHalfDifference(observed) != "" {
					caught = append(caught, row.name+" (diff)")
					continue
				}
				if got == halfOutcomeBothRefuse && !sameOpenCodeRefusal(observed.halfErr, observed.buildErr) {
					caught = append(caught, row.name+" (refusal mismatch)")
				}
			}
			if len(caught) == 0 {
				t.Fatal("the mutation passed the differential on every corpus row")
			}
			t.Logf("caught on %v", caught)
		})
	}
}
