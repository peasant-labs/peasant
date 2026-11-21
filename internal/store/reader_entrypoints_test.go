package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/reader_entrypoints.yaml
var readerEntrypointsYAML []byte

//go:embed testdata/reader_entrypoints.manifest.yaml
var readerEntrypointsManifestYAML []byte

// readerEntrypointGoldensDir holds the committed goldens, one file per
// harness and entry point.
const readerEntrypointGoldensDir = "testdata/reader_entrypoints"

// The golden generator runs when UPDATE_READER_ENTRYPOINT_GOLDENS=1. It writes
// to READER_ENTRYPOINT_GOLDENS_OUT when that is set (an absolute path, so a
// generator run in an older worktree can write into a newer one), and to the
// committed directory otherwise.
const (
	readerEntrypointUpdateEnv = "UPDATE_READER_ENTRYPOINT_GOLDENS"
	readerEntrypointOutEnv    = "READER_ENTRYPOINT_GOLDENS_OUT"
)

// The entry points a harvest reads.
const (
	// The stored entries and the session detail built from them:
	// ListEntries → EntriesToTurns → SessionToDetail.
	readerEntryListEntriesDetail = "list-entries-detail"
	// The full read: LoadFullSessionEntries with its capture certificate.
	readerEntryFullRead = "full-read"
	// The skip comparison's operand: readActiveHarmonizedView.
	readerEntryActiveView = "active-harmonized-view"
	// The prior-evidence reader a builder consumes: ReadGenerationPrior.
	readerEntryPriorEvidence = "prior-evidence"
)

var allReaderEntries = []string{readerEntryListEntriesDetail, readerEntryFullRead, readerEntryActiveView, readerEntryPriorEvidence}

type readerEntrypointCorpus struct {
	GenerationID string                    `yaml:"generationID"`
	ClockMs      int64                     `yaml:"clockMs"`
	Harnesses    []readerEntrypointHarness `yaml:"harnesses"`
}

type readerEntrypointHarness struct {
	Harness       string `yaml:"harness"`
	SessionID     string `yaml:"sessionID"`
	CWD           string `yaml:"cwd"`
	ProjectName   string `yaml:"projectName"`
	HostSlug      string `yaml:"hostSlug"`
	SourcePath    string `yaml:"sourcePath"`
	Source        string `yaml:"source"`
	NativeFixture string `yaml:"nativeFixture"`
}

type readerEntrypointManifest struct {
	Entries       []string                        `yaml:"entries"`
	Harnesses     []string                        `yaml:"harnesses"`
	Normalize     []readerEntrypointNormalization `yaml:"normalize"`
	RequiredNames []string                        `yaml:"requiredNames"`
	NotApplicable []readerEntrypointNotApplicable `yaml:"notApplicable"`
}

type readerEntrypointNormalization struct {
	Harness string `yaml:"harness"`
	Entry   string `yaml:"entry"`
	Field   string `yaml:"field"`
	Reason  string `yaml:"reason"`
}

type readerEntrypointNotApplicable struct {
	Name   string `yaml:"name"`
	Reason string `yaml:"reason"`
}

func decodeStrictYAML(t *testing.T, data []byte, into any, what string) {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("decode %s: %v", what, err)
	}
}

// loadReaderEntrypoints loads the corpus and the manifest and checks that the
// manifest names every harness × entry pair exactly once, either as a golden
// or as not applicable with a reason.
func loadReaderEntrypoints(t *testing.T) (readerEntrypointCorpus, readerEntrypointManifest) {
	t.Helper()
	var corpus readerEntrypointCorpus
	decodeStrictYAML(t, readerEntrypointsYAML, &corpus, "reader entry-point corpus")
	var manifest readerEntrypointManifest
	decodeStrictYAML(t, readerEntrypointsManifestYAML, &manifest, "reader entry-point manifest")
	if corpus.GenerationID == "" || corpus.ClockMs <= 0 {
		t.Fatal("the reader corpus needs a generation id and a positive clock")
	}
	if !slices.Equal(manifest.Entries, allReaderEntries) {
		t.Fatalf("manifest entries %v, want exactly %v", manifest.Entries, allReaderEntries)
	}
	var harnesses []string
	for _, h := range corpus.Harnesses {
		harnesses = append(harnesses, h.Harness)
	}
	if !slices.Equal(manifest.Harnesses, harnesses) {
		t.Fatalf("manifest harnesses %v, corpus harnesses %v; they must match in order", manifest.Harnesses, harnesses)
	}
	for _, n := range manifest.Normalize {
		known := slices.Contains(harnesses, n.Harness) && slices.Contains(allReaderEntries, n.Entry)
		stated := n.Field != "" && len(strings.Fields(n.Reason)) >= 3
		if !known || !stated {
			t.Fatalf("normalized field %+v must name a known entry, a field and a reason", n)
		}
	}
	all := map[string]bool{}
	for _, h := range manifest.Harnesses {
		for _, e := range manifest.Entries {
			all[h+"/"+e] = true
		}
	}
	seen := map[string]bool{}
	for _, name := range manifest.RequiredNames {
		if !all[name] || seen[name] {
			t.Fatalf("required golden %q is unknown or listed twice", name)
		}
		seen[name] = true
	}
	for _, na := range manifest.NotApplicable {
		if !all[na.Name] || seen[na.Name] || len(strings.Fields(na.Reason)) < 3 {
			t.Fatalf("not-applicable row %+v is unknown, listed twice, or has no reason", na)
		}
		seen[na.Name] = true
	}
	for name := range all {
		if !seen[name] {
			t.Fatalf("harness × entry pair %q is neither a required golden nor not applicable", name)
		}
	}
	return corpus, manifest
}

// TestReaderEntrypointGoldens holds the bytes every harvest-path reader
// returns for the synthetic corpus. The bytes were generated by the readers
// as they stood before any reader learned to follow a shared parse. A reader
// change that alters them fails here.
func TestReaderEntrypointGoldens(t *testing.T) {
	corpus, manifest := loadReaderEntrypoints(t)
	update := os.Getenv(readerEntrypointUpdateEnv) == "1"
	out := readerEntrypointGoldensDir
	if dir := os.Getenv(readerEntrypointOutEnv); dir != "" {
		out = dir
	}
	for _, h := range corpus.Harnesses {
		t.Run(h.Harness, func(t *testing.T) {
			observed, applied := readReaderEntrypoints(t, corpus, seedReaderEntrypointStore(t, corpus, h))
			var stated []string
			for _, n := range manifest.Normalize {
				if n.Harness == h.Harness {
					stated = append(stated, n.Entry+":"+n.Field)
				}
			}
			slices.Sort(stated)
			slices.Sort(applied)
			if !slices.Equal(stated, applied) {
				t.Fatalf("the generator normalized %v, the manifest states %v; every normalized field is stated with its reason", applied, stated)
			}
			for _, entry := range manifest.Entries {
				name := h.Harness + "/" + entry
				if !slices.Contains(manifest.RequiredNames, name) {
					if _, ok := observed[entry]; ok {
						t.Errorf("%s is listed as not applicable but the generator captured it", name)
					}
					continue
				}
				got, ok := observed[entry]
				if !ok {
					t.Errorf("%s: the generator captured nothing", name)
					continue
				}
				path := filepath.Join(out, h.Harness, entry+".golden")
				if update {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, got, 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%s: read golden: %v; generate it from the pre-change commit with %s=1", name, err, readerEntrypointUpdateEnv)
				}
				if !bytes.Equal(want, got) {
					t.Errorf("%s: reader bytes differ from the golden\nwant %s\n got %s\nthe golden is frozen; fix the reader, not the golden", name, want, got)
				}
			}
		})
	}
}

// TestReaderEntrypointGoldensAreSubstantive checks the committed goldens
// themselves: each holds at least one entry, and no golden names a generation
// other than the injected one.
func TestReaderEntrypointGoldensAreSubstantive(t *testing.T) {
	corpus, manifest := loadReaderEntrypoints(t)
	for _, name := range manifest.RequiredNames {
		data, err := os.ReadFile(filepath.Join(readerEntrypointGoldensDir, name+".golden"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var probe struct {
			Entries []json.RawMessage `json:"entries"`
			Mapping []json.RawMessage
			Aliases struct {
				Entries map[string]string
			}
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		held := map[string]int{
			readerEntryListEntriesDetail: len(probe.Entries),
			readerEntryFullRead:          len(probe.Entries),
			readerEntryActiveView:        len(probe.Mapping),
			readerEntryPriorEvidence:     len(probe.Aliases.Entries),
		}[strings.SplitN(name, "/", 2)[1]]
		if held == 0 {
			t.Errorf("%s holds no entry", name)
		}
		for _, other := range []string{"g_", "G_owner"} {
			if bytes.Contains(data, []byte(`"`+other)) {
				t.Errorf("%s names a generation id with prefix %q; only %s may appear", name, other, corpus.GenerationID)
			}
		}
	}
}

// countingRefAllocator mints e_golden_0001, e_golden_0002, … so the goldens
// carry stable references.
type countingRefAllocator struct{ entries, submissions int }

func (a *countingRefAllocator) NewEntryRef() (schema.SourceEntryRef, error) {
	a.entries++
	return schema.SourceEntryRef(fmt.Sprintf("e_golden_%04d", a.entries)), nil
}

func (a *countingRefAllocator) NewSubmissionRef() (schema.SubmissionRef, error) {
	a.submissions++
	return schema.SubmissionRef(fmt.Sprintf("s_golden_%04d", a.submissions)), nil
}

// readerOpenCodeAccess serves the snapshot of the corpus's synthetic native
// database; the locator resolution is the pipeline's, not the reader's.
type readerOpenCodeAccess struct {
	snapshot func(context.Context) (ingest.OpenCodeHistorySnapshot, error)
}

func (a readerOpenCodeAccess) Snapshot(ctx context.Context, _ string) (ingest.OpenCodeHistorySnapshot, error) {
	return a.snapshot(ctx)
}

// readerPriorSource serves the store's own prior-evidence reader to the
// builder, as the pipeline's per-attempt access does.
type readerPriorSource struct {
	db  *store.Store
	sid schema.SessionID
}

func (s readerPriorSource) ReadGenerationPrior(ctx context.Context) (*ingest.GenerationPrior, error) {
	return s.db.ReadGenerationPrior(ctx, s.sid)
}

func (s readerPriorSource) ReadStoredCaptureAuthority(ctx context.Context) (*ingest.StoredCaptureAuthority, error) {
	return s.db.ReadStoredCaptureAuthority(ctx, s.sid)
}

// readerRecordedMetadata is the committed metadata a drain records for the
// corpus session before its index work runs.
func readerRecordedMetadata(t *testing.T, corpus readerEntrypointCorpus, h readerEntrypointHarness, harness schema.Harness, sid schema.SessionID, transcriptBytes []byte) schema.UnifiedMetadata {
	t.Helper()
	metadata := schema.UnifiedMetadata{SchemaVersion: ingest.CurrentSchemaVersion, SessionID: sid, ModelHarness: harness, CWD: h.CWD}
	metadata.Timestamp.Start = corpus.ClockMs - 10_000
	metadata.Timestamp.End = corpus.ClockMs - 5_000
	ingested := corpus.ClockMs
	metadata.Timestamp.Ingested = &ingested
	source := h.SourcePath
	if source == "" {
		source = "/synthetic/" + h.Harness + "/" + string(sid)
	}
	metadata.Source.FilePath = source
	metadata.Source.Format = schema.SourceFormatJSONL
	if harness == schema.HarnessOpenCode {
		metadata.Source.Format = schema.SourceFormatJSON
	}
	sum := sha256.Sum256([]byte(h.ProjectName))
	metadata.Project.Hash = schema.ProjectHash(hex.EncodeToString(sum[:]))
	metadata.Project.Name = h.ProjectName
	metadata.Project.FilePath = h.CWD
	metadata.HostSlug = schema.HostSlug(h.HostSlug)
	adapter := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[harness].AdapterVersion
	metadata.AdapterVersion = &adapter
	metadata.ContentHash = schema.ComputeTranscriptHash(transcriptBytes)
	metadata.MetadataHash = schema.ComputeMetadataHash(&metadata)
	return metadata
}

// readerEntrypointStore is one harness's corpus session written to its own
// store. Root holds the database and the managed artifact tree, so a test can
// close the store, change the file and open it again.
type readerEntrypointStore struct {
	db      *store.Store
	root    string
	sid     schema.SessionID
	harness schema.Harness
	managed bool
}

func (s readerEntrypointStore) path() string { return filepath.Join(s.root, "peasant.db") }

// openReaderEntrypointStore opens the corpus store with the production
// managed-generation options.
func openReaderEntrypointStore(t *testing.T, root string) *store.Store {
	t.Helper()
	options, err := store.ManagedGenerationOptions(filepath.Join(root, "managed"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "peasant.db"), append(options, store.WithPoolSize(4))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedReaderEntrypointStore writes the corpus session for one harness through
// the production builder and activation (or, for a harness with no managed
// generation, the production entry batch).
func seedReaderEntrypointStore(t *testing.T, corpus readerEntrypointCorpus, h readerEntrypointHarness) readerEntrypointStore {
	t.Helper()
	ctx := t.Context()
	var harness schema.Harness
	if err := harness.UnmarshalText([]byte(h.Harness)); err != nil {
		t.Fatal(err)
	}
	sid, err := schema.NewSessionID(h.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	db := openReaderEntrypointStore(t, root)
	seeded := readerEntrypointStore{db: db, root: root, sid: sid, harness: harness}

	fs := testutil.NewMemFS()
	transcriptBytes := []byte(h.Source)
	session := ingest.DiscoveredSession{SessionID: sid, Harness: harness, SourcePath: ingest.ResolvedPath(h.SourcePath)}
	var native testfixture.MaterializedSource
	if h.NativeFixture != "" {
		native = testfixture.MaterializeByName(t, h.NativeFixture)
		session.SourcePath = ingest.ResolvedPath(native.Path)
		session.TranscriptOrigin = ingest.TranscriptOriginOpenCodeCurrentSQLite
		transcriptBytes = nil
	}
	if h.SourcePath != "" {
		if err := fs.WriteFile(h.SourcePath, transcriptBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recorded := readerRecordedMetadata(t, corpus, h, harness, sid, transcriptBytes)
	revisions, err := db.InsertSessionsWithRevisions(ctx, []ingest.StoreEntry{{Metadata: &recorded, PublicationCapture: true, CWDProvenance: ingest.CWDSourceExact, Session: session}})
	if err != nil {
		t.Fatalf("record the corpus session: %v", err)
	}
	target := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[harness]
	indexer := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{FullContent: true})[harness]
	builder, managed := indexer.(ingest.GenerationBuilder)
	if !managed {
		writeUnmanagedReaderSession(t, db, indexer, session, transcriptBytes, corpus.ClockMs, revisions[sid], target)
		return seeded
	}
	seeded.managed = true
	input := ingest.GenerationBuildInput{
		Session:            session,
		GenerationID:       corpus.GenerationID,
		RecordedMetadata:   &recorded,
		CapturedTranscript: transcriptBytes,
		Prior:              readerPriorSource{db: db, sid: sid},
		Allocator:          &countingRefAllocator{},
	}
	if h.NativeFixture != "" {
		adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, nil, salt.Salt{})
		input.OpenCode = readerOpenCodeAccess{snapshot: func(ctx context.Context) (ingest.OpenCodeHistorySnapshot, error) {
			return adapter.SnapshotOpenCodeProvenance(ctx, native.Path, h.SessionID, ingest.OpenCodeSnapshotOptions{})
		}}
	}
	candidate, err := builder.BuildGeneration(ctx, input)
	if err != nil {
		t.Fatalf("build the corpus generation: %v", err)
	}
	generation := candidate.Result
	if generation.Generation.Metadata.AdapterVersion == nil {
		version := target.AdapterVersion
		generation.Generation.Metadata.AdapterVersion = &version
	}
	assessment, err := ingest.AssessCapture(ingest.CaptureFacts{Harness: harness, Result: generation, Policy: ingest.CaptureFreshCandidate, Authoritative: true})
	if err != nil {
		t.Fatal(err)
	}
	capture, err := assessment.ContentCapture(ingest.ContentSourceProviderSource, session.TranscriptOrigin, corpus.ClockMs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ActivateGenerationCandidate(ctx, ingest.GenerationActivation{
		Generation: generation, Blobs: candidate.Blobs, PriorEvidence: candidate.PriorEvidence,
		IndexerVersion: target.IndexerVersion, IndexedAtMs: corpus.ClockMs, ContentCapture: capture,
		CaptureRevision: revisions[sid],
	}); err != nil {
		t.Fatalf("activate the corpus generation: %v", err)
	}
	return seeded
}

// readReaderEntrypoints returns each entry point's bytes for the corpus
// session, and the normalizations it applied.
func readReaderEntrypoints(t *testing.T, corpus readerEntrypointCorpus, s readerEntrypointStore) (map[string][]byte, []string) {
	t.Helper()
	ctx := t.Context()
	db, sid, harness := s.db, s.sid, s.harness
	observed := readUnmanagedEntrypoints(t, db, sid)
	if !s.managed {
		return observed, nil
	}
	view, err := db.ReadActiveHarmonizedViewForTest(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Found || view.GenerationID != corpus.GenerationID || len(view.Mapping) == 0 {
		t.Fatalf("the active view did not find the corpus generation: found=%t id=%q mapping=%d", view.Found, view.GenerationID, len(view.Mapping))
	}
	var applied []string
	normalize := func(entry, field string) { applied = append(applied, entry+":"+field) }
	// The commit stamps the install and the activation with its own clock.
	view.Record.InstalledAtMs = 0
	normalize(readerEntryActiveView, "Record.InstalledAtMs")
	view.Record.ActivatedAtMs = nil
	normalize(readerEntryActiveView, "Record.ActivatedAtMs")
	prior, err := db.ReadGenerationPrior(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if prior == nil || prior.Metadata == nil {
		t.Fatal("the prior-evidence reader returned nothing for an active generation")
	}
	if harness == schema.HarnessCodex {
		// The Codex metadata extraction stamps the ingest time with its own
		// clock, and the Codex history registry mints its segment references
		// at random; neither takes an injected source. The two digests cover
		// the stamp and the references.
		view.Record.TimestampIngestedMs = nil
		normalize(readerEntryActiveView, "Record.TimestampIngestedMs")
		view.Record.MetadataHash = ""
		normalize(readerEntryActiveView, "Record.MetadataHash")
		view.Record.CandidateDigest = ""
		normalize(readerEntryActiveView, "Record.CandidateDigest")
		for i := range view.Children.SegmentRefs {
			view.Children.SegmentRefs[i].SourceEntryRef = schema.SourceEntryRef(fmt.Sprintf("e_segment_%04d", i+1))
		}
		normalize(readerEntryActiveView, "Children.SegmentRefs.SourceEntryRef")
		prior.Metadata.Timestamp.Ingested = nil
		normalize(readerEntryPriorEvidence, "Metadata.timestamp.ingested")
	}
	observed[readerEntryActiveView] = readerGoldenJSON(t, view)
	observed[readerEntryPriorEvidence] = readerGoldenJSON(t, prior)
	return observed, applied
}

// writeUnmanagedReaderSession writes a harness without a managed generation
// through the production entry batch the pipeline uses for it.
func writeUnmanagedReaderSession(t *testing.T, db *store.Store, indexer ingest.Indexer, session ingest.DiscoveredSession, data []byte, clockMs, revision int64, target ingest.HarvesterVersions) {
	t.Helper()
	entries, err := indexer.IndexTranscriptBytes(t.Context(), session, data)
	if err != nil {
		t.Fatal(err)
	}
	assessment, err := ingest.AssessCapture(ingest.V1CaptureFacts(session.Harness, indexformat.V1{Entries: entries}, true, false))
	if err != nil {
		t.Fatal(err)
	}
	capture, err := assessment.ContentCapture(ingest.ContentSourceProviderSource, ingest.TranscriptOriginFile, clockMs)
	if err != nil {
		t.Fatal(err)
	}
	writes := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{
		SessionID: session.SessionID, Result: indexformat.V1{Entries: entries}, IndexVersion: target.IndexVersion,
		IndexerVersion: target.IndexerVersion, IndexedAtMs: clockMs, RequireFullContent: true,
		CaptureRevision: revision, ContentCapture: capture,
	}})
	if len(writes) != 1 || writes[0].Err != nil {
		t.Fatalf("write the corpus entries: %+v", writes)
	}
}

// readUnmanagedEntrypoints captures the entry points every harness serves.
func readUnmanagedEntrypoints(t *testing.T, db *store.Store, sid schema.SessionID) map[string][]byte {
	t.Helper()
	ctx := t.Context()
	observed := map[string][]byte{}
	entries, err := db.ListEntries(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("ListEntries returned no entry for the corpus session")
	}
	session, err := api.NewStoreDataProvider(db, sessionvisibility.Browsing()).SessionByID(ctx, string(sid))
	if err != nil {
		t.Fatal(err)
	}
	session.Turns = transcript.EntriesToTurns(entries)
	detail := transcript.SessionToDetail(session)
	observed[readerEntryListEntriesDetail] = readerGoldenJSON(t, struct {
		Entries []schema.SessionEntry        `json:"entries"`
		Detail  *schema.SessionDetailPayload `json:"detail"`
	}{entries, detail})
	full, capture, err := db.LoadFullSessionEntries(ctx, sid, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	observed[readerEntryFullRead] = readerGoldenJSON(t, struct {
		Entries []schema.SessionEntry        `json:"entries"`
		Capture ingest.SessionContentCapture `json:"capture"`
	}{full, capture})
	return observed
}

// readerGoldenJSON is the golden encoding: indented JSON with sorted map keys
// (encoding/json sorts them) and a trailing newline.
func readerGoldenJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

//go:embed testdata/context_changes.yaml
var contextChangesYAML []byte

// The routes a context change can take, as the fixture spells them.
const (
	contextRouteSettle    = "settle"
	contextRouteReference = "reference"
	contextRouteParse     = "parse:"
)

type contextChangeFixture struct {
	Harnesses     []string        `yaml:"harnesses"`
	RequiredNames []string        `yaml:"requiredNames"`
	Changes       []contextChange `yaml:"changes"`
}

type contextChange struct {
	Name   string            `yaml:"name"`
	Field  string            `yaml:"field"`
	Before string            `yaml:"before"`
	After  string            `yaml:"after"`
	Routes map[string]string `yaml:"routes"`
}

// LoadContextChanges loads the shared context-change fixture and refuses a
// row that is unnamed, duplicated, missing from the required names, changes
// nothing, names a field UnifiedMetadata does not have, or leaves a managed
// harness without a well-formed route.
func LoadContextChanges(t *testing.T) contextChangeFixture {
	t.Helper()
	var fixture contextChangeFixture
	decodeStrictYAML(t, contextChangesYAML, &fixture, "context-change fixture")
	if len(fixture.Harnesses) == 0 || len(fixture.RequiredNames) == 0 {
		t.Fatal("the context-change fixture needs its harnesses and a required-name manifest")
	}
	for _, h := range fixture.Harnesses {
		var harness schema.Harness
		if err := harness.UnmarshalText([]byte(h)); err != nil {
			t.Fatalf("context-change harness %q: %v", h, err)
		}
	}
	var names []string
	for _, change := range fixture.Changes {
		if change.Name == "" || slices.Contains(names, change.Name) {
			t.Fatalf("context change %q is unnamed or duplicated", change.Name)
		}
		names = append(names, change.Name)
		if change.Before == change.After {
			t.Fatalf("context change %s changes nothing", change.Name)
		}
		if !unifiedMetadataHasJSONPath(change.Field) {
			t.Fatalf("context change %s names %q, which is not a UnifiedMetadata field", change.Name, change.Field)
		}
		var routed []string
		for harness, route := range change.Routes {
			routed = append(routed, harness)
			reason, isParse := strings.CutPrefix(route, contextRouteParse)
			parses := isParse && reason != "" && !strings.ContainsAny(reason, " :")
			if route != contextRouteSettle && route != contextRouteReference && !parses {
				t.Fatalf("context change %s routes %s to %q; want settle, reference or parse:<reason>", change.Name, harness, route)
			}
		}
		slices.Sort(routed)
		want := slices.Sorted(slices.Values(fixture.Harnesses))
		if !slices.Equal(routed, want) {
			t.Fatalf("context change %s routes harnesses %v, want exactly %v", change.Name, routed, want)
		}
	}
	required := slices.Sorted(slices.Values(fixture.RequiredNames))
	if got := slices.Sorted(slices.Values(names)); !slices.Equal(got, required) {
		t.Fatalf("context changes %v differ from the required names %v", got, required)
	}
	return fixture
}

func TestContextChangesFixture(t *testing.T) {
	LoadContextChanges(t)
}

// unifiedMetadataHasJSONPath reports whether a dotted path of JSON names
// resolves to a field of schema.UnifiedMetadata.
func unifiedMetadataHasJSONPath(path string) bool {
	typ := reflect.TypeOf(schema.UnifiedMetadata{})
	for _, part := range strings.Split(path, ".") {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return false
		}
		found := false
		for i := range typ.NumField() {
			field := typ.Field(i)
			// The JSON field name, not a transcript source format.
			// ast-grep-ignore: no-bare-format-literals
			if name, _, _ := strings.Cut(field.Tag.Get("json"), ","); name == part {
				typ, found = field.Type, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
