// Package e2e holds the local end-to-end harness and the committed,
// redaction-safe transcript fixtures it ingests.
//
// The harness proper is in `e2e`-build-tagged files (excluded from `make check`).
// The committed fixtures + the ALWAYS-ON no-secrets gate (nosecrets_test.go) are
// UNTAGGED, so the gate runs in `make check` on every build over ALL of
// testdata/ — every committed fixture is data that must never carry a secret.
//
// FIXTURE PROVENANCE & SAFETY (see docs/e2e-fixture.md):
//
// testdata/claude-fixture/ is fully synthetic and hand-authored. It preserves the
// root/subagent layout and fields consumed by the adapter without containing any
// provider-derived user conversation. One assistant turn carries a clean synthetic Go
// code block whose identifier (PinnedCodeIdentifier) drives the Maximum-redaction
// differential test. The directory slug + cwd are synthetic (`/home/user/...`);
// only opaque session UUIDs are retained.
//
// testdata/codex-fixture/ holds two fully synthetic, hand-authored Codex CLI
// rollouts. One is a tool-using edit session (exec_command + apply_patch), and the
// other is a shorter, tool-free question and answer. Each is a
// sessions/YYYY/MM/DD/rollout-{ISO-ts}-{uuid}.jsonl whose first line is
// session_meta, followed by a turn_context carrying the required model,
// user/assistant turns (with tool calls in the tool-using session), and a
// final token_count event_msg; session_index.jsonl sits as a SIBLING of sessions/
// with one line per rollout. Fixture shape expectations live in each
// fixture-index.yaml. The bytes carry NO real home path and NO emails anywhere
// (the no-secrets gate trips on any email, including synthetic addresses).
//
// testdata/cursor-fixture/ and all legacy transcript fixtures are likewise fully
// synthetic and hand-authored. No fixture is sourced from a provider data directory.
package e2e

import (
	"path/filepath"
	"runtime"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// Fixture structural identifiers — pinned so the harness asserts on stable values
// (never on salt-derived hashes, timestamps, or S3 keys).
const (
	// FixtureSlugDir is the synthetic project-slug directory under the fixture
	// root (decodes to /home/user/projects/peasant-e2e — no real username).
	FixtureSlugDir = "-home-user-projects-peasant-e2e"

	// FixtureRootSessionID is the root session UUID (the {uuid}.jsonl filename and
	// the subagent's parent-uuid directory; they are identical).
	FixtureRootSessionID = "6a46bcd8-b365-4608-be7d-5601e2fdd805"

	// PinnedCodeIdentifier is the non-keyword identifier in the injected clean Go
	// code block. The Maximum-differential test keys on it: at Standard the rules
	// run over the code and this identifier SURVIVES, because no rule matches its
	// shape; at Maximum the AST anonymizer replaces it with an idN placeholder
	// while preserving code structure.
	//
	// It used to read the other way round - Standard masking the whole block to
	// <CODE_BLOCK>, identifier absent - which was shipped behaviour under an
	// inverted gate, and the test in this package now asserts the inverse.
	PinnedCodeIdentifier = "computeWidgetTotal"

	// ExpectedClaudeTranscripts is the transcript count the claude fixture ingests
	// to: the root session + its one subagent.
	ExpectedClaudeTranscripts = 2

	// ExpectedCodexTranscripts is the transcript count the codex fixture ingests
	// to: two independent synthetic rollouts. Codex has
	// no native subagent model, so each rollout is exactly one transcript.
	ExpectedCodexTranscripts = 2

	// FixtureClaudeSubagentSessionID is the Claude subagent session id (the
	// agent-*.jsonl basename under {root}/subagents/).
	FixtureClaudeSubagentSessionID = "agent-a083060"

	// CursorFixtureRootSessionID is the committed cursor-root session UUID.
	CursorFixtureRootSessionID = "6f4a7c29-3150-4719-a562-b9d28d84f199"

	// CursorFixtureAbortedSessionID is the committed cursor-aborted session UUID.
	CursorFixtureAbortedSessionID = "6db3c729-3f6b-46cb-92b5-121db7a20da9"

	// CursorFixtureWorkspaceSlug is the synthetic Cursor workspace directory under
	// cursor-fixture/ (decodes to CursorFixtureDecodedDir).
	CursorFixtureWorkspaceSlug = "home-user-projects-peasant-e2e"

	// CursorFixtureDecodedDir is the synthetic project path pinned in
	// cursor-fixture/fixture-index.yaml slug.decoded_dir.
	CursorFixtureDecodedDir = "/home/user/projects/peasant-e2e"

	// ExpectedCursorTranscripts is the transcript count the cursor fixture ingests
	// to: one full root session + one aborted turn_ended line.
	ExpectedCursorTranscripts = 2

	// ExpectedTranscriptCount is the total number of transcripts the mixed-provider
	// skip-gate / pull harness ingests in one manifest cycle.
	ExpectedTranscriptCount = ExpectedClaudeTranscripts + ExpectedCodexTranscripts + ExpectedCursorTranscripts

	// ExpectedPushTranscriptCount is the number of fixture transcripts with a real
	// recorded model. The Cursor fixture sessions intentionally omit message.model
	// and must be held by the push ErrNoModel gate rather than published with a
	// fake fallback model.
	ExpectedPushTranscriptCount = ExpectedClaudeTranscripts + ExpectedCodexTranscripts
)

// --- Legacy content round-trip fixtures ---
//
// LegacyRawJSONLFixtureName is the committed legacy-transcript fixture the content
// round-trip phase seeds directly into the village's S3 + transcripts table to
// prove the GET /content migrate-on-read path renders a SessionDetailPayload from
// a PRE-ENVELOPE (raw provider) blob — NOT the legacy fallback.
//
// FIXTURE PROVENANCE & SHAPE (see docs/e2e-fixture.md — do NOT regenerate):
// This is a SYNTHETIC, hand-authored raw-JSONL blob shaped for the VILLAGE
// migrate-on-read decoder (backend/internal/handler/migrator.go), NOT for the
// peasant claude adapter. The village's sniffShape classifies a blob that starts
// with '{' but does NOT json.Unmarshal cleanly as a single object (i.e. multiple
// newline-delimited rows) as raw-JSONL, then decodeRawJSONL reads TOP-LEVEL
// row["role"] and row["content"] per line. The two flat rows below therefore
// migrate to two turns whose content is preserved verbatim.
//
// It is INTENTIONALLY DISTINCT from cmd/peasant/testdata/legacy-raw-jsonl.transcript.jsonl,
// which nests role/content under message.content (claude shape) — that blob would
// decode to EMPTY content under the village decoder, so the legacy snippet
// assertion would silently pass len(turns)>0 but never see the snippet. This
// committed copy lives in internal/e2e/testdata/ (matches the FixtureSourcePath /
// CursorFixtureSourcePath convention; avoids a cross-package read of
// cmd/peasant/testdata) and is referenced via LegacyRawJSONLFixturePath().
//
// It is a FLAT FILE directly under testdata/ (not a fixture-dir with a
// fixture-index.yaml). The always-on TestFixture_NoSecrets gate scans top-level
// flat *.json/*.jsonl fixtures too (collectFixtureFiles), so this blob IS
// secret-scanned as a synthetic file with NO allowed real home path — its bytes
// must carry no secret/PII/home-path, same as the indexed fixtures.
const LegacyRawJSONLFixtureName = "legacy-raw-jsonl.transcript.jsonl"

// LegacyRawJSONLReply is the assistant-turn text in the legacy raw-JSONL
// fixture's SECOND row. The raw-JSONL case pins the EXACT migrated turns
// ([{user, LegacyFixtureSnippet}, {assistant, LegacyRawJSONLReply}]), since the
// village raw-JSONL decoder preserves top-level row content verbatim (these bytes
// are direct-seeded, never redacted).
const LegacyRawJSONLReply = "on it"

// --- Rich bare-payload legacy fixture ---
//
// LegacyBarePayloadFixtureName is a committed legacy fixture that is a SINGLE JSON
// object = a full schema.SessionDetailPayload. It exercises the village
// migrate-on-read BARE-PAYLOAD branch: sniffShape sees a blob that starts with '{'
// AND json.Unmarshal's cleanly as ONE object → ShapeBarePayload → decodeLegacyPayload,
// which whole-blob json.Unmarshal's the bytes back into a SessionDetailPayload,
// PRESERVING rich fields (ToolCalls, HasThinking) that the raw-JSONL decoder
// (role/content only) strips. This is why both fixtures exist: the raw-JSONL
// fixture is kept and the bare-payload one added because only the bare payload can
// prove rich features round-trip.
//
// SHAPE CONTRACT (asserted by the content round-trip + the always-on no-secrets
// gate that now scans flat fixtures): MUST start with '{', unmarshal cleanly as a
// single object, carry NO top-level "kind" and NO top-level "contractVersion" key
// (so it is treated as a pre-envelope bare payload, not a TranscriptContent
// envelope), and include a "harness" so decodeLegacyPayload sets the harness. Its
// turns include a ToolCalls-bearing turn (a minimal Bash execute call) and a
// HasThinking=true turn (a BOOL FLAG, not a content block). Synthetic + benign.
const LegacyBarePayloadFixtureName = "legacy-bare-payload.transcript.json"

// LegacyBarePayloadSnippet is the user-turn text in the bare-payload fixture; the
// bare-payload case requires it survives migrate-on-read into a rendered turn.
const LegacyBarePayloadSnippet = "please render the rich payload"

// LegacyBarePayloadReply is the assistant-turn text in the bare-payload fixture
// (the turn that carries the rich ToolCalls + HasThinking features).
const LegacyBarePayloadReply = "on it - building the widget now"

// LegacyBareToolID / LegacyBareToolName pin the single ToolCallDetail authored on
// the bare-payload fixture's assistant turn. The bare-payload case checks the
// migrated turn's ToolCalls[0] matches these (whole-blob unmarshal preserves them).
const (
	LegacyBareToolID   = "toolu_bare01"
	LegacyBareToolName = "Bash"
)

// LegacyFixtureSnippet is the user-turn text planted in the legacy fixture's first
// row. The content round-trip phase asserts it survives the village migrate-on-read
// path into the served SessionDetailPayload's turns. It is the meaningful guard for
// the fully-controlled legacy case (cf. the current-format case, whose snippet must
// survive the real ingest->redact->push pipeline; see CurrentRenderSnippet).
const LegacyFixtureSnippet = "please build the thing"

// CurrentRenderSnippet is a stable plain-text turn from the committed claude
// fixture's root session (an assistant message). The current-format content
// round-trip asserts it survives the real ingest->redact(standard)->push pipeline
// into the served body (lead-verified via make e2e; a benign phrase untouched by
// standard redaction). If a future fixture/redaction change drops it, make e2e
// fails loudly — re-pick a surviving snippet from internal/e2e/testdata/claude-fixture.
const CurrentRenderSnippet = "Here is a compact implementation"

// --- Version-pin constants ---
//
// These pin which ingest/content-contract version each content round-trip fixture
// represents. The served GET /content body is a BARE SessionDetailPayload whose
// SchemaVersion is ADVISORY + omitempty (github.com/peasant-labs/schema local_api.go; push_content.go) —
// the authoritative version is the envelope's ContractVersion, absent on the bare
// payload — so there is NO non-advisory, stable version surface to hard-assert.
// The pin is therefore DOCUMENTARY + STRUCTURAL: the test references these typed
// constants, and the fixture provenance is recorded here + in docs/e2e-fixture.md.
// When the ingest pipeline / content contract bumps, the pinned version becomes
// explicit and the fixture's staleness is visible.

// CurrentFormatPinnedVersion is the push-content-contract version the CURRENT-format
// fixture (the pushed synthetic claude transcript) is pinned to. It references
// defaults.PublishSchemaVersion (the single source of truth stamped into both the
// envelope ContractVersion and the embedded SessionDetailPayload.SchemaVersion in
// lockstep) rather than hardcoding the version literal.
const CurrentFormatPinnedVersion schema.PushContractVersion = defaults.PublishSchemaVersion

// LegacyPreEnvelopeVersionMarker pins the LEGACY fixture as PRE-ENVELOPE: a raw
// provider blob published before the TranscriptContent envelope existed. In this
// codebase legacy detection IS the empty BlobContractVersion (internal/pull/manifest.go;
// cmd/peasant/cmd_village_transcripts.go), so the empty contract version is the
// named pre-envelope marker. The village migrate-on-read path renders such a blob
// into a SessionDetailPayload without an authoritative envelope version.
const LegacyPreEnvelopeVersionMarker schema.PushContractVersion = ""

// FixtureSourcePath returns the absolute path to the directory that the claude
// source path must point at: the dir CONTAINING the slug dir (the Claude adapter
// then finds {slug}/{uuid}.jsonl and {slug}/{uuid}/subagents/agent-*.jsonl
// beneath it). Resolved relative to this source file so it works regardless of
// the test's cwd.
func FixtureSourcePath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata", "claude-fixture")
}

// CodexFixtureSourcePath returns the absolute path the codex source path must
// point at: the sessions/ dir ITSELF (NOT its parent). The codex adapter's
// Discover does a strict depth-4 walk of {root}/YYYY/MM/DD/rollout-*.jsonl, so
// pointing at codex-fixture/ instead of codex-fixture/sessions/ would discover
// zero sessions. The sibling session_index.jsonl is read from {root}/.. by the
// adapter, mirroring the supported provider directory shape.
func CodexFixtureSourcePath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata", "codex-fixture", "sessions")
}

// CursorFixtureSourcePath returns the absolute path the cursor source path must
// point at: the dir CONTAINING the workspace slug folder (the Cursor adapter
// then finds {workspace}/agent-transcripts/{uuid}/{uuid}.jsonl beneath it).
func CursorFixtureSourcePath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata", "cursor-fixture")
}

// LegacyRawJSONLFixturePath returns the absolute path to the committed legacy
// raw-JSONL transcript fixture (LegacyRawJSONLFixtureName) under testdata/. The
// content round-trip phase reads these bytes and seeds them directly into the
// village's S3 + transcripts table. Resolved relative to this source file so it
// works regardless of the test's cwd (mirrors FixtureSourcePath /
// CursorFixtureSourcePath).
func LegacyRawJSONLFixturePath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata", LegacyRawJSONLFixtureName)
}

// LegacyBarePayloadFixturePath returns the absolute path to the committed rich
// bare-payload legacy fixture (LegacyBarePayloadFixtureName) under testdata/. The
// content round-trip phase reads these bytes and seeds them directly into the
// village's S3 + transcripts table to exercise the migrate-on-read bare-payload
// branch. Resolved relative to this source file (mirrors LegacyRawJSONLFixturePath).
func LegacyBarePayloadFixturePath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata", LegacyBarePayloadFixtureName)
}
