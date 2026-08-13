package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	storepkg "github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/authoritative-receipt-mismatches.yaml
var authoritativeReceiptMismatchYAML []byte

type authoritativeReceiptMismatchDocument struct {
	ExpectedCaseCount int                                `yaml:"expectedCaseCount"`
	Cases             []authoritativeReceiptMismatchCase `yaml:"cases"`
}
type authoritativeReceiptMismatchCase struct {
	Name            string `yaml:"name"`
	Mismatch        string `yaml:"mismatch"`
	Replacement     string `yaml:"replacement"`
	ErrorContains   string `yaml:"errorContains"`
	ParentSessionID string `yaml:"parentSessionID,omitempty"`
}

func loadAuthoritativeReceiptMismatchCases(t *testing.T) []authoritativeReceiptMismatchCase {
	t.Helper()
	var doc authoritativeReceiptMismatchDocument
	decoder := yaml.NewDecoder(bytes.NewReader(authoritativeReceiptMismatchYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("receipt mismatch corpus must have exact EOF: %v", err)
	}
	if doc.ExpectedCaseCount != 3 || len(doc.Cases) != doc.ExpectedCaseCount {
		t.Fatalf("receipt mismatch rows=%d declared=%d", len(doc.Cases), doc.ExpectedCaseCount)
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || c.Replacement == "" || len(c.Replacement) != 64 || c.ErrorContains == "" || seen[c.Name] || (c.Mismatch != "content" && c.Mismatch != "fingerprint" && c.Mismatch != "parent-fingerprint") || (c.Mismatch == "parent-fingerprint" && c.ParentSessionID == "") {
			t.Fatalf("invalid receipt mismatch row: %+v", c)
		}
		seen[c.Name] = true
	}
	return doc.Cases
}

// baseConfig returns a minimal config.Config for pipeline tests.
func baseTestConfig() *config.Config {
	return &config.Config{
		Output: config.OutputConfig{BasePath: "/sync"},
		Push: config.PushConfig{
			Method:     config.PushMethodAll,
			Visibility: config.VisibilityPrivate,
		},
	}
}

// baseCreds returns minimal credentials for pipeline tests.
func baseCreds() *auth.Credentials {
	return &auth.Credentials{
		APIKey:     "key-abc",
		KeyID:      "kid-1",
		UserID:     "uid-1",
		Username:   "testuser",
		VillageURL: "https://village.example.com",
	}
}

// sessionOpt mutates a PushSessionRow fixture (functional-options pattern so
// existing 4-arg makeSession callers stay valid while branch-aware tests can
// set GitRemote/GitBranch/ProjectName without inlining a full struct).
type sessionOpt func(*ingest.PushSessionRow)

func withGitRemote(remote string) sessionOpt {
	return func(s *ingest.PushSessionRow) { s.GitRemote = remote }
}

func withGitBranch(branch string) sessionOpt {
	return func(s *ingest.PushSessionRow) { s.GitBranch = &branch }
}

func withProjectName(name string) sessionOpt {
	return func(s *ingest.PushSessionRow) { s.ProjectName = name }
}

// makeSession creates a PushSessionRow fixture.
// provider must be a typed constant string (e.g. string(defaults.HarnessClaudeCode)).
// pushedAt may be nil (never pushed) or a pointer to a unix-millis timestamp.
// Optional opts set branch-aware fields (GitRemote/GitBranch/ProjectName).
func makeSession(id, hostSlug, provider string, pushedAt *int64, opts ...sessionOpt) ingest.PushSessionRow {
	s := ingest.PushSessionRow{
		SessionID:      id,
		HostSlug:       hostSlug,
		ModelHarness:   provider,
		ModelID:        string(testutil.TestModel),
		ProjectName:    "myapp",
		ProjectHash:    string(testutil.TestProjectHash),
		StartMs:        1740312000000,
		EndMs:          1740312360000,
		IngestedMs:     1740312400000,
		PushedAt:       pushedAt,
		SourceFilePath: "/source/file.jsonl",
		SourceFormat:   string(ingest.SourceFormatJSONL),
		TurnCount:      5,
		ToolCalls:      3,
		InputTokens:    1000,
		OutputTokens:   500,
		TokensTotal:    1500,
		DurationMs:     360000,
	}
	for _, o := range opts {
		o(&s)
	}
	return s
}

func TestApplySelection(t *testing.T) {
	t.Parallel()
	cc := string(defaults.HarnessClaudeCode)
	remote := "git@github.com:u/r.git"

	rows := []ingest.PushSessionRow{
		makeSession("s-main", "h", cc, nil, withGitRemote(remote), withGitBranch("main")),                        // kept
		makeSession("s-feature", "h", cc, nil, withGitRemote(remote), withGitBranch("feature")),                  // dropped: branch not selected
		makeSession("s-other", "h", cc, nil, withGitRemote("git@github.com:other/x.git"), withGitBranch("main")), // dropped: project not selected
		makeSession("s-unknown", "h", cc, nil, withGitRemote(remote)),                                            // kept: unknown branch (conservative)
	}

	t.Run("nil selection keeps all", func(t *testing.T) {
		kept, withheld := push.ApplySelection(rows, nil)
		if len(kept) != len(rows) || len(withheld) != 0 {
			t.Fatalf("nil selection: kept=%d withheld=%d, want kept=%d withheld=0", len(kept), len(withheld), len(rows))
		}
	})

	t.Run("branch-aware filter", func(t *testing.T) {
		selection := push.NewSessionSelection(map[ingest.SessionID]ingest.BranchMatch{
			"s-main":    ingest.BranchMatchYes,
			"s-feature": ingest.BranchMatchNo,
			"s-other":   ingest.BranchMatchNo,
			"s-unknown": ingest.BranchMatchYes,
		})
		kept, withheld := push.ApplySelection(rows, selection)
		gotKept := map[string]bool{}
		for _, s := range kept {
			gotKept[s.SessionID] = true
		}
		if !gotKept["s-main"] || !gotKept["s-unknown"] {
			t.Errorf("expected s-main + s-unknown kept, got %v", gotKept)
		}
		if gotKept["s-feature"] || gotKept["s-other"] {
			t.Errorf("expected s-feature + s-other dropped, got %v", gotKept)
		}
		if len(withheld) != 0 {
			t.Errorf("expected no withheld (single-project), got %d", len(withheld))
		}
	})

	t.Run("multi-project conflict is withheld", func(t *testing.T) {
		conflict := []ingest.PushSessionRow{
			makeSession("s-conflict", "h", cc, nil, withGitRemote(remote), withProjectName("proj"), withGitBranch("main")),
		}
		selection := push.NewSessionSelection(map[ingest.SessionID]ingest.BranchMatch{
			"s-conflict": ingest.BranchMatchWithheldConflict,
		})
		kept, withheld := push.ApplySelection(conflict, selection)
		if len(kept) != 0 {
			t.Errorf("expected conflict session withheld (not kept), got %d kept", len(kept))
		}
		if len(withheld) != 1 || withheld[0].SessionID != "s-conflict" {
			t.Errorf("expected 1 withheld (s-conflict), got %v", withheld)
		}
	})
}

// seedMemFS creates the metadata.json and transcript.jsonl files for a session
// in the given MemFS under /sync/{hostSlug}/{sessionID}/.
// provider must be a typed constant string (e.g. string(defaults.HarnessClaudeCode)).
func seedMemFS(t *testing.T, fs *testutil.MemFS, hostSlug, sessionID string, provider defaults.Harness) {
	t.Helper()

	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = ingest.SessionID(sessionID)
	meta.ModelHarness = provider
	meta.Model = testutil.TestModel
	meta.Version = "2.1.47"
	ingested := int64(1740312400000)
	meta.Timestamp = ingest.TimestampInfo{
		Start:    1740312000000,
		End:      1740312360000,
		Ingested: &ingested,
	}
	meta.Project = ingest.ProjectInfo{
		Hash:     testutil.TestProjectHash,
		Name:     "myapp",
		FilePath: "/home/test/myapp",
	}
	meta.HostSlug = ingest.HostSlug(hostSlug)
	meta.Source = ingest.SourceInfo{
		Format:   ingest.SourceFormatJSONL,
		FilePath: "/source/file.jsonl",
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}

	dir := filepath.Join("/sync", hostSlug, sessionID)
	metaPath := filepath.Join(dir, sessionID+"--metadata.json")
	transcriptPath := filepath.Join(dir, sessionID+"--transcript.jsonl")

	if err := fs.WriteFile(metaPath, metaJSON, 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	if err := fs.WriteFile(transcriptPath, []byte(`{"type":"say"}`), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}

func plantRawPublicationIdentity(t *testing.T, fs *testutil.MemFS, hostSlug, sessionID string) (project, workdir, branch, remote string) {
	t.Helper()
	path := filepath.Join("/sync", hostSlug, sessionID, sessionID+"--metadata.json")
	raw, err := fs.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded publication metadata: %v", err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode seeded publication metadata: %v", err)
	}
	project = meta.Project.Name
	workdir = "/sensitive-worktree/acme-private"
	branch = "feature/customer-secret-rollout"
	remote = "ssh://git.internal/acme/private-project.git"
	meta.CWD = workdir
	meta.Git.Branch = &branch
	meta.Git.Remote = &remote
	rewritten, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("encode identity-rich publication metadata: %v", err)
	}
	if err := fs.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatalf("write identity-rich publication metadata: %v", err)
	}
	return project, workdir, branch, remote
}

// seedSubagentMemFS writes a subagent session's metadata.json under the
// subagent layout {base}/{hostSlug}/{parentID}/subagents/{sessionID}/, with
// meta.ParentUUID set so the mapper emits identity.parentUuid. It uses the
// production ingest.SessionMetadataPath helper to place the file, so the test
// fails if push and ingest ever disagree on the subagent path.
func seedSubagentMemFS(t *testing.T, fs *testutil.MemFS, hostSlug, sessionID, parentID string, provider defaults.Harness) {
	t.Helper()

	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = ingest.SessionID(sessionID)
	meta.ModelHarness = provider
	meta.Model = testutil.TestModel
	meta.Version = "2.1.47"
	parent := ingest.SessionID(parentID)
	meta.ParentUUID = &parent
	ingested := int64(1740312400000)
	meta.Timestamp = ingest.TimestampInfo{
		Start:    1740312000000,
		End:      1740312360000,
		Ingested: &ingested,
	}
	meta.Project = ingest.ProjectInfo{
		Hash:     testutil.TestProjectHash,
		Name:     "myapp",
		FilePath: "/home/test/myapp",
	}
	meta.HostSlug = ingest.HostSlug(hostSlug)
	meta.Source = ingest.SourceInfo{
		Format:   ingest.SourceFormatJSONL,
		FilePath: "/source/file.jsonl",
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal subagent metadata: %v", err)
	}

	metaPath := ingest.SessionMetadataPath("/sync", hostSlug, sessionID, parentID)
	if err := fs.WriteFile(metaPath, metaJSON, 0o644); err != nil {
		t.Fatalf("write subagent metadata: %v", err)
	}
}

// TestPipeline_Subagent_ResolvesAndUploadsParentUUID is the R2 integration test:
// a subagent session whose metadata lives under {parentID}/subagents/{id}/ must
// resolve (no "read metadata: no such file") and upload with identity.parentUuid
// set, alongside its root parent.
func TestPipeline_Subagent_ResolvesAndUploadsParentUUID(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()
	cc := string(defaults.HarnessClaudeCode)

	const hostSlug = testutil.TestHostSlug
	parentID := testutil.TestSessionUUID
	subagentID := testutil.TestSubagentID

	// Root parent at top level; subagent under {parentID}/subagents/{id}.
	seedMemFS(t, fs, hostSlug, parentID, defaults.HarnessClaudeCode)
	seedSubagentMemFS(t, fs, hostSlug, subagentID, parentID, defaults.HarnessClaudeCode)

	parentRow := makeSession(parentID, hostSlug, cc, nil)
	subRow := makeSession(subagentID, hostSlug, cc, nil)
	subRow.ParentID = parentID // the push query populates this from sessions.parent_id

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{parentRow, subRow},
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// No read-metadata errors: both sessions uploaded.
	if result.Errors != 0 {
		t.Fatalf("expected 0 errors, got %d (stderr: %s); sessions: %+v",
			result.Errors, stderr.String(), result.Sessions)
	}
	if len(pub.Calls) != 2 {
		t.Fatalf("expected 2 uploads (parent + subagent), got %d", len(pub.Calls))
	}

	// Find the typed authoritative request and assert the parent identity survived conversion.
	var sawSubagentParentUUID bool
	for _, request := range pub.AuthoritativeCalls {
		if request.Identity.SessionID.String() == subagentID {
			if request.Identity.ParentSessionID == nil || request.Identity.ParentSessionID.String() != parentID {
				t.Errorf("subagent parent identity: got %v, want %q", request.Identity.ParentSessionID, parentID)
			}
			operation, canonicalErr := schema.CanonicalizePublishRequest(request)
			if canonicalErr != nil {
				t.Fatal(canonicalErr)
			}
			expected, fingerprintErr := schema.FingerprintPublishOperation(operation)
			if fingerprintErr != nil {
				t.Fatal(fingerprintErr)
			}
			projectHash, hashErr := schema.NewProjectHash(subRow.ProjectHash)
			if hashErr != nil {
				t.Fatal(hashErr)
			}
			creds := baseCreds()
			record, readErr := store.Publication(ctx, creds.VillageURL, creds.UserID, projectHash, subagentID)
			if readErr != nil || record == nil {
				t.Fatalf("read subagent publication: record=%+v err=%v", record, readErr)
			}
			if record.Receipt.RequestOperationFingerprint != expected {
				t.Fatalf("subagent receipt fingerprint=%s want parent-bearing %s", record.Receipt.RequestOperationFingerprint, expected)
			}
			sawSubagentParentUUID = true
		}
	}
	if !sawSubagentParentUUID {
		t.Errorf("subagent session %q was not found among uploads", subagentID)
	}
}

// seedEmptyModelMemFS writes a root session's metadata.json with an EMPTY model
// field (mirroring the known ingest gap), so the push path's client-side
// refusal can be exercised.
func seedEmptyModelMemFS(t *testing.T, fs *testutil.MemFS, hostSlug, sessionID string, provider defaults.Harness) {
	t.Helper()

	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = ingest.SessionID(sessionID)
	meta.ModelHarness = provider
	meta.Model = "" // the defect under test
	meta.Version = "2.1.47"
	ingested := int64(1740312400000)
	meta.Timestamp = ingest.TimestampInfo{Start: 1740312000000, End: 1740312360000, Ingested: &ingested}
	meta.Project = ingest.ProjectInfo{
		Hash: testutil.TestProjectHash,
		Name: "myapp",
	}
	meta.HostSlug = ingest.HostSlug(hostSlug)
	meta.Source = ingest.SourceInfo{Format: ingest.SourceFormatJSONL, FilePath: "/source/file.jsonl"}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal empty-model metadata: %v", err)
	}
	metaPath := ingest.SessionMetadataPath("/sync", hostSlug, sessionID, "")
	if err := fs.WriteFile(metaPath, metaJSON, 0o644); err != nil {
		t.Fatalf("write empty-model metadata: %v", err)
	}
}

// TestPipeline_EmptyModel_RefusedNotUploaded verifies R3: a session whose
// metadata records no model is a client-side Error (sentinel push.ErrNoModel),
// is NOT uploaded (no village 400), in BOTH the real push and dry-run paths.
func TestPipeline_EmptyModel_RefusedNotUploaded(t *testing.T) {
	cc := string(defaults.HarnessClaudeCode)
	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID

	for _, dryRun := range []bool{false, true} {
		name := "push"
		if dryRun {
			name = "dry-run"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fs := testutil.NewMemFS()
			seedEmptyModelMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)

			store := &testutil.StubPushStore{
				Sessions: []ingest.PushSessionRow{makeSession(sessID, hostSlug, cc, nil)},
			}
			pub := &testutil.StubPublisher{StatusCode: 201}

			var stderr bytes.Buffer
			p := newTestPipeline(store, pub, fs, baseTestConfig(),
				push.PipelineConfig{DryRun: dryRun}, &stderr)

			result, err := p.Run(ctx)
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}

			if len(result.Sessions) != 1 {
				t.Fatalf("expected 1 session result, got %d", len(result.Sessions))
			}
			sr := result.Sessions[0]
			if sr.Status != push.PushStatusError {
				t.Errorf("status = %v, want Error", sr.Status)
			}
			if !errors.Is(sr.Error, push.ErrNoModel) {
				t.Errorf("error = %v, want errors.Is ErrNoModel", sr.Error)
			}
			if result.Errors != 1 {
				t.Errorf("result.Errors = %d, want 1", result.Errors)
			}
			// Never uploaded — the whole point of the client-side refusal.
			if len(pub.Calls) != 0 {
				t.Errorf("empty-model session must not upload, got %d calls", len(pub.Calls))
			}
			if push.ClassifyPushError(sr.Error) != push.CategoryNoModel {
				t.Errorf("classify = %v, want no-model", push.ClassifyPushError(sr.Error))
			}
		})
	}
}

// seedMissingHarnessMemFS writes a session whose metadata has a MODEL but NO
// harness (ModelHarness ""). It passes the meta.Model=="" guard yet maps to a
// publish body with model.harness:"", which client-side schema validation rejects
// against the harness enum. This case exercises the schema preflight, distinct
// from the empty-model guard.
func seedMissingHarnessMemFS(t *testing.T, fs *testutil.MemFS, hostSlug, sessionID string) {
	t.Helper()

	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = ingest.SessionID(sessionID)
	meta.ModelHarness = "" // the defect under test: no harness
	meta.Model = testutil.TestModel
	meta.Version = "2.1.47"
	ingested := int64(1740312400000)
	meta.Timestamp = ingest.TimestampInfo{Start: 1740312000000, End: 1740312360000, Ingested: &ingested}
	meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "myapp"}
	meta.HostSlug = ingest.HostSlug(hostSlug)
	meta.Source = ingest.SourceInfo{Format: ingest.SourceFormatJSONL, FilePath: "/source/file.jsonl"}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal missing-harness metadata: %v", err)
	}
	metaPath := ingest.SessionMetadataPath("/sync", hostSlug, sessionID, "")
	if err := fs.WriteFile(metaPath, metaJSON, 0o644); err != nil {
		t.Fatalf("write missing-harness metadata: %v", err)
	}
}

// TestPipeline_InvalidBody_RefusedNotUploaded is the client-side pre-flight
// integration test: a session whose mapped publish body is
// missing the required model.harness is rejected client-side with
// push.ErrInvalidPublishBody and is NEVER uploaded, while a valid session in the
// same run still uploads. It runs in BOTH the real push and dry-run paths (the
// pre-flight applies before the dry-run upload skip), driving the REAL pipeline +
// validator + StubPublisher (the publisher is the only mocked dependency).
func TestPipeline_InvalidBody_RefusedNotUploaded(t *testing.T) {
	const hostSlug = testutil.TestHostSlug
	validID := testutil.TestSessionUUID
	invalidID := testutil.TestOpenCodeSesID

	for _, dryRun := range []bool{false, true} {
		name := "push"
		if dryRun {
			name = "dry-run"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fs := testutil.NewMemFS()
			seedMemFS(t, fs, hostSlug, validID, defaults.HarnessClaudeCode)
			seedMissingHarnessMemFS(t, fs, hostSlug, invalidID)

			store := &testutil.StubPushStore{
				Sessions: []ingest.PushSessionRow{
					makeSession(validID, hostSlug, string(defaults.HarnessClaudeCode), nil),
					makeSession(invalidID, hostSlug, "", nil), // no harness, matches metadata
				},
			}
			pub := &testutil.StubPublisher{StatusCode: 201}

			var stderr bytes.Buffer
			p := newTestPipeline(store, pub, fs, baseTestConfig(),
				push.PipelineConfig{DryRun: dryRun}, &stderr)

			result, err := p.Run(ctx)
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}

			byID := map[string]push.SessionPushResult{}
			for _, sr := range result.Sessions {
				byID[sr.SessionID] = sr
			}

			// The missing-harness session is a client-side Error, classified
			// invalid-body, regardless of dry-run.
			inv, ok := byID[invalidID]
			if !ok {
				t.Fatalf("no result for the missing-harness session %s", invalidID)
			}
			if inv.Status != push.PushStatusError {
				t.Errorf("missing-harness status = %v, want Error", inv.Status)
			}
			if !errors.Is(inv.Error, push.ErrInvalidPublishBody) {
				t.Errorf("missing-harness error = %v, want errors.Is ErrInvalidPublishBody", inv.Error)
			}
			if got := push.ClassifyPushError(inv.Error); got != push.CategoryInvalidBody {
				t.Errorf("classify = %v, want invalid-body", got)
			}

			if dryRun {
				// Pre-flight runs in dry-run too, but nothing uploads either way.
				if len(pub.Calls) != 0 {
					t.Errorf("dry-run must make no uploads, got %d", len(pub.Calls))
				}
				return
			}

			// Real push: the invalid body never reaches the wire; the valid one does.
			if len(pub.Calls) != 1 {
				t.Errorf("exactly the valid session must upload (invalid rejected pre-flight), got %d calls", len(pub.Calls))
			}
			if valid := byID[validID]; valid.Status == push.PushStatusError {
				t.Errorf("valid session must not error under the new pre-flight: %v", valid.Error)
			}
		})
	}
}

// TestPipeline_UploadError_VillageVsNetwork drives the upload-error sentinel
// split end-to-end through the real pipeline + StubPublisher: a non-2xx village
// answer (statusCode 422 + err) classifies as village-rejected, while a
// transport failure (statusCode 0 + connection error) classifies as network.
// This covers the statusCode==0 || isConnectionError branch beyond the unit test.
func TestPipeline_UploadError_VillageVsNetwork(t *testing.T) {
	cc := string(defaults.HarnessClaudeCode)
	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID

	cases := []struct {
		name       string
		statusCode int
		uploadErr  error
		wantCat    push.PushErrorCategory
	}{
		{
			name:       "village rejection (non-2xx)",
			statusCode: 422,
			uploadErr:  errors.New("village returned 422: unprocessable entity"),
			wantCat:    push.CategoryVillageRejected,
		},
		{
			name:       "transport failure (connection refused)",
			statusCode: 0,
			uploadErr:  errors.New("execute request: dial tcp: connection refused"),
			wantCat:    push.CategoryNetwork,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fs := testutil.NewMemFS()
			seedMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)

			store := &testutil.StubPushStore{
				Sessions: []ingest.PushSessionRow{makeSession(sessID, hostSlug, cc, nil)},
			}
			pub := &testutil.StubPublisher{StatusCode: tc.statusCode, Err: tc.uploadErr}

			var stderr bytes.Buffer
			p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

			result, err := p.Run(ctx)
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if len(result.Sessions) != 1 || result.Sessions[0].Status != push.PushStatusError {
				t.Fatalf("expected 1 error session, got %+v", result.Sessions)
			}
			if got := push.ClassifyPushError(result.Sessions[0].Error); got != tc.wantCat {
				t.Errorf("classify = %q, want %q (err: %v)", got, tc.wantCat, result.Sessions[0].Error)
			}
			// The upload was attempted but no authoritative receipt was persisted.
			if len(pub.Calls) != 1 {
				t.Errorf("expected 1 upload attempt, got %d", len(pub.Calls))
			}
			if len(store.SavedPublicationIDs) != 0 {
				t.Errorf("failed upload must not persist a publication, got %d", len(store.SavedPublicationIDs))
			}
		})
	}
}

// newTestPipeline is a test helper to construct a Pipeline with the given store and publisher.
func newTestPipeline(
	store *testutil.StubPushStore,
	pub *testutil.StubPublisher,
	fs *testutil.MemFS,
	cfg *config.Config,
	runCfg push.PipelineConfig,
	stderr *bytes.Buffer,
) *push.Pipeline {
	return push.NewPipeline(store, pub, baseCreds(), cfg, fs, runCfg, nil, stderr)
}

func TestPipeline_DefaultFieldsUseSaltedProjectLabel(t *testing.T) {
	fs := testutil.NewMemFS()
	seedMemFS(t, fs, testutil.TestHostSlug, testutil.TestSessionUUID, defaults.HarnessClaudeCode)
	rawProject, rawWorkdir, rawBranch, rawRemote := plantRawPublicationIdentity(t, fs, testutil.TestHostSlug, testutil.TestSessionUUID)
	storeDouble := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, defaults.HarnessClaudeCode.String(), nil)},
	}
	publisher := &testutil.StubPublisher{}
	var stderr bytes.Buffer
	result, err := newTestPipeline(storeDouble, publisher, fs, baseTestConfig(), push.PipelineConfig{}, &stderr).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Errors != 0 || len(publisher.AuthoritativeCalls) != 1 || len(publisher.Calls) != 1 {
		t.Fatalf("authoritative publication result=%+v metadata calls=%d content calls=%d, want one successful call", result, len(publisher.AuthoritativeCalls), len(publisher.Calls))
	}
	want := "project-" + string(testutil.TestProjectHash)[:12]
	if got := publisher.AuthoritativeCalls[0].Project.Name; got != want {
		t.Fatalf("authoritative project name = %q, want privacy-safe label %q derived from the salted hash", got, want)
	}
	var content schema.TranscriptContent
	if err := json.Unmarshal(publisher.Calls[0].TranscriptBody, &content); err != nil {
		t.Fatalf("decode published transcript content: %v", err)
	}
	if content.SessionDetail == nil {
		t.Fatal("published transcript content has no session detail")
	}
	detail := content.SessionDetail
	if detail.Project != want {
		t.Errorf("transcript project = %q, want privacy-safe label %q derived from the salted hash", detail.Project, want)
	}
	if detail.WorkingDirectory != "" || detail.GitBranch != "" || detail.GitRemote != "" {
		t.Errorf("transcript identity fields were not withheld: workingDirectory=%q gitBranch=%q gitRemote=%q", detail.WorkingDirectory, detail.GitBranch, detail.GitRemote)
	}
	publishedBody := string(publisher.Calls[0].TranscriptBody)
	if strings.Contains(publishedBody, rawProject) {
		t.Errorf("transcript content contains opted-out raw project %q", rawProject)
	}
	if strings.Contains(publishedBody, rawWorkdir) {
		t.Errorf("transcript content contains opted-out working directory %q", rawWorkdir)
	}
	if strings.Contains(publishedBody, rawBranch) {
		t.Errorf("transcript content contains opted-out branch %q", rawBranch)
	}
	if strings.Contains(publishedBody, rawRemote) {
		t.Errorf("transcript content contains opted-out remote %q", rawRemote)
	}
}

func TestPipelineRejectsAuthoritativeReceiptIdentityMismatch(t *testing.T) {
	for _, fixture := range loadAuthoritativeReceiptMismatchCases(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			sessionID := testutil.TestSessionUUID
			row := makeSession(sessionID, testutil.TestHostSlug, defaults.HarnessClaudeCode.String(), nil)
			if fixture.ParentSessionID != "" {
				sessionID = testutil.TestSubagentID
				seedSubagentMemFS(t, fs, testutil.TestHostSlug, sessionID, fixture.ParentSessionID, defaults.HarnessClaudeCode)
				row = makeSession(sessionID, testutil.TestHostSlug, defaults.HarnessClaudeCode.String(), nil)
				row.ParentID = fixture.ParentSessionID
			} else {
				seedMemFS(t, fs, testutil.TestHostSlug, sessionID, defaults.HarnessClaudeCode)
			}
			storeDouble := &testutil.StubPushStore{Sessions: []ingest.PushSessionRow{row}}
			publisher := &testutil.StubPublisher{}
			if fixture.Mismatch == "content" {
				publisher.ReceiptContentHash = schema.TranscriptContentHash(fixture.Replacement)
			} else {
				publisher.ReceiptFingerprint = schema.PublishRequestFingerprint(fixture.Replacement)
			}
			var stderr bytes.Buffer
			result, err := newTestPipeline(storeDouble, publisher, fs, baseTestConfig(), push.PipelineConfig{}, &stderr).Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Sessions) != 1 || result.Sessions[0].Status != push.PushStatusError || !strings.Contains(result.Sessions[0].Error.Error(), fixture.ErrorContains) {
				t.Fatalf("mismatch result=%+v", result)
			}
			if len(storeDouble.Publications) != 0 || len(storeDouble.SavedPublicationIDs) != 0 || len(storeDouble.PublicationAttempts) != 1 || storeDouble.PublicationAttempts[0].Stage != storepkg.PublicationAttemptStageValidate {
				t.Fatalf("mismatch persisted state: publications=%+v saved=%+v attempts=%+v", storeDouble.Publications, storeDouble.SavedPublicationIDs, storeDouble.PublicationAttempts)
			}
			if fixture.ParentSessionID != "" {
				if len(publisher.AuthoritativeCalls) != 1 || publisher.AuthoritativeCalls[0].Identity.ParentSessionID == nil || publisher.AuthoritativeCalls[0].Identity.ParentSessionID.String() != fixture.ParentSessionID {
					t.Fatalf("parent-bearing outgoing request=%+v", publisher.AuthoritativeCalls)
				}
			}
		})
	}
}

// --- Tests ---

func TestPipeline_Selection_FiltersByBranch(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()
	cc := string(defaults.HarnessClaudeCode)
	remote := "git@github.com:user/repo.git"

	// Dry-run reads metadata for kept sessions; seed the one that survives
	// selection (s-main). Dropped sessions are never dry-run'd.
	seedMemFS(t, fs, "h", "s-main", defaults.HarnessClaudeCode)
	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession("s-main", "h", cc, nil, withGitRemote(remote), withGitBranch("main")),                        // selected
			makeSession("s-feature", "h", cc, nil, withGitRemote(remote), withGitBranch("feature")),                  // dropped: branch not selected
			makeSession("s-other", "h", cc, nil, withGitRemote("git@github.com:other/x.git"), withGitBranch("main")), // dropped: project not selected
		},
	}
	selection := push.NewSessionSelection(map[ingest.SessionID]ingest.BranchMatch{
		"s-main":    ingest.BranchMatchYes,
		"s-feature": ingest.BranchMatchNo,
		"s-other":   ingest.BranchMatchNo,
	})

	var stderr bytes.Buffer
	p := newTestPipeline(store, &testutil.StubPublisher{}, fs, baseTestConfig(),
		push.PipelineConfig{DryRun: true, Selection: selection}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Sessions) != 1 {
		t.Fatalf("expected exactly 1 targeted session (s-main), got %d: %+v", len(result.Sessions), result.Sessions)
	}
	if result.Sessions[0].SessionID != "s-main" {
		t.Errorf("expected s-main, got %q", result.Sessions[0].SessionID)
	}
}

func TestPipeline_Selection_WithheldConflict_Surfaced(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()
	cc := string(defaults.HarnessClaudeCode)
	remote := "git@github.com:user/repo.git"

	// Multi-project conflict: proj-by-remote allows "main"; proj-by-name allows
	// only "dev". A session on "main" matching both => AND-strict WithheldConflict.
	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession("s-conflict", "h", cc, nil, withGitRemote(remote), withProjectName("conflictproj"), withGitBranch("main")),
		},
	}
	selection := push.NewSessionSelection(map[ingest.SessionID]ingest.BranchMatch{
		"s-conflict": ingest.BranchMatchWithheldConflict,
	})

	var stderr bytes.Buffer
	p := newTestPipeline(store, &testutil.StubPublisher{}, fs, baseTestConfig(),
		push.PipelineConfig{DryRun: true, Verbose: true, Selection: selection}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The conflicting session is withheld from the push set...
	if len(result.Sessions) != 0 {
		t.Errorf("expected 0 targeted sessions (conflict withheld), got %d: %+v", len(result.Sessions), result.Sessions)
	}
	// ...but surfaced on stderr (count line + per-session line under --verbose),
	// never silently dropped.
	out := stderr.String()
	if !strings.Contains(out, "withheld") || !strings.Contains(out, "conflicting branch selection") {
		t.Errorf("expected stderr to flag the withheld conflict; got: %q", out)
	}
	if !strings.Contains(out, "s-conflict") {
		t.Errorf("expected --verbose stderr to name the withheld session s-conflict; got: %q", out)
	}
}

func TestPipeline_NormalPush_ThreeNew(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	sess1ID := testutil.TestSessionUUID
	sess2ID := testutil.TestSubagentID
	sess3ID := testutil.TestOpenCodeSesID

	const hostSlug = testutil.TestHostSlug

	seedMemFS(t, fs, hostSlug, sess1ID, defaults.HarnessClaudeCode)
	seedMemFS(t, fs, hostSlug, sess2ID, defaults.HarnessClaudeCode)
	seedMemFS(t, fs, hostSlug, sess3ID, defaults.HarnessClaudeCode)

	pushedAt := time.Now().UnixMilli() - 1000
	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sess1ID, hostSlug, string(defaults.HarnessClaudeCode), nil),       // never pushed
			makeSession(sess2ID, hostSlug, string(defaults.HarnessClaudeCode), nil),       // never pushed
			makeSession(sess3ID, hostSlug, string(defaults.HarnessClaudeCode), &pushedAt), // previously pushed
		},
	}

	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.New+result.Updated != 3 {
		t.Errorf("expected 3 total pushed (new+updated), got new=%d updated=%d", result.New, result.Updated)
	}
	if result.Errors != 0 {
		t.Errorf("expected 0 errors, got %d", result.Errors)
	}
	// Verify the authoritative receipt was persisted.
	if len(store.SavedPublicationIDs) == 0 {
		t.Error("SavePublication was never called")
	}
	// Verify audit log was written.
	if len(store.PushLogs) != 1 {
		t.Errorf("InsertPushLog call count: got %d, want 1", len(store.PushLogs))
	}
}

// TestPipeline_License drives the resolved license end-to-end through the REAL
// pipeline: config default vs --license override vs none, asserting it lands on the
// published body AND is persisted with the authoritative receipt. Exercises resolveLicense (CLI >
// config > none) without reaching into the unexported method.
func TestPipeline_License(t *testing.T) {
	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID

	// licenseOf runs a single push and returns (wire license on the body, persisted
	// license in the saved receipt).
	licenseOf := func(t *testing.T, cfgLicense config.License, runLicense schema.License) (string, schema.License) {
		t.Helper()
		fs := testutil.NewMemFS()
		seedMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)
		store := &testutil.StubPushStore{
			Sessions: []ingest.PushSessionRow{
				makeSession(sessID, hostSlug, string(defaults.HarnessClaudeCode), nil),
			},
		}
		cfg := baseTestConfig()
		cfg.Push.License = cfgLicense
		pub := &testutil.StubPublisher{StatusCode: 201}
		var stderr bytes.Buffer
		p := newTestPipeline(store, pub, fs, cfg, push.PipelineConfig{License: runLicense}, &stderr)

		if _, err := p.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(pub.Calls) != 1 {
			t.Fatalf("expected 1 publish call, got %d", len(pub.Calls))
		}
		var body map[string]any
		if err := json.Unmarshal(pub.Calls[0].MetadataJSON, &body); err != nil {
			t.Fatalf("unmarshal published body: %v", err)
		}
		wire, _ := body["license"].(string) // absent ⇒ "" (omitempty)
		return wire, store.SavedPublicationLicense
	}

	t.Run("config default lands on body + is persisted", func(t *testing.T) {
		wire, persisted := licenseOf(t, config.LicenseCCBY, "")
		if wire != string(config.LicenseCCBY) {
			t.Errorf("wire license = %q, want %q", wire, config.LicenseCCBY)
		}
		if persisted != config.LicenseCCBY {
			t.Errorf("persisted license = %q, want %q", persisted, config.LicenseCCBY)
		}
	})

	t.Run("--license overrides the config default", func(t *testing.T) {
		wire, persisted := licenseOf(t, config.LicenseCCBY, config.LicenseCC0)
		if wire != string(config.LicenseCC0) {
			t.Errorf("wire license = %q, want override %q", wire, config.LicenseCC0)
		}
		if persisted != config.LicenseCC0 {
			t.Errorf("persisted license = %q, want override %q", persisted, config.LicenseCC0)
		}
	})

	t.Run("no license ⇒ omitted on body + NULL-equivalent persisted", func(t *testing.T) {
		wire, persisted := licenseOf(t, "", "")
		if wire != "" {
			t.Errorf("wire license = %q, want \"\" (omitted)", wire)
		}
		if persisted != "" {
			t.Errorf("persisted license = %q, want \"\"", persisted)
		}
	})
}

func TestPipeline_DryRun_NoHTTPNoStoreWrites(t *testing.T) {
	if push.DryRunCapabilityMutation {
		t.Skip("the mounted capability fixture owns the dry-run decision mutation")
	}
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sess1ID := testutil.TestSessionUUID

	// Dry-run now reads metadata (parity with the real push path: it predicts the
	// empty-model / missing-metadata Error states), so seed the metadata file.
	seedMemFS(t, fs, hostSlug, sess1ID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sess1ID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
	}
	pub := &testutil.StubPublisher{}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{DryRun: true}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// No HTTP calls.
	if len(pub.Calls) != 0 {
		t.Errorf("dry-run should make no HTTP calls, got %d", len(pub.Calls))
	}
	// No publication receipt persistence.
	if len(store.SavedPublicationIDs) != 0 {
		t.Errorf("dry-run should not persist publications, got %d calls", len(store.SavedPublicationIDs))
	}
	// No audit log.
	if len(store.PushLogs) != 0 {
		t.Errorf("dry-run should not write push_log, got %d entries", len(store.PushLogs))
	}
	// But result should show the session.
	if len(result.Sessions) != 1 {
		t.Errorf("dry-run result: got %d sessions, want 1", len(result.Sessions))
	}
	if result.Sessions[0].Status != push.PushStatusNew {
		t.Errorf("dry-run status: got %v, want %v", result.Sessions[0].Status, push.PushStatusNew)
	}
}

func TestPipeline_DryRun_UpdatedStatus(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sess1ID := testutil.TestSessionUUID
	pushedAt := time.Now().UnixMilli() - 5000

	// Dry-run reads metadata for parity with the real push path.
	seedMemFS(t, fs, hostSlug, sess1ID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sess1ID, hostSlug, string(defaults.HarnessClaudeCode), &pushedAt),
		},
	}
	pub := &testutil.StubPublisher{}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{DryRun: true}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Sessions[0].Status != push.PushStatusUpdated {
		t.Errorf("dry-run for previously-pushed session: got %v, want %v",
			result.Sessions[0].Status, push.PushStatusUpdated)
	}
}

func TestPipeline_Force_CallsAllPushableSessions(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sess1ID := testutil.TestSessionUUID

	seedMemFS(t, fs, hostSlug, sess1ID, defaults.HarnessClaudeCode)

	pushedAt := time.Now().UnixMilli() - 1000
	store := &testutil.StubPushStore{
		// Sessions (unpushed) is empty — force mode should bypass this.
		Sessions: []ingest.PushSessionRow{},
		// AllSessions has the previously-pushed session.
		AllSessions: []ingest.PushSessionRow{
			makeSession(sess1ID, hostSlug, string(defaults.HarnessClaudeCode), &pushedAt),
		},
	}
	pub := &testutil.StubPublisher{StatusCode: 200}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{Force: true}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Updated != 1 {
		t.Errorf("force: expected 1 updated, got %d", result.Updated)
	}
	if len(pub.Calls) != 1 {
		t.Errorf("force: expected 1 HTTP call, got %d", len(pub.Calls))
	}
}

func TestPipeline_EmptyState_NothingIngested(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	store := &testutil.StubPushStore{
		Sessions:    []ingest.PushSessionRow{},
		AllSessions: []ingest.PushSessionRow{}, // no sessions at all
	}
	pub := &testutil.StubPublisher{}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(result.EmptyReason, "Nothing to push") {
		t.Errorf("EmptyReason: got %q, want contains 'Nothing to push'", result.EmptyReason)
	}
}

func TestPipeline_EmptyState_AllAlreadyPushed(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	pushedAt := time.Now().UnixMilli() - 1000
	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{}, // nothing unpushed
		AllSessions: []ingest.PushSessionRow{
			makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, string(defaults.HarnessClaudeCode), &pushedAt),
		},
	}
	pub := &testutil.StubPublisher{}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(result.EmptyReason, "already pushed") {
		t.Errorf("EmptyReason: got %q, want contains 'already pushed'", result.EmptyReason)
	}
}

// TestPipeline_EmptyState_RepositoryScope covers the state a per-commit hook
// actually lives in: nothing new to push. The corpus, and the reasoning behind
// every phrase it demands and forbids, is in
// internal/push/testdata/scoped_empty_state.yaml.
func TestPipeline_EmptyState_RepositoryScope(t *testing.T) {
	t.Parallel()
	document, err := loadScopedEmptyStateFixture(scopedEmptyStateFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			result, err := runScopedEmptyState(t, testCase, false)
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			for _, want := range testCase.MustContain {
				if !strings.Contains(result.EmptyReason, want) {
					t.Errorf("EmptyReason must state %q; got: %q", want, result.EmptyReason)
				}
			}
			for _, forbidden := range testCase.MustNotContain {
				if strings.Contains(result.EmptyReason, forbidden) {
					t.Errorf("EmptyReason must not claim %q; got: %q", forbidden, result.EmptyReason)
				}
			}
			assertStaleIdentityAdvice(t, testCase, result.EmptyReason)
		})
	}
}

// TestPipeline_EmptyState_RepositoryScopeIsOneLineUnderQuiet holds the size of
// what a hook prints into every commit.
//
// --quiet promises errors plus one final result line, and the empty-state line
// is that final line. It carried the resolved scope, and with it every directory
// the scope admits: 4,585 bytes per commit on a monorepo, longest line 4,309
// characters, dumped into terminals and CI logs on every commit and every push.
func TestPipeline_EmptyState_RepositoryScopeIsOneLineUnderQuiet(t *testing.T) {
	t.Parallel()
	document, err := loadScopedEmptyStateFixture(scopedEmptyStateFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			result, err := runScopedEmptyState(t, testCase, true)
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if strings.Contains(result.EmptyReason, "\n") {
				t.Errorf("under --quiet the empty state must be one line; got:\n%s", result.EmptyReason)
			}
			// It still has to be an answer, not a stub: the repository, what was
			// found, and the command that changes it.
			for _, want := range []string{scopedRepositoryRoot, "peasant"} {
				if !strings.Contains(result.EmptyReason, want) {
					t.Errorf("the one-line empty state must still state %q; got: %q", want, result.EmptyReason)
				}
			}
			if strings.Contains(result.EmptyReason, scopedRecordedSubdirectory) {
				t.Errorf("the one-line empty state must not enumerate admitted directories; got: %q", result.EmptyReason)
			}
			// --quiet is what a hook runs with, so the summary line is the only
			// line it can ever show. A diagnosis that lives in the detail is
			// invisible to exactly the user living in the state it diagnoses.
			assertStaleIdentityAdvice(t, testCase, result.EmptyReason)
		})
	}
}

// assertStaleIdentityAdvice holds the re-ingest recommendation to its evidence.
//
// With evidence, it has to be there AND it has to be the scoped form: the
// unscoped 'peasant ingest --force' re-ingests every project on the machine and
// clears every already-published marker, so the next push re-uploads the user's
// whole corpus — from a hook, on every commit.
//
// Without evidence it must not appear at all. The note used to print on all
// three scoped-empty branches unconditionally; on a MOVED repository the
// recorded directory is gone, so the re-ingest re-derives the identity from a
// dead path and the sessions become unreachable from every scope, with no
// command that brings them back.
func assertStaleIdentityAdvice(t *testing.T, testCase scopedEmptyStateCase, reason string) {
	t.Helper()
	if !testCase.StaleIdentity {
		if strings.Contains(reason, "ingest --force") {
			t.Errorf("a re-ingest must not be recommended without evidence of a stale identity; got: %q", reason)
		}
		return
	}
	for _, want := range []string{
		scopedStaleDirectory,
		"peasant --data-dir '/tmp/bound data' ingest --force --session '" + scopedStaleSessionID + "'",
		"clears every already-published marker",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("the stale-identity diagnosis must state %q; got: %q", want, reason)
		}
	}
}

// runScopedEmptyState builds the world a case describes and runs a scoped push
// against it. Nothing is ever eligible: that is the steady state a hook lives in.
func runScopedEmptyState(t *testing.T, testCase scopedEmptyStateCase, quiet bool) (*push.PushResult, error) {
	t.Helper()
	pushedAt := time.Now().UnixMilli() - 1000
	inScope := makeSession("11111111-1111-4111-8111-111111111111", testutil.TestHostSlug,
		string(defaults.HarnessClaudeCode), &pushedAt,
		withGitRemote(scopedGitRemote), withGitBranch("main"),
		func(s *ingest.PushSessionRow) { s.ProjectHash = scopedProjectHash })

	var all []ingest.PushSessionRow
	if testCase.OtherPending {
		// Unrelated pending work: what the wording used to key off.
		all = append(all, makeSession(testutil.TestSessionUUID, testutil.TestHostSlug,
			string(defaults.HarnessClaudeCode), nil))
	}
	if testCase.StaleIdentity {
		// Work recorded inside this repository that the scope cannot reach:
		// the session the diagnosis has to name, and the only thing that makes
		// a re-ingest recommendation safe to print.
		all = append(all, makeSession(scopedStaleSessionID, testutil.TestHostSlug,
			string(defaults.HarnessClaudeCode), &pushedAt,
			func(s *ingest.PushSessionRow) { s.ProjectHash = scopedStaleProjectHash }))
	}
	runCfg := push.PipelineConfig{
		Repository:     testScope(testCase.StaleIdentity),
		Quiet:          quiet,
		CommandBinding: githooks.Binding{DataDir: "/tmp/bound data"},
	}
	switch testCase.World {
	case worldNothingRecorded:
		// No session carries this repository's identity.
	case worldAllPublished:
		all = append(all, inScope)
	case worldSelectionExcluded:
		all = append(all, inScope)
		// A prepared selection excludes this repository's recorded session.
		runCfg.Selection = push.NewSessionSelection(map[ingest.SessionID]ingest.BranchMatch{
			ingest.SessionID(inScope.SessionID): ingest.BranchMatchNo,
		})
	}

	store := &testutil.StubPushStore{
		Sessions:    []ingest.PushSessionRow{}, // nothing eligible: the hook's steady state
		AllSessions: all,
	}
	var stderr bytes.Buffer
	p := newTestPipeline(store, &testutil.StubPublisher{}, testutil.NewMemFS(), baseTestConfig(), runCfg, &stderr)
	return p.Run(context.Background())
}

// TestPipeline_BoundedRunUploadsSmallestFirst proves a fixed budget buys as much
// recorded progress as it can.
//
// A budget spent on one oversized transcript buys nothing: the upload is cut
// off, nothing is recorded, and the next commit spends the whole budget on
// exactly the same session — forever. Sending the small ones first means every
// bounded run converts its budget into published sessions, and the large one is
// left alone at the end, which is the state that can honestly be reported as
// needing a manual run or a bigger budget.
//
// A run with no deadline keeps the order it had, so an ordinary push is
// unchanged.
func TestPipeline_BoundedRunUploadsSmallestFirst(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	// Recorded newest-first (the pre-existing order) with size growing the same
	// way, so "newest first" and "largest first" are the same order — which is
	// what a bounded run must NOT do.
	ids := []string{testutil.TestSessionUUID, testutil.TestSubagentID, testutil.TestOpenCodeSesID}
	sessions := make([]ingest.PushSessionRow, len(ids))
	for i, id := range ids {
		seedMemFS(t, fs, testutil.TestHostSlug, id, defaults.HarnessClaudeCode)
		tokens := (len(ids) - i) * 1000
		sessions[i] = makeSession(id, testutil.TestHostSlug, string(defaults.HarnessClaudeCode), nil,
			func(s *ingest.PushSessionRow) { s.TokensTotal = tokens })
	}
	smallest := ids[len(ids)-1]

	for _, testCase := range []struct {
		name     string
		bounded  bool
		wantHead string
	}{
		{name: "a bounded run sends the smallest first", bounded: true, wantHead: smallest + "--content.json"},
		{name: "an unbounded run keeps its order", bounded: false, wantHead: ids[0] + "--content.json"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if testCase.bounded {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Minute)
				defer cancel()
			}
			pub := &testutil.StubPublisher{StatusCode: 201}
			var stderr bytes.Buffer
			// Concurrency 1 so arrival order is the decision under test rather
			// than a scheduling race.
			p := newTestPipeline(&testutil.StubPushStore{Sessions: sessions}, pub, fs, baseTestConfig(),
				push.PipelineConfig{Concurrency: 1}, &stderr)

			if _, err := p.Run(ctx); err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if len(pub.Calls) != len(ids) {
				t.Fatalf("expected %d uploads, got %d", len(ids), len(pub.Calls))
			}
			if pub.Calls[0].Filename != testCase.wantHead {
				got := make([]string, 0, len(pub.Calls))
				for _, call := range pub.Calls {
					got = append(got, call.Filename)
				}
				t.Errorf("first upload = %s, want %s; order was %v", pub.Calls[0].Filename, testCase.wantHead, got)
			}
		})
	}
}

// TestPipeline_HeldBackNoticeIsScopedToTheRepository proves the held-back notice
// does not name another repository's sessions. A hook fires on every commit in
// ONE repository; listing unrelated session identifiers there is noise the user
// cannot act on and disclosure they did not ask for.
func TestPipeline_HeldBackNoticeIsScopedToTheRepository(t *testing.T) {
	ctx := context.Background()
	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{},
		HeldSessions: []ingest.HeldSession{
			{SessionID: "held-elsewhere", ProjectHash: string(testutil.TestProjectHash)},
			{SessionID: "held-in-scope", ProjectHash: scopedProjectHash},
		},
	}
	var stderr bytes.Buffer
	runCfg := push.PipelineConfig{Repository: testScope(false)}
	p := newTestPipeline(store, &testutil.StubPublisher{}, testutil.NewMemFS(), baseTestConfig(), runCfg, &stderr)

	if _, err := p.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(stderr.String(), "held-in-scope") {
		t.Errorf("this repository's held session must still be reported; stderr: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "held-elsewhere") {
		t.Errorf("another repository's held session must not be named; stderr: %q", stderr.String())
	}
}

// testScope builds the repository scope used by the scoped-push tests. It
// admits scopedProjectHash, the shared fixture identity that is deliberately
// distinct from testutil.TestProjectHash, so a session either belongs to the
// scope or provably does not.
//
// It also carries a recorded subdirectory, because that is where the size of the
// scoped messages comes from: the resolved scope enumerates every directory it
// admits, and a real monorepo has a hundred and fifty of them.
//
// stale adds the evidence that entitles the empty state to recommend a
// re-ingest: a directory still inside this worktree whose sessions carry an
// identity the scope does not admit. Without that evidence the recommendation
// must not appear at all — on a moved repository it re-derives the identity
// from a dead path and loses the sessions for good.
func testScope(stale bool) *push.RepositoryScope {
	var unadmitted []push.RecordedUnderRoot
	if stale {
		unadmitted = []push.RecordedUnderRoot{{
			Hash:      ingest.ProjectHash(scopedStaleProjectHash),
			Directory: scopedStaleDirectory,
		}}
	}
	return push.NewRepositoryScope(
		scopedRepositoryRoot, ingest.ProjectHash(scopedProjectHash), push.IdentityFromPath,
		[]push.RecordedUnderRoot{{
			Hash:      ingest.ProjectHash("2222222222222222222222222222222222222222222222222222222222222222"),
			Directory: scopedRecordedSubdirectory,
		}},
		unadmitted,
	)
}

func TestPipeline_ConnectionAbort_ThreeConsecutiveFailures(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	ids := []string{
		testutil.TestSessionUUID,
		testutil.TestSubagentID,
		testutil.TestOpenCodeSesID,
	}

	sessions := make([]ingest.PushSessionRow, len(ids))
	for i, id := range ids {
		seedMemFS(t, fs, hostSlug, id, defaults.HarnessClaudeCode)
		sessions[i] = makeSession(id, hostSlug, string(defaults.HarnessClaudeCode), nil)
	}

	store := &testutil.StubPushStore{Sessions: sessions}
	pub := &testutil.StubPublisher{
		Err: fmt.Errorf("connection refused: unable to reach village"),
	}

	var stderr bytes.Buffer
	// Use concurrency=1 so failures are guaranteed sequential for deterministic counting.
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{Concurrency: 1}, &stderr)

	result, err := p.Run(ctx)
	if err == nil {
		t.Fatal("expected abort error, got nil")
	}
	if !strings.Contains(err.Error(), "connection") {
		t.Errorf("abort error: got %q, want contains 'connection'", err.Error())
	}
	if result.Errors == 0 {
		t.Error("expected some error-count sessions in result")
	}
}

func TestPipeline_ContinueOnError_NonConnectionError(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sess1ID := testutil.TestSessionUUID
	sess2ID := testutil.TestSubagentID

	// Only seed sess2 — sess1 will fail on file read.
	seedMemFS(t, fs, hostSlug, sess2ID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sess1ID, hostSlug, string(defaults.HarnessClaudeCode), nil), // will fail (no files in MemFS)
			makeSession(sess2ID, hostSlug, string(defaults.HarnessClaudeCode), nil), // will succeed
		},
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{Concurrency: 1}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Errors != 1 {
		t.Errorf("errors: got %d, want 1", result.Errors)
	}
	if result.New != 1 {
		t.Errorf("new: got %d, want 1", result.New)
	}
}

func TestPipeline_UnscopedHeldBackSessions_StderrNotice(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sess1ID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sess1ID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sess1ID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
		HeldSessions: []ingest.HeldSession{{SessionID: "held-session-abc", ProjectHash: string(testutil.TestProjectHash)}},
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	_, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "held back") {
		t.Errorf("stderr should contain 'held back', got: %q", stderrStr)
	}
	if !strings.Contains(stderrStr, "held-session-abc") {
		t.Errorf("stderr should contain held session ID, got: %q", stderrStr)
	}
}

func TestPipeline_IndividualMethod_Error(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	cfg := baseTestConfig()
	cfg.Push.Method = config.PushMethodIndividual

	store := &testutil.StubPushStore{}
	pub := &testutil.StubPublisher{}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, cfg, push.PipelineConfig{}, &stderr)

	_, err := p.Run(ctx)
	if err == nil {
		t.Fatal("expected error for individual mode, got nil")
	}
	if !strings.Contains(err.Error(), "individual") {
		t.Errorf("error message: got %q, want contains 'individual'", err.Error())
	}
}

func TestPipeline_SourceProvider_FiltersCorrectly(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	claudeID := testutil.TestSessionUUID
	openCodeID := testutil.TestOpenCodeSesID

	seedMemFS(t, fs, hostSlug, claudeID, defaults.HarnessClaudeCode)
	// Do NOT seed opencode — it should not be fetched when filtered to claude.

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(claudeID, hostSlug, string(defaults.HarnessClaudeCode), nil),
			makeSession(openCodeID, hostSlug, string(defaults.HarnessOpenCode), nil),
		},
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(),
		push.PipelineConfig{SourceProvider: string(defaults.HarnessClaudeCode)}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.New != 1 {
		t.Errorf("new: got %d, want 1 (only claude sessions)", result.New)
	}
	if len(pub.Calls) != 1 {
		t.Errorf("HTTP calls: got %d, want 1", len(pub.Calls))
	}
}

func TestPipeline_InsertPushLog_ErrorLoggedToStderr(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sessID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
		InsertLogErr: fmt.Errorf("database locked"),
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run should not return error when only InsertPushLog fails: %v", err)
	}
	if result.New != 1 {
		t.Errorf("new: got %d, want 1", result.New)
	}
	if !strings.Contains(stderr.String(), "audit log") {
		t.Errorf("stderr should contain audit log warning, got: %q", stderr.String())
	}
}

func TestPipeline_ReceiptPersistenceFailureIsPartialFailure(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sessID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
		SavePublicationErr: fmt.Errorf("constraint violation"),
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run should not return an aggregate error when one receipt persistence fails: %v", err)
	}
	if result.Errors != 1 || len(result.Sessions) != 1 || result.Sessions[0].Status != push.PushStatusError {
		t.Fatalf("receipt persistence failure must remain retryable: %+v", result)
	}
	for _, want := range []string{
		"Village accepted",
		"constraint violation",
		"terminal receipt could not be persisted",
	} {
		if !strings.Contains(result.Sessions[0].Error.Error(), want) {
			t.Errorf("partial failure must state %q, got: %q", want, result.Sessions[0].Error)
		}
	}
	if len(store.PublicationAttempts) != 1 || store.PublicationAttempts[0].Stage != storepkg.PublicationAttemptStagePersistence {
		t.Fatalf("persistence diagnostic=%+v", store.PublicationAttempts)
	}
}

func TestPipeline_BySourceMethod(t *testing.T) {
	// Verifies the by-source code path in getTargetSessions:
	// when Method == PushMethodBySource the pipeline calls UnpushedSessionsByProvider
	// for each configured provider, collects their sessions, and pushes them.
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	claudeID := testutil.TestSessionUUID
	openCodeID := testutil.TestOpenCodeSesID

	// Seed filesystem for both sessions.
	seedMemFS(t, fs, hostSlug, claudeID, defaults.HarnessClaudeCode)
	seedMemFS(t, fs, hostSlug, openCodeID, defaults.HarnessOpenCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(claudeID, hostSlug, string(defaults.HarnessClaudeCode), nil),
			makeSession(openCodeID, hostSlug, string(defaults.HarnessOpenCode), nil),
		},
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	cfg := baseTestConfig()
	cfg.Push.Method = config.PushMethodBySource
	cfg.Push.Sources = []string{
		string(defaults.HarnessClaudeCode),
		string(defaults.HarnessOpenCode),
	}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, cfg, push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// Both providers' sessions should be pushed.
	if result.New != 2 {
		t.Errorf("by-source: expected 2 new, got new=%d updated=%d errors=%d",
			result.New, result.Updated, result.Errors)
	}
	if len(pub.Calls) != 2 {
		t.Errorf("by-source: expected 2 HTTP calls, got %d", len(pub.Calls))
	}
	// Audit log must be written.
	if len(store.PushLogs) != 1 {
		t.Errorf("by-source: InsertPushLog call count: got %d, want 1", len(store.PushLogs))
	}
}

func TestPipeline_BySourceMethod_SingleProvider(t *testing.T) {
	// Verifies by-source with a single provider configured: only that provider's
	// sessions are fetched; sessions for other providers in the store are not pushed.
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	claudeID := testutil.TestSessionUUID
	openCodeID := testutil.TestOpenCodeSesID

	// Only seed claude — opencode must never be accessed.
	seedMemFS(t, fs, hostSlug, claudeID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(claudeID, hostSlug, string(defaults.HarnessClaudeCode), nil),
			makeSession(openCodeID, hostSlug, string(defaults.HarnessOpenCode), nil),
		},
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	cfg := baseTestConfig()
	cfg.Push.Method = config.PushMethodBySource
	cfg.Push.Sources = []string{string(defaults.HarnessClaudeCode)} // only claude configured

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, cfg, push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// Only the claude session should be pushed.
	if result.New != 1 {
		t.Errorf("by-source single-provider: expected 1 new, got new=%d updated=%d errors=%d",
			result.New, result.Updated, result.Errors)
	}
	if len(pub.Calls) != 1 {
		t.Errorf("by-source single-provider: expected 1 HTTP call, got %d", len(pub.Calls))
	}
}

func TestPipeline_MetricsPresent_QualityIncludedInPayload(t *testing.T) {
	// When the store has QualityMetrics for a session, the pipeline should
	// fetch them via GetQualityMetrics and include them in the publish request
	// JSON under the "quality" key.
	//
	// NOTE (L2): This test will FAIL until L3 wires GetQualityMetrics into
	// the pushSession path. Currently pipeline.go passes nil metrics to
	// MapMetadata (line ~306).
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)

	sid := ingest.SessionID(sessID)
	title := "Fix login bug"
	outcome := ingest.OutcomeResolved
	signalDensity := 0.85
	computeVersion := 1

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sessID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
		Metrics: map[ingest.SessionID]*schema.QualityMetrics{
			sid: {
				TitleGenerated: &title,
				Outcome:        &outcome,
				SignalDensity:  &signalDensity,
				ComputeVersion: &computeVersion,
			},
		},
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.New != 1 {
		t.Errorf("new: got %d, want 1", result.New)
	}
	if len(pub.Calls) != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", len(pub.Calls))
	}

	// Parse the published metadata JSON and verify the quality key is present.
	var payload map[string]any
	if err := json.Unmarshal(pub.Calls[0].MetadataJSON, &payload); err != nil {
		t.Fatalf("unmarshal published metadata: %v", err)
	}

	qualityRaw, exists := payload["quality"]
	if !exists {
		t.Fatal("quality key should be present in published payload when metrics exist in store")
	}
	qualityMap, ok := qualityRaw.(map[string]any)
	if !ok {
		t.Fatalf("quality is not a map, got %T", qualityRaw)
	}
	assertEqual(t, "titleGenerated", "Fix login bug", qualityMap["titleGenerated"])
	assertEqual(t, "outcome", string(ingest.OutcomeResolved), qualityMap["outcome"])
	assertFloat64(t, "signalDensity", 0.85, qualityMap["signalDensity"])
	assertFloat64(t, "computeVersion", 1, qualityMap["computeVersion"])
}

func TestPipeline_MetricsAbsent_PushSucceedsWithoutQuality(t *testing.T) {
	// When the store has NO metrics for a session (GetQualityMetrics returns nil),
	// the pipeline should still push successfully. The published JSON should
	// NOT contain the "quality" key (graceful degradation).
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sessID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
		// No Metrics map — GetQualityMetrics returns nil.
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.New != 1 {
		t.Errorf("new: got %d, want 1", result.New)
	}
	if len(pub.Calls) != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", len(pub.Calls))
	}

	// Parse the published metadata JSON and verify quality is absent.
	var payload map[string]any
	if err := json.Unmarshal(pub.Calls[0].MetadataJSON, &payload); err != nil {
		t.Fatalf("unmarshal published metadata: %v", err)
	}
	if _, exists := payload["quality"]; exists {
		t.Error("quality key should be absent in published payload when no metrics exist")
	}
}

func TestPipeline_MetricsError_PushSucceedsWithoutQuality(t *testing.T) {
	// When GetQualityMetrics returns an error, the pipeline should degrade
	// gracefully: push the session without quality metrics (no abort).
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sessID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
		GetQualityMetricsErr: fmt.Errorf("database locked"),
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.New != 1 {
		t.Errorf("new: got %d, want 1", result.New)
	}

	// Should still have pushed (no quality key).
	if len(pub.Calls) != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", len(pub.Calls))
	}
	var payload map[string]any
	if err := json.Unmarshal(pub.Calls[0].MetadataJSON, &payload); err != nil {
		t.Fatalf("unmarshal published metadata: %v", err)
	}
	if _, exists := payload["quality"]; exists {
		t.Error("quality key should be absent when GetQualityMetrics fails")
	}
}

func TestPipeline_EntriesPresent_IncludedInPayload(t *testing.T) {
	// When the store has entries for a session, the pipeline should fetch them
	// via ListEntries and include them in the published JSON under "entries".
	//
	// NOTE (L2): This test will FAIL at runtime until L3 wires ListEntries
	// into the pushSession path. Currently pipeline.go passes nil entries
	// to MapMetadata.
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)

	sid := ingest.SessionID(sessID)
	toolKind := schema.ToolCallKindRead
	stopReason := schema.StopReasonEndTurn
	preview := "file content"
	toolCallID := "tc-001"

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sessID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {
				{
					SessionID:      sid,
					EntryIndex:     0,
					Harness:        ingest.HarnessClaudeCode,
					EntryType:      ingest.EntryTypeToolUse,
					Role:           ingest.RoleAssistant,
					HasToolUse:     true,
					ToolKind:       &toolKind,
					StopReason:     &stopReason,
					ContentPreview: &preview,
					ToolCallID:     &toolCallID,
				},
			},
		},
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.New != 1 {
		t.Errorf("new: got %d, want 1", result.New)
	}
	if len(pub.Calls) != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", len(pub.Calls))
	}

	// Parse the published metadata JSON and verify entries are present.
	var payload map[string]any
	if err := json.Unmarshal(pub.Calls[0].MetadataJSON, &payload); err != nil {
		t.Fatalf("unmarshal published metadata: %v", err)
	}

	entriesRaw, exists := payload["entries"]
	if !exists {
		t.Fatal("entries key should be present in published payload when entries exist in store")
	}
	entriesList, ok := entriesRaw.([]any)
	if !ok {
		t.Fatalf("entries is not an array, got %T", entriesRaw)
	}
	if len(entriesList) != 1 {
		t.Fatalf("entries count: got %d, want 1", len(entriesList))
	}

	entry, ok := entriesList[0].(map[string]any)
	if !ok {
		t.Fatalf("entry is not a map, got %T", entriesList[0])
	}
	assertEqual(t, "entryType", string(schema.EntryTypeToolUse), entry["entryType"])
	assertEqual(t, "toolKind", string(schema.ToolCallKindRead), entry["toolKind"])
	assertEqual(t, "stopReason", string(schema.StopReasonEndTurn), entry["stopReason"])
	assertEqual(t, "toolCallId", "tc-001", entry["toolCallId"])
}

func TestPipeline_EntriesAbsent_PushSucceedsWithoutEntries(t *testing.T) {
	// When the store has NO entries for a session (ListEntries returns nil),
	// the pipeline should push successfully. The published JSON should NOT
	// contain the "entries" key (omitempty).
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sessID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
		// No Entries map — ListEntries returns nil.
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.New != 1 {
		t.Errorf("new: got %d, want 1", result.New)
	}
	if len(pub.Calls) != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", len(pub.Calls))
	}

	// Parse the published metadata JSON and verify entries is absent.
	var payload map[string]any
	if err := json.Unmarshal(pub.Calls[0].MetadataJSON, &payload); err != nil {
		t.Fatalf("unmarshal published metadata: %v", err)
	}
	if _, exists := payload["entries"]; exists {
		t.Error("entries key should be absent in published payload when no entries exist")
	}
}

func TestPipeline_EntriesError_FailsBeforeUpload(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sessID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
		ListEntriesErr: fmt.Errorf("database locked"),
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Errors != 1 {
		t.Errorf("errors: got %d, want 1", result.Errors)
	}
	if len(pub.Calls) != 0 {
		t.Fatalf("entry read failure made %d HTTP calls, want 0", len(pub.Calls))
	}
	if pub.SchemaVersionCalls != 0 || len(pub.AuthoritativeCalls) != 0 || len(store.SavedPublicationIDs) != 0 || len(store.PushLogs) != 0 || len(store.PublicationAttempts) != 0 {
		t.Fatalf("entry-read failure side effects: schema=%d authoritative=%d persistence=%d audit=%d attempts=%d", pub.SchemaVersionCalls, len(pub.AuthoritativeCalls), len(store.SavedPublicationIDs), len(store.PushLogs), len(store.PublicationAttempts))
	}
	message := result.Sessions[0].Error.Error()
	for _, fragment := range []string{"what:", "why:", "where:", "when:", "meaning:", "fix:", "no transcript bytes"} {
		if !strings.Contains(message, fragment) {
			t.Errorf("actionable entry-read error missing %q: %s", fragment, message)
		}
	}
}

func TestPipeline_EntriesRereadErrorReportsPostNegotiationStage(t *testing.T) {
	fs := testutil.NewMemFS()
	seedMemFS(t, fs, testutil.TestHostSlug, testutil.TestSessionUUID, defaults.HarnessClaudeCode)
	store := &testutil.StubPushStore{
		Sessions:       []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, string(defaults.HarnessClaudeCode), nil)},
		ListEntriesErr: fmt.Errorf("database changed after preflight"), ListEntriesFailOnCall: 2,
	}
	pub := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion}}
	var stderr bytes.Buffer
	result, err := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Errors != 1 || pub.SchemaVersionCalls != 1 || len(pub.Calls) != 0 || len(pub.AuthoritativeCalls) != 0 || len(store.SavedPublicationIDs) != 0 || len(store.PushLogs) != 1 || len(store.PublicationAttempts) != 0 {
		t.Fatalf("reread result=%+v schema=%d uploads=%d authoritative=%d persistence=%d audit=%d attempts=%d", result, pub.SchemaVersionCalls, len(pub.Calls), len(pub.AuthoritativeCalls), len(store.SavedPublicationIDs), len(store.PushLogs), len(store.PublicationAttempts))
	}
	message := result.Sessions[0].Error.Error()
	if !strings.Contains(message, "after run-level capability negotiation and before redaction, content construction, or upload") {
		t.Fatalf("post-negotiation stage missing: %s", message)
	}
	if !strings.Contains(message, "the ordinary local run audit still records this failed session") {
		t.Fatalf("truthful audit consequence missing: %s", message)
	}
}

// TestPipeline_PublishesCurrentDurableAssociations verifies the mounted push
// production path loads the store's current durable associations and sends them
// in GitContext. A store read failure is fail-closed: it must not publish a
// session with incomplete association ownership context.
func TestPipeline_PublishesCurrentDurableAssociations(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()
	const hostSlug = testutil.TestHostSlug
	sessionID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sessionID, defaults.HarnessClaudeCode)

	associationID, err := schema.NewAssociationID("assoc-00000000-0000-0000-0000-000000000002")
	if err != nil {
		t.Fatalf("NewAssociationID: %v", err)
	}
	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sessionID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
		Associations: map[ingest.SessionID][]ingest.CurrentCommitAssociation{
			ingest.SessionID(sessionID): {{
				ID:                 associationID,
				ObservedCommitHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}},
		},
	}
	publisher := &testutil.StubPublisher{StatusCode: 201}
	var stderr bytes.Buffer
	pipeline := newTestPipeline(store, publisher, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.New != 1 || len(publisher.Calls) != 1 {
		t.Fatalf("push result new=%d calls=%d, want 1 successful publish", result.New, len(publisher.Calls))
	}
	var request schema.PublishRequest
	if err := json.Unmarshal(publisher.Calls[0].MetadataJSON, &request); err != nil {
		t.Fatalf("unmarshal published request: %v", err)
	}
	if len(request.Git.Associations) != 1 {
		t.Fatalf("published associations = %d, want 1", len(request.Git.Associations))
	}
	if got := request.Git.Associations[0]; got.ID != associationID || got.ObservedCommitHash != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("published association = %+v, want ID %q and observed hash", got, associationID)
	}

	store.AssociationsErr = errors.New("durable association query failed")
	publisher.Calls = nil
	result, err = pipeline.Run(ctx)
	if err != nil {
		t.Fatalf("Run with association query failure: %v", err)
	}
	if result.Errors != 1 {
		t.Errorf("association query failure errors = %d, want 1", result.Errors)
	}
	if len(publisher.Calls) != 0 {
		t.Errorf("association query failure made %d publish calls, want 0", len(publisher.Calls))
	}
}

func TestPipeline_StatusCode_201New_200Updated(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sess1ID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sess1ID, defaults.HarnessClaudeCode)

	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sess1ID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
	}

	t.Run("201 → new", func(t *testing.T) {
		pub := &testutil.StubPublisher{StatusCode: 201}
		var stderr bytes.Buffer
		p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)
		result, err := p.Run(ctx)
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if result.New != 1 || result.Updated != 0 {
			t.Errorf("201: new=%d updated=%d, want new=1 updated=0", result.New, result.Updated)
		}
	})

	t.Run("200 → updated", func(t *testing.T) {
		pub := &testutil.StubPublisher{StatusCode: 200}
		var stderr bytes.Buffer
		p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)
		result, err := p.Run(ctx)
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if result.Updated != 1 || result.New != 0 {
			t.Errorf("200: new=%d updated=%d, want new=0 updated=1", result.New, result.Updated)
		}
	})
}

// TestPipeline_StructuredContentUpload verifies the push pipeline uploads a
// versioned, structured TranscriptContent envelope as the transcript body
// instead of raw provider JSONL. Village's migrate-on-read path
// migrate-on-read consumes exactly this shape.
func TestPipeline_StructuredContentUpload(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()

	const hostSlug = testutil.TestHostSlug
	sessID := testutil.TestSessionUUID

	seedMemFS(t, fs, hostSlug, sessID, defaults.HarnessClaudeCode)

	sid, _ := ingest.NewSessionID(sessID)
	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{
			makeSession(sessID, hostSlug, string(defaults.HarnessClaudeCode), nil),
		},
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			sid: {
				{
					SessionID:  sid,
					EntryIndex: 0,
					Harness:    defaults.HarnessClaudeCode,
					EntryType:  schema.EntryTypeText,
					Role:       schema.RoleUser,
					Depth:      0,
				},
			},
		},
	}

	pub := &testutil.StubPublisher{StatusCode: 201}
	var stderr bytes.Buffer
	p := newTestPipeline(store, pub, fs, baseTestConfig(), push.PipelineConfig{}, &stderr)

	if _, err := p.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(pub.Calls) != 1 {
		t.Fatalf("expected 1 publish call, got %d", len(pub.Calls))
	}
	body := pub.Calls[0].TranscriptBody
	if len(body) == 0 {
		t.Fatal("transcript body was empty")
	}

	// The body must be a structured TranscriptContent envelope, NOT raw JSONL.
	var env schema.TranscriptContent
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("transcript body is not a TranscriptContent envelope: %v\nbody=%s", err, body)
	}
	if env.Kind != schema.ContentKindSessionDetail {
		t.Errorf("envelope Kind: got %q, want %q", env.Kind, schema.ContentKindSessionDetail)
	}
	if env.ContractVersion != defaults.PublishSchemaVersion {
		t.Errorf("envelope ContractVersion: got %q, want %q", env.ContractVersion, defaults.PublishSchemaVersion)
	}
	if env.SessionDetail == nil {
		t.Fatal("envelope SessionDetail is nil")
	}
	if env.SessionDetail.SchemaVersion != defaults.PublishSchemaVersion {
		t.Errorf("embedded SchemaVersion: got %q, want %q",
			env.SessionDetail.SchemaVersion, defaults.PublishSchemaVersion)
	}
	if env.SessionDetail.ID != sessID {
		t.Errorf("SessionDetail.ID: got %q, want %q", env.SessionDetail.ID, sessID)
	}
	if env.SessionDetail.Harness != defaults.HarnessClaudeCode {
		t.Errorf("SessionDetail.Harness: got %q, want %q", env.SessionDetail.Harness, defaults.HarnessClaudeCode)
	}
	// Structured content must carry the unified harness key, never legacy provider.
	if strings.Contains(string(body), `"provider"`) {
		t.Errorf("structured content must not contain legacy \"provider\" key; body=%s", body)
	}
}
