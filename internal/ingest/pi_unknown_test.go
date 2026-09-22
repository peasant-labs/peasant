package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/pi_unknown.yaml
var piUnknownYAML []byte

//go:embed testdata/pi_unknown_carriers.yaml
var piUnknownCarriersYAML []byte

//go:embed testdata/pi_unknown_oracle.yaml
var piUnknownOracleYAML []byte

func TestPiUnknownOriginalPayloadOracle(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name     string `yaml:"name"`
			Original string `yaml:"original"`
			Mutation string `yaml:"mutation"`
			Remove   string `yaml:"remove"`
			Reject   bool   `yaml:"reject"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(piUnknownOracleYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			actual := c.Original
			switch c.Mutation {
			case "unchanged":
			case "delete-summary":
				if c.Remove == "" || strings.Count(actual, c.Remove) != 1 {
					t.Fatal("vacuous field-deletion mutation")
				}
				actual = strings.Replace(actual, c.Remove, "", 1)
				if !json.Valid([]byte(actual)) {
					t.Fatal("field deletion damaged JSON rather than deleting only the selected field")
				}
			case "compact":
				var compact bytes.Buffer
				if err := json.Compact(&compact, []byte(actual)); err != nil {
					t.Fatal(err)
				}
				actual = compact.String()
			default:
				t.Fatal("unknown oracle mutation")
			}
			if err := comparePiUnknownPayload(c.Original, actual); (err != nil) != c.Reject {
				t.Fatalf("original-source oracle: %v, want rejection %t", err, c.Reject)
			}
		})
	}
}

func TestPiUnknownCarrierValidation(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name     string  `yaml:"name"`
			Evidence string  `yaml:"evidence"`
			Owner    *string `yaml:"owner"`
			Array    string  `yaml:"array"`
			Reject   bool    `yaml:"reject"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(piUnknownCarriersYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			ref := ingest.PiPublicRef(testutil.TestSessionUUID, "entry", "x")
			if tc.Owner != nil {
				ref = *tc.Owner
			}
			extra, err := ingest.EncodePiExtra(ingest.PiExtra{Kind: ingest.PiExtraState, Harness: schema.HarnessPi, SourceRef: ref})
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(*extra), &fields); err != nil {
				t.Fatal(err)
			}
			array := tc.Array
			if array == "" {
				array = "[" + strings.ReplaceAll(tc.Evidence, "SOURCE", ref) + "]"
			}
			fields["retainedUnknown"] = json.RawMessage(array)
			encoded, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			text := string(encoded)
			decoded, _, err := ingest.DecodePiExtra(&text)
			if (err != nil) != tc.Reject {
				t.Fatalf("carrier outcome: %v", err)
			}
			if tc.Reject {
				return
			}
			roundTrip, err := ingest.EncodePiExtra(decoded)
			if err != nil {
				t.Fatal(err)
			}
			entry := schema.SessionEntry{Harness: schema.HarnessPi, Extra: roundTrip}
			found, err := ingest.RetainedUnknownOf(entry)
			if err != nil || len(found) != 1 {
				t.Fatalf("common evidence extraction: %v", err)
			}
			if !reflect.DeepEqual(found, decoded.RetainedUnknown) {
				t.Fatal("typed carrier round trip changed public coordinates")
			}
			if found[0].Position.Public != nil {
				public, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{entry}, schema.HarnessPi)
				if err != nil || len(public) != 1 || public[0].RecordIndex != 0 || public[0].Position != 0 {
					t.Fatalf("first zero position was lost: %+v %v", public, err)
				}
			}
			if err := ingest.AttachRetainedUnknown(&entry, found); err != nil {
				t.Fatal(err)
			}
			merged, _, err := ingest.DecodePiEntryExtra(entry)
			if err != nil || merged.SourceRef != ref || len(merged.RetainedUnknown) != 2 {
				t.Fatalf("common merge rejected or erased Pi fields: %v", err)
			}
		})
	}
}

type piUnknownExpectation struct {
	Namespace string `yaml:"namespace"`
	Kind      string `yaml:"kind"`
	ID        string `yaml:"id"`
	Line      int    `yaml:"line"`
	Sequence  int    `yaml:"sequence"`
	Position  int64  `yaml:"position"`
	Pointer   string `yaml:"pointer"`
	Payload   string `yaml:"payload"`
}

// This comparison deliberately accepts fixture-owned text, never a projected
// production capture as its expectation. Neither JSON normalization nor partial
// substring matches can certify that the complete source payload survived.
func comparePiUnknownPayload(expected, actual string) error {
	if expected == "" || !json.Valid([]byte(expected)) {
		return fmt.Errorf("missing complete fixture payload")
	}
	if expected != actual {
		return fmt.Errorf("retained payload differs from complete source fixture (%d expected bytes, %d actual bytes)", len(expected), len(actual))
	}
	return nil
}

func TestPiUnknownPersistence(t *testing.T) {
	var fixture struct {
		SessionID     string   `yaml:"sessionID"`
		Header        string   `yaml:"header"`
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name         string                 `yaml:"name"`
			Source       string                 `yaml:"source"`
			PaddingBytes int                    `yaml:"paddingBytes"`
			Reject       bool                   `yaml:"reject"`
			Publish      bool                   `yaml:"publish"`
			Content      []string               `yaml:"content"`
			Expected     []piUnknownExpectation `yaml:"expected"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(piUnknownYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	sid, err := ingest.NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "source.jsonl")
			padding := strings.Repeat("z", tc.PaddingBytes)
			var wantPublic []schema.RetainedUnknownRecord
			for i := range tc.Expected {
				want := &tc.Expected[i]
				want.Payload = strings.ReplaceAll(want.Payload, "PADDING", padding)
				if err := comparePiUnknownPayload(want.Payload, want.Payload); err != nil {
					t.Fatal(err)
				}
				sequence := want.Sequence
				if sequence == 0 {
					sequence = want.Line
				}
				wantPublic = append(wantPublic, schema.RetainedUnknownRecord{SourceRef: ingest.PiPublicRef(sid.String(), "stream", "recording"), RecordIndex: int64(sequence - 1), Position: want.Position, Pointer: want.Pointer, Namespace: want.Namespace, Kind: want.Kind, Payload: want.Payload})
			}
			sort.Slice(wantPublic, func(i, j int) bool { return wantPublic[i].Position < wantPublic[j].Position })
			source := fixture.Header + "\n" + strings.ReplaceAll(tc.Source, "PADDING", padding)
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			resolved, err := ingest.NewResolvedPath(path)
			if err != nil {
				t.Fatal(err)
			}
			fs := &ingest.OSFileSystem{}
			session := ingest.DiscoveredSession{SessionID: sid, Harness: schema.HarnessPi, SourcePath: resolved}
			indexer := ingest.NewPiIndexer(fs)
			capture, err := indexer.IndexTranscriptForCapture(t.Context(), session)
			if tc.Reject {
				if err == nil || len(capture.Entries) != 0 {
					t.Fatal("corrupt source accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			check := func(entries []schema.SessionEntry) {
				t.Helper()
				var found []ingest.RetainedUnknown
				var content []string
				for _, entry := range entries {
					if _, _, err := ingest.DecodePiEntryExtra(entry); err != nil {
						t.Fatal(err)
					}
					unknown, err := ingest.RetainedUnknownOf(entry)
					if err != nil {
						t.Fatal(err)
					}
					found = append(found, unknown...)
					if entry.ContentPreview != nil {
						content = append(content, *entry.ContentPreview)
					}
				}
				if !reflect.DeepEqual(content, tc.Content) {
					t.Fatalf("known content: %q want %q", content, tc.Content)
				}
				if len(found) != len(tc.Expected) {
					t.Fatalf("evidence count: %d want %d", len(found), len(tc.Expected))
				}
				for i, want := range tc.Expected {
					got := found[i]
					sequence := want.Sequence
					if sequence == 0 {
						sequence = want.Line
					}
					if got.Position.Sequence != sequence {
						t.Fatalf("record sequence: %d want %d", got.Position.Sequence, sequence)
					}
					public := got.Position.Public
					if public == nil || public.SourceRef != ingest.PiPublicRef(sid.String(), "stream", "recording") || public.RecordIndex != int64(sequence-1) || public.Position != want.Position {
						t.Fatalf("captured public coordinates: %+v want %+v", public, want)
					}
					if got.Namespace != want.Namespace || got.Kind != want.Kind || got.Harness != schema.HarnessPi || got.Position.Line != want.Line || got.Position.SourceID != want.ID || got.Position.JSONPointer != want.Pointer || string(got.Position.SourceEntryRef) != ingest.PiPublicRef(sid.String(), "entry", want.ID) {
						t.Fatalf("evidence identity: %+v want %+v", got, want)
					}
					if err := comparePiUnknownPayload(want.Payload, string(got.Payload)); err != nil {
						t.Fatalf("source occurrence %d: %v", i, err)
					}
					if strings.Contains(string(got.Payload), "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij") {
						t.Fatal("source secret persisted without redaction")
					}
					if tc.PaddingBytes > 0 && !strings.Contains(string(got.Payload), padding) {
						t.Fatal("large evidence truncated")
					}
				}
				projection, err := transcript.EntriesToProjectionValidated(entries, transcript.ProjectionOptions{Harness: schema.HarnessPi})
				if err != nil {
					t.Fatal(err)
				}
				if len(projection.NativeMetadata) != 0 {
					t.Fatal("generic unknown evidence became Pi native metadata")
				}
				if len(projection.Turns) != len(tc.Content) {
					t.Fatal("unknown evidence fabricated visible turns")
				}
			}
			check(capture.Entries)
			if len(capture.RetainedUnknown) != len(tc.Expected) {
				t.Fatal("capture accounting lost evidence")
			}
			retained, err := indexer.IndexTranscriptBytesForCapture(t.Context(), session, []byte(source))
			if err != nil || !reflect.DeepEqual(capture, retained) {
				t.Fatalf("retained/native mismatch: %v", err)
			}
			dbPath := filepath.Join(root, "index.db")
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			writer := &unknownFailingStore{Store: db}
			output, err := ingest.NewResolvedPath(filepath.Join(root, "managed"))
			if err != nil {
				t.Fatal(err)
			}
			cfg := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{schema.HarnessPi: {Enabled: true, Paths: []ingest.ResolvedPath{resolved}}}, OutputDir: output, IncludeActive: true, Parallelism: 1}
			run := func() *ingest.PipelineResult {
				t.Helper()
				p, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg, ingest.WithStore(writer), ingest.WithMetricsStore(writer), ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
				if err != nil {
					t.Fatal(err)
				}
				result, err := p.Run(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			result := run()
			if result.Summary.Indexed != 1 || result.Summary.StoreError != nil {
				t.Fatalf("pipeline: %+v", result.Summary)
			}
			occurrences := 0
			for _, count := range result.Summary.RetainedUnknownKinds {
				occurrences += count.Occurrences
				if count.Sessions != 1 {
					t.Fatal("session count")
				}
			}
			if occurrences != len(tc.Expected) {
				t.Fatal("persisted accounting mismatch")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			writer.Store = db
			entries, err := db.ListEntries(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			check(entries)
			beforeExport, err := export.ExportSession(t.Context(), db, fs, sid.String())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeExport.RetainedUnknown, wantPublic) {
				t.Fatal("source-to-export evidence changed")
			}
			if !sort.SliceIsSorted(beforeExport.RetainedUnknown, func(i, j int) bool {
				return beforeExport.RetainedUnknown[i].Position < beforeExport.RetainedUnknown[j].Position
			}) {
				t.Fatal("export did not restore captured source order")
			}
			if len(wantPublic) > 0 && (beforeExport.Diagnostics == nil || !beforeExport.Diagnostics.Partial) {
				t.Fatal("export falsely complete")
			}
			if len(beforeExport.Turns) != len(tc.Content) {
				t.Fatal("export invented unknown turns")
			}
			if tc.Publish {
				assertPiUnknownPublication(t, db, fs, sid, output, wantPublic, len(tc.Content))
			}
			// Remove native input to exercise database-driven retained indexing,
			// rather than mistaking another native capture for the batch route.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			cfg.Reindex, cfg.Force = true, true
			batch := run()
			if batch.Summary.Indexed != 1 {
				t.Fatalf("retained batch did not index: %+v", batch.Summary)
			}
			batchEntries, err := db.ListEntries(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			check(batchEntries)
			if !reflect.DeepEqual(entries, batchEntries) {
				t.Fatal("retained batch changed native evidence")
			}
			// Force an ordinary reindex with the real store behind a failing
			// transaction boundary. A failed replacement cannot account evidence.
			beforeExport, err = export.ExportSession(t.Context(), db, fs, sid.String())
			if err != nil {
				t.Fatal(err)
			}
			writer.fail = true
			cfg.Force = true
			failed := run()
			if failed.Summary.Indexed != 0 || len(failed.Summary.RetainedUnknownKinds) != 0 {
				t.Fatalf("failed write certified evidence: %+v", failed.Summary)
			}
			after, err := db.ListEntries(t.Context(), sid)
			if err != nil || !reflect.DeepEqual(entries, after) {
				t.Fatal("failed replacement changed prior entries")
			}
			afterExport, err := export.ExportSession(t.Context(), db, fs, sid.String())
			if err != nil || !reflect.DeepEqual(beforeExport, afterExport) {
				t.Fatalf("failed replacement changed prior export: %v", err)
			}
		})
	}
}

func assertPiUnknownPublication(t *testing.T, db *store.Store, fs ingest.FileSystem, sid schema.SessionID, output ingest.ResolvedPath, expected []schema.RetainedUnknownRecord, turns int) {
	t.Helper()
	publisher := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: schema.AllContentCapabilities}}
	cfg := &config.Config{Output: config.OutputConfig{BasePath: output.String()}, Push: config.PushConfig{Method: config.PushMethodAll, Visibility: config.VisibilityPrivate}}
	creds := &auth.Credentials{APIKey: "synthetic-key", KeyID: "synthetic-key-id", UserID: "synthetic-user", Username: "fixture", VillageURL: "https://village.example.com"}
	var stderr bytes.Buffer
	pipeline, err := push.NewPipeline(db, publisher, creds, cfg, fs, push.PipelineConfig{Force: true, Concurrency: 1}, &testutil.NoopRedactor{}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil || result.Errors != 0 || len(publisher.Calls) != 1 {
		t.Fatalf("Pi publication: %v %+v %s", err, result, stderr.String())
	}
	content, err := schema.DecodeTranscriptContentRaw(publisher.Calls[0].TranscriptBody)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(content.SessionDetail.RetainedUnknown, expected) {
		t.Fatal("publication changed source payload or coordinates")
	}
	if content.SessionDetail.Diagnostics == nil || !content.SessionDetail.Diagnostics.Partial || len(publisher.AuthoritativeCalls) != 1 || publisher.AuthoritativeCalls[0].Diagnostics.Partial == nil || !*publisher.AuthoritativeCalls[0].Diagnostics.Partial {
		t.Fatal("publication lost partial detail/metadata mirror")
	}
	if len(content.SessionDetail.NativeMetadata) != 0 || len(content.SessionDetail.Turns) != turns {
		t.Fatal("publication used Pi metadata or fabricated conversation")
	}
}
