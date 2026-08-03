// Package pull implements the peasant-side transcript pull pipeline: given a
// village transcript reference (a bare UUID or a pasted village web URL), it
// negotiates the village pull contract, fetches the transcript blob + its
// authored annotations, and lands them atomically into a SEPARATE village-pulls/
// namespace plus the V34 pulled_* tables — without ever touching the ingest
// (peasant-sync) tree, the sessions analytics tables, or the annotate-push
// candidate set because pulled data is foreign and one-way.
//
// The pipeline obeys an INVERTED fail-open doctrine relative to push: any failure
// before the WRITE stage leaves ZERO local mutation. All network I/O completes
// into a temp dir / memory BEFORE the single atomic dir-rename and the single
// SQLite transaction (CommitPull). See Pipeline.PullTranscript for the staged
// order and the rename↔tx compensation.
package pull

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/peasant-labs/schema"
)

// --- PullStatus (typed enum + Stringer) ---

// PullStatus is the typed outcome of a pull operation, mapped from the pipeline's
// internal flow and the village client's error sentinels. It is the public,
// machine-comparable result the CLI keys its exit code and rendering
// off — never a bare string. The String() method is the only serialization
// boundary (CLI/JSON output).
type PullStatus string

const (
	// PullStatusPulled: the transcript (and its annotations) were fetched and
	// committed locally — a fresh pull or a --force/changed re-pull.
	PullStatusPulled PullStatus = "pulled"
	// PullStatusUpToDate: the local copy already matched the served blob (hash
	// match, or a 304 Not Modified conditional GET) — no re-download, no rewrite.
	// This is a SUCCESS outcome, never reported for an error.
	PullStatusUpToDate PullStatus = "up-to-date"
	// PullStatusNotFound: the transcript is not pullable for this account (does
	// not exist, or is neither owned-by nor group-shared-with the requester;
	// public is excluded). Mapped from village.ErrPullNotFound (404, no leak).
	PullStatusNotFound PullStatus = "not-found"
	// PullStatusNotLoggedIn: no valid credentials — a village-contacting command
	// was attempted logged-out. The CLI prints an actionable `peasant village
	// login` message.
	PullStatusNotLoggedIn PullStatus = "not-logged-in"
	// PullStatusContractError: the village's advertised pull window is absent or
	// excludes this CLI's pull-contract version. Mapped from
	// village.ErrPullContractIncompatible.
	PullStatusContractError PullStatus = "contract-error"
	// PullStatusError: any other failure (transport, decode, filesystem, DB).
	PullStatusError PullStatus = "error"
)

// String returns the wire/CLI representation of the status.
func (s PullStatus) String() string { return string(s) }

// transcriptsPathSegment is the canonical first (and only non-id) path segment of
// a village transcript web URL: https://<host>/transcripts/<uuid>.
const transcriptsPathSegment = "transcripts"

// splitNonEmpty splits a URL path on "/" and drops empty segments (leading/
// trailing/duplicate slashes), so "/transcripts/<uuid>/" yields exactly
// ["transcripts", "<uuid>"].
func splitNonEmpty(path string) []string {
	raw := strings.Split(path, "/")
	out := raw[:0]
	for _, s := range raw {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// --- TranscriptRef + ParseTranscriptRef ---

// TranscriptRef is a resolved, validated reference to a village transcript. ID is
// always a canonical schema.TranscriptID (lowercase-hex UUID); FromURL records
// whether the user pasted a full village web URL (vs a bare UUID) so the CLI can
// echo the resolution it performed.
type TranscriptRef struct {
	ID      schema.TranscriptID
	FromURL bool
}

// ParseTranscriptRef parses a user-supplied transcript reference — either a bare
// UUID or a pasted village web URL of the canonical /transcripts/<uuid> shape
// (e.g. https://village.example.com/transcripts/<uuid>; a trailing slash and a
// query string are allowed). It does NOT accept any URL whose last path segment
// merely happens to be a UUID — see the body comment + docs/pull.md §6.1. The
// extracted UUID is CASE-NORMALIZED to lowercase before construction, because NewTranscriptID
// accepts canonical lowercase-hex only: a pasted uppercase UUID must not be
// rejected. The error is actionable (what/why/how-to-fix) per C-actionable-errors.
func ParseTranscriptRef(raw string) (TranscriptRef, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return TranscriptRef{}, fmt.Errorf(
			"invalid transcript reference: empty\n" +
				"  what: no transcript UUID or URL was given\n" +
				"  why:  `peasant village transcripts pull` needs a reference to pull\n" +
				"  fix:  pass a transcript UUID (e.g. 99d59925-36bc-424c-a789-8be54d9702ba) or a pasted village web URL")
	}

	// URL form: anything that parses with a scheme + host. The path must be the
	// CANONICAL village transcript shape — exactly /transcripts/<uuid> (a single
	// "transcripts" segment followed by exactly one UUID segment). An optional
	// trailing slash is allowed (.../transcripts/<uuid>/) and query strings are
	// ignored (url.Parse keeps them out of u.Path). We deliberately do NOT accept
	// any URL whose last path segment merely happens to be a UUID: that loose form
	// admitted lookalikes such as /users/<uuid> and /transcripts/<uuid>/annotations/
	// <uuid> (where the WRONG, annotation, UUID would be extracted).
	fromURL := false
	candidate := trimmed
	if u, err := url.Parse(trimmed); err == nil && u.Scheme != "" && u.Host != "" {
		fromURL = true
		segs := splitNonEmpty(u.Path)
		if len(segs) == 2 && segs[0] == transcriptsPathSegment {
			candidate = segs[1]
		} else {
			candidate = ""
		}
		if candidate == "" {
			return TranscriptRef{}, fmt.Errorf(
				"invalid transcript reference %q: URL is not a village transcript URL\n"+
					"  what: the URL path is not the canonical /transcripts/<uuid> shape (got %q)\n"+
					"  why:  a transcript pull URL must be exactly https://<village-host>/transcripts/<uuid> "+
					"(a trailing slash is allowed); URLs like /users/<uuid> or "+
					"/transcripts/<uuid>/annotations/<uuid> are NOT transcript references\n"+
					"  fix:  copy the transcript's own web URL (…/transcripts/<uuid>), or pass the bare UUID",
				trimmed, u.Path)
		}
	}

	// Case-normalize the extracted candidate: NewTranscriptID requires canonical
	// lowercase-hex, but a user may paste an uppercase UUID.
	candidate = strings.ToLower(candidate)

	id, err := schema.NewTranscriptID(candidate)
	if err != nil {
		if fromURL {
			return TranscriptRef{}, fmt.Errorf(
				"invalid transcript reference %q: last URL path segment %q is not a transcript UUID\n"+
					"  what: %v\n"+
					"  why:  the pull URL must end in a canonical transcript UUID\n"+
					"  fix:  copy the transcript's web URL again, or pass the bare UUID",
				trimmed, candidate, err)
		}
		return TranscriptRef{}, fmt.Errorf(
			"invalid transcript reference %q: not a transcript UUID\n"+
				"  what: %v\n"+
				"  why:  a bare reference must be a canonical UUID; a URL must end in one\n"+
				"  fix:  pass a UUID like 99d59925-36bc-424c-a789-8be54d9702ba, or paste the transcript's web URL",
			trimmed, err)
	}

	return TranscriptRef{ID: id, FromURL: fromURL}, nil
}

// --- Pull / Refresh options + results ---

// PullOptions controls a single PullTranscript run.
type PullOptions struct {
	// Force bypasses the DIFF stage (always re-downloads + rewrites even when the
	// local copy matches the served-blob hash). Used to repair a files↔DB
	// divergence (the V34 tables are a derived index, repaired by re-pull).
	Force bool
	// DryRun runs RESOLVE → AUTH-CHECK → NEGOTIATE → FETCH-META → DIFF and then
	// SHORT-CIRCUITS before any DOWNLOAD/WRITE/DB-TX: zero downloads, zero local
	// mutation, zero DB writes. The returned PullResult.DryRun is true and Status
	// reports the WOULD-BE outcome (PullStatusUpToDate when the local copy already
	// matches the server hash, else PullStatusPulled for "would pull"). Mirrors the
	// push pipeline's dry-run reporting spirit (no side effects, reports intent).
	DryRun bool
}

// PullResult is the observable outcome of a PullTranscript run.
type PullResult struct {
	// Ref is the resolved reference that was pulled.
	Ref TranscriptRef
	// Status is the typed outcome (pulled / up-to-date / not-found / …). When
	// DryRun is true this is the WOULD-BE outcome (would-pull vs already-up-to-date)
	// — no mutation occurred.
	Status PullStatus
	// VillageHost is the host the transcript was pulled from. It is ALWAYS the
	// LOGGED-IN village's host (derived from creds.VillageURL), never the host of a
	// pasted village web URL — the pasted URL contributes only the transcript UUID
	// (ParseTranscriptRef discards its host). This keeps the on-disk namespace and
	// the pulled_transcripts composite key consistent with where the fetch actually
	// happens (the configured village client baseURL). A pasted foreign-village URL
	// therefore resolves against the logged-in village (typically a 404) — intended
	// for the single-village MVP; revisit for the multi-village future.
	VillageHost string
	// PullDir is the on-disk directory the transcript landed in
	// (village-pulls/{villageHost}/{transcriptId}). Empty when nothing was written.
	// Under DryRun it is the directory that WOULD be written.
	PullDir string
	// ServedBlobHash is the RAW (unquoted) content-identity hash of the served blob
	// (the pull-idempotency key). When the village serves a quoted ETag the quotes
	// are stripped; when it serves none this is the locally-computed hash. May be
	// empty only when no blob was fetched (e.g. a DryRun that stops at an
	// already-up-to-date DIFF, where it carries the stored raw hash).
	ServedBlobHash string
	// DryRun is true when this result came from a DryRun=true run (no mutation; the
	// Status/PullDir/ServedBlobHash describe what WOULD have happened).
	DryRun bool
	// AnnotationCount is the number of annotations committed alongside the
	// transcript.
	AnnotationCount int
	// License is the license the village served with the transcript metadata
	// ("" = none granted — default copyright). Populated on every result built
	// after FETCH-META (pulled, up-to-date, dry-run); persisted to the V38
	// pulled_transcripts.license_id column on a real pull.
	License schema.License
}

// RefreshOptions controls a RefreshOwnAnnotations run.
type RefreshOptions struct {
	// SessionID, when non-empty, restricts the refresh to the single OWN pushed
	// transcript correlated to this local session ID (the --session flag). Empty
	// ⇒ refresh foreign annotations across all own pushed transcripts.
	SessionID string
}

// RefreshResult is the observable outcome of a RefreshOwnAnnotations run.
type RefreshResult struct {
	// Status is the typed outcome (pulled when any annotations were written,
	// up-to-date when nothing changed, or an error/contract/auth status).
	Status PullStatus
	// VillageHost is the host the annotations were refreshed from.
	VillageHost string
	// TranscriptsScanned is the number of own pushed transcripts enumerated.
	TranscriptsScanned int
	// Created/Updated/Skipped mirror the UpsertPulledAnnotations created/updated/
	// skipped vocabulary (skipped = payload-identical, no write).
	Created int
	Updated int
	Skipped int
	// Excluded is the number of own-authored annotations filtered out
	// (AuthorUserID == creds.UserID) before the upsert.
	Excluded int
}
