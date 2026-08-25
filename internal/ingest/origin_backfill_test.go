package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/origin/stored_backfill.yaml
var storedBackfillFixtureBytes []byte

// storedBackfillFixture is the whole corpus: one case per evidence source and
// per hazard, each describing a complete store.
type storedBackfillFixture struct {
	RequiredCaseNames []string             `yaml:"required_case_names"`
	Cases             []storedBackfillCase `yaml:"cases"`
}

type storedBackfillCase struct {
	Name        string                      `yaml:"name"`
	Arm         string                      `yaml:"arm"`
	Transcripts []claudeEvidenceFile        `yaml:"transcripts"`
	Cache       []storedBackfillCacheRecord `yaml:"cache"`
	// UnreadableTranscripts are paths the case writes and then makes refuse to
	// be read: present in the directory, unreadable when opened. A permission
	// change or a drive that went away mid-run looks exactly like this.
	UnreadableTranscripts []string                    `yaml:"unreadable_transcripts"`
	Rows                  []storedBackfillRow         `yaml:"rows"`
	Expected              []storedBackfillExpectation `yaml:"expected"`
}

// storedBackfillCacheRecord is an evidence-cache row the discovery this pass
// rides on has already written.
type storedBackfillCacheRecord struct {
	Path   string `yaml:"path"`
	Origin string `yaml:"origin"`
}

// storedBackfillRow is one session an earlier run persisted.
type storedBackfillRow struct {
	SessionID string `yaml:"session_id"`
	ParentID  string `yaml:"parent_id"`
	// Harness names the producer. Empty means Claude Code, the only harness
	// this build can re-read origin evidence from.
	Harness string `yaml:"harness"`
	// SourcePath is where the transcript was read from. The fixture may name a
	// path it never writes, which is the transcript-is-gone case.
	SourcePath string `yaml:"source_path"`
	// StoredOrigin is the verdict already on the row; empty means the column
	// default that migration writes.
	StoredOrigin string `yaml:"stored_origin"`
	// AtCurrentRuleVersion puts the row above the version line before the pass
	// runs, so the pass must not list it at all.
	AtCurrentRuleVersion bool `yaml:"at_current_rule_version"`
	// FirstUserMessage is the indexed first user entry, which is the last
	// surviving content evidence once the transcript is gone.
	FirstUserMessage string `yaml:"first_user_message"`
}

type storedBackfillExpectation struct {
	SessionID string `yaml:"session_id"`
	// Origin is the session_origin the row must carry when the pass returns.
	Origin string `yaml:"origin"`
	// Finalised is whether origin_version reached the current rule version.
	Finalised bool `yaml:"finalised"`
	// Examined is whether the pass listed the row. Absent means yes.
	Examined *bool `yaml:"examined"`
	// Written is whether the pass had to persist a change for the row.
	Written bool `yaml:"written"`
}

// examined answers the fixture's default: a row is examined unless the case
// says otherwise, because the whole point of the pass is that it looks at
// everything below the line.
func (e storedBackfillExpectation) examined() bool {
	return e.Examined == nil || *e.Examined
}

// degraded is what the expectation implies about the watermark: a row the pass
// looked at and did NOT finalise is a row it resolved without the transcript.
func (e storedBackfillExpectation) degraded() bool {
	return e.examined() && !e.Finalised
}

// storedBackfillRuleVersion is the version the pass is run at throughout. It is
// taken from production so a bump reclassifies these arms too, exactly as it
// reclassifies a real store.
const storedBackfillRuleVersion = ingest.OriginRuleVersion

// LoadStoredBackfillFixtures decodes the corpus and refuses a fixture that has
// lost a required case, repeated a name, expects nothing, declares an origin
// outside the production menu, or expects a session it never stored.
func LoadStoredBackfillFixtures(data []byte) (storedBackfillFixture, error) {
	var fixture storedBackfillFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return storedBackfillFixture{}, fmt.Errorf("decode stored-origin backfill fixture first document: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return storedBackfillFixture{}, fmt.Errorf("stored-origin backfill fixture must contain exactly one YAML document: %v", err)
	}

	present := make(map[string]bool, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" || tc.Arm == "" {
			return storedBackfillFixture{}, errors.New("stored-origin backfill fixture holds a case with no name or no arm")
		}
		if present[tc.Name] {
			return storedBackfillFixture{}, fmt.Errorf("stored-origin backfill fixture repeats case name %q", tc.Name)
		}
		present[tc.Name] = true
		if len(tc.Rows) == 0 {
			return storedBackfillFixture{}, fmt.Errorf("stored-origin backfill case %q stores no session, so it proves nothing", tc.Name)
		}
		if len(tc.Expected) != len(tc.Rows) {
			return storedBackfillFixture{}, fmt.Errorf(
				"stored-origin backfill case %q stores %d sessions but expects %d outcomes; every stored row needs an "+
					"expectation, because the report counters are derived from them",
				tc.Name, len(tc.Rows), len(tc.Expected))
		}
		stored := make(map[string]bool, len(tc.Rows))
		for _, row := range tc.Rows {
			if row.SessionID == "" || row.SourcePath == "" {
				return storedBackfillFixture{}, fmt.Errorf("stored-origin backfill case %q holds a row with no session id or no source path", tc.Name)
			}
			stored[row.SessionID] = true
		}
		for _, want := range tc.Expected {
			if !stored[want.SessionID] {
				return storedBackfillFixture{}, fmt.Errorf(
					"stored-origin backfill case %q expects an outcome for session %q, which it never stores",
					tc.Name, want.SessionID)
			}
			if _, err := sessionorigin.Parse(want.Origin); err != nil {
				return storedBackfillFixture{}, fmt.Errorf(
					"stored-origin backfill case %q expects origin %q for session %q: %w",
					tc.Name, want.Origin, want.SessionID, err)
			}
		}
	}
	if err := testutil.RequireFixtureNames("stored-origin backfill fixture", "case", fixture.RequiredCaseNames, present); err != nil {
		return storedBackfillFixture{}, err
	}
	return fixture, nil
}

// storedBackfillWorld is one fixture case realised: a real local store holding
// the rows, a filesystem holding whichever transcripts survived, and the real
// Claude adapter as the miner.
type storedBackfillWorld struct {
	database *store.Store
	miners   map[ingest.Harness]ingest.OriginEvidenceMiner
}

// refusingFS is a filesystem where some files exist but cannot be read. It is
// the difference between a transcript being PRESENT and a transcript being
// EVIDENCE: a directory entry proves neither, and only a successful read does.
type refusingFS struct {
	*testutil.MemFS
	refused map[string]bool
}

func (f *refusingFS) ReadFile(path string) ([]byte, error) {
	if f.refused[path] {
		return nil, fmt.Errorf("read %q: permission denied", path)
	}
	return f.MemFS.ReadFile(path)
}

func newStoredBackfillWorld(t *testing.T, tc storedBackfillCase) storedBackfillWorld {
	t.Helper()
	ctx := context.Background()

	database, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"))
	if err != nil {
		t.Fatalf("open the local store: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close the local store: %v", err)
		}
	})

	memFS := testutil.NewMemFS()
	for _, transcript := range tc.Transcripts {
		body := strings.Join(transcript.Lines, "\n") + "\n"
		if err := memFS.WriteFile(transcript.Path, []byte(body), 0o644); err != nil {
			t.Fatalf("write transcript fixture %q: %v", transcript.Path, err)
		}
	}
	transcriptFS := ingest.FileSystem(memFS)
	if len(tc.UnreadableTranscripts) > 0 {
		refused := make(map[string]bool, len(tc.UnreadableTranscripts))
		for _, path := range tc.UnreadableTranscripts {
			refused[path] = true
		}
		transcriptFS = &refusingFS{MemFS: memFS, refused: refused}
	}

	// Store the rows through the production insert path, one batch, in fixture
	// order so a child never precedes the parent it points at.
	entries := make([]ingest.StoreEntry, 0, len(tc.Rows))
	for _, row := range tc.Rows {
		entries = append(entries, storedBackfillEntry(t, row))
	}
	if err := database.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("insert the stored sessions: %v", err)
	}

	for _, row := range tc.Rows {
		if row.FirstUserMessage == "" {
			continue
		}
		sid, err := ingest.NewSessionID(row.SessionID)
		if err != nil {
			t.Fatalf("NewSessionID(%q): %v", row.SessionID, err)
		}
		preview := row.FirstUserMessage
		if err := database.IndexSessionEntries(ctx, sid, []schema.SessionEntry{{
			SessionID:      sid,
			EntryIndex:     0,
			Harness:        storedBackfillHarness(row),
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			ContentPreview: &preview,
			Depth:          0,
		}}); err != nil {
			t.Fatalf("index the stored first user message for %q: %v", row.SessionID, err)
		}
	}

	if len(tc.Cache) > 0 {
		records := make([]ingest.ClaudeTranscriptEvidence, 0, len(tc.Cache))
		for _, record := range tc.Cache {
			origin, err := sessionorigin.Parse(record.Origin)
			if err != nil {
				t.Fatalf("cache record %q declares origin %q: %v", record.Path, record.Origin, err)
			}
			records = append(records, ingest.ClaudeTranscriptEvidence{
				SourcePath:            ingest.ResolvedPath(record.Path),
				Scope:                 ingest.ClaudeEvidenceScopeRoot,
				HasConversationRecord: true,
				Origin:                origin,
			})
		}
		if err := database.SaveClaudeEvidence(ctx, records, nil); err != nil {
			t.Fatalf("seed the evidence cache: %v", err)
		}
	}

	// Put the already-judged rows above the version line, through the same
	// production update the pass itself uses.
	for _, row := range tc.Rows {
		if !row.AtCurrentRuleVersion {
			continue
		}
		sid, err := ingest.NewSessionID(row.SessionID)
		if err != nil {
			t.Fatalf("NewSessionID(%q): %v", row.SessionID, err)
		}
		if err := database.UpdateOriginState(ctx, sid, storedBackfillStoredOrigin(row), storedBackfillRuleVersion); err != nil {
			t.Fatalf("put %q above the version line: %v", row.SessionID, err)
		}
	}

	adapter := ingest.NewClaudeAdapter(transcriptFS, testutil.DefaultGitResolver(), salt.Salt{})
	return storedBackfillWorld{
		database: database,
		miners:   map[ingest.Harness]ingest.OriginEvidenceMiner{ingest.HarnessClaudeCode: adapter},
	}
}

// resolver builds the production resolver over this world, optionally through a
// store wrapper that can interrupt it.
func (w storedBackfillWorld) resolver(t *testing.T, backing ingest.OriginResolverStore) *ingest.OriginResolver {
	t.Helper()
	if backing == nil {
		backing = w.database
	}
	resolver, err := ingest.NewOriginResolver(backing, w.database, w.miners)
	if err != nil {
		t.Fatalf("build the stored-origin resolver: %v", err)
	}
	return resolver
}

// storedOrigins reads back every row's verdict and watermark through the
// production listing, asking for one version above the rule so that finalised
// rows are included too.
func (w storedBackfillWorld) storedOrigins(t *testing.T) map[string]ingest.StoredOriginRow {
	t.Helper()
	rows, err := w.database.ListStaleOriginSessions(context.Background(), storedBackfillRuleVersion+1)
	if err != nil {
		t.Fatalf("read back the stored origins: %v", err)
	}
	byID := make(map[string]ingest.StoredOriginRow, len(rows))
	for _, row := range rows {
		byID[string(row.SessionID)] = row
	}
	return byID
}

// storedBackfillHarness answers the fixture's default: a row is a Claude Code
// row unless the case says otherwise.
func storedBackfillHarness(row storedBackfillRow) ingest.Harness {
	if row.Harness == "" {
		return ingest.HarnessClaudeCode
	}
	return ingest.Harness(row.Harness)
}

func storedBackfillStoredOrigin(row storedBackfillRow) string {
	if row.StoredOrigin == "" {
		return sessionorigin.Unknown.String()
	}
	return row.StoredOrigin
}

// storedBackfillEntry builds one insert through the real metadata type, so the
// row the pass later reads is a row production could have written.
func storedBackfillEntry(t *testing.T, row storedBackfillRow) ingest.StoreEntry {
	t.Helper()
	sid, err := ingest.NewSessionID(row.SessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", row.SessionID, err)
	}
	projectHash, err := ingest.NewProjectHash(strings.Repeat("b", 64))
	if err != nil {
		t.Fatalf("NewProjectHash: %v", err)
	}
	hostSlug, err := ingest.NewHostSlug("github.com-origin-backfill")
	if err != nil {
		t.Fatalf("NewHostSlug: %v", err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatalf("NewModelID: %v", err)
	}
	var parent *ingest.SessionID
	if row.ParentID != "" {
		parentID, err := ingest.NewSessionID(row.ParentID)
		if err != nil {
			t.Fatalf("NewSessionID(parent %q): %v", row.ParentID, err)
		}
		parent = &parentID
	}
	origin, err := sessionorigin.Parse(storedBackfillStoredOrigin(row))
	if err != nil {
		t.Fatalf("row %q declares stored origin %q: %v", row.SessionID, row.StoredOrigin, err)
	}
	ingested := int64(1700000120000)
	return ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ParentUUID:    parent,
			ModelHarness:  storedBackfillHarness(row),
			Model:         model,
			HostSlug:      hostSlug,
			Timestamp: ingest.TimestampInfo{
				Start:    1700000000000,
				End:      1700000060000,
				Ingested: &ingested,
			},
			Source: ingest.SourceInfo{
				FilePath: row.SourcePath,
				Format:   ingest.SourceFormatJSONL,
			},
			Project: ingest.ProjectInfo{
				Hash:     projectHash,
				Name:     "origin-backfill",
				FilePath: "/workspace",
			},
			Stats:       ingest.StatsInfo{TurnCount: 1},
			Version:     "2.1.71",
			Subagents:   []ingest.SubagentRef{},
			Diagnostics: ingest.DiagnosticsInfo{Warnings: []ingest.DiagnosticEntry{}},
		},
		Session: ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      storedBackfillHarness(row),
			SourcePath:   ingest.ResolvedPath(row.SourcePath),
			SourceFormat: ingest.SourceFormatJSONL,
			ParentUUID:   parent,
			Origin:       origin,
		},
	}
}

// expectedReport derives the three counters from the per-row expectations, so
// the counters and the store can never be asserted to disagree.
func expectedReport(tc storedBackfillCase) ingest.ResolveReport {
	var report ingest.ResolveReport
	for _, want := range tc.Expected {
		if want.examined() {
			report.Examined++
		}
		if want.Written {
			report.Written++
		}
		if want.degraded() {
			report.Degraded++
		}
	}
	return report
}

func assertStoredBackfillRows(t *testing.T, label string, tc storedBackfillCase, got map[string]ingest.StoredOriginRow) {
	t.Helper()
	for _, want := range tc.Expected {
		row, ok := got[want.SessionID]
		if !ok {
			t.Fatalf("%s: session %q is not in the store at all", label, want.SessionID)
		}
		if row.StoredOrigin != want.Origin {
			t.Errorf("%s: session %q carries origin %q, want %q", label, want.SessionID, row.StoredOrigin, want.Origin)
		}
		finalised := row.OriginVersion >= storedBackfillRuleVersion
		if finalised != want.Finalised {
			t.Errorf("%s: session %q is at origin version %d (finalised=%v), want finalised=%v; a row resolved without "+
				"its transcript must stay below version %d so a later run retries it",
				label, want.SessionID, row.OriginVersion, finalised, want.Finalised, storedBackfillRuleVersion)
		}
	}
}

// TestResolveStoredOriginsWritesAVerdictIntoEveryRow is the acceptance evidence
// for the pass: every stored row carries a verdict after the FIRST run, with no
// command, no re-import and no second run, and only full-evidence rows are
// finalised.
func TestResolveStoredOriginsWritesAVerdictIntoEveryRow(t *testing.T) {
	fixture, err := LoadStoredBackfillFixtures(storedBackfillFixtureBytes)
	if err != nil {
		t.Fatalf("load stored-origin backfill fixture: %v", err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()
			world := newStoredBackfillWorld(t, tc)

			report, err := world.resolver(t, nil).ResolveStoredOrigins(ctx, storedBackfillRuleVersion)
			if err != nil {
				t.Fatalf("first pass: %v", err)
			}
			if want := expectedReport(tc); report != want {
				t.Errorf("first pass reported %+v, want %+v", report, want)
			}
			assertStoredBackfillRows(t, "after the first pass", tc, world.storedOrigins(t))

			// Every arm is also an idempotence arm: a further pass over a store
			// whose verdicts are already right must persist nothing, including
			// for the rows it has to keep re-examining forever.
			second, err := world.resolver(t, nil).ResolveStoredOrigins(ctx, storedBackfillRuleVersion)
			if err != nil {
				t.Fatalf("second pass: %v", err)
			}
			if second.Written != 0 {
				t.Errorf("second pass wrote %d rows, want none: a settled verdict must not be rewritten", second.Written)
			}
			if second.Examined != report.Degraded {
				t.Errorf("second pass examined %d rows, want the %d rows the first pass left retryable",
					second.Examined, report.Degraded)
			}
			if second.Degraded != report.Degraded {
				t.Errorf("second pass reported %d degraded rows, want %d", second.Degraded, report.Degraded)
			}
			assertStoredBackfillRows(t, "after the second pass", tc, world.storedOrigins(t))
		})
	}
}

// TestResolveStoredOriginsLeavesEveryRowRetryableWhenNoTranscriptSurvives is
// the trap arm stated as its own guard, because it is the one most easily
// written vacuously.
//
// It asserts the property directly against the store rather than through the
// case table: NOT ONE row may be at or above the rule version, and the pass
// must still have judged every one of them. An implementation that finalised
// what it resolved from a stored message fails here on every row.
func TestResolveStoredOriginsLeavesEveryRowRetryableWhenNoTranscriptSurvives(t *testing.T) {
	fixture, err := LoadStoredBackfillFixtures(storedBackfillFixtureBytes)
	if err != nil {
		t.Fatalf("load stored-origin backfill fixture: %v", err)
	}
	exercised := false
	for _, tc := range fixture.Cases {
		if tc.Arm != "all-unreadable" {
			continue
		}
		exercised = true
		t.Run(tc.Name, func(t *testing.T) {
			if len(tc.Transcripts) != 0 {
				t.Fatalf("the all-unreadable arm writes %d transcripts, so no row in it is actually degraded and the "+
					"guard proves nothing", len(tc.Transcripts))
			}
			ctx := context.Background()
			world := newStoredBackfillWorld(t, tc)

			report, err := world.resolver(t, nil).ResolveStoredOrigins(ctx, storedBackfillRuleVersion)
			if err != nil {
				t.Fatalf("resolve with no readable transcript: %v", err)
			}
			if report.Examined != len(tc.Rows) || report.Degraded != len(tc.Rows) {
				t.Fatalf("the pass examined %d and degraded %d of %d rows; with no transcript readable every row must be "+
					"both examined and left retryable", report.Examined, report.Degraded, len(tc.Rows))
			}
			for id, row := range world.storedOrigins(t) {
				if row.OriginVersion >= storedBackfillRuleVersion {
					t.Errorf("session %q was marked final at version %d on evidence that can still improve; a store whose "+
						"transcripts are all unreachable - an unmounted drive - must stay entirely retryable",
						id, row.OriginVersion)
				}
				if !sessionorigin.Origin(row.StoredOrigin).Valid() {
					t.Errorf("session %q carries %q, which is not a verdict; every row gets one on the first pass",
						id, row.StoredOrigin)
				}
				if row.StoredOrigin == sessionorigin.Agent.String() {
					t.Errorf("session %q resolved agent without its transcript; the structured markers are unrecoverable, "+
						"so a degraded row may only ever reach a person's session or unknown", id)
				}
			}
		})
	}
	if !exercised {
		t.Fatal("no all-unreadable case was exercised, so the trap guard proved nothing")
	}
}

// interruptibleOriginStore is the real store with one deliberate failure in it.
// It fails the update that follows the first successful one, which is what an
// interrupted pass looks like from the resolver's side.
type interruptibleOriginStore struct {
	*store.Store
	writes    int
	failAfter int
}

var errOriginWriteInterrupted = errors.New("the origin write was interrupted")

func (s *interruptibleOriginStore) UpdateOriginState(ctx context.Context, sessionID ingest.SessionID, origin string, version int) error {
	if s.writes >= s.failAfter {
		return errOriginWriteInterrupted
	}
	s.writes++
	return s.Store.UpdateOriginState(ctx, sessionID, origin, version)
}

// TestResolveStoredOriginsResumesAfterAnInterruptedPass proves the crash-safety
// property directly: a row is above the version line only if its verdict went
// down with it, and a row the pass never reached is untouched rather than
// half-marked. Running the pass again finishes the job.
func TestResolveStoredOriginsResumesAfterAnInterruptedPass(t *testing.T) {
	fixture, err := LoadStoredBackfillFixtures(storedBackfillFixtureBytes)
	if err != nil {
		t.Fatalf("load stored-origin backfill fixture: %v", err)
	}
	exercised := false
	for _, tc := range fixture.Cases {
		if tc.Arm != "interrupted" {
			continue
		}
		exercised = true
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()
			world := newStoredBackfillWorld(t, tc)

			interrupted := &interruptibleOriginStore{Store: world.database, failAfter: 1}
			partial, err := world.resolver(t, interrupted).ResolveStoredOrigins(ctx, storedBackfillRuleVersion)
			if !errors.Is(err, errOriginWriteInterrupted) {
				t.Fatalf("the interrupted pass returned %v, want the write failure", err)
			}
			if partial.Written != interrupted.failAfter {
				t.Fatalf("the interrupted pass reported %d writes, want the %d it managed before the failure",
					partial.Written, interrupted.failAfter)
			}

			// Whatever it managed must be internally consistent: a row that
			// moved past the line carries the verdict that moved with it, and
			// a row it never reached is exactly as it was stored.
			midway := world.storedOrigins(t)
			finalisedMidway := 0
			for id, row := range midway {
				if row.OriginVersion < storedBackfillRuleVersion {
					continue
				}
				finalisedMidway++
				if !sessionorigin.Origin(row.StoredOrigin).Valid() {
					t.Errorf("session %q sits above the version line carrying %q, which is not a verdict", id, row.StoredOrigin)
				}
			}
			if finalisedMidway != interrupted.failAfter {
				t.Fatalf("%d rows were finalised by the interrupted pass, want the %d it wrote; an interrupted pass must "+
					"never advance a row it did not commit a verdict for", finalisedMidway, interrupted.failAfter)
			}

			// Resuming reaches exactly the state an uninterrupted pass reaches.
			resumed, err := world.resolver(t, nil).ResolveStoredOrigins(ctx, storedBackfillRuleVersion)
			if err != nil {
				t.Fatalf("resumed pass: %v", err)
			}
			want := expectedReport(tc)
			if resumed.Written != want.Written-partial.Written {
				t.Errorf("the resumed pass wrote %d rows, want the %d the interruption left", resumed.Written, want.Written-partial.Written)
			}
			assertStoredBackfillRows(t, "after resuming", tc, world.storedOrigins(t))
		})
	}
	if !exercised {
		t.Fatal("no interrupted case was exercised, so the resume guard proved nothing")
	}
}

// TestResolveStoredOriginsRefusesAnUnusableRuleVersion checks the trust
// boundary on the watermark itself: a pass at version zero would list nothing,
// finalise nothing, and report success over a store where every row is still
// unjudged.
func TestResolveStoredOriginsRefusesAnUnusableRuleVersion(t *testing.T) {
	fixture, err := LoadStoredBackfillFixtures(storedBackfillFixtureBytes)
	if err != nil {
		t.Fatalf("load stored-origin backfill fixture: %v", err)
	}
	world := newStoredBackfillWorld(t, fixture.Cases[0])
	if _, err := world.resolver(t, nil).ResolveStoredOrigins(context.Background(), 0); err == nil {
		t.Fatal("a pass at rule version zero was accepted; it can only ever report success over an unjudged store")
	}
}

// TestNewOriginResolverRefusesToRunWithoutAStore checks the other trust
// boundary: a resolver with nothing to read from would report a clean pass over
// a store it never opened.
func TestNewOriginResolverRefusesToRunWithoutAStore(t *testing.T) {
	if _, err := ingest.NewOriginResolver(nil, nil, nil); err == nil {
		t.Fatal("a resolver with no store was accepted; it would report a clean pass over rows it never read")
	}
}

// TestLoadStoredBackfillFixturesRejectsADeletedCase is the deletion guard,
// proven rather than declared: drop any one case and the loader refuses the
// corpus by NAME, so an arm cannot be quietly removed to make a change pass.
func TestLoadStoredBackfillFixturesRejectsADeletedCase(t *testing.T) {
	fixture, err := LoadStoredBackfillFixtures(storedBackfillFixtureBytes)
	if err != nil {
		t.Fatalf("load stored-origin backfill fixture: %v", err)
	}
	for dropped := range fixture.Cases {
		t.Run(fixture.Cases[dropped].Name, func(t *testing.T) {
			trimmed := storedBackfillFixture{RequiredCaseNames: fixture.RequiredCaseNames}
			for i, tc := range fixture.Cases {
				if i == dropped {
					continue
				}
				trimmed.Cases = append(trimmed.Cases, tc)
			}
			encoded, err := yaml.Marshal(trimmed)
			if err != nil {
				t.Fatalf("marshal the trimmed fixture: %v", err)
			}
			if _, err := LoadStoredBackfillFixtures(encoded); err == nil {
				t.Fatal("the loader accepted a corpus with a required case removed")
			} else if !strings.Contains(err.Error(), "missing required case") {
				t.Fatalf("the loader refused for the wrong reason: %v", err)
			}
		})
	}
}
