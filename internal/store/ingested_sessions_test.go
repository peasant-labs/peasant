package store

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	ingestedSessionWithMetrics    = "50000000-0000-0000-0000-000000000001"
	ingestedSessionWithoutMetrics = "50000000-0000-0000-0000-000000000002"

	allIngestedSessionsFixturePath      = "internal/store/testdata/reader/all_ingested_sessions.yaml"
	allIngestedSessionsFixtureCaseCount = 2
)

//go:embed testdata/reader/all_ingested_sessions.yaml
var allIngestedSessionsFixtureData []byte

type allIngestedSessionsFixtures struct {
	Cases []allIngestedSessionFixture `yaml:"cases"`
}

type allIngestedSessionFixture struct {
	Name          string           `yaml:"name"`
	SessionID     string           `yaml:"sessionId"`
	Harness       defaults.Harness `yaml:"harness"`
	ProjectHash   string           `yaml:"projectHash"`
	HostSlug      string           `yaml:"hostSlug"`
	StartMs       int64            `yaml:"startMs"`
	GitRemote     string           `yaml:"gitRemote"`
	Branch        string           `yaml:"branch"`
	GitWorktree   string           `yaml:"gitWorktree"`
	CanonicalCwd  string           `yaml:"canonicalCwd"`
	Title         string           `yaml:"title"`
	IngestedMs    int64            `yaml:"ingestedMs"`
	SchemaVersion int              `yaml:"schemaVersion"`
}

func loadAllIngestedSessionFixtures(data []byte) ([]allIngestedSessionFixture, error) {
	var fixtures allIngestedSessionsFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		return nil, fmt.Errorf("decode committed fixture %s: %w; fix the YAML schema or remove unknown fields", allIngestedSessionsFixturePath, err)
	}
	var trailing any
	switch err := decoder.Decode(&trailing); err {
	case io.EOF:
	case nil:
		return nil, fmt.Errorf("committed fixture %s contains a trailing YAML document; remove the extra document so the fixture contains exactly one YAML document", allIngestedSessionsFixturePath)
	default:
		return nil, fmt.Errorf("decode trailing YAML content in committed fixture %s: %w; remove or repair the trailing YAML document", allIngestedSessionsFixturePath, err)
	}
	if len(fixtures.Cases) != allIngestedSessionsFixtureCaseCount {
		return nil, fmt.Errorf("committed fixture %s defines %d cases, want exactly %d store read scenarios; add or remove cases and keep the row-count guard current", allIngestedSessionsFixturePath, len(fixtures.Cases), allIngestedSessionsFixtureCaseCount)
	}

	seenNames := make(map[string]struct{}, len(fixtures.Cases))
	seenSessionIDs := make(map[string]struct{}, len(fixtures.Cases))
	seenHarnesses := make(map[defaults.Harness]struct{}, len(fixtures.Cases))
	seenIngestedMs := make(map[int64]struct{}, len(fixtures.Cases))
	seenSchemaVersions := make(map[int]struct{}, len(fixtures.Cases))
	hasPopulatedColumns := false
	hasEmptyColumns := false
	for i, fixture := range fixtures.Cases {
		if fixture.Name == "" || fixture.SessionID == "" || fixture.Harness == "" || fixture.ProjectHash == "" || fixture.HostSlug == "" || fixture.StartMs <= 0 || fixture.IngestedMs <= 0 || fixture.SchemaVersion <= 0 {
			return nil, fmt.Errorf("committed fixture %s case %d is incomplete; populate its name, sessionId, harness, projectHash, hostSlug, positive startMs, positive ingestedMs, and positive schemaVersion", allIngestedSessionsFixturePath, i)
		}
		if !fixture.Harness.IsKnown() {
			return nil, fmt.Errorf("committed fixture %s case %q has unknown harness %q; use a canonical harness identifier", allIngestedSessionsFixturePath, fixture.Name, fixture.Harness)
		}
		if _, exists := seenNames[fixture.Name]; exists {
			return nil, fmt.Errorf("committed fixture %s repeats case name %q; use a unique name for each scenario", allIngestedSessionsFixturePath, fixture.Name)
		}
		seenNames[fixture.Name] = struct{}{}
		if _, exists := seenSessionIDs[fixture.SessionID]; exists {
			return nil, fmt.Errorf("committed fixture %s repeats sessionId %q; use a unique stored session for each scenario", allIngestedSessionsFixturePath, fixture.SessionID)
		}
		seenSessionIDs[fixture.SessionID] = struct{}{}
		seenHarnesses[fixture.Harness] = struct{}{}
		if _, exists := seenIngestedMs[fixture.IngestedMs]; exists {
			return nil, fmt.Errorf("committed fixture %s repeats ingestedMs %d; use a distinct timestamp for each row so exact timestamp readback is covered", allIngestedSessionsFixturePath, fixture.IngestedMs)
		}
		seenIngestedMs[fixture.IngestedMs] = struct{}{}
		if _, exists := seenSchemaVersions[fixture.SchemaVersion]; exists {
			return nil, fmt.Errorf("committed fixture %s repeats schemaVersion %d; use a distinct version for each row so exact schema readback is covered", allIngestedSessionsFixturePath, fixture.SchemaVersion)
		}
		seenSchemaVersions[fixture.SchemaVersion] = struct{}{}
		switch {
		case fixture.GitRemote != "" && fixture.Branch != "" && fixture.GitWorktree != "" && fixture.CanonicalCwd != "" && fixture.Title != "":
			hasPopulatedColumns = true
		case fixture.GitRemote == "" && fixture.Branch == "" && fixture.GitWorktree == "" && fixture.CanonicalCwd == "" && fixture.Title == "":
			hasEmptyColumns = true
		default:
			return nil, fmt.Errorf("committed fixture %s case %q must populate gitRemote, branch, gitWorktree, canonicalCwd, and title together or leave all five empty; keep the corpus focused on full and empty row behavior", allIngestedSessionsFixturePath, fixture.Name)
		}
	}
	if !hasPopulatedColumns || !hasEmptyColumns {
		return nil, fmt.Errorf("committed fixture %s must include one populated row and one empty-compatible row; keep both read behaviors covered", allIngestedSessionsFixturePath)
	}
	if len(seenHarnesses) != allIngestedSessionsFixtureCaseCount {
		return nil, fmt.Errorf("committed fixture %s defines %d distinct harnesses, want exactly %d; use a different harness per row so harness readback cannot pass with a fixed value", allIngestedSessionsFixturePath, len(seenHarnesses), allIngestedSessionsFixtureCaseCount)
	}
	return fixtures.Cases, nil
}

func TestAllIngestedSessionsReadsCompleteRows(t *testing.T) {
	t.Parallel()
	fixtures, err := loadAllIngestedSessionFixtures(allIngestedSessionsFixtureData)
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "ingested-identity.db"), WithPoolSize(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, fixture := range fixtures {
		entry := makeStoreEntry(t, fixture.SessionID, fixture.ProjectHash, fixture.HostSlug, fixture.Harness, fixture.StartMs, 0, 0)
		entry.Metadata.SchemaVersion = fixture.SchemaVersion
		entry.Metadata.Project.FilePath = fixture.CanonicalCwd
		ingestedMs := fixture.IngestedMs
		entry.Metadata.Timestamp.Ingested = &ingestedMs
		if fixture.GitRemote != "" {
			remote := fixture.GitRemote
			entry.Metadata.Git.Remote = &remote
		}
		if fixture.Branch != "" {
			branch := fixture.Branch
			entry.Metadata.Git.Branch = &branch
		}
		if fixture.GitWorktree != "" {
			worktree := fixture.GitWorktree
			entry.Metadata.Git.Worktree = &worktree
		}
		if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
			t.Fatalf("%s: InsertSessions: %v", fixture.Name, err)
		}
		if fixture.Title != "" {
			title := fixture.Title
			metrics := ingest.SessionMetrics{SessionID: entry.Metadata.SessionID}
			metrics.TitleGenerated = &title
			if err := s.SaveMetrics(ctx, &metrics); err != nil {
				t.Fatalf("%s: SaveMetrics: %v", fixture.Name, err)
			}
		}
	}

	rows, err := s.AllIngestedSessions(ctx)
	if err != nil {
		t.Fatalf("AllIngestedSessions: %v", err)
	}
	if len(rows) != len(fixtures) {
		t.Fatalf("AllIngestedSessions returned %d rows, want %d fixture rows", len(rows), len(fixtures))
	}
	byID := make(map[string]IngestedSessionRow, len(rows))
	for _, row := range rows {
		byID[row.SessionID] = row
	}
	for _, fixture := range fixtures {
		row, ok := byID[fixture.SessionID]
		if !ok {
			t.Errorf("%s: session %s is missing from AllIngestedSessions", fixture.Name, fixture.SessionID)
			continue
		}
		want := IngestedSessionRow{
			SessionID:     fixture.SessionID,
			Harness:       fixture.Harness.String(),
			GitRemote:     fixture.GitRemote,
			Branch:        fixture.Branch,
			GitWorktree:   fixture.GitWorktree,
			CanonicalCwd:  fixture.CanonicalCwd,
			Title:         fixture.Title,
			IngestedMs:    fixture.IngestedMs,
			SchemaVersion: fixture.SchemaVersion,
		}
		if row != want {
			t.Errorf("%s: AllIngestedSessions row = %+v, want %+v", fixture.Name, row, want)
		}
	}
}

// TestAllIngestedSessionsKeepsSessionsWithoutMetrics pins the join AllIngestedSessions
// depends on. Metrics are written beside a session and deleted BEFORE it (prune
// removes the metrics row, then the session row), so a session can outlive its
// metrics. Requiring a metrics row would drop such a session from the result,
// and a caller that reads this as "everything the store holds" would then treat
// an already-ingested session as new and pay to resolve it all over again.
func TestAllIngestedSessionsKeepsSessionsWithoutMetrics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "ingested.db"), WithPoolSize(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i, sessionID := range []string{ingestedSessionWithMetrics, ingestedSessionWithoutMetrics} {
		harness := defaults.HarnessClaudeCode
		if sessionID == ingestedSessionWithoutMetrics {
			harness = defaults.HarnessOpenCode
		}
		entry := makeStoreEntry(t, sessionID,
			// One project and host slug per session so neither row can mask the other.
			repeatHex(t, i+1), "github.com--acme--ingested", harness, int64(1000+i), 0, 0)
		if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
			t.Fatalf("InsertSessions(%s): %v", sessionID, err)
		}
	}

	// The ingest write path always creates the metrics row beside the session,
	// so reaching the state under test means taking that row back out.
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	err = sqlitex.ExecuteTransient(conn, "DELETE FROM session_metrics WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{ingestedSessionWithoutMetrics},
	})
	changed := conn.Changes()
	s.pool.Put(conn)
	if err != nil {
		t.Fatalf("delete metrics row: %v", err)
	}
	if changed != 1 {
		t.Fatalf("deleting the metrics row removed %d rows, want 1; the ingest write path no longer creates one, so this test no longer builds the state it claims", changed)
	}

	rows, err := s.AllIngestedSessions(ctx)
	if err != nil {
		t.Fatalf("AllIngestedSessions: %v", err)
	}
	byID := make(map[string]IngestedSessionRow, len(rows))
	for _, row := range rows {
		byID[row.SessionID] = row
	}

	if _, ok := byID[ingestedSessionWithMetrics]; !ok {
		t.Errorf("session %s with metrics is missing from the result", ingestedSessionWithMetrics)
	}
	row, ok := byID[ingestedSessionWithoutMetrics]
	if !ok {
		t.Fatalf("session %s was dropped because it has no metrics row; the store still holds it, so the read must still return it", ingestedSessionWithoutMetrics)
	}
	if row.Harness != defaults.HarnessOpenCode.String() {
		t.Errorf("harness = %q, want %q for a session with no metrics row", row.Harness, defaults.HarnessOpenCode)
	}
	if row.Title != "" {
		t.Errorf("title = %q, want empty for a session with no metrics row", row.Title)
	}
	if row.IngestedMs == 0 {
		t.Error("ingested timestamp is 0; the diff rule cannot classify a source against it")
	}
	if row.SchemaVersion != ingest.CurrentSchemaVersion {
		t.Errorf("schema version = %d, want %d", row.SchemaVersion, ingest.CurrentSchemaVersion)
	}
}

// repeatHex builds a distinct valid project hash for the nth seeded session.
func repeatHex(t *testing.T, n int) string {
	t.Helper()
	const width = 64
	digit := byte('0' + n)
	out := make([]byte, width)
	for i := range out {
		out[i] = digit
	}
	return string(out)
}
