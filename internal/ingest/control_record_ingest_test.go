package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/control_record_ingest.yaml
var controlRecordIngestYAML []byte

type controlRecordIngestCase struct {
	Name                string            `yaml:"name"`
	Managed             bool              `yaml:"managed"`
	Harness             ingest.Harness    `yaml:"harness"`
	Transcript          string            `yaml:"transcript"`
	CaptureStatus       string            `yaml:"capture_status"`
	CaptureFormat       string            `yaml:"capture_format"`
	FailureCode         string            `yaml:"failure_code"`
	ControlPartType     string            `yaml:"control_part_type"`
	ControlPartTypes    []string          `yaml:"control_part_types"`
	ControlPreview      string            `yaml:"control_preview"`
	ControlExtraMembers map[string]string `yaml:"control_extra_members"`
	ExportAccepted      bool              `yaml:"export_accepted"`
	PublicationReady    bool              `yaml:"publication_ready"`
	PreflightAccepts    bool              `yaml:"preflight_accepts"`
	RetainedKind        string            `yaml:"retained_kind"`
}

func loadControlRecordIngestFixtures(t *testing.T) []controlRecordIngestCase {
	t.Helper()
	var document struct {
		Required []string                  `yaml:"required_names"`
		Cases    []controlRecordIngestCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(controlRecordIngestYAML, &document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range document.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid control-record ingest fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range document.Required {
		if !names[name] {
			t.Fatalf("missing required control-record ingest fixture %s", name)
		}
	}
	return document.Cases
}

// TestControlRecordIngestExportAndPublication drives the REAL adapter, the REAL
// ordinary harvest, the REAL store writer and the REAL export and publication
// preparation over a mixed conversation/control session and its unknown-kind
// counterpart. The capture API alone cannot prove the ordinary path kept the
// control fields or that export and publication accept the stored capture.
func TestControlRecordIngestExportAndPublication(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadControlRecordIngestFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			harness := fixture.Harness
			if harness == "" {
				harness = ingest.HarnessClaudeCode
			}
			sourceRoot := filepath.Join(root, "source")
			sourceDir := filepath.Join(sourceRoot, "-workspace")
			if harness == ingest.HarnessCodex {
				sourceDir = filepath.Join(sourceRoot, "2026", "01", "01")
			}
			if err := os.MkdirAll(sourceDir, 0o700); err != nil {
				t.Fatal(err)
			}
			transcript := strings.ReplaceAll(fixture.Transcript, "SESSION_ID", testutil.TestSessionUUID)
			filename := testutil.TestSessionUUID + ".jsonl"
			if harness == ingest.HarnessCodex {
				filename = "rollout-2026-01-01T00-00-00-" + filename
			}
			sourcePath := filepath.Join(sourceDir, filename)
			if err := os.WriteFile(sourcePath, []byte(transcript), 0o600); err != nil {
				t.Fatal(err)
			}
			path := storetest.CopyGoldenDB(t)
			openStore := func() (*store.Store, error) {
				if fixture.Managed {
					return nativeRepairStore(t, path, filepath.Join(root, "artifacts")), nil
				}
				return store.Open(path, store.WithSkipMigrations())
			}
			db, err := openStore()
			if err != nil {
				t.Fatal(err)
			}
			fs := &ingest.OSFileSystem{}
			cfg := ingest.PipelineConfig{
				Sources: map[ingest.Harness]ingest.SourceConfig{
					harness: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(sourceRoot)}},
				},
				OutputDir:   ingest.ResolvedPath(filepath.Join(root, "output")),
				Parallelism: 1,
			}
			options := []ingest.PipelineOption{
				ingest.WithStore(db), ingest.WithMetricsStore(db),
				ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
			}
			if fixture.Managed {
				options = append(options, ingest.WithHarvesterVersions(ingest.GenerationTargets(ingest.HarvesterVersionRegistry)))
			}
			pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg, options...)
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

			// Close and reopen so the assertions run against the durable store,
			// not the writer's in-process state.
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = openStore()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			capture, found, err := db.GetSessionContentCapture(ctx, sid)
			if err != nil || !found {
				t.Fatalf("read the stored capture: found=%v err=%v", found, err)
			}
			if string(capture.Status) != fixture.CaptureStatus {
				t.Fatalf("capture status = %q, want %q", capture.Status, fixture.CaptureStatus)
			}
			if string(capture.CaptureFormat) != fixture.CaptureFormat {
				t.Fatalf("capture format = %q, want %q", capture.CaptureFormat, fixture.CaptureFormat)
			}
			if string(capture.FailureCode) != fixture.FailureCode {
				t.Fatalf("capture failure code = %q, want %q", capture.FailureCode, fixture.FailureCode)
			}
			if fixture.Managed {
				state, err := db.ReadIndexState(ctx, sid)
				if err != nil || state == nil || state.IndexVersion == nil || *state.IndexVersion != 2 {
					t.Fatalf("managed case stored index state %+v, want format 2 (%v)", state, err)
				}
			}
			var nativeEntries []schema.SessionEntry
			if fixture.Managed {
				if err := db.WithSessionSnapshot(ctx, schema.SessionID(sid), func(read indexformat.ReadSnapshot) error {
					if read.IndexVersion != 2 || read.Completeness != indexformat.GenerationCompletenessComplete {
						return fmt.Errorf("managed snapshot format=%d completeness=%q", read.IndexVersion, read.Completeness)
					}
					nativeEntries = read.Main.Entries
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}

			// The complete-content reader is the boundary export and publication
			// share. A certified capture must retain the control fields there.
			snapshot, readErr := db.ReadSessionContent(ctx, testutil.TestSessionUUID)
			if readErr != nil {
				if fixture.ExportAccepted {
					t.Fatalf("the complete-content reader refused a certified control capture: %v", readErr)
				}
			} else {
				if !fixture.ExportAccepted {
					t.Fatal("the complete-content reader accepted an incomplete capture")
				}
				if fixture.RetainedKind != "" {
					records, err := ingest.ProjectRetainedUnknown(snapshot.Entries, harness)
					if err != nil || len(records) != 1 || records[0].Kind != fixture.RetainedKind {
						t.Fatalf("retained evidence missing: %+v %v", records, err)
					}
				} else {
					if fixture.Managed {
						wanted := make(map[string]bool, len(fixture.ControlPartTypes))
						for _, kind := range fixture.ControlPartTypes {
							wanted[kind] = true
						}
						counts := make(map[string]int)
						refs := make(map[schema.SourceEntryRef]bool)
						for _, entry := range nativeEntries {
							if entry.PartType == nil || !wanted[*entry.PartType] {
								continue
							}
							counts[*entry.PartType]++
							if entry.Role != ingest.RoleSystem || entry.EntryType != ingest.EntryTypeSystem || entry.Depth != 0 {
								t.Fatalf("managed control %q has conversation shape: %+v", *entry.PartType, entry)
							}
							if entry.SourceEntryRef == "" || refs[entry.SourceEntryRef] {
								t.Fatalf("managed control %q has missing or duplicate source ref %q", *entry.PartType, entry.SourceEntryRef)
							}
							refs[entry.SourceEntryRef] = true
							if entry.RawByteLength == nil || *entry.RawByteLength <= 0 || entry.Extra == nil || len(*entry.Extra) > 8208 {
								t.Fatalf("managed control %q lost bounded source accounting", *entry.PartType)
							}
						}
						for kind := range wanted {
							if counts[kind] != 1 {
								t.Fatalf("stored managed generation has %d %q controls, want one", counts[kind], kind)
							}
						}
					}
					for _, kind := range fixture.ControlPartTypes {
						if findControlIngestEntry(snapshot.Entries, controlRecordIngestCase{ControlPartType: kind}) == nil {
							t.Fatalf("the full-content reader lost %q control", kind)
						}
					}
					entry := findControlIngestEntry(snapshot.Entries, fixture)
					if entry == nil {
						t.Fatalf("the stored session lost its %q control record", fixture.ControlPartType)
					}
					if entry.ContentPreview == nil || *entry.ContentPreview != fixture.ControlPreview {
						t.Fatalf("stored control preview = %v, want %q", entry.ContentPreview, fixture.ControlPreview)
					}
					if entry.Extra == nil {
						t.Fatal("stored control record lost its payload")
					}
					var extra map[string]json.RawMessage
					if err := json.Unmarshal([]byte(*entry.Extra), &extra); err != nil {
						t.Fatalf("stored control payload is not JSON: %v", err)
					}
					for key, want := range fixture.ControlExtraMembers {
						raw, ok := extra[key]
						if !ok {
							t.Fatalf("stored control payload lacks %q", key)
						}
						if got := canonicalJSONValue(t, string(raw)); got != canonicalJSONValue(t, want) {
							t.Fatalf("stored control payload[%q] = %s, want %s", key, got, want)
						}
					}
				}
			}

			exported, exportErr := export.ExportSession(ctx, db, fs, testutil.TestSessionUUID)
			if (exportErr == nil) != fixture.ExportAccepted {
				t.Fatalf("export accepted=%v, want %v (%v)", exportErr == nil, fixture.ExportAccepted, exportErr)
			}
			if fixture.ExportAccepted {
				if fixture.RetainedKind != "" && (exported.Diagnostics == nil || !exported.Diagnostics.Partial || len(exported.RetainedUnknown) != 1) {
					t.Fatal("export lost partial evidence")
				}
				if exported == nil || !strings.Contains(exportPayloadText(t, exported), fixture.ControlPreview) {
					t.Fatalf("exported detail does not carry the control preview %q", fixture.ControlPreview)
				}
			}

			bundle, _, err := push.LoadPublicationInput(ctx, db, testutil.TestSessionUUID)
			if err != nil {
				t.Fatalf("load the publication input: %v", err)
			}
			if (bundle.Readiness == ingest.PublicationReady) != fixture.PublicationReady {
				t.Fatalf("publication readiness = %q, want ready=%v", bundle.Readiness, fixture.PublicationReady)
			}
			if preflightErr := push.ValidatePublicationInput(bundle); (preflightErr == nil) != fixture.PreflightAccepts {
				t.Fatalf("publication preflight accepted=%v, want %v (%v)", preflightErr == nil, fixture.PreflightAccepts, preflightErr)
			}
			// The share scan redacts the same complete entries; a publishable
			// capture must pass its strict Extra decode too.
			if fixture.PublicationReady && readErr == nil {
				redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
				if err != nil {
					t.Fatal(err)
				}
				if _, scanErr := push.RedactEntries(redactor, snapshot.Entries); scanErr != nil {
					t.Fatalf("the share scan refused a publishable capture: %v", scanErr)
				}
			}
		})
	}
}

func findControlIngestEntry(entries []schema.SessionEntry, fixture controlRecordIngestCase) *schema.SessionEntry {
	for i := range entries {
		if entries[i].PartType != nil && *entries[i].PartType == fixture.ControlPartType {
			return &entries[i]
		}
	}
	return nil
}

func exportPayloadText(t *testing.T, payload *schema.SessionDetailPayload) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestControlPayloadReindexesOnUpgradeAndPublishes drives the upgrade path the
// Claude Code indexer bump exists for: a session indexed under the previous
// Claude Code indexer target is re-indexed by the next ordinary harvest of
// this build, with no command beyond that harvest, and then scans and
// publishes. The control payload holds a lone surrogate escape, so the
// published transcript and the redacted entries must carry only its identity
// object, never the payload text.
func TestControlPayloadReindexesOnUpgradeAndPublishes(t *testing.T) {
	t.Parallel()
	var fixture controlRecordIngestCase
	for _, candidate := range loadControlRecordIngestFixtures(t) {
		if candidate.Name == "control-payload-lone-surrogate-publishes" {
			fixture = candidate
		}
	}
	if fixture.Name == "" {
		t.Fatal("missing the control-payload-lone-surrogate-publishes fixture")
	}
	ctx := context.Background()
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source", "-workspace")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := strings.ReplaceAll(fixture.Transcript, "SESSION_ID", testutil.TestSessionUUID)
	if err := os.WriteFile(filepath.Join(sourceDir, testutil.TestSessionUUID+".jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	path := storetest.CopyGoldenDB(t)
	fs := &ingest.OSFileSystem{}
	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Join(root, "source"))}},
		},
		OutputDir:   ingest.ResolvedPath(filepath.Join(root, "output")),
		Parallelism: 1,
	}
	harvest := func(opts ...ingest.PipelineOption) int {
		t.Helper()
		db, err := store.Open(path, store.WithSkipMigrations())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg, append([]ingest.PipelineOption{
			ingest.WithStore(db), ingest.WithMetricsStore(db),
			ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(ctx)
		if err != nil {
			t.Fatalf("ordinary harvest: %v", err)
		}
		return result.Summary.Indexed
	}

	previous := maps.Clone(ingest.HarvesterVersionRegistry)
	claude := previous[ingest.HarnessClaudeCode]
	claude.IndexerVersion--
	previous[ingest.HarnessClaudeCode] = claude
	if indexed := harvest(ingest.WithHarvesterVersions(previous)); indexed != 1 {
		t.Fatalf("the harvest under the previous target indexed %d sessions, want 1", indexed)
	}
	// The upgrade: this build's targets re-index the settled session on the
	// next ordinary harvest, and a further harvest finds nothing to do.
	if indexed := harvest(); indexed != 1 {
		t.Fatalf("the first harvest after the upgrade re-indexed %d sessions, want 1", indexed)
	}
	if indexed := harvest(); indexed != 0 {
		t.Fatalf("a settled session was re-indexed again: %d", indexed)
	}

	db, err := store.Open(path, store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bundle, _, err := push.LoadPublicationInput(ctx, db, testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("load the publication input: %v", err)
	}
	if bundle.Readiness != ingest.PublicationReady {
		t.Fatalf("publication readiness = %q, want ready", bundle.Readiness)
	}
	if err := push.ValidatePublicationInput(bundle); err != nil {
		t.Fatalf("publication preflight: %v", err)
	}
	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatal(err)
	}
	redacted, err := push.RedactEntries(redactor, bundle.Entries)
	if err != nil {
		t.Fatalf("the share scan refused the re-indexed session: %v", err)
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	// The identity object stands in for the payload: its kind and size only.
	if strings.Contains(string(encoded), "build ok") || !strings.Contains(string(encoded), `\"rawBytes\"`) {
		t.Fatalf("the redacted entries leak the control payload or lost its identity object: %s", encoded)
	}
	exported, err := export.ExportSession(ctx, db, fs, testutil.TestSessionUUID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if strings.Contains(exportPayloadText(t, exported), "build ok") {
		t.Fatal("the exported transcript leaks the control payload text")
	}
}
