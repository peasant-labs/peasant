package ingest

import (
	"context"
	"errors"
	"fmt"
	"github.com/peasant-labs/schema"
)

// ContentBackfillShapeMismatch means no persisted state was changed. A force
// reindex may instead replace the canonical projection with annotation remapping.
var ContentBackfillShapeMismatch = errors.New("content backfill shape mismatch: stored canonical entries or derived rows differ; no data changed; run harvest index --force to replace entries with annotation remapping")

func NewTranscriptOrigin(n int) (TranscriptOrigin, error) {
	switch n {
	case 0:
		return TranscriptOriginFile, nil
	case 1:
		return TranscriptOriginOpenCodeLegacySQLite, nil
	case 2:
		return TranscriptOriginOpenCodeCurrentSQLite, nil
	}
	return 0, fmt.Errorf("content capture: unknown transcript origin %d; regenerate capture using a supported source", n)
}

type ContentCaptureStatus string

const (
	ContentCaptureComplete   ContentCaptureStatus = "complete"
	ContentCaptureIncomplete ContentCaptureStatus = "incomplete"
	ContentCaptureFailed     ContentCaptureStatus = "failed"
)

func NewContentCaptureStatus(s string) (ContentCaptureStatus, error) {
	switch s {
	case "complete":
		return ContentCaptureComplete, nil
	case "incomplete":
		return ContentCaptureIncomplete, nil
	case "failed":
		return ContentCaptureFailed, nil
	}
	return "", fmt.Errorf("content capture: unknown status %q; use complete, incomplete or failed before storing capture", s)
}

type ContentSourceAuthority string

const (
	ContentSourceNewIngest       ContentSourceAuthority = "new_ingest"
	ContentSourcePeasantSnapshot ContentSourceAuthority = "peasant_snapshot"
	ContentSourceProviderSource  ContentSourceAuthority = "provider_source"
	ContentSourceNone            ContentSourceAuthority = "none"
)

func NewContentSourceAuthority(s string) (ContentSourceAuthority, error) {
	switch s {
	case "new_ingest":
		return ContentSourceNewIngest, nil
	case "peasant_snapshot":
		return ContentSourcePeasantSnapshot, nil
	case "provider_source":
		return ContentSourceProviderSource, nil
	case "none":
		return ContentSourceNone, nil
	}
	return "", fmt.Errorf("content capture: unknown source authority %q; select an attributable source before storing capture", s)
}

type SessionEntryWriteMode string

const (
	SessionEntryWriteReplaceAll      SessionEntryWriteMode = "replace_all"
	SessionEntryWriteContentBackfill SessionEntryWriteMode = "content_backfill"
	// SessionEntryWriteFormatConversion rewrites the stored representation of
	// entries that a prior parser run already produced. It is not a parser run:
	// it preserves the producing indexer, its timestamp and the retained input
	// proof. The store does not perform the conversion yet; the mode exists so
	// callers and the store agree on one closed set of write intents.
	SessionEntryWriteFormatConversion SessionEntryWriteMode = "format_conversion"
)

func NewSessionEntryWriteMode(s string) (SessionEntryWriteMode, error) {
	switch s {
	case "", "replace_all":
		return SessionEntryWriteReplaceAll, nil
	case "content_backfill":
		return SessionEntryWriteContentBackfill, nil
	case "format_conversion":
		return SessionEntryWriteFormatConversion, nil
	}
	return "", fmt.Errorf("content write: unknown mode %q; use replace_all, content_backfill or format_conversion", s)
}

type SessionContentCaptureWrite struct {
	PublicationCaptureRevision int64
	Status                     ContentCaptureStatus
	SourceAuthority            ContentSourceAuthority
	TranscriptOrigin           TranscriptOrigin
	CaptureRevision            string
	CapturedAtMs               int64
	FailureCode                string
	FailureMessage             string
}
type SessionContentCapture struct {
	PublicationCaptureRevision int64
	SessionID                  SessionID
	Status                     ContentCaptureStatus
	SourceAuthority            ContentSourceAuthority
	TranscriptOrigin           TranscriptOrigin
	CaptureRevision            string
	EntryCount                 int
	ContentRowCount            int
	FullCaptureSHA256          string
	CapturedAtMs               int64
	FailureCode                string
	FailureMessage             string
}
type SessionEntryReadMode string

const (
	SessionEntryReadPreview     SessionEntryReadMode = "preview"
	SessionEntryReadFullContent SessionEntryReadMode = "full_content"
	// SessionEntryReadAvailable reads the content that is actually stored: the
	// full content when the capture is complete, the bounded preview projection
	// otherwise. It is never gated on capture completeness, publication
	// readiness, recovery or a native source. It never certifies content for
	// export or publication; use SessionEntryReadFullContent for that.
	SessionEntryReadAvailable SessionEntryReadMode = "available"
)

func NewSessionEntryReadMode(s string) (SessionEntryReadMode, error) {
	switch s {
	case "", "preview":
		return SessionEntryReadPreview, nil
	case "full_content":
		return SessionEntryReadFullContent, nil
	case "available":
		return SessionEntryReadAvailable, nil
	}
	return "", fmt.Errorf("content read: unknown mode %q; use preview, full_content or available", s)
}

type SessionEntryReadOptions struct {
	Mode         SessionEntryReadMode
	FromIndex    int
	Limit        int
	SoftMaxBytes int64
}
type SessionEntryReadPage struct {
	Entries   []schema.SessionEntry
	Capture   SessionContentCapture
	FromIndex int
	NextIndex *int
	BytesRead int64
}

// FullSessionEntryReader returns a complete verified capture from one snapshot.
// Zero selects asymmetric initial/continuation budgets; positive overrides all pages.
type FullSessionEntryReader interface {
	LoadFullSessionEntries(context.Context, SessionID, int64) ([]schema.SessionEntry, SessionContentCapture, error)
}

type SessionEntryFullReader interface {
	FullSessionEntryReader
	ListEntries(context.Context, SessionID) ([]schema.SessionEntry, error)
	ListEntriesRange(context.Context, schema.SessionID, int, int) ([]schema.SessionEntry, error)
	ReadSessionEntries(context.Context, SessionID, SessionEntryReadOptions) (SessionEntryReadPage, error)
	GetSessionContentCapture(context.Context, SessionID) (SessionContentCapture, bool, error)
}

// ContentCaptureIncompleteSession names one recovery target. The harness and
// the session start time travel with the identifier so a caller can scope,
// order and report recovery work without a second lookup per session.
type ContentCaptureIncompleteSession struct {
	SessionID SessionID
	Harness   Harness
	StartMs   int64
}

type ContentBackfillTargetStore interface {
	ListContentCaptureIncompleteSessions(context.Context, int) ([]SessionID, error)
	ListContentCaptureIncompleteSessionsAfter(context.Context, SessionID, int) ([]ContentCaptureIncompleteSession, error)
	LookupSessionLocation(context.Context, SessionID) (string, string, error)
	LookupSourceInfo(context.Context, SessionID) (string, SourceFormat, string, error)
	IndexSessionEntryBatch(context.Context, []SessionEntryWrite) []SessionEntryWriteResult
}
