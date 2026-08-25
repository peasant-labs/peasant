package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	kit "github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type demoRepoResolver struct{}

func (demoRepoResolver) ResolveRepositoryIdentity(_ context.Context, dir ingest.ClonePath) (ingest.RepositoryIdentity, error) {
	return ingest.RepositoryIdentity{
		CohortKey:    ingest.RepositoryCohortKey(dir.String()),
		GitDirectory: ingest.RepositoryPath(dir.String()),
	}, nil
}

const (
	demoAgentRoot   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	demoUserRoot    = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	demoParentRoot  = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	demoChildOne    = "agent-abc123"
	demoChildTwo    = "agent-def456"
	demoProjectSlug = "-demo-project"
)

// TestTwoRunKickstartDemonstration is the acceptance demonstration, not a unit
// test: run one works from a cache holding ONLY pre-origin records whose size
// and modification time still match their files, run two works from the cache
// run one wrote, and the two runs must agree on every visible row while
// disagreeing on exactly one thing -- how much they had to mine.
func TestTwoRunKickstartDemonstration(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	workDir := filepath.Join(dataDir, "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("create project directory: %v", err)
	}
	transcriptRoot := filepath.Join(dataDir, string(defaults.HarnessClaudeCode), "projects")
	slugDir := filepath.Join(transcriptRoot, demoProjectSlug)
	if err := os.MkdirAll(filepath.Join(slugDir, demoParentRoot, "subagents"), 0o755); err != nil {
		t.Fatalf("create transcript tree: %v", err)
	}

	line := func(id, extra, content string) string {
		return fmt.Sprintf(`{"sessionId":%q,"type":"user","cwd":%q%s,"message":{"role":"user","content":%q}}`+"\n",
			id, workDir, extra, content)
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	// An agent-driven root: structured teammate identity.
	write(filepath.Join(slugDir, demoAgentRoot+".jsonl"),
		line(demoAgentRoot, `,"teamName":"demo","agentName":"worker"`, "take the failing slice"))
	// A person's root: a slash command the harness wrapped.
	write(filepath.Join(slugDir, demoUserRoot+".jsonl"),
		line(demoUserRoot, "", "<command-name>/share</command-name>"))
	// A root with no evidence either way, carrying two subagent children.
	write(filepath.Join(slugDir, demoParentRoot+".jsonl"),
		line(demoParentRoot, "", "please review the change"))
	for _, child := range []string{demoChildOne, demoChildTwo} {
		write(filepath.Join(slugDir, demoParentRoot, "subagents", child+".jsonl"),
			line(child, "", "child work"))
	}

	dbPath := filepath.Join(dataDir, "peasant.db")
	database, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open the isolated store: %v", err)
	}
	defer func() { _ = database.Close() }()

	filesystem := &ingest.OSFileSystem{}
	git := &testutil.StubGitResolver{Remote: "git@example.com:demo/project.git", BranchName: "main"}

	cfg := config.BaseConfig()
	for harness := range ingest.DefaultAdapterRegistry {
		src, ok := cfg.Sources.Provider(harness)
		if !ok {
			t.Fatalf("registered harness %q has no source configuration", harness)
		}
		if harness == defaults.HarnessClaudeCode {
			src.Enabled = true
			src.Paths = []string{transcriptRoot}
			continue
		}
		src.Paths = nil
	}

	// Seed a cache that predates the origin field: mine once through the
	// production adapter, then blank the origin column exactly as an upgraded
	// store would look. Size and modification time still match their files, so
	// the empty origin is the ONLY reason a record can be judged stale.
	seed := ingest.DefaultAdapterRegistry[defaults.HarnessClaudeCode](filesystem, git, salt.Salt{})
	ingest.AttachClaudeEvidenceCache(seed, database)
	if _, err := seed.Discover(ctx, mustSource(t, cfg)); err != nil {
		t.Fatalf("seed discovery: %v", err)
	}
	blankStoredOrigins(t, dbPath)

	type observation struct {
		remined int
		hidden  []string
		badges  map[string]string
		rows    []string
	}
	observe := func(label string) observation {
		adapter := ingest.DefaultAdapterRegistry[defaults.HarnessClaudeCode](filesystem, git, salt.Salt{})
		ingest.AttachClaudeEvidenceCache(adapter, database)
		discovered, err := adapter.Discover(ctx, mustSource(t, cfg))
		if err != nil {
			t.Fatalf("%s discovery: %v", label, err)
		}
		stats, ok := adapter.(ingest.DiscoveryStatistics)
		if !ok {
			t.Fatalf("%s: the Claude adapter does not report discovery statistics, so the re-mine claim is unfalsifiable", label)
		}
		var hidden []string
		for _, session := range discovered {
			if session.ParentUUID == nil && session.Origin == sessionorigin.Agent {
				hidden = append(hidden, string(session.SessionID))
			}
		}
		sort.Strings(hidden)

		_, listings, subagents := ftueDiscoverWith(ctx, cfg, filesystem, git, nil, database, nil)
		// The PRODUCTION construction, not a hand-assembled one: the guided flow
		// builds its picker through exactly this function, so an option left
		// unwired there fails this demonstration instead of passing unnoticed.
		// Only the repository resolver is substituted, to keep the fixture
		// deterministic.
		source := newKickstartScannerSource(
			listings,
			subagents,
			ingest.NewPhysicalPathResolver(),
			demoRepoResolver{},
			nil,
		)
		roots, err := source.Load(ctx)
		if err != nil {
			t.Fatalf("%s load the kickstart forest: %v", label, err)
		}
		badges := map[string]string{}
		var rows []string
		var walk func(nodes []*kit.TreeNode)
		walk = func(nodes []*kit.TreeNode) {
			for _, node := range nodes {
				if node.Meta != nil {
					if _, isSession := node.Meta[settings.MetaHarness]; isSession {
						rows = append(rows, node.ID)
						badges[node.ID] = node.Meta[settings.MetaChildCount]
					}
				}
				walk(node.Children)
			}
		}
		walk(roots)
		sort.Strings(rows)
		return observation{remined: stats.ReminedCount(), hidden: hidden, badges: badges, rows: rows}
	}

	one := observe("run one")
	two := observe("run two")

	t.Logf("run one: remined=%d hidden=%v rows=%v badges=%v", one.remined, one.hidden, one.rows, one.badges)
	t.Logf("run two: remined=%d hidden=%v rows=%v badges=%v", two.remined, two.hidden, two.rows, two.badges)

	if one.remined <= 0 {
		t.Errorf("run one re-mined %d records, want above zero over a cache of pre-origin records", one.remined)
	}
	if two.remined != 0 {
		t.Errorf("run two re-mined %d records, want exactly zero over the cache run one wrote", two.remined)
	}
	if !reflect.DeepEqual(one.hidden, two.hidden) {
		t.Errorf("the runs hid different agent roots: %v vs %v", one.hidden, two.hidden)
	}
	if len(one.hidden) == 0 {
		t.Error("no agent root was hidden, so the comparison proves nothing")
	}
	if !reflect.DeepEqual(one.rows, two.rows) {
		t.Errorf("the runs listed different rows: %v vs %v", one.rows, two.rows)
	}
	for _, id := range one.rows {
		if one.badges[id] != two.badges[id] {
			t.Errorf("row %s child badge differs: %q vs %q", id, one.badges[id], two.badges[id])
		}
	}
	for _, id := range one.rows {
		if id == demoAgentRoot {
			t.Errorf("the agent-driven root %s is still listed", id)
		}
	}
	// The badge is asserted against a KNOWN value, not only compared between the
	// runs. Agreement alone would hold even if every badge were empty, which is
	// what this path produced while the listing cohort was the only thing the
	// count could be resolved from. The parent below has exactly two subagent
	// sessions on disk, and the discovered subagent relation now carries them,
	// so the count the row renders is a real number both runs must reach.
	const wantParentBadge = "2"
	for _, run := range []struct {
		label string
		got   observation
	}{{"run one", one}, {"run two", two}} {
		if got := run.got.badges[demoParentRoot]; got != wantParentBadge {
			t.Errorf("%s: parent row %s child badge = %q, want %q from its two discovered subagent sessions",
				run.label, demoParentRoot, got, wantParentBadge)
		}
	}
	// The children are counted, and the listed rows are pinned to a known set
	// rather than only compared between the runs. This is what keeps the
	// count-only promise honest: the two subagent sessions contribute to the
	// badge above and must still be absent from the rows a user can select and
	// import, and no other session may appear either.
	wantRows := []string{demoUserRoot, demoParentRoot}
	sort.Strings(wantRows)
	if !reflect.DeepEqual(one.rows, wantRows) {
		t.Errorf("the listed rows are %v, want exactly %v: the user root and the parent root, and no subagent session", one.rows, wantRows)
	}
}

func mustSource(t *testing.T, cfg *config.Config) ingest.SourceConfig {
	t.Helper()
	src, _, ok := resolveConfiguredSource(cfg, defaults.HarnessClaudeCode)
	if !ok {
		t.Fatal("the Claude source is not configured")
	}
	src.Enabled = true
	return src
}

// blankStoredOrigins rewrites every cached evidence record to the empty
// "predates the origin field" marker, leaving size and modification time
// untouched. This is what an upgraded store looks like before its first run.
func blankStoredOrigins(t *testing.T, dbPath string) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open the store for the pre-origin seed: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := sqlitex.ExecuteTransient(conn, `UPDATE claude_transcript_evidence SET origin = ''`, nil); err != nil {
		t.Fatalf("blank the cached origins: %v", err)
	}
	rows := conn.Changes()
	if rows == 0 {
		t.Fatal("the seed discovery cached no evidence record, so there is no pre-origin cache to demonstrate against")
	}
	t.Logf("seeded %d pre-origin cache records (empty origin, size and mod time unchanged)", rows)
}
