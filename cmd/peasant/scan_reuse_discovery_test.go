package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/scan_reuse_discovery.yaml
var scanReuseDiscoveryYAML []byte

//go:embed testdata/scan_reuse_discovery.manifest.yaml
var scanReuseDiscoveryManifest []byte

type scanReuseDiscoveryCase struct {
	Name     string `yaml:"name"`
	State    string `yaml:"state"`
	SavesNew bool   `yaml:"saves_new_evidence"`
}

type scanReuseDiscoveryFixtures struct {
	Cases         []scanReuseDiscoveryCase `yaml:"cases"`
	CachedSession string                   `yaml:"cached_session"`
	NewSession    string                   `yaml:"new_session"`
	StaleSession  string                   `yaml:"stale_session"`
	CachedTitle   string                   `yaml:"cached_title"`
	Remote        string                   `yaml:"recorded_remote"`
	Branch        string                   `yaml:"recorded_branch"`
	Transcript    string                   `yaml:"transcript"`
}

func loadScanReuseDiscoveryFixtures(t *testing.T) scanReuseDiscoveryFixtures {
	t.Helper()
	var fixture scanReuseDiscoveryFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(scanReuseDiscoveryYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(scanReuseDiscoveryManifest, "scan reuse discovery")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "scan reuse discovery"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestScanReuseDiscoveryCache drives the same ftueDiscover entry point used by
// config and kickstart, with only synthetic transcripts and a private store.
// The behind case uses the existing v78 fixture, which restores the schema that
// shipped before v79; the store package separately proves frozen prefixes.
func TestScanReuseDiscoveryCache(t *testing.T) {
	fixture := loadScanReuseDiscoveryFixtures(t)
	var root string
	if os.Getenv("PEASANT_RETAIN_SCAN_REUSE_TESTDATA") != "" {
		var err error
		root, err = os.MkdirTemp("", "scan-reuse-discovery-")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("retained synthetic discovery data: %s", root)
	} else {
		root = t.TempDir()
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := filepath.Join(root, c.Name)
			projects := filepath.Join(dir, "projects")
			project := filepath.Join(projects, "-synthetic-project")
			if err := os.MkdirAll(project, 0700); err != nil {
				t.Fatal(err)
			}
			cachedPath := filepath.Join(project, fixture.CachedSession+".jsonl")
			newPath := filepath.Join(project, fixture.NewSession+".jsonl")
			stalePath := filepath.Join(project, fixture.StaleSession+".jsonl")
			if err := os.WriteFile(cachedPath, []byte(fixture.Transcript), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(newPath, []byte(fixture.Transcript), 0600); err != nil {
				t.Fatal(err)
			}
			settled := time.Now().Add(-2 * time.Hour)
			if err := os.Chtimes(cachedPath, settled, settled); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(cachedPath)
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.BaseConfig()
			for harness := range ingest.DefaultAdapterRegistry {
				source, ok := cfg.Sources.Provider(harness)
				if !ok {
					t.Fatalf("missing source %s", harness)
				}
				source.Enabled = false
				source.Paths = nil
				if harness == defaults.HarnessClaudeCode {
					source.Enabled = true
					source.Paths = []string{projects}
				}
			}
			configPath := filepath.Join(dir, "config.yaml")
			if err := config.SaveAtomic(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "store.db")
			if c.State == "behind" {
				storetest.BuildQuickV78Store(t, path)
				if store.CurrentSchemaVersion() <= 78 {
					t.Fatal("the v78 fixture is no longer behind the current store")
				}
			} else {
				db, err := store.Open(path, store.WithPoolSize(1))
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
			if err != nil {
				t.Fatal(err)
			}
			seed := func(query string, args ...any) {
				t.Helper()
				if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
					t.Fatalf("seed synthetic discovery store: %v", err)
				}
			}
			seed(`INSERT INTO host_slugs(opaque_id,host_slug,git_remote) VALUES('scan-host','scan-host',?)`, fixture.Remote)
			seed(`INSERT INTO projects(project_hash,canonical_cwd) VALUES('scan-project','/synthetic/project')`)
			seed(`INSERT INTO sessions(session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format,schema_version,git_branch,git_worktree) VALUES(?,'claude-code','synthetic-model','scan-host','scan-project',1,2,?,?, 'jsonl',?,?,'/synthetic/project')`, fixture.CachedSession, time.Now().Add(-time.Hour).UnixMilli(), cachedPath, int(ingest.CurrentSchemaVersion), fixture.Branch)
			seedEvidence := func(path string) {
				seed(`INSERT INTO claude_transcript_evidence(source_path,scope,mod_time_unix_nano,size_bytes,has_conversation,spawns_json,title,branch,cwd,origin,proof_version) VALUES(?,'root',?,?,1,'[]',?,'','','user',?)`, path, info.ModTime().UnixNano(), info.Size(), fixture.CachedTitle, ingest.ClaudeIdentityProofVersion)
			}
			seedEvidence(cachedPath)
			seedEvidence(stalePath)
			switch c.State {
			case "current", "behind":
			case "recount_pending":
				if err := sqlitex.ExecuteTransient(conn, `INSERT INTO pending_recounts(name) VALUES('turn_counts')`, nil); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unknown scan discovery state %q", c.State)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeHash := sha256.Sum256(before)
			inventory, listings, _ := ftueDiscover(t.Context(), configPath, path, nil)
			if inventory[defaults.HarnessClaudeCode].SessionCount != 2 {
				t.Fatalf("inventory=%+v listings=%+v", inventory[defaults.HarnessClaudeCode], listings)
			}
			if detail := inventory[defaults.HarnessClaudeCode].Detail; detail != "" {
				t.Fatalf("discovery reported an unexpected cache write failure: %s", detail)
			}
			byID := listingByID(listings)
			if byID[fixture.CachedSession].Title != fixture.CachedTitle {
				t.Fatalf("scan did not reuse cached title: %+v", byID[fixture.CachedSession])
			}
			if got := byID[fixture.CachedSession]; got.GitRemote != fixture.Remote || got.Branch != fixture.Branch {
				t.Fatalf("scan did not reuse the saved session's git data: %+v", got)
			}
			reader, err := store.OpenForScanReuse(path, store.ReadSurfaceClaudeEvidence)
			if err != nil {
				t.Fatal(err)
			}
			if c.State == "behind" && reader.SchemaVersion() != 78 {
				t.Fatalf("behind discovery changed schema version to %d", reader.SchemaVersion())
			}
			evidence, _, err := reader.LoadClaudeEvidenceCompletely(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if _, found := evidence[ingest.ResolvedPath(newPath)]; found != c.SavesNew {
				t.Fatalf("new evidence saved=%v, want %v", found, c.SavesNew)
			}
			if _, found := evidence[ingest.ResolvedPath(stalePath)]; found == c.SavesNew {
				t.Fatalf("stale evidence retained=%v, want %v", found, !c.SavesNew)
			}
			if !c.SavesNew {
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if sha256.Sum256(after) != beforeHash {
					t.Fatal("behind-store discovery changed the database")
				}
			}
		})
	}
}
