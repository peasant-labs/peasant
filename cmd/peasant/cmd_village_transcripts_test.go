package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// --- seeding helpers --------------------------------------------------------

// seededPull is the on-disk + DB state a pulled transcript test starts from.
type seededPull struct {
	id      schema.TranscriptID
	host    string
	pullDir string
	turns   []schema.TurnDetail
}

// foldInvariantTurns returns a turn slice that survives the EntriesToTurns fold
// round-trip at the render level: plain message turns plus an
// assistant turn carrying a single tool call (one tool_use + one tool_result
// pair). These are exactly the entries the fold produces and re-projects without
// loss, so the golden equivalence test compares like with like.
func foldInvariantTurns() []schema.TurnDetail {
	ts := time.Unix(1_700_000_000, 0).UTC()
	return []schema.TurnDetail{
		{Index: 0, Role: schema.RoleUser, Content: "please build the thing", Timestamp: ts, EntryType: schema.EntryTypeText},
		{
			Index: 1, Role: schema.RoleAssistant, Content: "running a command", Timestamp: ts, EntryType: schema.EntryTypeText,
			ToolCalls: []schema.ToolCallDetail{
				{ID: "tool-1", Name: "Bash", Arguments: `{"command":"ls"}`, Result: "file.go", ToolKind: schema.ToolCallKindExecute},
			},
		},
		{Index: 2, Role: schema.RoleAssistant, Content: "done", Timestamp: ts, EntryType: schema.EntryTypeText},
	}
}

// seedPulledTranscript writes a valid envelope blob + manifest into the
// village-pulls dir under dir AND inserts a pulled_transcripts DB row, so the
// offline `context` and `list --local` commands find a real local copy. It is
// the "seed pulled data" step the positive offline auth cells require.
func seedPulledTranscript(t *testing.T, dir string, turns []schema.TurnDetail) seededPull {
	t.Helper()

	id := schema.TranscriptID(testutil.TestSessionUUID)
	host := "village.example.test"
	pullsRoot := string(defaults.ResolveVillagePullsDirPathWith(dir))
	pullDir := filepath.Join(pullsRoot, host, id.String())
	if err := os.MkdirAll(pullDir, 0o755); err != nil {
		t.Fatalf("mkdir pull dir: %v", err)
	}

	// Valid TranscriptContent envelope blob.
	envelope := schema.TranscriptContent{
		ContractVersion: defaults.PublishSchemaVersion,
		Kind:            schema.ContentKindSessionDetail,
		SessionDetail: &schema.SessionDetailPayload{
			ID:      id.String(),
			Harness: defaults.HarnessClaudeCode,
			Turns:   turns,
		},
	}
	blob, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pullDir, pull.TranscriptFilename), blob, 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	// Manifest recording a KNOWN blob contract (non-legacy).
	manifest := pull.PullManifest{
		ManifestVersion:     pull.PullManifestVersion,
		VillageURL:          "https://" + host,
		VillageHost:         host,
		TranscriptID:        id,
		OwnerUserID:         "owner-1",
		OwnerUsername:       "owneruser",
		BlobContractVersion: defaults.PublishSchemaVersion,
		PullEnvelopeVersion: defaults.PullContractVersion,
		PulledAt:            1_700_000_000_000,
	}
	writeManifest(t, pullDir, manifest)

	commitDBRow(t, dir, store.PulledTranscriptRow{
		VillageHost:   host,
		TranscriptID:  id,
		OwnerUserID:   "owner-1",
		OwnerUsername: "owneruser",
		Title:         "Build the thing",
		Harness:       defaults.HarnessClaudeCode,
		PullDir:       pullDir,
		FirstPulledAt: 1_700_000_000_000,
		LastPulledAt:  1_700_000_000_000,
	})

	return seededPull{id: id, host: host, pullDir: pullDir, turns: turns}
}

// seedLegacyPulledTranscript writes the COMMITTED legacy raw-JSONL fixture
// (testdata/legacy-raw-jsonl.transcript.jsonl) as the served blob plus a
// manifest with an EMPTY BlobContractVersion, then inserts a DB row — the
// pre-envelope blob case.
func seedLegacyPulledTranscript(t *testing.T, dir string) seededPull {
	t.Helper()

	id := schema.TranscriptID(testutil.TestSessionUUID2)
	host := "village.example.test"
	pullsRoot := string(defaults.ResolveVillagePullsDirPathWith(dir))
	pullDir := filepath.Join(pullsRoot, host, id.String())
	if err := os.MkdirAll(pullDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy pull dir: %v", err)
	}

	fixture, err := os.ReadFile(filepath.Join("testdata", "legacy-raw-jsonl.transcript.jsonl"))
	if err != nil {
		t.Fatalf("read legacy fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pullDir, pull.TranscriptFilename), fixture, 0o600); err != nil {
		t.Fatalf("write legacy blob: %v", err)
	}

	// Manifest with empty BlobContractVersion ⇒ legacy (manifest-axis detection).
	manifest := pull.PullManifest{
		ManifestVersion:     pull.PullManifestVersion,
		VillageURL:          "https://" + host,
		VillageHost:         host,
		TranscriptID:        id,
		OwnerUserID:         "owner-1",
		OwnerUsername:       "owneruser",
		BlobContractVersion: "", // legacy
		PullEnvelopeVersion: defaults.PullContractVersion,
		PulledAt:            1_700_000_000_000,
	}
	writeManifest(t, pullDir, manifest)

	commitDBRow(t, dir, store.PulledTranscriptRow{
		VillageHost:   host,
		TranscriptID:  id,
		OwnerUserID:   "owner-1",
		OwnerUsername: "owneruser",
		Harness:       defaults.HarnessClaudeCode,
		PullDir:       pullDir,
		FirstPulledAt: 1_700_000_000_000,
		LastPulledAt:  1_700_000_000_000,
	})

	return seededPull{id: id, host: host, pullDir: pullDir}
}

func writeManifest(t *testing.T, pullDir string, m pull.PullManifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pullDir, pull.ManifestFilename), b, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// commitDBRow opens the real store at dir and inserts the pulled_transcripts row
// via CommitPull (the production write path), then closes the store so the
// command-under-test can re-open it.
func commitDBRow(t *testing.T, dir string, row store.PulledTranscriptRow) {
	t.Helper()
	dataDir := string(defaults.ResolveDataDirPathWith(dir))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.CommitPull(t.Context(), store.PullCommit{Transcript: row}); err != nil {
		t.Fatalf("commit pull row: %v", err)
	}
}

// writeCreds writes a valid credentials.json so a village-contacting command
// passes the auth gate. The village URL points at an unreachable host so the
// command fails PAST the gate (a transport error), proving the gate admits the
// creds without needing a live village.
func writeCreds(t *testing.T, dir string) {
	t.Helper()
	peasantDir := string(defaults.ResolveConfigDirPathWith(dir))
	if err := os.MkdirAll(peasantDir, 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	creds := `{
		"api_key": "test-api-key",
		"key_id": "test-key-id",
		"user_id": "user-1",
		"username": "testuser",
		"village_url": "http://127.0.0.1:1/unreachable",
		"linked_at": "2025-01-01T00:00:00Z"
	}`
	if err := os.WriteFile(filepath.Join(peasantDir, string(defaults.CredentialsFile)), []byte(creds), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
}

// --- 5x2 auth matrix ---------------------------------------------------------

// TestVillageAuthMatrix_LoggedOut asserts each of the 5 commands logged-out:
// the THREE village-contacting commands fail with the actionable login error;
// the TWO offline commands (list --local, context) succeed WITHOUT credentials.
func TestVillageAuthMatrix_LoggedOut(t *testing.T) {
	t.Parallel()

	const loginHint = "peasant village login"

	t.Run("pull_logged_out_fails_with_login_error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out, err := runVillage(t, dir, "transcripts", "pull", testutil.TestSessionUUID)
		assertLoginError(t, out, err, loginHint)
	})

	t.Run("list_remote_logged_out_fails_with_login_error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out, err := runVillage(t, dir, "transcripts", "list")
		assertLoginError(t, out, err, loginHint)
	})

	t.Run("annotations_sync_logged_out_fails_with_login_error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out, err := runVillage(t, dir, "annotations", "sync")
		assertLoginError(t, out, err, loginHint)
	})

	// Offline-ALLOWED cell #1: list --local succeeds logged-out (positive).
	t.Run("list_local_logged_out_succeeds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		seedPulledTranscript(t, dir, foldInvariantTurns())
		out, err := runVillage(t, dir, "transcripts", "list", "--local")
		if err != nil {
			t.Fatalf("list --local logged-out must succeed, got err=%v out=%q", err, out)
		}
		if !strings.Contains(out, testutil.TestSessionUUID) {
			t.Errorf("list --local must show the seeded transcript; out=%q", out)
		}
		// The seeded row carries no license: the LICENSE column renders "-".
		if !strings.Contains(out, "LICENSE") {
			t.Errorf("list --local must show the LICENSE column; out=%q", out)
		}
		if !regexp.MustCompile(`claude-code\s+-\s`).MatchString(out) {
			t.Errorf("unlicensed row must render '-' in the LICENSE cell; out=%q", out)
		}
	})

	// Offline-ALLOWED cell #2: context succeeds logged-out (positive).
	t.Run("context_logged_out_succeeds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		seed := seedPulledTranscript(t, dir, foldInvariantTurns())
		out, err := runVillage(t, dir, "transcripts", "context", seed.id.String())
		if err != nil {
			t.Fatalf("context logged-out must succeed, got err=%v out=%q", err, out)
		}
		if !strings.Contains(out, "please build the thing") {
			t.Errorf("context must render the transcript content; out=%q", out)
		}
		// License absence is legal information: the header line is UNCONDITIONAL.
		if !strings.Contains(out, "license: none (all rights reserved)") {
			t.Errorf("context header must state the license unconditionally; out=%q", out)
		}
	})
}

// TestVillageAuthMatrix_LoggedIn asserts each command logged-in PASSES the auth
// gate. The two offline commands behave as before; the three village-contacting
// commands get PAST the gate and fail on the UNREACHABLE village instead of the
// login error, proving valid credentials are admitted in the logged-in column.
func TestVillageAuthMatrix_LoggedIn(t *testing.T) {
	t.Parallel()

	assertPastGate := func(t *testing.T, out string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("expected a village-contact error (unreachable), got nil; out=%q", out)
		}
		if strings.Contains(err.Error(), "not logged in") {
			t.Fatalf("logged-in command must pass the auth gate, got login error: %v", err)
		}
	}

	t.Run("pull_logged_in_passes_gate", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCreds(t, dir)
		out, err := runVillage(t, dir, "transcripts", "pull", testutil.TestSessionUUID)
		assertPastGate(t, out, err)
	})

	t.Run("list_remote_logged_in_passes_gate", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCreds(t, dir)
		out, err := runVillage(t, dir, "transcripts", "list")
		assertPastGate(t, out, err)
	})

	t.Run("annotations_sync_logged_in_passes_gate", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCreds(t, dir)
		out, err := runVillage(t, dir, "annotations", "sync")
		assertPastGate(t, out, err)
	})

	t.Run("list_local_logged_in_succeeds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCreds(t, dir)
		seedPulledTranscript(t, dir, foldInvariantTurns())
		out, err := runVillage(t, dir, "transcripts", "list", "--local")
		if err != nil {
			t.Fatalf("list --local logged-in must succeed, got err=%v out=%q", err, out)
		}
	})

	t.Run("context_logged_in_succeeds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCreds(t, dir)
		seed := seedPulledTranscript(t, dir, foldInvariantTurns())
		out, err := runVillage(t, dir, "transcripts", "context", seed.id.String())
		if err != nil {
			t.Fatalf("context logged-in must succeed, got err=%v out=%q", err, out)
		}
	})
}

// --- legacy fixture case -----------------------------------------------------

// assertUnsupportedBlobContract asserts a `context` invocation failed with the
// actionable "unsupported blob contract" error naming the fix (re-push).
func assertUnsupportedBlobContract(t *testing.T, out string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("blob must error; out=%q", out)
	}
	if !strings.Contains(err.Error(), "unsupported blob contract") {
		t.Errorf("expected 'unsupported blob contract' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "re-push") {
		t.Errorf("legacy error must be actionable (name the fix), got: %v", err)
	}
}

// seedDecodeAxisPulledTranscript seeds a pulled transcript whose manifest records
// a NON-EMPTY BlobContractVersion (so the manifest-axis at :245 does NOT fire) but
// whose served blob is `blob` — exercising the DECODE axis (:259-267). The
// transcript ID is parameterized so parallel sub-cases do not collide.
func seedDecodeAxisPulledTranscript(t *testing.T, dir string, id schema.TranscriptID, blob []byte) seededPull {
	t.Helper()

	host := "village.example.test"
	pullsRoot := string(defaults.ResolveVillagePullsDirPathWith(dir))
	pullDir := filepath.Join(pullsRoot, host, id.String())
	if err := os.MkdirAll(pullDir, 0o755); err != nil {
		t.Fatalf("mkdir decode-axis pull dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pullDir, pull.TranscriptFilename), blob, 0o600); err != nil {
		t.Fatalf("write decode-axis blob: %v", err)
	}

	// Manifest with a KNOWN (non-empty) blob contract — the manifest axis passes,
	// so detection must fall through to the decode axis.
	manifest := pull.PullManifest{
		ManifestVersion:     pull.PullManifestVersion,
		VillageURL:          "https://" + host,
		VillageHost:         host,
		TranscriptID:        id,
		OwnerUserID:         "owner-1",
		OwnerUsername:       "owneruser",
		BlobContractVersion: defaults.PublishSchemaVersion, // non-empty ⇒ NOT manifest-axis legacy
		PullEnvelopeVersion: defaults.PullContractVersion,
		PulledAt:            1_700_000_000_000,
	}
	writeManifest(t, pullDir, manifest)

	commitDBRow(t, dir, store.PulledTranscriptRow{
		VillageHost:   host,
		TranscriptID:  id,
		OwnerUserID:   "owner-1",
		OwnerUsername: "owneruser",
		Harness:       defaults.HarnessClaudeCode,
		PullDir:       pullDir,
		FirstPulledAt: 1_700_000_000_000,
		LastPulledAt:  1_700_000_000_000,
	})

	return seededPull{id: id, host: host, pullDir: pullDir}
}

// TestVillageContext_LegacyBlob_ActionableError asserts BOTH legacy-detection axes
// surface the same actionable "unsupported blob contract" error:
//   - the MANIFEST axis (empty BlobContractVersion, via the committed raw-JSONL
//     fixture);
//   - the DECODE axis (non-empty BlobContractVersion, but the blob is garbage /
//     carries the wrong envelope Kind) — the defensive branch that catches a
//     manifest/blob disagreement.
func TestVillageContext_LegacyBlob_ActionableError(t *testing.T) {
	t.Parallel()

	t.Run("manifest_axis_empty_contract_version", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		seed := seedLegacyPulledTranscript(t, dir)
		out, err := runVillage(t, dir, "transcripts", "context", seed.id.String())
		assertUnsupportedBlobContract(t, out, err)
	})

	// DECODE axis (i): a non-JSON garbage blob under a non-empty contract version.
	t.Run("decode_axis_garbage_blob", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		id := schema.TranscriptID(testutil.TestTranscriptUUID)
		seed := seedDecodeAxisPulledTranscript(t, dir, id, []byte("this is not json at all {{{"))
		out, err := runVillage(t, dir, "transcripts", "context", seed.id.String())
		assertUnsupportedBlobContract(t, out, err)
	})

	// DECODE axis (ii): a valid-JSON envelope whose Kind is NOT session_detail.
	t.Run("decode_axis_wrong_envelope_kind", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		id := schema.TranscriptID(testutil.TestTranscriptUUID2)
		wrongKind := schema.TranscriptContent{
			ContractVersion: defaults.PublishSchemaVersion,
			Kind:            schema.ContentKind("not-a-session-detail"),
		}
		blob, err := json.Marshal(wrongKind)
		if err != nil {
			t.Fatalf("marshal wrong-kind envelope: %v", err)
		}
		seed := seedDecodeAxisPulledTranscript(t, dir, id, blob)
		out, runErr := runVillage(t, dir, "transcripts", "context", seed.id.String())
		assertUnsupportedBlobContract(t, out, runErr)
	})
}

// --- golden output-equivalence -----------------------------------------------

// TestVillageContext_OutputEquivalence pins the renderer-reuse contract: the
// SAME entry data rendered via the sessions-context path and via the
// transcripts-context path (TurnsToEntries projection) produces IDENTICAL output,
// modulo the pinned provenance header. Because the transcripts-context path goes
// turns→entries while sessions-context goes entries directly, the fixture is
// chosen fold-invariant so EntriesToTurns(TurnsToEntries(turns)) preserves render.
func TestVillageContext_OutputEquivalence(t *testing.T) {
	t.Parallel()

	turns := foldInvariantTurns()
	entries := TurnsToEntries(turns)

	// (a) Render via the sessions-context renderer directly (the reference path).
	refCmd := newTestRoot()
	var refBuf bytes.Buffer
	refCmd.SetOut(&refBuf)
	refCmd.SetErr(&refBuf)
	renderSessionContextHuman(refCmd, schema.SessionID(testutil.TestSessionUUID), -1, entries, "", defaults.ToolCallFormatVerbose)
	refOut := refBuf.String()

	// (b) Render via the transcripts-context command over a seeded pull. Strip the
	// pinned provenance header (everything up to and including the first blank
	// line) so the comparison is body-vs-body.
	dir := t.TempDir()
	seed := seedPulledTranscript(t, dir, turns)
	ctxOut, err := runVillage(t, dir, "transcripts", "context", seed.id.String())
	if err != nil {
		t.Fatalf("context command failed: %v", err)
	}
	body := stripProvenanceHeader(ctxOut)

	if body != refOut {
		t.Errorf("transcripts-context body must equal sessions-context output\n--- sessions-context ---\n%s\n--- transcripts-context body ---\n%s", refOut, body)
	}
}

// stripProvenanceHeader removes the provenance header block (up to and including
// the first blank line) from the human context output.
func stripProvenanceHeader(out string) string {
	idx := strings.Index(out, "\n\n")
	if idx < 0 {
		return out
	}
	return out[idx+2:]
}

// --- CLI flag / exit-code tests ---------------------------------------------

// TestVillageContext_InvalidRef asserts a malformed ref errors before any I/O.
func TestVillageContext_InvalidRef(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := runVillage(t, dir, "transcripts", "context", "not-a-uuid")
	if err == nil {
		t.Fatal("invalid ref must error")
	}
	if !strings.Contains(err.Error(), "transcript") {
		t.Errorf("expected an actionable ref error, got: %v", err)
	}
}

// TestVillageContext_InvalidFormatToolCalls asserts an invalid --format-tool-calls
// value fails fast.
func TestVillageContext_InvalidFormatToolCalls(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := runVillage(t, dir, "transcripts", "context", testutil.TestSessionUUID, "--format-tool-calls", "bogus")
	if err == nil {
		t.Fatal("invalid --format-tool-calls must error")
	}
	if !strings.Contains(err.Error(), "format-tool-calls") {
		t.Errorf("expected format-tool-calls error, got: %v", err)
	}
}

// TestVillageContext_JSON asserts --json emits parseable output for a seeded pull.
func TestVillageContext_JSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed := seedPulledTranscript(t, dir, foldInvariantTurns())
	out, err := runVillage(t, dir, "transcripts", "context", seed.id.String(), "--json")
	if err != nil {
		t.Fatalf("context --json failed: %v", err)
	}
	var parsed pulledContextJSON
	if jErr := json.Unmarshal([]byte(out), &parsed); jErr != nil {
		t.Fatalf("context --json must be parseable: %v\nout=%q", jErr, out)
	}
	if parsed.TranscriptID != seed.id.String() {
		t.Errorf("json transcriptId = %q, want %q", parsed.TranscriptID, seed.id)
	}
	if len(parsed.Entries) == 0 {
		t.Errorf("json entries must be non-empty")
	}
}

// TestVillageListLocal_JSON asserts list --local --json is parseable.
func TestVillageListLocal_JSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPulledTranscript(t, dir, foldInvariantTurns())
	out, err := runVillage(t, dir, "transcripts", "list", "--local", "--json")
	if err != nil {
		t.Fatalf("list --local --json failed: %v", err)
	}
	var rows []jsonLocalListRow
	if jErr := json.Unmarshal([]byte(out), &rows); jErr != nil {
		t.Fatalf("list --local --json must be parseable: %v\nout=%q", jErr, out)
	}
	if len(rows) != 1 || rows[0].TranscriptID != testutil.TestSessionUUID {
		t.Errorf("expected one seeded row, got %+v", rows)
	}
}

// TestVillageContext_TurnWindow asserts --turn narrows the rendered window.
func TestVillageContext_TurnWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed := seedPulledTranscript(t, dir, foldInvariantTurns())

	// Whole transcript (no --turn) contains both the first user message and the
	// final "done" turn.
	whole, err := runVillage(t, dir, "transcripts", "context", seed.id.String())
	if err != nil {
		t.Fatalf("whole context failed: %v", err)
	}
	if !strings.Contains(whole, "please build the thing") || !strings.Contains(whole, "done") {
		t.Fatalf("whole render missing content; out=%q", whole)
	}

	// Windowed around entry 0 with -C 0 shows only the first entry, not "done".
	windowed, err := runVillage(t, dir, "transcripts", "context", seed.id.String(), "--turn", "0", "-C", "0")
	if err != nil {
		t.Fatalf("windowed context failed: %v", err)
	}
	if !strings.Contains(windowed, "please build the thing") {
		t.Errorf("windowed render must include the centered entry; out=%q", windowed)
	}
	if strings.Contains(windowed, "done") {
		t.Errorf("windowed render (-C 0 around entry 0) must NOT include the last turn; out=%q", windowed)
	}
}

// --- shared command runner --------------------------------------------------

// runVillage runs `peasant village <args...>` under a root carrying
// --data-dir/--config-dir/--state-dir all pointed at dir, capturing combined
// stdout+stderr. It MIRRORS main()'s rootCmd, which sets NEITHER SilenceUsage
// NOR SilenceErrors — so the buffer faithfully reflects what an end user sees,
// INCLUDING any cobra Usage/Flags dump. This is what lets the auth-matrix tests
// assert that the per-command RunE `cmd.SilenceUsage = true` fix suppresses the
// usage dump on runtime errors (the captured `err` carries the message; the
// buffer must NOT contain the "Usage:" block).
func runVillage(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	root := newTestRoot()
	village := BuildVillageCommand()
	root.AddCommand(village)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "village"}, args...))
	err := root.Execute()
	return buf.String(), err
}

// assertLoginError asserts a village-contacting command failed logged-out with
// the actionable login error AND that the cobra Usage/Flags dump was SUPPRESSED.
// The auth gate is a RUNTIME failure, not a usage error, so the one-liner must
// stand alone: the returned error names the login hint, the
// captured output surfaces it, and the output must NOT contain the "Usage:"
// block that cobra prints by default on a RunE error.
func assertLoginError(t *testing.T, out string, err error, loginHint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected login error, got nil; out=%q", out)
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("expected 'not logged in' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), loginHint) {
		t.Errorf("login error must name %q, got: %v", loginHint, err)
	}
	// The actionable one-liner must reach the user's terminal (cobra prints the
	// error to stderr, which the test buffer captures).
	if !strings.Contains(out, "not logged in") {
		t.Errorf("login one-liner must appear in command output; out=%q", out)
	}
	// The usage dump must be suppressed on this runtime error.
	if strings.Contains(out, "Usage:") {
		t.Errorf("usage dump must be suppressed on the auth-gate runtime error; out=%q", out)
	}
}
