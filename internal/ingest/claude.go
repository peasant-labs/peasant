package ingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/redact"
)

// ClaudeAdapter discovers and extracts metadata from Claude Code JSONL transcripts.
type ClaudeAdapter struct {
	fs             FileSystem
	git            GitResolver
	salt           salt.Salt
	evidence       ClaudeEvidenceCache
	locationLookup SessionLocationLookup
	// reminedCount is how many evidence records the most recent Discover had
	// to mine again. It describes that one call and is overwritten by the next.
	reminedCount int
}

var _ SourceAdapter = (*ClaudeAdapter)(nil)
var _ ClaudeEvidenceCaching = (*ClaudeAdapter)(nil)
var _ ClaudeSessionLocationLookupCapable = (*ClaudeAdapter)(nil)
var _ DiscoveryStatistics = (*ClaudeAdapter)(nil)
var _ OriginEvidenceMiner = (*ClaudeAdapter)(nil)

// NewClaudeAdapter creates a ClaudeAdapter with injected dependencies.
func NewClaudeAdapter(fs FileSystem, git GitResolver, s salt.Salt) *ClaudeAdapter {
	return &ClaudeAdapter{fs: fs, git: git, salt: s}
}

// SetClaudeEvidenceCache makes discovery reuse the evidence it mined during an
// earlier run. Without a cache the adapter mines every transcript again.
func (a *ClaudeAdapter) SetClaudeEvidenceCache(cache ClaudeEvidenceCache) {
	a.evidence = cache
}

// SetSessionLocationLookup gives the adapter a way to confirm whether a
// candidate cross-run spawner is already persisted, and what parent it
// already has, so re-parenting can trust data outside this run's own write
// batch. Without a lookup, cross-run linking never fires (see
// claudeSessionAlreadyStored and claudeStoredParent).
func (a *ClaudeAdapter) SetSessionLocationLookup(lookup SessionLocationLookup) {
	a.locationLookup = lookup
}

func (a *ClaudeAdapter) Harness() Harness {
	return HarnessClaudeCode
}

// claudeJSONLLine is the raw shape of each line in a Claude Code JSONL transcript.
// Fields are parsed only as-needed; unmapped fields are silently ignored.
type claudeJSONLLine struct {
	SessionID string           `json:"sessionId"`
	Version   string           `json:"version"`
	Type      claudeRecordType `json:"type"`
	CWD       string           `json:"cwd"`
	GitBranch string           `json:"gitBranch"`
	AgentID   string           `json:"agentId"`
	UUID      string           `json:"uuid"`
	Timestamp string           `json:"timestamp"`
	Message   struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"` // string or []contentBlock
		Usage   *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message"`
}

// claudeRecordType names the record kinds discovery reads from a transcript.
type claudeRecordType string

// String returns the wire form of the record type.
func (t claudeRecordType) String() string { return string(t) }

// isConversation reports whether the record type is one a reader can render as
// part of the conversation.
func (t claudeRecordType) isConversation() bool {
	return t == claudeRecordTypeUser || t == claudeRecordTypeAssistant
}

const (
	// claudeRecordTypeUser marks a user record.
	claudeRecordTypeUser claudeRecordType = "user"
	// claudeRecordTypeAssistant marks an assistant record.
	claudeRecordTypeAssistant claudeRecordType = "assistant"
)

// claudeContentBlockText names the content block that carries prose. A user
// record writes its prompt either as a bare string or as blocks of this type.
const claudeContentBlockText = "text"

// claudeHintLineLimit caps how many leading lines discovery reads for the
// display hints, so a large transcript never costs a full hint scan.
const claudeHintLineLimit = 10

// contentBlock is a typed block inside an assistant message content array.
type contentBlock struct {
	Type string `json:"type"`
}

type claudeFileOpener interface {
	Open(path string) (io.ReadCloser, error)
}

// Discover walks each configured source path looking for *.jsonl files.
// It identifies root sessions (UUID.jsonl at the project slug level) and links
// subagent sessions found under {uuid}/subagents/agent-*.jsonl.
//
// Uses a two-pass approach: first collects all file entries, then processes
// roots before subagents. This ensures parent sessions are registered before
// subagent linking, regardless of filesystem walk order.
//
// Per RFC Section 6.4, symlinks are skipped silently.
func (a *ClaudeAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	var sessions []DiscoveredSession

	// Mining a transcript is the expensive part of Claude discovery: it reads
	// the whole file and parses every line. Load what an earlier run mined,
	// reuse every record whose transcript has not changed, and write back only
	// the records this run had to mine again.
	cached := a.loadCachedEvidence(ctx)
	mined := make(map[ResolvedPath]ClaudeTranscriptEvidence, len(cached))
	var remined []ClaudeTranscriptEvidence

	for _, basePath := range cfg.Paths {
		root := string(basePath)

		// Two-pass approach to handle walk order differences between
		// os.WalkDir (DFS: dirs before sibling files) and MemFS (flat sorted).

		type rootEntry struct {
			path      string
			sessionID string
		}
		type subagentEntry struct {
			path          string
			subagentID    string
			parentUUIDStr string
		}

		var rootEntries []rootEntry
		var subagentEntries []subagentEntry

		// Pass 1: Collection — walk and categorize files.
		walkErr := a.fs.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			if err != nil {
				return nil // skip unreadable entries
			}

			// Skip symlinks per RFC Section 6.4.
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}

			if d.IsDir() {
				return nil
			}

			// Only process .jsonl files.
			if !strings.HasSuffix(path, defaults.ExtJSONL.String()) {
				return nil
			}

			// Determine the session type by relative path structure.
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}

			parts := strings.Split(rel, string(filepath.Separator))

			// Root session: {project-slug}/{uuid}.jsonl (depth 2)
			if len(parts) == 2 {
				sessionIDStr := strings.TrimSuffix(parts[1], defaults.ExtJSONL.String())
				if _, err := NewSessionID(sessionIDStr); err == nil {
					rootEntries = append(rootEntries, rootEntry{path: path, sessionID: sessionIDStr})
				}
				return nil
			}

			// Subagent session: {project-slug}/{uuid}/subagents/agent-{hex}.jsonl (depth 4)
			if len(parts) == 4 && parts[2] == defaults.DirSubagents.String() && strings.HasPrefix(parts[3], defaults.ClaudeSubagentPrefix) {
				parentUUIDStr := parts[1]
				subagentIDStr := strings.TrimSuffix(parts[3], defaults.ExtJSONL.String())
				if _, err := NewSessionID(parentUUIDStr); err == nil {
					if _, err := NewSessionID(subagentIDStr); err == nil {
						subagentEntries = append(subagentEntries, subagentEntry{
							path:          path,
							subagentID:    subagentIDStr,
							parentUUIDStr: parentUUIDStr,
						})
					}
				}
				return nil
			}

			return nil
		})

		if walkErr != nil {
			return nil, fmt.Errorf("Discover: walk %q: %w", root, walkErr)
		}

		// Pass 2: Process roots first, then subagents.
		rootIndex := make(map[string]int)

		for _, entry := range rootEntries {
			info, err := a.fs.Stat(entry.path)
			if err != nil {
				continue
			}
			rp := ResolvedPath(entry.path)

			evidence, ok := cached[rp]
			if !ok || !evidence.Fresh(ClaudeEvidenceScopeRoot, info) {
				var cacheable bool
				evidence, cacheable = a.mineClaudeRootTranscript(rp, info)
				if cacheable {
					remined = append(remined, evidence)
				}
			}
			mined[rp] = evidence

			if !evidence.HasConversationRecord {
				continue
			}
			sid, _ := NewSessionID(entry.sessionID) // already validated in pass 1

			ds := DiscoveredSession{
				SessionID:     sid,
				Harness:       HarnessClaudeCode,
				SourcePath:    rp,
				SourceFormat:  SourceFormatJSONL,
				OriginalRoot:  basePath,
				SubagentPaths: []ResolvedPath{},
				DebugPaths:    []ResolvedPath{},
				ModTime:       info.ModTime(),
				Title:         evidence.Title,
				Branch:        evidence.Branch,
				CWD:           evidence.CWD,
				Origin:        evidence.Origin,
				Signal:        evidence.Signal,
			}
			rootIndex[entry.sessionID] = len(sessions)
			sessions = append(sessions, ds)
		}

		for _, entry := range subagentEntries {
			info, err := a.fs.Stat(entry.path)
			if err != nil {
				continue
			}
			rp := ResolvedPath(entry.path)

			evidence, ok := cached[rp]
			if !ok || !evidence.Fresh(ClaudeEvidenceScopeSubagent, info) {
				evidence = a.mineClaudeSubagentTranscript(rp, info)
				remined = append(remined, evidence)
			}
			mined[rp] = evidence

			if !evidence.HasConversationRecord {
				continue
			}
			parentSID, _ := NewSessionID(entry.parentUUIDStr) // already validated
			subSID, _ := NewSessionID(entry.subagentID)       // already validated

			// Link to parent's SubagentPaths — guaranteed parent is already registered.
			if idx, ok := rootIndex[entry.parentUUIDStr]; ok {
				sessions[idx].SubagentPaths = append(sessions[idx].SubagentPaths, rp)
			}

			ds := DiscoveredSession{
				SessionID:     subSID,
				Harness:       HarnessClaudeCode,
				SourcePath:    rp,
				SourceFormat:  SourceFormatJSONL,
				OriginalRoot:  basePath,
				ParentUUID:    &parentSID,
				SubagentPaths: []ResolvedPath{},
				DebugPaths:    []ResolvedPath{},
				ModTime:       info.ModTime(),
				Origin:        evidence.Origin,
				Signal:        evidence.Signal,
			}
			sessions = append(sessions, ds)
		}
	}

	a.linkClaudeTeammates(ctx, sessions, cached, mined)
	a.saveMinedEvidence(ctx, cfg, cached, mined, remined)
	a.reminedCount = len(remined)
	return sessions, nil
}

// ReminedCount reports how many cached evidence records the most recent Discover
// had to mine again. See DiscoveryStatistics for the scoping rule.
func (a *ClaudeAdapter) ReminedCount() int { return a.reminedCount }

// MineOriginEvidence re-reads one Claude transcript that is still on disk and
// returns the origin its content decides, for the stored-row resolve pass.
//
// It is the ordinary root miner, not a second one: the same read, the same
// captures, and the same rule. A transcript that cannot be stat-ed or cannot be
// read reports false, which is the resolver's degraded case, because the same
// file may be readable on a later run.
//
// A child transcript never reaches here. The resolver decides a row that already
// carries a parent at rule step one, with no file read at all.
func (a *ClaudeAdapter) MineOriginEvidence(path ResolvedPath) (sessionorigin.Origin, bool) {
	info, err := a.fs.Stat(path.String())
	if err != nil {
		return "", false
	}
	evidence, readable := a.mineClaudeRootTranscript(path, info)
	if !readable {
		return "", false
	}
	return evidence.Origin, true
}

// loadCachedEvidence returns the evidence an earlier discovery mined. A missing
// or failing cache returns an empty map, so discovery mines everything again.
func (a *ClaudeAdapter) loadCachedEvidence(ctx context.Context) map[ResolvedPath]ClaudeTranscriptEvidence {
	if a.evidence == nil {
		return nil
	}
	records, err := a.evidence.LoadClaudeEvidence(ctx)
	if err != nil {
		return nil
	}
	return records
}

// saveMinedEvidence writes the records this run mined and removes the records
// whose transcripts are gone. Only the walked source paths are pruned, so a
// record left by a path this run did not visit stays in the cache. A cache
// failure is silent: the next discovery simply mines the transcripts again.
func (a *ClaudeAdapter) saveMinedEvidence(
	ctx context.Context,
	cfg SourceConfig,
	cached map[ResolvedPath]ClaudeTranscriptEvidence,
	mined map[ResolvedPath]ClaudeTranscriptEvidence,
	remined []ClaudeTranscriptEvidence,
) {
	if a.evidence == nil {
		return
	}
	var deletes []ResolvedPath
	for path := range cached {
		if _, seen := mined[path]; seen {
			continue
		}
		if pathUnderAnyRoot(path, cfg.Paths) {
			deletes = append(deletes, path)
		}
	}
	if len(remined) == 0 && len(deletes) == 0 {
		return
	}
	_ = a.evidence.SaveClaudeEvidence(ctx, remined, deletes)
}

// linkClaudeTeammates links independently persisted Claude root transcripts.
// A relationship is accepted only when both sides provide one unambiguous
// complete identity, computed over every piece of root evidence this
// discovery knows: the records this run mined, plus whatever survives in the
// persisted evidence cache from an earlier run — so a child discovered in a
// LATER run still finds a spawner an EARLIER run discovered, not only a
// spawner in the same batch. Files that cannot be read or parsed simply
// provide no evidence and do not make discovery fail.
//
// A spawner found only in the persisted cache is pointed at ONLY when it is
// already stored: a parent identifier must never point at a session that is
// neither in this write batch nor already persisted, because the store's own
// FK-orphan guard would otherwise silently drop the child at write time,
// which is worse than leaving it parentless. The extension is one-directional
// — a later child may find an earlier spawner, never the reverse — and never
// creates a cycle: any assignment that would place a session among its own
// ancestors is refused rather than guessed.
func (a *ClaudeAdapter) linkClaudeTeammates(
	ctx context.Context,
	sessions []DiscoveredSession,
	cached, mined map[ResolvedPath]ClaudeTranscriptEvidence,
) {
	rootByPath := make(map[ResolvedPath]int, len(sessions))
	for i := range sessions {
		if sessions[i].ParentUUID == nil {
			rootByPath[sessions[i].SourcePath] = i
		}
	}

	index := buildClaudeSpawnIndex(mergeClaudeEvidence(cached, mined))

	candidates := make(map[SessionID]SessionID, len(index))
	childIndexBySessionID := make(map[SessionID]int, len(index))
	parentIndexBySessionID := make(map[SessionID]int, len(index))

	for _, link := range index {
		childIdx, inBatch := rootByPath[link.Child]
		if !inBatch {
			continue // nothing in this write batch to attach a parent to
		}
		childID := sessions[childIdx].SessionID

		var parentID SessionID
		parentIdx, parentInBatch := rootByPath[link.Parent]
		if parentInBatch {
			parentID = sessions[parentIdx].SessionID
		} else {
			id, ok := claudeSessionIDFromRootPath(link.Parent)
			if !ok || !a.claudeSessionAlreadyStored(ctx, id) {
				continue // unresolvable or unverified: leave the child parentless
			}
			parentID = id
		}
		if parentID == childID {
			continue
		}

		candidates[childID] = parentID
		childIndexBySessionID[childID] = childIdx
		if parentInBatch {
			parentIndexBySessionID[childID] = parentIdx
		}
	}

	cyclic := a.claudeCyclicChildren(ctx, candidates)

	for childID, parentID := range candidates {
		if cyclic[childID] {
			continue
		}
		childIdx := childIndexBySessionID[childID]
		sessions[childIdx].ParentUUID = &parentID
		if parentIdx, ok := parentIndexBySessionID[childID]; ok {
			sessions[parentIdx].SubagentPaths = append(sessions[parentIdx].SubagentPaths, sessions[childIdx].SourcePath)
		}
	}
}

// mineClaudeRootTranscript reads one root transcript ONCE and derives every
// fact discovery takes from its content: whether it holds a conversation, the
// teammate identity it declares, the teammates it spawned, and the display
// hints. One read replaces the three separate passes over the same bytes.
//
// The second result reports whether the record may be cached. An unreadable
// file produces no durable fact, so it is mined again on the next discovery.
func (a *ClaudeAdapter) mineClaudeRootTranscript(path ResolvedPath, info os.FileInfo) (ClaudeTranscriptEvidence, bool) {
	evidence := newClaudeEvidence(path, ClaudeEvidenceScopeRoot, info)

	data, err := a.fs.ReadFile(path.String())
	if err != nil {
		// An unreadable file fails open, exactly as the conversation check does,
		// so discovery can surface it instead of discarding it silently. It
		// carries no evidence, so the rule declares it unknown, which is the
		// visible answer.
		evidence.HasConversationRecord = true
		evidence.Origin, evidence.Signal = sessionorigin.Classify(sessionorigin.Evidence{})
		return evidence, false
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, defaults.ScannerInitBuf), defaults.ScannerMaxLine)

	var identity *ClaudeTeammateIdentity
	var invalidIdentity, malformed, conversation bool
	var validRecords, lineNumber int
	var hints claudeSessionHints
	// The origin captures ride this same scan. Like the identity capture above
	// them they read every line, because a harness is free to record the launch
	// fields on any record; the hint limit below bounds the display hints alone
	// and has never bounded identity.
	var entrypoint, promptSource, firstUserText string
	var haveFirstUserText bool

	for scanner.Scan() {
		raw := scanner.Bytes()
		if lineNumber < claudeHintLineLimit && !hints.complete() {
			applyClaudeHints(&hints, raw)
		}
		lineNumber++

		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var value map[string]any
		if json.Unmarshal(line, &value) != nil {
			// A record this build cannot parse fails open.
			malformed = true
			continue
		}
		validRecords++
		if recordType, present := value["type"]; present {
			text, isText := recordType.(string)
			switch {
			case !isText:
				malformed = true
			case claudeRecordType(text).isConversation():
				conversation = true
			}
		}

		_, hasTeam := value["teamName"]
		_, hasName := value["agentName"]
		if hasTeam || hasName {
			team, teamOK := value["teamName"].(string)
			name, nameOK := value["agentName"].(string)
			if !teamOK || !nameOK || team == "" || name == "" {
				invalidIdentity = true
			} else {
				candidate := ClaudeTeammateIdentity{Team: team, Name: name}
				if identity == nil {
					identity = &candidate
				} else if *identity != candidate {
					invalidIdentity = true
				}
			}
		}
		if entrypoint == "" {
			entrypoint, _ = value["entrypoint"].(string)
		}
		if promptSource == "" {
			promptSource, _ = value["promptSource"].(string)
		}
		if !haveFirstUserText {
			// The first REAL user record decides, not the first user record.
			// Claude Code opens a locally run slash command with a caveat
			// record, and that record would otherwise become the transcript's
			// first user text and carry no signal at all. Leading scaffolding
			// is read past; the first record that is not scaffolding stops the
			// search, so nothing after it is skipped.
			if text, ok := claudeUserRecordText(value); ok && !skipClaudeInjectedOnlyUserRecord(text) {
				firstUserText = text
				haveFirstUserText = true
			}
		}
		if spawn, ok := claudeTeammateSpawn(value); ok {
			evidence.Spawns = append(evidence.Spawns, spawn)
		}
	}

	if !invalidIdentity {
		evidence.Identity = identity
	}
	evidence.HasConversationRecord = conversation || malformed || scanner.Err() != nil || validRecords == 0
	evidence.Title = hints.title
	evidence.Branch = hints.branch
	evidence.CWD = hints.cwd
	// Mining runs before linking, so a root has no known parent yet. A root that
	// linking later attaches to a spawner already declares the identity that
	// makes it agent-driven, so nothing is lost by asking the rule here.
	evidence.Origin, evidence.Signal = sessionorigin.Classify(
		ClaudeOriginEvidence(evidence.Identity, entrypoint, promptSource, firstUserText, false),
	)
	return evidence, true
}

// ClaudeOriginEvidence assembles the origin evidence for one mined transcript.
// identity is the value the scan already collected; entrypoint, promptSource and
// firstUserText are captured in that same pass. hasParent is supplied by the
// caller because it is Peasant's discovery state rather than harness content.
func ClaudeOriginEvidence(
	identity *ClaudeTeammateIdentity,
	entrypoint, promptSource, firstUserText string,
	hasParent bool,
) sessionorigin.Evidence {
	evidence := sessionorigin.Evidence{
		HasParent:     hasParent,
		Entrypoint:    entrypoint,
		PromptSource:  promptSource,
		FirstUserText: firstUserText,
	}
	if identity != nil {
		evidence.TeamName = identity.Team
		evidence.AgentName = identity.Name
	}
	return evidence
}

// claudeInjectedOnlyUserWrappers is the closed set of Claude Code wrappers that
// open a user-role record the HARNESS wrote, containing nothing a person typed
// and nothing an agent authored. A run of these at the head of a transcript is
// scaffolding in front of the real opening record, so the origin capture reads
// past them (see skipClaudeInjectedOnlyUserRecord).
//
// The names come from the redact wrapper catalog. No markup literal is spelled
// here, so a harness renaming a wrapper stays one upstream change.
//
// Why each member is in the set:
//
//   - LocalCommandCaveat: the fixed notice Claude Code emits before a locally
//     run slash command ("the messages below were generated by the user while
//     running local commands"). It is boilerplate the harness composes; the
//     person's actual action is the command wrapper that follows it. This is
//     the record that motivated reading past the head at all.
//   - LocalCommandStdout and LocalCommandStderr: captured output of a local
//     command. Program output, echoed into the turn by the harness.
//   - SystemReminder: harness-composed reminders and notes injected into a user
//     turn. Never typed, never authored by an agent.
//   - TaskNotification: the harness's own report that a background task changed
//     state. Machine-generated status, not authorship by anyone.
//   - EnvironmentContext: a harness dump of the working environment.
//
// Why the near neighbours are NOT in the set, since a wrong member here is a
// misclassification with no other symptom:
//
//   - CommandName, CommandMessage and CommandArgs are the person's action. A
//     command wrapper IS the evidence the rule is looking for; skipping one
//     would delete the very signal this change exists to reach.
//   - TeammateMessage and AgentMessage carry agent authorship. Skipping either
//     would let a later, human-looking record decide a session an agent in fact
//     opened -- the permissive failure, which hides a person's view of who
//     started what.
//   - UserQuery wraps the person's own prose.
//   - SystemContext, RecommendedPlugins and UserAction belong to other
//     harnesses in the same catalog. This skip runs only inside the Claude
//     adapter, so including them would add members no Claude transcript can
//     produce and no test can exercise.
var claudeInjectedOnlyUserWrappers = []string{
	redact.WrapperLocalCommandCaveat,
	redact.WrapperLocalCommandStdout,
	redact.WrapperLocalCommandStderr,
	redact.WrapperSystemReminder,
	redact.WrapperTaskNotification,
	redact.WrapperEnvironmentContext,
}

// skipClaudeInjectedOnlyUserRecord reports whether one user-record text is
// harness scaffolding that the origin capture should read past rather than hand
// to the rule.
//
// Only a LEADING run is ever skipped: the caller stops at the first record this
// returns false for, so an injected record appearing AFTER the real opening
// record is never reached, let alone skipped. A transcript whose user records
// are all injected yields no first user text at all, which is the honest answer
// -- the rule then reaches no content signal and declares the origin unknown.
func skipClaudeInjectedOnlyUserRecord(text string) bool {
	opening := strings.TrimLeft(text, " \t\r\n")
	for _, name := range claudeInjectedOnlyUserWrappers {
		if sessionorigin.OpensWithTag(opening, name) {
			return true
		}
	}
	return false
}

// claudeUserRecordText returns the text of one user-role record. The second
// result says whether the record was a user-role record at all, so the caller
// can stop at the first one even when that first one carries no text.
func claudeUserRecordText(value map[string]any) (string, bool) {
	if recordType, ok := value["type"].(string); !ok || claudeRecordType(recordType) != claudeRecordTypeUser {
		return "", false
	}
	message, ok := value["message"].(map[string]any)
	if !ok {
		return "", false
	}
	if role, ok := message["role"].(string); !ok || role != "user" {
		return "", false
	}
	switch content := message["content"].(type) {
	case string:
		return content, true
	case []any:
		var builder strings.Builder
		for _, block := range content {
			fields, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if kind, ok := fields["type"].(string); !ok || kind != claudeContentBlockText {
				continue
			}
			text, ok := fields["text"].(string)
			if !ok {
				continue
			}
			if builder.Len() > 0 {
				builder.WriteByte('\n')
			}
			builder.WriteString(text)
		}
		return builder.String(), true
	default:
		return "", true
	}
}

// mineClaudeSubagentTranscript checks one subagent transcript for a
// conversation record. A subagent file carries no teammate identity and no
// display hints, so the streaming check that stops at the first conversation
// record stays the cheapest way to read it.
func (a *ClaudeAdapter) mineClaudeSubagentTranscript(path ResolvedPath, info os.FileInfo) ClaudeTranscriptEvidence {
	evidence := newClaudeEvidence(path, ClaudeEvidenceScopeSubagent, info)
	evidence.HasConversationRecord = a.hasClaudeConversationRecord(path.String())
	// A subagent transcript is by definition the child of a root, which is the
	// first step of the rule. The rule is asked rather than answered for it, so
	// that step 1 keeps exactly one definition and a change to it reaches this
	// path too.
	evidence.Origin, evidence.Signal = sessionorigin.Classify(sessionorigin.Evidence{HasParent: true})
	return evidence
}

// newClaudeEvidence starts a record keyed on the file that produced it.
func newClaudeEvidence(path ResolvedPath, scope ClaudeEvidenceScope, info os.FileInfo) ClaudeTranscriptEvidence {
	return ClaudeTranscriptEvidence{
		SourcePath:      path,
		Scope:           scope,
		ModTimeUnixNano: info.ModTime().UnixNano(),
		SizeBytes:       info.Size(),
	}
}

// hasClaudeConversationRecord reports whether path contains at least one native
// user or assistant record. Claude stores summary and file-history records in
// session-shaped JSONL files, but neither represents a renderable conversation.
// Unreadable, empty, or malformed files fail open so discovery can surface them
// for diagnostics rather than silently discarding a future transcript format.
func (a *ClaudeAdapter) hasClaudeConversationRecord(path string) bool {
	var reader io.Reader
	var closeFile func() error
	if opener, ok := a.fs.(claudeFileOpener); ok {
		file, err := opener.Open(path)
		if err != nil {
			return true
		}
		reader = file
		closeFile = file.Close
	} else {
		data, err := a.fs.ReadFile(path)
		if err != nil {
			return true
		}
		reader = bytes.NewReader(data)
	}
	if closeFile != nil {
		defer closeFile()
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, defaults.ScannerInitBuf), defaults.ScannerMaxLine)
	var validRecords int
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var value struct {
			Type claudeRecordType `json:"type"`
		}
		if json.Unmarshal(line, &value) != nil {
			return true
		}
		validRecords++
		if value.Type.isConversation() {
			return true
		}
	}
	return scanner.Err() != nil || validRecords == 0
}

// claudeTeammateSpawn recognizes only the top-level native tool result. Tool
// output may contain arbitrary JSON-shaped text, so recursive matching would
// let an unrelated tool payload forge discovery evidence.
func claudeTeammateSpawn(value map[string]any) (ClaudeTeammateIdentity, bool) {
	result, ok := value["toolUseResult"].(map[string]any)
	if !ok {
		return ClaudeTeammateIdentity{}, false
	}
	if status, _ := result["status"].(string); status != "teammate_spawned" {
		return ClaudeTeammateIdentity{}, false
	}
	team, teamOK := result["team_name"].(string)
	name, nameOK := result["name"].(string)
	if !teamOK || !nameOK || team == "" || name == "" {
		return ClaudeTeammateIdentity{}, false
	}
	return ClaudeTeammateIdentity{Team: team, Name: name}, true
}

// claudeSessionHints holds metadata extracted from the first few JSONL lines.
type claudeSessionHints struct {
	title  string
	branch string
	cwd    string
}

// complete reports whether every hint already has a value, so the caller can
// stop looking at further lines.
func (h claudeSessionHints) complete() bool {
	return h.title != "" && h.branch != "" && h.cwd != ""
}

// applyClaudeHints takes the session title, the git branch, and the working
// directory from one raw JSONL line. Each hint keeps the first value it finds.
// A line this build cannot parse contributes nothing.
func applyClaudeHints(hints *claudeSessionHints, raw []byte) {
	var line claudeJSONLLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return
	}
	// Grab branch and CWD from the first line that has them.
	if hints.branch == "" && line.GitBranch != "" {
		hints.branch = line.GitBranch
	}
	if hints.cwd == "" && line.CWD != "" {
		hints.cwd = line.CWD
	}
	// Derive the display title from the first user message that carries real
	// user prose. The shared redaction-free pipeline strips Claude's own markup
	// (system-reminder blocks, command and local-command wrappers, skill
	// bodies) and caps the length, so the raw markup no longer leaks into the
	// title. A turn that cleans to nothing, or that cannot be cleaned safely,
	// leaves the hint empty and the next user line becomes the candidate.
	if hints.title == "" && line.Type == claudeRecordTypeUser && line.Message.Role == "user" {
		if text := extractTextFromContent(line.Message.Content); text != "" {
			hints.title = simpleTitle(text, defaults.HarnessClaudeCode)
		}
	}
}

// extractTextFromContent extracts plain text from a Claude message content field.
// Content can be a JSON string or an array of content blocks.
func extractTextFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	// Try array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return strings.TrimSpace(b.Text)
			}
		}
	}
	return ""
}

// ExtractMetadata reads the JSONL transcript and builds UnifiedMetadata.
func (a *ClaudeAdapter) ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	data, err := a.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("ExtractMetadata: read %q: %w", session.SourcePath, err)
	}

	meta := NewUnifiedMetadata()
	meta.SessionID = session.SessionID
	meta.ParentUUID = session.ParentUUID
	meta.ModelHarness = HarnessClaudeCode
	meta.Source = SourceInfo{
		FilePath: string(session.SourcePath),
		Format:   SourceFormatJSONL,
	}

	var (
		firstLine      claudeJSONLLine
		hasFirst       bool
		firstTimestamp string // earliest non-empty timestamp across all lines
		lastTimestamp  string // latest non-empty timestamp across all lines
		lineNum        int
		turnCount      int
		toolCount      int
		tokensIn       int
		tokensOut      int
	)

	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Set scanner buffer to handle large lines. While the entire file is already
	// in memory via ReadFile(), bufio.Scanner has an internal line-length limit
	// that defaults to 64KiB. The 10 MiB limit prevents token scan errors on
	// assistant messages with very large tool outputs.
	buf := make([]byte, defaults.ScannerInitBuf)
	scanner.Buffer(buf, defaults.ScannerMaxLine)

	for scanner.Scan() {
		lineNum++
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}

		var line claudeJSONLLine
		if err := json.Unmarshal(raw, &line); err != nil {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "parse_error",
				Location:    fmt.Sprintf("line %d", lineNum),
				Message:     fmt.Sprintf("failed to parse JSONL line: %v", err),
				Remediation: "Check the transcript file for corruption or truncation at the indicated line.",
			})
			continue
		}

		if !hasFirst {
			firstLine = line
			hasFirst = true
		}

		// Track earliest and latest non-empty timestamps across all lines.
		if line.Timestamp != "" {
			if firstTimestamp == "" {
				firstTimestamp = line.Timestamp
			}
			lastTimestamp = line.Timestamp
		}

		// Count turns.
		if line.Type.isConversation() {
			turnCount++
		}

		// Count tool calls and accumulate tokens from assistant messages.
		if line.Type == claudeRecordTypeAssistant && len(line.Message.Content) > 0 {
			// content may be a string or an array of blocks.
			// Only count tool_use blocks from arrays.
			var blocks []contentBlock
			if err := json.Unmarshal(line.Message.Content, &blocks); err == nil {
				for _, b := range blocks {
					if b.Type == "tool_use" {
						toolCount++
					}
				}
			}

			// Capture model from first assistant message seen.
			if line.Message.Model != "" && meta.Model == "" {
				mid, merr := NewModelID(line.Message.Model)
				if merr == nil {
					meta.Model = mid
				}
			}

			// Accumulate token usage from assistant messages.
			// input_tokens in Claude's JSONL is only the non-cached portion;
			// the bulk of input tokens are in cache_creation and cache_read fields.
			if line.Message.Usage != nil {
				tokensIn += line.Message.Usage.InputTokens +
					line.Message.Usage.CacheCreationInputTokens +
					line.Message.Usage.CacheReadInputTokens
				tokensOut += line.Message.Usage.OutputTokens
			}
		}
	}

	if err := scanner.Err(); err != nil {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "read_error",
			Location:    fmt.Sprintf("line %d", lineNum),
			Message:     fmt.Sprintf("scanner error reading transcript: %v", err),
			Remediation: "Verify the transcript file is not corrupted or truncated.",
		})
	}

	// Populate fields from parsed lines.
	if hasFirst {
		meta.Version = firstLine.Version

		// Timestamps: use earliest/latest non-empty timestamps found across
		// all lines, not just the first/last line positions. Some sessions
		// have a first line (e.g. init/system) without a timestamp field.
		startMs := parseTimestampMillis(firstTimestamp)
		endMs := parseTimestampMillis(lastTimestamp)
		ingested := time.Now().UnixMilli()
		meta.Timestamp = TimestampInfo{
			Start:    startMs,
			End:      endMs,
			Ingested: &ingested,
		}

		// Duration.
		if startMs > 0 && endMs >= startMs {
			meta.Stats.DurationMs = endMs - startMs
		}

		// Resolve git metadata using the cwd from the first line.
		cwd := firstLine.CWD
		if cwd == "" {
			cwd = filepath.Dir(string(session.SourcePath))
		}

		// If cwd is under ~/.claude/projects/, try to decode the actual project path.
		// Claude project dirs encode the real path by replacing "/" with "-".
		if decoded := a.decodeClaudeProjectDir(cwd); decoded != "" {
			cwd = decoded
		}

		remoteURL, remoteErr := a.git.RemoteURL(ctx, cwd)
		// If direct remote check fails, walk up parent directories to find one.
		// This ensures sessions from decoded slug paths (which may point to a
		// subdirectory) still resolve the correct git remote for project grouping.
		if (remoteErr != nil || remoteURL == "") && a.git != nil {
			if walkedRemote, _, walkErr := a.git.WalkUpRemoteURL(ctx, cwd); walkErr == nil && walkedRemote != "" {
				remoteURL = walkedRemote
				remoteErr = nil
			}
		}
		branchStr, branchErr := a.git.Branch(ctx, cwd)
		worktreeStr, worktreeErr := a.git.Worktree(ctx, cwd)
		trackingStr, trackingErr := a.git.TrackingBranch(ctx, cwd)

		// Build GitContext — all fields are nullable.
		gitInfo := GitContext{}

		// Prefer gitBranch from JSONL over resolved branch.
		branch := firstLine.GitBranch
		if branch == "" && branchErr == nil && branchStr != "" {
			branch = branchStr
		}
		if branch != "" {
			b := branch
			gitInfo.Branch = &b
		}

		if remoteErr == nil && remoteURL != "" {
			r := remoteURL
			gitInfo.Remote = &r
		}

		if worktreeErr == nil && worktreeStr != "" {
			w := worktreeStr
			gitInfo.Worktree = &w
		}

		if trackingErr == nil && trackingStr != "" {
			tr := trackingStr
			gitInfo.Tracking = &tr
		}

		meta.Git = gitInfo

		// Store the real working directory for context-aware slug redaction.
		meta.CWD = cwd

		// ProjectInfo.
		projectPath := worktreeStr
		if projectPath == "" {
			projectPath = cwd
		}

		projectHash, hostSlug, err := DeriveProjectIdentifiersWithGit(ctx, a.salt, a.git, remoteURL, projectPath)
		if err != nil {
			// DeriveProjectIdentifiersWithGit should not fail for valid paths,
			// but fall back to zero-value hash if it does.
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "derive_identity_error",
				Location:    fmt.Sprintf("session %s", session.SessionID),
				Message:     fmt.Sprintf("failed to derive project identifiers: %v", err),
				Remediation: "Check git remote URL or working directory path.",
			})
		} else {
			meta.HostSlug = hostSlug
		}

		// Derive project name from git remote (repo name) when available,
		// so that worktrees and subdirectories show the repository name
		// (e.g., "widget-service") rather than the worktree/branch
		// name (e.g., "feature-push").
		projectName := RepoNameFromRemote(remoteURL)
		if projectName == "" {
			projectName = filepath.Base(projectPath)
		}

		meta.Project = ProjectInfo{
			Hash:     projectHash,
			FilePath: projectPath,
			Name:     projectName,
		}
	}

	meta.Stats.TurnCount = turnCount
	meta.Stats.ToolCallCount = toolCount
	meta.Stats.SubagentCount = len(session.SubagentPaths)
	meta.Stats.TokensIn = tokensIn
	meta.Stats.TokensOut = tokensOut

	// Build subagent refs from SubagentPaths.
	for _, sp := range session.SubagentPaths {
		filename := filepath.Base(string(sp))
		subIDStr := strings.TrimSuffix(filename, defaults.ExtJSONL.String())
		subSID, err := NewSessionID(subIDStr)
		if err != nil {
			continue
		}
		meta.Subagents = append(meta.Subagents, SubagentRef{
			SessionID:  subSID,
			ParentUUID: session.SessionID,
		})
	}

	return &meta, nil
}

// claudeProjectsDirSegment is the path segment that identifies a directory as a
// Claude project memory directory. Used to detect when the CWD fallback points
// at a Claude internal directory rather than the actual project.
const claudeProjectsDirSegment = "/.claude/projects/"

// decodeClaudeProjectDir checks if cwd is under a Claude project memory directory
// and attempts to decode the encoded directory name back to the real filesystem path.
//
// Claude encodes project paths by replacing "/" with "-" and prepending "-":
//
//	/home/user/dev/my-project → -home-user-dev-my-project
//
// The encoding is ambiguous (dashes in directory names vs path separators), so this
// method uses a greedy filesystem-based decoder: starting from root, it tries each
// segment and checks if a directory exists. If not, it merges segments with dashes.
//
// Returns the decoded path if it exists on the filesystem, or empty string if
// decoding fails or the path does not exist.
func (a *ClaudeAdapter) decodeClaudeProjectDir(cwd string) string {
	idx := strings.Index(cwd, claudeProjectsDirSegment)
	if idx < 0 {
		return ""
	}

	// Extract the portion after "/.claude/projects/".
	// cwd may be the project dir itself or a parent of the JSONL file.
	after := cwd[idx+len(claudeProjectsDirSegment):]
	if after == "" {
		return ""
	}

	// The basename may have a trailing slash or additional path components
	// (e.g., if cwd is the dir containing the JSONL). Take only the first
	// path component after "/.claude/projects/".
	encoded := strings.SplitN(after, "/", 2)[0]

	return DecodeClaudeSlug(encoded, a.dirExists)
}

// dirExists checks if path exists as a directory using the adapter's FileSystem.
func (a *ClaudeAdapter) dirExists(path string) bool {
	info, err := a.fs.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// parseTimestampMillis parses an ISO 8601 / RFC 3339 timestamp and returns Unix milliseconds.
// Returns 0 on parse failure.
func parseTimestampMillis(ts string) int64 {
	if ts == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		// Try RFC3339Nano as fallback.
		t, err = time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}
