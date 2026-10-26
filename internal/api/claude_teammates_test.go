package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/claude_teammates.yaml
var claudeTeammateAPIFixtureYAML []byte

type claudeTeammateAPICase struct {
	Name            string   `yaml:"name"`
	ParentSessionID string   `yaml:"parent_session_id"`
	ChildSessionID  string   `yaml:"child_session_id"`
	RootSessionID   string   `yaml:"root_session_id"`
	Team            string   `yaml:"team"`
	Agent           string   `yaml:"agent"`
	ParentLines     []string `yaml:"parent_transcript"`
	ChildLines      []string `yaml:"child_transcript"`
	RootLines       []string `yaml:"root_transcript"`
}

type claudeTeammateAPIFixture struct {
	RequiredNames []string                `yaml:"requiredNames"`
	Cases         []claudeTeammateAPICase `yaml:"cases"`
}

func loadClaudeTeammateAPIFixture(t *testing.T) claudeTeammateAPIFixture {
	t.Helper()
	var fixture claudeTeammateAPIFixture
	if err := testutil.DecodeNamedFixtureYAML(claudeTeammateAPIFixtureYAML, &fixture); err != nil {
		t.Fatalf("load Claude teammate API fixture: %v", err)
	}
	for _, testCase := range fixture.Cases {
		if testCase.ParentSessionID == "" || testCase.ChildSessionID == "" || testCase.RootSessionID == "" {
			t.Fatalf("Claude teammate API case %q must name the parent, child, and unrelated root sessions", testCase.Name)
		}
		if len(testCase.ParentLines) == 0 || len(testCase.ChildLines) == 0 || len(testCase.RootLines) == 0 {
			t.Fatalf("Claude teammate API case %q must record a transcript for each session", testCase.Name)
		}
		if testCase.Team == "" || testCase.Agent == "" {
			t.Fatalf("Claude teammate API case %q must name the spawned team and agent", testCase.Name)
		}
	}
	return fixture
}

// TestClaudeTeammateIdentityReachesLocalReads harvests real Claude Code
// transcripts and drives the identity through every local read the feature
// names: the session summary projection, the grouped sync chooser's paired
// summaries, the bounded Home reads behind the home list rows, and the flat
// transcript detail read. The same scenario proves what must not change: the
// durable SessionDetailPayload never carries the identity, the recorded parent
// link still rides the summaries and the detail read, and the mined agent
// origin is still the producer's stored decision.
func TestClaudeTeammateIdentityReachesLocalReads(t *testing.T) {
	for _, testCase := range loadClaudeTeammateAPIFixture(t).Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			db := harvestClaudeTeammateScenario(t, testCase)
			ctx := context.Background()
			want := schema.SessionTeammate{
				Team:     testCase.Team,
				Name:     testCase.Agent,
				Evidence: schema.SessionTeammateEvidenceNativeDynamicWorkflow,
			}

			// 1. The session summary construction site both list paths share.
			provider := NewStoreDataProvider(db, sessionvisibility.Browsing())
			summaries, err := provider.SessionSummariesByID(ctx, []string{testCase.ChildSessionID, testCase.ParentSessionID, testCase.RootSessionID})
			if err != nil {
				t.Fatalf("SessionSummariesByID: %v", err)
			}
			byID := make(map[string]schema.SessionSummary, len(summaries))
			for _, summary := range summaries {
				byID[summary.ID] = summary
			}
			child, ok := byID[testCase.ChildSessionID]
			if !ok {
				t.Fatalf("summaries omitted the spawn-recorded child %q", testCase.ChildSessionID)
			}
			assertTeammate(t, "SessionSummary for "+testCase.ChildSessionID, child.Teammate, &want)
			if child.ParentSessionID == nil || *child.ParentSessionID != testCase.ParentSessionID {
				t.Fatalf("summary parentSessionId = %v, want the recorded parent link %q", child.ParentSessionID, testCase.ParentSessionID)
			}
			if child.SessionOrigin != schema.SessionOriginAgent {
				t.Fatalf("summary sessionOrigin = %q, want %q for a session whose transcript declares a teammate identity", child.SessionOrigin, schema.SessionOriginAgent)
			}
			for _, id := range []string{testCase.ParentSessionID, testCase.RootSessionID} {
				summary, present := byID[id]
				if !present {
					t.Fatalf("summaries omitted session %q, so its absent teammate cannot be asserted", id)
				}
				assertTeammate(t, "SessionSummary for "+id, summary.Teammate, nil)
			}

			// 2. The grouped sync chooser's paired SessionSummary/LocalSyncSummary.
			homes := newTestXDGHomes(t)
			syncHandler := homes.handler(db, config.BaseConfig())
			candidates, err := syncHandler.gatherGroupedSyncCandidates(ctx, GroupedFilters{Variant: GroupedRouteSync})
			if err != nil {
				t.Fatalf("gatherGroupedSyncCandidates: %v", err)
			}
			var syncChild, syncParent *GroupedCandidate
			for i := range candidates {
				switch candidates[i].StableID {
				case testCase.ChildSessionID:
					syncChild = &candidates[i]
				case testCase.ParentSessionID:
					syncParent = &candidates[i]
				}
			}
			if syncChild == nil || syncChild.Sync == nil {
				t.Fatalf("the sync chooser omitted the spawn-recorded child %q", testCase.ChildSessionID)
			}
			if syncParent == nil || syncParent.Sync == nil {
				t.Fatalf("the sync chooser omitted the parent %q", testCase.ParentSessionID)
			}
			assertTeammate(t, "grouped sync SessionSummary for "+testCase.ChildSessionID, syncChild.Summary.Teammate, &want)
			assertTeammate(t, "grouped sync LocalSyncSummary for "+testCase.ChildSessionID, syncChild.Sync.Teammate, &want)
			assertTeammate(t, "grouped sync LocalSyncSummary for "+testCase.ParentSessionID, syncParent.Sync.Teammate, nil)

			// 3. The bounded Home reads the home list renders: its root page and
			// the expanded parent's child page, whose rows carry row.sync.teammate.
			allowed := map[string]bool{testCase.ParentSessionID: true, testCase.ChildSessionID: true, testCase.RootSessionID: true}
			home := homeTestHandler(t, &boundedHomeReader{Store: db, allowed: allowed, reads: map[string][]string{}}, db, config.BaseConfig())
			rootPage := syncPageRequest(t, home, "view=home", http.StatusOK)
			homeParent, homeRoot := findHomeRow(rootPage.Items, testCase.ParentSessionID), findHomeRow(rootPage.Items, testCase.RootSessionID)
			if homeParent == nil || homeRoot == nil {
				t.Fatalf("the Home root page omitted a seeded root session: %+v", rootPage.Items)
			}
			if childOnRootPage := findHomeRow(rootPage.Items, testCase.ChildSessionID); childOnRootPage != nil {
				t.Fatalf("the Home root page listed the child %q as a root row", testCase.ChildSessionID)
			}
			assertTeammate(t, "Home LocalSyncSummary for "+testCase.ParentSessionID, homeParent.Sync.Teammate, nil)
			assertTeammate(t, "Home LocalSyncSummary for "+testCase.RootSessionID, homeRoot.Sync.Teammate, nil)

			childPage := syncPageRequest(t, home, "view=home&parentSessionId="+url.QueryEscape(testCase.ParentSessionID), http.StatusOK)
			homeChild := findHomeRow(childPage.Items, testCase.ChildSessionID)
			if homeChild == nil {
				t.Fatalf("the Home child page for parent %q omitted the child %q: %+v", testCase.ParentSessionID, testCase.ChildSessionID, childPage.Items)
			}
			assertTeammate(t, "Home LocalSyncSummary for "+testCase.ChildSessionID, homeChild.Sync.Teammate, &want)

			// 4. The flat transcript detail read: the teammate rides beside the
			// durable payload and never inside it.
			read, err := provider.DetailReadPayload(ctx, testCase.ChildSessionID)
			if err != nil {
				t.Fatalf("DetailReadPayload: %v", err)
			}
			assertTeammate(t, "SessionDetailReadPayload for "+testCase.ChildSessionID, read.Teammate, &want)
			if read.ParentSessionID == nil || string(*read.ParentSessionID) != testCase.ParentSessionID {
				t.Fatalf("detail parentSessionId = %v, want the recorded parent link %q", read.ParentSessionID, testCase.ParentSessionID)
			}
			durable, err := json.Marshal(read.SessionDetailPayload)
			if err != nil {
				t.Fatalf("marshal durable detail payload: %v", err)
			}
			if strings.Contains(string(durable), `"teammate"`) {
				t.Fatalf("SessionDetailPayload carried the local teammate evidence onto the durable payload: %s", durable)
			}
			local, err := json.Marshal(read)
			if err != nil {
				t.Fatalf("marshal local detail read: %v", err)
			}
			if !strings.Contains(string(local), `"teammate"`) {
				t.Fatalf("SessionDetailReadPayload omitted the recorded teammate identity: %s", local)
			}

			parentRead, err := provider.DetailReadPayload(ctx, testCase.ParentSessionID)
			if err != nil {
				t.Fatalf("DetailReadPayload(parent): %v", err)
			}
			assertTeammate(t, "SessionDetailReadPayload for "+testCase.ParentSessionID, parentRead.Teammate, nil)

			// 5. The flat legacy sync route is deliberately unchanged: a
			// child row keeps its parent marker and never gains a teammate
			// key. The grouped and Home reads are the only sync surfaces that
			// carry it.
			flat := httptest.NewRecorder()
			serveSyncSessions(flat, httptest.NewRequest(http.MethodGet, defaults.RouteSyncSessions.String(), nil), db)
			if flat.Code != http.StatusOK {
				t.Fatalf("flat sync route status = %d (body %s)", flat.Code, flat.Body.String())
			}
			var flatEnvelope struct {
				Sessions []map[string]json.RawMessage `json:"sessions"`
			}
			if err := json.Unmarshal(flat.Body.Bytes(), &flatEnvelope); err != nil {
				t.Fatalf("decode flat sync response: %v", err)
			}
			var flatChild map[string]json.RawMessage
			for _, row := range flatEnvelope.Sessions {
				var id string
				if err := json.Unmarshal(row["id"], &id); err != nil {
					t.Fatalf("decode flat sync row id: %v", err)
				}
				if id == testCase.ChildSessionID {
					flatChild = row
					break
				}
			}
			if flatChild == nil {
				t.Fatalf("the flat sync route omitted the stored child %q", testCase.ChildSessionID)
			}
			if _, carried := flatChild["teammate"]; carried {
				t.Fatalf("the flat legacy sync row for %q carries a teammate key; only the grouped and Home reads expose the recorded identity", testCase.ChildSessionID)
			}
			if _, carried := flatChild["parentSessionId"]; !carried {
				t.Fatalf("the flat legacy sync row for %q lost its parent marker; the teammate change must not disturb it", testCase.ChildSessionID)
			}
		})
	}
}

// findHomeRow returns the page row for one session id, or nil when the page
// withheld it.
func findHomeRow(items []schema.LocalSyncSessionRow, sessionID string) *schema.LocalSyncSessionRow {
	for i := range items {
		if items[i].Sync.ID == sessionID {
			return &items[i]
		}
	}
	return nil
}

func assertTeammate(t *testing.T, where string, got, want *schema.SessionTeammate) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("%s carries teammate %+v; the recorded spawn evidence names none for it", where, *got)
		}
		return
	}
	if got == nil {
		t.Fatalf("%s carries no teammate; the parent transcript records spawning %q/%q", where, want.Team, want.Name)
	}
	if *got != *want {
		t.Fatalf("%s teammate = %+v, want %+v", where, got, want)
	}
}

// harvestClaudeTeammateScenario records the case's transcripts and harvests them
// through the production pipeline, then opens the resulting store with its
// managed generations. Discovery mines the identities and the spawn edges and
// links the child to its spawner, exactly as the local app does.
func harvestClaudeTeammateScenario(t *testing.T, testCase claudeTeammateAPICase) *store.Store {
	t.Helper()
	homes := newTestXDGHomes(t)
	root := t.TempDir()
	cfg := config.BaseConfig()
	cfg.Output.BasePath = filepath.Join(root, "managed")
	for harness := range ingest.DefaultAdapterRegistry {
		if provider, ok := cfg.Sources.Provider(harness); ok {
			provider.Enabled = harness == ingest.HarnessClaudeCode
		}
	}
	source, _ := cfg.Sources.Provider(ingest.HarnessClaudeCode)
	recordings := filepath.Join(root, "recordings")
	source.Paths = []string{recordings}
	project := filepath.Join(recordings, "-workspace")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	transcripts := map[string][]string{
		testCase.ParentSessionID: testCase.ParentLines,
		testCase.ChildSessionID:  testCase.ChildLines,
		testCase.RootSessionID:   testCase.RootLines,
	}
	tokens := map[string]string{
		"PARENT_SESSION_ID": testCase.ParentSessionID,
		"CHILD_SESSION_ID":  testCase.ChildSessionID,
		"ROOT_SESSION_ID":   testCase.RootSessionID,
	}
	for id, lines := range transcripts {
		content := strings.Join(lines, "\n") + "\n"
		for token, sessionID := range tokens {
			content = strings.ReplaceAll(content, token, sessionID)
		}
		if err := os.WriteFile(filepath.Join(project, id+".jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	harvestLikeTheCLI(t, homes.dbPath(), cfg)
	storeOptions, err := store.ManagedGenerationOptions(cfg.Output.BasePath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(homes.dbPath(), append(storeOptions, store.WithSkipMigrations())...)
	if err != nil {
		t.Fatalf("open harvested store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	row, err := db.SessionByID(context.Background(), testCase.ChildSessionID)
	if err != nil {
		t.Fatalf("read the harvested child %q: %v", testCase.ChildSessionID, err)
	}
	if row == nil {
		t.Fatalf("harvest did not store the child %q", testCase.ChildSessionID)
	}
	if row.SessionOrigin != sessionorigin.Agent.String() {
		t.Fatalf("harvested child %q origin = %q, want the mined %q", testCase.ChildSessionID, row.SessionOrigin, sessionorigin.Agent)
	}
	return db
}
