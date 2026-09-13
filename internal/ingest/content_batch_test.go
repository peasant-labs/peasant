package ingest

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_batch.yaml
var contentBatchFixtureData []byte

type captureBatchObserver struct {
	MetricsStore
	batches []int
	cancel  context.CancelFunc
}

var _ SessionEntryBatchStore = (*captureBatchObserver)(nil)

func (s *captureBatchObserver) IndexSessionEntryBatch(_ context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	s.batches = append(s.batches, len(writes))
	results := make([]SessionEntryWriteResult, len(writes))
	for i, write := range writes {
		results[i] = SessionEntryWriteResult{SessionID: write.SessionID, Written: true}
	}
	if s.cancel != nil {
		s.cancel()
	}
	return results
}

// TestFullContentWriteBatchCommitsEachSession drives the write flush over input
// captured at the production boundary.
//
// The cases used to hand the flush a hand-built parse result whose captured input
// was nil. Production never produces that: a parse runs on input captured from the
// committed artifact, and the write carries that input's hash as its proof of what
// was parsed. The flush dereferenced it and the test died in a nil panic, which is
// a fixture arranging a state the product cannot be in.
//
// Each case now writes a managed artifact pair, captures it through
// CaptureIndexInput, and parses THAT. What the flush then owes is one atomic
// commit per session while that session's artifact is still the one it parsed, an
// outcome recorded for every session, and a stop once the run is cancelled.
func TestFullContentWriteBatchCommitsEachSession(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name    string `yaml:"name"`
			Bytes   []int  `yaml:"session_bytes"`
			Commits []int  `yaml:"expected_commits"`
			Cancel  bool   `yaml:"cancel_after_commit"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentBatchFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if names[fixture.Name] {
			t.Fatalf("duplicate fixture %s", fixture.Name)
		}
		names[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observer := &captureBatchObserver{}
			if fixture.Cancel {
				observer.cancel = cancel
			}
			output := t.TempDir()
			// Built by literal to reach the unexported flush. The path under test
			// reads exactly these: fs and config.OutputDir for the artifact
			// publisher, and metricsStore for the atomic write. Every other field
			// is nil or zero on purpose: store stays nil and metricsStore is not a
			// SessionStore, so the metadata-compatibility check has no backing
			// store and returns nil in file-only mode, asking nothing here for a
			// schema version; a nil harvester version map means the shipped
			// registry.
			assertFileOnlyCompatibility(t, observer)
			pipeline := &Pipeline{metricsStore: observer, fs: &OSFileSystem{}, config: PipelineConfig{OutputDir: ResolvedPath(output)}}
			var parsed []indexParseResult
			for index, size := range fixture.Bytes {
				parsed = append(parsed, captureSyntheticIndexInput(t, ctx, pipeline, output, index, size))
			}
			flush := pipeline.flushIndexParseResults(ctx, parsed, IndexOutcomeIndexed, "capture test", nil)
			if len(observer.batches) != len(fixture.Commits) {
				t.Fatalf("commits=%v want=%v", observer.batches, fixture.Commits)
			}
			for position, got := range observer.batches {
				if got != fixture.Commits[position] {
					t.Fatalf("commit %d carried %d sessions, want %d (commits=%v)", position, got, fixture.Commits[position], observer.batches)
				}
			}
			if len(flush.logEntries) != len(parsed) {
				t.Fatal("session outcome lost")
			}
		})
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}
}

// captureSyntheticIndexInput publishes one synthetic managed artifact of about
// the requested size and captures its parser input the way the pipeline does, so
// the flush under test receives a result production could have produced.
func captureSyntheticIndexInput(t *testing.T, ctx context.Context, pipeline *Pipeline, output string, index, size int) indexParseResult {
	t.Helper()
	sessionID, err := NewSessionID(fmt.Sprintf("11111111-1111-4111-8111-00000000000%d", index))
	if err != nil {
		t.Fatal(err)
	}
	const hostSlug = "github.com--peasant-labs--content-batch"
	meta := NewUnifiedMetadata()
	meta.SessionID = sessionID
	meta.ModelHarness = HarnessClaudeCode
	meta.Model = "claude-opus-4-8"
	meta.HostSlug = hostSlug
	meta.Timestamp = TimestampInfo{Start: 1700000000000, End: 1700000001000}
	meta.Project = ProjectInfo{Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "content-batch", FilePath: "/synthetic/content-batch"}
	transcriptPath := filepath.Join(SessionDir(output, hostSlug, string(sessionID), ""), string(sessionID)+"--transcript."+string(SourceFormatJSONL))
	meta.Source = SourceInfo{Format: SourceFormatJSONL, FilePath: transcriptPath}
	transcript := syntheticClaudeTranscript(string(sessionID), size)
	meta.ContentHash = schema.ComputeTranscriptHash(transcript)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := SessionMetadataPath(output, hostSlug, string(sessionID), "")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, metaJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, err := NewManagedArtifact(metaJSON, transcript)
	if err != nil {
		t.Fatalf("publish a synthetic managed artifact: %v", err)
	}
	indexer := NewClaudeIndexer(pipeline.fs, WithClaudeFullContent(true))
	session := DiscoveredSession{SessionID: sessionID, Harness: HarnessClaudeCode, SourceFormat: SourceFormatJSONL, SourcePath: ResolvedPath(transcriptPath)}
	input, err := CaptureIndexInput(ctx, indexer, session, artifact)
	if err != nil {
		t.Fatalf("capture the parser's input from the committed artifact: %v", err)
	}
	input.metadataPath = metadataPath
	result, err := input.Parse(ctx, indexer)
	if err != nil {
		t.Fatalf("parse the captured input: %v", err)
	}
	return indexParseResult{
		im:          indexedMeta{session: session, outputTranscriptPath: transcriptPath, startMs: meta.Timestamp.Start},
		input:       input,
		output:      result,
		fullContent: true,
		bytes:       int64(len(transcript)),
	}
}

// syntheticClaudeTranscript builds a JSONL transcript of about size bytes. The
// content is synthetic text, never a recorded session.
func syntheticClaudeTranscript(sessionID string, size int) []byte {
	line := func(text string) string {
		return fmt.Sprintf(`{"type":"user","sessionId":%q,"timestamp":"2023-11-14T22:13:20.000Z","cwd":"/synthetic/content-batch","message":{"role":"user","content":%q}}`, sessionID, text)
	}
	overhead := len(line("")) + 1
	body := size - overhead
	if body < 1 {
		body = 1
	}
	return []byte(line(strings.Repeat("a", body)) + "\n")
}

//go:embed testdata/content_batch_budget.yaml
var contentBatchBudgetFixtureData []byte

// budgetIndexer is the parser seam for the byte-budget cases: for one session it
// returns entries whose total write bytes are exactly what the case declares.
//
// The grouping boundary measures the PARSE OUTPUT, not the transcript, so a case
// can cross a 32 MiB budget from a transcript of a few bytes. Everything else on
// the path is real: the artifact is committed and validated, the input is captured
// and hashed, and the grouping and the flush are the production ones.
type budgetIndexer struct{ bytes map[SessionID]int }

var (
	_ TranscriptIndexer              = (*budgetIndexer)(nil)
	_ AuthoritativeTranscriptIndexer = (*budgetIndexer)(nil)
)

func (idx *budgetIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

func (idx *budgetIndexer) entries(session DiscoveredSession) []schema.SessionEntry {
	size := idx.bytes[session.SessionID]
	if size < 1 {
		size = 1
	}
	content := strings.Repeat("a", size)
	return []schema.SessionEntry{{
		SessionID: schema.SessionID(session.SessionID), EntryIndex: 0, Harness: HarnessClaudeCode,
		Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &content,
	}}
}

func (idx *budgetIndexer) IndexTranscript(_ context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	return idx.entries(session), nil
}

func (idx *budgetIndexer) IndexTranscriptBytes(_ context.Context, session DiscoveredSession, _ []byte) ([]schema.SessionEntry, error) {
	return idx.entries(session), nil
}

func (idx *budgetIndexer) IndexTranscriptForCapture(_ context.Context, session DiscoveredSession) (TranscriptCaptureResult, error) {
	return TranscriptCaptureResult{Entries: idx.entries(session)}, nil
}

func (idx *budgetIndexer) IndexTranscriptBytesForCapture(_ context.Context, session DiscoveredSession, _ []byte) (TranscriptCaptureResult, error) {
	return TranscriptCaptureResult{Entries: idx.entries(session)}, nil
}

// budgetStore is the store the grouping boundary writes through: it records the
// ORDER sessions were committed in and answers the index-state read the capture
// requires. The order is what reveals the grouping; see the fixture header.
type budgetStore struct {
	MetricsStore
	states    map[SessionID]*SessionIndexState
	committed []SessionID
}

var (
	_ SessionEntryBatchStore  = (*budgetStore)(nil)
	_ SessionIndexStateReader = (*budgetStore)(nil)
)

func (s *budgetStore) ReadIndexState(_ context.Context, id SessionID) (*SessionIndexState, error) {
	return s.states[id], nil
}

func (s *budgetStore) IndexSessionEntryBatch(_ context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	results := make([]SessionEntryWriteResult, len(writes))
	for i, write := range writes {
		s.committed = append(s.committed, write.SessionID)
		results[i] = SessionEntryWriteResult{SessionID: write.SessionID, Written: true}
	}
	return results
}

// TestFullContentWriteBatchBudgetGroupsByBytes holds the byte budget that bounds
// one write wave.
//
// The budget is what keeps a wave of large sessions from being held in memory at
// once; losing the split is an out-of-memory failure on a real store, not a
// cosmetic regression. It is asserted at the boundary that applies it: the batch
// grouping that decides how many parsed sessions travel to one flush.
//
// Sizes are declared as a FRACTION of the shipped budget rather than in bytes, so
// the corpus still describes the same situations if the budget ever moves.
func TestFullContentWriteBatchBudgetGroupsByBytes(t *testing.T) {
	var fixtures struct {
		Budget   string   `yaml:"budget"`
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name      string    `yaml:"name"`
			Fractions []float64 `yaml:"session_budget_fractions"`
			Groups    [][]int   `yaml:"expected_flush_groups"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentBatchBudgetFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	if fixtures.Budget != "defaults.FullContentWriteBatchBytes" {
		t.Fatalf("the corpus must name the budget under test, got %q", fixtures.Budget)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if names[fixture.Name] {
			t.Fatalf("duplicate fixture %s", fixture.Name)
		}
		names[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := context.Background()
			output := t.TempDir()
			indexer := &budgetIndexer{bytes: make(map[SessionID]int)}
			store := &budgetStore{states: make(map[SessionID]*SessionIndexState)}
			// Built by literal to reach the unexported grouping. This path reads
			// fs and config.OutputDir for the artifact publisher, metricsStore for
			// both the index-state read and the atomic write, and indexers for the
			// parse. store stays nil and metricsStore is not a SessionStore, so the
			// metadata-compatibility check has no backing store and returns nil in
			// file-only mode: nothing here is asked for a schema version. The
			// harvester version map stays nil so the shipped registry applies, and
			// the profiler stays nil and records nothing.
			assertFileOnlyCompatibility(t, store)
			pipeline := &Pipeline{
				fs:           &OSFileSystem{},
				metricsStore: store,
				indexers:     map[Harness]TranscriptIndexer{HarnessClaudeCode: indexer},
				// One parser worker keeps the waves and therefore the observed
				// grouping deterministic; Force makes every session need work, so
				// no case is skipped for being current.
				config: PipelineConfig{OutputDir: ResolvedPath(output), Parallelism: 1, Force: true},
			}
			var metas []indexedMeta
			for index, fraction := range fixture.Fractions {
				written := int(float64(defaults.FullContentWriteBatchBytes) * fraction)
				meta, artifactHash := publishSyntheticArtifact(t, output, index)
				indexer.bytes[meta.session.SessionID] = written
				store.states[meta.session.SessionID] = &SessionIndexState{
					SessionID: meta.session.SessionID, Harness: HarnessClaudeCode, ArtifactHash: &artifactHash,
				}
				metas = append(metas, meta)
			}
			indexed, logs := pipeline.indexBatch(ctx, metas, IndexOutcomeIndexed, "budget test")
			if len(indexed) != len(metas) || len(logs) != len(metas) {
				t.Fatalf("the grouping lost a session: indexed=%d logs=%d for %d sessions", len(indexed), len(logs), len(metas))
			}
			want := expectedCommitOrder(t, fixture.Groups, metas)
			if !reflect.DeepEqual(store.committed, want) {
				t.Fatalf("commit order %v does not match the flush groups %v (that grouping commits in order %v); the budget is %d bytes and this case wrote fractions %v",
					store.committed, fixture.Groups, want, defaults.FullContentWriteBatchBytes, fixture.Fractions)
			}
		})
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}
}

// publishSyntheticArtifact commits one tiny managed artifact and returns the meta
// the index stage consumes plus the artifact hash the capture will demand.
func publishSyntheticArtifact(t *testing.T, output string, index int) (indexedMeta, string) {
	t.Helper()
	// Descending identifiers: a flush sorts its own sessions, so this is what makes
	// a change in grouping show up as a change in commit order.
	sessionID, err := NewSessionID(fmt.Sprintf("22222222-2222-4222-8222-00000000000%d", 9-index))
	if err != nil {
		t.Fatal(err)
	}
	const hostSlug = "github.com--peasant-labs--content-batch-budget"
	meta := NewUnifiedMetadata()
	meta.SessionID = sessionID
	meta.ModelHarness = HarnessClaudeCode
	meta.Model = "claude-opus-4-8"
	meta.HostSlug = hostSlug
	meta.Timestamp = TimestampInfo{Start: 1700000000000, End: 1700000001000}
	meta.Project = ProjectInfo{Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "content-batch", FilePath: "/synthetic/content-batch"}
	transcriptPath := filepath.Join(SessionDir(output, hostSlug, string(sessionID), ""), string(sessionID)+"--transcript."+string(SourceFormatJSONL))
	meta.Source = SourceInfo{Format: SourceFormatJSONL, FilePath: transcriptPath}
	transcript := syntheticClaudeTranscript(string(sessionID), 256)
	meta.ContentHash = schema.ComputeTranscriptHash(transcript)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := SessionMetadataPath(output, hostSlug, string(sessionID), "")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, metaJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, err := NewManagedArtifact(metaJSON, transcript)
	if err != nil {
		t.Fatalf("publish a synthetic managed artifact: %v", err)
	}
	session := DiscoveredSession{SessionID: sessionID, Harness: HarnessClaudeCode, SourceFormat: SourceFormatJSONL, SourcePath: ResolvedPath(transcriptPath)}
	return indexedMeta{session: session, outputTranscriptPath: transcriptPath, startMs: meta.Timestamp.Start}, artifact.ArtifactHash
}

// expectedCommitOrder turns the flush groups a case declares into the commit
// order they produce.
//
// A flush commits each of its sessions separately, so the NUMBER of commits
// cannot show the grouping. The order can: a flush sorts its own sessions by
// identifier before committing them, while the flushes themselves run in parse
// order, and these fixtures seed identifiers that DESCEND as parsing advances.
// Any change in grouping therefore changes the order, which is what makes a lost
// split visible here instead of silently identical.
func expectedCommitOrder(t *testing.T, groups [][]int, metas []indexedMeta) []SessionID {
	t.Helper()
	seen := make(map[int]bool)
	var order []SessionID
	for _, group := range groups {
		var ids []SessionID
		for _, index := range group {
			if index < 0 || index >= len(metas) {
				t.Fatalf("a flush group names session %d, which this case does not seed", index)
			}
			if seen[index] {
				t.Fatalf("session %d appears in more than one flush group", index)
			}
			seen[index] = true
			ids = append(ids, metas[index].session.SessionID)
		}
		slices.Sort(ids)
		order = append(order, ids...)
	}
	if len(seen) != len(metas) {
		t.Fatalf("the flush groups cover %d of %d seeded sessions; every session is committed exactly once", len(seen), len(metas))
	}
	return order
}

// assertFileOnlyCompatibility holds what the comments above these pipelines
// claim: with no store, the metadata-compatibility check has a backing store
// only if the metrics store is also a SessionStore. These stores are not, so
// the check returns in file-only mode and asks nothing for a schema version.
//
// A future test store that grew the session-lookup method would change what
// this path reads, silently, while the comment kept saying otherwise.
func assertFileOnlyCompatibility(t *testing.T, metricsStore MetricsStore) {
	t.Helper()
	if _, backing := metricsStore.(SessionStore); backing {
		t.Fatalf("%T is now a SessionStore, so the metadata-compatibility check has a backing store and this path no longer runs in file-only mode; re-read what it asks before trusting the grouping", metricsStore)
	}
}

// streamingBudgetFixtures is the second half of the budget corpus: the
// predicate both grouping sites ask, and the streaming drain that asks it on
// the primary harvest path. The first half (indexBatch) is loaded above from
// the same file.
type streamingBudgetFixtures struct {
	Budget            string   `yaml:"budget"`
	PredicateRequired []string `yaml:"predicate_required_names"`
	PredicateCases    []struct {
		Name            string  `yaml:"name"`
		PendingCount    int     `yaml:"pending_count"`
		PendingFraction float64 `yaml:"pending_budget_fraction"`
		NextFraction    float64 `yaml:"next_budget_fraction"`
		WantFlush       bool    `yaml:"want_flush"`
	} `yaml:"predicate_cases"`
	DrainRequired []string `yaml:"drain_required_names"`
	DrainCases    []struct {
		Name      string    `yaml:"name"`
		Fractions []float64 `yaml:"session_budget_fractions"`
		Groups    [][]int   `yaml:"expected_flush_groups"`
	} `yaml:"drain_cases"`
}

func loadStreamingBudgetFixtures(t *testing.T) streamingBudgetFixtures {
	t.Helper()
	var fixtures streamingBudgetFixtures
	if err := yaml.Unmarshal(contentBatchBudgetFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	if fixtures.Budget != "defaults.FullContentWriteBatchBytes" {
		t.Fatalf("the corpus must name the budget under test, got %q", fixtures.Budget)
	}
	return fixtures
}

// requireFixtureNames refuses a corpus that lost a case. The manifest names the
// situations that must stay covered, so deleting one is a load failure rather
// than a quietly smaller run.
func requireFixtureNames(t *testing.T, corpus string, required []string, present map[string]bool) {
	t.Helper()
	if len(required) == 0 {
		t.Fatalf("%s: the corpus must declare the case names it requires", corpus)
	}
	for _, name := range required {
		if !present[name] {
			t.Fatalf("%s: required case %q is missing from the corpus", corpus, name)
		}
	}
	if len(present) != len(required) {
		t.Fatalf("%s: the corpus carries %d cases and requires %d; add the new case to the manifest", corpus, len(present), len(required))
	}
}

// budgetFractionBytes turns a fraction of the shipped budget into bytes, so a
// case keeps describing the same situation if the budget moves.
func budgetFractionBytes(fraction float64) int64 {
	return int64(float64(defaults.FullContentWriteBatchBytes) * fraction)
}

// TestIndexWriteBudgetPredicate holds the rule both grouping sites apply,
// stated over its own inputs rather than over a pipeline.
//
// The rule is small and total, so it is asserted directly: an empty batch takes
// anything (an oversized session must still be written, alone), a full batch
// flushes on the session count before bytes are consulted, and a non-empty
// batch flushes exactly when the next result would carry it past the budget.
func TestIndexWriteBudgetPredicate(t *testing.T) {
	t.Parallel()
	fixtures := loadStreamingBudgetFixtures(t)
	present := make(map[string]bool, len(fixtures.PredicateCases))
	for _, fixture := range fixtures.PredicateCases {
		if present[fixture.Name] {
			t.Fatalf("duplicate fixture %s", fixture.Name)
		}
		present[fixture.Name] = true
	}
	requireFixtureNames(t, "index write budget predicate", fixtures.PredicateRequired, present)
	for _, fixture := range fixtures.PredicateCases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			got := exceedsIndexWriteBudget(
				fixture.PendingCount,
				budgetFractionBytes(fixture.PendingFraction),
				budgetFractionBytes(fixture.NextFraction),
			)
			if got != fixture.WantFlush {
				t.Fatalf("a pending batch of %d sessions holding %.6f of the budget, offered %.6f more, flushes=%v want %v",
					fixture.PendingCount, fixture.PendingFraction, fixture.NextFraction, got, fixture.WantFlush)
			}
		})
	}
}

// TestStreamingIndexDrainGroupsByBytes holds the byte budget at the boundary
// EVERY parsed session crosses: the streaming drain that an ordinary harvest
// and a reindex both run.
//
// Losing the split here is the out-of-memory failure the budget exists to
// prevent, on the primary path rather than on the stale-session batch. The
// drain is driven over a channel the test fills and CLOSES first: its receive
// is then always ready, so the select never falls to its default and the
// grouping depends on the budget alone, not on parser timing.
func TestStreamingIndexDrainGroupsByBytes(t *testing.T) {
	t.Parallel()
	fixtures := loadStreamingBudgetFixtures(t)
	present := make(map[string]bool, len(fixtures.DrainCases))
	for _, fixture := range fixtures.DrainCases {
		if present[fixture.Name] {
			t.Fatalf("duplicate fixture %s", fixture.Name)
		}
		present[fixture.Name] = true
	}
	requireFixtureNames(t, "streaming index drain", fixtures.DrainRequired, present)
	for _, fixture := range fixtures.DrainCases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			parsedCh := make(chan indexParseResult, len(fixture.Fractions))
			for index, fraction := range fixture.Fractions {
				content := strings.Repeat("a", int(budgetFractionBytes(fraction)))
				id := SessionID(fmt.Sprintf("session-%02d", index))
				parsedCh <- indexParseResult{output: indexformat.V1{Entries: []schema.SessionEntry{{
					SessionID: schema.SessionID(id), EntryIndex: 0, Harness: HarnessClaudeCode,
					Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &content,
				}}}}
			}
			close(parsedCh)
			var groups [][]int
			drainIndexParseResults(parsedCh, make([]indexParseResult, 0, indexWriteBatchLimit), func(results []indexParseResult) {
				group := make([]int, 0, len(results))
				for _, result := range results {
					entries := result.output.(indexformat.V1).Entries
					var index int
					if _, err := fmt.Sscanf(string(entries[0].SessionID), "session-%d", &index); err != nil {
						t.Errorf("a flushed result lost its identity: %v", err)
						return
					}
					group = append(group, index)
				}
				groups = append(groups, group)
			})
			if !reflect.DeepEqual(groups, fixture.Groups) {
				t.Fatalf("the drain flushed %v, want %v: the budget decides where a wave is split", groups, fixture.Groups)
			}
		})
	}
}

// budgetTargetSession is one row the budget-pass double serves.
type budgetTargetSession struct {
	harness  Harness
	host     string
	parent   string
	complete bool
	state    *SessionIndexState
}

// budgetTargetStore is the content-recovery store the budget pass reads and
// writes through. It lists the sessions still awaiting a content capture and,
// when the pass writes a recovered capture, marks that session complete so it
// drops out of the next listing, exactly as a real store does. It embeds
// MetricsStore so it satisfies the field type without being a SessionStore, so
// the stored-metadata compatibility check treats it as file-only.
type budgetTargetStore struct {
	MetricsStore
	order    []SessionID
	sessions map[SessionID]*budgetTargetSession
}

var (
	_ ContentBackfillTargetStore = (*budgetTargetStore)(nil)
	_ SessionIndexStateReader    = (*budgetTargetStore)(nil)
)

func (s *budgetTargetStore) ListContentCaptureIncompleteSessionsAfter(_ context.Context, after SessionID, limit int) ([]ContentCaptureIncompleteSession, error) {
	var out []ContentCaptureIncompleteSession
	for _, id := range s.order {
		if string(id) <= string(after) {
			continue
		}
		session := s.sessions[id]
		if session.complete {
			continue
		}
		out = append(out, ContentCaptureIncompleteSession{SessionID: id, Harness: session.harness, StartMs: 1700000000000})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *budgetTargetStore) ReadIndexState(_ context.Context, id SessionID) (*SessionIndexState, error) {
	return s.sessions[id].state, nil
}

func (s *budgetTargetStore) LookupSessionLocation(_ context.Context, id SessionID) (string, string, error) {
	session := s.sessions[id]
	return session.host, session.parent, nil
}

func (s *budgetTargetStore) LookupSourceInfo(_ context.Context, id SessionID) (string, SourceFormat, string, error) {
	// The retained pair is on the filesystem, so the pass reads it through the
	// snapshot path and never asks for a native source here.
	return "", "", "", fmt.Errorf("budget double serves the retained pair, not a native source for %s", id)
}

func (s *budgetTargetStore) IndexSessionEntryBatch(_ context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	results := make([]SessionEntryWriteResult, len(writes))
	for i, write := range writes {
		s.sessions[write.SessionID].complete = true
		results[i] = SessionEntryWriteResult{SessionID: write.SessionID, Written: true}
	}
	return results
}

// seedBudgetContentSession writes a valid retained pair to the filesystem and
// registers the session as an incomplete recovery target in the double.
func seedBudgetContentSession(t *testing.T, fs FileSystem, store *budgetTargetStore, output string, n int) SessionID {
	t.Helper()
	sid, err := NewSessionID(fmt.Sprintf("ses_budget%05d", n))
	if err != nil {
		t.Fatal(err)
	}
	const hostSlug = "github.com--peasant-labs--content-budget"
	meta := NewUnifiedMetadata()
	meta.SessionID = sid
	meta.ModelHarness = HarnessClaudeCode
	meta.Model = "claude-opus-4-8"
	meta.HostSlug = hostSlug
	meta.Timestamp = TimestampInfo{Start: 1700000000000, End: 1700000001000}
	meta.Project = ProjectInfo{Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "content-budget", FilePath: "/synthetic/content-budget"}
	sessionDir := SessionDir(output, hostSlug, string(sid), "")
	transcriptPath := filepath.Join(sessionDir, string(sid)+"--transcript."+string(SourceFormatJSONL))
	meta.Source = SourceInfo{Format: SourceFormatJSONL, FilePath: transcriptPath}
	transcript := syntheticClaudeTranscript(string(sid), 256)
	meta.ContentHash = schema.ComputeTranscriptHash(transcript)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(SessionMetadataPath(output, hostSlug, string(sid), ""), metaJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(transcriptPath, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	store.order = append(store.order, sid)
	// ArtifactHash stays nil: this is a clean recovery target, not a torn pair,
	// so the pass recovers it and charges its input bytes.
	store.sessions[sid] = &budgetTargetSession{harness: HarnessClaudeCode, host: hostSlug, state: &SessionIndexState{SessionID: sid, Harness: HarnessClaudeCode, IndexerVersion: 1}}
	return sid
}

// TestContentBudgetStopsAfterK proves the content pass is bounded and reports the
// backlog it leaves. A one-byte budget processes exactly one session (nothing is
// charged when the pass starts, so one session runs even though it alone exceeds
// the budget) and stops, reporting the remaining count so the stage renders short
// (Done < Total) and content_remaining names the work owed. The next run
// continues that backlog with no stored cursor because the recovered session is
// already complete and drops out. A budget of zero (harvest index) is unbounded.
func TestContentBudgetStopsAfterK(t *testing.T) {
	t.Run("content_budget_stops_after_K", func(t *testing.T) {
		ctx := context.Background()
		output := t.TempDir()
		fs := &OSFileSystem{}
		store := &budgetTargetStore{sessions: make(map[SessionID]*budgetTargetSession)}

		const total = 3
		var ids []SessionID
		for i := 0; i < total; i++ {
			ids = append(ids, seedBudgetContentSession(t, fs, store, output, i))
		}

		pipeline := &Pipeline{
			fs:           fs,
			metricsStore: store,
			indexers:     NewIndexerRegistry(fs, IndexerRegistryOptions{}),
			config:       PipelineConfig{OutputDir: ResolvedPath(output), Parallelism: 1, Force: true},
		}

		// A one-byte budget stops after the first session and reports two left.
		recovered, stopped, remaining, err := pipeline.backfillIncompleteContent(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(recovered) != 1 || !stopped {
			t.Fatalf("a one-byte budget processes exactly one session and stops: recovered=%d stopped=%t", len(recovered), stopped)
		}
		if remaining != total-1 {
			t.Fatalf("a stopped pass reports the backlog it leaves: remaining=%d, want %d", remaining, total-1)
		}

		// The next run continues the backlog with no cursor: the recovered
		// session is complete and no longer listed, so it stops after one more
		// and reports one left.
		recovered2, stopped2, remaining2, err := pipeline.backfillIncompleteContent(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(recovered2) != 1 || !stopped2 || remaining2 != total-2 {
			t.Fatalf("the next run continues the backlog without a cursor: recovered=%d stopped=%t remaining=%d want remaining %d", len(recovered2), stopped2, remaining2, total-2)
		}

		// A budget of zero is unbounded: it clears the rest, does not report
		// stopping, and leaves no backlog.
		recovered3, stopped3, remaining3, err := pipeline.backfillIncompleteContent(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(recovered3) != total-2 || stopped3 || remaining3 != 0 {
			t.Fatalf("an unbounded pass clears the backlog: recovered=%d stopped=%t remaining=%d", len(recovered3), stopped3, remaining3)
		}
	})
}
