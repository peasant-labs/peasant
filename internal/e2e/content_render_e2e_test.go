//go:build e2e

// Content round-trip phase. It proves the village's GET /content migrate-on-read
// path renders a bare SessionDetailPayload (turns + snippet survived) — NOT the
// legacy fallback — for BOTH:
//
//   - a REAL-PUSHED current-format transcript (the claude root session that
//     push#1 already published, version-pinned to CurrentFormatPinnedVersion =
//     defaults.PublishSchemaVersion), and
//   - committed legacy raw-JSONL and bare-payload bytes, encrypted through the
//     real Village publish path and then version-pinned PRE-ENVELOPE via
//     LegacyPreEnvelopeVersionMarker = empty BlobContractVersion.
//
// The shared assertion (assertContentRendersViaPayload) GETs the content endpoint
// with a Bearer key and asserts: 200; the body unmarshals to a bare
// schema.SessionDetailPayload; len(Turns)>0; NEGATIVE — no top-level "kind" key
// (it is the bare payload, NOT a TranscriptContent envelope); and, when a snippet
// is requested, the snippet appears in some turn's content. Every assertion routes
// through check()/fatalActionable(opts) so `make e2e` (assert) fails loudly while
// `make demo` logs only.
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/peasant-labs/schema"
)

// expectedContentShape declares the per-case shape assertions layered on top of
// the always-checked migrate-on-read contract (200 + bare payload + no-"kind" +
// len(turns)>0). Every field is independently optional so ONE struct serves all
// three cases: the EXACT un-redacted legacy turns (exactTurns), the STRUCTURAL
// post-redaction current-format case (allRolesAndContent + snippetRole), and the
// rich bare-payload case (richTurn). snippet, when set, must appear in some turn.
type expectedContentShape struct {
	// snippet, when non-empty, must appear in some rendered turn's content.
	snippet string
	// snippetRole, when set (non-""), requires the turn containing snippet to have
	// exactly this role. Used by the redaction-lossy current-format case, where
	// exact content cannot be pinned but the snippet's role can.
	snippetRole schema.Role
	// allRolesAndContent, when true, requires EVERY rendered turn to have a
	// non-empty role AND non-empty content (a structural floor for cases where
	// exact turns cannot be pinned).
	allRolesAndContent bool
	// exactTurns, when non-nil, requires the rendered turns to match these
	// role+content EXACTLY (same count + same per-index role and content). Used by
	// production-encrypted, never-document-redacted legacy cases.
	exactTurns []expectedTurn
	// richTurn, when non-nil, asserts a specific rendered turn carries the authored
	// rich features (role + HasThinking + ToolCalls) — the bare-payload case,
	// proving the village whole-blob decoder preserves them.
	richTurn *expectedRichTurn
}

// expectedTurn is one pinned (role, content) pair for expectedContentShape.exactTurns.
type expectedTurn struct {
	role    schema.Role
	content string
}

// expectedRichTurn pins the rich features the bare-payload case requires on the
// turn at index. toolCalls must be present and each must match an expectedToolCall.
type expectedRichTurn struct {
	index       int
	role        schema.Role
	hasThinking bool
	toolCalls   []expectedToolCall
}

// expectedToolCall pins the identity + ACP kind of one migrated schema.ToolCallDetail.
type expectedToolCall struct {
	id       string
	name     string
	toolKind schema.ToolCallKind
}

// legacySeedSpec parameterizes seedLegacyTranscript over the two committed legacy
// fixtures (the raw-JSONL fixture and the bare-payload fixture). Both use the real
// encrypted writer and differ only in committed bytes and declared source format.
type legacySeedSpec struct {
	fixturePath    string // absolute path to the committed fixture bytes
	format         schema.SourceFormat
	plaintextProbe string
}

// rawJSONLSeedSpec seeds the raw-JSONL legacy fixture (role/content rows).
func rawJSONLSeedSpec() legacySeedSpec {
	return legacySeedSpec{
		fixturePath:    LegacyRawJSONLFixturePath(),
		format:         schema.SourceFormatJSONL,
		plaintextProbe: LegacyFixtureSnippet,
	}
}

// barePayloadSeedSpec seeds the bare-payload legacy fixture (a whole
// SessionDetailPayload object with rich ToolCalls + HasThinking).
func barePayloadSeedSpec() legacySeedSpec {
	return legacySeedSpec{
		fixturePath:    LegacyBarePayloadFixturePath(),
		format:         schema.SourceFormatJSON,
		plaintextProbe: LegacyBarePayloadSnippet,
	}
}

type legacyStorageSnapshot struct {
	nonVersionRow       string
	schemaVersion       string
	blobKey             string
	wrappedDataKey      []byte
	encryptionAlgorithm string
	keyVersion          int32
	contentHash         sql.NullString
	blobSizeBytes       sql.NullInt64
}

// contentRenderPhase drives the content round-trip. It runs AFTER push#1
// (the current-format transcript is already published) and seeds legacy bytes
// through the same encrypted writer. It is skip-capable: the legacy half needs
// harness-owned Postgres to resolve rows and install the historical version marker,
// so an external stack logs and skips that half.
func contentRenderPhase(t *testing.T, opts harnessOptions, stack harnessStack, apiKey, configHome string) {
	t.Helper()

	// --- current-format case: the real-pushed claude root transcript ---
	// Resolve its village UUID by the publisher's local session id. This needs the
	// owner id from credentials plus listVillageTranscripts, which requires the
	// harness's own Postgres. On an external stack, skip the whole phase because we
	// cannot resolve the UUID or seed the legacy blob through a harness-owned DB.
	if stack.external {
		t.Logf("content-render: external stack — skipping content round-trip phase (needs harness Postgres for UUID resolution and legacy version marker)")
		return
	}
	if stack.db == nil {
		fatalActionable(t, actionableFailure{
			title: "content round-trip database handle missing",
			what:  "the self-provisioned harness stack has no database/sql handle",
			why:   "content rendering must resolve and seed transcripts through the harness-owned Postgres connection",
			where: "internal/e2e/content_render_e2e_test.go contentRenderPhase",
			when:  "starting the content round-trip phase",
			means: "the phase cannot prove encrypted current transcript rendering and would otherwise nil-panic",
			fix:   "use the harnessStack returned by provisionHarnessStack without clearing its database handle",
		})
	}

	ownerID := readDemoUserID(t, configHome)
	vts := listVillageTranscripts(t, stack.db, ownerID)
	current := villageTranscriptByLocalID(t, vts, FixtureRootSessionID)

	// CURRENT-FORMAT: STRUCTURAL only. Post-redaction(standard) the exact content
	// cannot be pinned, so we assert the snippet survives AND lands on an ASSISTANT
	// turn, and that every rendered turn carries a non-empty role+content.
	// CurrentRenderSnippet was lead-verified present in the served body via make e2e.
	// Version pin (documentary): CurrentFormatPinnedVersion = defaults.PublishSchemaVersion.
	assertContentRendersViaPayload(t, opts, stack.villageURL, apiKey, current.ID, expectedContentShape{
		snippet:            CurrentRenderSnippet,
		snippetRole:        schema.RoleAssistant,
		allRolesAndContent: true,
	})

	// --- legacy raw-JSONL: production-encrypted pre-envelope RAW-JSONL bytes ---
	// Never redacted, so EXACT turns are pinned — the village raw-JSONL decoder
	// preserves top-level role/content verbatim (it strips everything else).
	// Version pin (documentary): LegacyPreEnvelopeVersionMarker = empty BlobContractVersion.
	legacyID := seedLegacyTranscript(t, opts, stack, ownerID, apiKey, rawJSONLSeedSpec())
	assertContentRendersViaPayload(t, opts, stack.villageURL, apiKey, legacyID, expectedContentShape{
		snippet: LegacyFixtureSnippet,
		exactTurns: []expectedTurn{
			{role: schema.RoleUser, content: LegacyFixtureSnippet},
			{role: schema.RoleAssistant, content: LegacyRawJSONLReply},
		},
	})

	// --- legacy bare-payload: production-encrypted pre-envelope BARE-PAYLOAD bytes ---
	// A whole-object SessionDetailPayload, so the village bare-payload decoder
	// (whole-blob json.Unmarshal) preserves the rich ToolCalls + HasThinking fields
	// that the raw-JSONL decoder strips. Asserts exact turns AND the rich features.
	bareID := seedLegacyTranscript(t, opts, stack, ownerID, apiKey, barePayloadSeedSpec())
	assertContentRendersViaPayload(t, opts, stack.villageURL, apiKey, bareID, expectedContentShape{
		snippet: LegacyBarePayloadSnippet,
		exactTurns: []expectedTurn{
			{role: schema.RoleUser, content: LegacyBarePayloadSnippet},
			{role: schema.RoleAssistant, content: LegacyBarePayloadReply},
		},
		richTurn: &expectedRichTurn{
			index:       1,
			role:        schema.RoleAssistant,
			hasThinking: true,
			toolCalls: []expectedToolCall{
				{id: LegacyBareToolID, name: LegacyBareToolName, toolKind: schema.ToolCallKindExecute},
			},
		},
	})

	t.Logf("content-render: current=%s (structural, snippet %q) + legacy-raw=%s (exact 2 turns) + legacy-bare=%s (rich: toolcalls+thinking) rendered via SessionDetailPayload",
		current.ID, CurrentRenderSnippet, legacyID, bareID)
}

// assertContentRendersViaPayload is the shared integration point. It GETs
// the village content endpoint with a Bearer key and asserts the migrate-on-read
// contract: 200; a bare schema.SessionDetailPayload (turns preserved); NEGATIVE no
// top-level "kind" (proving it is the bare payload, not a TranscriptContent
// envelope, and not the legacy fallback). The per-case EXPECTED-SHAPE checks (exact
// turns, structural role+content floor, snippet-on-role, and rich ToolCalls +
// HasThinking) are declared via `want` and applied by assertContentShape. Every
// assertion routes through check()/fatalActionable(opts).
func assertContentRendersViaPayload(t *testing.T, opts harnessOptions, villageURL, apiKey, transcriptID string, want expectedContentShape) {
	t.Helper()

	url := fmt.Sprintf("%s/api/v1/transcripts/%s/content", villageURL, transcriptID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "content round-trip request build failed",
			what:  fmt.Sprintf("could not build GET %s", url),
			why:   err.Error(),
			where: "internal/e2e/content_render_e2e_test.go assertContentRendersViaPayload",
			when:  "preparing the content round-trip GET",
			means: "the content round-trip assertion cannot run",
			fix:   "check the village URL and transcript id are well-formed",
		})
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "content round-trip GET failed",
			what:  fmt.Sprintf("GET %s did not return a response", url),
			why:   err.Error(),
			where: "internal/e2e/content_render_e2e_test.go assertContentRendersViaPayload",
			when:  "issuing the content round-trip GET",
			means: "the village content endpoint is unreachable or the harness server crashed",
			fix:   "check the village server is healthy and the transcript is owned by / shared with the authenticated user",
		})
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// (a) 200.
	check(t, opts, resp.StatusCode == http.StatusOK,
		"content round-trip GET %s status = %d, want 200\nbody: %s", url, resp.StatusCode, body)
	if resp.StatusCode != http.StatusOK {
		return
	}

	// (d) NEGATIVE: no top-level "kind" key. A bare SessionDetailPayload has no
	// "kind" field; only a TranscriptContent envelope does (json:"kind"). Decode to
	// a generic map FIRST so we assert on the raw served shape, not a lossy struct
	// unmarshal (which would silently drop a stray "kind").
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		check(t, opts, false,
			"content round-trip body for %s is not a JSON object: %v\nbody: %s", transcriptID, err, body)
		return
	}
	_, hasKind := raw["kind"]
	check(t, opts, !hasKind,
		"content round-trip body for %s has a top-level \"kind\" key — it is a TranscriptContent envelope, not a bare SessionDetailPayload (migrate-on-read did not render the payload)\nbody: %s",
		transcriptID, body)

	// (b) unmarshals to schema.SessionDetailPayload.
	var payload schema.SessionDetailPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		check(t, opts, false,
			"content round-trip body for %s does not unmarshal to schema.SessionDetailPayload: %v\nbody: %s",
			transcriptID, err, body)
		return
	}

	// (c) len(Turns) > 0.
	turnsNonEmpty := len(payload.Turns) > 0
	check(t, opts, turnsNonEmpty,
		"content round-trip for %s rendered %d turns, want >0 (migrate-on-read produced an empty payload — likely the legacy fallback)\nbody: %s",
		transcriptID, len(payload.Turns), body)
	if !turnsNonEmpty {
		return // nothing to shape-check; avoid indexing an empty payload in demo mode
	}

	// (e) per-case EXPECTED-SHAPE assertions.
	assertContentShape(t, opts, transcriptID, payload, want, body)
}

// assertContentShape layers the per-case expected-shape checks onto an already
// base-validated payload (200 + bare + no-"kind" + len(turns)>0). See
// expectedContentShape. Length/index-dependent checks return early on mismatch so
// demo mode (where check() only logs) never panics on an out-of-range turn.
func assertContentShape(t *testing.T, opts harnessOptions, transcriptID string, payload schema.SessionDetailPayload, want expectedContentShape, body []byte) {
	t.Helper()

	// EXACT turns: same count + per-index role/content (un-redacted legacy cases).
	if want.exactTurns != nil {
		exactLen := len(payload.Turns) == len(want.exactTurns)
		check(t, opts, exactLen,
			"content round-trip for %s rendered %d turns, want exactly %d\nbody: %s",
			transcriptID, len(payload.Turns), len(want.exactTurns), body)
		if !exactLen {
			return
		}
		for i, et := range want.exactTurns {
			got := payload.Turns[i]
			check(t, opts, got.Role == et.role,
				"content round-trip for %s turn[%d] role = %q, want %q\nbody: %s",
				transcriptID, i, got.Role, et.role, body)
			check(t, opts, got.Content == et.content,
				"content round-trip for %s turn[%d] content = %q, want %q\nbody: %s",
				transcriptID, i, got.Content, et.content, body)
		}
	}

	// STRUCTURAL floor: every turn has a non-empty role AND non-empty content.
	if want.allRolesAndContent {
		for i, turn := range payload.Turns {
			check(t, opts, turn.Role != "",
				"content round-trip for %s turn[%d] has an empty role (want a non-empty role on every turn)\nbody: %s",
				transcriptID, i, body)
			check(t, opts, turn.Content != "",
				"content round-trip for %s turn[%d] has empty content (want non-empty content on every turn)\nbody: %s",
				transcriptID, i, body)
		}
	}

	// snippet present, and (optionally) carried by a turn of the expected role.
	if want.snippet != "" {
		var snippetTurn *schema.TurnDetail
		for i := range payload.Turns {
			if strings.Contains(payload.Turns[i].Content, want.snippet) {
				snippetTurn = &payload.Turns[i]
				break
			}
		}
		check(t, opts, snippetTurn != nil,
			"content round-trip for %s: snippet %q not found in any of the %d rendered turns (content did not survive migrate-on-read)\nbody: %s",
			transcriptID, want.snippet, len(payload.Turns), body)
		if snippetTurn != nil && want.snippetRole != "" {
			check(t, opts, snippetTurn.Role == want.snippetRole,
				"content round-trip for %s: snippet %q appeared on a %q turn, want role %q\nbody: %s",
				transcriptID, want.snippet, snippetTurn.Role, want.snippetRole, body)
		}
	}

	// rich turn: ToolCalls + HasThinking preserved on the authored bare-payload turn.
	if want.richTurn != nil {
		assertRichTurn(t, opts, transcriptID, payload, *want.richTurn, body)
	}
}

// assertRichTurn asserts the bare-payload case's rich features survived
// migrate-on-read: the turn at want.index has the expected role, HasThinking flag,
// and ToolCalls (identity + ACP kind). Whole-blob json.Unmarshal of a bare payload
// preserves these; the raw-JSONL decoder would strip them.
func assertRichTurn(t *testing.T, opts harnessOptions, transcriptID string, payload schema.SessionDetailPayload, want expectedRichTurn, body []byte) {
	t.Helper()

	inRange := want.index >= 0 && want.index < len(payload.Turns)
	check(t, opts, inRange,
		"content round-trip for %s: rich turn index %d out of range (%d turns rendered)\nbody: %s",
		transcriptID, want.index, len(payload.Turns), body)
	if !inRange {
		return
	}
	turn := payload.Turns[want.index]

	check(t, opts, turn.Role == want.role,
		"content round-trip for %s rich turn[%d] role = %q, want %q\nbody: %s",
		transcriptID, want.index, turn.Role, want.role, body)
	check(t, opts, turn.HasThinking == want.hasThinking,
		"content round-trip for %s rich turn[%d] hasThinking = %v, want %v (bare-payload decoder dropped the thinking flag?)\nbody: %s",
		transcriptID, want.index, turn.HasThinking, want.hasThinking, body)

	tcLen := len(turn.ToolCalls) == len(want.toolCalls)
	check(t, opts, tcLen,
		"content round-trip for %s rich turn[%d] has %d tool calls, want %d (bare-payload decoder dropped ToolCalls?)\nbody: %s",
		transcriptID, want.index, len(turn.ToolCalls), len(want.toolCalls), body)
	if !tcLen {
		return
	}
	for i, wc := range want.toolCalls {
		gc := turn.ToolCalls[i]
		check(t, opts, gc.ID == wc.id,
			"content round-trip for %s rich turn[%d] toolCall[%d] id = %q, want %q\nbody: %s",
			transcriptID, want.index, i, gc.ID, wc.id, body)
		check(t, opts, gc.Name == wc.name,
			"content round-trip for %s rich turn[%d] toolCall[%d] name = %q, want %q\nbody: %s",
			transcriptID, want.index, i, gc.Name, wc.name, body)
		check(t, opts, gc.ToolKind == wc.toolKind,
			"content round-trip for %s rich turn[%d] toolCall[%d] toolKind = %q, want %q\nbody: %s",
			transcriptID, want.index, i, gc.ToolKind, wc.toolKind, body)
	}
}

// seedLegacyTranscript sends PRE-ENVELOPE bytes (the fixture named by spec)
// through the real legacy-compatible publish endpoint so the object and row carry
// a valid encryption descriptor, then installs only the pre-envelope version
// marker. It returns the new transcript's Village UUID. spec selects the committed
// raw-JSONL or bare-payload fixture. Requires harness-owned Postgres.
func seedLegacyTranscript(t *testing.T, opts harnessOptions, stack harnessStack, ownerID, apiKey string, spec legacySeedSpec) string {
	t.Helper()

	data, err := os.ReadFile(spec.fixturePath)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "seed legacy transcript failed",
			what:  fmt.Sprintf("could not read committed legacy fixture %s", spec.fixturePath),
			why:   err.Error(),
			where: "internal/e2e/content_render_e2e_test.go seedLegacyTranscript",
			when:  "reading the legacy fixture bytes before encrypted publication",
			means: "the content round-trip cannot seed the legacy migrate-on-read case",
			fix:   "ensure the committed fixture at " + spec.fixturePath + " is present",
		})
	}

	// Current Village requires every transcript object and database row to carry
	// one authenticated encryption descriptor. Publish the pre-envelope bytes
	// through the real endpoint so production storage creates that state, then
	// restore only the historical content-version marker below.
	localID := uuid.NewString()
	metadata, err := json.Marshal(schema.PublishRequest{
		Identity: schema.SessionIdentity{SessionID: schema.SessionID(localID), SchemaVersion: 2},
		Model:    schema.ModelInfo{Harness: schema.HarnessClaudeCode, Model: schema.ModelID("legacy-content-fixture")},
		Timestamp: schema.TimestampInfo{
			Start: 1700000000000,
			End:   1700000001000,
		},
		Source: schema.SourceInfo{FilePath: "/fixtures/legacy-transcript", Format: spec.format},
		Project: schema.ProjectContext{
			Hash: schema.ProjectHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
			Name: "legacy-content-fixture",
		},
	})
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "seed legacy transcript metadata failed",
			what:  "the typed legacy publish metadata could not be serialized",
			why:   err.Error(),
			where: "internal/e2e/content_render_e2e_test.go seedLegacyTranscript",
			when:  "before sending pre-envelope fixture bytes through the encrypted Village writer",
			means: "the content round-trip cannot seed the legacy migrate-on-read case",
			fix:   "restore schema-valid typed fixture metadata and retry",
		})
	}
	status, body := directPublish(t, stack.villageURL, apiKey, metadata, data)
	if status != http.StatusCreated {
		fatalActionable(t, actionableFailure{
			title: "seed legacy transcript failed",
			what:  fmt.Sprintf("the encrypted Village writer returned HTTP %d for local session %s", status, localID),
			why:   body,
			where: "internal/e2e/content_render_e2e_test.go seedLegacyTranscript",
			when:  "publishing pre-envelope fixture bytes through the production encrypted storage path",
			means: "the legacy rendering fixture has no authenticated object or database descriptor",
			fix:   "inspect the Village publish response and restore its PostgreSQL, KEK, or S3 authority",
		})
	}

	transcriptID := psqlScan(t, stack.db,
		`SELECT id::text FROM transcripts WHERE owner_id=$1 AND local_id=$2;`, ownerID, localID)
	check(t, opts, transcriptID != "",
		"seed legacy transcript: encrypted publish returned no persisted transcript id (local_id=%s)", localID)
	before := readLegacyStorageSnapshot(t, stack, transcriptID)
	psqlExec(t, stack.db, `UPDATE transcripts SET schema_version=$1 WHERE id=$2;`,
		string(LegacyPreEnvelopeVersionMarker), transcriptID)
	after := readLegacyStorageSnapshot(t, stack, transcriptID)
	assertLegacyEncryptedStorage(t, opts, stack, spec, data, transcriptID, before, after)

	// Owner-attribution gate (standing): read back the persisted governance actor
	// the audited authenticated publish recorded and prove it names the OWNER fixture user —
	// the column is changed_by (the GUC app.actor_id is persisted INTO it), never a
	// literal actor_id column. Emitting the value gives positive evidence the read
	// actually executed: this phase early-returns silently on an external stack, so
	// the log line's PRESENCE is what proves the owner-attribution check ran.
	gotActor := psqlScan(t, stack.db,
		`SELECT changed_by::text FROM transcript_governance_events_audit WHERE transcript_id = $1 ORDER BY seq DESC LIMIT 1;`,
		transcriptID)
	t.Logf("legacy-seed audit changed_by=%s owner=%s", gotActor, ownerID)
	check(t, opts, gotActor == ownerID,
		"seed legacy transcript: audit changed_by = %q, want owner %q (authenticated publish must attribute audited creation to the owner fixture user, not the system actor)\ntranscript_id=%s, local_id=%s",
		gotActor, ownerID, transcriptID, localID)
	return transcriptID
}

func readLegacyStorageSnapshot(t *testing.T, stack harnessStack, transcriptID string) legacyStorageSnapshot {
	t.Helper()
	var snapshot legacyStorageSnapshot
	err := stack.db.QueryRow(`
		SELECT
			(to_jsonb(t) - 'schema_version')::text,
			t.schema_version,
			t.blob_key,
			t.wrapped_data_key,
			t.encryption_algorithm,
			t.key_version,
			t.content_hash,
			t.blob_size_bytes
		FROM transcripts AS t
		WHERE t.id = $1
	`, transcriptID).Scan(
		&snapshot.nonVersionRow,
		&snapshot.schemaVersion,
		&snapshot.blobKey,
		&snapshot.wrappedDataKey,
		&snapshot.encryptionAlgorithm,
		&snapshot.keyVersion,
		&snapshot.contentHash,
		&snapshot.blobSizeBytes,
	)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "legacy storage snapshot",
			what:  fmt.Sprintf("transcript row %s could not be read: %v", transcriptID, err),
			why:   "the legacy-envelope compatibility gate needs the complete persisted descriptor before and after applying the legacy marker",
			where: "internal/e2e/content_render_e2e_test.go readLegacyStorageSnapshot",
			when:  "proving the legacy marker changes no other persisted transcript field",
			means: "the gate cannot distinguish an encrypted production publish from a hand-seeded or mutated row",
			fix:   "inspect the Village transcript schema and authenticated publish persistence, then rerun make e2e",
		})
	}
	return snapshot
}

func assertLegacyEncryptedStorage(t *testing.T, opts harnessOptions, stack harnessStack, spec legacySeedSpec, plaintext []byte, transcriptID string, before, after legacyStorageSnapshot) {
	t.Helper()
	check(t, opts, before.schemaVersion != string(LegacyPreEnvelopeVersionMarker),
		"authenticated publish stores the current schema version before the E2E legacy marker is applied")
	check(t, opts, after.schemaVersion == string(LegacyPreEnvelopeVersionMarker),
		"the direct SQL mutation applies exactly LegacyPreEnvelopeVersionMarker")
	check(t, opts, before.nonVersionRow == after.nonVersionRow,
		"applying the legacy marker preserves every other persisted transcript column")

	keyPrefix, keySuffix := "transcripts/", ".bin"
	hasOpaqueShape := strings.HasPrefix(after.blobKey, keyPrefix) && strings.HasSuffix(after.blobKey, keySuffix)
	var objectID uuid.UUID
	var objectIDErr error
	if hasOpaqueShape {
		objectID, objectIDErr = uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(after.blobKey, keyPrefix), keySuffix))
	}
	check(t, opts, hasOpaqueShape && objectIDErr == nil && objectID.String() != transcriptID,
		"authenticated publish stores an opaque transcripts/<uuid>.bin object key distinct from the transcript ID")
	check(t, opts, len(after.wrappedDataKey) > 0,
		"authenticated publish persists a non-empty wrapped data-encryption key")
	check(t, opts, after.encryptionAlgorithm == "aes-256-gcm-random-nonce-v1",
		"authenticated publish persists the production AES-256-GCM random-nonce algorithm")
	check(t, opts, after.keyVersion == 1,
		"authenticated publish persists the active deterministic E2E key version")

	expectedHash := schema.ComputeTranscriptContentHash(plaintext).String()
	decodedHash, hashErr := hex.DecodeString(after.contentHash.String)
	check(t, opts, after.contentHash.Valid && after.contentHash.String == expectedHash && hashErr == nil && len(decodedHash) == 32,
		"authenticated publish persists the exact 64-hex SHA3-256 hash of the committed fixture bytes")
	check(t, opts, after.blobSizeBytes.Valid && after.blobSizeBytes.Int64 == int64(len(plaintext)),
		"authenticated publish persists the exact plaintext fixture size")

	if !hasOpaqueShape || objectIDErr != nil {
		return
	}
	client, err := newMinioClient(stack.minioEndpoint)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "legacy ciphertext client",
			what:  fmt.Sprintf("the E2E MinIO client could not be created: %v", err),
			why:   "the gate must read the exact object named by the persisted transcript descriptor",
			where: "internal/e2e/content_render_e2e_test.go assertLegacyEncryptedStorage",
			when:  "after authenticated publication and before exercising legacy-envelope reads",
			means: "the gate cannot prove the legacy fixture was encrypted at rest",
			fix:   "inspect the E2E MinIO endpoint and credentials, then rerun make e2e",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), s3OpTimeout)
	defer cancel()
	objectInfo, err := client.StatObject(ctx, stack.bucket, after.blobKey, minio.StatObjectOptions{})
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "legacy ciphertext descriptor",
			what:  fmt.Sprintf("the persisted object %s could not be statted: %v", after.blobKey, err),
			why:   "the transcript row must identify a real encrypted object in the E2E bucket",
			where: "internal/e2e/content_render_e2e_test.go assertLegacyEncryptedStorage",
			when:  "proving authenticated legacy fixture publication used production encryption",
			means: "the compatibility read could be exercising a dangling or synthetic persistence descriptor",
			fix:   "inspect Village encrypted transcript persistence and rerun make e2e",
		})
	}
	object, err := client.GetObject(ctx, stack.bucket, after.blobKey, minio.GetObjectOptions{})
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "legacy ciphertext read",
			what:  fmt.Sprintf("the persisted object %s could not be opened: %v", after.blobKey, err),
			why:   "the gate must compare the stored bytes with the committed plaintext fixture",
			where: "internal/e2e/content_render_e2e_test.go assertLegacyEncryptedStorage",
			when:  "reading the exact MinIO object named by the transcript row",
			means: "the gate cannot prove ciphertext rather than plaintext was stored",
			fix:   "inspect Village encrypted transcript persistence and rerun make e2e",
		})
	}
	defer object.Close()
	ciphertext, err := io.ReadAll(object)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "legacy ciphertext body",
			what:  fmt.Sprintf("the persisted object %s could not be read: %v", after.blobKey, err),
			why:   "the gate must inspect the complete stored object bytes",
			where: "internal/e2e/content_render_e2e_test.go assertLegacyEncryptedStorage",
			when:  "comparing the authenticated publish result with its plaintext fixture",
			means: "the gate cannot prove the object is encrypted at rest",
			fix:   "inspect the MinIO object and Village encrypted writer, then rerun make e2e",
		})
	}
	check(t, opts, objectInfo.ContentType == "application/octet-stream",
		"the persisted encrypted object uses application/octet-stream")
	check(t, opts, len(ciphertext) > 0 && objectInfo.Size == int64(len(ciphertext)),
		"the persisted descriptor resolves to a complete non-empty MinIO object")
	check(t, opts, !bytes.Equal(ciphertext, plaintext) && !bytes.Contains(ciphertext, plaintext),
		"the persisted object bytes differ from and do not embed the committed plaintext fixture")
	plaintextProbe := []byte(spec.plaintextProbe)
	check(t, opts, len(plaintextProbe) > 0 && bytes.Contains(plaintext, plaintextProbe),
		"the committed fixture contains its controlled plaintext probe before encryption")
	check(t, opts, !bytes.Contains(ciphertext, plaintextProbe),
		"the persisted object omits the fixture's controlled plaintext probe")
}
