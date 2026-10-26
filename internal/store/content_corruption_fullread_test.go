package store_test

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	sessionexport "github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_corruption.yaml
var fullReadCorruptionYAML []byte

//go:embed testdata/content_corruption.manifest.yaml
var fullReadCorruptionManifestYAML []byte

type fullReadCorruptionCase struct {
	Name                    string   `yaml:"name"`
	Owner                   string   `yaml:"owner"`
	Damage                  string   `yaml:"damage"`
	Surfaces                []string `yaml:"surfaces"`
	Expect                  string   `yaml:"expect"`
	Why                     string   `yaml:"why"`
	Query                   string   `yaml:"query"`
	WantFoundOnce           bool     `yaml:"wantFoundOnce"`
	WantStaleRawMatch       bool     `yaml:"wantStaleRawMatch"`
	WantRefusedWhileFlagged bool     `yaml:"wantRefusedWhileFlagged"`
	WantCleanAfterRebuild   bool     `yaml:"wantCleanAfterRebuild"`
}

func loadFullReadCorruptionFixtures(t *testing.T) []fullReadCorruptionCase {
	t.Helper()
	var fixtures struct {
		Cases []fullReadCorruptionCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(fullReadCorruptionYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(fullReadCorruptionManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, c := range fixtures.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("duplicate or blank corruption case %q", c.Name)
		}
		names[c.Name] = true
		switch c.Owner {
		case "full-read", "verify":
			if c.Damage == "" || c.Expect == "" || len(c.Surfaces) == 0 {
				t.Fatalf("incomplete corruption case %+v", c)
			}
		case "search", "repair":
		default:
			t.Fatalf("unknown corruption owner %q", c.Owner)
		}
	}
	for _, name := range manifest.RequiredNames {
		if !names[name] {
			t.Fatalf("missing corruption case %q", name)
		}
		delete(names, name)
	}
	if len(names) != 0 {
		t.Fatalf("unmanifested corruption cases: %v", names)
	}
	return fixtures.Cases
}

func seedFullReadCorruption(t *testing.T, damage string, texts ...string) (*store.Store, schema.SessionID) {
	t.Helper()
	s := openSnapshotStore(t, 1)
	e := publicationEntry(t, "c4c4c4c4-c4c4-44c4-84c4-c4c4c4c4c4c4")
	revision := capturePublication(t, s, e)
	text := "original full read bytes"
	if len(texts) != 0 {
		text = texts[0]
	}
	activation := snapshotActivation(e.Metadata.SessionID, "full-read-corruption", text)
	activation.Generation.Generation.Metadata = *e.Metadata
	count := int64(1)
	activation.Generation.Generation.Metadata.Stats = schema.SessionStats{TurnCount: 1, InputSubmissionCount: &count}
	activation.Generation.Generation.Main.Entries[0].Harness = e.Metadata.ModelHarness
	activation.CaptureRevision = revision
	activation.ContentCapture = ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest, TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: 1}
	if damage == "carrier-column" || damage == "carrier-missing" {
		activation.Generation.Generation.Main.Entries[0].SourceEntryRef = ""
		activation.Generation.Generation.Content = nil
		activation.Generation.Generation.TitleRefs = nil
		activation.Blobs = nil
	}
	if damage == "preview-only-column" {
		activation.Generation.Generation.Completeness = indexformat.GenerationCompletenessIncompleteNew
		activation.Generation.Generation.Metadata.Stats.InputSubmissionCount = nil
		activation.ContentCapture = ingest.SessionContentCaptureWrite{}
	}
	if _, err := s.ActivateGeneration(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if damage == "preview-only-column" {
		if _, payload, err := transcript.BuildSnapshotPreviewBytes(t.Context(), s, e.Metadata.SessionID); err != nil || payload == nil {
			t.Fatalf("healthy preview: %v", err)
		}
		return s, e.Metadata.SessionID
	}
	if _, detail, err := transcript.BuildSnapshotDetailBytes(t.Context(), s, s, e.Metadata.SessionID); err != nil || detail == nil {
		t.Fatalf("healthy detail: %v", err)
	}
	input, detail, err := push.LoadPublicationInput(t.Context(), s, string(e.Metadata.SessionID))
	if err != nil || input.Readiness != ingest.PublicationReady || detail == nil {
		t.Fatalf("healthy publication: readiness %s detail %v error %v", input.Readiness, detail, err)
	}
	return s, e.Metadata.SessionID
}

func TestContentCorruptionFullRead(t *testing.T) {
	for _, c := range loadFullReadCorruptionFixtures(t) {
		if c.Owner != "full-read" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			s, sid := seedFullReadCorruption(t, c.Damage)
			switch c.Damage {
			case "carrier-missing":
				func() {
					conn, err := s.PoolForTest().Take(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer s.PoolForTest().Put(conn)
					if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys=OFF`, nil); err != nil {
						t.Fatal(err)
					}
					err = sqlitex.Execute(conn, `DELETE FROM session_entry_bodies WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}})
					if restoreErr := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys=ON`, nil); restoreErr != nil {
						t.Fatal(restoreErr)
					}
					if err != nil {
						t.Fatal(err)
					}
				}()
			case "body-column", "carrier-column", "preview-only-column":
				publicationSQL(t, s, `DROP TRIGGER session_entry_bodies_immutable`)
				publicationSQL(t, s, `UPDATE session_entry_bodies SET content_preview = 'tampered full read bytes' WHERE session_id = ?`, string(sid))
			case "capture-hash":
				publicationSQL(t, s, `UPDATE session_content_captures SET full_capture_sha256 = ? WHERE session_id = ?`, strings.Repeat("b", 64), string(sid))
			case "body-delete":
				conn, err := s.PoolForTest().Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				err = sqlitex.Execute(conn, `DELETE FROM session_entry_bodies WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}})
				s.PoolForTest().Put(conn)
				if c.Expect != "fk-refuses" || err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
					t.Fatalf("mapped body delete = %v; want foreign-key refusal", err)
				}
				if _, payload, err := transcript.BuildSnapshotDetailBytes(t.Context(), s, s, sid); err != nil || payload == nil {
					t.Fatalf("refused delete changed the healthy read: %v", err)
				}
				return
			default:
				t.Fatalf("unknown full-read damage %q", c.Damage)
			}
			for _, surface := range c.Surfaces {
				t.Run(surface, func(t *testing.T) {
					switch surface {
					case "detail":
						data, payload, err := transcript.BuildSnapshotDetailBytes(t.Context(), s, s, sid)
						if err == nil || data != nil || payload != nil {
							t.Fatalf("damaged detail returned partial output: bytes=%s payload=%v error=%v", data, payload, err)
						}
						if c.Expect == "incomplete-refuses-full" && !errors.Is(err, transcript.ErrSnapshotIncomplete) {
							t.Fatalf("preview-only full read lost incompleteness: %v", err)
						}
					case "export":
						payload, err := sessionexport.ExportSnapshotPayload(t.Context(), s, s, sid)
						if err == nil || payload != nil {
							t.Fatalf("damaged export returned partial output: %v %v", payload, err)
						}
					case "publish", "scan":
						input, payload, err := push.LoadPublicationInput(t.Context(), s, string(sid))
						hasInputContent := input.Entries != nil || input.Generation != nil
						if err == nil || payload != nil || hasInputContent {
							t.Fatalf("damaged publication returned partial output: %+v %v %v", input, payload, err)
						}
						if surface == "scan" {
							scan, err := push.ScanPublication(schema.TranscriptContent{SessionDetail: payload})
							if err == nil || scan.Required != nil {
								t.Fatalf("scan admitted refused publication: %+v %v", scan, err)
							}
						}
					case "preview":
						data, payload, err := transcript.BuildSnapshotPreviewBytes(t.Context(), s, sid)
						if err != nil || payload == nil || !strings.Contains(string(data), "tampered full read bytes") {
							t.Fatalf("unverified preview = %s %v", data, err)
						}
					case "api-preview":
						payload, err := api.DetailPayloadWithReader(t.Context(), s, s, string(sid), func(context.Context, string) (*schema.SessionDetailPayload, error) {
							return nil, fmt.Errorf("unexpected legacy fallback")
						})
						if err != nil || payload == nil {
							t.Fatalf("mounted preview exit refused: %+v %v", payload, err)
						}
						if payload.Diagnostics == nil || !payload.Diagnostics.Partial {
							t.Fatalf("mounted preview exit refused or lost partial status: %+v %v", payload, err)
						}
					case "routing":
						entries, err := s.ListEntries(t.Context(), sid)
						if err != nil || len(entries) != 1 {
							t.Fatalf("unverified routing entries = %+v %v", entries, err)
						}
						if entries[0].ContentPreview == nil || *entries[0].ContentPreview != "tampered full read bytes" {
							t.Fatalf("unverified routing = %+v %v", entries, err)
						}
					default:
						t.Fatalf("unknown corruption surface %q", surface)
					}
				})
			}
		})
	}
}
