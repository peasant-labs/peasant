package ingest_test

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/control_record_absence.yaml
var controlRecordAbsenceYAML []byte

// controlRecordAbsenceCase pins the stored shape of the bookkeeping records the
// empty-system-turn rule exists for. The rule is stated over role and depth,
// not over a record list, so its premise has to be proven at the write
// boundary: the rows it removes are stored as role=system, entry_type=text,
// part_type NULL, content_preview NULL, with a source ref the managed
// generation writes and the legacy read clears.
type controlRecordAbsenceCase struct {
	Name          string   `yaml:"name"`
	Managed       bool     `yaml:"managed"`
	Transcript    string   `yaml:"transcript"`
	AbsentRecords []string `yaml:"absent_records"`
}

func loadControlRecordAbsenceFixtures(t *testing.T) []controlRecordAbsenceCase {
	t.Helper()
	var document struct {
		Required []string                   `yaml:"required_names"`
		Cases    []controlRecordAbsenceCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(controlRecordAbsenceYAML, &document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range document.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid control-record absence fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range document.Required {
		if !names[name] {
			t.Fatalf("missing required control-record absence fixture %s", name)
		}
	}
	return document.Cases
}

// TestControlRecordAbsenceStoredShape harvests a real Claude transcript whose
// bookkeeping lines are not represented control records, then asserts the
// durable managed generation stored exactly the shape the projection rule
// removes: a headerless system text row with no part type and no preview.
func TestControlRecordAbsenceStoredShape(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadControlRecordAbsenceFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			sourceRoot := filepath.Join(root, "source")
			sourceDir := filepath.Join(sourceRoot, "-workspace")
			if err := os.MkdirAll(sourceDir, 0o700); err != nil {
				t.Fatal(err)
			}
			transcript := strings.ReplaceAll(fixture.Transcript, "SESSION_ID", testutil.TestSessionUUID)
			sourcePath := filepath.Join(sourceDir, testutil.TestSessionUUID+".jsonl")
			if err := os.WriteFile(sourcePath, []byte(transcript), 0o600); err != nil {
				t.Fatal(err)
			}
			path := storetest.CopyGoldenDB(t)
			db := nativeRepairStore(t, path, filepath.Join(root, "artifacts"))
			fs := &ingest.OSFileSystem{}
			cfg := ingest.PipelineConfig{
				Sources: map[ingest.Harness]ingest.SourceConfig{
					ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(sourceRoot)}},
				},
				OutputDir:   ingest.ResolvedPath(filepath.Join(root, "output")),
				Parallelism: 1,
			}
			pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
				ingest.WithStore(db), ingest.WithMetricsStore(db),
				ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
				ingest.WithHarvesterVersions(ingest.GenerationTargets(ingest.HarvesterVersionRegistry)),
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatalf("ordinary harvest: %v", err)
			}
			if result.Summary.New != 1 {
				t.Fatalf("ordinary harvest recorded %+v, want exactly one new session", result.Summary)
			}
			sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = nativeRepairStore(t, path, filepath.Join(root, "artifacts"))
			defer db.Close()

			var entries []schema.SessionEntry
			if err := db.WithSessionSnapshot(ctx, schema.SessionID(sid), func(read indexformat.ReadSnapshot) error {
				if read.IndexVersion != 2 || read.Completeness != indexformat.GenerationCompletenessComplete {
					return fmt.Errorf("managed generation shape is index format %d completeness %q, want format 2 complete", read.IndexVersion, read.Completeness)
				}
				entries = read.Main.Entries
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			wantAbsent := make(map[string]bool, len(fixture.AbsentRecords))
			for _, name := range fixture.AbsentRecords {
				wantAbsent[name] = true
			}
			if len(wantAbsent) == 0 {
				t.Fatal("fixture declares no absent records; the case would be vacuous")
			}
			absent := 0
			seenOpen := 0
			for _, entry := range entries {
				if entry.PartType != nil && wantAbsent[*entry.PartType] {
					t.Fatalf("bookkeeping record %q was stored as a represented part type; it must stay unrepresented", *entry.PartType)
				}
				if entry.Depth == 0 && (entry.Role == schema.RoleUser || entry.Role == schema.RoleAssistant) {
					seenOpen++
				}
				if entry.Role != schema.RoleSystem || entry.EntryType != schema.EntryTypeText || entry.Depth != 0 {
					continue
				}
				if entry.PartType != nil || entry.ContentPreview != nil || entry.HasToolUse {
					continue
				}
				if entry.SourceEntryRef == "" {
					t.Fatalf("headerless system text row at entry %d has no source ref; the managed generation must write one so the projection rule's premise holds", entry.EntryIndex)
				}
				absent++
			}
			if absent != len(wantAbsent) {
				t.Fatalf("stored headerless system text rows = %d, want %d (one per bookkeeping record %v); a shape change here invalidates the empty-turn suppression rule", absent, len(wantAbsent), fixture.AbsentRecords)
			}
			if seenOpen != 2 {
				t.Fatalf("stored conversation rows = %d, want 2", seenOpen)
			}
		})
	}
}
