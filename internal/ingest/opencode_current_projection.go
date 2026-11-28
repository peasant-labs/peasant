package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

const (
	openCodeCurrentProjectionFormat = "peasant.opencode.current-sqlite" // Version 3 adds redacted opaque evidence to normalized messages. The
	// shared readable floor stays 1, including older control-free projections.
	openCodeCurrentProjectionVersion = 3
	openCodeCurrentMaterializePage   = 128
)

type openCodeCurrentProjection struct {
	ContentOmitted bool                              `json:"content_omitted,omitempty"`
	Format         string                            `json:"format"`
	Version        int                               `json:"version"`
	SessionID      string                            `json:"session_id"`
	Messages       []openCodeLegacyProjectionMessage `json:"messages"`
}

// The historical shapes follow upstream 4643e65ad6334de3e4e68dedc201d5fbb828c9fe.
// Native identity and assistant timing compatibility follows the V2 schema at
// 52685d451777fbccc99736ab31ffc37de8475a45 (packages/schema/src/session-message.ts).
// The idle marker follows packages/schema/src/session-message.ts in two steps:
// 2816d1c849 added Session.Message.Idle with its outcome literals, and
// 40cc0f377b added the optional structured error. The earlier V2 pin has no
// idle variant at all.
// The projector stores type and id in columns and the remaining fields in data.
// Redundant payload identities are accepted only when they agree with the columns.
type openCodeCurrentBase struct {
	ID       string                     `json:"id"`
	Type     string                     `json:"type,omitempty"`
	Metadata map[string]json.RawMessage `json:"metadata,omitempty"`
	Time     openCodeCurrentTime        `json:"time"`
}

type openCodeCurrentTime struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed,omitempty"`
}

type openCodeCurrentAssistantTime struct {
	openCodeCurrentTime
	Streamed int64 `json:"streamed,omitempty"`
}

type openCodeCurrentModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

type openCodeCurrentTokens struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

type openCodeCurrentSource struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

type openCodeCurrentFile struct {
	URI         string                 `json:"uri"`
	MIME        string                 `json:"mime"`
	Name        string                 `json:"name,omitempty"`
	Description string                 `json:"description,omitempty"`
	Source      *openCodeCurrentSource `json:"source,omitempty"`
}

type openCodeCurrentAgent struct {
	Name    string                 `json:"name"`
	Source  *openCodeCurrentSource `json:"source,omitempty"`
	Mention *openCodeV2Mention     `json:"mention,omitempty"`
}

type openCodeCurrentUser struct {
	openCodeCurrentBase
	Text   string                      `json:"text"`
	Files  []json.RawMessage           `json:"files,omitempty"`
	Agents []openCodeCurrentAgent      `json:"agents,omitempty"`
	Skills []openCodeV2SkillAttachment `json:"skills,omitempty"`
}

type openCodeCurrentAssistant struct {
	openCodeCurrentBase
	Time          openCodeCurrentAssistantTime `json:"time"`
	ParentID      string                       `json:"parentID,omitempty"`
	Agent         string                       `json:"agent"`
	Model         openCodeCurrentModel         `json:"model"`
	Content       []json.RawMessage            `json:"content"`
	Snapshot      *openCodeCurrentSnapshot     `json:"snapshot,omitempty"`
	Finish        string                       `json:"finish,omitempty"`
	RawFinish     string                       `json:"rawFinish,omitempty"`
	Cost          *float64                     `json:"cost,omitempty"`
	Tokens        *openCodeCurrentTokens       `json:"tokens,omitempty"`
	Error         *openCodeCurrentUnknownError `json:"error,omitempty"`
	ProviderState map[string]json.RawMessage   `json:"providerState,omitempty"`
	Retry         *openCodeV2Retry             `json:"retry,omitempty"`
}

type openCodeCurrentSnapshot struct {
	Start string   `json:"start,omitempty"`
	End   string   `json:"end,omitempty"`
	Files []string `json:"files,omitempty"`
}

type openCodeCurrentUnknownError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  *int   `json:"status,omitempty"`
}

type openCodeCurrentAssistantText struct {
	Type  string                     `json:"type"`
	ID    string                     `json:"id"`
	Text  string                     `json:"text"`
	State map[string]json.RawMessage `json:"state,omitempty"`
}

type openCodeCurrentAssistantReasoning struct {
	Type             string                                `json:"type"`
	ID               string                                `json:"id"`
	Text             string                                `json:"text"`
	ProviderMetadata map[string]map[string]json.RawMessage `json:"providerMetadata,omitempty"`
	Time             *openCodeCurrentTime                  `json:"time,omitempty"`
	State            map[string]json.RawMessage            `json:"state,omitempty"`
}

type openCodeCurrentAssistantTool struct {
	Type                string                       `json:"type"`
	ID                  string                       `json:"id"`
	Name                string                       `json:"name"`
	Provider            *openCodeCurrentToolProvider `json:"provider,omitempty"`
	State               json.RawMessage              `json:"state"`
	Time                openCodeCurrentToolTime      `json:"time"`
	Executed            *bool                        `json:"executed,omitempty"`
	ProviderState       map[string]json.RawMessage   `json:"providerState,omitempty"`
	ProviderResultState map[string]json.RawMessage   `json:"providerResultState,omitempty"`
}

type openCodeCurrentToolProvider struct {
	Executed       bool                                  `json:"executed"`
	Metadata       map[string]map[string]json.RawMessage `json:"metadata,omitempty"`
	ResultMetadata map[string]map[string]json.RawMessage `json:"resultMetadata,omitempty"`
}

type openCodeCurrentToolTime struct {
	Created   int64 `json:"created"`
	Ran       int64 `json:"ran,omitempty"`
	Completed int64 `json:"completed,omitempty"`
	Pruned    int64 `json:"pruned,omitempty"`
}

type openCodeCurrentToolState struct {
	Status      string                       `json:"status"`
	Input       json.RawMessage              `json:"input"`
	Structured  map[string]json.RawMessage   `json:"structured"`
	Content     []openCodeCurrentToolContent `json:"content"`
	Attachments []openCodeCurrentFile        `json:"attachments,omitempty"`
	OutputPaths []string                     `json:"outputPaths,omitempty"`
	Result      json.RawMessage              `json:"result,omitempty"`
	Error       *openCodeCurrentUnknownError `json:"error,omitempty"`
}

type openCodeCurrentToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URI  string `json:"uri,omitempty"`
	MIME string `json:"mime,omitempty"`
	Name string `json:"name,omitempty"`
}

type openCodeCurrentTextMessage struct {
	openCodeCurrentBase
	Text        string `json:"text"`
	Description string `json:"description,omitempty"`
}

type openCodeCurrentSynthetic struct {
	openCodeCurrentBase
	SessionID   string `json:"sessionID"`
	Text        string `json:"text"`
	Description string `json:"description,omitempty"`
}

type openCodeCurrentShell struct {
	openCodeCurrentBase
	CallID  string `json:"callID"`
	Command string `json:"command"`
	Output  string `json:"output"`
}

type openCodeCurrentCompaction struct {
	openCodeCurrentBase
	Reason  string `json:"reason"`
	Summary string `json:"summary"`
	Recent  string `json:"recent"`
}

type openCodeCurrentAgentSwitched struct {
	openCodeCurrentBase
	Agent    string `json:"agent"`
	Previous string `json:"previous,omitempty"`
}

type openCodeCurrentModelSwitched struct {
	openCodeCurrentBase
	Model    openCodeCurrentModel  `json:"model"`
	Previous *openCodeCurrentModel `json:"previous,omitempty"`
}

type openCodeCurrentIdle struct {
	openCodeCurrentBase
	Outcome string                  `json:"outcome"`
	Error   *openCodeCurrentFailure `json:"error,omitempty"`
}

// openCodeCurrentFailure is the pinned structured error an idle row carries when
// a turn fails.
type openCodeCurrentFailure struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
}

type openCodeCurrentIdentityRegistry struct {
	kinds map[string]string
}

func (registry *openCodeCurrentIdentityRegistry) add(id, kind string) error {
	if id == "" {
		return fmt.Errorf("%s identity is empty", kind)
	}
	if previous, exists := registry.kinds[id]; exists {
		return fmt.Errorf("identity %q is already registered as %s and cannot also identify %s", id, previous, kind)
	}
	registry.kinds[id] = kind
	return nil
}

// errOpenCodeSkipControlRow signals that a well-formed current row carried no
// upstream id and a type outside the known set, so it is a newer control record
// the caller skips and counts rather than a fatal decode failure.
var errOpenCodeSkipControlRow = errors.New("skip newer OpenCode control row")

// errOpenCodeIgnoredControlRow signals an entryless control row of a kind this
// build interprets. It contributes no transcript content, so the caller skips
// it without marking the projection partial: an interpreted marker is accounted
// for, not omitted.
var errOpenCodeIgnoredControlRow = errors.New("ignore interpreted OpenCode control row")

// OpenCodeControlRowIgnored reports whether err is the sentinel an entryless
// control row of a kind this build interprets returns from the provenance decode
// boundary. The row was read successfully and lost nothing, so a caller that
// counts decode refusals must not count it. A skipped newer control row returns
// a different sentinel and is an omission, not an interpretation.
func OpenCodeControlRowIgnored(err error) bool {
	return errors.Is(err, errOpenCodeIgnoredControlRow)
}

// Native objects may add fields; managed projection envelopes use their separate
// strict whitelist decoder. Known field types and variant invariants stay checked.
func decodeOpenCodeCurrentJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("expected exactly one JSON value: %w", err)
	}
	return nil
}

func (a *OpenCodeAdapter) discoverCurrentSQLite(ctx context.Context, source OpenCodeSQLiteSource, candidate OpenCodeCandidate) (discovered []openCodeSessionSource, err error) {
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return nil, err
	}
	var cursor *OpenCodeCurrentSessionCursor
	for {
		page, readErr := source.CurrentSessionIDs(ctx, OpenCodeCurrentSessionPageRequest{PageSize: pageSize, After: cursor})
		if readErr != nil {
			return nil, fmt.Errorf("discover current OpenCode SQLite candidate %q failed while enumerating a bounded session page: %w; no partial discovery result is eligible; verify the database remains a supported current session_message store and retry", candidate.Path, readErr)
		}
		for _, currentID := range page.SessionIDs {
			sessionID, idErr := NewSessionID(currentID.String())
			if idErr != nil {
				return nil, fmt.Errorf("discover current OpenCode SQLite candidate %q found session identifier %q that Peasant cannot store: %w; no partial discovery result is eligible; repair the upstream identifier or use a supported export", candidate.Path, currentID.String(), idErr)
			}
			discovered = append(discovered, openCodeSQLiteSessionSource(candidate, sessionID, TranscriptOriginOpenCodeCurrentSQLite))
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	return discovered, nil
}

// openCodeSQLiteSessionSource is one enumerated SQLite session before the
// shared build names it. Both projections build it, so the file, the root and
// the format of a database-backed session are stated once.
func openCodeSQLiteSessionSource(candidate OpenCodeCandidate, id SessionID, origin TranscriptOrigin) openCodeSessionSource {
	return openCodeSessionSource{
		ID:     id,
		Path:   ResolvedPath(candidate.Path),
		Root:   ResolvedPath(filepath.Dir(candidate.Path)),
		Format: SourceFormatJSON,
		Origin: origin,
	}
}

// currentControlOnlySessions probes each discovered current session for a
// substantive row and returns the set that holds only control records. It uses
// the enumeration source already open for the candidate, so no second database
// is opened, and each probe is a single bounded existence read keyed on the
// indexed session_id column rather than a scan. A probe failure fails safe: the
// session is treated as substantive so a transient read error never demotes a
// real current conversation to its legacy sibling, and one diagnostic names the
// affected session.
func (a *OpenCodeAdapter) currentControlOnlySessions(ctx context.Context, source OpenCodeSQLiteSource, candidate OpenCodeCandidate, sessions []openCodeSessionSource) map[SessionID]bool {
	controlOnly := make(map[SessionID]bool, len(sessions))
	for _, session := range sessions {
		currentID, err := NewOpenCodeCurrentSessionID(string(session.ID))
		if err != nil {
			// Enumeration already validated the identifier, so this is
			// unreachable; treat it as substantive rather than demote it.
			continue
		}
		hasSubstantive, probeErr := source.CurrentSessionHasSubstantive(ctx, currentID)
		if probeErr != nil {
			a.recordCandidateFailure(candidate.Path, OpenCodeProbeDiscover, fmt.Sprintf("substantive-row probe failed for current session %q; the session stays a current winner rather than deferring to a legacy sibling", session.ID), probeErr)
			continue
		}
		if !hasSubstantive {
			controlOnly[session.ID] = true
		}
	}
	return controlOnly
}

// finishCurrentManagedProjection encodes a read current projection into the
// managed JSON bytes and derives its metadata. The full-session and preview
// prefix reads share it. A split part this build could not decode is an
// accounted omission: it is reported as a warning and the projection itself
// carries content_omitted so no capture is certified complete.
func (a *OpenCodeAdapter) finishCurrentManagedProjection(ctx context.Context, session DiscoveredSession, projection openCodeCurrentProjection, unknownControlTypes map[string]int, partOmissions []openCodeCurrentPartOmission) (*UnifiedMetadata, []byte, error) {
	if len(projection.Messages) == 0 {
		return nil, nil, fmt.Errorf("materialize current OpenCode SQLite session %q from %q produced no semantic messages even though discovery enumerated it; no empty managed artifact was written; retry after OpenCode finishes its transaction or remove the stale source row", session.SessionID, session.SourcePath)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return nil, nil, fmt.Errorf("materialize current OpenCode SQLite session %q failed while encoding the deterministic managed JSON projection: %w; detached source rows remain unchanged and no managed state was written; report the unsupported row shape", session.SessionID, err)
	}
	data = append(data, '\n')
	managed := openCodeLegacyProjection{Format: projection.Format, Version: projection.Version, SessionID: projection.SessionID, Messages: projection.Messages}
	metadata, err := a.metadataFromManagedProjection(ctx, session, managed)
	if err != nil {
		return nil, nil, err
	}
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, openCodeUnknownTypeDiagnostics(session, unknownControlTypes, "control message row")...)
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, openCodeCurrentPartOmissionDiagnostics(session, partOmissions)...)
	return metadata, data, nil
}

// openCodeCurrentPartOmissionDiagnostics turns the split parts a current read
// could not carry into one warning per part type. It uses the existing
// OpenCodeUnknownPartType code, which the capture assessment already treats as
// an accounted omission, so the session is stored partial instead of certified
// complete. The reason of the first row of each type is named so a reader can
// act on it.
func openCodeCurrentPartOmissionDiagnostics(session DiscoveredSession, omissions []openCodeCurrentPartOmission) []DiagnosticEntry {
	if len(omissions) == 0 {
		return nil
	}
	type omissionSummary struct {
		count  int
		reason string
	}
	byType := make(map[string]omissionSummary)
	for _, omission := range omissions {
		partType := omission.Type
		if partType == "" {
			partType = "unknown"
		}
		summary := byType[partType]
		summary.count++
		if summary.reason == "" {
			summary.reason = omission.Reason
		}
		byType[partType] = summary
	}
	types := make([]string, 0, len(byType))
	for partType := range byType {
		types = append(types, partType)
	}
	sort.Strings(types)
	diagnostics := make([]DiagnosticEntry, 0, len(types))
	for _, partType := range types {
		summary := byType[partType]
		diagnostics = append(diagnostics, DiagnosticEntry{
			ErrorType:   string(OpenCodeUnknownPartType),
			Location:    fmt.Sprintf("selected OpenCode session %s from %s", session.SessionID, session.SourcePath),
			Message:     fmt.Sprintf("%d split assistant part row(s) of type %q could not be decoded and were omitted: %s; the rest of the message remains readable, but this capture is not complete", summary.count, partType, summary.reason),
			Remediation: "Upgrade Peasant when it supports this part shape, or export the session through OpenCode; do not edit the OpenCode database with Peasant.",
		})
	}
	return diagnostics
}

func (a *OpenCodeAdapter) materializeCurrentTranscriptBounded(ctx context.Context, session DiscoveredSession, budgetBytes int64) (*UnifiedMetadata, []byte, MaterializeTruncation, error) {
	currentID, err := NewOpenCodeCurrentSessionID(string(session.SessionID))
	if err != nil {
		return nil, nil, MaterializeTruncation{}, err
	}
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return nil, nil, MaterializeTruncation{}, err
	}
	var projection openCodeCurrentProjection
	var unknownControlTypes map[string]int
	var partOmissions []openCodeCurrentPartOmission
	var truncation MaterializeTruncation
	if err := a.withOpenCodeSQLiteSource(ctx, session.SourcePath.String(), func(source OpenCodeSQLiteSource) error {
		size, sizeErr := source.CurrentSessionPayloadSize(ctx, currentID)
		if sizeErr != nil {
			return sizeErr
		}
		budget := int64(0)
		if budgetBytes > 0 && size.Bytes > budgetBytes {
			budget = budgetBytes
		}
		var readErr error
		projection, unknownControlTypes, partOmissions, truncation, readErr = readOpenCodeCurrentProjectionCoreWithOmissions(ctx, source, currentID, pageSize, budget, size)
		return readErr
	}); err != nil {
		return nil, nil, MaterializeTruncation{}, fmt.Errorf("materialize bounded current OpenCode SQLite session %q failed while reading selected session_message rows and closing the bounded source: %w; no partial managed artifact or store row was written; fix malformed current rows in OpenCode and retry", session.SessionID, err)
	}
	metadata, data, err := a.finishCurrentManagedProjection(ctx, session, projection, unknownControlTypes, partOmissions)
	if err != nil {
		return nil, nil, MaterializeTruncation{}, err
	}
	return metadata, data, truncation, nil
}

func (a *OpenCodeAdapter) materializeCurrentTranscriptFirstPage(ctx context.Context, session DiscoveredSession, budgetBytes int64) (*UnifiedMetadata, []byte, bool, error) {
	currentID, err := NewOpenCodeCurrentSessionID(string(session.SessionID))
	if err != nil {
		return nil, nil, false, err
	}
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return nil, nil, false, err
	}
	if budgetBytes <= 0 {
		return nil, nil, false, fmt.Errorf("materialize the first page of current OpenCode SQLite session %q failed before source access: the byte budget %d is not positive; no managed state was written; pass the preview first-page budget", session.SessionID, budgetBytes)
	}
	var projection openCodeCurrentProjection
	var unknownControlTypes map[string]int
	var partOmissions []openCodeCurrentPartOmission
	var truncation MaterializeTruncation
	if err := a.withOpenCodeSQLiteSource(ctx, session.SourcePath.String(), func(source OpenCodeSQLiteSource) error {
		// The zero payload size is deliberate: it fills only the totals a
		// truncation note quotes, and the first-page read writes no note.
		var readErr error
		projection, unknownControlTypes, partOmissions, truncation, readErr = readOpenCodeCurrentProjectionCoreWithOmissions(ctx, source, currentID, pageSize, budgetBytes, OpenCodePayloadSize{})
		return readErr
	}); err != nil {
		return nil, nil, false, fmt.Errorf("materialize the first page of current OpenCode SQLite session %q failed while reading selected session_message rows and closing the bounded source: %w; no partial managed artifact or store row was written; fix malformed current rows in OpenCode and retry", session.SessionID, err)
	}
	metadata, data, err := a.finishCurrentManagedProjection(ctx, session, projection, unknownControlTypes, partOmissions)
	if err != nil {
		return nil, nil, false, err
	}
	return metadata, data, truncation.Truncated, nil
}

func readOpenCodeCurrentProjection(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeCurrentSessionID, pageSize OpenCodeCurrentPageSize) (openCodeCurrentProjection, map[string]int, error) {
	projection, unknownControlTypes, _, err := readOpenCodeCurrentProjectionCore(ctx, source, sessionID, pageSize, 0, OpenCodePayloadSize{})
	return projection, unknownControlTypes, err
}

// readOpenCodeCurrentProjectionCore reads the session's current message rows in
// seq order and normalizes each. When budget is positive it stops accumulating
// once the summed message payload byte length reaches the budget and reports the
// truncation; a non-positive budget reads the whole session and reports no
// truncation. size carries the whole-session totals so a truncated result can
// name how much it left out. The full-session read and the preview prefix read
// share this one path.
func readOpenCodeCurrentProjectionCore(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeCurrentSessionID, pageSize OpenCodeCurrentPageSize, budget int64, size OpenCodePayloadSize) (openCodeCurrentProjection, map[string]int, MaterializeTruncation, error) {
	projection, unknownControlTypes, _, truncation, _, err := readOpenCodeCurrentProjectionSliceWithOmissions(ctx, source, sessionID, pageSize, budget, size, nil, openCodeRowCeiling{})
	return projection, unknownControlTypes, truncation, err
}

// readOpenCodeCurrentProjectionCoreWithOmissions is the omission-aware core read.
// The plain form stays for existing callers that only need the projection; the
// production paths use this one so a split part this build cannot decode is
// reported and the capture is never certified complete.
func readOpenCodeCurrentProjectionCoreWithOmissions(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeCurrentSessionID, pageSize OpenCodeCurrentPageSize, budget int64, size OpenCodePayloadSize) (openCodeCurrentProjection, map[string]int, []openCodeCurrentPartOmission, MaterializeTruncation, error) {
	projection, unknownControlTypes, omissions, truncation, _, err := readOpenCodeCurrentProjectionSliceWithOmissions(ctx, source, sessionID, pageSize, budget, size, nil, openCodeRowCeiling{})
	return projection, unknownControlTypes, omissions, truncation, err
}

// readOpenCodeCurrentProjectionSlice is the resumable form of the core read: it
// starts after the given cursor and returns the cursor of the last row it took,
// so a later call continues exactly where this one stopped.
//
// A current message row IS one message, parts nested inside it, and the budget
// is checked BEFORE a row is taken. A slice boundary therefore always lands on
// a message boundary, so no message is ever split across two slices and
// appending one slice's turns to the previous slice's can never duplicate or
// halve a turn.
func readOpenCodeCurrentProjectionSlice(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeCurrentSessionID, pageSize OpenCodeCurrentPageSize, budget int64, size OpenCodePayloadSize, after *OpenCodeCurrentCursor, ceiling openCodeRowCeiling) (openCodeCurrentProjection, map[string]int, MaterializeTruncation, openCodeCurrentSliceStop, error) {
	projection, unknownControlTypes, _, truncation, stop, err := readOpenCodeCurrentProjectionSliceWithOmissions(ctx, source, sessionID, pageSize, budget, size, after, ceiling)
	return projection, unknownControlTypes, truncation, stop, err
}

// openCodeCurrentPartOmission records one split assistant part this build could
// not decode. Type is the part's type when the row named one, and Reason
// explains the refusal. A part is omitted, never silently dropped: the caller
// marks the projection partial and records a diagnostic.
type openCodeCurrentPartOmission struct {
	Type   string
	Reason string
}

// readOpenCodeCurrentProjectionSliceWithOmissions is readOpenCodeCurrentProjectionSlice
// with the split-part omissions the read found.
func readOpenCodeCurrentProjectionSliceWithOmissions(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeCurrentSessionID, pageSize OpenCodeCurrentPageSize, budget int64, size OpenCodePayloadSize, after *OpenCodeCurrentCursor, ceiling openCodeRowCeiling) (openCodeCurrentProjection, map[string]int, []openCodeCurrentPartOmission, MaterializeTruncation, openCodeCurrentSliceStop, error) {
	projection := openCodeCurrentProjection{Format: openCodeCurrentProjectionFormat, Version: openCodeCurrentProjectionVersion, SessionID: sessionID.String(), Messages: []openCodeLegacyProjectionMessage{}}
	registry := openCodeCurrentIdentityRegistry{kinds: make(map[string]string)}
	unknownControlTypes := make(map[string]int)
	var partOmissions []openCodeCurrentPartOmission
	cursor := after
	// stop is the position of the last row this read took, which is what a
	// continuation resumes from. It is distinct from cursor, which is the page
	// cursor and runs ahead of it whenever a page is only partly consumed.
	stop := after
	// readBytes is what the read pulled out of the source and is what the budget
	// governs. includedRows and includedBytes are what reached the projection: a
	// control row is read and paid for but never shown.
	var readBytes, includedBytes, includedRows int64
	var traversalPosition int64
	truncated := false
	var readRows int64
rowLoop:
	for {
		request := OpenCodeCurrentPageRequest{SessionID: sessionID, PageSize: pageSize, After: cursor}
		// stopBefore is set when the row-length probe found a row this read may
		// not hold: the page is fetched only up to it, so the row is never read.
		stopBefore := false
		if ceiling.bytes > 0 {
			if lengths, ok := source.(OpenCodeCurrentRowLengthSource); ok {
				probed, err := lengths.CurrentMessageLengths(ctx, request)
				if err != nil {
					return openCodeCurrentProjection{}, nil, nil, MaterializeTruncation{}, openCodeCurrentSliceStop{}, err
				}
				tentative := readBytes
				for i, length := range probed {
					if budget > 0 && tentative >= budget {
						break
					}
					if length.Bytes > ceiling.bytes-tentative {
						if tentative == 0 {
							return openCodeCurrentProjection{}, nil, nil, MaterializeTruncation{}, openCodeCurrentSliceStop{}, &RecordExceedsBudgetError{Row: ceiling.rowsBefore + readRows + int64(i) + 1, AtLeastBytes: length.Bytes, CeilingBytes: ceiling.bytes}
						}
						before := length.Seq
						request.Before = &before
						stopBefore = true
						break
					}
					tentative += length.Bytes
				}
			}
		}
		page, err := source.CurrentMessages(ctx, request)
		if err != nil {
			return openCodeCurrentProjection{}, nil, nil, MaterializeTruncation{}, openCodeCurrentSliceStop{}, err
		}
		for _, drop := range page.DroppedParts {
			// The source read the row but could not hand it to the projection.
			// Its type is not trustworthy, so the omission is named generically
			// with the source's reason.
			projection.ContentOmitted = true
			partOmissions = append(partOmissions, openCodeCurrentPartOmission{Type: "session_part", Reason: drop.Reason})
		}
		for _, row := range page.Messages {
			// Every read row, control or substantive, counts toward the budget: the
			// budget bounds how much source payload the read pulls into memory, not
			// how many rows survive normalization. The budget is checked BEFORE the
			// row is taken, so a read that spent its budget exactly on the last row
			// of the session reports no truncation: nothing was left out.
			rowBytes := openCodeCurrentRowPayloadBytes(row)
			if budget > 0 && readBytes >= budget {
				truncated = true
				break rowLoop
			}
			if ceiling.bytes > 0 && rowBytes > ceiling.bytes-readBytes {
				// A source without the row-length probe: the row is already in
				// memory, but it is still never taken into the projection.
				if readBytes == 0 {
					return openCodeCurrentProjection{}, nil, nil, MaterializeTruncation{}, openCodeCurrentSliceStop{}, &RecordExceedsBudgetError{Row: ceiling.rowsBefore + readRows + 1, AtLeastBytes: rowBytes, CeilingBytes: ceiling.bytes}
				}
				truncated = true
				break rowLoop
			}
			readBytes += rowBytes
			readRows++
			taken := NewOpenCodeCurrentCursor(row.Seq)
			stop = &taken
			if err := registry.add(row.ID.String(), "message row"); err != nil {
				return openCodeCurrentProjection{}, nil, nil, MaterializeTruncation{}, openCodeCurrentSliceStop{}, currentNormalizationError(row, "registering stable identities", err)
			}
			position := openCodeUnknownPosition(sessionID.String(), "current", row.ID.String(), includedRows, traversalPosition, "")
			if seq := row.Seq.Value(); seq > 0 {
				position.Sequence = int(seq)
			}
			traversal := decodeOpenCodeCurrentTraversal([]byte(row.Data), true)
			traversalPosition += traversal.size
			var rowOmissions []openCodeCurrentPartOmission
			message, err := normalizeOpenCodeCurrentRowWithTraversal(row, &registry, position, &traversal, &rowOmissions)
			if errors.Is(err, errOpenCodeSkipControlRow) {
				// Compatibility may omit future vocabulary, but the retained
				// projection must never certify those missing rows as complete.
				projection.ContentOmitted = true
				// A newer id-less control record: keep the session, drop the row,
				// and count its type for one diagnostic per type.
				unknownControlTypes[row.Type.String()]++
				continue
			}
			if errors.Is(err, errOpenCodeIgnoredControlRow) {
				// An interpreted entryless marker: nothing to show, nothing lost.
				continue
			}
			if err != nil {
				return openCodeCurrentProjection{}, nil, nil, MaterializeTruncation{}, openCodeCurrentSliceStop{}, currentNormalizationError(row, "decoding the pinned SessionMessage shape", err)
			}
			if len(rowOmissions) > 0 {
				// A split part this build cannot decode is an accounted omission:
				// the rest of the message stays readable, but the capture is not
				// complete and the caller must not certify it.
				projection.ContentOmitted = true
				partOmissions = append(partOmissions, rowOmissions...)
			}
			for _, evidence := range message.RetainedUnknown {
				unknownControlTypes[evidence.Kind]++
			}
			projection.Messages = append(projection.Messages, message)
			includedRows++
			includedBytes += rowBytes
		}
		if stopBefore {
			// The page ended just before a row this read may not hold, so the
			// slice stops there and the next slice meets that row first.
			truncated = true
			break
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	truncation := MaterializeTruncation{}
	if truncated {
		truncation = MaterializeTruncation{Truncated: true, Unit: MaterializeUnitMessages, BudgetBytes: budget, IncludedBytes: includedBytes, TotalBytes: size.Bytes, IncludedRows: includedRows, TotalRows: size.Rows}
	}
	return projection, unknownControlTypes, partOmissions, truncation, openCodeCurrentSliceStop{cursor: stop, exhausted: !truncated, includedBytes: includedBytes, includedRows: includedRows}, nil
}

// openCodeCurrentRowPayloadBytes is the source payload one message row costs.
// A split 2.4.x assistant message carries its content in part rows, so the
// preview budget and its truncation totals include those bytes.
func openCodeCurrentRowPayloadBytes(row OpenCodeCurrentMessageRow) int64 {
	total := int64(len(row.Data))
	for _, part := range row.Parts {
		total += int64(len(part.Data))
	}
	return total
}

// openCodeCurrentMessageDataWithSplitParts returns a message row's data with
// its session_part rows moved into content in OpenCode's hydration order: the
// parts in ordinal order, then any inline content an older build appended.
// A row without parts is returned byte-for-byte unchanged. Readers that decode
// the whole row as one document (the provenance snapshot) use this; the
// projection feeds the parts through the same part decoder instead so it can
// report an omission without failing the rest of the message.
func openCodeCurrentMessageDataWithSplitParts(row OpenCodeCurrentMessageRow) (string, error) {
	if len(row.Parts) == 0 {
		return row.Data, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(row.Data), &envelope); err != nil {
		return "", fmt.Errorf("decode current message %q before merging split parts: %w", row.ID.String(), err)
	}
	var inline []json.RawMessage
	if rawContent, present := envelope["content"]; present {
		if err := json.Unmarshal(rawContent, &inline); err != nil {
			// A non-array content value is replaced by the parts, matching
			// OpenCode's hydration rule.
			inline = nil
		}
	}
	content := make([]json.RawMessage, 0, len(row.Parts)+len(inline))
	for _, part := range row.Parts {
		content = append(content, json.RawMessage(part.Data))
	}
	content = append(content, inline...)
	encoded, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("encode current message %q merged split parts: %w", row.ID.String(), err)
	}
	envelope["content"] = encoded
	merged, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("encode current message %q with merged split parts: %w", row.ID.String(), err)
	}
	return string(merged), nil
}

// openCodeRowCeiling is the largest row one slice read may hold, and how many
// rows earlier slices read, so a refused row can be named by its ordinal. The
// zero value adds no ceiling.
type openCodeRowCeiling struct {
	bytes      int64
	rowsBefore int64
}

// OpenCodeCurrentRowLength is the size of one current row's payload, read
// without the payload itself.
type OpenCodeCurrentRowLength struct {
	Seq   OpenCodeCurrentSeq
	Bytes int64
}

// OpenCodeCurrentRowLengthSource is the OPTIONAL capability of a source that can
// report the payload length of the rows a page request would return, without
// reading the payloads. A bounded preview asks it before each page, so a row
// larger than the preview may hold is refused before it is allocated.
type OpenCodeCurrentRowLengthSource interface {
	CurrentMessageLengths(ctx context.Context, request OpenCodeCurrentPageRequest) ([]OpenCodeCurrentRowLength, error)
}

// OpenCodeCurrentChangeMarker summarizes the current rows of one session at or
// before a sequence: how many there are, the latest update time among them and
// their summed payload length. Two equal markers mean no row a preview already
// read was added, removed, edited or resized in between.
type OpenCodeCurrentChangeMarker struct {
	Rows       int64
	MaxUpdated int64
	Bytes      int64
}

// OpenCodeCurrentChangeMarkerSource is the OPTIONAL capability of a source that
// can compute an [OpenCodeCurrentChangeMarker] in one bounded aggregate.
type OpenCodeCurrentChangeMarkerSource interface {
	CurrentMessagesMarker(ctx context.Context, sessionID OpenCodeCurrentSessionID, through OpenCodeCurrentSeq) (OpenCodeCurrentChangeMarker, error)
}

// openCodeCurrentSliceStop is where a resumable current read ended. exhausted
// reports that the read walked off the end of the session, and the included
// counters are what THIS slice put into the projection - reported apart from
// the truncation record, which the last slice of a session never carries.
type openCodeCurrentSliceStop struct {
	cursor        *OpenCodeCurrentCursor
	exhausted     bool
	includedBytes int64
	includedRows  int64
}

func currentNormalizationError(row OpenCodeCurrentMessageRow, operation string, cause error) error {
	return fmt.Errorf("normalize current OpenCode session_message row %q type %q failed while %s: %w; the row does not satisfy the pinned upstream SessionMessage materialized schema, so no projection or caller-visible partial state was emitted; repair the upstream row or upgrade Peasant for a newly supported schema and retry", row.ID.String(), row.Type.String(), operation, cause)
}

func normalizeOpenCodeCurrentRow(row OpenCodeCurrentMessageRow, registry *openCodeCurrentIdentityRegistry) (openCodeLegacyProjectionMessage, error) {
	return normalizeOpenCodeCurrentRowAt(row, registry, UnknownSourcePosition{SourceID: row.ID.String()})
}

func normalizeOpenCodeCurrentRowAt(row OpenCodeCurrentMessageRow, registry *openCodeCurrentIdentityRegistry, position UnknownSourcePosition) (openCodeLegacyProjectionMessage, error) {
	return normalizeOpenCodeCurrentRowWithTraversal(row, registry, position, nil, nil)
}

// normalizeOpenCodeCurrentRowWithTraversal normalizes one current row. When
// splitParts names a slice, a session_part row the projector cannot decode is
// appended there and the rest of the message stays readable; when it is nil,
// the same failure is returned, preserving the strict direct-call behavior.
func normalizeOpenCodeCurrentRowWithTraversal(row OpenCodeCurrentMessageRow, registry *openCodeCurrentIdentityRegistry, position UnknownSourcePosition, traversal *openCodeCurrentTraversal, splitParts *[]openCodeCurrentPartOmission) (openCodeLegacyProjectionMessage, error) {
	message := openCodeLegacyProjectionMessage{ID: row.ID.String(), SessionID: row.SessionID.String(), TimeCreated: row.TimeCreated, TimeUpdated: row.TimeUpdated, Parts: []openCodeLegacyProjectionPart{}}
	appendPart := func(id string, created int64, data any) error {
		if id != "" {
			if err := registry.add(id, "nested message content"); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		message.Parts = append(message.Parts, openCodeLegacyProjectionPart{ID: id, MessageID: message.ID, SessionID: message.SessionID, TimeCreated: created, TimeUpdated: created, Data: raw})
		return nil
	}
	messageData := func(role, text, model, agent, parentID string, metadata map[string]json.RawMessage, time openCodeCurrentTime, tokens *openCodeCurrentTokens) error {
		cwd, err := currentMetadataString(metadata, "cwd")
		if err != nil {
			return err
		}
		value := map[string]any{"id": message.ID, "sessionID": message.SessionID, "role": role, "time": time}
		if text != "" {
			value["content"] = text
		}
		if model != "" {
			value["modelID"] = model
		}
		if agent != "" {
			value["agent"] = agent
		}
		if parentID != "" {
			value["parentID"] = parentID
		}
		if cwd != "" {
			value["cwd"] = cwd
		}
		if tokens != nil {
			value["tokens"] = map[string]any{"input": tokens.Input, "output": tokens.Output, "reasoning": tokens.Reasoning, "cache": tokens.Cache}
		}
		raw, err := json.Marshal(value)
		message.Data = raw
		return err
	}
	data := []byte(row.Data)
	// SQLite owns the message identity and discriminator. Payloads may omit
	// both, but an explicit value must never contradict the enclosing row.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return message, err
	}
	if raw, exists := envelope["id"]; exists {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return message, err
		}
		if id != message.ID {
			return message, fmt.Errorf("upstream message id %q conflicts with SQLite row id %q", id, message.ID)
		}
	}
	if raw, exists := envelope["type"]; exists {
		var rowType string
		if err := json.Unmarshal(raw, &rowType); err != nil {
			return message, err
		}
		if rowType != row.Type.String() {
			return message, fmt.Errorf("upstream message type %q conflicts with SQLite row type %q", rowType, row.Type.String())
		}
	}
	prepared, unknown, prepareErr := prepareOpenCodeCurrentFieldsWithTraversal(row.Type.String(), data, envelope, position, traversal)
	if prepareErr != nil {
		return message, prepareErr
	}
	message.RetainedUnknown = unknown
	if prepared == nil {
		message.Data = json.RawMessage(`{"role":"system"}`)
		return message, nil
	}
	data = prepared
	var err error
	data, err = normalizeOpenCodeV2StructuralRow(row, data, envelope)
	if err != nil {
		return message, err
	}
	switch row.Type.String() {
	case "user":
		var value openCodeCurrentUser
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentObjectFields(envelope, "text", "time"); err != nil {
			return message, err
		}
		if err := messageData(RoleUser.String(), value.Text, "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
		for _, agent := range value.Agents {
			if agent.Name == "" {
				return message, errors.New("user agent attachment requires name")
			}
			if err := appendPart("", value.Time.Created, map[string]any{"type": "agent", "name": agent.Name, "text": agent.Name}); err != nil {
				return message, err
			}
		}
		for _, raw := range value.Files {
			file, err := decodeOpenCodeV2File(raw)
			if err != nil {
				return message, err
			}
			text := file.Name
			if text == "" {
				text = file.URI
			}
			if text == "" {
				text = file.Description
			}
			if text != "" {
				if err := appendPart("", value.Time.Created, map[string]any{"type": "text", "text": text}); err != nil {
					return message, err
				}
			}
		}
		for _, skill := range value.Skills {
			if skill.ID == "" || skill.Name == "" {
				return message, errors.New("user skill attachment requires id and name")
			}
			text := skill.Text
			if text == "" {
				text = skill.Name
			}
			if err := appendPart("", value.Time.Created, map[string]any{"type": "text", "text": text}); err != nil {
				return message, err
			}
		}
	case "assistant":
		var value openCodeCurrentAssistant
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentObjectFields(envelope, "agent", "model", "content", "time"); err != nil {
			return message, err
		}
		if err := validateOpenCodeV2Error(value.Error); err != nil {
			return message, err
		}
		if value.Retry != nil {
			if err := requireOpenCodeCurrentFields(envelope["retry"], "attempt", "at", "error"); err != nil {
				return message, err
			}
			if value.Retry.Attempt <= 0 || value.Retry.Error == nil {
				return message, errors.New("assistant retry requires positive attempt and error")
			}
			if err := validateOpenCodeV2Error(value.Retry.Error); err != nil {
				return message, err
			}
		}
		if err := messageData(RoleAssistant.String(), "", value.Model.ID, value.Agent, value.ParentID, value.Metadata, value.Time.openCodeCurrentTime, value.Tokens); err != nil {
			return message, err
		}
		// Preserve native variant evidence for metadata replay and the retained
		// indexer. A model without a variant contributes no level.
		if value.Model.Variant != "" {
			var normalized map[string]json.RawMessage
			if err := json.Unmarshal(message.Data, &normalized); err != nil {
				return message, err
			}
			normalized["model"], _ = json.Marshal(value.Model)
			message.Data, err = json.Marshal(normalized)
			if err != nil {
				return message, err
			}
		}
		// OpenCode 2.4.x writes the assistant parts as their own rows and
		// hydrates them before any inline content an older build left on the
		// message. Feed them in ordinal order first, then the inline content, so
		// both storage shapes decode through the same projection.
		for _, part := range row.Parts {
			if err := appendOpenCodeCurrentAssistantPart(&message, part, registry, appendPart); err != nil {
				if splitParts == nil {
					return message, err
				}
				*splitParts = append(*splitParts, openCodeCurrentPartOmission{Type: part.Type.String(), Reason: err.Error()})
			}
		}
		for _, content := range value.Content {
			if err := appendOpenCodeCurrentAssistantContent(&message, content, registry, appendPart); err != nil {
				return message, err
			}
		}
	case "shell":
		var value openCodeCurrentShell
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentFields(data, "callID", "command", "output", "time"); err != nil {
			return message, err
		}
		if envelope["shellID"] != nil {
			if err := messageData(RoleAssistant.String(), "", "", "", "", value.Metadata, value.Time, nil); err != nil {
				return message, err
			}
			state, err := normalizeOpenCodeV2ShellState(value, envelope)
			if err != nil {
				return message, err
			}
			if err := appendPart(value.CallID, value.Time.Created, map[string]any{"id": value.CallID, "type": "tool", "name": "shell", "state": state, "time": value.Time}); err != nil {
				return message, err
			}
			break
		}
		if err := registry.add(value.CallID, "shell tool call"); err != nil {
			return message, err
		}
		if err := messageData(RoleAssistant.String(), "", "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
		if err := appendPart("", value.Time.Created, map[string]any{"id": value.CallID, "type": "tool_use", "name": "shell", "input": map[string]any{"command": value.Command}, "time": map[string]any{"created": value.Time.Created}}); err != nil {
			return message, err
		}
		if value.Time.Completed > 0 || value.Output != "" {
			if err := appendPart("", value.Time.Completed, map[string]any{"id": value.CallID, "type": "tool_result", "output": value.Output, "time": map[string]any{"created": value.Time.Completed}}); err != nil {
				return message, err
			}
		}
	case "synthetic":
		var value openCodeCurrentSynthetic
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentObjectFields(envelope, "text", "time"); err != nil {
			return message, err
		}
		if raw, present := envelope["sessionID"]; present && (bytes.Equal(raw, []byte("null")) || value.SessionID != message.SessionID) {
			return message, errors.New("synthetic sessionID must match SQLite row session")
		}
		if err := messageData(RoleSystem.String(), value.Text, "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
	case "system":
		var value openCodeCurrentTextMessage
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentObjectFields(envelope, "text", "time"); err != nil {
			return message, err
		}
		if err := messageData(RoleSystem.String(), value.Text, "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
	case "skill":
		var value openCodeV2SkillMessage
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentObjectFields(envelope, "skill", "name", "text", "time"); err != nil {
			return message, err
		}
		if value.Skill == "" || value.Name == "" {
			return message, errors.New("native skill message requires skill and name")
		}
		if err := messageData(RoleSystem.String(), value.Text, "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
	case "compaction":
		var value openCodeCurrentCompaction
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentFields(data, "reason", "summary", "recent", "time"); err != nil {
			return message, err
		}
		if value.Reason != "auto" && value.Reason != "manual" {
			return message, errors.New("compaction reason must be auto or manual")
		}
		if err := messageData(RoleSystem.String(), value.Summary, "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
		// A failed native compaction contributes its actual error as system
		// context, but must not acquire a successful compaction marker.
		if envelope["error"] != nil {
			break
		}
		if err := appendPart("", value.Time.Created, map[string]any{"type": "compaction", "text": value.Summary, "content": value.Recent, "time": map[string]any{"created": value.Time.Created}}); err != nil {
			return message, err
		}
	case "agent-switched":
		// A control record legitimately carries no upstream id, so absence is
		// tolerated rather than fatal. It renders as a system record naming the
		// new agent, which is inert transcript context, not a user or assistant
		// turn.
		var value openCodeCurrentAgentSwitched
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentObjectFields(envelope, "agent", "time"); err != nil {
			return message, err
		}
		message.Control = "agent-switched"
		if err := messageData(RoleSystem.String(), value.Agent, "", value.Agent, "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
		if err := appendPart("", value.Time.Created, map[string]any{"type": "agent", "name": value.Agent, "text": value.Agent}); err != nil {
			return message, err
		}
	case "model-switched":
		// A control record legitimately carries no upstream id, so absence is
		// tolerated rather than fatal. A model switch is the same signal the
		// indexer records as a per-turn model observation, so it carries the
		// model id and the indexer applies it to the following assistant turn.
		var value openCodeCurrentModelSwitched
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentObjectFields(envelope, "model", "time"); err != nil {
			return message, err
		}
		message.Control = "model-switched"
		if err := messageData(RoleSystem.String(), value.Model.ID, value.Model.ID, "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
		if err := appendPart("", value.Time.Created, map[string]any{"type": "subtask", "name": value.Model.ID, "text": value.Model.ID}); err != nil {
			return message, err
		}
	case "idle":
		// An idle row records how a turn settled. A clean or interrupted settle is
		// an entryless marker. A failed settle carries the failure text, which is
		// often the only durable record of it, so it becomes a system entry
		// instead of being dropped.
		var value openCodeCurrentIdle
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentFields(data, "time", "outcome"); err != nil {
			return message, err
		}
		idleText, ignored, err := openCodeIdleDisposition(value.Outcome, value.Error)
		if err != nil {
			return message, err
		}
		if ignored {
			return message, errOpenCodeIgnoredControlRow
		}
		if err := messageData(RoleSystem.String(), idleText, "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
	default:
		// A well-formed row that carries no upstream id but is control-shaped (it
		// has a time) is a newer control record: skip it and let the caller count
		// its type. A row that should carry an id, or is not control-shaped,
		// stays an error so genuinely malformed content still fails the session.
		hasID := requireOpenCodeCurrentFields(data, "id") == nil
		hasTime := requireOpenCodeCurrentFields(data, "time") == nil
		if !hasID && hasTime {
			return message, errOpenCodeSkipControlRow
		}
		return message, fmt.Errorf("type %q is outside the supported pinned SessionMessage closed set", row.Type.String())
	}
	return message, nil
}

func currentMetadataString(metadata map[string]json.RawMessage, key string) (string, error) {
	var value string
	if raw := metadata[key]; len(raw) != 0 {
		if !json.Valid(raw) {
			return "", fmt.Errorf("current metadata %q contains malformed JSON; correct the metadata value in OpenCode and retry", key)
		}
		// Metadata is an upstream open record. A valid non-string value is not a
		// CWD, but must remain harmlessly optional as it was before normalization.
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", nil
		}
	}
	return value, nil
}

func requireOpenCodeCurrentFields(raw []byte, fields ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	return requireOpenCodeCurrentObjectFields(object, fields...)
}

// The caller has already decoded this exact object. Presence and explicit null
// checks do not need another scan and copy of its potentially large payload.
func requireOpenCodeCurrentObjectFields(object map[string]json.RawMessage, fields ...string) error {
	for _, field := range fields {
		value, ok := object[field]
		if !ok {
			return fmt.Errorf("required upstream field %q is absent", field)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("required upstream field %q is null", field)
		}
	}
	return nil
}

// appendOpenCodeCurrentAssistantPart appends one session_part row to a message.
// The type column is the row's authoritative discriminator; data.type must
// agree with it, and a disagreement is malformed rather than something to
// guess at. The part's data then goes through the same decoder as inline
// assistant content, so both shapes share one interpretation.
func appendOpenCodeCurrentAssistantPart(message *openCodeLegacyProjectionMessage, part OpenCodeCurrentPartRow, registry *openCodeCurrentIdentityRegistry, appendPart func(string, int64, any) error) error {
	raw := json.RawMessage(part.Data)
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &discriminator); err != nil {
		return fmt.Errorf("decode split part %q at ordinal %d: %w", part.MessageID.String(), part.Ordinal, err)
	}
	if discriminator.Type != part.Type.String() {
		return fmt.Errorf("split part %q at ordinal %d declares type column %q but data.type %q", part.MessageID.String(), part.Ordinal, part.Type.String(), discriminator.Type)
	}
	return appendOpenCodeCurrentAssistantContent(message, raw, registry, appendPart)
}

func appendOpenCodeCurrentAssistantContent(message *openCodeLegacyProjectionMessage, raw json.RawMessage, registry *openCodeCurrentIdentityRegistry, appendPart func(string, int64, any) error) error {
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &discriminator); err != nil {
		return err
	}
	switch discriminator.Type {
	case "text":
		var value openCodeCurrentAssistantText
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return err
		}
		if err := requireOpenCodeCurrentFields(raw, "type", "text"); err != nil {
			return err
		}
		if err := validateOpenCodeCurrentContentIdentity(raw, value.ID); err != nil {
			return err
		}
		value.State = nil // Provider continuation state is not transcript content.
		return appendPart(value.ID, 0, value)
	case "reasoning":
		var value openCodeCurrentAssistantReasoning
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return err
		}
		if err := requireOpenCodeCurrentFields(raw, "type", "text"); err != nil {
			return err
		}
		if err := validateOpenCodeCurrentContentIdentity(raw, value.ID); err != nil {
			return err
		}
		created := int64(0)
		if value.Time != nil {
			created = value.Time.Created
		}
		value.State = nil
		return appendPart(value.ID, created, value)
	case "tool":
		var value openCodeCurrentAssistantTool
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return err
		}
		if err := requireOpenCodeCurrentFields(raw, "type", "id", "name", "state", "time"); err != nil {
			return err
		}
		if value.ID == "" {
			return errors.New("assistant tool requires id")
		}
		if state, native, err := decodeOpenCodeV2ToolState(value.State); native {
			if err != nil {
				return err
			}
			return appendPart(value.ID, value.Time.Created, map[string]any{"id": value.ID, "type": "tool", "name": value.Name, "state": state, "time": value.Time})
		}
		var state openCodeCurrentToolState
		if err := decodeOpenCodeCurrentJSON(value.State, &state); err != nil {
			return fmt.Errorf("decode tool %q state: %w", value.ID, err)
		}
		if state.Status != "pending" && state.Status != "running" && state.Status != "completed" && state.Status != "error" {
			return fmt.Errorf("tool %q state status %q is unsupported", value.ID, state.Status)
		}
		if len(state.Input) == 0 {
			return fmt.Errorf("tool %q state requires input", value.ID)
		}
		if state.Status == "pending" {
			var pending string
			if err := json.Unmarshal(state.Input, &pending); err != nil || state.Structured != nil || state.Content != nil || len(state.Result) != 0 || state.Error != nil {
				return fmt.Errorf("tool %q pending state requires string input", value.ID)
			}
		} else {
			var input map[string]json.RawMessage
			if err := json.Unmarshal(state.Input, &input); err != nil || state.Structured == nil || state.Content == nil {
				return fmt.Errorf("tool %q %s state requires object input", value.ID, state.Status)
			}
			for _, content := range state.Content {
				if content.Type == "text" && content.Text == "" {
					return fmt.Errorf("tool %q text content requires text", value.ID)
				}
				if content.Type == "file" && (content.URI == "" || content.MIME == "") {
					return fmt.Errorf("tool %q file content requires uri and mime", value.ID)
				}
				if content.Type != "text" && content.Type != "file" {
					return fmt.Errorf("tool %q content type %q is unsupported", value.ID, content.Type)
				}
			}
			if state.Status == "running" && (len(state.Result) != 0 || state.Error != nil || len(state.Attachments) != 0 || len(state.OutputPaths) != 0) {
				return fmt.Errorf("tool %q running state contains completed/error-only fields", value.ID)
			}
			if state.Status == "completed" && state.Error != nil {
				return fmt.Errorf("tool %q completed state contains error-only fields", value.ID)
			}
			if state.Status == "error" && (state.Error == nil || state.Error.Type != "unknown" || state.Error.Message == "") {
				return fmt.Errorf("tool %q error state requires an unknown error with message", value.ID)
			}
		}
		return appendPart(value.ID, value.Time.Created, map[string]any{"id": value.ID, "type": "tool", "name": value.Name, "state": state, "time": value.Time})
	default:
		return fmt.Errorf("assistant content type %q is outside the supported text/reasoning/tool closed set", discriminator.Type)
	}
}

func validateOpenCodeCurrentContentIdentity(raw json.RawMessage, id string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if _, explicit := fields["id"]; explicit && id == "" {
		return errors.New("assistant content explicit id is empty")
	}
	return nil
}

// openCodeIdleDisposition decides what an idle row contributes. A settled or
// interrupted turn contributes nothing, because its own marker carries no
// conversation text. A failed turn contributes its failure text: the structured
// error is the only durable record when the assistant row did not keep one.
// An outcome outside the pinned set is malformed known data, so it fails.
func openCodeIdleDisposition(outcome string, failure *openCodeCurrentFailure) (text string, ignored bool, err error) {
	switch outcome {
	case "succeeded", "interrupted":
		return "", true, nil
	case "failed":
		if failure == nil {
			return "", false, fmt.Errorf("idle row reports a failed outcome without its structured error; restore the source row and retry")
		}
		text = strings.TrimSpace(failure.Message)
		if text == "" {
			text = strings.TrimSpace(failure.Type)
		}
		if text == "" {
			return "", false, fmt.Errorf("idle row reports a failed outcome whose structured error names neither a message nor a type; restore the source row and retry")
		}
		return text, false, nil
	default:
		return "", false, fmt.Errorf("idle outcome %q is outside the pinned succeeded, failed or interrupted set; verify the source row and retry", outcome)
	}
}
