package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/stale_index_settling.yaml
var staleIndexSettlingFixtureData []byte

type staleIndexSettlingDocument struct {
	RequiredNames []string                    `yaml:"required_names"`
	Transcripts   map[string]string           `yaml:"transcripts"`
	Cases         []staleIndexSettlingFixture `yaml:"cases"`
	// LatestAttemptRequiredNames and LatestAttemptCases are the coexistence
	// section of the same family: a full read authority with a fully settled
	// latest attempt beside it, driven through the ordinary harvest.
	LatestAttemptRequiredNames []string                                 `yaml:"latest_attempt_required_names"`
	LatestAttemptCases         []staleIndexSettlingLatestAttemptFixture `yaml:"latest_attempt_cases"`
}

// staleIndexSettlingLatestAttemptFixture names one settled-latest-attempt
// harvest case. The transcript is the saved pair the harvest would read if the
// attempt had not settled.
type staleIndexSettlingLatestAttemptFixture struct {
	Name       string `yaml:"name"`
	Harness    string `yaml:"harness"`
	Transcript string `yaml:"transcript"`
}

type staleIndexSettlingFixture struct {
	Name              string `yaml:"name"`
	Harness           string `yaml:"harness"`
	Transcript        string `yaml:"transcript"`
	StoredIdentity    bool   `yaml:"storedIdentity"`
	OmitContentHash   bool   `yaml:"omitContentHash"`
	Refused           bool   `yaml:"refused"`
	WantCaptureStatus string `yaml:"wantCaptureStatus"`
	WantCaptureFormat string `yaml:"wantCaptureFormat"`
	WantFailureCode   string `yaml:"wantFailureCode"`
	WantEntries       int    `yaml:"wantEntries"`
	WantRetainedKind  string `yaml:"wantRetainedKind"`
}

func loadStaleIndexSettlingFixtures(t *testing.T) staleIndexSettlingDocument {
	t.Helper()
	var document staleIndexSettlingDocument
	if err := testutil.DecodeFixtureYAML(staleIndexSettlingFixtureData, &document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range document.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid stale index settling fixture %q", fixture.Name)
		}
		if _, ok := document.Transcripts[fixture.Transcript]; !ok {
			t.Fatalf("stale index settling fixture %s names unknown transcript %q", fixture.Name, fixture.Transcript)
		}
		if fixture.Refused {
			if fixture.WantCaptureStatus != "" || fixture.WantCaptureFormat != "" || fixture.WantFailureCode != "" || fixture.WantEntries != 0 {
				t.Fatalf("fixture %s is refused and must not declare a settled outcome", fixture.Name)
			}
			names[fixture.Name] = true
			continue
		}
		if _, err := ingest.NewContentCaptureStatus(fixture.WantCaptureStatus); err != nil {
			t.Fatalf("fixture %s: %v", fixture.Name, err)
		}
		if _, err := ingest.NewContentCaptureFormat(fixture.WantCaptureFormat); err != nil {
			t.Fatalf("fixture %s: %v", fixture.Name, err)
		}
		if _, err := ingest.NewContentCaptureFailureCode(fixture.WantFailureCode); err != nil {
			t.Fatalf("fixture %s: %v", fixture.Name, err)
		}
		names[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("stale index settling", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	latestNames := make(map[string]bool)
	for _, fixture := range document.LatestAttemptCases {
		if fixture.Name == "" || latestNames[fixture.Name] {
			t.Fatalf("invalid settled latest-attempt fixture %q", fixture.Name)
		}
		if _, ok := document.Transcripts[fixture.Transcript]; !ok {
			t.Fatalf("settled latest-attempt fixture %s names unknown transcript %q", fixture.Name, fixture.Transcript)
		}
		latestNames[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("settled latest attempt", "case", document.LatestAttemptRequiredNames, latestNames); err != nil {
		t.Fatal(err)
	}
	return document
}

// TestOrdinaryHarvestSettlesStaleIndexSessions drives the ORDINARY harvest
// (no reindex mode, no native discovery) over one seeded stored row per case.
// Settling cases assert the two-run contract the fixture declares: the first
// harvest advances the producer, records the input proof, establishes a
// missing artifact identity and stores the represented entries; the second
// reads no transcript and reports nothing. Refused cases assert the opposite
// for input this build must not settle: the stale state is unchanged and the
// refusal is reported.
func TestOrdinaryHarvestSettlesStaleIndexSessions(t *testing.T) {
	t.Parallel()
	document := loadStaleIndexSettlingFixtures(t)
	for _, fixture := range document.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			harness := ingest.Harness(fixture.Harness)
			if !harness.IsKnown() {
				t.Fatalf("fixture names unknown harness %q", fixture.Harness)
			}
			ctx := t.Context()
			fs := testutil.NewCountingFS(testutil.NewMemFS())
			database := storetest.OpenWith(t)
			defer database.Close()

			id, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			transcript := []byte(document.Transcripts[fixture.Transcript])
			meta := makeMinimalMeta(t, id.String())
			meta.ModelHarness = harness
			adapterVersion := ingest.HarvesterVersionRegistry[harness].AdapterVersion
			meta.AdapterVersion = &adapterVersion
			if !fixture.OmitContentHash {
				meta.ContentHash = schema.ComputeTranscriptHash(transcript)
			}
			dir := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String())
			path := filepath.Join(dir, id.String()+"--transcript.jsonl")
			if err := fs.WriteFile(path, transcript, 0600); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile(filepath.Join(dir, id.String()+"--metadata.json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
				t.Fatal(err)
			}
			pair, err := ingest.NewManagedArtifact(encoded, transcript)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.StoredIdentity {
				results := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: pair}})
				if len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
					t.Fatalf("seed stored artifact identity: %+v", results)
				}
			}
			seedStalePreviewCapture(t, ctx, database, id)

			run := func() *ingest.PipelineResult {
				t.Helper()
				cfg := makePipelineConfig(testOutputDir)
				adapters := map[ingest.Harness]ingest.AdapterFactory{
					ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
				}
				pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
					ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
					ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database))
				if err != nil {
					t.Fatal(err)
				}
				fs.ResetCounts()
				result, err := pipeline.Run(ctx)
				if err != nil {
					t.Fatalf("harvest: %v", err)
				}
				return result
			}

			first := run()
			if fixture.Refused {
				// The refusal contract: nothing is indexed, the stale state is
				// untouched (no producer advance, no input proof, no established
				// artifact identity), and the user is told why.
				if first.Summary.Indexed != 0 {
					t.Fatalf("a corrupt or unidentifiable session was indexed; diagnostics: %+v", first.Diagnostics)
				}
				state, err := database.ReadIndexState(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if state == nil || state.IndexerVersion != 15 || state.IndexedInputHash != nil || state.ArtifactHash != nil {
					t.Fatalf("refused session changed its stored index state: %+v", state)
				}
				if namesSession(first, id) == 0 {
					t.Fatalf("the refusal was not reported: %+v", first.Diagnostics)
				}
				return
			}
			if first.Summary.Indexed == 0 {
				t.Fatalf("the first harvest indexed nothing; diagnostics: %+v", first.Diagnostics)
			}
			state, err := database.ReadIndexState(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if state == nil {
				t.Fatal("no stored index state after the first harvest")
			}
			target := ingest.HarvesterVersionRegistry[harness]
			if state.IndexerVersion != target.IndexerVersion {
				t.Fatalf("stored producer revision = %d, want %d; a stale session must advance to the current producer or it is re-selected forever", state.IndexerVersion, target.IndexerVersion)
			}
			if state.IndexedInputHash == nil {
				t.Fatal("the first harvest recorded no input proof; without it the content pass re-attempts the session every harvest")
			}
			if state.ArtifactHash == nil || *state.ArtifactHash != pair.ArtifactHash {
				t.Fatalf("stored artifact identity = %v, want the parsed pair identity %s; the index write must establish a missing identity and never alter a stored one", state.ArtifactHash, pair.ArtifactHash)
			}
			capture, found, err := database.GetSessionContentCapture(ctx, id)
			if err != nil || !found {
				t.Fatalf("the first harvest stored no content capture: found=%t err=%v", found, err)
			}
			if string(capture.Status) != fixture.WantCaptureStatus {
				t.Fatalf("capture status = %q, want %q", capture.Status, fixture.WantCaptureStatus)
			}
			if string(capture.CaptureFormat) != fixture.WantCaptureFormat {
				t.Fatalf("capture format = %q, want %q", capture.CaptureFormat, fixture.WantCaptureFormat)
			}
			if string(capture.FailureCode) != fixture.WantFailureCode {
				t.Fatalf("capture failure code = %q, want %q", capture.FailureCode, fixture.WantFailureCode)
			}
			entries, err := database.ListEntries(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != fixture.WantEntries {
				t.Fatalf("stored entries = %d, want %d; the represented entries must be stored", len(entries), fixture.WantEntries)
			}
			if fixture.WantRetainedKind != "" {
				var records []ingest.RetainedUnknown
				for _, entry := range entries {
					found, err := ingest.RetainedUnknownOf(entry)
					if err != nil {
						t.Fatal(err)
					}
					records = append(records, found...)
				}
				if len(records) != 1 || records[0].Kind != fixture.WantRetainedKind || len(records[0].Payload) == 0 {
					t.Fatalf("retained source evidence missing: %+v", records)
				}
				counts := first.Summary.RetainedUnknownKinds
				if len(counts) != 1 || counts[0].Kind != fixture.WantRetainedKind || counts[0].Occurrences != 1 || counts[0].Sessions != 1 {
					t.Fatalf("retained accounting: %+v", counts)
				}
			}

			second := run()
			if second.Summary.Indexed != 0 {
				t.Fatalf("the second harvest indexed %d session(s); a settled session owes no parser work", second.Summary.Indexed)
			}
			if reads := fs.ReadCount(path); reads != 0 {
				t.Fatalf("the second harvest read the retained transcript %d time(s), want 0", reads)
			}
			// This case carries one session and nothing else, so a settled
			// second harvest owes no diagnostic at all, not merely none that
			// names the session.
			if len(second.Diagnostics) != 0 {
				t.Fatalf("the second harvest reported %d diagnostic(s), want 0: %+v", len(second.Diagnostics), second.Diagnostics)
			}
		})
	}
}

// TestOrdinaryHarvestReportsStaleSessionsOutsideTheSessionAllowlist seeds one
// stored session that BOTH maintenance inventories select (a stale revision
// and a recorded pair whose index input proof is missing) and runs the
// ordinary harvest with a session allowlist that excludes it. The session must
// stay stale, and the run must report it exactly once with the remedy instead
// of folding it into the unchanged total.
func TestOrdinaryHarvestReportsStaleSessionsOutsideTheSessionAllowlist(t *testing.T) {
	t.Parallel()
	document := loadStaleIndexSettlingFixtures(t)
	var fixture *staleIndexSettlingFixture
	for index := range document.Cases {
		if document.Cases[index].Name == "claude-stored-identity-strict-refused" {
			fixture = &document.Cases[index]
			break
		}
	}
	if fixture == nil {
		t.Fatal("fixture declares no claude-stored-identity-strict-refused case")
	}
	if !fixture.StoredIdentity {
		t.Fatal("fixture invariant: the stored-identity case must record an artifact identity so both maintenance inventories select it")
	}

	ctx := t.Context()
	fs := testutil.NewCountingFS(testutil.NewMemFS())
	database := storetest.OpenWith(t)
	defer database.Close()

	id, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	transcript := []byte(document.Transcripts[fixture.Transcript])
	meta := makeMinimalMeta(t, id.String())
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.ContentHash = schema.ComputeTranscriptHash(transcript)
	dir := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String())
	if err := fs.WriteFile(filepath.Join(dir, id.String()+"--transcript.jsonl"), transcript, 0600); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(dir, id.String()+"--metadata.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	// The recorded identity is what makes the repair inventory select the
	// session on top of the stale revision: the shape that used to be counted
	// twice.
	pair, err := ingest.NewManagedArtifact(encoded, transcript)
	if err != nil {
		t.Fatal(err)
	}
	results := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: pair}})
	if len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
		t.Fatalf("seed stored artifact identity: %+v", results)
	}
	seedStalePreviewCapture(t, ctx, database, id)

	// A committed selected-mode policy crosses as an allowlist naming the
	// sessions the wizard kept. This run names a different session, so the
	// seeded one is out of scope.
	other, err := ingest.NewSessionID("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	cfg := makePipelineConfig(testOutputDir, func(cfg *ingest.PipelineConfig) {
		cfg.AllowedSessionIDs = map[ingest.SessionID]bool{other: true}
	})
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if result.Summary.Indexed != 0 {
		t.Fatalf("a session outside the allowlist was indexed; diagnostics: %+v", result.Diagnostics)
	}
	state, err := database.ReadIndexState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.IndexerVersion != 15 || state.IndexedInputHash != nil {
		t.Fatalf("a session outside the allowlist changed its stored index state: %+v", state)
	}
	var reported *ingest.DiagnosticEntry
	reportedCount := 0
	for index := range result.Diagnostics {
		if result.Diagnostics[index].ErrorType == "stale_index_outside_selection" {
			reported = &result.Diagnostics[index]
			reportedCount++
		}
	}
	if reportedCount != 1 || reported == nil {
		t.Fatalf("the run reported %d selection diagnostics, want exactly 1: %+v", reportedCount, result.Diagnostics)
	}
	if !strings.Contains(reported.Message, "1 stored session") {
		t.Fatalf("diagnostic message = %q, want the skipped count of one", reported.Message)
	}
	if !strings.Contains(reported.Remediation, "peasant harvest index") {
		t.Fatalf("diagnostic remediation = %q, want the harvest index remedy", reported.Remediation)
	}
}

// TestOrdinaryHarvestReportsStaleSessionsWithoutInput seeds a stale stored
// session whose managed pair and recorded source are both absent and runs the
// ordinary harvest without a session allowlist. The run must count it and
// report the missing-input remedy instead of dropping it silently.
func TestOrdinaryHarvestReportsStaleSessionsWithoutInput(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fs := testutil.NewCountingFS(testutil.NewMemFS())
	database := storetest.OpenWith(t)
	defer database.Close()

	id, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	// No pair is written and the metadata records no source path, so neither
	// reconstruction path can produce the session.
	meta := makeMinimalMeta(t, id.String())
	meta.ModelHarness = ingest.HarnessClaudeCode
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	seedStalePreviewCapture(t, ctx, database, id)

	cfg := makePipelineConfig(testOutputDir)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if result.Summary.Indexed != 0 {
		t.Fatalf("a session with no readable input was indexed; diagnostics: %+v", result.Diagnostics)
	}
	var reported *ingest.DiagnosticEntry
	for index := range result.Diagnostics {
		if result.Diagnostics[index].ErrorType == "stale_index_missing_input" {
			reported = &result.Diagnostics[index]
		}
	}
	if reported == nil {
		t.Fatalf("the run did not report the stale session without input: %+v", result.Diagnostics)
	}
	if !strings.Contains(reported.Message, "1 stored session") {
		t.Fatalf("diagnostic message = %q, want the missing-input count of one", reported.Message)
	}
}

// seedStalePreviewCapture stores the exact state an older build leaves for the
// sessions this regression is about: entries at producer 15, an incomplete
// legacy-preview capture, and no index input proof.
func seedStalePreviewCapture(t *testing.T, ctx context.Context, database *store.Store, id ingest.SessionID) {
	t.Helper()
	results := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{}, IndexVersion: 1, IndexedAtMs: 1700000000000,
		IndexerVersion: 15,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureIncomplete, SourceAuthority: ingest.ContentSourcePeasantSnapshot,
			CaptureFormat: ingest.ContentCaptureFormatLegacyPreviewOnly, CapturedAtMs: 1700000000000,
			FailureCode: ingest.ContentCaptureLegacyPreviewOnly,
		},
	}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("seed stale preview capture: %+v", results)
	}
}

// TestOrdinaryHarvestSettlesLatestAttempt drives the ORDINARY harvest over a
// real generation-capable store holding a full read authority with a fully
// settled latest attempt beside it. The attempt already consumed the current
// pair under this build's producer and adapter targets, so the harvest selects
// no work for the session: it reads no transcript, indexes nothing, and leaves
// both the active authority's producer stamp and the latest-attempt pointer
// exactly as they were. The per-facet selection surfaces and the replace/true
// no-op lifecycle are covered by the store family; this case proves the
// production harvest path obeys them.
func TestOrdinaryHarvestSettlesLatestAttempt(t *testing.T) {
	t.Parallel()
	document := loadStaleIndexSettlingFixtures(t)
	for _, fixture := range document.LatestAttemptCases {
		t.Run(fixture.Name, func(t *testing.T) {
			harness := ingest.Harness(fixture.Harness)
			if !harness.IsKnown() {
				t.Fatalf("fixture names unknown harness %q", fixture.Harness)
			}
			ctx := t.Context()
			fs := testutil.NewCountingFS(testutil.NewMemFS())
			root := filepath.Join(t.TempDir(), "artifacts")
			artifacts, err := store.NewOSGenerationArtifactStore(root)
			if err != nil {
				t.Fatal(err)
			}
			locker, err := store.NewFileSessionLocker(root)
			if err != nil {
				t.Fatal(err)
			}
			database := storetest.OpenWith(t, store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))

			id, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			transcript := []byte(document.Transcripts[fixture.Transcript])
			meta := makeMinimalMeta(t, id.String())
			meta.ModelHarness = harness
			meta.ContentHash = schema.ComputeTranscriptHash(transcript)
			sessionDir := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String())
			path := filepath.Join(sessionDir, id.String()+"--transcript.jsonl")
			if err := fs.WriteFile(path, transcript, 0600); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile(filepath.Join(sessionDir, id.String()+"--metadata.json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
				t.Fatal(err)
			}

			target := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[harness]
			sid := schema.SessionID(id.String())
			identity := strings.Repeat("7", 64)
			seedSettledLatestAttempt(t, ctx, database, sid, target, identity)

			cfg := makePipelineConfig(testOutputDir)
			pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), ingest.DefaultAdapterRegistry, cfg,
				ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database),
				ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
				ingest.WithHarvesterVersions(ingest.GenerationTargets(ingest.HarvesterVersionRegistry)))
			if err != nil {
				t.Fatal(err)
			}
			fs.ResetCounts()
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatalf("harvest: %v", err)
			}
			if result.Summary.Indexed != 0 {
				t.Fatalf("a settled latest attempt was re-indexed (%d session(s)); diagnostics: %+v", result.Summary.Indexed, result.Diagnostics)
			}
			if reads := fs.ReadCount(path); reads != 0 {
				t.Fatalf("the harvest read the settled attempt's transcript %d time(s), want 0", reads)
			}
			state, err := database.ReadIndexState(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if state == nil {
				t.Fatal("no stored index state after the harvest")
			}
			if state.IndexerVersion != 1 {
				t.Fatalf("the authority's producer stamp moved to %d, want the authority's own 1; the harvest must not stamp A as current", state.IndexerVersion)
			}
			if state.ArtifactHash == nil || *state.ArtifactHash != identity {
				t.Fatalf("stored pair identity = %v, want the recorded %s", state.ArtifactHash, identity)
			}
			if state.LatestAttemptArtifactHash == nil || *state.LatestAttemptArtifactHash != identity {
				t.Fatalf("latest-attempt pair facet = %v, want the recorded %s", state.LatestAttemptArtifactHash, identity)
			}
			if state.LatestAttemptIndexerVersion == nil || *state.LatestAttemptIndexerVersion != target.IndexerVersion {
				t.Fatalf("latest-attempt producer facet = %v, want %d", state.LatestAttemptIndexerVersion, target.IndexerVersion)
			}
			if state.LatestAttemptAdapterVersion == nil || *state.LatestAttemptAdapterVersion != target.AdapterVersion {
				t.Fatalf("latest-attempt adapter facet = %v, want %d", state.LatestAttemptAdapterVersion, target.AdapterVersion)
			}
		})
	}
}

// seedSettledLatestAttempt seeds the coexistence state the settled harvest case
// starts from: a complete full generation installed as the read authority, then
// a complete preview attempt over it recorded as the latest attempt with the
// authority's pair identity. Both writes go through the real activation route,
// so the seeded state is exactly what production leaves.
func seedSettledLatestAttempt(t *testing.T, ctx context.Context, database *store.Store, sid schema.SessionID, target ingest.HarvesterVersions, identity string) {
	t.Helper()
	authority, authorityBlobs := buildSettlingGeneration(t, sid, "gen_settle_authority", "authority text", "authority input", "authority output")
	assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
		Harness: ingest.HarnessClaudeCode, Result: authority,
		Policy: ingest.CaptureFreshCandidate, Authoritative: true,
	})
	if err != nil {
		t.Fatalf("assess the read authority: %v", err)
	}
	capture, err := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1)
	if err != nil {
		t.Fatalf("build the read authority's capture: %v", err)
	}
	outcome, err := database.ActivateGeneration(ctx, store.GenerationActivation{
		Generation: authority, Blobs: authorityBlobs, IndexerVersion: 1, IndexedAtMs: 1, ContentCapture: capture,
	})
	if err != nil {
		t.Fatalf("activate the read authority: %v", err)
	}
	if outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("read authority disposition = %v, want ActivationCommittedNow", outcome.Disposition)
	}

	// The pair-identity claim is only admitted against the captured SQL state,
	// so the activation carries the snapshot it describes.
	expected, err := database.ReadIndexState(ctx, ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("read the index state before the attempt: %v", err)
	}
	if expected == nil {
		t.Fatal("the read authority left no stored index state")
	}
	attempt, attemptBlobs := buildSettlingGeneration(t, sid, "gen_settle_attempt", "attempt text", "attempt input", "attempt output")
	adapter := target.AdapterVersion
	attempt.Generation.Metadata.AdapterVersion = &adapter
	preview, err := database.ActivateGeneration(ctx, store.GenerationActivation{
		Generation: attempt, Blobs: attemptBlobs, ExpectedState: expected,
		IndexerVersion: target.IndexerVersion, IndexedAtMs: 2,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status:           ingest.ContentCaptureIncomplete,
			SourceAuthority:  ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile,
			CaptureFormat:    ingest.ContentCaptureFormatPreviewOnly,
			FailureCode:      ingest.ContentCaptureSourceRecordsOmitted,
			FailureMessage:   "oversized source records were omitted",
			CapturedAtMs:     1,
		},
		ArtifactIdentity: &identity,
	})
	if err != nil {
		t.Fatalf("activate the latest attempt: %v", err)
	}
	if preview.Disposition != ingest.ActivationLatestAttemptPreview {
		t.Fatalf("latest-attempt disposition = %v, want ActivationLatestAttemptPreview", preview.Disposition)
	}
}

// buildSettlingGeneration builds a valid, self-contained complete V2 candidate
// whose content records carry only their refs; activation staging fills the
// managed relative path, byte length and integrity digest.
func buildSettlingGeneration(t *testing.T, sid schema.SessionID, genID, text, toolInput, toolOutput string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	refs := []schema.SourceEntryRef{"e_u1", "e_call1", "e_result1"}
	entries := []schema.SessionEntry{
		{
			SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeText,
			Role: ingest.RoleUser, ContentPreview: &text, SourceEntryRef: refs[0],
		},
		{
			SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse,
			Role: ingest.RoleAssistant, ToolInput: &toolInput, SourceEntryRef: refs[1],
		},
		{
			SessionID: sid, EntryIndex: 2, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolResult,
			Role: ingest.RoleTool, ToolOutput: &toolOutput, SourceEntryRef: refs[2],
		},
	}
	inputCount := int64(1)
	generation := indexformat.Generation{
		ID:           genID,
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Stats:         schema.SessionStats{TurnCount: len(entries), InputSubmissionCount: &inputCount},
		},
		Main:                 indexformat.Partition{Entries: entries},
		Content:              []indexformat.ContentRecord{{Ref: refs[0]}, {Ref: refs[1]}, {Ref: refs[2]}},
		Aliases:              []indexformat.NativeAlias{{NativeKey: "native-0", Ref: refs[0]}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{refs[0]},
	}
	blobs := map[schema.SourceEntryRef][]byte{
		refs[0]: []byte(text),
		refs[1]: []byte(toolInput),
		refs[2]: []byte(toolOutput),
	}
	return indexformat.V2{Generation: generation}, blobs
}
