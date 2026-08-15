package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const expectedLegacySQLiteMountCases = 6

type legacySQLiteHarvestExpectation string

const (
	legacySQLiteHarvestSuccess      legacySQLiteHarvestExpectation = "success"
	legacySQLiteHarvestSessionError legacySQLiteHarvestExpectation = "session_error"
	legacySQLiteHarvestSkip         legacySQLiteHarvestExpectation = "skip"
)

type legacySQLiteMutation string

const (
	legacySQLiteMutationNone             legacySQLiteMutation = "none"
	legacySQLiteMutationMalformedMessage legacySQLiteMutation = "malformed_message_json"
)

type legacySQLiteMountCase struct {
	Name                      string                         `yaml:"name"`
	SourceFixture             string                         `yaml:"source_fixture"`
	ExpectedKickstartSessions int                            `yaml:"expected_kickstart_sessions"`
	ExpectedStoredSessions    int                            `yaml:"expected_stored_sessions"`
	Harvest                   legacySQLiteHarvestExpectation `yaml:"harvest"`
	TargetSession             string                         `yaml:"target_session"`
	ExpectedProjectionSHA256  string                         `yaml:"expected_projection_sha256"`
	ExpectedEntries           int                            `yaml:"expected_entries"`
	Mutation                  legacySQLiteMutation           `yaml:"mutation"`
	Entries                   []legacySQLiteEntryExpectation `yaml:"entries"`
}

type legacySQLiteEntryExpectation struct {
	Index           int    `yaml:"index"`
	Role            string `yaml:"role"`
	EntryType       string `yaml:"entry_type"`
	PartType        string `yaml:"part_type"`
	ParentIndex     int    `yaml:"parent_index"`
	ContentContains string `yaml:"content_contains"`
	ToolCallID      string `yaml:"tool_call_id"`
}

type legacySQLiteMountDocument struct {
	DeclaredCases int                     `yaml:"declared_cases"`
	Cases         []legacySQLiteMountCase `yaml:"cases"`
}

//go:embed testdata/opencode_legacy_sqlite_mount.yaml
var legacySQLiteMountYAML []byte

func loadLegacySQLiteMountDocument(t testing.TB) legacySQLiteMountDocument {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(legacySQLiteMountYAML))
	decoder.KnownFields(true)
	var document legacySQLiteMountDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode legacy SQLite mounted expectations: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("legacy SQLite mounted expectations must contain exactly one document: %v", err)
	}
	if document.DeclaredCases != expectedLegacySQLiteMountCases || len(document.Cases) != expectedLegacySQLiteMountCases {
		t.Fatalf("legacy SQLite mounted expectation row guard: declared=%d actual=%d expected=%d", document.DeclaredCases, len(document.Cases), expectedLegacySQLiteMountCases)
	}
	seen := make(map[string]struct{}, len(document.Cases))
	for _, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || strings.TrimSpace(testCase.SourceFixture) == "" {
			t.Fatalf("legacy SQLite mounted expectation has an empty name or source fixture: %+v", testCase)
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			t.Fatalf("legacy SQLite mounted expectation has duplicate name %q", testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
		if testCase.ExpectedEntries != len(testCase.Entries) {
			t.Fatalf("legacy SQLite mounted expectation %q entry row guard: declared=%d actual=%d", testCase.Name, testCase.ExpectedEntries, len(testCase.Entries))
		}
		switch testCase.Harvest {
		case legacySQLiteHarvestSuccess, legacySQLiteHarvestSessionError, legacySQLiteHarvestSkip:
		default:
			t.Fatalf("legacy SQLite mounted expectation %q has unknown harvest result %q", testCase.Name, testCase.Harvest)
		}
		switch testCase.Mutation {
		case legacySQLiteMutationNone, legacySQLiteMutationMalformedMessage:
		default:
			t.Fatalf("legacy SQLite mounted expectation %q has unknown mutation %q", testCase.Name, testCase.Mutation)
		}
	}
	return document
}

func TestLegacyOpenCodeSQLiteKickstartEligibilityUsesTypedSessions(t *testing.T) {
	document := loadLegacySQLiteMountDocument(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, testCase.SourceFixture)
			before := mustReadFile(t, materialized.Path)
			cfg := mountedOpenCodeConfig(t, filepath.Dir(materialized.Path))
			if testCase.ExpectedKickstartSessions > 0 {
				adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
				direct, directErr := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Dir(materialized.Path))}})
				if directErr != nil || len(direct) != testCase.ExpectedKickstartSessions {
					t.Fatalf("direct production OpenCode discovery sessions=%d error=%v evidence=%+v", len(direct), directErr, adapter.CandidateEvidence())
				}
			}
			inventory, listings := ftueDiscoverWith(t.Context(), cfg, &ingest.OSFileSystem{}, testutil.NoGitResolver(), nil, nil)
			if got := inventory[defaults.HarnessOpenCode].SessionCount; got != testCase.ExpectedKickstartSessions || len(listings) != testCase.ExpectedKickstartSessions {
				t.Fatalf("kickstart SQLite discovery inventory=%d listings=%d, want %d typed sessions", got, len(listings), testCase.ExpectedKickstartSessions)
			}
			for _, listing := range listings {
				if listing.SessionID == "" {
					t.Fatal("kickstart exposed a SQLite session without typed source identity")
				}
			}
			if after := mustReadFile(t, materialized.Path); !bytes.Equal(after, before) {
				t.Fatal("kickstart changed the synthetic source main database bytes")
			}
		})
	}
}

func TestLegacyOpenCodeSQLiteMountedHarvestCreatesManagedIndexedAnalyticsState(t *testing.T) {
	document := loadLegacySQLiteMountDocument(t)
	for _, testCase := range document.Cases {
		if testCase.Harvest == legacySQLiteHarvestSkip {
			continue
		}
		t.Run(testCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, testCase.SourceFixture)
			if testCase.Mutation == legacySQLiteMutationMalformedMessage {
				mutateLegacySQLiteMessageJSON(t, materialized.Path)
				assertMalformedLegacyMaterializationIsActionable(t, materialized.Path, testCase.TargetSession)
			}
			setSyntheticSourceModTime(t, materialized.Path, time.Unix(1_700_001_000, 0))
			before := mustReadFile(t, materialized.Path)
			commandRoot := t.TempDir()
			outputRoot := filepath.Join(commandRoot, "managed")
			args := []string{"--source-provider=" + defaults.HarnessOpenCode.String(), "--source-path=" + filepath.Dir(materialized.Path), "--output=" + outputRoot}
			output, err := executeHarvestCmd(t, commandRoot, args)
			if testCase.Harvest == legacySQLiteHarvestSessionError {
				if err == nil || !strings.Contains(err.Error(), "session(s) failed") {
					t.Fatalf("mounted malformed-row harvest error=%v, want per-session failure\n%s", err, output)
				}
			} else if err != nil {
				t.Fatalf("run mounted harvest command: %v\n%s", err, output)
			}
			if after := mustReadFile(t, materialized.Path); !bytes.Equal(after, before) {
				t.Fatal("harvest changed the synthetic source main database bytes")
			}

			databasePath := defaults.ResolveDBFilePathWith(commandRoot).String()
			localStore, err := store.Open(databasePath, store.WithPoolSize(1))
			if err != nil {
				t.Fatalf("open mounted harvest store: %v", err)
			}
			defer func() {
				if closeErr := localStore.Close(); closeErr != nil {
					t.Errorf("close mounted harvest store: %v", closeErr)
				}
			}()
			rows, err := localStore.ListSessionsFiltered(t.Context(), store.SessionListFilter{})
			if err != nil {
				t.Fatalf("list mounted harvest session rows: %v", err)
			}
			if testCase.Harvest == legacySQLiteHarvestSessionError {
				if len(rows) != testCase.ExpectedStoredSessions {
					t.Fatalf("malformed required row JSON stored %d sessions, want only %d unaffected sessions", len(rows), testCase.ExpectedStoredSessions)
				}
				assertNoManagedTranscriptForSession(t, outputRoot, testCase.TargetSession)
				return
			}
			if len(rows) != testCase.ExpectedStoredSessions {
				t.Fatalf("mounted harvest stored %d sessions, want %d", len(rows), testCase.ExpectedStoredSessions)
			}
			sessionID, err := ingest.NewSessionID(testCase.TargetSession)
			if err != nil {
				t.Fatalf("validate target session expectation: %v", err)
			}
			entries, err := localStore.ListEntries(t.Context(), sessionID)
			if err != nil {
				t.Fatalf("list mounted legacy entries: %v", err)
			}
			if len(entries) != testCase.ExpectedEntries {
				t.Fatalf("mounted legacy entries=%d, want %d: %+v", len(entries), testCase.ExpectedEntries, entries)
			}
			for index, entry := range entries {
				if entry.EntryIndex != index {
					t.Errorf("entry %d has non-contiguous index %d", index, entry.EntryIndex)
				}
			}
			assertLegacySQLiteEntries(t, entries, testCase.Entries)
			metrics, err := localStore.GetMetrics(t.Context(), sessionID)
			if err != nil || metrics == nil {
				t.Fatalf("mounted legacy analytics evidence missing: metrics=%+v error=%v", metrics, err)
			}

			managedPath := findManagedTranscript(t, outputRoot, testCase.TargetSession)
			managedBytes, err := os.ReadFile(managedPath)
			if err != nil {
				t.Fatalf("read managed legacy projection: %v", err)
			}
			if bytes.HasPrefix(managedBytes, []byte("SQLite format 3\x00")) || bytes.Equal(managedBytes, mustReadFile(t, materialized.Path)) {
				t.Fatal("managed transcript contains a SQLite header or is a raw database copy")
			}
			if testCase.ExpectedProjectionSHA256 != "" {
				hash := sha256.Sum256(managedBytes)
				if got := hex.EncodeToString(hash[:]); got != testCase.ExpectedProjectionSHA256 {
					t.Fatalf("managed projection SHA-256=%s, want %s; bytes=%s", got, testCase.ExpectedProjectionSHA256, managedBytes)
				}
			}
			assertManagedMetadata(t, managedPath, sessionID)

			firstBytes := append([]byte(nil), managedBytes...)
			firstRows := len(rows)
			output, err = executeHarvestCmd(t, commandRoot, args)
			if err != nil {
				t.Fatalf("repeat mounted harvest command: %v\n%s", err, output)
			}
			repeatedStore, err := store.Open(databasePath, store.WithPoolSize(1))
			if err != nil {
				t.Fatalf("reopen repeated harvest store: %v", err)
			}
			repeatedRows, listErr := repeatedStore.ListSessionsFiltered(t.Context(), store.SessionListFilter{})
			closeErr := repeatedStore.Close()
			if listErr != nil || closeErr != nil {
				t.Fatalf("inspect repeated harvest store: %v", errors.Join(listErr, closeErr))
			}
			if len(repeatedRows) != firstRows {
				t.Fatalf("repeat harvest changed session row count from %d to %d", firstRows, len(repeatedRows))
			}
			if repeatedBytes := mustReadFile(t, managedPath); !bytes.Equal(repeatedBytes, firstBytes) {
				t.Fatal("repeat harvest changed deterministic managed projection bytes")
			}

			mutateLegacySQLiteMessageVersion(t, materialized.Path)
			setSyntheticSourceModTime(t, materialized.Path, time.Unix(1_700_002_000, 0))
			setLocalIngestedTimestamp(t, databasePath, 1_700_001_500_000)
			output, err = executeHarvestCmd(t, commandRoot, args)
			if err != nil {
				t.Fatalf("harvest changed synthetic source: %v\n%s", err, output)
			}
			if !strings.Contains(output, "2 updated") {
				t.Fatalf("shared SQLite source change did not report only its two expected session updates:\n%s", output)
			}
			changedStore, err := store.Open(databasePath, store.WithPoolSize(1))
			if err != nil {
				t.Fatalf("reopen changed-source store: %v", err)
			}
			changedRows, listErr := changedStore.ListSessionsFiltered(t.Context(), store.SessionListFilter{})
			closeErr = changedStore.Close()
			if listErr != nil || closeErr != nil || len(changedRows) != firstRows {
				t.Fatalf("changed-source harvest rows=%d want=%d error=%v", len(changedRows), firstRows, errors.Join(listErr, closeErr))
			}
			if changedBytes := mustReadFile(t, managedPath); bytes.Equal(changedBytes, firstBytes) {
				t.Fatal("changed selected source row did not update the managed projection")
			}
			reindexConfig := filepath.Join(commandRoot, "reindex-config.yaml")
			reindexConfigData := []byte("version: 1\nsources:\n  claude-code: {enabled: false}\n  opencode: {enabled: false}\n  cursor: {enabled: false}\noutput:\n  basePath: " + outputRoot + "\n")
			if writeErr := os.WriteFile(reindexConfig, reindexConfigData, 0o600); writeErr != nil {
				t.Fatalf("write mounted reindex config: %v", writeErr)
			}
			reindexOutput, reindexErr := executeHarvestCmd(t, commandRoot, []string{"--config", reindexConfig, "index", "--all"})
			if reindexErr != nil {
				t.Fatalf("reindex managed legacy projection through mounted command: %v\n%s", reindexErr, reindexOutput)
			}
			reindexStore, err := store.Open(databasePath, store.WithPoolSize(1))
			if err != nil {
				t.Fatalf("reopen reindexed store: %v", err)
			}
			reindexedEntries, listErr := reindexStore.ListEntries(t.Context(), sessionID)
			closeErr = reindexStore.Close()
			if listErr != nil || closeErr != nil || len(reindexedEntries) != testCase.ExpectedEntries {
				t.Fatalf("managed projection reconstruction entries=%d want=%d error=%v", len(reindexedEntries), testCase.ExpectedEntries, errors.Join(listErr, closeErr))
			}
		})
	}
}

func mutateLegacySQLiteMessageJSON(t testing.TB, path string) {
	t.Helper()
	connection, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic malformed-row control: %v", err)
	}
	updateErr := sqlitex.ExecuteTransient(connection, "UPDATE message SET data = 'not-json' WHERE id = 'msg_equal_a'", nil)
	closeErr := connection.Close()
	if updateErr != nil || closeErr != nil {
		t.Fatalf("prepare malformed-row control: %v", errors.Join(updateErr, closeErr))
	}
}

func mutateLegacySQLiteMessageVersion(t testing.TB, path string) {
	t.Helper()
	connection, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic source-change control: %v", err)
	}
	updateErr := sqlitex.ExecuteTransient(connection, `UPDATE message SET data = '{"role":"user","version":"changed-source","path":{"cwd":"/synthetic/work/project"},"time":{"created":1700000000000,"completed":1700000000010}}' WHERE id = 'msg_equal_a'`, nil)
	closeErr := connection.Close()
	if updateErr != nil || closeErr != nil {
		t.Fatalf("prepare synthetic source change: %v", errors.Join(updateErr, closeErr))
	}
}

func setSyntheticSourceModTime(t testing.TB, path string, modified time.Time) {
	t.Helper()
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatalf("set synthetic source modification time: %v", err)
	}
}

func setLocalIngestedTimestamp(t testing.TB, databasePath string, timestamp int64) {
	t.Helper()
	connection, err := sqlite.OpenConn(databasePath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open local update-classification control: %v", err)
	}
	updateErr := sqlitex.ExecuteTransient(connection, "UPDATE sessions SET ingested_ms = ?1", &sqlitex.ExecOptions{Args: []any{timestamp}})
	closeErr := connection.Close()
	if updateErr != nil || closeErr != nil {
		t.Fatalf("prepare local update-classification control: %v", errors.Join(updateErr, closeErr))
	}
}

func findManagedTranscript(t testing.TB, root, sessionID string) string {
	t.Helper()
	want := sessionID + "--transcript." + string(ingest.SourceFormatJSON)
	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == want {
			found = path
		}
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("find managed transcript %q under %q: found=%q error=%v", want, root, found, err)
	}
	return found
}

func assertNoManagedTranscriptForSession(t testing.TB, root, sessionID string) {
	t.Helper()
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == sessionID+"--transcript."+string(ingest.SourceFormatJSON) {
			t.Errorf("malformed source created partial managed transcript %q", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inspect managed output after malformed source: %v", err)
	}
}

func assertMalformedLegacyMaterializationIsActionable(t testing.TB, databasePath, targetSession string) {
	t.Helper()
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
	sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Dir(databasePath))}})
	if err != nil {
		t.Fatalf("discover malformed-row source identity: %v", err)
	}
	for _, session := range sessions {
		if string(session.SessionID) != targetSession {
			continue
		}
		_, _, err = adapter.MaterializeTranscript(context.Background(), session)
		if err == nil || !strings.Contains(err.Error(), "not valid JSON") || !strings.Contains(err.Error(), "no partial") {
			t.Fatalf("malformed required row diagnostic=%v, want reason, no-partial meaning, location, and remediation", err)
		}
		return
	}
	t.Fatalf("malformed-row target session %q was not discovered", targetSession)
}

func assertLegacySQLiteEntries(t testing.TB, entries []schema.SessionEntry, expected []legacySQLiteEntryExpectation) {
	t.Helper()
	if len(entries) != len(expected) {
		t.Fatalf("entry expectation guard: actual=%d expected=%d", len(entries), len(expected))
	}
	for index, want := range expected {
		got := entries[index]
		partType, toolCallID, content := "", "", ""
		if got.PartType != nil {
			partType = *got.PartType
		}
		if got.ToolCallID != nil {
			toolCallID = *got.ToolCallID
		}
		if got.ContentPreview != nil {
			content = *got.ContentPreview
		}
		parentIndex := -1
		if got.ParentIndex != nil {
			parentIndex = *got.ParentIndex
		}
		if got.EntryIndex != want.Index || got.Role.String() != want.Role || got.EntryType.String() != want.EntryType || partType != want.PartType || parentIndex != want.ParentIndex || toolCallID != want.ToolCallID || !strings.Contains(content, want.ContentContains) {
			t.Errorf("entry %d=%+v, want %+v (part=%q parent=%d tool=%q content=%q)", index, got, want, partType, parentIndex, toolCallID, content)
		}
	}
}

func assertManagedMetadata(t testing.TB, transcriptPath string, sessionID ingest.SessionID) {
	t.Helper()
	metadataPath := filepath.Join(filepath.Dir(transcriptPath), string(sessionID)+defaults.MetadataSuffix)
	data := mustReadFile(t, metadataPath)
	var metadata ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("decode managed legacy metadata: %v", err)
	}
	if metadata.SessionID != sessionID || metadata.Source.Format != ingest.SourceFormatJSON || !strings.HasSuffix(metadata.Source.FilePath, ".db") {
		t.Fatalf("managed metadata lost SQLite source identity: %+v", metadata)
	}
	if sessionID == ingest.SessionID("ses_3cd91f52effeXd3QAJ54jOyzv5") {
		if metadata.CWD != "/synthetic/work/project" || metadata.Project.Name != "project" || metadata.Model.String() != "synthetic-model" || metadata.Stats.TokensIn != 7 || metadata.Stats.TokensOut != 11 || metadata.Stats.ToolCallCount != 1 {
			t.Fatalf("managed metadata omitted selected row evidence: %+v", metadata)
		}
	}
}

func mustReadFile(t testing.TB, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return data
}
