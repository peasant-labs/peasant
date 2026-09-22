package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/control_record_ingest.yaml
var controlRecordIngestYAML []byte

type controlRecordIngestCase struct {
	Name                string            `yaml:"name"`
	Transcript          string            `yaml:"transcript"`
	CaptureStatus       string            `yaml:"capture_status"`
	CaptureFormat       string            `yaml:"capture_format"`
	FailureCode         string            `yaml:"failure_code"`
	ControlPartType     string            `yaml:"control_part_type"`
	ControlPreview      string            `yaml:"control_preview"`
	ControlExtraMembers map[string]string `yaml:"control_extra_members"`
	ExportAccepted      bool              `yaml:"export_accepted"`
	PublicationReady    bool              `yaml:"publication_ready"`
	PreflightAccepts    bool              `yaml:"preflight_accepts"`
}

func loadControlRecordIngestFixtures(t *testing.T) []controlRecordIngestCase {
	t.Helper()
	var document struct {
		Required []string                  `yaml:"required_names"`
		Cases    []controlRecordIngestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(controlRecordIngestYAML, &document); err != nil {
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
	for _, fixture := range loadControlRecordIngestFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			sourceDir := filepath.Join(root, "source", "-workspace")
			if err := os.MkdirAll(sourceDir, 0o700); err != nil {
				t.Fatal(err)
			}
			transcript := strings.ReplaceAll(fixture.Transcript, "SESSION_ID", testutil.TestSessionUUID)
			sourcePath := filepath.Join(sourceDir, testutil.TestSessionUUID+".jsonl")
			if err := os.WriteFile(sourcePath, []byte(transcript), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "peasant.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			fs := &ingest.OSFileSystem{}
			cfg := ingest.PipelineConfig{
				Sources: map[ingest.Harness]ingest.SourceConfig{
					ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Join(root, "source"))}},
				},
				OutputDir:   ingest.ResolvedPath(filepath.Join(root, "output")),
				Parallelism: 1,
			}
			pipeline, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
				ingest.WithStore(db), ingest.WithMetricsStore(db),
				ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
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
			db, err = store.Open(path)
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

			exported, exportErr := export.ExportSession(ctx, db, fs, testutil.TestSessionUUID)
			if (exportErr == nil) != fixture.ExportAccepted {
				t.Fatalf("export accepted=%v, want %v (%v)", exportErr == nil, fixture.ExportAccepted, exportErr)
			}
			if fixture.ExportAccepted {
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
