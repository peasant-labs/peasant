package store

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/scan_reuse_floor.yaml
var scanReuseYAML []byte

//go:embed testdata/scan_reuse_floor.manifest.yaml
var scanReuseManifest []byte

// scanReuseCaseKind names the behavior each synthetic scan-reuse case exercises.
// "unknown" and "empty" here are case kinds, not a coordinate kind or an
// OpenCode schema kind.
type scanReuseCaseKind string

const (
	scanReuseEquivalent      scanReuseCaseKind = "equivalent"
	scanReuseBelowFloor      scanReuseCaseKind = "below_floor"
	scanReuseNewer           scanReuseCaseKind = "newer"
	scanReuseReadOnly        scanReuseCaseKind = "read_only"
	scanReuseUnrequested     scanReuseCaseKind = "unrequested"
	scanReuseUnknown         scanReuseCaseKind = "unknown"
	scanReuseEmpty           scanReuseCaseKind = "empty"
	scanReuseSurfaceSet      scanReuseCaseKind = "surface_set"
	scanReuseWriteGate       scanReuseCaseKind = "write_gate"
	scanReuseSnapshot        scanReuseCaseKind = "snapshot"
	scanReuseEndedSnapshot   scanReuseCaseKind = "ended_snapshot"
	scanReuseConcurrentClose scanReuseCaseKind = "concurrent_close"
)

type scanReuseCase struct {
	Name    string            `yaml:"name"`
	Kind    scanReuseCaseKind `yaml:"kind"`
	Surface string            `yaml:"surface"`
}

type scanReuseSession struct {
	ID            string `yaml:"id"`
	Harness       string `yaml:"harness"`
	LegacyHarness string `yaml:"legacy_harness"`
	Branch        string `yaml:"branch"`
	Worktree      string `yaml:"worktree"`
	Title         string `yaml:"title"`
	HasMetrics    bool   `yaml:"has_metrics"`
}

type scanReuseEvidence struct {
	Path      string                     `yaml:"path"`
	Scope     ingest.ClaudeEvidenceScope `yaml:"scope"`
	Title     string                     `yaml:"title"`
	Origin    string                     `yaml:"origin"`
	SessionID string                     `yaml:"session_id"`
	SpawnID   string                     `yaml:"spawn_id"`
}

type scanReuseFixtures struct {
	Cases        []scanReuseCase     `yaml:"cases"`
	SurfaceNames []string            `yaml:"surface_names"`
	Sessions     []scanReuseSession  `yaml:"known_sessions"`
	Evidence     []scanReuseEvidence `yaml:"evidence"`
}

func loadScanReuseFixtures(t *testing.T) scanReuseFixtures {
	t.Helper()
	var fixture scanReuseFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(scanReuseYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	required, err := decodeRecoveryRequiredNames(scanReuseManifest)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(required, names, "scan reuse"); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Sessions) == 0 || len(fixture.Evidence) == 0 {
		t.Fatal("scan reuse fixture must seed both read surfaces")
	}
	return fixture
}

// scanReuseTestDir can retain the synthetic databases for an isolated local
// investigation. Normal test runs use the testing package's owned temp dir.
func scanReuseTestDir(t *testing.T) string {
	t.Helper()
	if os.Getenv("PEASANT_RETAIN_SCAN_REUSE_TESTDATA") == "" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("", "scan-reuse-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained synthetic scan reuse data: %s", dir)
	return dir
}

func TestScanReuseAfterUpgradeSelectsHeadSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	buildBehindStore(t, path, knownSessionsReuseFloor, nil)
	if _, err := Upgrade(t.Context(), path, UpgradeOptions{}); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	reader, err := OpenForScanReuse(path, ReadSurfaceKnownSessions, ReadSurfaceClaudeEvidence)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer reader.Close()
	if reader.SchemaVersion() != CurrentSchemaVersion() || !reader.IsCurrent() {
		t.Fatalf("upgraded snapshot version=%d current=%v", reader.SchemaVersion(), reader.IsCurrent())
	}
	if _, err := reader.AllIngestedSessions(t.Context()); err != nil {
		t.Fatalf("read upgraded sessions: %v", err)
	}
	if _, _, err := reader.LoadClaudeEvidenceCompletely(t.Context()); err != nil {
		t.Fatalf("read upgraded evidence: %v", err)
	}
}

func scanReuseExec(t *testing.T, conn *sqlite.Conn, query string, args ...any) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("seed/query %s: %v", query, err)
	}
}

func seedScanReuseSessions(t *testing.T, conn *sqlite.Conn, version int, fixture scanReuseFixtures) {
	t.Helper()
	scanReuseExec(t, conn, `INSERT OR IGNORE INTO host_slugs(opaque_id,host_slug,git_remote) VALUES('scan-host','scan-host','https://example.invalid/synthetic/repo')`)
	scanReuseExec(t, conn, `INSERT OR IGNORE INTO projects(project_hash,canonical_cwd) VALUES('scan-project','/synthetic/project')`)
	if version < migrationV33TargetVersion {
		// Frozen V1's current CREATE text is newer than the shipped predecessor's
		// harness CHECK. The legacy row exercises V33's real rename on lowering
		// the floor; it must never occur in a post-V33 seed.
		scanReuseExec(t, conn, `PRAGMA ignore_check_constraints = ON`)
	}
	for i, row := range fixture.Sessions {
		harness := row.Harness
		if version < migrationV33TargetVersion && row.LegacyHarness != "" {
			harness = row.LegacyHarness
		}
		scanReuseExec(t, conn, `INSERT INTO sessions(session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format,schema_version,git_branch,git_worktree) VALUES(?,?, 'synthetic-model','scan-host','scan-project',?,?,?,?,'jsonl',11,?,?)`, row.ID, harness, i+1, i+2, i+3, "/synthetic/"+row.ID, row.Branch, row.Worktree)
		if row.HasMetrics {
			scanReuseExec(t, conn, `INSERT INTO session_metrics(session_id,title) VALUES(?,?)`, row.ID, row.Title)
		}
	}
	if version < migrationV33TargetVersion {
		scanReuseExec(t, conn, `PRAGMA ignore_check_constraints = OFF`)
	}
}

func seedScanReuseEvidence(t *testing.T, conn *sqlite.Conn, version int, fixture scanReuseFixtures) {
	t.Helper()
	for _, row := range fixture.Evidence {
		scanReuseExec(t, conn, `INSERT INTO claude_transcript_evidence(source_path,scope,mod_time_unix_nano,size_bytes,has_conversation,spawns_json,title,branch,cwd,origin,proof_version,native_agent_id,native_agent_valid,content_digest,identity_records_json) VALUES(?,?,12,34,1,'[]',?,'synthetic-branch','/synthetic/project',?,5,'worker',1,'synthetic-digest',?)`, row.Path, row.Scope.String(), row.Title, row.Origin, `[{"sessionId":"`+row.SessionID+`"}]`)
		if version >= 72 {
			ids, _ := json.Marshal([]string{row.SessionID})
			spawnIDs, _ := json.Marshal([]string{row.SpawnID})
			chain := "[]"
			if row.Scope == ingest.ClaudeEvidenceScopeRoot {
				chain = `[{"sessionId":"` + row.SessionID + `","uuid":"synthetic-uuid","parentUuid":"","type":"user","subtype":""}]`
			}
			scanReuseExec(t, conn, `UPDATE claude_transcript_evidence SET identity_session_ids_json=?,identity_spawn_agent_ids_json=?,identity_legacy_agent_proof=1,identity_chain_json=? WHERE source_path=?`, string(ids), string(spawnIDs), chain, row.Path)
		}
	}
}

type scanEvidenceRead struct {
	All      map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence
	ByPath   map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence
	Complete bool
	Chains   map[ingest.ResolvedPath][]ingest.ClaudeIdentityChainLink
	Unread   []ingest.ResolvedPath
}

func readScanSurface(t *testing.T, handle *ScanReuse, surface ReadSurface, fixture scanReuseFixtures) any {
	t.Helper()
	if surface == ReadSurfaceKnownSessions {
		rows, err := handle.AllIngestedSessions(t.Context())
		if err != nil {
			t.Fatalf("read known sessions: %v", err)
		}
		byID := make(map[string]IngestedSessionRow, len(rows))
		for _, row := range rows {
			byID[row.SessionID] = row
		}
		rows = nil
		for _, expected := range fixture.Sessions {
			row, ok := byID[expected.ID]
			if !ok ||
				row.Title != expected.Title ||
				row.Branch != expected.Branch ||
				row.GitWorktree != expected.Worktree ||
				row.CanonicalCwd != "/synthetic/project" ||
				row.GitRemote != "https://example.invalid/synthetic/repo" {
				t.Fatalf("known-session seed %s not read correctly: %+v", expected.ID, row)
			}
			rows = append(rows, row)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].SessionID < rows[j].SessionID })
		return rows
	}
	all, complete, err := handle.LoadClaudeEvidenceCompletely(t.Context())
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	var paths []ingest.ResolvedPath
	for _, row := range fixture.Evidence {
		paths = append(paths, ingest.ResolvedPath(row.Path))
	}
	byPath, byPathComplete, err := handle.LoadClaudeEvidenceForCompletely(t.Context(), paths)
	if err != nil || !byPathComplete {
		t.Fatalf("read evidence by path: complete=%v err=%v", byPathComplete, err)
	}
	chains, unread, err := handle.LoadClaudeIdentityChains(t.Context(), paths)
	if err != nil {
		t.Fatalf("read identity chains: %v", err)
	}
	if !complete || len(byPath) != len(fixture.Evidence) || len(unread) != 0 {
		t.Fatalf("evidence seed was not fully read: all=%d byPath=%d complete=%v unread=%v", len(all), len(byPath), complete, unread)
	}
	selected := make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, len(fixture.Evidence))
	for _, expected := range fixture.Evidence {
		path := ingest.ResolvedPath(expected.Path)
		row, ok := all[path]
		if !ok ||
			row.Title != expected.Title ||
			row.Scope != expected.Scope ||
			row.Origin.String() != expected.Origin ||
			row.ProofVersion != 5 ||
			!reflect.DeepEqual(row.SessionIDs, []string{expected.SessionID}) ||
			!reflect.DeepEqual(row.SpawnAgentIDs, []string{expected.SpawnID}) {
			t.Fatalf("evidence seed %s not read correctly: %+v", path, row)
		}
		if !reflect.DeepEqual(row, byPath[path]) {
			t.Fatalf("by-path evidence for %s differs from the whole-table read", path)
		}
		selected[path] = row
		chain, ok := chains[path]
		rootChainWrong := expected.Scope == ingest.ClaudeEvidenceScopeRoot && (len(chain) != 1 || chain[0].SessionID != expected.SessionID)
		childChainWrong := expected.Scope == ingest.ClaudeEvidenceScopeSubagent && len(chain) != 0
		if !ok || rootChainWrong || childChainWrong {
			t.Fatalf("identity chain for %s not read correctly: %+v", path, chain)
		}
	}
	return scanEvidenceRead{All: selected, ByPath: byPath, Complete: complete, Chains: chains, Unread: unread}
}

// versionedScanReuseFixture gives each version its own synthetic rows. They all
// travel through the same production migration steps to the head, so the test
// reads rows seeded at every version without replaying the whole chain N times.
func versionedScanReuseFixture(fixture scanReuseFixtures, version int) scanReuseFixtures {
	out := fixture
	out.Sessions = append([]scanReuseSession(nil), fixture.Sessions...)
	for i := range out.Sessions {
		out.Sessions[i].ID = fmt.Sprintf("%s-v%d", out.Sessions[i].ID, version)
	}
	out.Evidence = append([]scanReuseEvidence(nil), fixture.Evidence...)
	for i := range out.Evidence {
		out.Evidence[i].Path = fmt.Sprintf("/synthetic/version-%d/%s", version, filepath.Base(out.Evidence[i].Path))
	}
	return out
}

func assertScanReuseEquivalence(t *testing.T, dir string, surface ReadSurface, fixture scanReuseFixtures) {
	t.Helper()
	floor, _ := surface.floor()
	path := filepath.Join(dir, "versions.db")
	buildBehindStore(t, path, floor, nil)
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := preparePragmas(conn); err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	before := make(map[int]any)
	for version := floor; version <= CurrentSchemaVersion(); version++ {
		seed := versionedScanReuseFixture(fixture, version)
		if surface == ReadSurfaceKnownSessions {
			seedScanReuseSessions(t, conn, version, seed)
		} else {
			seedScanReuseEvidence(t, conn, version, seed)
		}
		reader, err := OpenForScanReuse(path, surface)
		if err != nil {
			t.Fatalf("open %s at version %d: %v", surface, version, err)
		}
		if got := reader.SchemaVersion(); got != version {
			t.Fatalf("seeded %s at version %d, but snapshot reports version %d", surface, version, got)
		}
		before[version] = readScanSurface(t, reader, surface, seed)
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		owed, err := owedRecountsOnConn(conn)
		if err != nil {
			t.Fatalf("read recounts at version %d: %v", version, err)
		}
		for _, name := range owed {
			if err := applyRecountStep(t.Context(), conn, recountStep(name), nil, nil); err != nil {
				t.Fatalf("recount %s at version %d: %v", name, version, err)
			}
		}
		if version == CurrentSchemaVersion() {
			break
		}
		if err := applyUpgradeStep(t.Context(), conn, version+1, false, nil, nil); err != nil {
			t.Fatalf("upgrade %s from version %d: %v", surface, version, err)
		}
	}
	reader, err := OpenForScanReuse(path, surface)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if got := reader.SchemaVersion(); got != CurrentSchemaVersion() {
		t.Fatalf("upgraded %s snapshot is at version %d, want %d", surface, got, CurrentSchemaVersion())
	}
	for version := floor; version <= CurrentSchemaVersion(); version++ {
		after := readScanSurface(t, reader, surface, versionedScanReuseFixture(fixture, version))
		if !reflect.DeepEqual(before[version], after) {
			t.Fatalf("surface %s changed across the version %d upgrade:\nbefore: %#v\nafter: %#v", surface, version, before[version], after)
		}
	}
}

func TestScanReuseFloors(t *testing.T) {
	fixture := loadScanReuseFixtures(t)
	root := scanReuseTestDir(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := filepath.Join(root, c.Name)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			switch c.Kind {
			case scanReuseEquivalent:
				surface, err := NewReadSurface(c.Surface)
				if err != nil {
					t.Fatal(err)
				}
				assertScanReuseEquivalence(t, dir, surface, fixture)
			case scanReuseBelowFloor:
				surface, err := NewReadSurface(c.Surface)
				if err != nil {
					t.Fatal(err)
				}
				floor, _ := surface.floor()
				path := filepath.Join(dir, "below.db")
				buildBehindStore(t, path, floor-1, nil)
				if _, err := OpenForScanReuse(path, surface); err == nil {
					t.Fatal("accepted a store below the requested floor")
				}
			case scanReuseNewer, scanReuseReadOnly, scanReuseUnrequested, scanReuseUnknown, scanReuseEmpty, scanReuseSurfaceSet, scanReuseWriteGate, scanReuseSnapshot, scanReuseEndedSnapshot, scanReuseConcurrentClose:
				runScanReuseBoundary(t, dir, c, fixture)
			default:
				t.Fatalf("unknown scan reuse case kind %q", c.Kind)
			}
		})
	}
}

func runScanReuseBoundary(t *testing.T, dir string, c scanReuseCase, fixture scanReuseFixtures) {
	t.Helper()
	path := filepath.Join(dir, "boundary.db")
	version := CurrentSchemaVersion()
	if c.Kind == scanReuseSnapshot {
		version--
	}
	if c.Kind == scanReuseSnapshot {
		buildBehindStore(t, path, version, func(conn *sqlite.Conn) { seedScanReuseEvidence(t, conn, version, fixture) })
	} else {
		// These boundaries need a current store, not another replay of all
		// historical migrations. The equivalence family above owns that proof.
		db, err := Open(path, WithPoolSize(1))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := db.pool.Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		seedScanReuseEvidence(t, conn, version, fixture)
		db.pool.Put(conn)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	switch c.Kind {
	case scanReuseUnknown:
		if _, err := NewReadSurface(c.Surface); err == nil {
			t.Fatal("accepted an unknown surface")
		}
		if _, err := OpenForScanReuse(path, ReadSurface(c.Surface)); err == nil {
			t.Fatal("opened an unknown surface")
		}
	case scanReuseEmpty:
		if _, err := OpenForScanReuse(path); err == nil {
			t.Fatal("opened with no surfaces")
		}
	case scanReuseSurfaceSet:
		got := make(map[string]bool, len(AllReadSurfaces))
		for _, surface := range AllReadSurfaces {
			if got[surface.String()] {
				t.Fatalf("duplicate surface %s", surface)
			}
			if _, err := NewReadSurface(surface.String()); err != nil {
				t.Fatal(err)
			}
			got[surface.String()] = true
		}
		want := make(map[string]bool, len(fixture.SurfaceNames))
		for _, name := range fixture.SurfaceNames {
			want[name] = true
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read surface membership = %v, want %v", got, want)
		}
	case scanReuseNewer:
		conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
		if err != nil {
			t.Fatal(err)
		}
		scanReuseExec(t, conn, fmt.Sprintf("PRAGMA user_version = %d", CurrentSchemaVersion()+1))
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		before := fileHash(t, path)
		if _, err := OpenForScanReuse(path, ReadSurfaceKnownSessions); err == nil {
			t.Fatal("accepted a newer schema")
		}
		assertHashUnchanged(t, path, before, "newer-schema refusal")
	case scanReuseUnrequested:
		reader, err := OpenForScanReuse(path, ReadSurfaceKnownSessions)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		if _, _, err := reader.LoadClaudeEvidenceCompletely(t.Context()); err == nil {
			t.Fatal("read an unrequested surface")
		}
	case scanReuseReadOnly:
		assertScanReuseReadOnly(t, path)
	case scanReuseWriteGate:
		assertScanReuseWriteGate(t, path)
	case scanReuseSnapshot:
		assertScanReuseSnapshot(t, path, fixture)
	case scanReuseEndedSnapshot:
		reader, err := OpenForScanReuse(path, ReadSurfaceClaudeEvidence)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := reader.pool.Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		scanReuseExec(t, conn, "ROLLBACK")
		reader.pool.Put(conn)
		if _, _, err := reader.LoadClaudeEvidenceCompletely(t.Context()); err == nil {
			t.Fatal("an ended snapshot silently allowed a new read")
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	case scanReuseConcurrentClose:
		assertConcurrentScanReuseClose(t, path)
	}
}

func assertConcurrentScanReuseClose(t *testing.T, path string) {
	t.Helper()
	reader, err := OpenForScanReuse(path, ReadSurfaceClaudeEvidence)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	readResult := make(chan error, 1)
	closeResult := make(chan error, 1)
	go func() {
		<-start
		_, _, readErr := reader.LoadClaudeEvidenceCompletely(ctx)
		readResult <- readErr
	}()
	go func() {
		<-start
		closeResult <- reader.Close()
	}()
	close(start)
	select {
	case err := <-readResult:
		if err != nil && !strings.Contains(err.Error(), "snapshot is closed") {
			t.Fatalf("concurrent read: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("concurrent read did not finish before the deadline")
	}
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("concurrent close: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("concurrent close did not finish before the deadline")
	}
}

func assertScanReuseReadOnly(t *testing.T, path string) {
	t.Helper()
	writer, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	scanReuseExec(t, writer, `PRAGMA wal_autocheckpoint = 0`)
	scanReuseExec(t, writer, `UPDATE claude_transcript_evidence SET title='synthetic WAL value' WHERE source_path='/synthetic/one.jsonl'`)
	before, wal := fileHash(t, path), fileHash(t, path+"-wal")
	// Copy only the synthetic database and its committed WAL while the writer
	// keeps them stable. No connection has ever opened the copy: this exercises
	// the last-reader close after a previous process left a WAL behind.
	orphan := path + ".orphan"
	for _, suffix := range []string{"", "-wal"} {
		contents, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(orphan+suffix, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	orphanDBHash, orphanWALHash := fileHash(t, orphan), fileHash(t, orphan+"-wal")
	orphanReader, err := OpenForScanReuse(orphan, ReadSurfaceClaudeEvidence)
	if err != nil {
		t.Fatalf("open synthetic store left with a WAL and no writer: %v", err)
	}
	orphanRows, _, err := orphanReader.LoadClaudeEvidenceCompletely(t.Context())
	if err != nil || orphanRows[ingest.ResolvedPath("/synthetic/one.jsonl")].Title != "synthetic WAL value" {
		t.Fatalf("read committed orphan WAL: rows=%v err=%v", orphanRows, err)
	}
	if err := orphanReader.Close(); err != nil {
		t.Fatal(err)
	}
	assertHashUnchanged(t, orphan, orphanDBHash, "last read-only connection close")
	assertHashUnchanged(t, orphan+"-wal", orphanWALHash, "last read-only connection close")
	reader, err := OpenForScanReuse(path, ReadSurfaceClaudeEvidence)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := reader.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	queryOnly, err := readPragmaInt(conn, `PRAGMA query_only`)
	if err != nil || queryOnly != 1 {
		t.Fatalf("query_only=%d err=%v", queryOnly, err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE claude_transcript_evidence SET title='forbidden'`, nil); err == nil {
		t.Fatal("query-only snapshot accepted a write")
	}
	// The SQLite open mode must refuse a write independently of query_only.
	// Toggling a connection-local pragma cannot make a read-only file writable.
	scanReuseExec(t, conn, `PRAGMA query_only = OFF`)
	if err := sqlitex.ExecuteTransient(conn, `UPDATE claude_transcript_evidence SET title='forbidden'`, nil); err == nil {
		t.Fatal("read-only connection accepted a write after query_only was disabled")
	}
	scanReuseExec(t, conn, `PRAGMA query_only = ON`)
	reader.pool.Put(conn)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close the snapshot twice: %v", err)
	}
	if _, _, err := reader.LoadClaudeEvidenceCompletely(t.Context()); err == nil {
		t.Fatal("read from a closed snapshot")
	}
	assertHashUnchanged(t, path, before, "read-only reuse")
	assertHashUnchanged(t, path+"-wal", wal, "read-only reuse")
}

func assertScanReuseWriteGate(t *testing.T, path string) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	scanReuseExec(t, conn, fmt.Sprintf("PRAGMA user_version = %d", CurrentSchemaVersion()-1))
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	before := fileHash(t, path)
	deletePath := []ingest.ResolvedPath{"/synthetic/one.jsonl"}
	if err := SaveClaudeEvidenceIfCurrent(t.Context(), path, nil, deletePath); err == nil {
		t.Fatal("wrote a behind store")
	}
	assertHashUnchanged(t, path, before, "behind evidence write refusal")
	// A writable connection can checkpoint an abandoned WAL at close even if
	// it never executes a SQL write. A rejected writer must not do that either.
	writer, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	scanReuseExec(t, writer, `PRAGMA wal_autocheckpoint = 0`)
	scanReuseExec(t, writer, `UPDATE claude_transcript_evidence SET title='synthetic behind WAL' WHERE source_path='/synthetic/one.jsonl'`)
	orphan := path + ".behind-orphan"
	for _, suffix := range []string{"", "-wal"} {
		contents, readErr := os.ReadFile(path + suffix)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err := os.WriteFile(orphan+suffix, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	orphanDBHash, orphanWALHash := fileHash(t, orphan), fileHash(t, orphan+"-wal")
	if err := SaveClaudeEvidenceIfCurrent(t.Context(), orphan, nil, deletePath); err == nil {
		t.Fatal("wrote a behind store with an abandoned WAL")
	}
	assertHashUnchanged(t, orphan, orphanDBHash, "behind evidence write refusal with WAL")
	assertHashUnchanged(t, orphan+"-wal", orphanWALHash, "behind evidence write refusal with WAL")
	conn, err = sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	scanReuseExec(t, conn, fmt.Sprintf("PRAGMA user_version = %d", CurrentSchemaVersion()))
	scanReuseExec(t, conn, `INSERT INTO pending_recounts(name) VALUES('turn_counts')`)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	before = fileHash(t, path)
	if err := SaveClaudeEvidenceIfCurrent(t.Context(), path, nil, deletePath); err == nil {
		t.Fatal("wrote with an owed recount")
	}
	assertHashUnchanged(t, path, before, "pending recount write refusal")
	conn, err = sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	scanReuseExec(t, conn, `DELETE FROM pending_recounts WHERE name='turn_counts'`)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := SaveClaudeEvidenceIfCurrent(t.Context(), path, nil, deletePath); err != nil {
		t.Fatalf("current-store write: %v", err)
	}
	reader, err := OpenForScanReuse(path, ReadSurfaceClaudeEvidence)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rows, _, err := reader.LoadClaudeEvidenceCompletely(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, present := rows[deletePath[0]]; present {
		t.Fatal("current-store write did not remove the requested evidence")
	}
}

func assertScanReuseSnapshot(t *testing.T, path string, fixture scanReuseFixtures) {
	t.Helper()
	reader, err := OpenForScanReuse(path, ReadSurfaceClaudeEvidence)
	if err != nil {
		t.Fatal(err)
	}
	before := readScanSurface(t, reader, ReadSurfaceClaudeEvidence, fixture)
	writer, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	scanReuseExec(t, writer, `UPDATE claude_transcript_evidence SET title='changed after snapshot' WHERE source_path='/synthetic/one.jsonl'`)
	scanReuseExec(t, writer, fmt.Sprintf("PRAGMA user_version = %d", CurrentSchemaVersion()))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	after := readScanSurface(t, reader, ReadSurfaceClaudeEvidence, fixture)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("the pinned scan snapshot changed with a concurrent writer")
	}
	if reader.SchemaVersion() != CurrentSchemaVersion()-1 || reader.IsCurrent() {
		t.Fatal("the pinned scan snapshot changed its schema version")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := OpenForScanReuse(path, ReadSurfaceClaudeEvidence)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	rows, _, err := fresh.LoadClaudeEvidenceCompletely(t.Context())
	if err != nil || rows[ingest.ResolvedPath(fixture.Evidence[0].Path)].Title != "changed after snapshot" {
		t.Fatalf("fresh snapshot did not observe the concurrent write: %v", err)
	}
}
