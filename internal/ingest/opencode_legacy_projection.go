package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"time"
)

const (
	openCodeLegacyProjectionFormat = "peasant.opencode.legacy-sqlite"
	// openCodeLegacyProjectionVersion is version 2: the orphan slot is a typed
	// message field. The minimum readable version is the version-field floor a
	// decoded artifact must meet.
	openCodeLegacyProjectionVersion            = 2
	openCodeLegacyProjectionMinReadableVersion = 1
	openCodeLegacyMaterializePage              = 128
	// openCodeLegacyMessageBudgetShare is the fraction of a bounded read's byte
	// budget the message pass may spend before it stops and leaves the rest to
	// the part pass.
	//
	// A legacy session keeps its RENDERABLE content in part rows; a message row
	// carries the role, the timing, and whatever metadata the harness attached,
	// which on a real store reaches 36 MiB for a single row. A bound that let
	// the message pass spend the whole budget therefore produced a preview of
	// dozens of turns with no content in any of them. Reserving most of the
	// budget for parts keeps a bounded preview readable.
	openCodeLegacyMessageBudgetShare = 4
)

type openCodeLegacyProjection struct {
	Format    string                            `json:"format"`
	Version   int                               `json:"version"`
	SessionID string                            `json:"session_id"`
	Messages  []openCodeLegacyProjectionMessage `json:"messages"`
}

type openCodeLegacyProjectionMessage struct {
	ID          string                         `json:"id"`
	SessionID   string                         `json:"session_id"`
	TimeCreated int64                          `json:"time_created"`
	TimeUpdated int64                          `json:"time_updated"`
	Data        json.RawMessage                `json:"data"`
	Parts       []openCodeLegacyProjectionPart `json:"parts"`
	// Orphan marks a synthetic message that carries one orphan part whose parent
	// message is absent from the selected source. The indexer reads it directly,
	// so no fabricated parent link is needed.
	Orphan bool `json:"orphan,omitempty"`
	// Control names a current-schema control record, such as a model switch or
	// an agent switch, that carries no upstream message id and no transcript
	// content. The legacy projection never sets it, so its bytes are unchanged.
	Control string `json:"control,omitempty"`
}

type openCodeLegacyProjectionPart struct {
	ID          string          `json:"id"`
	MessageID   string          `json:"message_id"`
	SessionID   string          `json:"session_id"`
	TimeCreated int64           `json:"time_created"`
	TimeUpdated int64           `json:"time_updated"`
	Data        json.RawMessage `json:"data"`
}

func (a *OpenCodeAdapter) discoverLegacySQLite(ctx context.Context, source OpenCodeSQLiteSource, candidate OpenCodeCandidate) ([]DiscoveredSession, error) {
	pageSize, err := NewOpenCodeLegacyPageSize(openCodeLegacyMaterializePage)
	if err != nil {
		return nil, err
	}
	var discovered []DiscoveredSession
	var cursor *OpenCodeLegacySessionCursor
	for {
		page, readErr := source.LegacySessionIDs(ctx, OpenCodeLegacySessionPageRequest{PageSize: pageSize, After: cursor})
		if readErr != nil {
			return nil, fmt.Errorf("discover legacy OpenCode SQLite candidate %q failed while enumerating a bounded session page: %w; no partial discovery result is eligible; verify the database remains a supported legacy message/part store and retry", candidate.Path, readErr)
		}
		for _, legacyID := range page.SessionIDs {
			sessionID, idErr := NewSessionID(legacyID.String())
			if idErr != nil {
				return nil, fmt.Errorf("discover legacy OpenCode SQLite candidate %q found session identifier %q that Peasant cannot store: %w; no partial discovery result is eligible; repair the upstream identifier or use a supported export", candidate.Path, legacyID.String(), idErr)
			}
			session := DiscoveredSession{
				SessionID:        sessionID,
				Harness:          HarnessOpenCode,
				SourcePath:       ResolvedPath(candidate.Path),
				SourceFormat:     SourceFormatJSON,
				OriginalRoot:     ResolvedPath(filepath.Dir(candidate.Path)),
				TranscriptOrigin: TranscriptOriginOpenCodeLegacySQLite,
			}
			discovered = append(discovered, session)
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	return discovered, nil
}

// sqliteContentModTime returns the newest content-bearing source time of one
// OpenCode database. Discovery uses it as the freshness floor for every
// selected SQLite session, because a row deletion moves the file or WAL mtime
// but never raises the surviving rows' own times. SQLite's shared-memory
// sidecar contains reader coordination state rather than committed transcript
// content and is deliberately never inspected here.
func sqliteContentModTime(filesystem FileSystem, databasePath string) (time.Time, error) {
	databaseInfo, err := filesystem.Stat(databasePath)
	if err != nil {
		return time.Time{}, fmt.Errorf("determine legacy OpenCode SQLite freshness for %q failed while inspecting the main database: %w; discovery cannot classify sessions safely without content evidence; verify the database still exists and is readable, then retry", databasePath, err)
	}
	modified := databaseInfo.ModTime()
	walPath := databasePath + "-wal"
	walInfo, err := filesystem.Stat(walPath)
	if errors.Is(err, fs.ErrNotExist) {
		return modified, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("determine legacy OpenCode SQLite freshness for %q failed while inspecting committed WAL freshness evidence at %q: %w; discovery cannot distinguish a current source from stale managed output; fix sidecar permissions or source accessibility and retry", databasePath, walPath, err)
	}
	// A zero-length or header-only WAL can be benign reader-created residue. It
	// has no transaction frame and therefore is not content freshness evidence.
	const sqliteWALHeaderBytes = 32
	if walInfo.Size() <= sqliteWALHeaderBytes {
		return modified, nil
	}
	if walInfo.ModTime().After(modified) {
		modified = walInfo.ModTime()
	}
	return modified, nil
}

// MaterializeTranscript reads only the selected detached legacy rows and builds
// the versioned JSON transcript Peasant owns. Database, WAL, and SHM bytes never
// enter the returned data.
func (a *OpenCodeAdapter) MaterializeTranscript(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, []byte, error) {
	if session.TranscriptOrigin == TranscriptOriginOpenCodeCurrentSQLite {
		return a.materializeCurrentTranscript(ctx, session)
	}
	if session.TranscriptOrigin != TranscriptOriginOpenCodeLegacySQLite {
		return nil, nil, fmt.Errorf("materialize OpenCode session %q failed before source access: transcript origin %d is not a supported managed OpenCode SQLite origin; no managed state was written; use the file origin for JSON sessions or return a supported typed SQLite origin from discovery", session.SessionID, session.TranscriptOrigin)
	}
	legacyID, err := NewOpenCodeLegacySessionID(string(session.SessionID))
	if err != nil {
		return nil, nil, err
	}
	pageSize, err := NewOpenCodeLegacyPageSize(openCodeLegacyMaterializePage)
	if err != nil {
		return nil, nil, err
	}
	var projection openCodeLegacyProjection
	var dropped []openCodeDroppedOrphanPart
	if err := a.withOpenCodeSQLiteSource(ctx, session.SourcePath.String(), func(source OpenCodeSQLiteSource) error {
		var readErr error
		projection, dropped, readErr = readOpenCodeLegacyProjectionWithDiagnostics(ctx, source, legacyID, pageSize)
		return readErr
	}); err != nil {
		return nil, nil, fmt.Errorf("materialize legacy OpenCode SQLite session %q failed while reading selected message/part rows and closing the bounded source: %w; no partial managed artifact or store row was written; fix malformed required row JSON or retry after source locks clear", session.SessionID, err)
	}
	return a.finishLegacyManagedProjection(ctx, session, projection, dropped)
}

// finishLegacyManagedProjection encodes a read legacy projection into the
// managed JSON bytes and derives its metadata. The full-session and preview
// prefix reads share it, so both encode and attribute the projection the same
// way; the prefix read simply hands it a projection bounded by the budget.
func (a *OpenCodeAdapter) finishLegacyManagedProjection(ctx context.Context, session DiscoveredSession, projection openCodeLegacyProjection, dropped []openCodeDroppedOrphanPart) (*UnifiedMetadata, []byte, error) {
	if len(projection.Messages) == 0 {
		return nil, nil, fmt.Errorf("materialize legacy OpenCode SQLite session %q from %q produced no messages even though discovery enumerated it; no empty managed artifact was written; retry after OpenCode finishes its transaction or remove the stale source row", session.SessionID, session.SourcePath)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return nil, nil, fmt.Errorf("materialize legacy OpenCode SQLite session %q failed while encoding the versioned managed JSON projection: %w; detached source rows remain unchanged and no managed state was written; report the unsupported row shape", session.SessionID, err)
	}
	data = append(data, '\n')
	metadata, err := a.metadataFromManagedProjection(ctx, session, projection)
	if err != nil {
		return nil, nil, err
	}
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, droppedOpenCodeOrphanPartDiagnostics(session, dropped)...)
	return metadata, data, nil
}

// MaterializeTranscriptBounded materializes a preview-only prefix of a session
// under budgetBytes. It first probes the session's payload size. When the
// payload fits the budget it materializes the whole session and reports no
// truncation, so a short session previews exactly as it ingests. When the
// payload is over the budget it materializes a prefix, stopping once the summed
// part or message payload reaches the budget, and reports how much it left out.
// The whole session still ingests normally through MaterializeTranscript; this
// bound applies only to the preview.
func (a *OpenCodeAdapter) MaterializeTranscriptBounded(ctx context.Context, session DiscoveredSession, budgetBytes int64) (*UnifiedMetadata, []byte, MaterializeTruncation, error) {
	switch session.TranscriptOrigin {
	case TranscriptOriginOpenCodeCurrentSQLite:
		return a.materializeCurrentTranscriptBounded(ctx, session, budgetBytes)
	case TranscriptOriginOpenCodeLegacySQLite:
		return a.materializeLegacyTranscriptBounded(ctx, session, budgetBytes)
	default:
		return nil, nil, MaterializeTruncation{}, fmt.Errorf("materialize bounded OpenCode session %q failed before source access: transcript origin %d is not a supported managed OpenCode SQLite origin; no managed state was written; use the file origin for JSON sessions or return a supported typed SQLite origin from discovery", session.SessionID, session.TranscriptOrigin)
	}
}

// MaterializeTranscriptFirstPage materializes the leading slice of a session
// under budgetBytes and reports whether the session continues past it.
//
// It differs from MaterializeTranscriptBounded in one deliberate way: it does
// NOT probe the session's payload size first. That probe sums the payload
// length of every row of the session, which on a multi-gigabyte session reads
// the overflow pages of every row and costs seconds, and its only product is
// the total a truncation note quotes. This read writes no note, so it skips the
// probe and reads rows straight away.
//
// The whole session still ingests normally; this slice is a preview slice
// alone.
func (a *OpenCodeAdapter) MaterializeTranscriptFirstPage(ctx context.Context, session DiscoveredSession, budgetBytes int64) (*UnifiedMetadata, []byte, bool, error) {
	switch session.TranscriptOrigin {
	case TranscriptOriginOpenCodeCurrentSQLite:
		return a.materializeCurrentTranscriptFirstPage(ctx, session, budgetBytes)
	case TranscriptOriginOpenCodeLegacySQLite:
		return a.materializeLegacyTranscriptFirstPage(ctx, session, budgetBytes)
	default:
		return nil, nil, false, fmt.Errorf("materialize the first page of OpenCode session %q failed before source access: transcript origin %d is not a supported managed OpenCode SQLite origin; no managed state was written; use the file origin for JSON sessions or return a supported typed SQLite origin from discovery", session.SessionID, session.TranscriptOrigin)
	}
}

func (a *OpenCodeAdapter) materializeLegacyTranscriptFirstPage(ctx context.Context, session DiscoveredSession, budgetBytes int64) (*UnifiedMetadata, []byte, bool, error) {
	legacyID, err := NewOpenCodeLegacySessionID(string(session.SessionID))
	if err != nil {
		return nil, nil, false, err
	}
	pageSize, err := NewOpenCodeLegacyPageSize(openCodeLegacyMaterializePage)
	if err != nil {
		return nil, nil, false, err
	}
	if budgetBytes <= 0 {
		return nil, nil, false, fmt.Errorf("materialize the first page of legacy OpenCode SQLite session %q failed before source access: the byte budget %d is not positive; no managed state was written; pass the preview first-page budget", session.SessionID, budgetBytes)
	}
	var projection openCodeLegacyProjection
	var dropped []openCodeDroppedOrphanPart
	var truncation MaterializeTruncation
	if err := a.withOpenCodeSQLiteSource(ctx, session.SourcePath.String(), func(source OpenCodeSQLiteSource) error {
		// The zero payload size is correct here, not a missing measurement: the
		// core reads it only to fill the totals a truncation note quotes, and
		// this read reports no note. Truncated is decided by the read loops
		// alone, so it stays honest under a zero size.
		var readErr error
		projection, dropped, truncation, readErr = readOpenCodeLegacyProjectionCore(ctx, source, legacyID, pageSize, budgetBytes, OpenCodePayloadSize{})
		return readErr
	}); err != nil {
		return nil, nil, false, fmt.Errorf("materialize the first page of legacy OpenCode SQLite session %q failed while reading selected message/part rows and closing the bounded source: %w; no partial managed artifact or store row was written; fix malformed required row JSON or retry after source locks clear", session.SessionID, err)
	}
	metadata, data, err := a.finishLegacyManagedProjection(ctx, session, projection, dropped)
	if err != nil {
		return nil, nil, false, err
	}
	return metadata, data, truncation.Truncated, nil
}

func (a *OpenCodeAdapter) materializeLegacyTranscriptBounded(ctx context.Context, session DiscoveredSession, budgetBytes int64) (*UnifiedMetadata, []byte, MaterializeTruncation, error) {
	legacyID, err := NewOpenCodeLegacySessionID(string(session.SessionID))
	if err != nil {
		return nil, nil, MaterializeTruncation{}, err
	}
	pageSize, err := NewOpenCodeLegacyPageSize(openCodeLegacyMaterializePage)
	if err != nil {
		return nil, nil, MaterializeTruncation{}, err
	}
	var projection openCodeLegacyProjection
	var dropped []openCodeDroppedOrphanPart
	var truncation MaterializeTruncation
	if err := a.withOpenCodeSQLiteSource(ctx, session.SourcePath.String(), func(source OpenCodeSQLiteSource) error {
		size, sizeErr := source.LegacySessionPayloadSize(ctx, legacyID)
		if sizeErr != nil {
			return sizeErr
		}
		budget := int64(0)
		if budgetBytes > 0 && size.Bytes > budgetBytes {
			// Only bound the read when the session is genuinely over the budget, so
			// a session that fits materializes exactly as it ingests.
			budget = budgetBytes
		}
		var readErr error
		projection, dropped, truncation, readErr = readOpenCodeLegacyProjectionCore(ctx, source, legacyID, pageSize, budget, size)
		return readErr
	}); err != nil {
		return nil, nil, MaterializeTruncation{}, fmt.Errorf("materialize bounded legacy OpenCode SQLite session %q failed while reading selected message/part rows and closing the bounded source: %w; no partial managed artifact or store row was written; fix malformed required row JSON or retry after source locks clear", session.SessionID, err)
	}
	metadata, data, err := a.finishLegacyManagedProjection(ctx, session, projection, dropped)
	if err != nil {
		return nil, nil, MaterializeTruncation{}, err
	}
	return metadata, data, truncation, nil
}

// openCodeDroppedOrphanPart records one selected orphan part row that the
// projection could not carry as transcript content.
type openCodeDroppedOrphanPart struct {
	partID    string
	messageID string
	reason    string
}

// readOpenCodeLegacyProjection reads the selected legacy rows and discards the
// dropped-orphan notes. Production callers use the diagnostics-returning form.
func readOpenCodeLegacyProjection(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeLegacySessionID, pageSize OpenCodeLegacyPageSize) (openCodeLegacyProjection, error) {
	projection, _, err := readOpenCodeLegacyProjectionWithDiagnostics(ctx, source, sessionID, pageSize)
	return projection, err
}

// unusableOpenCodeOrphanPartReason explains why an orphan part row cannot
// enter the managed projection. It returns "" when the row is usable.
func unusableOpenCodeOrphanPartReason(data []byte) string {
	if err := requireJSONObject(data); err != nil {
		return "its data is not a JSON object: " + err.Error()
	}
	part, err := parseOpenCodeSemanticPart("", 0, data)
	if err != nil {
		return "its data does not decode as an OpenCode part: " + err.Error()
	}
	if !isKnownOpenCodeSemanticPartType(part.Data.Type) {
		return fmt.Sprintf("its type %q is outside the supported transcript part set", part.Data.Type)
	}
	return ""
}

// droppedOpenCodeOrphanPartDiagnostics turns dropped orphan rows into
// actionable warnings on the session metadata. The session still ingests.
func droppedOpenCodeOrphanPartDiagnostics(session DiscoveredSession, dropped []openCodeDroppedOrphanPart) []DiagnosticEntry {
	diagnostics := make([]DiagnosticEntry, 0, len(dropped))
	for _, part := range dropped {
		diagnostics = append(diagnostics, DiagnosticEntry{
			ErrorType:   string(OpenCodeGraphOrphanPartDropped),
			Location:    fmt.Sprintf("selected OpenCode session %s orphan part %s for absent message %s from %s", session.SessionID, part.partID, part.messageID, session.SourcePath),
			Message:     fmt.Sprintf("orphan part %s was dropped from the selected transcript because %s; while materializing the selected legacy rows Peasant kept every other message and part, which means the session ingested without this row", part.partID, part.reason),
			Remediation: "Let OpenCode finish or repair the session, then re-run ingest; if the row stays unusable, export the session through OpenCode rather than editing its database with Peasant.",
		})
	}
	return diagnostics
}

// openCodeUnknownTypeDiagnostics records one warning per distinct tolerated
// type, naming the type and how many rows of it the session carried. The
// subject names what kind of row was tolerated, such as "part" or "control
// message row". A session with no unknown type produces no warning. The order
// is stable so the diagnostics do not churn between runs of one session.
func openCodeUnknownTypeDiagnostics(session DiscoveredSession, counts map[string]int, subject string) []DiagnosticEntry {
	if len(counts) == 0 {
		return nil
	}
	types := make([]string, 0, len(counts))
	for typeName := range counts {
		types = append(types, typeName)
	}
	sort.Strings(types)
	diagnostics := make([]DiagnosticEntry, 0, len(types))
	for _, typeName := range types {
		diagnostics = append(diagnostics, DiagnosticEntry{
			ErrorType:   string(OpenCodeUnknownPartType),
			Location:    fmt.Sprintf("selected OpenCode session %s from %s", session.SessionID, session.SourcePath),
			Message:     fmt.Sprintf("%d %s row(s) of type %q are outside the known transcript vocabulary, so Peasant kept the session and every known row and treated the newer %s rows as inert; this is newer OpenCode vocabulary, not corruption", counts[typeName], subject, typeName, subject),
			Remediation: "No action is required; upgrade Peasant when it adds first-class support for this type if you want it rendered as more than an inert note.",
		})
	}
	return diagnostics
}

func readOpenCodeLegacyProjectionWithDiagnostics(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeLegacySessionID, pageSize OpenCodeLegacyPageSize) (openCodeLegacyProjection, []openCodeDroppedOrphanPart, error) {
	// A zero budget and zero payload size read the whole session: the part loop
	// never crosses an unset budget, so no truncation is possible.
	projection, dropped, _, err := readOpenCodeLegacyProjectionCore(ctx, source, sessionID, pageSize, 0, OpenCodePayloadSize{})
	return projection, dropped, err
}

// readOpenCodeLegacyProjectionCore reads the session's messages and then its
// parts in one pass, partitioning each part into its message or into an orphan.
//
// When budget is positive it stops accumulating once the summed payload byte
// length of the rows it took reaches the budget, and reports the truncation. It
// counts message rows and part rows alike, because a legacy session carries
// payload in both tables and a materialization reads both, and it caps the
// message pass at a share of the budget so the part rows that hold the
// transcript content always have room left. A non-positive budget reads the
// whole session and reports no truncation. size carries the whole-session
// totals so a truncated result can name how much it left out. The full-session
// read and the preview prefix read share this one path.
func readOpenCodeLegacyProjectionCore(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeLegacySessionID, pageSize OpenCodeLegacyPageSize, budget int64, size OpenCodePayloadSize) (openCodeLegacyProjection, []openCodeDroppedOrphanPart, MaterializeTruncation, error) {
	projection, dropped, truncation, _, err := readOpenCodeLegacyProjectionSlice(ctx, source, sessionID, pageSize, budget, size, openCodeLegacySliceStart{})
	return projection, dropped, truncation, err
}

// openCodeLegacySliceStart is where a resumable legacy read begins: the keyset
// positions of the two passes, and the part that blocked the previous slice.
type openCodeLegacySliceStart struct {
	message *OpenCodeLegacyMessageCursor
	part    *OpenCodeLegacyPartCursor
	// blockedPart is the part the PREVIOUS slice stopped at because its message
	// lay past that slice's message window. See openCodeLegacySliceStop.
	blockedPart *OpenCodeLegacyPartID
	// resumable marks a read another read will CONTINUE from. Only such a read
	// ends on a message boundary, because only such a read can come back for
	// what the boundary leaves behind. A one-shot bounded read keeps every
	// window message it took, since dropping one would simply lose it.
	resumable bool
}

// openCodeLegacySliceStop is where a resumable legacy read ended, and is what
// the next slice starts from.
type openCodeLegacySliceStop struct {
	message *OpenCodeLegacyMessageCursor
	part    *OpenCodeLegacyPartCursor
	// blockedPart names the part the part pass refused to take because its
	// message is outside this slice's window. The next slice takes that message
	// into its window, so the part attaches there instead of being lost.
	//
	// It is also the TERMINATION guard. A part that blocks two slices in a row
	// is not merely ahead of the window - it belongs to a message an earlier
	// slice already emitted - so the second slice steps over it rather than
	// stalling on it forever. Stepping over it drops that one part from the
	// preview; it never duplicates a turn, because a message is emitted by
	// exactly one slice.
	blockedPart *OpenCodeLegacyPartID
	// exhausted reports that BOTH passes reached the end of the session, which
	// is the only state that means there is nothing more to read.
	exhausted bool
	// includedBytes and includedRows are what THIS slice put into the
	// projection. They are reported separately from the truncation record
	// because the last slice of a session is not truncated and would otherwise
	// report nothing.
	includedBytes int64
	includedRows  int64
}

// readOpenCodeLegacyProjectionSlice is the resumable form of the core read.
//
// The slice contract, which is what makes appending one slice's turns to the
// previous slice's safe:
//
//   - The message pass is a strictly forward keyset walk, and every slice takes
//     at least one message. A message therefore enters exactly ONE slice, so an
//     appended slice can never duplicate or halve a turn at the seam.
//   - The part pass attaches a part ONLY to a message in the CURRENT slice's
//     window. On reaching a part whose message is outside the window it stops
//     WITHOUT consuming that part, so the next slice - whose window has moved
//     forward - sees the same part again and attaches it. The previous
//     behaviour, skipping such a part, would silently lose it once the read
//     could continue.
func readOpenCodeLegacyProjectionSlice(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeLegacySessionID, pageSize OpenCodeLegacyPageSize, budget int64, size OpenCodePayloadSize, from openCodeLegacySliceStart) (openCodeLegacyProjection, []openCodeDroppedOrphanPart, MaterializeTruncation, openCodeLegacySliceStop, error) {
	projection := openCodeLegacyProjection{Format: openCodeLegacyProjectionFormat, Version: openCodeLegacyProjectionVersion, SessionID: sessionID.String(), Messages: []openCodeLegacyProjectionMessage{}}
	// Read the session's messages first and remember each message's slot, so the
	// single part pass can attach a part to its message in memory.
	messageSlot := make(map[string]int)
	// messageCursors[slot] is the continuation cursor of the window message in
	// that slot, parallel to the leading real-message run of projection.Messages
	// (an orphan message is appended past that run and has no cursor).
	var messageCursors []OpenCodeLegacyMessageCursor
	messageCursor := from.message
	stop := openCodeLegacySliceStop{message: from.message, part: from.part}
	// messagesExhausted and partsExhausted record that a pass walked off the end
	// of the session rather than stopping on its budget or on the window edge.
	messagesExhausted, partsExhausted := false, false
	// readBytes is what the read pulled out of the source and is what the budget
	// governs. includedRows and includedBytes are what reached the projection,
	// which is what a note about the preview describes: a part of a message the
	// read stopped short of was paid for but is not shown.
	var readBytes, includedBytes, includedRows int64
	messagesTruncated := false
	partsTruncated := false
	messageBudget := budget / openCodeLegacyMessageBudgetShare
messageLoop:
	for {
		page, err := source.LegacyMessages(ctx, OpenCodeLegacyMessagePageRequest{SessionID: sessionID, PageSize: pageSize, After: messageCursor})
		if err != nil {
			return openCodeLegacyProjection{}, nil, MaterializeTruncation{}, openCodeLegacySliceStop{}, err
		}
		for _, row := range page.Messages {
			// The budget is checked BEFORE the row is taken, so a read that spent
			// its budget exactly on the last row of the session reports no
			// truncation: there was nothing left to leave out.
			if messageBudget > 0 && readBytes >= messageBudget {
				messagesTruncated = true
				break messageLoop
			}
			// A legacy message row carries payload of its own, past 36 MiB on a
			// real store, so the budget must count it. Counting parts alone let a
			// message read pull gigabytes before any bound applied.
			readBytes += int64(len(row.Data))
			includedRows++
			includedBytes += int64(len(row.Data))
			messageSlot[row.ID.String()] = len(projection.Messages)
			projection.Messages = append(projection.Messages, openCodeLegacyProjectionMessage{ID: row.ID.String(), SessionID: row.SessionID.String(), TimeCreated: row.TimeCreated, TimeUpdated: row.TimeUpdated, Data: json.RawMessage(row.Data), Parts: []openCodeLegacyProjectionPart{}})
			taken, cursorErr := NewOpenCodeLegacyMessageCursor(row.TimeCreated, row.ID)
			if cursorErr != nil {
				return openCodeLegacyProjection{}, nil, MaterializeTruncation{}, openCodeLegacySliceStop{}, fmt.Errorf("record the continuation position of legacy OpenCode message %q failed while reading a bounded slice: %w; no partial managed projection was emitted; repair the row identity in OpenCode and retry", row.ID.String(), cursorErr)
			}
			// The cursor of every window message is kept beside it, so a slice
			// that ends inside its window can name the message it cut at.
			messageCursors = append(messageCursors, taken)
			stop.message = &taken
		}
		if page.Next == nil {
			messagesExhausted = true
			break
		}
		messageCursor = page.Next
	}
	// Read the session's parts once, in identifier order, and partition them in
	// memory: a part whose message is in this slice's window attaches to that
	// message; a part with no message at all, on a read that covered the whole
	// session, is an orphan.
	//
	// A SLICE ENDS ON A MESSAGE BOUNDARY. When the budget runs out part-way
	// through a message the pass keeps taking that message's remaining parts and
	// stops at the first part of the NEXT message; every window message after
	// the one it cut at is then dropped from this slice and read again by the
	// next one. That is what lets a caller simply APPEND one slice's turns to
	// the previous slice's: a message, and therefore the turn folded from it,
	// belongs to exactly one slice. The overshoot is bounded by one message's
	// parts, the same shape of bound the message pass already accepts for one
	// oversized row.
	var dropped []openCodeDroppedOrphanPart
	partCursor := from.part
	// wholeSession is true only when the message pass covered EVERY message of
	// the session: it started at the beginning and ran off the end. That is the
	// one state in which a part with no message in memory is a true orphan.
	//
	// On a continuation the map holds this slice's window alone, so an
	// unattached part is far more likely to belong to a message an adjacent
	// slice owns. Synthesizing a root attachment for it there would invent a
	// turn the session never held, and would show a part twice once its real
	// message arrives.
	wholeSession := from.message == nil && !messagesTruncated
	// cutSlot and cutPart name where the slice ends - the last window message
	// whose parts are fully read, and that message's last part - so a trim can
	// resume exactly there.
	cutSlot := -1
	var cutPart *OpenCodeLegacyPartCursor
	// partSpend is what the PART pass is charged against the budget.
	//
	// A one-shot read charges it the message pass's spend too, so the budget
	// bounds the whole read's live payload in one number. A RESUMABLE read
	// cannot: a single legacy message row reaches 25 MiB on a real store, which
	// alone exceeds a sensible slice budget, so a shared counter would leave
	// every continuation able to afford exactly one message and turn a 2 GiB
	// session into thousands of slices. A slice therefore charges the two passes
	// separately, and its live payload is bounded by the message share plus the
	// part budget plus one oversized row.
	partSpend := readBytes
	if from.resumable {
		partSpend = 0
	}
	lastAttached := ""
	trimmed := false
partLoop:
	for {
		page, err := source.LegacySessionParts(ctx, OpenCodeLegacySessionPartPageRequest{SessionID: sessionID, PageSize: pageSize, After: partCursor})
		if err != nil {
			return openCodeLegacyProjection{}, nil, MaterializeTruncation{}, openCodeLegacySliceStop{}, err
		}
		for _, drop := range page.Dropped {
			if _, present := messageSlot[drop.MessageID.String()]; present && !drop.MessageID.IsEmpty() {
				// A part whose message is present must decode. A decode failure
				// fails the whole session, matching the strict per-message read
				// this single pass replaces; it is not a tolerable orphan.
				return openCodeLegacyProjection{}, nil, MaterializeTruncation{}, openCodeLegacySliceStop{}, fmt.Errorf("read OpenCode legacy part %q for present message %q failed while partitioning the single part pass: %s; no partial managed projection was emitted; repair the malformed part in OpenCode and retry", drop.PartID.String(), drop.MessageID.String(), drop.Reason)
			}
			if !wholeSession {
				// The message is absent from THIS slice's window, which does not
				// make it absent from the session. Calling it an orphan here would
				// warn about a row an adjacent slice owns.
				continue
			}
			// A part row whose message is absent is a true orphan. Drop it with a
			// warning that names the row, never a session failure.
			dropped = append(dropped, openCodeDroppedOrphanPart{partID: drop.PartID.String(), messageID: drop.MessageID.String(), reason: drop.Reason})
		}
		for _, part := range page.Parts {
			messageID := part.MessageID.String()
			slot, present := messageSlot[messageID]
			if !present && !wholeSession {
				// The part belongs to a message this slice's window stops short
				// of. Stop WITHOUT consuming it, so the next slice - whose window
				// has moved forward past that message - sees this same part and
				// attaches it. Skipping it would drop transcript content the read
				// is able to come back for.
				//
				// Unless the previous slice already stopped on this same part.
				// Then the window has moved and it still does not attach, which
				// means its message came BEFORE the window and an earlier slice
				// already emitted it. Step over it so the read always makes
				// progress; it is left out of the preview, never shown twice.
				if from.blockedPart != nil && *from.blockedPart == part.ID {
					readBytes += int64(len(part.Data))
					partSpend += int64(len(part.Data))
					stepped, cursorErr := NewOpenCodeLegacyPartCursor(part.ID)
					if cursorErr != nil {
						return openCodeLegacyProjection{}, nil, MaterializeTruncation{}, openCodeLegacySliceStop{}, fmt.Errorf("record the continuation position of legacy OpenCode part %q failed while stepping over an out-of-window row: %w; no partial managed projection was emitted; repair the row identity in OpenCode and retry", part.ID.String(), cursorErr)
					}
					stop.part = &stepped
					cutPart = &stepped
					continue
				}
				blocked := part.ID
				stop.blockedPart = &blocked
				partsTruncated = true
				break partLoop
			}
			// Every part row the source returned counts toward the budget, whether
			// or not it reaches the projection: the budget bounds how much source
			// payload this read pulls into memory, and a skipped row was already
			// paid for. Counting only the rows that landed let a session whose
			// message pass stopped early scan its whole part table.
			if budget > 0 && partSpend >= budget {
				if !from.resumable {
					partsTruncated = true
					break partLoop
				}
				if lastAttached != "" && messageID != lastAttached {
					// The drain is finished: this part opens a new message, which
					// is the message boundary the slice ends on.
					partsTruncated = true
					trimmed = true
					break partLoop
				}
				// The budget is spent inside this message. Keep taking its parts
				// so the slice can end after it rather than inside it.
			}
			readBytes += int64(len(part.Data))
			partSpend += int64(len(part.Data))
			takenPart, cursorErr := NewOpenCodeLegacyPartCursor(part.ID)
			if cursorErr != nil {
				return openCodeLegacyProjection{}, nil, MaterializeTruncation{}, openCodeLegacySliceStop{}, fmt.Errorf("record the continuation position of legacy OpenCode part %q failed while reading a bounded slice: %w; no partial managed projection was emitted; repair the row identity in OpenCode and retry", part.ID.String(), cursorErr)
			}
			stop.part = &takenPart
			projectionPart := openCodeLegacyProjectionPart{ID: part.ID.String(), MessageID: messageID, SessionID: part.SessionID.String(), TimeCreated: part.TimeCreated, TimeUpdated: part.TimeUpdated, Data: json.RawMessage(part.Data)}
			if present {
				// A part whose message is present must carry valid JSON, matching
				// the strict per-message read this pass replaces.
				if !json.Valid([]byte(part.Data)) {
					return openCodeLegacyProjection{}, nil, MaterializeTruncation{}, openCodeLegacySliceStop{}, fmt.Errorf("read OpenCode legacy part %q for message %q failed while partitioning the single part pass: its data is not valid JSON; no partial managed projection was emitted; repair the malformed part in OpenCode and retry", part.ID.String(), messageID)
				}
				projection.Messages[slot].Parts = append(projection.Messages[slot].Parts, projectionPart)
				includedRows++
				includedBytes += int64(len(part.Data))
				lastAttached = messageID
				if slot > cutSlot {
					cutSlot = slot
				}
				cutPart = &takenPart
				continue
			}
			if reason := unusableOpenCodeOrphanPartReason([]byte(part.Data)); reason != "" {
				// An unusable orphan row must not fail the whole session.
				dropped = append(dropped, openCodeDroppedOrphanPart{partID: part.ID.String(), messageID: messageID, reason: reason})
				continue
			}
			syntheticMessageID := "orphan-parent-" + part.ID.String()
			// The orphan slot is a typed field on the message, not an in-band
			// marker with a fabricated parentID. The synthetic message carries no
			// parent link, so it never trips the missing-parent diagnostic.
			data, marshalErr := json.Marshal(map[string]any{
				"id":        syntheticMessageID,
				"sessionID": sessionID.String(),
				"role":      RoleSystem.String(),
			})
			if marshalErr != nil {
				return openCodeLegacyProjection{}, nil, MaterializeTruncation{}, openCodeLegacySliceStop{}, fmt.Errorf("normalize orphan legacy part %q failed while encoding its root attachment: %w; the selected row was not dropped and no partial projection was emitted; report the unsupported row identity", part.ID.String(), marshalErr)
			}
			projection.Messages = append(projection.Messages, openCodeLegacyProjectionMessage{
				ID: syntheticMessageID, SessionID: sessionID.String(), TimeCreated: part.TimeCreated, TimeUpdated: part.TimeUpdated, Data: data, Orphan: true,
				Parts: []openCodeLegacyProjectionPart{{ID: part.ID.String(), MessageID: syntheticMessageID, SessionID: sessionID.String(), TimeCreated: part.TimeCreated, TimeUpdated: part.TimeUpdated, Data: json.RawMessage(part.Data)}},
			})
			includedRows++
			includedBytes += int64(len(part.Data))
		}
		if page.Next == nil {
			partsExhausted = true
			break
		}
		partCursor = page.Next
	}
	if trimmed && cutSlot >= 0 && cutSlot+1 < len(projection.Messages) {
		// End the slice on the message boundary the drain stopped at. The window
		// messages past the cut were never given their parts, so this slice drops
		// them whole and the next slice reads them again from the cut cursor.
		// They were never emitted, so nothing is shown twice.
		for _, message := range projection.Messages[cutSlot+1:] {
			includedRows--
			includedBytes -= int64(len(message.Data))
			for _, part := range message.Parts {
				includedRows--
				includedBytes -= int64(len(part.Data))
			}
		}
		projection.Messages = projection.Messages[:cutSlot+1]
		if cutSlot < len(messageCursors) {
			cut := messageCursors[cutSlot]
			stop.message = &cut
		}
		stop.part = cutPart
		messagesTruncated = true
	}
	stop.exhausted = messagesExhausted && partsExhausted && !trimmed
	stop.includedBytes = includedBytes
	stop.includedRows = includedRows
	truncation := MaterializeTruncation{}
	if messagesTruncated || partsTruncated {
		truncation = MaterializeTruncation{Truncated: true, Unit: MaterializeUnitRows, BudgetBytes: budget, IncludedBytes: includedBytes, TotalBytes: size.Bytes, IncludedRows: includedRows, TotalRows: size.Rows}
	}
	return projection, dropped, truncation, stop, nil
}

func (a *OpenCodeAdapter) metadataFromManagedProjection(ctx context.Context, session DiscoveredSession, projection openCodeLegacyProjection) (*UnifiedMetadata, error) {
	metadata := NewUnifiedMetadata()
	metadata.SessionID = session.SessionID
	metadata.ModelHarness = HarnessOpenCode
	metadata.Source = SourceInfo{FilePath: session.SourcePath.String(), Format: SourceFormatJSON}
	metadata.CWD = session.CWD
	if session.ParentUUID != nil {
		parent := *session.ParentUUID
		metadata.ParentUUID = &parent
	}

	kind := managedOpenCodeProjectionKind(session.TranscriptOrigin)
	messages, unknownPartTypes, err := parseManagedOpenCodeSemanticMessages(projection, kind)
	if err != nil {
		return nil, fmt.Errorf("extract metadata from %s projection for session %q failed while decoding the shared semantic corpus: %w; no managed artifact or store state was committed, so repair the malformed normalized row and retry harvest", kind, session.SessionID, err)
	}
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, openCodeUnknownTypeDiagnostics(session, unknownPartTypes, "part")...)
	summary := summarizeOpenCodeSemanticMessages(messages)
	metadata.Stats = StatsInfo{TurnCount: summary.turnCount, ToolCallCount: summary.toolCallCount, TokensIn: summary.tokensIn, TokensOut: summary.tokensOut, DurationMs: summary.endMS - summary.startMS}
	metadata.Timestamp = TimestampInfo{Start: summary.startMS, End: summary.endMS}
	metadata.Version = summary.version
	// The session row carries authoritative token totals and the harness version,
	// so a session whose row exposed them fills the statistics and version without
	// folding entries. An older layout leaves these zero or empty on the session,
	// so the folded summary above still stands.
	if session.TokensIn > 0 || session.TokensOut > 0 {
		metadata.Stats.TokensIn = session.TokensIn
		metadata.Stats.TokensOut = session.TokensOut
	}
	if session.Version != "" {
		metadata.Version = session.Version
	}
	workDir := session.CWD
	if workDir == "" {
		workDir = summary.cwd
		metadata.CWD = workDir
	}
	projectMissing := workDir == ""
	modelMissing := summary.modelID == ""
	if !modelMissing {
		if parsed, modelErr := NewModelID(summary.modelID); modelErr == nil {
			metadata.Model = parsed
		} else {
			modelMissing = true
		}
	}
	var gitBranch, gitRemote, gitWorktree, gitTracking *string
	if workDir != "" {
		if value, gitErr := a.git.Branch(ctx, workDir); gitErr == nil && value != "" {
			gitBranch = &value
		}
		if value, gitErr := a.git.RemoteURL(ctx, workDir); gitErr == nil && value != "" {
			gitRemote = &value
		}
		if value, gitErr := a.git.Worktree(ctx, workDir); gitErr == nil && value != "" {
			gitWorktree = &value
		}
		if value, gitErr := a.git.TrackingBranch(ctx, workDir); gitErr == nil && value != "" {
			gitTracking = &value
		}
	}
	identityPath := workDir
	if gitWorktree != nil {
		identityPath = *gitWorktree
	}
	// The project tables give the canonical project root, so a session grouped
	// under a project resolves its identity from the project root rather than its
	// own worktree directory. Worktrees of one project then share a project. Git
	// worktree resolution, when it succeeds, still refines the root.
	if session.ProjectWorktree != "" && gitWorktree == nil {
		identityPath = session.ProjectWorktree
	}
	if identityPath == "" {
		identityPath = session.SourcePath.String()
	}
	remote := ""
	if gitRemote != nil {
		remote = *gitRemote
	}
	projectHash, hostSlug, err := DeriveProjectIdentifiersWithGit(ctx, a.salt, a.git, remote, identityPath)
	if err != nil {
		return nil, fmt.Errorf("extract metadata from %s projection for session %q failed while deriving stable source identity from %q: %w; no managed artifact or store state was committed, so verify the selected path and installation salt and retry", kind, session.SessionID, identityPath, err)
	}
	metadata.HostSlug = hostSlug
	metadata.Project.Hash = projectHash
	if workDir != "" {
		metadata.Project.FilePath = workDir
		metadata.Project.Name = filepath.Base(workDir)
	}
	// The project tables, when present, name the project and locate its root, so
	// a session groups under the project root rather than under its own worktree
	// directory. CWD stays the session directory set above.
	if session.ProjectWorktree != "" {
		metadata.Project.FilePath = session.ProjectWorktree
		if session.ProjectName != "" {
			metadata.Project.Name = session.ProjectName
		} else {
			metadata.Project.Name = filepath.Base(session.ProjectWorktree)
		}
	}
	if gitBranch != nil || gitRemote != nil || gitWorktree != nil || gitTracking != nil {
		metadata.Git = GitContext{Branch: gitBranch, Remote: gitRemote, Worktree: gitWorktree, Tracking: gitTracking}
	}
	if modelMissing {
		metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, DiagnosticEntry{ErrorType: "missing_model", Location: fmt.Sprintf("%s session %s", kind, session.SessionID), Message: "selected assistant messages contain no valid model identifier; Peasant left model empty rather than inventing one", Remediation: "Use an OpenCode source that records a supported assistant model or retain the session with an unknown model."})
	}
	if projectMissing {
		metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, DiagnosticEntry{ErrorType: "missing_project", Location: fmt.Sprintf("%s session %s in %s", kind, session.SessionID, session.SourcePath), Message: "selected messages contain no working-directory or project evidence; Peasant left project name and path empty and used only the source path for stable local placement", Remediation: "Use an OpenCode source that records path.cwd, cwd, or directory if project attribution is required."})
	}
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, missingOpenCodeParentDiagnostics(session, messages)...)
	return &metadata, nil
}

// recognizeManagedOpenCodeProjection distinguishes ordinary legacy JSON from a
// Peasant-owned envelope. A recognizable managed marker with invalid bytes is
// an error, not a file-origin fallback, so recovery cannot erase an index by
// treating corruption as an empty legacy directory.
func recognizeManagedOpenCodeProjection(data []byte, sessionID SessionID) (TranscriptOrigin, error) {
	formatMarker := managedOpenCodeFormatMarker(data)
	for _, candidate := range []struct {
		origin  TranscriptOrigin
		format  string
		version int
	}{
		{TranscriptOriginOpenCodeLegacySQLite, openCodeLegacyProjectionFormat, openCodeLegacyProjectionVersion},
		{TranscriptOriginOpenCodeCurrentSQLite, openCodeCurrentProjectionFormat, openCodeCurrentProjectionVersion},
	} {
		if formatMarker != candidate.format {
			continue
		}
		if _, err := decodeManagedOpenCodeProjection(data, candidate.format, candidate.version, sessionID); err != nil {
			return TranscriptOriginFile, fmt.Errorf("recognize managed OpenCode projection for session %q failed: found managed format marker %q but its envelope is corrupt: %w; recovery stopped before legacy fallback, so re-run harvest to regenerate the managed transcript", sessionID, candidate.format, err)
		}
		return candidate.origin, nil
	}
	return TranscriptOriginFile, nil
}

func managedOpenCodeFormatMarker(data []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ""
	}
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		key, ok := keyToken.(string)
		if keyErr != nil || !ok {
			return ""
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return ""
		}
		if key != "format" {
			continue
		}
		var format string
		if err := json.Unmarshal(value, &format); err != nil {
			return ""
		}
		return format
	}
	return ""
}

func decodeManagedOpenCodeProjection(data []byte, expectedFormat string, expectedVersion int, sessionID SessionID) (openCodeLegacyProjection, error) {
	fields, err := decodeOpenCodeProjectionObject(data, "managed envelope", []string{"format", "version", "session_id", "messages"})
	if err != nil {
		return openCodeLegacyProjection{}, err
	}
	var projection openCodeLegacyProjection
	if err := json.Unmarshal(fields["format"], &projection.Format); err != nil {
		return projection, fmt.Errorf("decode managed envelope format: %w", err)
	}
	if err := json.Unmarshal(fields["version"], &projection.Version); err != nil {
		return projection, fmt.Errorf("decode managed envelope version: %w", err)
	}
	if err := json.Unmarshal(fields["session_id"], &projection.SessionID); err != nil {
		return projection, fmt.Errorf("decode managed envelope session_id: %w", err)
	}
	// Accept any readable version up to the current write version, so a previously
	// persisted artifact still decodes after the format is bumped.
	if projection.Format != expectedFormat || projection.Version < openCodeLegacyProjectionMinReadableVersion || projection.Version > expectedVersion || projection.SessionID != string(sessionID) {
		return projection, fmt.Errorf("managed envelope identity format=%q version=%d session_id=%q does not match expected format=%q readable version 1..%d selected_session_id=%q", projection.Format, projection.Version, projection.SessionID, expectedFormat, expectedVersion, sessionID)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(fields["messages"], &rows); err != nil {
		return projection, fmt.Errorf("decode managed envelope messages: %w", err)
	}
	if len(rows) == 0 {
		return projection, errors.New("managed envelope messages must be present and non-empty")
	}
	projection.Messages = make([]openCodeLegacyProjectionMessage, 0, len(rows))
	identities := make(map[string]string, len(rows))
	for index, raw := range rows {
		message, messageErr := decodeManagedOpenCodeProjectionMessage(raw, projection.SessionID, identities)
		if messageErr != nil {
			return projection, fmt.Errorf("decode managed envelope message %d: %w", index, messageErr)
		}
		projection.Messages = append(projection.Messages, message)
	}
	if _, _, err := parseManagedOpenCodeSemanticMessages(projection, "managed OpenCode"); err != nil {
		return projection, fmt.Errorf("validate managed envelope semantic corpus: %w", err)
	}
	return projection, nil
}

func decodeManagedOpenCodeProjectionMessage(raw json.RawMessage, sessionID string, identities map[string]string) (openCodeLegacyProjectionMessage, error) {
	fields, err := decodeOpenCodeProjectionObject(raw, "managed message", []string{"id", "session_id", "time_created", "time_updated", "data", "parts"}, "orphan", "control")
	if err != nil {
		return openCodeLegacyProjectionMessage{}, err
	}
	var message openCodeLegacyProjectionMessage
	if orphanField, present := fields["orphan"]; present {
		if err := json.Unmarshal(orphanField, &message.Orphan); err != nil {
			return message, fmt.Errorf("decode orphan: %w", err)
		}
	}
	if controlField, present := fields["control"]; present {
		if err := json.Unmarshal(controlField, &message.Control); err != nil {
			return message, fmt.Errorf("decode control: %w", err)
		}
	}
	if err := json.Unmarshal(fields["id"], &message.ID); err != nil {
		return message, fmt.Errorf("decode id: %w", err)
	}
	if message.ID == "" {
		return message, errors.New("id must be non-empty")
	}
	if err := json.Unmarshal(fields["session_id"], &message.SessionID); err != nil {
		return message, fmt.Errorf("decode session_id: %w", err)
	}
	if message.SessionID != sessionID {
		return message, fmt.Errorf("session_id %q does not match envelope session_id %q", message.SessionID, sessionID)
	}
	if err := json.Unmarshal(fields["time_created"], &message.TimeCreated); err != nil {
		return message, fmt.Errorf("decode time_created: %w", err)
	}
	if err := json.Unmarshal(fields["time_updated"], &message.TimeUpdated); err != nil {
		return message, fmt.Errorf("decode time_updated: %w", err)
	}
	if err := requireJSONObject(fields["data"]); err != nil {
		return message, fmt.Errorf("decode data: %w", err)
	}
	message.Data = append(json.RawMessage(nil), fields["data"]...)
	if prior, exists := identities[message.ID]; exists {
		return message, fmt.Errorf("id %q duplicates %s", message.ID, prior)
	}
	identities[message.ID] = "a message"
	var parts []json.RawMessage
	if err := json.Unmarshal(fields["parts"], &parts); err != nil {
		return message, fmt.Errorf("decode parts: %w", err)
	}
	message.Parts = make([]openCodeLegacyProjectionPart, 0, len(parts))
	for index, partRaw := range parts {
		part, partErr := decodeManagedOpenCodeProjectionPart(partRaw, message.ID, sessionID, identities)
		if partErr != nil {
			return message, fmt.Errorf("decode part %d: %w", index, partErr)
		}
		message.Parts = append(message.Parts, part)
	}
	return message, nil
}

func decodeManagedOpenCodeProjectionPart(raw json.RawMessage, messageID, sessionID string, identities map[string]string) (openCodeLegacyProjectionPart, error) {
	fields, err := decodeOpenCodeProjectionObject(raw, "managed part", []string{"id", "message_id", "session_id", "time_created", "time_updated", "data"})
	if err != nil {
		return openCodeLegacyProjectionPart{}, err
	}
	var part openCodeLegacyProjectionPart
	if err := json.Unmarshal(fields["id"], &part.ID); err != nil {
		return part, fmt.Errorf("decode id: %w", err)
	}
	if part.ID != "" {
		if prior, exists := identities[part.ID]; exists {
			return part, fmt.Errorf("id %q duplicates %s", part.ID, prior)
		}
	}
	if err := json.Unmarshal(fields["message_id"], &part.MessageID); err != nil {
		return part, fmt.Errorf("decode message_id: %w", err)
	}
	if part.MessageID != messageID {
		return part, fmt.Errorf("message_id %q does not match containing message %q", part.MessageID, messageID)
	}
	if err := json.Unmarshal(fields["session_id"], &part.SessionID); err != nil {
		return part, fmt.Errorf("decode session_id: %w", err)
	}
	if part.SessionID != sessionID {
		return part, fmt.Errorf("session_id %q does not match envelope session_id %q", part.SessionID, sessionID)
	}
	if err := json.Unmarshal(fields["time_created"], &part.TimeCreated); err != nil {
		return part, fmt.Errorf("decode time_created: %w", err)
	}
	if err := json.Unmarshal(fields["time_updated"], &part.TimeUpdated); err != nil {
		return part, fmt.Errorf("decode time_updated: %w", err)
	}
	if err := requireJSONObject(fields["data"]); err != nil {
		return part, fmt.Errorf("decode data: %w", err)
	}
	part.Data = append(json.RawMessage(nil), fields["data"]...)
	if part.ID != "" {
		identities[part.ID] = "a part"
	}
	return part, nil
}

func decodeOpenCodeProjectionObject(data []byte, location string, expected []string, optional ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		if err != nil {
			return nil, fmt.Errorf("decode %s opening object: %w", location, managedProjectionJSONError(data, err))
		}
		return nil, fmt.Errorf("decode %s: expected JSON object", location)
	}
	allowed := make(map[string]struct{}, len(expected)+len(optional))
	for _, key := range expected {
		allowed[key] = struct{}{}
	}
	for _, key := range optional {
		allowed[key] = struct{}{}
	}
	fields := make(map[string]json.RawMessage, len(expected)+len(optional))
	for decoder.More() {
		keyToken, tokenErr := decoder.Token()
		key, ok := keyToken.(string)
		if tokenErr != nil || !ok {
			return nil, fmt.Errorf("decode %s field name: %w", location, tokenErr)
		}
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("decode %s: unknown field %q", location, key)
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("decode %s: duplicate field %q", location, key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode %s field %q: %w", location, key, err)
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("decode %s closing object: %w", location, managedProjectionJSONError(data, err))
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode %s: %w", location, err)
	}
	for _, key := range expected {
		if _, present := fields[key]; !present {
			return nil, fmt.Errorf("decode %s: required field %q is missing", location, key)
		}
	}
	return fields, nil
}

func managedProjectionJSONError(data []byte, fallback error) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	return fallback
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func requireJSONObject(data json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	if object == nil {
		return errors.New("must be a JSON object")
	}
	return nil
}
