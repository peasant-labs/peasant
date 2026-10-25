package store_test

import (
	"context"
	_ "embed"
	"fmt"
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
	ID         string                          `yaml:"id"`
	Harness    string                          `yaml:"harness"`
	SourcePath string                          `yaml:"source_path"`
	ParentID   string                          `yaml:"parent_id"`
	Identity   *claudeTeammateIdentityFixture  `yaml:"identity"`
	Spawns     []claudeTeammateIdentityFixture `yaml:"spawns"`
}

type claudeTeammateCase struct {
	Name         string                         `yaml:"name"`
	Subject      string                         `yaml:"subject"`
	Sessions     []claudeTeammateSessionFixture `yaml:"sessions"`
	WantPresent  bool                           `yaml:"want_present"`
	WantTeammate *claudeTeammateIdentityFixture `yaml:"want_teammate"`
}

type claudeTeammatesFixture struct {
	RequiredNames []string             `yaml:"requiredNames"`
	Cases         []claudeTeammateCase `yaml:"cases"`
}

func loadClaudeTeammatesFixture(t *testing.T) claudeTeammatesFixture {
	t.Helper()
	var fixture claudeTeammatesFixture
	if err := testutil.DecodeNamedFixtureYAML(claudeTeammatesYAML, &fixture); err != nil {
		t.Fatalf("load Claude teammate fixture: %v", err)
	}
	for _, testCase := range fixture.Cases {
		if testCase.Subject == "" || len(testCase.Sessions) == 0 {
			t.Fatalf("Claude teammate case %q must name a subject session and seed at least one session", testCase.Name)
		}
		if testCase.WantPresent != (testCase.WantTeammate != nil) {
			t.Fatalf("Claude teammate case %q declares want_present=%t with want_teammate=%v; a present case names the identity it expects and an absent case names none", testCase.Name, testCase.WantPresent, testCase.WantTeammate)
		}
	}
	return fixture
}

// TestClaudeTeammatesReadsOnlyRecordedSpawnEdges pins the one teammate rule: a
// session carries an identity only when it is a stored Claude Code child, its
// own transcript names a team and an agent, and its stored parent's transcript
// records spawning that exact pair. Ordinary parentage, another harness's
// parentage, a parent that spawned somebody else, and a child with no stored
// parent all carry nothing.
func TestClaudeTeammatesReadsOnlyRecordedSpawnEdges(t *testing.T) {
	for _, testCase := range loadClaudeTeammatesFixture(t).Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			db := storetest.Open(t)
			seedClaudeTeammateScenario(t, db, testCase)

			got, err := db.ClaudeTeammates(context.Background(), []string{testCase.Subject})
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
			want := schema.SessionTeammate{
				Team:     testCase.WantTeammate.Team,
				Name:     testCase.WantTeammate.Name,
				Evidence: schema.EvidenceNativeDynamicWorkflow,
			}
			if teammate != want {
				t.Fatalf("teammate for %q = %+v, want the recorded %+v", testCase.Subject, teammate, want)
			}
			if err := teammate.Validate(); err != nil {
				t.Fatalf("the store returned an identity the local read cannot carry: %v", err)
			}
		})
	}
}

// TestClaudeTeammatesStaysBoundedToTheNamedPage proves the read answers only for
// the identifiers the page already projects: a large stored library with no
// spawn edge contributes nothing, and a request that spans more than one batch
// still returns exactly the recorded edge.
func TestClaudeTeammatesStaysBoundedToTheNamedPage(t *testing.T) {
	const librarySize = 301
	base := loadClaudeTeammatesFixture(t).Cases[0]
	db := storetest.Open(t)
	seedClaudeTeammateScenario(t, db, base)

	requested := []string{base.Subject}
	entries := make([]ingest.StoreEntry, 0, librarySize)
	evidence := make([]ingest.ClaudeTranscriptEvidence, 0, librarySize)
	projectHash := schema.ProjectHash(fmt.Sprintf("%064d", 7))
	for i := 0; i < librarySize; i++ {
		id := fmt.Sprintf("30000000-0000-4000-8000-%012d", i)
		sourcePath := fmt.Sprintf("/captured/library/%s.jsonl", id)
		entries = append(entries, claudeTeammateStoreEntry(t, id, schema.HarnessClaudeCode, sourcePath, nil, projectHash))
		evidence = append(evidence, claudeTeammateEvidence(t, sourcePath, &claudeTeammateIdentityFixture{Team: "unrelated", Name: id}, nil))
		requested = append(requested, id)
	}
	if err := db.InsertSessions(context.Background(), entries); err != nil {
		t.Fatalf("seed unrelated library: %v", err)
	}
	if err := db.SaveClaudeEvidence(context.Background(), evidence, nil); err != nil {
		t.Fatalf("seed unrelated evidence: %v", err)
	}

	got, err := db.ClaudeTeammates(context.Background(), requested)
	if err != nil {
		t.Fatalf("ClaudeTeammates across batches: %v", err)
	}
	if len(got) != 1 || got[base.Subject].Team != base.WantTeammate.Team || got[base.Subject].Name != base.WantTeammate.Name {
		t.Fatalf("teammates across a %d-session library = %+v, want exactly the one recorded spawn edge", librarySize, got)
	}
}

// seedClaudeTeammateScenario stores the case's sessions and the cached root
// evidence each one records, the way a harvest leaves them.
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
		evidence = append(evidence, claudeTeammateEvidence(t, session.SourcePath, session.Identity, session.Spawns))
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

func claudeTeammateEvidence(t *testing.T, sourcePath string, identity *claudeTeammateIdentityFixture, spawns []claudeTeammateIdentityFixture) ingest.ClaudeTranscriptEvidence {
	t.Helper()
	record := ingest.ClaudeTranscriptEvidence{
		SourcePath: ingest.ResolvedPath(sourcePath),
		Scope:      ingest.ClaudeEvidenceScopeRoot,
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
