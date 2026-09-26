package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/native_coverage.yaml
var nativeCoverageYAML []byte

type nativeCoverageCarrier struct {
	Partition   string `yaml:"partition"`
	Namespace   string `yaml:"namespace"`
	Kind        string `yaml:"kind"`
	SourceRef   string `yaml:"source_ref"`
	RecordIndex int64  `yaml:"record_index"`
	Position    int64  `yaml:"position"`
	Pointer     string `yaml:"pointer"`
	Payload     string `yaml:"payload"`
}

type nativeCoverageCase struct {
	Name            string                  `yaml:"name"`
	Completeness    string                  `yaml:"completeness"`
	Carriers        []nativeCoverageCarrier `yaml:"carriers"`
	Prior           string                  `yaml:"prior"`
	ForgedFull      bool                    `yaml:"forged_full"`
	SourceOmitted   bool                    `yaml:"source_omitted"`
	Unaccounted     bool                    `yaml:"unaccounted"`
	WantDisposition string                  `yaml:"want_disposition"`
	WantOccurrences int                     `yaml:"want_occurrences"`
	WantExport      string                  `yaml:"want_export"`
	WantFullRead    string                  `yaml:"want_full_read"`
	WantFailureCode string                  `yaml:"want_failure_code"`
}

type nativeCoverageDocument struct {
	Required []string             `yaml:"required_names"`
	Cases    []nativeCoverageCase `yaml:"cases"`
}

func loadNativeCoverage(t *testing.T) nativeCoverageDocument {
	t.Helper()
	var doc nativeCoverageDocument
	decoder := yaml.NewDecoder(bytes.NewReader(nativeCoverageYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("trailing fixture document")
	}
	names := map[string]bool{}
	var actual []string
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("invalid fixture %q", c.Name)
		}
		names[c.Name] = true
		actual = append(actual, c.Name)
	}
	// Exact-set manifest in one check: every declared name present, no
	// undeclared row. A second subset-only check would add no signal.
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: doc.Required}, actual, "native coverage"); err != nil {
		t.Fatal(err)
	}
	return doc
}

// assertCoverageErrorRawSafe pins invariant 12 on one coverage refusal: the
// error must carry only fixed refusal categories and validated identities,
// never raw payload bytes, source labels, or coordinate namespaces.
func assertCoverageErrorRawSafe(t *testing.T, err error, carriers []nativeCoverageCarrier) {
	t.Helper()
	if err == nil {
		t.Fatal("want a refusal error, got nil")
	}
	msg := err.Error()
	secrets := []string{`{"type":"future"`}
	for _, carrier := range carriers {
		secrets = append(secrets, carrier.SourceRef, carrier.Namespace, carrier.Payload)
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(msg, secret) {
			t.Fatalf("coverage refusal echoes raw evidence %q: %v", secret, err)
		}
	}
}

func seedCoverageSession(t *testing.T, dbPath, sid string) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := func(query string, args ...any) {
		if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT OR IGNORE INTO host_slugs(opaque_id, host_slug) VALUES('host-cov','host-cov')`)
	exec(`INSERT OR IGNORE INTO projects(project_hash, canonical_cwd, canonical_remote) VALUES('proj-cov','/tmp/cov','github.com/cov/cov')`)
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version) VALUES(?, 'claude-code','model-cov','host-cov','proj-cov',1,2,3,'/tmp/cov/source.jsonl','jsonl',11)`, sid)
}

func buildCoverageV2(t *testing.T, sid schema.SessionID, genID, completeness string, carriers []nativeCoverageCarrier) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	knownText := "known coverage prose"
	knownInput := `{"input":"known"}`
	knownOutput := "known result"
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Harness: schema.Harness("claude-code"), Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, ContentPreview: &knownText, SourceEntryRef: "e_cov_known"},
	}
	blobs := map[schema.SourceEntryRef][]byte{
		"e_cov_known": []byte(knownText),
		"e_cov_in":    []byte(knownInput),
		"e_cov_out":   []byte(knownOutput),
	}
	entries = append(entries,
		schema.SessionEntry{SessionID: sid, EntryIndex: 1, Harness: schema.Harness("claude-code"), Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeToolUse, ToolInput: &knownInput, SourceEntryRef: "e_cov_in"},
		schema.SessionEntry{SessionID: sid, EntryIndex: 2, Harness: schema.Harness("claude-code"), Role: ingest.RoleTool, EntryType: ingest.EntryTypeToolResult, ToolOutput: &knownOutput, SourceEntryRef: "e_cov_out"},
	)
	content := []indexformat.ContentRecord{{Ref: "e_cov_known"}, {Ref: "e_cov_in"}, {Ref: "e_cov_out"}}
	nextIndex := 3
	var mainCarriers, earlierCarriers []schema.SessionEntry
	for _, carrier := range carriers {
		position := ingest.UnknownSourcePosition{
			Public:   &ingest.UnknownPublicPosition{SourceRef: carrier.SourceRef, RecordIndex: carrier.RecordIndex, Position: carrier.Position},
			SourceID: carrier.SourceRef,
		}
		if carrier.Pointer != "" {
			position.JSONPointer = carrier.Pointer
		}
		record, err := ingest.NewRetainedUnknown(ingest.Harness("claude-code"), carrier.Namespace, carrier.Kind, position, []byte(carrier.Payload))
		if err != nil {
			t.Fatalf("carrier %q: %v", carrier.SourceRef, err)
		}
		entry, err := ingest.RetainedUnknownEntry(ingest.SessionID(sid), nextIndex, record)
		if err != nil {
			t.Fatal(err)
		}
		nextIndex++
		if carrier.Partition == "earlier" {
			earlierCarriers = append(earlierCarriers, entry)
		} else {
			mainCarriers = append(mainCarriers, entry)
		}
	}
	mainEntries := append([]schema.SessionEntry(nil), entries...)
	mainEntries = append(mainEntries, mainCarriers...)
	var earlier []indexformat.EarlierPartition
	if len(earlierCarriers) > 0 {
		earlier = append(earlier, indexformat.EarlierPartition{
			State:   schema.EarlierHistoryUncertainMigrated,
			Content: indexformat.Partition{Entries: earlierCarriers},
		})
	}
	completenessValue := indexformat.GenerationCompleteness(completeness)
	var inputCount *int64
	if completenessValue == indexformat.GenerationCompletenessComplete {
		c := int64(1)
		inputCount = &c
	}
	generation := indexformat.Generation{
		ID:           genID,
		Completeness: completenessValue,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  schema.Harness("claude-code"),
			Stats:         schema.SessionStats{TurnCount: len(mainEntries), InputSubmissionCount: inputCount},
		},
		Main:                 indexformat.Partition{Entries: mainEntries},
		Earlier:              earlier,
		Content:              content,
		SourceEvidenceDigest: strings.Repeat("b", 64),
		TitleRefs:            []schema.SourceEntryRef{"e_cov_known"},
	}
	return indexformat.V2{Generation: generation}, blobs
}

func TestNativeCoverageMatrix(t *testing.T) {
	doc := loadNativeCoverage(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := storetest.CopyGoldenDB(t)
			sid := schema.SessionID(testutil.TestSessionUUID)
			seedCoverageSession(t, dbPath, string(sid))
			root := filepath.Join(dir, "artifacts")
			artifacts, err := store.NewOSGenerationArtifactStore(root)
			if err != nil {
				t.Fatal(err)
			}
			locks, err := store.NewFileSessionLocker(root)
			if err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(dbPath, store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locks))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			fs := &ingest.OSFileSystem{}
			// Prior full authority when requested.
			var priorID string
			var priorExport []byte
			if c.Prior == "full" {
				priorID = "gen-prior-full-" + c.Name
				priorV2, priorBlobs := buildCoverageV2(t, sid, priorID, "complete", nil)
				priorAssessment, err := ingest.AssessCapture(ingest.CaptureFacts{
					Harness: ingest.Harness("claude-code"), Result: priorV2,
					Policy: ingest.CaptureFreshCandidate, Authoritative: true,
				})
				if err != nil {
					t.Fatalf("prior assessment: %v", err)
				}
				priorCapture, err := priorAssessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1)
				if err != nil {
					t.Fatalf("prior capture: %v", err)
				}
				outcome, err := db.ActivateNativeGeneration(t.Context(), ingest.NativeGenerationActivation{
					Generation: priorV2, Blobs: priorBlobs,
					IndexerVersion: 1, IndexedAtMs: 1, ContentCapture: priorCapture,
				})
				if err != nil || outcome.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("prior full activation: outcome=%+v err=%v", outcome, err)
				}
				priorDetail, err := export.ExportSession(t.Context(), db, fs, string(sid))
				if err != nil {
					t.Fatalf("prior export: %v", err)
				}
				priorExport, _ = json.Marshal(priorDetail)
			}
			genID := "gen-" + c.Name
			v2, blobs := buildCoverageV2(t, sid, genID, c.Completeness, c.Carriers)
			assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
				Harness: ingest.Harness("claude-code"), Result: v2,
				Policy: ingest.CaptureFreshCandidate, Authoritative: true,
				SourceOmitted: c.SourceOmitted, Unaccounted: c.Unaccounted,
			})
			if err != nil {
				// One-set duplicate refusal: Main+Earlier validated together
				// refuses cross-partition duplicates before any counting or
				// persistence. Expected NotCommitted with no authority and no
				// entries served.
				if c.WantDisposition == "not_committed" && c.Name == "main-and-earlier-duplicate-refused" {
					assertCoverageErrorRawSafe(t, err, c.Carriers)
					var activeID string
					_ = db.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
						activeID = snapshot.GenerationID
						return nil
					})
					if activeID != "" {
						t.Fatalf("duplicate refused but active = %q, want none", activeID)
					}
					if _, exportErr := export.ExportSession(t.Context(), db, fs, string(sid)); exportErr == nil {
						t.Fatal("export certified duplicate evidence")
					}
					if _, _, fullErr := db.LoadFullSessionEntries(t.Context(), sid, 0); fullErr == nil {
						t.Fatal("full read certified duplicate evidence")
					}
					emptyPage, readErr := db.ReadSessionEntries(t.Context(), sid, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadAvailable, Limit: 100})
					if readErr != nil {
						t.Fatalf("available read failed after refused duplicate: %v", readErr)
					}
					if len(emptyPage.Entries) != 0 {
						t.Fatalf("available read served %d entries with no authority after refused duplicate", len(emptyPage.Entries))
					}
					return
				}
				t.Fatalf("assessment refused valid matrix case: %v", err)
			}
			candidates := assessment.CandidateCounts()
			capture, err := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 2)
			if err != nil {
				t.Fatalf("capture conversion: %v", err)
			}
			// The assessed failure code is YAML-owned: omission facts must map
			// to their preview codes, complete controls to no failure, and
			// retained carriers to unknown_data_retained.
			if string(capture.FailureCode) != c.WantFailureCode {
				t.Fatalf("failure code = %q, want %q", string(capture.FailureCode), c.WantFailureCode)
			}
			if c.ForgedFull {
				// Direct forged-full: incomplete_new claiming full/complete.
				capture = ingest.SessionContentCaptureWrite{
					Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
					TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: 2,
				}
			}
			outcome, err := db.ActivateNativeGeneration(t.Context(), ingest.NativeGenerationActivation{
				Generation: v2, Blobs: blobs,
				IndexerVersion: 1, IndexedAtMs: 2, ContentCapture: capture,
			})
			wantCommitted := c.WantDisposition == "committed_now"
			if wantCommitted && (err != nil || outcome.Disposition != ingest.ActivationCommittedNow) {
				t.Fatalf("want CommittedNow, got %+v err=%v", outcome, err)
			}
			if !wantCommitted && (err == nil || outcome.Disposition != ingest.ActivationNotCommitted) {
				t.Fatalf("want NotCommitted refusal, got %+v err=%v", outcome, err)
			}
			if !wantCommitted {
				assertCoverageErrorRawSafe(t, err, c.Carriers)
			}
			// Per-invocation counts: CommittedNow counts candidates once,
			// NotCommitted zero. Preview CommittedNow carries zero candidates
			// by assessment (incomplete_new returns no counts).
			var gotOccurrences int
			if outcome.Disposition == ingest.ActivationCommittedNow {
				for _, count := range candidates {
					gotOccurrences += count.Occurrences
				}
			}
			if gotOccurrences != c.WantOccurrences {
				t.Fatalf("occurrences = %d, want %d (candidates=%+v disposition=%v)", gotOccurrences, c.WantOccurrences, candidates, outcome.Disposition)
			}
			// Authority: active generation reflects outcome.
			var activeID string
			_ = db.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
				activeID = snapshot.GenerationID
				return nil
			})
			if wantCommitted && activeID != genID {
				t.Fatalf("active = %q, want %q", activeID, genID)
			}
			if !wantCommitted && c.Prior == "full" && activeID != priorID {
				t.Fatalf("prior full not preserved: active = %q, want %q", activeID, priorID)
			}
			// Actual export verdicts, never readiness alone.
			exported, exportErr := export.ExportSession(t.Context(), db, fs, string(sid))
			switch c.WantExport {
			case "success":
				if exportErr != nil {
					t.Fatalf("export refused complete control: %v", exportErr)
				}
				if len(exported.RetainedUnknown) != len(c.Carriers) {
					t.Fatalf("export retained = %d, want %d", len(exported.RetainedUnknown), len(c.Carriers))
				}
			case "refused":
				if exportErr == nil {
					t.Fatal("export certified incomplete control as full")
				}
			case "prior_preserved":
				if exportErr != nil {
					t.Fatalf("prior export lost after refused replacement: %v", exportErr)
				}
				after, _ := json.Marshal(exported)
				if !bytes.Equal(priorExport, after) {
					t.Fatal("refused replacement changed prior export bytes")
				}
			default:
				t.Fatalf("unknown want_export %q", c.WantExport)
			}
			// Local reads: full serves only publishable authority (complete
			// controls and preserved priors); available serves an honest
			// preview for valid incomplete, never corruption (all matrix cases
			// are valid). A refused activation with no prior authority
			// honestly serves no entries. Both verdicts are YAML-owned, never
			// inferred from each other: a complete generation committed under
			// a preview capture exports via the snapshot boundary yet stays
			// refused on the full-read path.
			_, _, fullErr := db.LoadFullSessionEntries(t.Context(), sid, 0)
			switch c.WantFullRead {
			case "success":
				if fullErr != nil {
					t.Fatalf("full read refused committed authority: %v", fullErr)
				}
			case "refused":
				if fullErr == nil {
					t.Fatal("full read certified refused authority")
				}
			default:
				t.Fatalf("unknown want_full_read %q", c.WantFullRead)
			}
			page, err := db.ReadSessionEntries(t.Context(), sid, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadAvailable, Limit: 100})
			if err != nil {
				t.Fatalf("available read refused valid coverage: %v", err)
			}
			if c.WantDisposition == "committed_now" && len(page.Entries) == 0 {
				t.Fatal("available read returned no entries for committed coverage")
			}
			if c.Prior == "full" && len(page.Entries) == 0 {
				t.Fatal("available read lost prior full entries")
			}
			if c.WantDisposition == "not_committed" && c.Prior != "full" && len(page.Entries) != 0 {
				t.Fatalf("available read served %d entries with no authority after refused activation", len(page.Entries))
			}
		})
	}
}
