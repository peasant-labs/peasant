package store_test

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/claude_teammates.yaml
var claudeTeammatesYAML []byte

type claudeTeammateIdentityFixture struct {
	Team string `yaml:"team"`
	Name string `yaml:"name"`
}

type claudeTeammateSessionFixture struct {
	ID         string `yaml:"id"`
	Harness    string `yaml:"harness"`
	SourcePath string `yaml:"source_path"`
	ParentID   string `yaml:"parent_id"`
	// Scope is the cached evidence row's scope, "root" when the case omits it.
	// Only a root transcript can name the teammate its parent spawned.
	Scope    string                          `yaml:"scope"`
	Identity *claudeTeammateIdentityFixture  `yaml:"identity"`
	Spawns   []claudeTeammateIdentityFixture `yaml:"spawns"`
}

type claudeTeammateCase struct {
	Name         string                         `yaml:"name"`
	Subject      string                         `yaml:"subject"`
	Sessions     []claudeTeammateSessionFixture `yaml:"sessions"`
	WantPresent  bool                           `yaml:"want_present"`
	WantTeammate *claudeTeammateIdentityFixture `yaml:"want_teammate"`
	// WantError names a fragment of the refusal a stored identity the local
	// contract cannot carry must produce. Such a case has no expected identity.
	WantError string `yaml:"want_error"`
}

// claudeTeammateBatchProbe names the two recorded spawn edges the bounded-page
// test seeds at the first and last positions of one multi-batch request.
type claudeTeammateBatchProbe struct {
	First string `yaml:"first"`
	Last  string `yaml:"last"`
}

type claudeTeammatesFixture struct {
	RequiredNames []string                 `yaml:"requiredNames"`
	BatchProbe    claudeTeammateBatchProbe `yaml:"batch_probe"`
	Cases         []claudeTeammateCase     `yaml:"cases"`
}

func loadClaudeTeammatesFixture(t *testing.T) claudeTeammatesFixture {
	t.Helper()
	var fixture claudeTeammatesFixture
	if err := testutil.DecodeNamedFixtureYAML(claudeTeammatesYAML, &fixture); err != nil {
		t.Fatalf("load Claude teammate fixture: %v", err)
	}
	if fixture.BatchProbe.First == "" || fixture.BatchProbe.Last == "" {
		t.Fatalf("Claude teammate fixture must name the two batch-probe cases")
	}
	for _, testCase := range fixture.Cases {
		if testCase.Subject == "" || len(testCase.Sessions) == 0 {
			t.Fatalf("Claude teammate case %q must name a subject session and seed at least one session", testCase.Name)
		}
		if testCase.WantError != "" {
			if testCase.WantPresent || testCase.WantTeammate != nil {
				t.Fatalf("Claude teammate case %q declares want_error %q together with an expected identity; a refused identity is never also present", testCase.Name, testCase.WantError)
			}
			continue
		}
		if testCase.WantPresent != (testCase.WantTeammate != nil) {
			t.Fatalf("Claude teammate case %q declares want_present=%t with want_teammate=%v; a present case names the identity it expects and an absent case names none", testCase.Name, testCase.WantPresent, testCase.WantTeammate)
		}
	}
	return fixture
}

// TestClaudeTeammatesReadsOnlyRecordedSpawnEdges pins the one teammate rule: a
// session carries an identity only when it is a stored Claude Code child, its
// own root transcript names a team and an agent, and its stored Claude Code
// parent's root transcript records spawning that exact pair. Ordinary
// parentage, another harness's parentage, a parent that spawned somebody else,
// a child with no stored parent, and a subagent-scope cache row all carry
// nothing; an identity the local contract cannot carry fails the read rather
// than disappearing.
func TestClaudeTeammatesReadsOnlyRecordedSpawnEdges(t *testing.T) {
	for _, testCase := range loadClaudeTeammatesFixture(t).Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			db := storetest.Open(t)
			seedClaudeTeammateScenario(t, db, testCase)

			got, err := db.ClaudeTeammates(context.Background(), []string{testCase.Subject})
			if testCase.WantError != "" {
				assertClaudeTeammateRefused(t, testCase, got, err)
				return
			}
			if err != nil {
				t.Fatalf("ClaudeTeammates(%q): %v", testCase.Subject, err)
			}
			for id := range got {
				if id != testCase.Subject {
					t.Fatalf("ClaudeTeammates returned an identity for %q, which the caller never named", id)
				}
			}
			teammate, present := got[testCase.Subject]
			if present != testCase.WantPresent {
				t.Fatalf("teammate presence for %q = %t (%+v), want %t from the recorded spawn evidence", testCase.Subject, present, teammate, testCase.WantPresent)
			}
			if !testCase.WantPresent {
				return
			}
			want := claudeTeammateWant(t, testCase)
			if teammate != want {
				t.Fatalf("teammate for %q = %+v, want the recorded %+v", testCase.Subject, teammate, want)
			}
			if err := teammate.Validate(); err != nil {
				t.Fatalf("the store returned an identity the local read cannot carry: %v", err)
			}
		})
	}
}

// assertClaudeTeammateRefused proves the read fails closed on a stored identity
// the local contract cannot carry, names the field in the refusal, and returns
// no teammate for any session.
func assertClaudeTeammateRefused(t *testing.T, testCase claudeTeammateCase, got map[string]schema.SessionTeammate, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("ClaudeTeammates(%q) returned %+v without an error; a stored identity the local contract cannot carry must fail the read instead of being silently omitted", testCase.Subject, got)
	}
	if !strings.Contains(err.Error(), testCase.WantError) {
		t.Fatalf("ClaudeTeammates(%q) error = %v, want it to name %q so the corrupt cached identity is repairable", testCase.Subject, err, testCase.WantError)
	}
	if len(got) != 0 {
		t.Fatalf("ClaudeTeammates(%q) returned %+v beside its error; a refused read returns no teammates", testCase.Subject, got)
	}
}

func claudeTeammateWant(t *testing.T, testCase claudeTeammateCase) schema.SessionTeammate {
	t.Helper()
	if testCase.WantTeammate == nil {
		t.Fatalf("Claude teammate case %q expects a present identity but names none", testCase.Name)
	}
	return schema.SessionTeammate{
		Team:     testCase.WantTeammate.Team,
		Name:     testCase.WantTeammate.Name,
		Evidence: schema.SessionTeammateEvidenceNativeDynamicWorkflow,
	}
}

// TestClaudeTeammatesStaysBoundedToTheNamedPage proves the read answers only for
// the identifiers the page already projects, and that it reads every batch of a
// long page. The request puts a recorded spawn edge at its first and last
// positions, with full batches of unrelated sessions between them, so dropping
// either the first or a later batch loses a recorded edge instead of passing.
func TestClaudeTeammatesStaysBoundedToTheNamedPage(t *testing.T) {
	fixture := loadClaudeTeammatesFixture(t)
	byName := make(map[string]claudeTeammateCase, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		byName[testCase.Name] = testCase
	}
	probes := []struct {
		position string
		testCase claudeTeammateCase
	}{
		{position: "first", testCase: byName[fixture.BatchProbe.First]},
		{position: "last", testCase: byName[fixture.BatchProbe.Last]},
	}
	for _, probe := range probes {
		if !probe.testCase.WantPresent || probe.testCase.Subject == "" {
			t.Fatalf("batch-probe case %q must be a recorded spawn edge", probe.testCase.Name)
		}
	}
	if probes[0].testCase.Subject == probes[1].testCase.Subject {
		t.Fatalf("batch-probe cases %q and %q name the same session; the probe needs two distinct edges", probes[0].testCase.Name, probes[1].testCase.Name)
	}

	db := storetest.Open(t)
	seedClaudeTeammateScenario(t, db, probes[0].testCase)
	seedClaudeTeammateScenario(t, db, probes[1].testCase)

	// Derived from the production batch size: two full batches of unrelated
	// sessions sit between the two edges, so the page spans three batches.
	librarySize := 2 * store.ClaudeTeammateReadBatchSize
	entries := make([]ingest.StoreEntry, 0, librarySize)
	evidence := make([]ingest.ClaudeTranscriptEvidence, 0, librarySize)
	projectHash := schema.ProjectHash(fmt.Sprintf("%064d", 7))
	requested := make([]string, 0, librarySize+2)
	requested = append(requested, probes[0].testCase.Subject)
	for i := 0; i < librarySize; i++ {
		id := fmt.Sprintf("30000000-0000-4000-8000-%012d", i)
		sourcePath := fmt.Sprintf("/captured/library/%s.jsonl", id)
		entries = append(entries, claudeTeammateStoreEntry(t, id, schema.HarnessClaudeCode, sourcePath, nil, projectHash))
		evidence = append(evidence, claudeTeammateEvidence(t, "", sourcePath, &claudeTeammateIdentityFixture{Team: "unrelated", Name: id}, nil))
		requested = append(requested, id)
	}
	requested = append(requested, probes[1].testCase.Subject)
	if err := db.InsertSessions(context.Background(), entries); err != nil {
		t.Fatalf("seed unrelated library: %v", err)
	}
	if err := db.SaveClaudeEvidence(context.Background(), evidence, nil); err != nil {
		t.Fatalf("seed unrelated evidence: %v", err)
	}
	batches := (len(requested) + store.ClaudeTeammateReadBatchSize - 1) / store.ClaudeTeammateReadBatchSize
	if batches < 3 {
		t.Fatalf("the bounded-page probe covers %d batches of %d identifiers; it must span at least three so a skipped middle or last batch is observable", batches, store.ClaudeTeammateReadBatchSize)
	}

	got, err := db.ClaudeTeammates(context.Background(), requested)
	if err != nil {
		t.Fatalf("ClaudeTeammates across %d batches: %v", batches, err)
	}
	for _, probe := range probes {
		teammate, present := got[probe.testCase.Subject]
		if !present {
			t.Fatalf("the %s recorded spawn edge %q is missing from a %d-identifier page spanning %d batches of %d; a batch was skipped or the page was not read in full", probe.position, probe.testCase.Subject, len(requested), batches, store.ClaudeTeammateReadBatchSize)
		}
		if want := claudeTeammateWant(t, probe.testCase); teammate != want {
			t.Fatalf("the %s recorded spawn edge %q = %+v, want %+v", probe.position, probe.testCase.Subject, teammate, want)
		}
	}
	if len(got) != len(probes) {
		t.Fatalf("teammates across %d unrelated sessions = %+v, want exactly the two recorded spawn edges", librarySize, got)
	}
}

// seedClaudeTeammateScenario stores the case's sessions and the cached evidence
// each one records, the way a harvest leaves them.
func seedClaudeTeammateScenario(t *testing.T, db *store.Store, testCase claudeTeammateCase) {
	t.Helper()
	projectHash := schema.ProjectHash(fmt.Sprintf("%064d", 9))
	entries := make([]ingest.StoreEntry, 0, len(testCase.Sessions))
	evidence := make([]ingest.ClaudeTranscriptEvidence, 0, len(testCase.Sessions))
	for _, session := range testCase.Sessions {
		var harness schema.Harness
		if err := harness.UnmarshalText([]byte(session.Harness)); err != nil || !harness.IsKnown() {
			t.Fatalf("Claude teammate case %q names unknown harness %q: %v", testCase.Name, session.Harness, err)
		}
		var parent *ingest.SessionID
		if session.ParentID != "" {
			id, err := ingest.NewSessionID(session.ParentID)
			if err != nil {
				t.Fatalf("Claude teammate case %q has invalid parent id %q: %v", testCase.Name, session.ParentID, err)
			}
			parent = &id
		}
		entries = append(entries, claudeTeammateStoreEntry(t, session.ID, harness, session.SourcePath, parent, projectHash))
		evidence = append(evidence, claudeTeammateEvidence(t, session.Scope, session.SourcePath, session.Identity, session.Spawns))
	}
	if err := db.InsertSessions(context.Background(), entries); err != nil {
		t.Fatalf("seed Claude teammate sessions for %q: %v", testCase.Name, err)
	}
	if err := db.SaveClaudeEvidence(context.Background(), evidence, nil); err != nil {
		t.Fatalf("seed Claude teammate evidence for %q: %v", testCase.Name, err)
	}
}

func claudeTeammateStoreEntry(t *testing.T, sessionID string, harness schema.Harness, sourcePath string, parent *ingest.SessionID, projectHash schema.ProjectHash) ingest.StoreEntry {
	t.Helper()
	id, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("invalid seeded session id %q: %v", sessionID, err)
	}
	ingested := int64(1_700_000_100_000)
	return ingest.StoreEntry{Metadata: &schema.UnifiedMetadata{
		SchemaVersion: ingest.CurrentSchemaVersion,
		SessionID:     id,
		ModelHarness:  harness,
		Model:         schema.ModelID("claude-opus-4-6"),
		HostSlug:      schema.HostSlug("teammate-fixture-host"),
		Project: schema.ProjectContext{
			Hash:     projectHash,
			Name:     "teammate-fixture-project",
			FilePath: "/teammate-fixture",
		},
		Timestamp:  schema.TimestampInfo{Start: 1_700_000_000_000, End: 1_700_000_060_000, Ingested: &ingested},
		Source:     schema.SourceInfo{FilePath: sourcePath, Format: schema.SourceFormatJSONL},
		ParentUUID: parent,
		Stats:      schema.SessionStats{TurnCount: 1},
	}}
}

func claudeTeammateEvidence(t *testing.T, scope, sourcePath string, identity *claudeTeammateIdentityFixture, spawns []claudeTeammateIdentityFixture) ingest.ClaudeTranscriptEvidence {
	t.Helper()
	record := ingest.ClaudeTranscriptEvidence{
		SourcePath: ingest.ResolvedPath(sourcePath),
		Scope:      claudeTeammateEvidenceScope(t, scope),
		Records:    []ingest.ClaudeIdentityRecord{},
	}
	if identity != nil {
		record.Identity = &ingest.ClaudeTeammateIdentity{Team: identity.Team, Name: identity.Name}
	}
	for _, spawn := range spawns {
		record.Spawns = append(record.Spawns, ingest.ClaudeTeammateIdentity{Team: spawn.Team, Name: spawn.Name})
	}
	return record
}

func claudeTeammateEvidenceScope(t *testing.T, scope string) ingest.ClaudeEvidenceScope {
	t.Helper()
	switch scope {
	case "", "root":
		return ingest.ClaudeEvidenceScopeRoot
	case "subagent":
		return ingest.ClaudeEvidenceScopeSubagent
	default:
		t.Fatalf("Claude teammate fixture names unknown evidence scope %q; use root or subagent", scope)
		return ""
	}
}
