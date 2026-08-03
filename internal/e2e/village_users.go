//go:build e2e

// Harness extension for the pull round-trip.
//
// The committed village-setup-demo mints ONE fixed demo user (github_id 990001,
// "demo-user"). The pull round-trip needs a SECOND, independent village user (and
// a transcript GROUP-SHARED to it) so that user2 can both PULL user1's shared
// transcript and ANNOTATE it (the foreign-annotation refresh case). Either
// extending the setup tool OR a sibling mint path works; this is the sibling mint
// path, kept ENTIRELY peasant-side so the review-clean village checkout is never
// modified:
//
//   - mintVillageUser inserts a users row + an api_keys row DIRECTLY over a
//     database/sql connection (the pgx stdlib driver, e2e-build-tagged so it never
//     enters production builds) with $1 parameterized statements. The API key is
//     generated to EXACTLY match the village's production auth.GenerateAPIKey
//     format — "peasant_" + hex(32 random bytes), sha256-hex hash, 8-char prefix —
//     so the village's AuthRequired middleware (GetAPIKeyByHash over the sha256
//     hash) accepts it identically to an OAuth-minted key.
//   - groupShareTranscript drives the REAL authenticated village API as the
//     transcript owner: it creates a group, adds the member by username, and shares
//     the transcript with the group. For an 'open' group the share is approved
//     immediately and the share call auto-flips visibility private→'shared', so the
//     pull surface's ListApprovedTranscriptShareGroups sees an approved share — no
//     raw DB writes. The handler's inTxAs transaction attributes the transcript's
//     visibility change to the owner's UUID, not the system actor.
//
// SEQUENCING (asserted by the caller): group-share MUST happen BEFORE user2
// annotates, because the village's CreateTranscriptAnnotation gates on
// canViewTranscript — an unshared transcript is 404 to user2.
package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// villageAPIKeyPrefix mirrors the village's auth.APIKeyPrefix. Kept as a local
// constant (the village's auth package is a separate module and not importable
// from peasant) so the minted key matches the production format byte-for-byte.
const villageAPIKeyPrefix = "peasant_"

// secondUserGitHubID / secondUsername identify the SECOND harness user. The id is
// a synthetic surrogate DISTINCT from village-setup-demo's demoGitHubID (990001),
// so the two users never collide on the github_id unique key.
const (
	secondUserGitHubID = 990002
	secondUsername     = "demo-user-2"
	secondKeyLabel     = "demo-e2e-user2"
)

// villageCredentials is the peasant credentials.json shape (mirrors
// internal/auth.Credentials, not importable across the demo boundary). The peasant
// client reads exactly these JSON keys. It is the in-memory record the harness
// holds for each minted user so it can drive that user's peasant subprocesses
// (via writeCredentialsFor) and its direct village API calls (via APIKey).
type villageCredentials struct {
	APIKey     string `json:"api_key"`
	KeyID      string `json:"key_id"`
	UserID     string `json:"user_id"`
	Username   string `json:"username"`
	VillageURL string `json:"village_url"`
}

// generateVillageAPIKey replicates the village's auth.GenerateAPIKey EXACTLY:
// plaintext = "peasant_" + hex(32 random bytes); hash = sha256-hex(plaintext);
// prefix = first 8 chars of plaintext. The village stores the hash and looks up by
// it (GetAPIKeyByHash), so a key minted here authenticates identically to one
// minted by the village's own code path.
//
// DRIFT WARNING: this duplicates the village's auth.GenerateAPIKey because that
// package is a separate, non-importable module from peasant. If the village ever
// changes its key prefix, byte length, or hash algorithm (auth.GenerateAPIKey /
// auth.GetAPIKeyByHash), update THIS in lockstep — otherwise AuthRequired will
// silently 401 every minted key, manifesting as pull/sync/list e2e failures at the
// village boundary (a 401, NOT an obvious format error, so the cause is non-obvious).
// Check village backend/internal/auth's GenerateAPIKey/HashAPIKey first when this drifts.
// The format-assert below trips a CLEAR local test failure if an accidental edit
// here breaks the format, rather than letting it surface as a remote 401.
func generateVillageAPIKey(t *testing.T) (plaintext, hash, prefix string) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("e2e: generate api key random bytes: %v", err)
	}
	plaintext = villageAPIKeyPrefix + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(sum[:])
	prefix = plaintext[:8]
	// Format guard (see DRIFT WARNING): village auth.GenerateAPIKey yields a
	// villageAPIKeyPrefix-prefixed plaintext of len(prefix)+64 (32 hex-encoded
	// bytes) and an 8-char key_prefix. If these drift here, fail LOUDLY and
	// locally instead of producing a key the village's AuthRequired will 401.
	if !strings.HasPrefix(plaintext, villageAPIKeyPrefix) || len(plaintext) != len(villageAPIKeyPrefix)+64 || len(prefix) != 8 {
		t.Fatalf("e2e: generateVillageAPIKey produced a key that drifted from the village format "+
			"(want %q-prefixed, len %d, 8-char prefix; got len %d, prefix %q) — see DRIFT WARNING; "+
			"a drifted key will 401 at the village's AuthRequired boundary, not fail an obvious format check",
			villageAPIKeyPrefix, len(villageAPIKeyPrefix)+64, len(plaintext), prefix)
	}
	return plaintext, hash, prefix
}

// psqlScan runs a single value-returning statement over the harness's ephemeral
// Postgres (the pgx stdlib database/sql handle) and scans the sole column of the
// sole row into a string. Use it for RETURNING inserts and single-value SELECTs.
// Statements are parameterized ($1, $2, …) — the harness never interpolates values
// into the SQL text. A failure is fatal (the harness cannot proceed without its
// provisioning SQL).
func psqlScan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(query, args...).Scan(&s); err != nil {
		fatalActionable(t, actionableFailure{
			title: "Village fixture query failed",
			what:  fmt.Sprintf("a single-value harness query could not return its expected row; query=%q args=%v", query, args),
			why:   err.Error(),
			where: "internal/e2e/village_users.go psqlScan",
			when:  "provisioning or reading owner-attributed Village fixture state",
			means: "the harness cannot construct the authenticated fixture identity needed by later API steps",
			fix:   "confirm the harness Postgres is reachable, migrations completed, and the parameterized statement matches the pinned Village schema",
		})
	}
	return s
}

// psqlExec runs a statement that returns no rows (the api_keys DELETE, the truncate
// DO-block) over the harness's ephemeral Postgres. Statements are parameterized. A
// failure is fatal.
func psqlExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		fatalActionable(t, actionableFailure{
			title: "Village fixture mutation failed",
			what:  fmt.Sprintf("a parameterized harness statement could not complete; query=%q args=%v", query, args),
			why:   err.Error(),
			where: "internal/e2e/village_users.go psqlExec",
			when:  "resetting or provisioning Village fixture state",
			means: "the database state is not the deterministic baseline required by the E2E",
			fix:   "confirm the harness Postgres is reachable, migrations completed, and the statement matches the pinned Village schema",
		})
	}
}

// mintVillageUser provisions an independent village user + API key DIRECTLY in the
// ephemeral Postgres (the sibling-mint path; see the package doc). It upserts the
// users row on github_id (idempotent across re-runs), mints a production-format
// API key, clears any prior keys for the user, inserts the fresh api_keys row, and
// returns the credentials the harness drives that user's commands with. It does
// NOT write credentials.json — the caller chooses the per-user sandbox via
// writeCredentialsFor.
func mintVillageUser(t *testing.T, db *sql.DB, villageURL string, githubID int, username, keyLabel string) villageCredentials {
	t.Helper()

	// provider defaults to 'github' (migration 015), so it is omitted; only the
	// NOT NULL provider_user_id is set explicitly — mirrors village-setup-demo.
	userID := psqlScan(t, db, `
		INSERT INTO users (github_id, github_username, provider_user_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (github_id) DO UPDATE SET github_username = EXCLUDED.github_username
		RETURNING id::text;`,
		githubID, username, fmt.Sprint(githubID))
	if userID == "" {
		t.Fatalf("e2e: mintVillageUser(%q): upsert returned empty user id", username)
	}

	plaintext, hash, prefix := generateVillageAPIKey(t)

	// Clear any prior keys for this user so a re-run leaves exactly one valid key
	// matching the emitted creds (mirrors village-setup-demo's idempotency).
	psqlExec(t, db, `DELETE FROM api_keys WHERE user_id = $1;`, userID)

	keyID := psqlScan(t, db, `
		INSERT INTO api_keys (user_id, key_hash, key_prefix, label)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text;`,
		userID, hash, prefix, keyLabel)
	if keyID == "" {
		t.Fatalf("e2e: mintVillageUser(%q): insert api key returned empty key id", username)
	}

	return villageCredentials{
		APIKey:     plaintext,
		KeyID:      keyID,
		UserID:     userID,
		Username:   username,
		VillageURL: villageURL,
	}
}

// writeCredentialsFor writes a peasant credentials.json for the given user into
// the given SANDBOX config dir (the path peasant reads under XDG_CONFIG_HOME), so
// that user's peasant subprocesses authenticate as that user. Each user gets its
// OWN sandbox so user1 and user2 never share local state.
func writeCredentialsFor(t *testing.T, configHome string, creds villageCredentials) {
	t.Helper()
	credDir := filepath.Join(configHome, string(defaults.AppName))
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatalf("e2e: mkdir creds dir %s: %v", credDir, err)
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		t.Fatalf("e2e: marshal credentials for %s: %v", creds.Username, err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "credentials.json"), data, 0o600); err != nil {
		t.Fatalf("e2e: write credentials.json for %s: %v", creds.Username, err)
	}
}

// villageTranscript is a minimal view of a row in the village transcripts table
// the harness needs: the village-side UUID (the pull ref), its owner, the
// publisher's local session id (round-trip correlation), and current visibility.
type villageTranscript struct {
	ID                   string
	OwnerID              string
	LocalID              string
	Visibility           string
	ContentHash          string
	OperationFingerprint string
	License              sql.NullString
	PublishedAtMillis    int64
	UpdatedAtMillis      int64
}

// listVillageTranscripts reads the village transcripts table for the given owner
// (user id), ordered by published_at. The harness uses it to discover the village
// UUID that user1's push produced (which it then pulls / shares / annotates).
func listVillageTranscripts(t *testing.T, db *sql.DB, ownerID string) []villageTranscript {
	t.Helper()
	rows, err := db.Query(`
		SELECT id::text, owner_id::text, local_id, visibility, content_hash,
		       accepted_request_operation_fingerprint, license_id,
		       FLOOR(EXTRACT(EPOCH FROM published_at) * 1000)::bigint,
		       FLOOR(EXTRACT(EPOCH FROM updated_at) * 1000)::bigint
		FROM transcripts WHERE owner_id = $1 ORDER BY published_at;`, ownerID)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "list Village transcript fixtures failed",
			what:  fmt.Sprintf("the transcript rows for owner %s could not be queried", ownerID),
			why:   err.Error(),
			where: "internal/e2e/village_users.go listVillageTranscripts",
			when:  "discovering API-published transcript UUIDs for pull and sharing assertions",
			means: "the harness cannot correlate local sessions with the owner-visible Village records",
			fix:   "confirm the harness database is reachable and the transcripts table matches the pinned Village schema",
		})
	}
	defer rows.Close()
	var out []villageTranscript
	for rows.Next() {
		var vt villageTranscript
		if err := rows.Scan(&vt.ID, &vt.OwnerID, &vt.LocalID, &vt.Visibility, &vt.ContentHash,
			&vt.OperationFingerprint, &vt.License, &vt.PublishedAtMillis, &vt.UpdatedAtMillis); err != nil {
			fatalActionable(t, actionableFailure{
				title: "scan Village transcript fixture failed",
				what:  fmt.Sprintf("a transcript row for owner %s did not match the expected id/owner/local_id/visibility shape", ownerID),
				why:   err.Error(),
				where: "internal/e2e/village_users.go listVillageTranscripts",
				when:  "reading API-published transcript rows for pull and sharing assertions",
				means: "the harness cannot safely choose the transcript under test",
				fix:   "update the harness row shape only after confirming the pinned Village transcripts schema changed intentionally",
			})
		}
		out = append(out, vt)
	}
	if err := rows.Err(); err != nil {
		fatalActionable(t, actionableFailure{
			title: "iterate Village transcript fixtures failed",
			what:  fmt.Sprintf("the transcript result set for owner %s ended with an iteration error", ownerID),
			why:   err.Error(),
			where: "internal/e2e/village_users.go listVillageTranscripts",
			when:  "after querying API-published transcripts for pull and sharing assertions",
			means: "the returned fixture set may be incomplete and cannot be trusted",
			fix:   "check the Postgres connection and retry against a healthy harness database",
		})
	}
	return out
}

func villageTranscriptByLocalID(t *testing.T, rows []villageTranscript, localID string) villageTranscript {
	t.Helper()
	for _, row := range rows {
		if row.LocalID == localID {
			return row
		}
	}
	t.Fatalf("e2e: no village transcript with local_id %q among %d rows", localID, len(rows))
	return villageTranscript{}
}

// assertLatestVisibilityAudit proves an API-driven visibility mutation crossed
// the authenticated handler transaction and reached the fail-closed governance
// trigger with the expected owner UUID. Checking the functional pull outcome is
// not sufficient: it would still pass if attribution regressed to the system
// actor while the visibility value changed correctly.
func assertLatestVisibilityAudit(t *testing.T, db *sql.DB, transcriptID, wantVisibility, wantChangedBy string) {
	t.Helper()
	if db == nil {
		fatalActionable(t, actionableFailure{
			title: "governance audit assertion database handle missing",
			what:  "the visibility-audit assertion received a nil database/sql handle",
			why:   "the assertion reads the trigger-written event from the harness-owned Postgres database",
			where: "internal/e2e/village_users.go assertLatestVisibilityAudit",
			when:  "verifying an authenticated visibility mutation",
			means: "owner attribution cannot be proven and a nil dereference would hide the setup error",
			fix:   "pass the database handle returned by startEphemeralPostgres",
		})
	}

	var gotEvent, gotVisibility, gotChangedBy string
	err := db.QueryRow(`
		SELECT event_type, visibility, changed_by::text
		FROM transcript_governance_events_audit
		WHERE transcript_id = $1
		ORDER BY seq DESC
		LIMIT 1;`, transcriptID).Scan(&gotEvent, &gotVisibility, &gotChangedBy)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "governance audit assertion query failed",
			what:  fmt.Sprintf("the latest audit event for transcript %s could not be read", transcriptID),
			why:   err.Error(),
			where: "internal/e2e/village_users.go assertLatestVisibilityAudit",
			when:  "after an authenticated group-share or visibility PATCH",
			means: "the E2E cannot prove that the API mutation was audited for the owning user",
			fix:   "confirm migration 026 ran and the authenticated handler wrote a governance event",
		})
	}

	t.Logf("visibility audit transcript=%s event=%s visibility=%s changed_by=%s",
		transcriptID, gotEvent, gotVisibility, gotChangedBy)
	if gotEvent != "visibility_changed" || gotVisibility != wantVisibility || gotChangedBy != wantChangedBy {
		fatalActionable(t, actionableFailure{
			title: "governance visibility attribution mismatch",
			what:  fmt.Sprintf("latest event = (%s, %s, %s), want (visibility_changed, %s, %s)", gotEvent, gotVisibility, gotChangedBy, wantVisibility, wantChangedBy),
			why:   "the API mutation did not persist the expected visibility snapshot under the transcript owner's UUID",
			where: "internal/e2e/village_users.go assertLatestVisibilityAudit",
			when:  "verifying an authenticated group-share or visibility PATCH",
			means: "the functional result may be correct, but the durable governance attribution is not",
			fix:   "route the handler mutation through inTxAs with the authenticated owner and preserve the trigger-written audit snapshot",
		})
	}
}

// villageAPIRequest issues an authenticated JSON request to the village API as the
// user holding apiKey (Bearer + Content-Type: application/json, 10s client),
// marshaling body to JSON (pass nil for no body), and returns the HTTP status + raw
// response bytes. It is the shared authed-JSON template the harness's API-driven
// seed steps use to create user-owned data through the REAL server the way a
// logged-in client would. AuthRequired places the user in request context; each
// governed handler then uses inTxAs to set app.actor_id to that user's UUID.
func villageAPIRequest(t *testing.T, method, villageURL, path, apiKey string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			fatalActionable(t, actionableFailure{
				title: "Village API request serialization failed",
				what:  fmt.Sprintf("the %s %s request body could not be encoded as JSON", method, path),
				why:   err.Error(),
				where: "internal/e2e/village_users.go villageAPIRequest",
				when:  "building an authenticated owner-driven API mutation",
				means: "the real Village handler cannot be exercised with the intended fixture",
				fix:   "make the request body JSON-serializable and preserve the pinned Village API field names",
			})
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, villageURL+path, reader)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "Village API request construction failed",
			what:  fmt.Sprintf("the authenticated %s %s request could not be created", method, path),
			why:   err.Error(),
			where: "internal/e2e/village_users.go villageAPIRequest",
			when:  "after serializing the API fixture and before contacting Village",
			means: "the owner-driven API mutation and governance attribution cannot be tested",
			fix:   "confirm VILLAGE_URL and the request path form a valid http(s) URL",
		})
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "Village API request failed",
			what:  fmt.Sprintf("the authenticated %s %s request did not receive a response", method, path),
			why:   err.Error(),
			where: "internal/e2e/village_users.go villageAPIRequest",
			when:  "driving an owner mutation through the real Village server",
			means: "the functional result and trigger-written governance attribution cannot be evaluated",
			fix:   "confirm the harness-owned Village process is healthy, reachable, and using the minted API key",
		})
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "village API response read failed",
			what:  fmt.Sprintf("the %s %s response body could not be read", method, path),
			why:   err.Error(),
			where: "internal/e2e/village_users.go villageAPIRequest",
			when:  "reading the authenticated village API response",
			means: "the caller cannot reliably evaluate the response status and payload",
			fix:   "check the village connection and retry after confirming the server returns a complete response body",
		})
	}
	return resp.StatusCode, out
}

// groupShareTranscript group-shares a transcript so a NON-owner member can pull +
// annotate it, driving the REAL authenticated village API as the transcript OWNER
// (ownerAPIKey) so the transcript mutation attributes to the owner's UUID. It
// (a) POSTs /groups {name} (the creator is auto-added as the owner member),
// (b) POSTs /groups/{id}/members {username} to add the member by username, and
// (c) POSTs /transcripts/{id}/share {group_ids:[id]}. For an 'open' group the share is
// approved immediately AND the share call auto-flips visibility private→'shared'
// (recorded by the governance trigger) — exactly what the pull surface's
// ListApprovedTranscriptShareGroups requires, with no raw DB writes. Returns the
// new group id.
//
// MUST be called BEFORE the member annotates the transcript (the village's
// CreateTranscriptAnnotation gates on canViewTranscript; an unshared transcript is
// 404 to a non-owner).
func groupShareTranscript(t *testing.T, villageURL, ownerAPIKey, memberUsername, transcriptID, groupName string) string {
	t.Helper()

	// (a) Create the group; the creator becomes its owner member automatically.
	status, resp := villageAPIRequest(t, http.MethodPost, villageURL, "/api/v1/groups", ownerAPIKey,
		map[string]any{"name": groupName})
	if status != http.StatusCreated {
		t.Fatalf("e2e: groupShareTranscript: POST /api/v1/groups status = %d, want 201\nbody: %s", status, resp)
	}
	var group struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &group); err != nil || group.ID == "" {
		t.Fatalf("e2e: groupShareTranscript: parse group id from POST /api/v1/groups response: %v\nbody: %s", err, resp)
	}

	// (b) Add the member by USERNAME (the endpoint takes a username, not a user id);
	// the caller (owner) is authorized because CreateGroup made it the owner member.
	status, resp = villageAPIRequest(t, http.MethodPost, villageURL,
		fmt.Sprintf("/api/v1/groups/%s/members", group.ID), ownerAPIKey,
		map[string]any{"username": memberUsername})
	if status != http.StatusOK {
		t.Fatalf("e2e: groupShareTranscript: POST /api/v1/groups/%s/members status = %d, want 200\nbody: %s",
			group.ID, status, resp)
	}

	// (c) Share the transcript with the group. For an 'open' group this approves the
	// share immediately and auto-flips visibility private→'shared' — no raw UPDATE or
	// transcript_shares INSERT needed.
	status, resp = villageAPIRequest(t, http.MethodPost, villageURL,
		fmt.Sprintf("/api/v1/transcripts/%s/share", transcriptID), ownerAPIKey,
		map[string]any{"group_ids": []string{group.ID}})
	if status != http.StatusOK {
		t.Fatalf("e2e: groupShareTranscript: POST /api/v1/transcripts/%s/share status = %d, want 200\nbody: %s",
			transcriptID, status, resp)
	}

	return group.ID
}

// createVillageAnnotation POSTs a manual annotation to the village as the given
// user (Bearer key), targeting the transcript by its village UUID. It mirrors the
// village's createManualAnnotationRequest body. The village gates this on
// canViewTranscript, so the transcript MUST already be shared with (or owned by)
// the authenticated user. Returns the HTTP status + body so the caller can assert
// both the happy path (201) and, before sharing, the 404 sequencing gate.
func createVillageAnnotation(t *testing.T, villageURL, apiKey, transcriptID, typeID, value string, entryIndex int) (int, string) {
	t.Helper()
	status, resp := villageAPIRequest(t, http.MethodPost, villageURL,
		fmt.Sprintf("/api/v1/transcripts/%s/annotations", transcriptID), apiKey,
		map[string]any{"typeId": typeID, "value": value, "entryIndex": entryIndex})
	return status, string(resp)
}
