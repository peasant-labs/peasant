package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_evidence_cache.yaml
var claudeEvidenceCacheYAML []byte

const claudeEvidenceCacheRoot = "/claude"

type claudeEvidenceCacheFixtures struct {
	DeclaredRows int                       `yaml:"declared_rows"`
	Cases        []claudeEvidenceCacheCase `yaml:"cases"`
}

type claudeEvidenceCacheCase struct {
	Name                   string                      `yaml:"name"`
	Files                  []claudeEvidenceFile        `yaml:"files"`
	Expected               []claudeEvidenceExpectation `yaml:"expected"`
	Rewrite                []claudeEvidenceFile        `yaml:"rewrite"`
	Remove                 []string                    `yaml:"remove"`
	ExpectedSecondRunReads []string                    `yaml:"expected_second_run_reads"`
	ExpectedAfter          []claudeEvidenceExpectation `yaml:"expected_after"`
}

type claudeEvidenceFile struct {
	Path  string   `yaml:"path"`
	Lines []string `yaml:"lines"`
}

type claudeEvidenceExpectation struct {
	SessionID     string   `yaml:"session_id"`
	ParentUUID    string   `yaml:"parent_uuid"`
	Title         string   `yaml:"title"`
	Branch        string   `yaml:"branch"`
	CWD           string   `yaml:"cwd"`
	SubagentPaths []string `yaml:"subagent_paths"`
}

func loadClaudeEvidenceCacheFixtures(t *testing.T) claudeEvidenceCacheFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(claudeEvidenceCacheYAML))
	decoder.KnownFields(true)
	var fixtures claudeEvidenceCacheFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode Claude evidence cache fixtures: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("Claude evidence cache fixture must contain exactly one YAML document: %v", err)
	}
	const expectedRows = 4
	if fixtures.DeclaredRows != expectedRows || len(fixtures.Cases) != expectedRows {
		t.Fatalf("Claude evidence cache fixture row guard failed: declared=%d actual=%d expected=%d",
			fixtures.DeclaredRows, len(fixtures.Cases), expectedRows)
	}
	return fixtures
}

// openEvidenceStore opens a local store and closes it when the test ends.
func openEvidenceStore(t *testing.T, dbPath string) *store.Store {
	t.Helper()
	database, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open the local store: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close the local store: %v", err)
		}
	})
	return database
}

func writeClaudeEvidenceFiles(t *testing.T, fs *testutil.CountingFS, files []claudeEvidenceFile) {
	t.Helper()
	for _, file := range files {
		path := claudeEvidenceCacheRoot + "/" + file.Path
		body := strings.Join(file.Lines, "\n") + "\n"
		if err := fs.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write transcript fixture %q: %v", path, err)
		}
	}
}

// TestClaudeAdapter_EvidenceCacheSkipsUnchangedTranscripts runs discovery twice
// over the same corpus with the local store as the evidence cache. The first
// run mines every transcript. The second run must produce the same links and
// the same display hints, and it must read only the transcripts that changed.
func TestClaudeAdapter_EvidenceCacheSkipsUnchangedTranscripts(t *testing.T) {
	fixtures := loadClaudeEvidenceCacheFixtures(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := context.Background()
			database := openEvidenceStore(t, filepath.Join(t.TempDir(), "peasant.db"))

			fs := testutil.NewCountingFS(testutil.NewMemFS())
			writeClaudeEvidenceFiles(t, fs, fixture.Files)

			cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{claudeEvidenceCacheRoot}, Enabled: true}
			adapter := ingest.NewClaudeAdapter(fs, testutil.DefaultGitResolver(), salt.Salt{})
			ingest.AttachClaudeEvidenceCache(adapter, database)

			first, err := adapter.Discover(ctx, cfg)
			if err != nil {
				t.Fatalf("first discovery: %v", err)
			}
			assertClaudeDiscovery(t, "first run", first, fixture.Expected)
			if fs.TotalReads() == 0 {
				t.Fatal("first discovery read no transcript, so the cache measurement proves nothing")
			}

			// Apply the change the fixture describes, then measure the reads the
			// second discovery makes.
			writeClaudeEvidenceFiles(t, fs, fixture.Rewrite)
			for _, path := range fixture.Remove {
				if err := fs.Remove(claudeEvidenceCacheRoot + "/" + path); err != nil {
					t.Fatalf("remove transcript fixture %q: %v", path, err)
				}
			}
			fs.ResetCounts()

			second, err := adapter.Discover(ctx, cfg)
			if err != nil {
				t.Fatalf("second discovery: %v", err)
			}
			assertClaudeDiscovery(t, "second run", second, fixture.ExpectedAfter)
			assertClaudeReads(t, fs, fixture.ExpectedSecondRunReads)

			// The cache must also hold only the transcripts that still exist.
			records, err := database.LoadClaudeEvidence(ctx)
			if err != nil {
				t.Fatalf("load cached evidence: %v", err)
			}
			assertCachedPaths(t, records, fixture)
		})
	}
}

// assertClaudeDiscovery checks the linking and the display hints of one run.
func assertClaudeDiscovery(t *testing.T, label string, sessions []ingest.DiscoveredSession, expected []claudeEvidenceExpectation) {
	t.Helper()
	if len(sessions) != len(expected) {
		t.Fatalf("%s discovered %d sessions, want %d", label, len(sessions), len(expected))
	}
	byID := make(map[string]ingest.DiscoveredSession, len(sessions))
	for _, session := range sessions {
		byID[string(session.SessionID)] = session
	}
	for _, want := range expected {
		session, ok := byID[want.SessionID]
		if !ok {
			t.Fatalf("%s did not discover session %q", label, want.SessionID)
		}
		gotParent := ""
		if session.ParentUUID != nil {
			gotParent = string(*session.ParentUUID)
		}
		if gotParent != want.ParentUUID {
			t.Errorf("%s session %q parent = %q, want %q", label, want.SessionID, gotParent, want.ParentUUID)
		}
		if session.Title != want.Title {
			t.Errorf("%s session %q title = %q, want %q", label, want.SessionID, session.Title, want.Title)
		}
		if session.Branch != want.Branch {
			t.Errorf("%s session %q branch = %q, want %q", label, want.SessionID, session.Branch, want.Branch)
		}
		if session.CWD != want.CWD {
			t.Errorf("%s session %q working directory = %q, want %q", label, want.SessionID, session.CWD, want.CWD)
		}
		gotPaths := make([]string, len(session.SubagentPaths))
		for index, path := range session.SubagentPaths {
			gotPaths[index] = string(path)
		}
		if strings.Join(gotPaths, "\n") != strings.Join(want.SubagentPaths, "\n") {
			t.Errorf("%s session %q subagent paths = %q, want %q", label, want.SessionID, gotPaths, want.SubagentPaths)
		}
	}
}

// assertClaudeReads checks exactly which transcripts the second run read.
func assertClaudeReads(t *testing.T, fs *testutil.CountingFS, expected []string) {
	t.Helper()
	for _, path := range expected {
		if fs.ReadCount(path) == 0 {
			t.Errorf("second discovery did not read changed transcript %q", path)
		}
	}
	if got, want := fs.TotalReads(), len(expected); got != want {
		t.Errorf("second discovery read transcripts %d times, want %d", got, want)
	}
}

// assertCachedPaths checks that the cache holds one record per transcript that
// still exists, and no record for a transcript that is gone.
func assertCachedPaths(t *testing.T, records map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, fixture claudeEvidenceCacheCase) {
	t.Helper()
	removed := make(map[string]bool, len(fixture.Remove))
	for _, path := range fixture.Remove {
		removed[claudeEvidenceCacheRoot+"/"+path] = true
	}
	want := make([]string, 0, len(fixture.Files))
	for _, file := range fixture.Files {
		path := claudeEvidenceCacheRoot + "/" + file.Path
		if !removed[path] {
			want = append(want, path)
		}
	}
	got := make([]string, 0, len(records))
	for path := range records {
		got = append(got, path.String())
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("cached transcripts = %q, want %q", got, want)
	}
}
