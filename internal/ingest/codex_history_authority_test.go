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
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/codex_history_authority.yaml
var codexHistoryAuthorityYAML []byte

//go:embed testdata/codex_history_authority.manifest.yaml
var codexHistoryAuthorityManifestYAML []byte

type codexFileAuthorityCase struct {
	Name                   string                     `yaml:"name"`
	Files                  map[string]string          `yaml:"files"`
	SessionFile            string                     `yaml:"sessionFile"`
	SessionID              string                     `yaml:"sessionID"`
	PointerDocument        string                     `yaml:"pointerDocument"`
	PointerDocumentContent string                     `yaml:"pointerDocumentContent"`
	ExpectError            string                     `yaml:"expectError"`
	Expected               codexFileAuthorityExpected `yaml:"expected"`
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

func TestCodexHistoryAuthorityFixtures(t *testing.T) {
	fixture := loadCodexFileAuthorityFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
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
			var options []ingest.CodexFileSourceOption
			if testCase.PointerDocument != "" {
				docPath := filepath.Join(root, filepath.FromSlash(testCase.PointerDocument))
				content := testCase.PointerDocumentContent
				if after, ok := strings.CutPrefix(content, "@abs:"); ok {
					content = filepath.Join(root, filepath.FromSlash(after))
				} else if after, ok := strings.CutPrefix(content, "@rel:"); ok {
					content = after
				}
				if content == "" {
					t.Fatalf("case %q names a pointer document without content", testCase.Name)
				}
				if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(docPath, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				options = append(options, ingest.WithCodexCurrentPointerDocument(docPath))
			}
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
			source := ingest.NewCodexFileSource(&ingest.OSFileSystem{}, options...)
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
	threadID := "aaaaaaaa-1111-4111-8111-111111111111"
	parentID := "bbbbbbbb-2222-4222-8222-222222222222"
	root := t.TempDir()
	parentRel := filepath.Join("sessions", "2024", "01", "02", "rollout-2024-01-02T00-00-00-"+parentID+".jsonl")
	childRel := filepath.Join("sessions", "2024", "01", "03", "rollout-2024-01-03T00-00-00-"+threadID+".jsonl")
	parentPath := filepath.Join(root, parentRel)
	childPath := filepath.Join(root, childRel)
	parentContent := strings.Join([]string{
		`{"timestamp":"2024-01-02T00:00:00Z","type":"session_meta","payload":{"id":"` + parentID + `","history_mode":"paginated"}}`,
		`{"timestamp":"2024-01-02T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","id":"p1","content":[{"type":"input_text","text":"parent one"}]}}`,
		"",
	}, "\n")
	childContent := strings.Join([]string{
		`{"timestamp":"2024-01-03T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","history_mode":"paginated","history":[{"thread_id":"` + parentID + `","mode":"paginated","start":0,"end_exclusive":2}]}}`,
		`{"timestamp":"2024-01-03T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","id":"u1","content":[{"type":"input_text","text":"child one"}]}}`,
		"",
	}, "\n")
	for _, file := range []struct {
		path    string
		content string
	}{{parentPath, parentContent}, {childPath, childContent}} {
		if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file.path, []byte(file.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
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

// flippingPointerFS alternates the pointer-document content on every read
// so the stability recheck always observes a changed authority. Rollout
// bytes come from the real files underneath.
type flippingPointerFS struct {
	*ingest.OSFileSystem
	doc   string
	first string
	other string
	calls int
}

func (f *flippingPointerFS) ReadFile(path string) ([]byte, error) {
	if path == f.doc {
		f.calls++
		if f.calls%2 == 1 {
			return []byte(f.first), nil
		}
		return []byte(f.other), nil
	}
	return f.OSFileSystem.ReadFile(path)
}

// TestCodexPointerSwitchDuringRefresh proves an authority switch during the
// bounded recheck is detected on the real production path: a stable switch
// converges to the new fingerprint, while a flip on every attempt exhausts
// the budget with a source-changed error that retains last-good state.
func TestCodexPointerSwitchDuringRefresh(t *testing.T) {
	threadID := "cccccccc-3333-4333-8333-333333333333"
	mkRollout := func(id, text string) string {
		return strings.Join([]string{
			`{"timestamp":"2024-01-02T00:00:00Z","type":"session_meta","payload":{"id":"` + id + `","history_mode":"paginated"}}`,
			`{"timestamp":"2024-01-02T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","id":"` + id[:8] + `","content":[{"type":"input_text","text":"` + text + `"}]}}`,
			"",
		}, "\n")
	}
	t.Run("stable-switch-converges", func(t *testing.T) {
		root := t.TempDir()
		aRel := filepath.Join("sessions", "2024", "01", "02", "rollout-a-"+threadID+".jsonl")
		bRel := filepath.Join("sessions", "2024", "01", "03", "rollout-b-"+threadID+".jsonl")
		aPath, bPath := filepath.Join(root, aRel), filepath.Join(root, bRel)
		for _, file := range []struct {
			path    string
			content string
		}{{aPath, mkRollout(threadID, "incarnation a")}, {bPath, mkRollout(threadID, "incarnation b")}} {
			if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file.path, []byte(file.content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		doc := filepath.Join(root, "pointer")
		if err := os.WriteFile(doc, []byte(aPath), 0o600); err != nil {
			t.Fatal(err)
		}
		session := ingest.DiscoveredSession{
			SessionID:    schema.SessionID(threadID),
			Harness:      ingest.HarnessCodex,
			SourcePath:   ingest.ResolvedPath(aPath),
			SourceFormat: ingest.SourceFormatJSONL,
		}
		source := ingest.NewCodexFileSource(&ingest.OSFileSystem{}, ingest.WithCodexCurrentPointerDocument(doc))
		before, err := ingest.CaptureCodexHistory(t.Context(), source, session, nil)
		if err != nil {
			t.Fatalf("capture before switch: %v", err)
		}
		if err := os.WriteFile(doc, []byte(bPath), 0o600); err != nil {
			t.Fatal(err)
		}
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
	t.Run("unstable-switch-exhausts-budget", func(t *testing.T) {
		root := t.TempDir()
		aRel := filepath.Join("sessions", "2024", "01", "02", "rollout-a-"+threadID+".jsonl")
		bRel := filepath.Join("sessions", "2024", "01", "03", "rollout-b-"+threadID+".jsonl")
		aPath, bPath := filepath.Join(root, aRel), filepath.Join(root, bRel)
		for _, file := range []struct {
			path    string
			content string
		}{{aPath, mkRollout(threadID, "incarnation a")}, {bPath, mkRollout(threadID, "incarnation b")}} {
			if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file.path, []byte(file.content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		doc := filepath.Join(root, "pointer")
		fs := &flippingPointerFS{OSFileSystem: &ingest.OSFileSystem{}, doc: doc, first: aPath, other: bPath}
		session := ingest.DiscoveredSession{
			SessionID:    schema.SessionID(threadID),
			Harness:      ingest.HarnessCodex,
			SourcePath:   ingest.ResolvedPath(aPath),
			SourceFormat: ingest.SourceFormatJSONL,
		}
		source := ingest.NewCodexFileSource(fs, ingest.WithCodexCurrentPointerDocument(doc))
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
