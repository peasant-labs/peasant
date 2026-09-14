package ingest_test

// Fixture-backed validation for production Codex authority derivation. Every
// case materializes sanitized native rollout layouts into a real temporary
// directory and drives the production read-only source
// (ingest.NewCodexFileSource) through ingest.CaptureCodexHistory: native
// current-pointer selection, stale rollouts, missing currents, competing
// detached candidates, copied boundaries and ordered native references are
// all derived from real file bytes, never from prepared authority claims.

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/codex_history_authority.yaml
var codexHistoryAuthorityYAML []byte

//go:embed testdata/codex_history_authority.manifest.yaml
var codexHistoryAuthorityManifestYAML []byte

//go:embed testdata/codex_history_authority_layouts.yaml
var codexHistoryAuthorityLayoutsYAML []byte

//go:embed testdata/codex_history_authority_layouts.manifest.yaml
var codexHistoryAuthorityLayoutsManifestYAML []byte

type codexNativePointerRow struct {
	ID          string `yaml:"id"`
	RolloutPath string `yaml:"rolloutPath"`
	HistoryMode string `yaml:"historyMode"`
}

type codexFileAuthorityCase struct {
	Name              string                     `yaml:"name"`
	Files             map[string]string          `yaml:"files"`
	SessionFile       string                     `yaml:"sessionFile"`
	SessionID         string                     `yaml:"sessionID"`
	NativePointerDB   string                     `yaml:"nativePointerDatabase"`
	NativePointerRows []codexNativePointerRow    `yaml:"nativePointerRows"`
	ExpectError       string                     `yaml:"expectError"`
	Expected          codexFileAuthorityExpected `yaml:"expected"`
}

type codexFileAuthorityExpected struct {
	Kind                 string `yaml:"kind"`
	StableThreadID       string `yaml:"stableThreadID"`
	codexHistoryExpected `yaml:",inline"`
}

type codexFileAuthorityFixture struct {
	Cases []codexFileAuthorityCase `yaml:"cases"`
}

func decodeCodexFileAuthorityFixture(data []byte) (codexFileAuthorityFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture codexFileAuthorityFixture
	if err := decoder.Decode(&fixture); err != nil {
		return codexFileAuthorityFixture{}, fmt.Errorf("decode codex history authority fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return codexFileAuthorityFixture{}, fmt.Errorf("codex history authority fixture must contain exactly one YAML document: %v", err)
	}
	for _, c := range fixture.Cases {
		if strings.TrimSpace(c.Name) == "" {
			return codexFileAuthorityFixture{}, fmt.Errorf("codex history authority fixture has an empty case name")
		}
		if c.SessionFile == "" || c.SessionID == "" {
			return codexFileAuthorityFixture{}, fmt.Errorf("codex history authority case %q has no session file or id", c.Name)
		}
	}
	return fixture, nil
}

func loadCodexFileAuthorityFixture(t *testing.T) codexFileAuthorityFixture {
	t.Helper()
	fixture, err := decodeCodexFileAuthorityFixture(codexHistoryAuthorityYAML)
	if err != nil {
		t.Fatalf("load codex history authority fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(codexHistoryAuthorityManifestYAML, "codex history authority")
	if err != nil {
		t.Fatalf("load codex history authority manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "codex history authority"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// codexAuthorityLayout is one named paired-rollout layout: the real native
// files a production-path test materializes into a temporary directory. The
// session file is the recovered session and the referenced file is the other
// rollout of the pair (the retained parent or the previous pointer
// incarnation), so scenario tests never carry inline path/content tables.
type codexAuthorityLayout struct {
	Name           string            `yaml:"name"`
	SessionID      string            `yaml:"sessionID"`
	SessionFile    string            `yaml:"sessionFile"`
	ReferencedFile string            `yaml:"referencedFile"`
	Files          map[string]string `yaml:"files"`
}

type codexAuthorityLayoutFixture struct {
	Layouts []codexAuthorityLayout `yaml:"layouts"`
}

func decodeCodexAuthorityLayouts(data []byte) (codexAuthorityLayoutFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture codexAuthorityLayoutFixture
	if err := decoder.Decode(&fixture); err != nil {
		return codexAuthorityLayoutFixture{}, fmt.Errorf("decode codex history authority layouts: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return codexAuthorityLayoutFixture{}, fmt.Errorf("codex history authority layouts must contain exactly one YAML document: %v", err)
	}
	for _, layout := range fixture.Layouts {
		if strings.TrimSpace(layout.Name) == "" {
			return codexAuthorityLayoutFixture{}, fmt.Errorf("codex history authority layout has an empty name")
		}
		if layout.SessionFile == "" || layout.SessionID == "" || layout.ReferencedFile == "" {
			return codexAuthorityLayoutFixture{}, fmt.Errorf("codex history authority layout %q is missing a session id, session file or referenced file", layout.Name)
		}
		for _, role := range []string{layout.SessionFile, layout.ReferencedFile} {
			if _, ok := layout.Files[role]; !ok {
				return codexAuthorityLayoutFixture{}, fmt.Errorf("codex history authority layout %q declares no content for %q", layout.Name, role)
			}
		}
	}
	return fixture, nil
}

func loadCodexAuthorityLayoutFixture(t *testing.T) codexAuthorityLayoutFixture {
	t.Helper()
	fixture, err := decodeCodexAuthorityLayouts(codexHistoryAuthorityLayoutsYAML)
	if err != nil {
		t.Fatalf("load codex history authority layouts: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(codexHistoryAuthorityLayoutsManifestYAML, "codex history authority layouts")
	if err != nil {
		t.Fatalf("load codex history authority layouts manifest: %v", err)
	}
	names := make([]string, 0, len(fixture.Layouts))
	for _, layout := range fixture.Layouts {
		names = append(names, layout.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "codex history authority layouts"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func codexAuthorityLayoutByName(t *testing.T, fixture codexAuthorityLayoutFixture, name string) codexAuthorityLayout {
	t.Helper()
	for _, layout := range fixture.Layouts {
		if layout.Name == name {
			return layout
		}
	}
	t.Fatalf("codex history authority layouts have no layout %q", name)
	return codexAuthorityLayout{}
}

// writeCodexAuthorityLayout writes every declared native file of one named
// layout under root and returns the absolute path of each file. It is a
// materializer for a real temp filesystem, not a replacement for the
// production path the caller drives with NewCodexFileSource/OSFileSystem.
func writeCodexAuthorityLayout(t *testing.T, root string, layout codexAuthorityLayout) map[string]string {
	t.Helper()
	abs := make(map[string]string, len(layout.Files))
	for rel, content := range layout.Files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		abs[rel] = path
	}
	return abs
}

// writeCodexNativeStateDB materializes a real temporary native Codex state
// database with the production threads columns so the read-only native pointer
// authority is exercised against real SQLite storage, never a plaintext side
// document.
func writeCodexNativeStateDB(t *testing.T, root, dbPath string, rows []codexNativePointerRow) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open native state database: %v", err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn, "CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL, history_mode TEXT NOT NULL DEFAULT 'legacy')", nil); err != nil {
		t.Fatalf("create native threads table: %v", err)
	}
	for _, row := range rows {
		rollout := row.RolloutPath
		if !filepath.IsAbs(rollout) {
			rollout = filepath.Join(root, filepath.FromSlash(rollout))
		}
		mode := row.HistoryMode
		if mode == "" {
			mode = "legacy"
		}
		if err := sqlitex.ExecuteTransient(conn, "INSERT INTO threads (id, rollout_path, history_mode) VALUES (?, ?, ?)", &sqlitex.ExecOptions{Args: []any{row.ID, rollout, mode}}); err != nil {
			t.Fatalf("insert native thread row: %v", err)
		}
	}
}

// updateCodexNativeStateDB switches the native current-rollout row for one
// thread in a real temporary state database, modelling the native writer
// moving its pointer between captures.
func updateCodexNativeStateDB(t *testing.T, root, dbPath, stableThreadID, rolloutPath string) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open native state database: %v", err)
	}
	defer conn.Close()
	rollout := rolloutPath
	if !filepath.IsAbs(rollout) {
		rollout = filepath.Join(root, filepath.FromSlash(rollout))
	}
	if err := sqlitex.ExecuteTransient(conn, "UPDATE threads SET rollout_path = ? WHERE id = ?", &sqlitex.ExecOptions{Args: []any{rollout, stableThreadID}}); err != nil {
		t.Fatalf("update native thread row: %v", err)
	}
}

// materializeCodexAuthorityCase writes every declared native rollout of one
// authority case into a real temporary directory, builds the native pointer
// database when the case declares one, and returns the root. The production
// path consumes the root through NewCodexFileSource/OSFileSystem.
func materializeCodexAuthorityCase(t *testing.T, testCase codexFileAuthorityCase) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range testCase.Files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if testCase.NativePointerDB != "" {
		dbPath := filepath.Join(root, filepath.FromSlash(testCase.NativePointerDB))
		writeCodexNativeStateDB(t, root, dbPath, testCase.NativePointerRows)
	}
	return root
}

func codexFileAuthorityCaseByName(t *testing.T, fixture codexFileAuthorityFixture, name string) codexFileAuthorityCase {
	t.Helper()
	for _, testCase := range fixture.Cases {
		if testCase.Name == name {
			return testCase
		}
	}
	t.Fatalf("codex history authority fixture has no case %q", name)
	return codexFileAuthorityCase{}
}

func TestCodexHistoryAuthorityFixtures(t *testing.T) {
	fixture := loadCodexFileAuthorityFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			root := materializeCodexAuthorityCase(t, testCase)
			sessionID, err := ingest.NewSessionID(testCase.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			session := ingest.DiscoveredSession{
				SessionID:    sessionID,
				Harness:      ingest.HarnessCodex,
				SourcePath:   ingest.ResolvedPath(filepath.Join(root, filepath.FromSlash(testCase.SessionFile))),
				SourceFormat: ingest.SourceFormatJSONL,
			}
			allocator := func(index int) schema.SourceEntryRef {
				return schema.SourceEntryRef(fmt.Sprintf("e_%d", index+1))
			}
			source := ingest.NewCodexFileSource(&ingest.OSFileSystem{})
			history, err := ingest.CaptureCodexHistory(t.Context(), source, session, ingest.NewCodexRefRegistry(allocator))
			if testCase.ExpectError != "" {
				if err == nil {
					t.Fatalf("CaptureCodexHistory error = nil, want %s", testCase.ExpectError)
				}
				switch testCase.ExpectError {
				case "codex_current_missing":
					var missing *ingest.CodexCurrentMissingError
					if !errors.As(err, &missing) {
						t.Fatalf("CaptureCodexHistory error = %v, want *CodexCurrentMissingError", err)
					}
				case "codex_authority_conflict":
					var conflict *ingest.CodexAuthorityConflictError
					if !errors.As(err, &conflict) {
						t.Fatalf("CaptureCodexHistory error = %v, want *CodexAuthorityConflictError", err)
					}
					if conflict.Candidates != 2 {
						t.Fatalf("conflict candidates = %d, want 2", conflict.Candidates)
					}
				default:
					t.Fatalf("unknown expectError %q", testCase.ExpectError)
				}
				if strings.Contains(err.Error(), root) {
					t.Fatalf("authority error leaks the raw private path: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CaptureCodexHistory: %v", err)
			}
			if string(history.AuthorityKind) != testCase.Expected.Kind {
				t.Errorf("authority kind = %q, want %q", history.AuthorityKind, testCase.Expected.Kind)
			}
			if history.StableThreadID != testCase.Expected.StableThreadID {
				t.Errorf("stable thread = %q, want %q", history.StableThreadID, testCase.Expected.StableThreadID)
			}
			codexAssertExpected(t, testCase.Expected.codexHistoryExpected, history)
		})
	}
}

// TestCodexFileAuthorityReadOnlyAndStable proves the production capture is
// read-only and stable across turns and repeated publishes: native bytes are
// byte-identical before and after capture, a parent append past the captured
// cutoff leaves the child fingerprint unchanged, a current append with a
// shared registry keeps surviving native identities on their refs, and a
// pointer switch is detected as a changed fingerprint.
func TestCodexFileAuthorityReadOnlyAndStable(t *testing.T) {
	layout := codexAuthorityLayoutByName(t, loadCodexAuthorityLayoutFixture(t), "parent-child-retention")
	root := t.TempDir()
	files := writeCodexAuthorityLayout(t, root, layout)
	threadID := layout.SessionID
	parentPath := files[layout.ReferencedFile]
	childPath := files[layout.SessionFile]
	snapshot := func(path string) []byte {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	parentBefore, childBefore := snapshot(parentPath), snapshot(childPath)
	sessionID, err := ingest.NewSessionID(threadID)
	if err != nil {
		t.Fatal(err)
	}
	session := ingest.DiscoveredSession{
		SessionID:    sessionID,
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(childPath),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	allocator := func(index int) schema.SourceEntryRef {
		return schema.SourceEntryRef(fmt.Sprintf("e_%d", index+1))
	}
	registry := ingest.NewCodexRefRegistry(allocator)
	source := ingest.NewCodexFileSource(&ingest.OSFileSystem{})
	first, err := ingest.CaptureCodexHistory(t.Context(), source, session, registry)
	if err != nil {
		t.Fatalf("first CaptureCodexHistory: %v", err)
	}
	if !bytes.Equal(snapshot(parentPath), parentBefore) || !bytes.Equal(snapshot(childPath), childBefore) {
		t.Fatal("production capture mutated native source bytes")
	}
	if len(first.InheritedRefs) != 1 || len(first.MainRefs) != 1 {
		t.Fatalf("inherited = %d, main = %d, want 1 and 1", len(first.InheritedRefs), len(first.MainRefs))
	}
	survivingParent, survivingChild := first.InheritedRefs[0], first.MainRefs[0]

	appendLine := func(path, line string) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, []byte(line+"\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	appendLine(parentPath, `{"timestamp":"2024-01-02T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","id":"p2","content":[{"type":"input_text","text":"parent append past cutoff"}]}}`)
	appendLine(childPath, `{"timestamp":"2024-01-03T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"a2","content":[{"type":"output_text","text":"child two"}]}}`)

	second, err := ingest.CaptureCodexHistory(t.Context(), source, session, registry)
	if err != nil {
		t.Fatalf("second CaptureCodexHistory: %v", err)
	}
	if second.Fingerprint == first.Fingerprint {
		t.Fatal("child fingerprint is unchanged after a current append; the capture missed new native bytes")
	}
	if len(second.InheritedRefs) != 1 || second.InheritedRefs[0] != survivingParent {
		t.Fatalf("parent append changed the inherited refs: %v, want [%s]", second.InheritedRefs, survivingParent)
	}
	if len(second.MainRefs) != 2 || second.MainRefs[0] != survivingChild {
		t.Fatalf("repeated publish rekeyed surviving native identities: %v, want first %s", second.MainRefs, survivingChild)
	}
	if len(second.CapturedSegments) != 2 || len(second.CapturedSegments[0].Data) == 0 || len(second.CapturedSegments[1].Data) == 0 {
		t.Fatal("captured segments do not carry every segment payload for the classifier")
	}
}

// countingCodexFS records which read methods the indexer path uses. The
// retained file path must perform exactly one full read and no bounded
// capture reads.
type countingCodexFS struct {
	*ingest.OSFileSystem
	reads       int
	prefixReads int
	headerReads int
}

func (f *countingCodexFS) ReadFile(path string) ([]byte, error) {
	f.reads++
	return f.OSFileSystem.ReadFile(path)
}

func (f *countingCodexFS) ReadSourcePrefix(path string) ([]byte, error) {
	f.prefixReads++
	return f.OSFileSystem.ReadSourcePrefix(path)
}

func (f *countingCodexFS) ReadFileHeader(path string, limit int) ([]byte, error) {
	f.headerReads++
	return f.OSFileSystem.ReadFileHeader(path, limit)
}

// TestCodexIndexerRetainedV1SingleRead proves the default indexer keeps its
// exact retained behavior: one file read, no authority selection, no capture
// reads, no refusal, including an active-writer partial tail.
func TestCodexIndexerRetainedV1SingleRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2024-01-02T00-00-00-11111111-1111-4111-8111-111111111111.jsonl")
	content := strings.Join([]string{
		`{"timestamp":"2024-01-02T00:00:00Z","type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111","history_mode":"paginated"}}`,
		`{"timestamp":"2024-01-02T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","id":"m1","content":[{"type":"input_text","text":"hello"}]}}`,
		`{"timestamp":"2024-01-02T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"m2","content":[{"type":"output_text","text":"hi"}]}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	session := ingest.DiscoveredSession{
		SessionID:    schema.SessionID("11111111-1111-4111-8111-111111111111"),
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(path),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	fs := &countingCodexFS{OSFileSystem: &ingest.OSFileSystem{}}
	indexer := ingest.NewCodexIndexer(fs)
	result, err := indexer.IndexTranscriptResult(t.Context(), session)
	if err != nil {
		t.Fatalf("retained IndexTranscriptResult: %v", err)
	}
	if entries := result.(indexformat.V1).Entries; len(entries) != 2 {
		t.Fatalf("retained entries = %d, want 2 (exact retained parse, no capture refusal)", len(entries))
	}
	if fs.reads != 1 || fs.prefixReads != 0 || fs.headerReads != 0 {
		t.Fatalf("retained reads = file:%d prefix:%d header:%d, want 1/0/0 (no capture reads)", fs.reads, fs.prefixReads, fs.headerReads)
	}
}

// TestCodexIndexerCaptureEnabledRefusesIncomplete proves the enabled capture
// path refuses an incomplete capture (missing reference proof) instead of
// replacing last-good entries with an uncertified result.
func TestCodexIndexerCaptureEnabledRefusesIncomplete(t *testing.T) {
	threadID := "99990000-9999-4999-8999-999999999999"
	root := t.TempDir()
	childRel := filepath.Join("sessions", "2024", "01", "03", "rollout-2024-01-03T00-00-00-"+threadID+".jsonl")
	childPath := filepath.Join(root, childRel)
	childContent := strings.Join([]string{
		`{"timestamp":"2024-01-03T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","history_mode":"paginated","history":[{"thread_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","mode":"paginated","start":0,"end_exclusive":2}]}}`,
		`{"timestamp":"2024-01-03T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","id":"own1","content":[{"type":"input_text","text":"child own"}]}}`,
		"",
	}, "\n")
	if err := os.MkdirAll(filepath.Dir(childPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte(childContent), 0o600); err != nil {
		t.Fatal(err)
	}
	session := ingest.DiscoveredSession{
		SessionID:    schema.SessionID(threadID),
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(childPath),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	indexer := ingest.NewCodexIndexer(&ingest.OSFileSystem{}, ingest.WithCodexHistoryCapture(true))
	result, err := indexer.IndexTranscriptResult(t.Context(), session)
	if err == nil {
		t.Fatalf("capture-enabled IndexTranscriptResult = %v, want refusal of the incomplete capture", result)
	}
	var incomplete *ingest.CodexIncompleteCaptureError
	if !errors.As(err, &incomplete) {
		t.Fatalf("capture-enabled error = %v, want *CodexIncompleteCaptureError", err)
	}
	if strings.Contains(incomplete.Error(), root) {
		t.Fatalf("incomplete-capture error leaks the raw private path: %v", incomplete)
	}
}

// TestCodexIndexerCaptureEnabledRefusesNestedMalformed drives the real enabled
// indexer capture path over a recursively referenced malformed history
// envelope. The referenced envelope decodes to an empty reference list, so
// without the referenced-envelope validation the parent would certify complete;
// the derivation must instead be incomplete_new with the malformed-history
// diagnostic, and the enabled indexer must refuse and retain last-good state.
func TestCodexIndexerCaptureEnabledRefusesNestedMalformed(t *testing.T) {
	fixture := loadCodexFileAuthorityFixture(t)
	testCase := codexFileAuthorityCaseByName(t, fixture, "native-nested-malformed-incomplete")
	root := materializeCodexAuthorityCase(t, testCase)
	session := ingest.DiscoveredSession{
		SessionID:    schema.SessionID(testCase.SessionID),
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(filepath.Join(root, filepath.FromSlash(testCase.SessionFile))),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	source := ingest.NewCodexFileSource(&ingest.OSFileSystem{})
	capture, err := ingest.CaptureCodexHistory(t.Context(), source, session, ingest.NewCodexRefRegistry(nil))
	if err != nil {
		t.Fatalf("CaptureCodexHistory: %v", err)
	}
	if capture.Completeness != indexformat.GenerationCompletenessIncompleteNew {
		t.Fatalf("nested-malformed completeness = %q, want incomplete_new (recursive malformed history must never certify complete)", capture.Completeness)
	}
	if got := codexDiagnosticTypes(capture.Diagnostics); !slices.Contains(got, "codex_history_envelope_malformed") {
		t.Fatalf("nested-malformed diagnostics = %v, want codex_history_envelope_malformed", got)
	}
	indexer := ingest.NewCodexIndexer(&ingest.OSFileSystem{}, ingest.WithCodexHistoryCapture(true))
	result, err := indexer.IndexTranscriptResult(t.Context(), session)
	if err == nil {
		t.Fatalf("enabled IndexTranscriptResult = %v, want refusal of the nested-malformed capture", result)
	}
	var incomplete *ingest.CodexIncompleteCaptureError
	if !errors.As(err, &incomplete) {
		t.Fatalf("nested-malformed error = %v, want *CodexIncompleteCaptureError", err)
	}
	if len(incomplete.Diagnostics) == 0 {
		t.Fatal("nested-malformed refusal carries no diagnostics")
	}
	for _, marker := range []string{"last good", "repair"} {
		if !strings.Contains(incomplete.Error(), marker) {
			t.Fatalf("nested-malformed refusal = %v, want actionable marker %q", incomplete, marker)
		}
	}
	if strings.Contains(incomplete.Error(), root) {
		t.Fatalf("nested-malformed refusal leaks the raw private path: %v", incomplete)
	}
}

// TestCodexCaptureSentinelPathsSanitized proves missing and unreadable native
// sources fail through the real capture path with actionable errors that
// carry no raw private path or wrapped OS error text.
func TestCodexCaptureSentinelPathsSanitized(t *testing.T) {
	newSession := func(path string) ingest.DiscoveredSession {
		return ingest.DiscoveredSession{
			SessionID:    schema.SessionID("aaaaaaaa-1111-4111-8111-111111111111"),
			Harness:      ingest.HarnessCodex,
			SourcePath:   ingest.ResolvedPath(path),
			SourceFormat: ingest.SourceFormatJSONL,
		}
	}
	t.Run("missing", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "sessions", "2024", "01", "02", "rollout-2024-01-02T00-00-00-aaaaaaaa-1111-4111-8111-111111111111.jsonl")
		indexer := ingest.NewCodexIndexer(&ingest.OSFileSystem{}, ingest.WithCodexHistoryCapture(true))
		_, err := indexer.IndexTranscriptResult(t.Context(), newSession(missing))
		if err == nil {
			t.Fatal("missing source error = nil")
		}
		var currentMissing *ingest.CodexCurrentMissingError
		if !errors.As(err, &currentMissing) {
			t.Fatalf("missing source error = %v, want *CodexCurrentMissingError", err)
		}
		if strings.Contains(currentMissing.Error(), dir) {
			t.Fatalf("missing-source error leaks the raw private path: %v", currentMissing)
		}
		for _, marker := range []string{"ingest.CodexFileSource", "last good", "rerun"} {
			if !strings.Contains(currentMissing.Error(), marker) {
				t.Fatalf("missing-source error = %v, want actionable marker %q", currentMissing, marker)
			}
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("unreadable-file probe needs a non-root user")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "rollout-2024-01-02T00-00-00-aaaaaaaa-1111-4111-8111-111111111111.jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(path, 0o600)
		indexer := ingest.NewCodexIndexer(&ingest.OSFileSystem{}, ingest.WithCodexHistoryCapture(true))
		_, err := indexer.IndexTranscriptResult(t.Context(), newSession(path))
		if err == nil {
			t.Fatal("unreadable source error = nil")
		}
		var currentMissing *ingest.CodexCurrentMissingError
		if !errors.As(err, &currentMissing) {
			t.Fatalf("unreadable source error = %v, want *CodexCurrentMissingError", err)
		}
		if strings.Contains(currentMissing.Error(), dir) {
			t.Fatalf("unreadable-source error leaks the raw private path: %v", currentMissing)
		}
		if !strings.Contains(currentMissing.Error(), "permission denied") {
			t.Fatalf("unreadable-source error = %v, want the sanitized permission cause", currentMissing)
		}
	})
}

// alternatingNativePointerStore alternates the native current-rollout record on
// every call so the stability recheck always observes a changed authority. It
// is a dependency control for the read-only source, not a replacement for the
// capture.
type alternatingNativePointerStore struct {
	first  ingest.CodexNativePointerRecord
	second ingest.CodexNativePointerRecord
	calls  int
}

func (s *alternatingNativePointerStore) NativeCurrentRollout(context.Context, string) (ingest.CodexNativePointerRecord, bool, error) {
	s.calls++
	if s.calls%2 == 1 {
		return s.first, true, nil
	}
	return s.second, true, nil
}

// switchingNativePointerStore delegates to the real read-only SQLite store and
// performs a one-time native row switch on a chosen call, so the recheck itself
// resolves the new pointer and the capture can converge after an in-flight
// switch.
type switchingNativePointerStore struct {
	store        *ingest.CodexSQLitePointerStore
	switchOnCall int
	onSwitch     func()
	calls        int
}

func (s *switchingNativePointerStore) NativeCurrentRollout(ctx context.Context, stableThreadID string) (ingest.CodexNativePointerRecord, bool, error) {
	s.calls++
	if s.calls == s.switchOnCall && s.onSwitch != nil {
		s.onSwitch()
	}
	return s.store.NativeCurrentRollout(ctx, stableThreadID)
}

// writeCodexSwitchRollouts materializes the named pointer-switch layout, two
// native rollouts that share one stable thread, under root and returns the
// stable thread id plus the previous and current incarnation paths.
func writeCodexSwitchRollouts(t *testing.T, root string) (string, string, string) {
	t.Helper()
	layout := codexAuthorityLayoutByName(t, loadCodexAuthorityLayoutFixture(t), "pointer-switch-incarnations")
	files := writeCodexAuthorityLayout(t, root, layout)
	return layout.SessionID, files[layout.SessionFile], files[layout.ReferencedFile]
}

// TestCodexPointerSwitchDuringRefresh proves an authority switch on the real
// read-only native database is detected on the production path: a stable
// switch converges to the new fingerprint, a one-time switch during the recheck
// converges on the new pointer, and a flip on every attempt exhausts the budget
// with a source-changed error that retains last-good state.
func TestCodexPointerSwitchDuringRefresh(t *testing.T) {
	t.Run("stable-switch-converges", func(t *testing.T) {
		root := t.TempDir()
		threadID, aPath, bPath := writeCodexSwitchRollouts(t, root)
		dbPath := filepath.Join(root, "state_5.sqlite")
		writeCodexNativeStateDB(t, root, dbPath, []codexNativePointerRow{{ID: threadID, RolloutPath: aPath, HistoryMode: "paginated"}})
		session := ingest.DiscoveredSession{
			SessionID:    schema.SessionID(threadID),
			Harness:      ingest.HarnessCodex,
			SourcePath:   ingest.ResolvedPath(aPath),
			SourceFormat: ingest.SourceFormatJSONL,
		}
		source := ingest.NewCodexFileSource(&ingest.OSFileSystem{})
		before, err := ingest.CaptureCodexHistory(t.Context(), source, session, nil)
		if err != nil {
			t.Fatalf("capture before switch: %v", err)
		}
		updateCodexNativeStateDB(t, root, dbPath, threadID, bPath)
		after, err := ingest.CaptureCodexHistoryWithRetry(t.Context(), source, session, nil)
		if err != nil {
			t.Fatalf("capture after stable switch: %v", err)
		}
		if after.Fingerprint == before.Fingerprint {
			t.Fatal("pointer switch did not change the capture fingerprint")
		}
		if after.AuthorityKind != ingest.CodexAuthorityNativeCurrentPointer || after.Pointer != bPath {
			t.Fatalf("switched authority = %q %q, want native pointer at the new rollout", after.AuthorityKind, after.Pointer)
		}
	})
	t.Run("single-switch-during-recheck-converges", func(t *testing.T) {
		root := t.TempDir()
		threadID, aPath, bPath := writeCodexSwitchRollouts(t, root)
		dbPath := filepath.Join(root, "state_5.sqlite")
		writeCodexNativeStateDB(t, root, dbPath, []codexNativePointerRow{{ID: threadID, RolloutPath: aPath, HistoryMode: "paginated"}})
		session := ingest.DiscoveredSession{
			SessionID:    schema.SessionID(threadID),
			Harness:      ingest.HarnessCodex,
			SourcePath:   ingest.ResolvedPath(aPath),
			SourceFormat: ingest.SourceFormatJSONL,
		}
		store := &switchingNativePointerStore{
			store:        ingest.NewCodexSQLitePointerStore(dbPath),
			switchOnCall: 2,
			onSwitch: func() {
				updateCodexNativeStateDB(t, root, dbPath, threadID, bPath)
			},
		}
		source := ingest.NewCodexFileSource(&ingest.OSFileSystem{}, ingest.WithCodexNativePointerStore(store))
		after, err := ingest.CaptureCodexHistoryWithRetry(t.Context(), source, session, nil)
		if err != nil {
			t.Fatalf("capture after in-flight switch: %v", err)
		}
		if after.Pointer != bPath || after.AuthorityKind != ingest.CodexAuthorityNativeCurrentPointer {
			t.Fatalf("in-flight switch converged on %q %q, want the new native pointer %q", after.AuthorityKind, after.Pointer, bPath)
		}
		if len(after.MainRefs) != 1 {
			t.Fatalf("in-flight switch main refs = %v, want the second incarnation's own item", after.MainRefs)
		}
	})
	t.Run("unstable-switch-exhausts-budget", func(t *testing.T) {
		root := t.TempDir()
		threadID, aRelPath, bRelPath := writeCodexSwitchRollouts(t, root)
		session := ingest.DiscoveredSession{
			SessionID:    schema.SessionID(threadID),
			Harness:      ingest.HarnessCodex,
			SourcePath:   ingest.ResolvedPath(aRelPath),
			SourceFormat: ingest.SourceFormatJSONL,
		}
		store := &alternatingNativePointerStore{
			first:  ingest.CodexNativePointerRecord{Pointer: aRelPath},
			second: ingest.CodexNativePointerRecord{Pointer: bRelPath},
		}
		source := ingest.NewCodexFileSource(&ingest.OSFileSystem{}, ingest.WithCodexNativePointerStore(store))
		_, err := ingest.CaptureCodexHistoryWithRetry(t.Context(), source, session, nil)
		var changed *ingest.CodexSourceChangedError
		if !errors.As(err, &changed) {
			t.Fatalf("unstable switch error = %v, want *CodexSourceChangedError", err)
		}
		if changed.Attempts != 3 {
			t.Fatalf("unstable switch attempts = %d, want 3", changed.Attempts)
		}
	})
}

// TestCodexAuthoritativeSessionSeam drives the exported pre-diff selection
// seam against a real temporary native pointer database: the filename-derived
// discovered identity is superseded by the stable session_meta.id and the
// authoritative current source replaces the discovered (stale) path before any
// diff.
func TestCodexAuthoritativeSessionSeam(t *testing.T) {
	threadID := "dddddddd-4444-4444-8444-444444444444"
	root := t.TempDir()
	staleRel := filepath.Join("sessions", "2024", "01", "02", "rollout-2024-01-02T00-00-00-"+threadID+".jsonl")
	currentRel := filepath.Join("sessions", "2024", "01", "03", "rollout-2024-01-03T00-00-00-"+threadID+".jsonl")
	for rel, id := range map[string]string{staleRel: "stale-id", currentRel: threadID} {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		content := `{"timestamp":"2024-01-02T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","history_mode":"paginated"}}` + "\n" +
			`{"timestamp":"2024-01-02T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","id":"` + id + `","content":[{"type":"input_text","text":"seam"}]}}` + "\n"
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(root, "state_6.sqlite")
	writeCodexNativeStateDB(t, root, dbPath, []codexNativePointerRow{{ID: threadID, RolloutPath: currentRel, HistoryMode: "paginated"}})
	session := ingest.DiscoveredSession{
		SessionID:    schema.SessionID(threadID),
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(filepath.Join(root, staleRel)),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	source := ingest.NewCodexFileSource(&ingest.OSFileSystem{})
	identified, err := ingest.ResolveCodexAuthoritativeSession(t.Context(), source, session)
	if err != nil {
		t.Fatalf("ResolveCodexAuthoritativeSession: %v", err)
	}
	if identified.Authority.Kind != ingest.CodexAuthorityNativeCurrentPointer {
		t.Fatalf("authority kind = %q, want native current pointer", identified.Authority.Kind)
	}
	if identified.Session.SessionID != schema.SessionID(threadID) {
		t.Fatalf("authoritative session id = %q, want stable %q", identified.Session.SessionID, threadID)
	}
	wantPath := filepath.Join(root, currentRel)
	if identified.Session.SourcePath.String() != wantPath {
		t.Fatalf("authoritative source = %q, want current native rollout %q", identified.Session.SourcePath, wantPath)
	}
}

// TestCodexIndexerCaptureConsumesNativeCurrentSource drives the real indexer
// capture path against a real temporary native state database: the discovered
// (stale) rollout is superseded by the native current-rollout pointer before
// entry parsing, so the indexed entries come from the authoritative current
// incarnation and never from the stale discovered path.
func TestCodexIndexerCaptureConsumesNativeCurrentSource(t *testing.T) {
	threadID := "eeeeeeee-5555-4555-8555-555555555555"
	root := t.TempDir()
	staleRel := filepath.Join("sessions", "2024", "01", "02", "rollout-2024-01-02T00-00-00-"+threadID+".jsonl")
	currentRel := filepath.Join("sessions", "2024", "01", "03", "rollout-2024-01-03T00-00-00-"+threadID+".jsonl")
	stale := strings.Join([]string{
		`{"timestamp":"2024-01-02T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","history_mode":"paginated"}}`,
		`{"timestamp":"2024-01-02T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","id":"stale","content":[{"type":"input_text","text":"stale incarnation"}]}}`,
		"",
	}, "\n")
	current := strings.Join([]string{
		`{"timestamp":"2024-01-03T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","history_mode":"paginated"}}`,
		`{"timestamp":"2024-01-03T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","id":"new1","content":[{"type":"input_text","text":"current one"}]}}`,
		`{"timestamp":"2024-01-03T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"new2","content":[{"type":"output_text","text":"current two"}]}}`,
		"",
	}, "\n")
	for rel, content := range map[string]string{staleRel: stale, currentRel: current} {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(root, "state_5.sqlite")
	writeCodexNativeStateDB(t, root, dbPath, []codexNativePointerRow{{ID: threadID, RolloutPath: currentRel, HistoryMode: "paginated"}})
	session := ingest.DiscoveredSession{
		SessionID:    schema.SessionID(threadID),
		Harness:      ingest.HarnessCodex,
		SourcePath:   ingest.ResolvedPath(filepath.Join(root, staleRel)),
		SourceFormat: ingest.SourceFormatJSONL,
	}
	indexer := ingest.NewCodexIndexer(&ingest.OSFileSystem{}, ingest.WithCodexHistoryCapture(true))
	result, err := indexer.IndexTranscriptResult(t.Context(), session)
	if err != nil {
		t.Fatalf("IndexTranscriptResult: %v", err)
	}
	entries := result.(indexformat.V1).Entries
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 from the native current rollout", len(entries))
	}
	if entries[0].ContentPreview == nil || *entries[0].ContentPreview != "current one" {
		t.Fatalf("first entry preview = %v, want the native current incarnation", entries[0].ContentPreview)
	}
}
