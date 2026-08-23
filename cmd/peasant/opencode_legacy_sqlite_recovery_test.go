package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	transcriptmodel "github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	expectedLegacySQLiteFreshnessCases = 1
	expectedLegacySQLiteCandidateCases = 3
	expectedLegacySQLiteRecoveryCases  = 5
)

type legacySQLiteChannelMutation string

const (
	legacySQLiteChannelMutationNone            legacySQLiteChannelMutation = "none"
	legacySQLiteChannelMutationReplaceQuestion legacySQLiteChannelMutation = "replace_question"
)

type legacySQLiteMetadataState string

const (
	legacySQLiteMetadataMissing legacySQLiteMetadataState = "missing"
	legacySQLiteMetadataCorrupt legacySQLiteMetadataState = "corrupt"
)

type legacySQLiteEnvelopeMutation string

const (
	legacySQLiteEnvelopeNone      legacySQLiteEnvelopeMutation = "none"
	legacySQLiteEnvelopeArbitrary legacySQLiteEnvelopeMutation = "arbitrary_json"
	legacySQLiteEnvelopeVersion   legacySQLiteEnvelopeMutation = "wrong_version"
	legacySQLiteEnvelopeKind      legacySQLiteEnvelopeMutation = "wrong_kind"
)

type legacySQLiteFreshnessCase struct {
	Name                  string `yaml:"name"`
	SourceFixture         string `yaml:"source_fixture"`
	TargetSession         string `yaml:"target_session"`
	MessageID             string `yaml:"message_id"`
	PartID                string `yaml:"part_id"`
	MessageTimeCreated    int64  `yaml:"message_time_created"`
	MessageTimeUpdated    int64  `yaml:"message_time_updated"`
	MessageData           string `yaml:"message_data"`
	PartTimeCreated       int64  `yaml:"part_time_created"`
	PartTimeUpdated       int64  `yaml:"part_time_updated"`
	PartData              string `yaml:"part_data"`
	ExpectedUpdateCount   int    `yaml:"expected_update_count"`
	ExpectedTargetEntries int    `yaml:"expected_target_entries"`
	ExpectedTargetTurns   int    `yaml:"expected_target_turns"`
	ExpectedContent       string `yaml:"expected_content"`
}

type legacySQLiteCandidateCase struct {
	Name                     string                      `yaml:"name"`
	OverrideFixture          string                      `yaml:"override_fixture"`
	ChannelFixture           string                      `yaml:"channel_fixture"`
	OriginalSessionIDs       []string                    `yaml:"original_session_ids"`
	OverrideSessionIDs       []string                    `yaml:"override_session_ids"`
	ChannelMutation          legacySQLiteChannelMutation `yaml:"channel_mutation"`
	ExpectedSessionIDs       []string                    `yaml:"expected_session_ids"`
	ForbiddenArtifactContent string                      `yaml:"forbidden_artifact_content"`
}

type legacySQLiteRecoveryCase struct {
	Name             string                       `yaml:"name"`
	SourceFixture    string                       `yaml:"source_fixture"`
	TargetSession    string                       `yaml:"target_session"`
	MetadataState    legacySQLiteMetadataState    `yaml:"metadata_state"`
	EnvelopeMutation legacySQLiteEnvelopeMutation `yaml:"envelope_mutation"`
	ExpectedRecovery bool                         `yaml:"expected_recovery"`
	ExpectedEntries  int                          `yaml:"expected_entries"`
	ExpectedTurns    int                          `yaml:"expected_turns"`
	ExpectedToolCall *legacySQLiteToolExpectation `yaml:"expected_tool_call"`
}

type legacySQLiteRecoveryDocument struct {
	DeclaredFreshnessCases int                         `yaml:"declared_freshness_cases"`
	FreshnessCases         []legacySQLiteFreshnessCase `yaml:"freshness_cases"`
	DeclaredCandidateCases int                         `yaml:"declared_candidate_cases"`
	CandidateCases         []legacySQLiteCandidateCase `yaml:"candidate_cases"`
	DeclaredRecoveryCases  int                         `yaml:"declared_recovery_cases"`
	RecoveryCases          []legacySQLiteRecoveryCase  `yaml:"recovery_cases"`
}

//go:embed testdata/opencode_legacy_sqlite_recovery.yaml
var legacySQLiteRecoveryYAML []byte

func loadLegacySQLiteRecoveryDocument(data []byte) (legacySQLiteRecoveryDocument, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var document legacySQLiteRecoveryDocument
	if err := decoder.Decode(&document); err != nil {
		return document, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, errors.New("legacy SQLite recovery fixture must contain exactly one YAML document")
	}
	if document.DeclaredFreshnessCases != expectedLegacySQLiteFreshnessCases || len(document.FreshnessCases) != expectedLegacySQLiteFreshnessCases ||
		document.DeclaredCandidateCases != expectedLegacySQLiteCandidateCases || len(document.CandidateCases) != expectedLegacySQLiteCandidateCases ||
		document.DeclaredRecoveryCases != expectedLegacySQLiteRecoveryCases || len(document.RecoveryCases) != expectedLegacySQLiteRecoveryCases {
		return document, errors.New("legacy SQLite recovery fixture count guard failed")
	}
	seen := make(map[string]struct{}, expectedLegacySQLiteFreshnessCases+expectedLegacySQLiteCandidateCases+expectedLegacySQLiteRecoveryCases)
	requireUnique := func(name string) error {
		if strings.TrimSpace(name) == "" {
			return errors.New("legacy SQLite recovery fixture contains an empty case name")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("legacy SQLite recovery fixture contains a duplicate case name")
		}
		seen[name] = struct{}{}
		return nil
	}
	requireUniqueSessionIDs := func(values []string) error {
		seenIDs := make(map[string]struct{}, len(values))
		for _, sessionID := range values {
			if strings.TrimSpace(sessionID) == "" {
				return errors.New("legacy SQLite recovery fixture contains an empty session ID")
			}
			if _, duplicate := seenIDs[sessionID]; duplicate {
				return errors.New("legacy SQLite recovery fixture contains a duplicate session ID")
			}
			seenIDs[sessionID] = struct{}{}
		}
		return nil
	}
	for _, testCase := range document.FreshnessCases {
		if err := requireUnique(testCase.Name); err != nil {
			return document, err
		}
		if testCase.SourceFixture == "" || testCase.TargetSession == "" || testCase.MessageID == "" || testCase.PartID == "" || testCase.MessageData == "" || testCase.PartData == "" || testCase.ExpectedContent == "" || testCase.ExpectedUpdateCount <= 0 || testCase.ExpectedTargetEntries <= 0 || testCase.ExpectedTargetTurns <= 0 {
			return document, errors.New("legacy SQLite freshness fixture contains an incomplete row")
		}
	}
	for _, testCase := range document.CandidateCases {
		if err := requireUnique(testCase.Name); err != nil {
			return document, err
		}
		if testCase.OverrideFixture == "" || testCase.ChannelFixture == "" || len(testCase.ExpectedSessionIDs) == 0 {
			return document, errors.New("legacy SQLite candidate fixture contains an incomplete row")
		}
		if len(testCase.OriginalSessionIDs) != len(testCase.OverrideSessionIDs) || len(testCase.OverrideSessionIDs) != 0 && len(testCase.OverrideSessionIDs) != 2 {
			return document, errors.New("legacy SQLite candidate fixture session rewrite must name both source sessions")
		}
		if err := requireUniqueSessionIDs(testCase.OriginalSessionIDs); err != nil {
			return document, err
		}
		if err := requireUniqueSessionIDs(testCase.OverrideSessionIDs); err != nil {
			return document, err
		}
		if err := requireUniqueSessionIDs(testCase.ExpectedSessionIDs); err != nil {
			return document, err
		}
		switch testCase.ChannelMutation {
		case legacySQLiteChannelMutationNone, legacySQLiteChannelMutationReplaceQuestion:
		default:
			return document, errors.New("legacy SQLite candidate fixture contains an unknown channel mutation")
		}
	}
	for _, testCase := range document.RecoveryCases {
		if err := requireUnique(testCase.Name); err != nil {
			return document, err
		}
		if testCase.SourceFixture == "" || testCase.TargetSession == "" || testCase.ExpectedEntries < 0 || testCase.ExpectedTurns < 0 {
			return document, errors.New("legacy SQLite recovery fixture contains an incomplete row")
		}
		if testCase.MetadataState != legacySQLiteMetadataMissing && testCase.MetadataState != legacySQLiteMetadataCorrupt {
			return document, errors.New("legacy SQLite recovery fixture contains an unknown metadata state")
		}
		switch testCase.EnvelopeMutation {
		case legacySQLiteEnvelopeNone, legacySQLiteEnvelopeArbitrary, legacySQLiteEnvelopeVersion, legacySQLiteEnvelopeKind:
		default:
			return document, errors.New("legacy SQLite recovery fixture contains an unknown envelope mutation")
		}
		if testCase.ExpectedRecovery != (testCase.EnvelopeMutation == legacySQLiteEnvelopeNone) {
			return document, errors.New("legacy SQLite recovery fixture does not distinguish valid and invalid managed envelopes")
		}
		if testCase.ExpectedRecovery != (testCase.ExpectedToolCall != nil) {
			return document, errors.New("legacy SQLite recovery fixture tool expectation does not match recovery outcome")
		}
	}
	return document, nil
}

func mustLegacySQLiteRecoveryDocument(t testing.TB) legacySQLiteRecoveryDocument {
	t.Helper()
	document, err := loadLegacySQLiteRecoveryDocument(legacySQLiteRecoveryYAML)
	if err != nil {
		t.Fatalf("load legacy SQLite recovery fixture: %v", err)
	}
	return document
}

func TestLegacyOpenCodeSQLiteCommittedWALUpdateRefreshesMountedState(t *testing.T) {
	testCase := mustLegacySQLiteRecoveryDocument(t).FreshnessCases[0]
	materialized := testfixture.MaterializeByName(t, testCase.SourceFixture)
	writer := openMountedLegacyWALWriter(t, materialized.Path)
	defer closeMountedSQLiteConnection(t, writer, "legacy WAL freshness writer")
	ensureMountedWALFile(t, writer)
	initialDatabaseTime := time.UnixMilli(1_700_000_000_000)
	initialWALTime := time.UnixMilli(1_700_000_001_000)
	setSyntheticSourceModTime(t, materialized.Path, initialDatabaseTime)
	setSyntheticSourceModTime(t, materialized.Path+"-wal", initialWALTime)

	commandRoot := t.TempDir()
	outputRoot := filepath.Join(commandRoot, "managed")
	args := []string{"--source-provider=" + defaults.HarnessOpenCode.String(), "--source-path=" + filepath.Dir(materialized.Path), "--output=" + outputRoot}
	initialArgs := append(append([]string(nil), args...), "--force", "--include-active")
	if output, err := executeHarvestCmd(t, commandRoot, initialArgs); err != nil {
		t.Fatalf("initial mounted WAL harvest: %v\n%s", err, output)
	}
	targetID, err := ingest.NewSessionID(testCase.TargetSession)
	if err != nil {
		t.Fatalf("validate WAL freshness target session: %v", err)
	}
	managedPath := findManagedTranscript(t, outputRoot, testCase.TargetSession)
	initialManaged := mustReadFile(t, managedPath)
	databasePath := defaults.ResolveDBFilePathWith(commandRoot).String()
	initialStore, err := store.Open(databasePath, store.WithPoolSize(1))
	if err != nil {
		t.Fatalf("open initial WAL freshness store: %v", err)
	}
	initialEntries, entriesErr := initialStore.ListEntries(t.Context(), targetID)
	initialMetrics, metricsErr := initialStore.GetMetrics(t.Context(), targetID)
	closeErr := initialStore.Close()
	if entriesErr != nil || metricsErr != nil || closeErr != nil || initialMetrics == nil {
		t.Fatalf("inspect initial WAL freshness state: %v", errors.Join(entriesErr, metricsErr, closeErr))
	}

	mainBefore := mustReadFile(t, materialized.Path)
	mainInfoBefore, err := os.Stat(materialized.Path)
	if err != nil {
		t.Fatalf("stat main database before WAL-only commit: %v", err)
	}
	walInfoBefore, err := os.Stat(materialized.Path + "-wal")
	if err != nil {
		t.Fatalf("stat live WAL before committed update: %v", err)
	}
	if err := appendMountedLegacyWALRows(writer, testCase); err != nil {
		t.Fatalf("commit synthetic mounted WAL-only rows: %v", err)
	}
	advancedWALTime := time.UnixMilli(1_700_000_003_000)
	if err := os.Chtimes(materialized.Path+"-wal", advancedWALTime, advancedWALTime); err != nil {
		t.Fatalf("advance synthetic committed WAL timestamp: %v", err)
	}
	walInfoAfterCommit, err := os.Stat(materialized.Path + "-wal")
	if err != nil {
		t.Fatalf("stat committed WAL update: %v", err)
	}
	if !walInfoAfterCommit.ModTime().After(walInfoBefore.ModTime()) {
		t.Fatalf("committed WAL timestamp did not advance: before=%s after=%s", walInfoBefore.ModTime(), walInfoAfterCommit.ModTime())
	}
	mainInfoAfterCommit, err := os.Stat(materialized.Path)
	if err != nil || !bytes.Equal(mustReadFile(t, materialized.Path), mainBefore) || !mainInfoAfterCommit.ModTime().Equal(mainInfoBefore.ModTime()) {
		t.Fatalf("WAL-only commit changed main database content or timestamp: error=%v before=%s after=%s", err, mainInfoBefore.ModTime(), mainInfoAfterCommit.ModTime())
	}
	setLocalIngestedTimestamp(t, databasePath, time.UnixMilli(1_700_000_002_000).UnixMilli())
	walBeforeRerun := mustReadFile(t, materialized.Path+"-wal")

	output, err := executeHarvestCmd(t, commandRoot, args)
	if err != nil {
		t.Fatalf("rerun mounted harvest after WAL-only update: %v\n%s", err, output)
	}
	if !strings.Contains(output, strings.Join([]string{strconv.Itoa(testCase.ExpectedUpdateCount), "updated"}, " ")) {
		t.Fatalf("WAL-only update was not classified as updated:\n%s", output)
	}
	if !bytes.Equal(mustReadFile(t, materialized.Path), mainBefore) || !bytes.Equal(mustReadFile(t, materialized.Path+"-wal"), walBeforeRerun) {
		t.Fatal("mounted rerun changed source database or committed WAL transaction bytes")
	}
	updatedManaged := mustReadFile(t, managedPath)
	if bytes.Equal(updatedManaged, initialManaged) || !bytes.Contains(updatedManaged, []byte(testCase.ExpectedContent)) {
		t.Fatalf("WAL-only update did not change deterministic managed projection with %q", testCase.ExpectedContent)
	}
	updatedStore, err := store.Open(databasePath, store.WithPoolSize(1))
	if err != nil {
		t.Fatalf("open updated WAL freshness store: %v", err)
	}
	updatedEntries, entriesErr := updatedStore.ListEntries(t.Context(), targetID)
	updatedMetrics, metricsErr := updatedStore.GetMetrics(t.Context(), targetID)
	closeErr = updatedStore.Close()
	if entriesErr != nil || metricsErr != nil || closeErr != nil || updatedMetrics == nil {
		t.Fatalf("inspect updated WAL freshness state: %v", errors.Join(entriesErr, metricsErr, closeErr))
	}
	if len(initialEntries) >= len(updatedEntries) || len(updatedEntries) != testCase.ExpectedTargetEntries || updatedMetrics.TurnCount == nil || *updatedMetrics.TurnCount != testCase.ExpectedTargetTurns {
		t.Fatalf("updated WAL state entries=%d initial=%d turns=%v, want entries=%d turns=%d", len(updatedEntries), len(initialEntries), updatedMetrics.TurnCount, testCase.ExpectedTargetEntries, testCase.ExpectedTargetTurns)
	}
	turns := transcriptmodel.EntriesToTurns(updatedEntries)
	detail := transcriptmodel.SessionToDetail(&ingest.Session{ID: targetID, Harness: ingest.HarnessOpenCode, Turns: turns})
	detailData, marshalErr := json.Marshal(detail)
	if marshalErr != nil || !bytes.Contains(detailData, []byte(testCase.ExpectedContent)) {
		t.Fatalf("updated session detail omitted WAL-only content %q: error=%v detail=%s", testCase.ExpectedContent, marshalErr, detailData)
	}
}

func TestLegacyOpenCodeSQLiteSelectsAcrossEligibleCandidates(t *testing.T) {
	document := mustLegacySQLiteRecoveryDocument(t)
	for _, testCase := range document.CandidateCases {
		t.Run(testCase.Name, func(t *testing.T) {
			overrideSource := testfixture.MaterializeByName(t, testCase.OverrideFixture)
			channelSource := testfixture.MaterializeByName(t, testCase.ChannelFixture)
			root := t.TempDir()
			overridePath := filepath.Join(root, "explicit.db")
			channelPath := filepath.Join(root, "opencode.db")
			copySyntheticDatabase(t, overrideSource.Path, overridePath)
			copySyntheticDatabase(t, channelSource.Path, channelPath)
			if len(testCase.OverrideSessionIDs) != 0 {
				rewriteMountedSessionIDs(t, overridePath, testCase.OriginalSessionIDs, testCase.OverrideSessionIDs)
			}
			if testCase.ChannelMutation == legacySQLiteChannelMutationReplaceQuestion {
				replaceMountedQuestion(t, channelPath, testCase.ForbiddenArtifactContent)
			}
			overrideBefore := mustReadFile(t, overridePath)
			channelBefore := mustReadFile(t, channelPath)
			t.Setenv("OPENCODE_DB", overridePath)
			t.Setenv("OPENCODE_DISABLE_CHANNEL_DB", "")

			adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
			discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(root)}})
			if err != nil {
				t.Fatalf("discover deterministic candidate sources: %v", err)
			}
			if got := discoveredSessionStrings(discovered); !slices.Equal(got, testCase.ExpectedSessionIDs) {
				t.Fatalf("discovered candidate sessions=%v, want canonical sessions %v; evidence=%+v", got, testCase.ExpectedSessionIDs, adapter.CandidateEvidence())
			}

			cfg := mountedOpenCodeConfig(t, root)
			inventory, listings := ftueDiscoverWith(t.Context(), cfg, &ingest.OSFileSystem{}, testutil.NoGitResolver(), nil, nil, nil)
			if inventory[defaults.HarnessOpenCode].SessionCount != len(testCase.ExpectedSessionIDs) || !slices.Equal(listingSessionStrings(listings), testCase.ExpectedSessionIDs) {
				t.Fatalf("kickstart candidate listings=%v inventory=%d, want %v", listingSessionStrings(listings), inventory[defaults.HarnessOpenCode].SessionCount, testCase.ExpectedSessionIDs)
			}

			commandRoot := t.TempDir()
			outputRoot := filepath.Join(commandRoot, "managed")
			output, err := executeHarvestCmd(t, commandRoot, []string{"--source-provider=" + defaults.HarnessOpenCode.String(), "--source-path=" + root, "--output=" + outputRoot, "--force", "--include-active"})
			if err != nil {
				t.Fatalf("harvest canonical eligible candidates: %v\n%s", err, output)
			}
			managedIDs := managedTranscriptSessionIDs(t, outputRoot)
			if !slices.Equal(managedIDs, testCase.ExpectedSessionIDs) {
				t.Fatalf("managed candidate artifacts=%v, want %v", managedIDs, testCase.ExpectedSessionIDs)
			}
			for _, sessionID := range managedIDs {
				if testCase.ForbiddenArtifactContent != "" && bytes.Contains(mustReadFile(t, findManagedTranscript(t, outputRoot, sessionID)), []byte(testCase.ForbiddenArtifactContent)) {
					t.Fatalf("managed candidate artifact for %s contains evidence from a later candidate", sessionID)
				}
			}
			localStore, err := store.Open(defaults.ResolveDBFilePathWith(commandRoot).String(), store.WithPoolSize(1))
			if err != nil {
				t.Fatalf("open candidate-selection store: %v", err)
			}
			rows, listErr := localStore.ListSessionsFiltered(t.Context(), store.SessionListFilter{})
			closeErr := localStore.Close()
			if listErr != nil || closeErr != nil || !slices.Equal(storedSessionStrings(rows), testCase.ExpectedSessionIDs) {
				t.Fatalf("candidate store sessions=%v, want %v: %v", storedSessionStrings(rows), testCase.ExpectedSessionIDs, errors.Join(listErr, closeErr))
			}
			if !bytes.Equal(mustReadFile(t, overridePath), overrideBefore) || !bytes.Equal(mustReadFile(t, channelPath), channelBefore) {
				t.Fatal("candidate selection changed source database bytes")
			}
		})
	}
}

func TestLegacyOpenCodeSQLiteSourceInfoRecoveryValidatesManagedEnvelope(t *testing.T) {
	document := mustLegacySQLiteRecoveryDocument(t)
	for _, testCase := range document.RecoveryCases {
		t.Run(testCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, testCase.SourceFixture)
			setSyntheticSQLiteContentModTime(t, materialized.Path, time.UnixMilli(1_700_000_000_000))
			commandRoot := t.TempDir()
			outputRoot := filepath.Join(commandRoot, "managed")
			args := []string{"--source-provider=" + defaults.HarnessOpenCode.String(), "--source-path=" + filepath.Dir(materialized.Path), "--output=" + outputRoot}
			initialArgs := append(append([]string(nil), args...), "--force", "--include-active")
			if output, err := executeHarvestCmd(t, commandRoot, initialArgs); err != nil {
				t.Fatalf("initial mounted recovery harvest: %v\n%s", err, output)
			}
			managedPath := findManagedTranscript(t, outputRoot, testCase.TargetSession)
			managedBefore := mustReadFile(t, managedPath)
			metadataPath := filepath.Join(filepath.Dir(managedPath), testCase.TargetSession+defaults.MetadataSuffix)
			switch testCase.MetadataState {
			case legacySQLiteMetadataMissing:
				if err := os.Remove(metadataPath); err != nil {
					t.Fatalf("remove managed metadata for recovery: %v", err)
				}
			case legacySQLiteMetadataCorrupt:
				if err := os.WriteFile(metadataPath, []byte("not-json\n"), 0o600); err != nil {
					t.Fatalf("corrupt managed metadata for recovery: %v", err)
				}
			}
			mutateManagedEnvelope(t, managedPath, testCase.EnvelopeMutation)
			databasePath := defaults.ResolveDBFilePathWith(commandRoot).String()
			markMountedIndexStaleAndEmpty(t, databasePath, testCase.TargetSession)
			sourceBefore := mustReadFile(t, materialized.Path)
			walBefore, walWasPresent := readOptionalSyntheticFile(t, materialized.Path+"-wal")

			output, err := executeHarvestCmd(t, commandRoot, args)
			if err != nil {
				t.Fatalf("run mounted source-info recovery: %v\n%s", err, output)
			}
			if !bytes.Equal(mustReadFile(t, materialized.Path), sourceBefore) {
				t.Fatal("source-info recovery changed source database bytes")
			}
			walAfter, walIsPresent := readOptionalSyntheticFile(t, materialized.Path+"-wal")
			if walWasPresent != walIsPresent || walWasPresent && !bytes.Equal(walAfter, walBefore) {
				t.Fatal("source-info recovery changed committed WAL transaction content")
			}
			targetID, idErr := ingest.NewSessionID(testCase.TargetSession)
			if idErr != nil {
				t.Fatalf("validate recovery target session: %v", idErr)
			}
			localStore, err := store.Open(databasePath, store.WithPoolSize(1))
			if err != nil {
				t.Fatalf("open recovered store: %v", err)
			}
			entries, entriesErr := localStore.ListEntries(t.Context(), targetID)
			metrics, metricsErr := localStore.GetMetrics(t.Context(), targetID)
			closeErr := localStore.Close()
			if entriesErr != nil || metricsErr != nil || closeErr != nil {
				t.Fatalf("inspect source-info recovery state: %v", errors.Join(entriesErr, metricsErr, closeErr))
			}
			if len(entries) != testCase.ExpectedEntries {
				t.Fatalf("source-info recovery entries=%d, want %d", len(entries), testCase.ExpectedEntries)
			}
			if testCase.ExpectedRecovery {
				if metrics == nil || metrics.TurnCount == nil || *metrics.TurnCount != testCase.ExpectedTurns {
					t.Fatalf("source-info recovery metrics=%+v, want %d turns", metrics, testCase.ExpectedTurns)
				}
				assertLegacySQLiteToolFold(t, entries, testCase.ExpectedToolCall)
				if !bytes.Equal(mustReadFile(t, managedPath), managedBefore) {
					t.Fatal("source-info recovery rewrote intact managed projection bytes")
				}
			} else if metrics != nil {
				t.Fatalf("invalid managed envelope unexpectedly recomputed metrics: %+v", metrics)
			}
		})
	}
}

func TestLegacySQLiteRecoveryFixtureLoaderMutationsAreRejected(t *testing.T) {
	unknownField := bytes.Replace(legacySQLiteRecoveryYAML, []byte("source_fixture:"), []byte("unknown_source_fixture:"), 1)
	if _, err := loadLegacySQLiteRecoveryDocument(unknownField); err == nil {
		t.Fatal("legacy SQLite recovery fixture accepted an unknown field mutation")
	}
	wrongCount := bytes.Replace(legacySQLiteRecoveryYAML, []byte("declared_recovery_cases: 5"), []byte("declared_recovery_cases: 4"), 1)
	if _, err := loadLegacySQLiteRecoveryDocument(wrongCount); err == nil {
		t.Fatal("legacy SQLite recovery fixture accepted an incorrect declared count")
	}
	duplicateName := bytes.Replace(legacySQLiteRecoveryYAML, []byte("name: unsupported-override-falls-through"), []byte("name: committed-wal-update"), 1)
	if _, err := loadLegacySQLiteRecoveryDocument(duplicateName); err == nil {
		t.Fatal("legacy SQLite recovery fixture accepted a duplicate case name")
	}
	unknownEnum := bytes.Replace(legacySQLiteRecoveryYAML, []byte("envelope_mutation: arbitrary_json"), []byte("envelope_mutation: unknown"), 1)
	if _, err := loadLegacySQLiteRecoveryDocument(unknownEnum); err == nil {
		t.Fatal("legacy SQLite recovery fixture accepted an unknown envelope mutation")
	}
}

func openMountedLegacyWALWriter(t testing.TB, path string) *sqlite.Conn {
	t.Helper()
	connection, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic mounted WAL writer: %v", err)
	}
	if err := sqlitex.ExecuteTransient(connection, "PRAGMA wal_autocheckpoint=0", nil); err != nil {
		_ = connection.Close()
		t.Fatalf("disable synthetic WAL auto-checkpoint: %v", err)
	}
	return connection
}

func closeMountedSQLiteConnection(t testing.TB, connection *sqlite.Conn, label string) {
	t.Helper()
	if err := connection.Close(); err != nil {
		t.Errorf("close %s: %v", label, err)
	}
}

func ensureMountedWALFile(t testing.TB, writer *sqlite.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(writer, "UPDATE message SET time_updated = time_updated WHERE id = 'msg_equal_a'", nil); err != nil {
		t.Fatalf("create synthetic committed WAL evidence before initial harvest: %v", err)
	}
}

func appendMountedLegacyWALRows(writer *sqlite.Conn, testCase legacySQLiteFreshnessCase) (err error) {
	endTransaction, err := sqlitex.ImmediateTransaction(writer)
	if err != nil {
		return err
	}
	defer endTransaction(&err)
	if err = sqlitex.ExecuteTransient(writer, "INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?1, ?2, ?3, ?4, ?5)", &sqlitex.ExecOptions{Args: []any{testCase.MessageID, testCase.TargetSession, testCase.MessageTimeCreated, testCase.MessageTimeUpdated, testCase.MessageData}}); err != nil {
		return err
	}
	if err = sqlitex.ExecuteTransient(writer, "INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?1, ?2, ?3, ?4, ?5, ?6)", &sqlitex.ExecOptions{Args: []any{testCase.PartID, testCase.MessageID, testCase.TargetSession, testCase.PartTimeCreated, testCase.PartTimeUpdated, testCase.PartData}}); err != nil {
		return err
	}
	// OpenCode moves the session's changed clock in the same commit as the new
	// content. Freshness is clock-first for a session that has a clock, so the
	// committed WAL update moves the target session's clock to the new content
	// time and re-ingests it.
	return sqlitex.ExecuteTransient(writer, "UPDATE session SET time_updated = ?1 WHERE id = ?2", &sqlitex.ExecOptions{Args: []any{testCase.MessageTimeUpdated, testCase.TargetSession}})
}

func copySyntheticDatabase(t testing.TB, source, destination string) {
	t.Helper()
	if err := os.WriteFile(destination, mustReadFile(t, source), 0o600); err != nil {
		t.Fatalf("copy synthetic candidate database: %v", err)
	}
}

func readOptionalSyntheticFile(t testing.TB, path string) ([]byte, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read optional synthetic source file %q: %v", path, err)
	}
	return data, true
}

func setSyntheticSQLiteContentModTime(t testing.TB, databasePath string, modified time.Time) {
	t.Helper()
	setSyntheticSourceModTime(t, databasePath, modified)
	if _, err := os.Stat(databasePath + "-wal"); err == nil {
		setSyntheticSourceModTime(t, databasePath+"-wal", modified)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect synthetic WAL before setting content time: %v", err)
	}
}

func replaceMountedQuestion(t testing.TB, path, replacement string) {
	t.Helper()
	connection, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic channel mutation database: %v", err)
	}
	data, marshalErr := json.Marshal(map[string]any{"type": "text", "text": replacement, "time": map[string]int64{"created": 1_700_000_000_001}})
	updateErr := sqlitex.ExecuteTransient(connection, "UPDATE part SET data = ?1 WHERE id = 'part_user_text'", &sqlitex.ExecOptions{Args: []any{string(data)}})
	closeErr := connection.Close()
	if marshalErr != nil || updateErr != nil || closeErr != nil {
		t.Fatalf("mutate later synthetic channel candidate: %v", errors.Join(marshalErr, updateErr, closeErr))
	}
}

func rewriteMountedSessionIDs(t testing.TB, path string, originals, replacements []string) {
	t.Helper()
	connection, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic override session rewrite database: %v", err)
	}
	for index, original := range originals {
		options := &sqlitex.ExecOptions{Args: []any{replacements[index], original}}
		if err := sqlitex.ExecuteTransient(connection, "UPDATE message SET session_id = ?1 WHERE session_id = ?2", options); err != nil {
			_ = connection.Close()
			t.Fatalf("rewrite synthetic override message session IDs: %v", err)
		}
		if err := sqlitex.ExecuteTransient(connection, "UPDATE part SET session_id = ?1 WHERE session_id = ?2", options); err != nil {
			_ = connection.Close()
			t.Fatalf("rewrite synthetic override part session IDs: %v", err)
		}
		// The session table is OpenCode's authoritative session list, so rewrite
		// its identifier too. Otherwise the rewritten sessions have no session
		// row and discovery treats them as deleted.
		if err := sqlitex.ExecuteTransient(connection, "UPDATE session SET id = ?1 WHERE id = ?2", options); err != nil {
			_ = connection.Close()
			t.Fatalf("rewrite synthetic override session table IDs: %v", err)
		}
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close synthetic override session rewrite database: %v", err)
	}
}

func discoveredSessionStrings(sessions []ingest.DiscoveredSession) []string {
	result := make([]string, len(sessions))
	for index, session := range sessions {
		result[index] = string(session.SessionID)
	}
	sort.Strings(result)
	return result
}

func listingSessionStrings(listings []ftue.SessionListing) []string {
	result := make([]string, len(listings))
	for index, listing := range listings {
		result[index] = listing.SessionID
	}
	sort.Strings(result)
	return result
}

func storedSessionStrings(rows []store.SessionRow) []string {
	result := make([]string, len(rows))
	for index, row := range rows {
		result[index] = string(row.SessionID)
	}
	sort.Strings(result)
	return result
}

func managedTranscriptSessionIDs(t testing.TB, root string) []string {
	t.Helper()
	var result []string
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "--transcript.json") {
			return nil
		}
		result = append(result, strings.TrimSuffix(entry.Name(), "--transcript.json"))
		return nil
	})
	if err != nil {
		t.Fatalf("list managed candidate artifacts: %v", err)
	}
	sort.Strings(result)
	return result
}

func markMountedIndexStaleAndEmpty(t testing.TB, databasePath, sessionID string) {
	t.Helper()
	connection, err := sqlite.OpenConn(databasePath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open mounted stale-index control: %v", err)
	}
	options := &sqlitex.ExecOptions{Args: []any{sessionID}}
	if err := sqlitex.ExecuteTransient(connection, "DELETE FROM session_entries_ext WHERE session_id = ?1", options); err != nil {
		_ = connection.Close()
		t.Fatalf("clear mounted extended entries for stale-index recovery: %v", err)
	}
	if err := sqlitex.ExecuteTransient(connection, "DELETE FROM session_entries WHERE session_id = ?1", options); err != nil {
		_ = connection.Close()
		t.Fatalf("clear mounted entries for stale-index recovery: %v", err)
	}
	if err := sqlitex.ExecuteTransient(connection, "DELETE FROM session_metrics WHERE session_id = ?1", options); err != nil {
		_ = connection.Close()
		t.Fatalf("clear mounted metrics for stale-index recovery: %v", err)
	}
	if err := sqlitex.ExecuteTransient(connection, "UPDATE sessions SET index_version = 0 WHERE session_id = ?1", options); err != nil {
		_ = connection.Close()
		t.Fatalf("mark mounted index stale for recovery: %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close mounted stale-index control: %v", err)
	}
}

func mutateManagedEnvelope(t testing.TB, managedPath string, mutation legacySQLiteEnvelopeMutation) {
	t.Helper()
	if mutation == legacySQLiteEnvelopeNone {
		return
	}
	data := mustReadFile(t, managedPath)
	switch mutation {
	case legacySQLiteEnvelopeArbitrary:
		data = []byte("{\"ordinary\":true}\n")
	case legacySQLiteEnvelopeVersion:
		data = bytes.Replace(data, []byte("\"version\":2"), []byte("\"version\":9"), 1)
	case legacySQLiteEnvelopeKind:
		data = bytes.Replace(data, []byte("peasant.opencode.legacy-sqlite"), []byte("peasant.opencode.other-source"), 1)
	}
	if err := os.WriteFile(managedPath, data, 0o600); err != nil {
		t.Fatalf("mutate managed recovery envelope: %v", err)
	}
}
