package ingest_test

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/pi_unknown.yaml
var piUnknownYAML []byte

//go:embed testdata/pi_unknown_carriers.yaml
var piUnknownCarriersYAML []byte

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
	Namespace string   `yaml:"namespace"`
	Kind      string   `yaml:"kind"`
	ID        string   `yaml:"id"`
	Line      int      `yaml:"line"`
	Sequence  int      `yaml:"sequence"`
	Pointer   string   `yaml:"pointer"`
	Contains  []string `yaml:"contains"`
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
					if got.Namespace != want.Namespace || got.Kind != want.Kind || got.Harness != schema.HarnessPi || got.Position.Line != want.Line || got.Position.SourceID != want.ID || got.Position.JSONPointer != want.Pointer || string(got.Position.SourceEntryRef) != ingest.PiPublicRef(sid.String(), "entry", want.ID) {
						t.Fatalf("evidence identity: %+v want %+v", got, want)
					}
					for _, text := range want.Contains {
						if !strings.Contains(string(got.Payload), text) {
							t.Fatalf("payload missing %q", text)
						}
					}
					if strings.Contains(string(got.Payload), "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij") {
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
		})
	}
}
