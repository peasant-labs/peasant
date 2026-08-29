package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// OpenCodeIndexer parses legacy JSON trees and Peasant-managed OpenCode projections into SessionEntry slices.
type OpenCodeIndexer struct {
	fs          FileSystem
	fullDepth   bool
	fullContent bool
}

// OpenCodeIndexerOption configures an OpenCodeIndexer.
type OpenCodeIndexerOption func(*OpenCodeIndexer)

// WithOpenCodeFullDepth enables full-depth part decomposition when true.
// When enabled, the indexer creates depth=1 entries for each content part
// in addition to the depth=0 message entries.
func WithOpenCodeFullDepth(enabled bool) OpenCodeIndexerOption {
	return func(idx *OpenCodeIndexer) { idx.fullDepth = enabled }
}

// WithOpenCodeFullContent enables full content extraction when set to true.
// When enabled, ContentPreview is populated with the complete content string
// without truncation. Used by the export pipeline to get full transcript content.
// When disabled (default), content is truncated to defaults.ContentPreviewLimit.
func WithOpenCodeFullContent(enabled bool) OpenCodeIndexerOption {
	return func(idx *OpenCodeIndexer) { idx.fullContent = enabled }
}

var _ TranscriptIndexer = (*OpenCodeIndexer)(nil)
var _ SessionTranscriptSourceResolver = (*OpenCodeIndexer)(nil)

// SourceKind reports the default legacy JSON representation. TranscriptSourceKindFor
// selects managed projection files for SQLite-backed sessions.
func (idx *OpenCodeIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceDirectory }

// TranscriptSourceKindFor selects the physical representation for one session.
// The pipeline remains provider-agnostic while OpenCode owns its two source
// shapes.
func (idx *OpenCodeIndexer) TranscriptSourceKindFor(session DiscoveredSession) TranscriptSourceKind {
	if session.TranscriptOrigin == TranscriptOriginOpenCodeLegacySQLite || session.TranscriptOrigin == TranscriptOriginOpenCodeCurrentSQLite {
		return TranscriptSourceFile
	}
	return TranscriptSourceDirectory
}

// NewOpenCodeIndexer creates an OpenCodeIndexer with an injected FileSystem.
func NewOpenCodeIndexer(fs FileSystem, opts ...OpenCodeIndexerOption) *OpenCodeIndexer {
	idx := &OpenCodeIndexer{fs: fs}
	for _, opt := range opts {
		opt(idx)
	}
	return idx
}

// IndexTranscriptBytes indexes a Peasant-managed OpenCode projection. Legacy
// JSON sessions retain the directory path because their entries remain spread
// across provider-owned message and part files.
func (idx *OpenCodeIndexer) IndexTranscriptBytes(ctx context.Context, session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	switch session.TranscriptOrigin {
	case TranscriptOriginFile:
		return idx.IndexTranscript(ctx, session)
	case TranscriptOriginOpenCodeLegacySQLite, TranscriptOriginOpenCodeCurrentSQLite:
		return idx.indexManagedProjection(session, data)
	default:
		return nil, fmt.Errorf("index OpenCode session %q failed before parsing transcript bytes: transcript origin %d is outside the supported closed set; no entry rows were stored; return TranscriptOriginFile, TranscriptOriginOpenCodeLegacySQLite, or TranscriptOriginOpenCodeCurrentSQLite from discovery", session.SessionID, session.TranscriptOrigin)
	}
}

func (idx *OpenCodeIndexer) indexManagedProjection(session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	expectedFormat, expectedVersion, err := managedOpenCodeProjectionFormat(session.TranscriptOrigin)
	if err != nil {
		return nil, err
	}
	projection, err := decodeManagedOpenCodeProjection(data, expectedFormat, expectedVersion, session.SessionID)
	if err != nil {
		return nil, fmt.Errorf("index %s OpenCode projection for session %q failed while strictly decoding the Peasant-owned envelope: %w; no entry rows were stored; re-run harvest to regenerate the projection", managedOpenCodeProjectionKind(session.TranscriptOrigin), session.SessionID, err)
	}
	kind := managedOpenCodeProjectionKind(session.TranscriptOrigin)
	messages, _, err := parseManagedOpenCodeSemanticMessages(projection, kind)
	if err != nil {
		return nil, fmt.Errorf("index %s OpenCode projection for session %q failed while decoding its normalized semantic corpus: %w; no partial entry rows were stored, so regenerate the managed artifact from a supported source and retry", kind, session.SessionID, err)
	}
	return idx.indexSemanticMessages(session.SessionID, messages), nil
}

func managedOpenCodeProjectionKind(origin TranscriptOrigin) string {
	switch origin {
	case TranscriptOriginOpenCodeCurrentSQLite:
		return "managed current SQLite"
	case TranscriptOriginOpenCodeLegacySQLite:
		return "managed legacy SQLite"
	default:
		return fmt.Sprintf("unsupported transcript origin %d", origin)
	}
}

func managedOpenCodeProjectionFormat(origin TranscriptOrigin) (string, int, error) {
	switch origin {
	case TranscriptOriginOpenCodeLegacySQLite:
		return openCodeLegacyProjectionFormat, openCodeLegacyProjectionVersion, nil
	case TranscriptOriginOpenCodeCurrentSQLite:
		return openCodeCurrentProjectionFormat, openCodeCurrentProjectionVersion, nil
	default:
		return "", 0, fmt.Errorf("index managed OpenCode projection failed before decoding transcript bytes: transcript origin %d is outside the supported managed origin set; no entry rows were stored; return a supported typed SQLite origin from discovery", origin)
	}
}

// parseManagedOpenCodeSemanticMessages decodes the shared semantic corpus. A
// part whose declared type is outside the known transcript vocabulary no longer
// fails the session: it is kept as an inert system note when it carries
// renderable text and dropped when it does not, and each distinct unknown type
// is counted so one diagnostic per type can name it. An undecodable part, or a
// message with an invalid role, still fails the session, so corruption stays
// fatal while merely newer vocabulary becomes tolerant.
func parseManagedOpenCodeSemanticMessages(projection openCodeLegacyProjection, kind string) ([]openCodeSemanticMessage, map[string]int, error) {
	messages := make([]openCodeSemanticMessage, 0, len(projection.Messages))
	unknownPartTypes := make(map[string]int)
	for _, message := range projection.Messages {
		semantic, err := parseOpenCodeSemanticMessage(message.ID, message.TimeCreated, message.Data)
		if err != nil {
			return nil, nil, fmt.Errorf("decode %s message row %q: %w", kind, message.ID, err)
		}
		if semantic.Data.Time.Completed <= 0 {
			semantic.TimeCompleted = message.TimeUpdated
		}
		if role := Role(semantic.Data.Role); !role.IsValid() {
			return nil, nil, fmt.Errorf("decode %s message row %q: role %q is outside the supported closed set", kind, message.ID, semantic.Data.Role)
		}
		semantic.Orphan = message.Orphan
		semantic.Control = message.Control
		for _, part := range message.Parts {
			semanticPart, partErr := parseOpenCodeSemanticPart(part.ID, part.TimeCreated, part.Data)
			if partErr != nil {
				return nil, nil, fmt.Errorf("decode %s part row %q for message %q: %w", kind, part.ID, message.ID, partErr)
			}
			if !isKnownOpenCodeSemanticPartType(semanticPart.Data.Type) {
				unknownPartTypes[semanticPart.Data.Type]++
				if semanticPart.Data.Text == "" {
					// No renderable text, so there is nothing to show: drop the
					// row and let the per-type diagnostic account for it.
					continue
				}
				semanticPart.UnknownType = true
			}
			semantic.Parts = append(semantic.Parts, semanticPart)
		}
		messages = append(messages, semantic)
	}
	return messages, unknownPartTypes, nil
}

func isKnownOpenCodeSemanticPartType(partType string) bool {
	switch partType {
	case "text", "reasoning", "tool", "tool_use", "tool_result", "compaction", "subtask", "agent":
		return true
	default:
		return false
	}
}

// IndexTranscript reads all msg_*.json files under the message directory for a session
// and returns SessionEntry rows ordered by filename (alphabetical ≈ chronological).
func (idx *OpenCodeIndexer) IndexTranscript(_ context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	if session.TranscriptOrigin == TranscriptOriginOpenCodeLegacySQLite || session.TranscriptOrigin == TranscriptOriginOpenCodeCurrentSQLite {
		projectionPath := session.SourcePath.String()
		info, statErr := idx.fs.Stat(projectionPath)
		if statErr != nil {
			return nil, fmt.Errorf("index %s OpenCode projection for session %q failed while sizing %q: %w; no entry rows were stored, so re-run harvest to restore the managed artifact", managedOpenCodeProjectionKind(session.TranscriptOrigin), session.SessionID, projectionPath, statErr)
		}
		if info.Size() > defaults.OpenCodeManagedProjectionMaxBytes {
			return nil, fmt.Errorf("index %s OpenCode projection for session %q refused %q because it is %d bytes, past the %d byte managed-projection bound; no entry rows were stored and the file was never read into memory; this path must hold the small per-session managed projection, so run harvest to regenerate the projection and never point the reader at the OpenCode database", managedOpenCodeProjectionKind(session.TranscriptOrigin), session.SessionID, projectionPath, info.Size(), int64(defaults.OpenCodeManagedProjectionMaxBytes))
		}
		data, err := idx.fs.ReadFile(projectionPath)
		if err != nil {
			return nil, fmt.Errorf("index %s OpenCode projection for session %q failed while reading %q: %w; no entry rows were stored, so re-run harvest to restore the managed artifact", managedOpenCodeProjectionKind(session.TranscriptOrigin), session.SessionID, session.SourcePath, err)
		}
		return idx.indexManagedProjection(session, data)
	}
	messages := loadOpenCodeJSONSemanticMessages(idx.fs, session)
	return idx.indexSemanticMessages(session.SessionID, messages), nil
}

func loadOpenCodeJSONSemanticMessages(filesystem FileSystem, session DiscoveredSession) []openCodeSemanticMessage {
	storageRoot := resolveStorageRoot(session)
	msgDir := filepath.Join(storageRoot, defaults.OpenCodeDirMessage.String(), string(session.SessionID))

	dirEntries, err := filesystem.ReadDir(msgDir)
	if err != nil {
		return nil
	}

	// Collect and sort message file names for deterministic ordering.
	var msgFiles []string
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, defaults.ExtJSON.String()) {
			continue
		}
		msgFiles = append(msgFiles, name)
	}
	sort.Strings(msgFiles)

	messages := make([]openCodeSemanticMessage, 0, len(msgFiles))
	for _, name := range msgFiles {
		data, err := filesystem.ReadFile(filepath.Join(msgDir, name))
		if err != nil {
			continue
		}
		messageID := strings.TrimSuffix(name, defaults.ExtJSON.String())
		semantic, err := parseOpenCodeSemanticMessage(messageID, 0, data)
		if err != nil {
			continue
		}
		partDir := filepath.Join(storageRoot, defaults.OpenCodeDirPart.String(), messageID)
		for _, partName := range listPartFilenames(filesystem, storageRoot, messageID) {
			partData, readErr := filesystem.ReadFile(filepath.Join(partDir, partName))
			if readErr != nil {
				continue
			}
			partID := strings.TrimSuffix(partName, defaults.ExtJSON.String())
			part, parseErr := parseOpenCodeSemanticPart(partID, 0, partData)
			if parseErr != nil {
				continue
			}
			semantic.Parts = append(semantic.Parts, part)
		}
		messages = append(messages, semantic)
	}
	return messages
}

type openCodeSemanticSummary struct {
	turnCount     int
	toolCallCount int
	tokensIn      int
	tokensOut     int
	modelID       string
	startMS       int64
	endMS         int64
	version       string
	cwd           string
}

func summarizeOpenCodeSemanticMessages(messages []openCodeSemanticMessage) openCodeSemanticSummary {
	var summary openCodeSemanticSummary
	for _, message := range messages {
		created := message.TimeCreated
		if created <= 0 {
			created = message.Data.Time.Created
		}
		completed := message.Data.Time.Completed
		if completed <= 0 {
			completed = message.TimeCompleted
		}
		if completed <= 0 {
			completed = created
		}
		if created > 0 && (summary.startMS == 0 || created < summary.startMS) {
			summary.startMS = created
		}
		if completed > summary.endMS {
			summary.endMS = completed
		}
		if summary.version == "" {
			summary.version = message.Data.Version
		}
		if summary.cwd == "" {
			summary.cwd = message.Data.semanticCWD()
		}
		role := Role(message.Data.Role)
		if role == RoleUser || role == RoleAssistant {
			summary.turnCount++
		}
		if role != RoleAssistant {
			continue
		}
		if message.Data.Tokens != nil {
			summary.tokensIn += message.Data.Tokens.Input
			summary.tokensOut += message.Data.Tokens.Output
		}
		if summary.modelID == "" {
			summary.modelID = message.Data.semanticModelID()
		}
		// Preserve the established JSON metadata contract: every assistant part
		// file is counted. All source representations now use this same policy.
		summary.toolCallCount += len(message.Parts)
	}
	if summary.endMS < summary.startMS {
		summary.endMS = summary.startMS
	}
	return summary
}

// openCodeIndexMsg is the minimal parsed shape for indexing.
type openCodeIndexMsg struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	ModelID   string `json:"modelID"`
	Model     struct {
		ID      string `json:"id"`
		ModelID string `json:"modelID"`
	} `json:"model"`
	Version   string `json:"version"`
	Directory string `json:"directory"`
	CWD       string `json:"cwd"`
	ParentID  string `json:"parentID"`
	Path      struct {
		CWD  string `json:"cwd"`
		Root string `json:"root"`
	} `json:"path"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens *struct {
		Input      int `json:"input"`
		Output     int `json:"output"`
		Reasoning  int `json:"reasoning"`
		CacheRead  int `json:"cache_read"`
		CacheWrite int `json:"cache_write"`
	} `json:"tokens,omitempty"`
	Content    json.RawMessage `json:"content"`
	CacheRead  int             `json:"cache_read"`
	CacheWrite int             `json:"cache_write"`
}

func (message openCodeIndexMsg) semanticCWD() string {
	if message.Path.CWD != "" {
		return message.Path.CWD
	}
	if message.CWD != "" {
		return message.CWD
	}
	return message.Directory
}

func (message openCodeIndexMsg) semanticModelID() string {
	if message.ModelID != "" {
		return message.ModelID
	}
	if message.Model.ModelID != "" {
		return message.Model.ModelID
	}
	return message.Model.ID
}

// openCodeSemanticMessage is the private representation shared by every
// OpenCode source loader. Outer storage formats supply row identity, ordering,
// and raw bytes; indexing consumes only this model.
type openCodeSemanticMessage struct {
	EntryID       string
	TimeCreated   int64
	TimeCompleted int64
	Raw           []byte
	Data          openCodeIndexMsg
	Parts         []openCodeSemanticPart
	// Orphan marks a synthetic message that holds one orphan part whose parent
	// is absent from the selected source. It replaces the in-band data marker.
	Orphan bool
	// Control names a current-schema control record, such as a model switch or
	// an agent switch, that carries no transcript content of its own. It lets
	// the indexer carry a model switch onto the following assistant turn as a
	// model observation while the record itself renders as an inert system note.
	Control string
}

type openCodeSemanticPart struct {
	EntryID     string
	TimeCreated int64
	Raw         []byte
	Data        openCodeIndexPart
	// UnknownType marks a well-formed part whose declared type is outside the
	// known transcript vocabulary. The parser keeps such a part only when it
	// carries renderable text, and the renderer maps it to an inert system note
	// rather than a tool, thinking, or text turn, so newer OpenCode part types
	// never fail a session and never inflate the tool count.
	UnknownType bool
}

type openCodeIndexPart struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
	Tool string `json:"tool"`
	Text string `json:"text"`
	// Synthetic marks a part the harness itself wrote into the transcript
	// rather than a part a person typed or a model produced. OpenCode's task
	// tool sets it on the text part it injects into the parent session when a
	// background task finishes, so the flag is the only honest signal that the
	// message is machine-authored. Reading it keeps the injected result out of
	// the user's own turns.
	Synthetic bool            `json:"synthetic"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	Output    json.RawMessage `json:"output"`
	Time      struct {
		Created int64 `json:"created"`
		Start   int64 `json:"start"`
	} `json:"time"`
	State *struct {
		Status  string          `json:"status"`
		Input   json.RawMessage `json:"input"`
		Content json.RawMessage `json:"content"`
		Output  json.RawMessage `json:"output"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	} `json:"state"`
}

func parseOpenCodeSemanticMessage(entryID string, outerTimeCreated int64, raw []byte) (openCodeSemanticMessage, error) {
	var data openCodeIndexMsg
	if err := json.Unmarshal(raw, &data); err != nil {
		return openCodeSemanticMessage{}, err
	}
	timestamp := outerTimeCreated
	if timestamp <= 0 {
		timestamp = data.Time.Created
	}
	if entryID == "" {
		entryID = data.ID
	}
	return openCodeSemanticMessage{EntryID: entryID, TimeCreated: timestamp, Raw: append([]byte(nil), raw...), Data: data}, nil
}

func parseOpenCodeSemanticPart(entryID string, outerTimeCreated int64, raw []byte) (openCodeSemanticPart, error) {
	var data openCodeIndexPart
	if err := json.Unmarshal(raw, &data); err != nil {
		return openCodeSemanticPart{}, err
	}
	data.Text = unwrapOpenCodeDoubleEncodedText(data.Text)
	timestamp := outerTimeCreated
	if timestamp <= 0 {
		timestamp = data.Time.Created
		if timestamp <= 0 {
			timestamp = data.Time.Start
		}
	}
	if entryID == "" {
		entryID = data.ID
	}
	return openCodeSemanticPart{EntryID: entryID, TimeCreated: timestamp, Raw: append([]byte(nil), raw...), Data: data}, nil
}

func (idx *OpenCodeIndexer) indexSemanticMessages(sessionID SessionID, messages []openCodeSemanticMessage) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, 0, len(messages))
	messageIndexes := make(map[string]int, len(messages))
	messageParents := make(map[int]string, len(messages))
	entryIndex := 0
	// pendingObservedModel carries a model switch onto the next assistant turn
	// that has no model of its own, which is exactly the per-turn model
	// observation the indexer records elsewhere.
	pendingObservedModel := ""
	for _, message := range messages {
		if isOrphanOpenCodeSemanticMessage(message) && len(message.Parts) == 1 {
			// Orphan parts follow the same depth gate as ordinary parts.
			if idx.fullDepth {
				entries = append(entries, idx.openCodeOrphanPartEntry(sessionID, message.Parts[0], entryIndex))
				entryIndex++
			}
			continue
		}
		entry := openCodeMessageEntry(sessionID, entryIndex, message, idx.fullContent)
		inspection := inspectOpenCodeSemanticParts(message.Parts)
		if inspection.isSystemEntry {
			entry.Role = RoleSystem
			entry.EntryType = EntryTypeSystem
		} else if inspection.skillName != "" {
			extra := `{"command_name":"` + jsonEscapeString(inspection.skillName) + `"}`
			entry.Extra = &extra
		} else if inspection.toolPartCount > 0 && entry.Role == RoleAssistant {
			entry.HasToolUse = true
			entry.EntryType = EntryTypeToolUse
		}
		if entry.Role != RoleAssistant {
			entry.Extra = removeModelObservation(entry.Extra)
		}
		if entry.Role == RoleAssistant && pendingObservedModel != "" {
			// The following assistant turn adopts a preceding model switch only
			// when it does not already carry its own model observation.
			if message.Data.semanticModelID() == "" {
				entry.Extra = addModelObservation(entry.Extra, pendingObservedModel)
			}
			pendingObservedModel = ""
		}
		if message.Control == "model-switched" {
			if observed := message.Data.semanticModelID(); observed != "" {
				pendingObservedModel = observed
			}
		}
		if entry.ContentPreview == nil {
			if preview := firstOpenCodeSemanticText(message.Parts); preview != "" {
				if !idx.fullContent {
					preview = truncateString(preview, defaults.ContentPreviewLimit)
				}
				entry.ContentPreview = &preview
			}
		}
		parentIndex := entryIndex
		entries = append(entries, entry)
		if message.EntryID != "" {
			messageIndexes[message.EntryID] = parentIndex
		}
		if message.Data.ParentID != "" {
			messageParents[parentIndex] = message.Data.ParentID
		}
		entryIndex++
		if !idx.fullDepth {
			continue
		}
		for _, part := range message.Parts {
			partEntry, include := idx.openCodePartEntry(sessionID, part, parentIndex, entryIndex, entry.Role, entry.ContentPreview)
			if !include {
				continue
			}
			entries = append(entries, partEntry)
			entryIndex++
		}
	}
	normalizeOpenCodeEntryGraph(entries, messageIndexes, messageParents)
	return entries
}

// normalizeOpenCodeEntryGraph carries the OpenCode message graph on
// ParentEntryID. A depth-0 message names its parent message by native id, the
// same field the Claude and Cursor indexers set at depth 0. ParentIndex stays
// nil at depth 0; parts keep their depth-1 ParentIndex to the enclosing
// message. The link is set only when the parent message is present in the
// selected source. A parent reference that would close a cycle stays at the
// root, so the pinned node is the same on every run.
func normalizeOpenCodeEntryGraph(entries []schema.SessionEntry, messageIndexes map[string]int, messageParents map[int]string) {
	children := make([]int, 0, len(messageParents))
	for childIndex := range messageParents {
		children = append(children, childIndex)
	}
	sort.Ints(children)
	linked := make(map[int]int, len(messageParents))
	for _, childIndex := range children {
		parentIndex, present := messageIndexes[messageParents[childIndex]]
		if !present || parentIndex == childIndex || openCodeLinkedChainReaches(linked, parentIndex, childIndex) {
			continue
		}
		linked[childIndex] = parentIndex
		parentID := messageParents[childIndex]
		entries[childIndex].ParentEntryID = &parentID
	}
}

// openCodeLinkedChainReaches reports whether the already-linked parent chain
// that starts at start already contains target. It walks only the links this
// pass has applied, so linking children in entry order breaks a cycle at the
// same member on every run.
func openCodeLinkedChainReaches(linked map[int]int, start, target int) bool {
	visited := make(map[int]bool)
	for index := start; ; {
		if index == target {
			return true
		}
		if visited[index] {
			return false
		}
		visited[index] = true
		parent, present := linked[index]
		if !present {
			return false
		}
		index = parent
	}
}

// openCodeOrphanPartEntry renders a part whose parent message is absent from
// the selected source. The entry is an inert root-level system note. It never
// becomes a tool turn, so consumers do not fold or count it as a tool call.
func (idx *OpenCodeIndexer) openCodeOrphanPartEntry(sessionID SessionID, part openCodeSemanticPart, entryIndex int) schema.SessionEntry {
	entry := schema.SessionEntry{SessionID: sessionID, EntryIndex: entryIndex, Harness: HarnessOpenCode, Role: RoleSystem, EntryType: EntryTypeSystem}
	rawLength := len(part.Raw)
	entry.RawByteLength = &rawLength
	partType := part.Data.Type
	entry.PartType = &partType
	if part.TimeCreated > 0 {
		timestamp := part.TimeCreated
		entry.TimestampMs = &timestamp
	}
	if part.EntryID != "" {
		entryID := part.EntryID
		entry.EntryID = &entryID
	}
	text := part.Data.Text
	if text == "" {
		text = fmt.Sprintf("OpenCode %s part %s has no parent message in the selected source", partType, part.EntryID)
	}
	if !idx.fullContent {
		text = truncateString(text, defaults.ContentPreviewLimit)
	}
	entry.ContentPreview = &text
	return entry
}

// isOrphanOpenCodeSemanticMessage reports whether a message is a synthetic
// orphan slot. The managed projection sets the typed Orphan field.
func isOrphanOpenCodeSemanticMessage(message openCodeSemanticMessage) bool {
	return message.Orphan
}

func missingOpenCodeParentDiagnostics(session DiscoveredSession, messages []openCodeSemanticMessage) []DiagnosticEntry {
	identities := make(map[string]struct{}, len(messages))
	for _, message := range messages {
		if message.EntryID == "" {
			continue
		}
		identities[message.EntryID] = struct{}{}
	}
	// Replay the entry-graph normalization once to learn which single link each
	// cycle actually dropped, so the cycle diagnostic names only the pinned
	// member rather than every node on the cycle.
	droppedCycleLinks := openCodeDroppedCycleLinks(messages)
	seen := make(map[string]struct{})
	diagnostics := make([]DiagnosticEntry, 0)
	for _, message := range messages {
		// A synthetic orphan slot carries no real parent, so it never produces a
		// missing-parent diagnostic.
		if isOrphanOpenCodeSemanticMessage(message) {
			continue
		}
		parentID := message.Data.ParentID
		if parentID == "" {
			continue
		}
		if _, present := identities[parentID]; !present {
			key := message.EntryID + "\x00" + parentID
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			diagnostics = append(diagnostics, DiagnosticEntry{
				ErrorType:   string(OpenCodeGraphMissingParent),
				Location:    fmt.Sprintf("selected OpenCode session %s message %s from %s", session.SessionID, message.EntryID, session.SourcePath),
				Message:     fmt.Sprintf("message %s references parent %s, but that parent is absent from the selected canonical representation; while normalizing the selected transcript Peasant retained the message at the root, which means no content was dropped but the upstream relationship is incomplete", message.EntryID, parentID),
				Remediation: "Allow OpenCode to finish persisting the session, then re-run ingest; if the parent remains absent, keep the root-attached message and repair or export the source through OpenCode rather than modifying its database with Peasant.",
			})
			continue
		}
		// The parent exists but linking it would have closed a cycle, so
		// normalization kept exactly this member at the root. Name that one
		// broken link, not every node on the cycle.
		if _, dropped := droppedCycleLinks[message.EntryID]; dropped {
			key := message.EntryID + "\x00" + parentID
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			diagnostics = append(diagnostics, DiagnosticEntry{
				ErrorType:   string(OpenCodeGraphMissingParent),
				Location:    fmt.Sprintf("selected OpenCode session %s message %s from %s", session.SessionID, message.EntryID, session.SourcePath),
				Message:     fmt.Sprintf("message %s references parent %s, but linking it would close a parent cycle, so while normalizing the selected transcript Peasant kept the message at the root; no content was dropped but the upstream relationship is incomplete", message.EntryID, parentID),
				Remediation: "Allow OpenCode to finish persisting the session, then re-run ingest; if the cycle remains, keep the root-attached message and repair or export the source through OpenCode rather than modifying its database with Peasant.",
			})
		}
	}
	return diagnostics
}

// openCodeDroppedCycleLinks replays the message linking that
// normalizeOpenCodeEntryGraph performs and returns each message whose parent
// link was dropped because linking it would have closed a cycle. Normalization
// links children in entry order and drops only the one reference that closes
// each cycle, so this names that single pinned member per cycle, not every node
// on it. A self reference counts as a dropped link.
func openCodeDroppedCycleLinks(messages []openCodeSemanticMessage) map[string]string {
	present := make(map[string]struct{}, len(messages))
	for _, message := range messages {
		if message.EntryID == "" || isOrphanOpenCodeSemanticMessage(message) {
			continue
		}
		present[message.EntryID] = struct{}{}
	}
	linked := make(map[string]string, len(messages))
	dropped := make(map[string]string)
	for _, message := range messages {
		if message.EntryID == "" || isOrphanOpenCodeSemanticMessage(message) {
			continue
		}
		parentID := message.Data.ParentID
		if parentID == "" {
			continue
		}
		if _, ok := present[parentID]; !ok {
			continue
		}
		if parentID == message.EntryID || openCodeLinkedChainReachesID(linked, parentID, message.EntryID) {
			dropped[message.EntryID] = parentID
			continue
		}
		linked[message.EntryID] = parentID
	}
	return dropped
}

// openCodeLinkedChainReachesID reports whether the already-linked parent chain
// that starts at start already contains target. It walks only the links applied
// so far, so linking children in entry order breaks a cycle at the same member
// on every run.
func openCodeLinkedChainReachesID(linked map[string]string, start, target string) bool {
	visited := make(map[string]bool)
	for cursor := start; ; {
		if cursor == target {
			return true
		}
		if visited[cursor] {
			return false
		}
		visited[cursor] = true
		parent, present := linked[cursor]
		if !present {
			return false
		}
		cursor = parent
	}
}

func openCodeMessageEntry(sessionID SessionID, index int, message openCodeSemanticMessage, fullContent bool) schema.SessionEntry {
	role := Role(message.Data.Role)
	if !role.IsValid() {
		role = RoleSystem
	}
	entry := schema.SessionEntry{SessionID: sessionID, EntryIndex: index, Harness: HarnessOpenCode, Role: role, EntryType: classifyOpenCodeEntry(role)}
	if message.TimeCreated > 0 {
		timestamp := message.TimeCreated
		entry.TimestampMs = &timestamp
	}
	if message.EntryID != "" {
		entryID := message.EntryID
		entry.EntryID = &entryID
	}
	rawLength := len(message.Raw)
	entry.RawByteLength = &rawLength
	if message.Data.Tokens != nil {
		if message.Data.Tokens.Input > 0 {
			value := message.Data.Tokens.Input
			entry.TokensIn = &value
		}
		if message.Data.Tokens.Output > 0 {
			value := message.Data.Tokens.Output
			entry.TokensOut = &value
		}
	}
	if preview := extractOpenCodePreview(message.Data.Content); preview != "" {
		if !fullContent {
			preview = truncateString(preview, defaults.ContentPreviewLimit)
		}
		entry.ContentPreview = &preview
	}
	entry.Extra = buildExtraJSON(&message.Data)
	return entry
}

func inspectOpenCodeSemanticParts(parts []openCodeSemanticPart) partInspection {
	var result partInspection
	for _, part := range parts {
		if part.UnknownType {
			// A tolerated unknown part is an inert note, so it never turns its
			// message into a tool or skill entry.
			continue
		}
		if part.Data.Synthetic {
			// A synthetic part is harness-authored. It reclassifies its message
			// the same way a compaction, subtask, or agent part does, whatever
			// role the stored message carries: a synthetic part on an assistant
			// message reclassifies that message to a system entry too.
			result.isSystemEntry = true
		}
		switch part.Data.Type {
		case "compaction", "subtask", "agent":
			result.isSystemEntry = true
		case "tool", "tool_use":
			toolName := openCodeSemanticToolName(part.Data)
			if toolName == "skill" {
				name := ""
				if part.Data.State != nil {
					var input struct {
						Name string `json:"name"`
					}
					_ = json.Unmarshal(part.Data.State.Input, &input)
					name = input.Name
				}
				if name == "" {
					name = "unknown-skill"
				}
				if !strings.HasPrefix(name, "/") {
					name = "/" + name
				}
				if result.skillName == "" {
					result.skillName = name
				}
			} else {
				result.toolPartCount++
			}
		case "text", "reasoning":
			// Prose and thinking parts carry no tool call. Counting them here
			// would relabel an ordinary assistant message as a tool turn and
			// hide its text behind an empty tool card.
		default:
			result.toolPartCount++
		}
	}
	return result
}

// openCodeSemanticToolName reads the invoked tool's name. OpenCode records it
// on the part's "tool" field; older rows carry it on "name". Both shapes must
// resolve to the same name, because the name is what a reader sees on the tool
// card and what the tool-kind classifier keys on.
func openCodeSemanticToolName(data openCodeIndexPart) string {
	if data.Tool != "" {
		return data.Tool
	}
	return data.Name
}

func firstOpenCodeSemanticText(parts []openCodeSemanticPart) string {
	for _, part := range parts {
		if part.Data.Type == "text" && part.Data.Text != "" {
			return part.Data.Text
		}
	}
	return ""
}

func (idx *OpenCodeIndexer) openCodePartEntry(sessionID SessionID, part openCodeSemanticPart, parentIndex, entryIndex int, parentRole schema.Role, parentContent *string) (schema.SessionEntry, bool) {
	entry := schema.SessionEntry{SessionID: sessionID, EntryIndex: entryIndex, Harness: HarnessOpenCode, Depth: 1, ParentIndex: &parentIndex}
	rawLength := len(part.Raw)
	entry.RawByteLength = &rawLength
	partType := part.Data.Type
	entry.PartType = &partType
	if part.TimeCreated > 0 {
		timestamp := part.TimeCreated
		entry.TimestampMs = &timestamp
	}
	if part.UnknownType {
		// A tolerated unknown part carries renderable text but no known role, so
		// it renders as an inert system note. It never becomes a tool, thinking,
		// or assistant turn.
		entry.EntryType, entry.Role = EntryTypeSystem, RoleSystem
		if part.Data.Text != "" {
			text := part.Data.Text
			if !idx.fullContent {
				text = truncateString(text, defaults.ContentPreviewLimit)
			}
			entry.ContentPreview = &text
		}
		if part.EntryID != "" {
			entryID := part.EntryID
			entry.EntryID = &entryID
		}
		return entry, true
	}
	switch partType {
	case "tool":
		entry.EntryType, entry.Role, entry.HasToolUse = EntryTypeToolUse, RoleAssistant, true
		if part.Data.State != nil {
			if value := rawOpenCodeJSON(part.Data.State.Input); value != "" {
				entry.ToolInput = &value
			}
			// Output precedence: "output" is what a current OpenCode row
			// writes, "result" is the older alias for the same value, "error"
			// names a failure that produced no output, and "content" is the
			// last legacy alias. The first present field wins, so a completed
			// call always shows its output rather than a stale alias.
			if value := firstOpenCodeToolOutput(part.Data.State.Output, part.Data.State.Result, part.Data.State.Error, part.Data.State.Content); value != "" {
				entry.ToolOutput = &value
			}
		}
	case "tool_use":
		entry.EntryType, entry.Role, entry.HasToolUse = EntryTypeToolUse, RoleAssistant, true
		if value := rawOpenCodeJSON(part.Data.Input); value != "" {
			entry.ToolInput = &value
		}
	case "tool_result":
		entry.EntryType, entry.Role = EntryTypeToolResult, RoleTool
		if value := firstOpenCodeToolOutput(part.Data.Content, part.Data.Output); value != "" {
			entry.ToolOutput = &value
		}
	case "text":
		entry.EntryType, entry.Role = EntryTypeText, parentRole
		// A text part that repeats its message's own preview would render the
		// same prose twice: once on the message turn and once on the part turn.
		// The message turn already carries it, so drop the part.
		if parentContent != nil && *parentContent != "" && part.Data.Text == *parentContent {
			return schema.SessionEntry{}, false
		}
		if part.Data.Text != "" {
			text := part.Data.Text
			if !idx.fullContent {
				text = truncateString(text, defaults.ContentPreviewLimit)
			}
			entry.ContentPreview = &text
		}
	default:
		entry.EntryType, entry.Role = EntryTypeText, parentRole
		if partType == "reasoning" {
			entry.EntryType, entry.HasThinking = EntryTypeThinking, true
		}
		if part.Data.Text != "" {
			text := part.Data.Text
			if !idx.fullContent {
				text = truncateString(text, defaults.ContentPreviewLimit)
			}
			entry.ContentPreview = &text
		}
	}
	if part.EntryID != "" {
		entryID := part.EntryID
		entry.EntryID = &entryID
	}
	if partType == "tool" || partType == "tool_use" || partType == "tool_result" {
		callID := part.Data.ID
		if callID == "" {
			callID = part.EntryID
		}
		if callID != "" {
			entry.ToolCallID = &callID
		}
	}
	if partType == "tool" || partType == "tool_use" {
		toolName := openCodeSemanticToolName(part.Data)
		if toolName == "" {
			return entry, true
		}
		entry.ToolNamesCSV = &toolName
		if kind := classifyToolKind(toolName); kind != "" {
			entry.ToolKind = &kind
		}
	}
	return entry, true
}

func rawOpenCodeJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	compacted, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	return string(compacted)
}

// firstOpenCodeToolOutput renders the first present tool output as the text a
// reader sees. A JSON string output is unwrapped to its plain text, the same
// shape the Claude reader stores, so one renderer shows both harnesses without
// escaped newlines or wrapping quotes. Any other JSON shape stays compact JSON.
func firstOpenCodeToolOutput(values ...json.RawMessage) string {
	for _, value := range values {
		encoded := rawOpenCodeJSON(value)
		if encoded == "" {
			continue
		}
		var text string
		if json.Unmarshal(value, &text) == nil {
			return text
		}
		return encoded
	}
	return ""
}

// parseOpenCodeMessage parses a single message JSON into a SessionEntry.
// When fullContent is true, ContentPreview is set without truncation.
func parseOpenCodeMessage(sessionID SessionID, index int, data []byte, fullContent bool) (schema.SessionEntry, bool) {
	message, err := parseOpenCodeSemanticMessage("", 0, data)
	if err != nil {
		return schema.SessionEntry{}, false
	}
	return openCodeMessageEntry(sessionID, index, message, fullContent), true
}

// classifyOpenCodeEntry determines EntryType from Role.
func classifyOpenCodeEntry(role Role) EntryType {
	switch role {
	case RoleUser:
		return EntryTypeText
	case RoleAssistant:
		return EntryTypeText // refined to EntryTypeToolUse if parts exist
	case RoleTool:
		return EntryTypeToolResult
	case RoleSystem:
		return EntryTypeSystem
	default:
		return EntryTypeText
	}
}

// unwrapOpenCodeDoubleEncodedText decodes a text value that is itself one JSON
// string literal, and returns every other value unchanged.
//
// Some launchers hand OpenCode a prompt that has already been JSON-encoded, so
// the stored part holds {"type":"text","text":"\"Run /reviewer first. ...\""}.
// The text VALUE is then a quoted literal with escaped newlines rather than the
// prompt itself, which renders as a quote-wrapped turn whose markdown is broken
// by literal backslash-n. Decoding it once at indexing time restores the prompt,
// so the preview, the stored transcript, and a push all carry the same text.
//
// The unwrap is deliberately narrow and never recursive:
//   - the whole value must be exactly one JSON string, so text that merely
//     CONTAINS quotes, and text that looks like a JSON object or array, is left
//     alone;
//   - a value that decodes to the empty string is left alone, because emptying
//     a turn loses more than the quotes cost;
//   - the decoded result is returned as-is, so a prompt that a launcher encoded
//     twice keeps one visible layer rather than being silently unwrapped again.
func unwrapOpenCodeDoubleEncodedText(text string) string {
	if len(text) < 2 || text[0] != '"' || text[len(text)-1] != '"' {
		return text
	}
	if decoded, ok := decodeOneJSONStringLiteral(text); ok {
		return decoded
	}
	// A launcher that wrapped a MULTI-LINE prompt left the newlines raw inside
	// the quotes, and no JSON string literal may hold a raw control character,
	// so the value above did not parse. On the real store 31 of the 32 wrapped
	// prompts are of this kind, so the narrow parse alone would have left almost
	// every affected turn quote-wrapped. Escaping exactly the whitespace control
	// characters and parsing once more accepts them and nothing else: a value
	// holding any other control character is still left alone.
	escaped, ok := escapeRawWhitespaceControls(text)
	if !ok {
		return text
	}
	if decoded, ok := decodeOneJSONStringLiteral(escaped); ok {
		return decoded
	}
	return text
}

// decodeOneJSONStringLiteral decodes text when the WHOLE of it is exactly one
// JSON string literal that stands for a non-empty string. A trailing token
// means the value was not one literal on its own, so decoding it would drop
// whatever followed. An empty result is refused because emptying a turn loses
// more than the quotes cost.
func decodeOneJSONStringLiteral(text string) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(text))
	var decoded string
	if err := decoder.Decode(&decoded); err != nil || decoded == "" {
		return "", false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", false
	}
	return decoded, true
}

// escapeRawWhitespaceControls rewrites raw newline, carriage return, and tab
// bytes as their JSON escapes and reports whether it could. It reports false
// for any other control character, so a value carrying one is never coerced
// into parsing.
func escapeRawWhitespaceControls(text string) (string, bool) {
	var builder strings.Builder
	builder.Grow(len(text))
	for index := 0; index < len(text); index++ {
		switch character := text[index]; character {
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if character < 0x20 {
				return "", false
			}
			builder.WriteByte(character)
		}
	}
	return builder.String(), true
}

// extractOpenCodePreview tries to extract preview text from message content.
// Content can be a string, an array of blocks, or absent.
func extractOpenCodePreview(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	// Try as plain string.
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return unwrapOpenCodeDoubleEncodedText(s)
	}

	// Try as array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return unwrapOpenCodeDoubleEncodedText(b.Text)
			}
		}
	}

	return ""
}

// listPartFilenames returns sorted JSON filenames under {storageRoot}/part/{msgID}/.
// Returns nil if the directory cannot be read.
// This is the package-level variant of (*OpenCodeIndexer).listPartFiles for use
// by types that do not embed OpenCodeIndexer.
func listPartFilenames(fs FileSystem, storageRoot, msgID string) []string {
	partDir := filepath.Join(storageRoot, defaults.OpenCodeDirPart.String(), msgID)
	dirEntries, err := fs.ReadDir(partDir)
	if err != nil {
		return nil
	}
	var partFiles []string
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, defaults.ExtJSON.String()) {
			continue
		}
		partFiles = append(partFiles, name)
	}
	sort.Strings(partFiles)
	return partFiles
}

// listPartFiles returns sorted JSON filenames under {storageRoot}/part/{msgID}/.
// Returns nil if the directory cannot be read.
func (idx *OpenCodeIndexer) listPartFiles(storageRoot, msgID string) []string {
	return listPartFilenames(idx.fs, storageRoot, msgID)
}

// countParts counts JSON files under {root}/part/{msgID}/.
func (idx *OpenCodeIndexer) countParts(storageRoot, msgID string) int {
	return len(idx.listPartFiles(storageRoot, msgID))
}

// partInspection summarises structural signals found across a message's part files.
// It is produced by inspectParts and consumed in IndexTranscript to reclassify
// the parent message entry.
type partInspection struct {
	// isSystemEntry is true if any part has a structural system type
	// ("compaction", "subtask", or "agent") or carries the harness's own
	// synthetic marker. These reclassify the parent message to role=system,
	// entry_type=system.
	isSystemEntry bool

	// skillName is non-empty if a part has type="tool" and tool="skill".
	// The value is the skill name extracted from state.input.name, prefixed
	// with "/" (e.g. "/aura:epoch"). Skill entries stay role=user.
	// isSystemEntry takes precedence: if both are set, isSystemEntry wins.
	skillName string

	// toolPartCount is the count of non-system, non-skill part files.
	// Used for HasToolUse / EntryTypeToolUse refinement when neither
	// isSystemEntry nor skillName is set.
	toolPartCount int
}

// buildExtraJSON builds the Extra JSON string from message fields.
// Returns nil if no extra fields are present.
func buildExtraJSON(msg *openCodeIndexMsg) *string {
	extra := make(map[string]any)

	if msg.Tokens != nil && msg.Tokens.Reasoning > 0 {
		extra["tokens_reasoning"] = msg.Tokens.Reasoning
	}
	if msg.Role == RoleAssistant.String() && ValidObservedModel(msg.ModelID) {
		extra["model_id"] = msg.ModelID
	}

	// Cache fields: prefer tokens-level, fall back to top-level message fields.
	cacheRead := 0
	cacheWrite := 0
	if msg.Tokens != nil {
		cacheRead = msg.Tokens.CacheRead
		cacheWrite = msg.Tokens.CacheWrite
	}
	if cacheRead == 0 {
		cacheRead = msg.CacheRead
	}
	if cacheWrite == 0 {
		cacheWrite = msg.CacheWrite
	}
	if cacheRead > 0 {
		extra["cache_read"] = cacheRead
	}
	if cacheWrite > 0 {
		extra["cache_write"] = cacheWrite
	}

	if len(extra) == 0 {
		return nil
	}

	b, err := json.Marshal(extra)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}
