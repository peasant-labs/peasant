package harnesslayout

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"path"
	"strings"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ToolCursorCLI is the chat store of the Cursor CLI agent (cursor-agent). The
// agent-transcripts JSONL that the CLI also writes is ingested by the cursor
// harness and is not part of this layout.
const ToolCursorCLI Tool = "cursor-cli"

const (
	cursorCLIMetaFile      = "meta.json"
	cursorCLIPromptHistory = "prompt_history.json"
	// cursorCLIAgentRecordKey is the meta row that holds the agent record.
	cursorCLIAgentRecordKey = "0"
	// cursorCLIDefaultName is the name the CLI gives every new agent.
	cursorCLIDefaultName = "New Agent"
	cursorCLIInjectedTag = "<user_info>"
)

var (
	cursorCLIChatStore = Artifact{
		Name:    "chat-store",
		Pattern: "chats/*/*/store.db",
		Format:  FormatSQLite,
		Role:    RoleTranscript,
		Description: "one SQLite store per chat under a bucket named by the hex MD5 of the workspace path: " +
			`meta(key, value) holds the agent record as hex-encoded JSON under key "0"; ` +
			"blobs(id, data) holds content-addressed JSON messages and protobuf turn-graph nodes",
	}
	cursorCLIChatMeta = Artifact{
		Name:        "chat-meta",
		Pattern:     "chats/*/*/" + cursorCLIMetaFile,
		Format:      FormatJSON,
		Role:        RoleMetadata,
		Description: "chat sidecar: schemaVersion, title, createdAtMs, updatedAtMs, hasConversation, isSubagent, cwd",
	}
	cursorCLIChatPrompts = Artifact{
		Name:        "prompt-history",
		Pattern:     "chats/*/*/" + cursorCLIPromptHistory,
		Format:      FormatJSON,
		Role:        RoleAuxiliary,
		Description: "array of the chat's recent user prompt strings",
	}
	cursorCLIACPStore = Artifact{
		Name:        "acp-store",
		Pattern:     "acp-sessions/*/store.db",
		Format:      FormatSQLite,
		Role:        RoleTranscript,
		Description: "one store per Agent Client Protocol session, in the chat-store format",
	}
	cursorCLIACPMeta = Artifact{
		Name:        "acp-meta",
		Pattern:     "acp-sessions/*/" + cursorCLIMetaFile,
		Format:      FormatJSON,
		Role:        RoleMetadata,
		Description: "ACP session sidecar: schemaVersion, cwd, title",
	}
)

type cursorCLIFamily struct {
	store, meta, prompts Artifact
}

var cursorCLIFamilies = []cursorCLIFamily{
	{store: cursorCLIChatStore, meta: cursorCLIChatMeta, prompts: cursorCLIChatPrompts},
	{store: cursorCLIACPStore, meta: cursorCLIACPMeta},
}

type cursorCLIProbe struct{}

var _ Probe = cursorCLIProbe{}

func (cursorCLIProbe) Discover(_ context.Context, src Source) ([]SessionRef, error) {
	var refs []SessionRef
	for _, family := range cursorCLIFamilies {
		matches, err := fs.Glob(src.FS, family.store.Pattern)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			dir := path.Dir(match)
			ref := SessionRef{ID: path.Base(dir), Paths: []string{match}}
			for _, sidecar := range []Artifact{family.meta, family.prompts} {
				if sidecar.Name == "" {
					continue
				}
				rel := path.Join(dir, path.Base(sidecar.Pattern))
				if _, err := fs.Stat(src.FS, rel); err == nil {
					ref.Paths = append(ref.Paths, rel)
				}
			}
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

func (cursorCLIProbe) Capture(ctx context.Context, src Source, ref SessionRef) (Capture, error) {
	if len(ref.Paths) == 0 {
		return Capture{}, errors.New("session has no store")
	}
	family := cursorCLIFamilies[0]
	for _, candidate := range cursorCLIFamilies {
		if ok, _ := path.Match(candidate.store.Pattern, ref.Paths[0]); ok {
			family = candidate
		}
	}
	meta := Metadata{SessionID: ref.ID}
	var record cursorCLIAgentRecord
	storeShape, err := cursorCLIShapeStore(ctx, src, ref.Paths[0], family.store, &meta, &record)
	if err != nil {
		return Capture{}, err
	}
	shapes := []ArtifactShape{storeShape}
	var sidecar cursorCLISidecar
	for _, rel := range ref.Paths[1:] {
		switch path.Base(rel) {
		case cursorCLIMetaFile:
			shapes = append(shapes, cursorCLIShapeSidecar(src, rel, family.meta, &sidecar))
		case cursorCLIPromptHistory:
			shapes = append(shapes, cursorCLIShapeSidecar(src, rel, family.prompts, nil))
		}
	}

	if record.id != "" {
		meta.SessionID = record.id
	}
	meta.Title = cursorFirstNonBlank(sidecar.title, record.name)
	meta.ProjectPath = cursorFirstNonBlank(sidecar.cwd, record.workspace)
	meta.Observe(sidecar.created, "")
	meta.Observe(sidecar.updated, "")
	return Capture{Metadata: meta, Shapes: shapes}, nil
}

type cursorCLIAgentRecord struct {
	id, name, workspace string
}

func cursorCLIShapeStore(ctx context.Context, src Source, rel string, artifact Artifact, meta *Metadata, record *cursorCLIAgentRecord) (ArtifactShape, error) {
	conn, err := cursorOpenReadOnly(ctx, src, rel)
	if err != nil {
		return ArtifactShape{}, err
	}
	defer conn.Close()
	rec := NewShapeRecorder(artifact, rel)
	tables, err := cursorShapeTables(conn, rec, "meta", "blobs")
	if err != nil {
		return ArtifactShape{}, err
	}
	if tables["meta"] {
		err := sqlitex.ExecuteTransient(conn,
			`SELECT key, CASE WHEN length(value) <= ? THEN CAST(value AS TEXT) END FROM meta ORDER BY key LIMIT ?`,
			&sqlitex.ExecOptions{Args: []any{cursorMaxValueBytes, cursorMaxRows}, ResultFunc: func(stmt *sqlite.Stmt) error {
				raw, ok := cursorBoundedValue(stmt, 1)
				if !ok {
					rec.shape.Malformed++
					return nil
				}
				value, ok := cursorDecode(rec, cursorCLIMetaJSON(raw))
				if !ok {
					return nil
				}
				cursorRecord(rec, "meta", "$.meta.value", value)
				if stmt.ColumnText(0) == cursorCLIAgentRecordKey {
					cursorCLIObserveAgent(value, meta, record)
				}
				return nil
			}})
		if err != nil {
			return ArtifactShape{}, err
		}
	}
	if tables["blobs"] {
		err := sqlitex.ExecuteTransient(conn,
			`SELECT CASE WHEN length(data) <= ? THEN data END FROM blobs ORDER BY rowid LIMIT ?`,
			&sqlitex.ExecOptions{Args: []any{cursorMaxValueBytes, cursorMaxRows}, ResultFunc: func(stmt *sqlite.Stmt) error {
				raw, ok := cursorBoundedValue(stmt, 0)
				if !ok {
					cursorRecord(rec, "oversized", "", nil)
					return nil
				}
				cursorCLIObserveBlob(rec, raw, meta)
				return nil
			}})
		if err != nil {
			return ArtifactShape{}, err
		}
	}
	return rec.Shape(), nil
}

// cursorCLIMetaJSON returns the agent record JSON. The CLI writes it
// hex-encoded; older releases were observed writing plain JSON.
func cursorCLIMetaJSON(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return trimmed
	}
	decoded, err := hex.DecodeString(string(trimmed))
	if err != nil {
		return trimmed
	}
	return decoded
}

// cursorCLIObserveAgent reads the identifying fields of the agent record. The
// record also carries blobEncryptionKey, which is never read.
func cursorCLIObserveAgent(value any, meta *Metadata, record *cursorCLIAgentRecord) {
	record.id = StringField(value, "agentId")
	if name := StringField(value, "name"); name != cursorCLIDefaultName {
		record.name = name
	}
	record.workspace = StringField(value, "workspacePath")
	meta.Observe(TimeField(value, "createdAt"), StringField(value, "lastUsedModel"))
}

// cursorCLIObserveBlob records one blob. JSON blobs are messages in the
// Vercel AI SDK shape; any other blob is a protobuf turn-graph node and is
// counted without decoding.
func cursorCLIObserveBlob(rec *ShapeRecorder, raw []byte, meta *Metadata) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		cursorRecord(rec, "binary", "", nil)
		return
	}
	value, err := decodeValue(trimmed)
	if err != nil {
		cursorRecord(rec, "binary", "", nil)
		return
	}
	role := StringField(value, "role")
	cursorRecord(rec, cursorKind(role, "no-role"), "$.blobs.data", value)
	meta.Observe(TimeField(value, "createdAt"), StringField(value, "providerOptions", "cursor", "modelName"))
	content := Field(value, "content")
	switch role {
	case "user":
		if !cursorCLIInjected(content) {
			meta.UserTurns++
		}
	case "assistant":
		meta.AssistantMsgs++
		for _, part := range cursorArray(content) {
			if StringField(part, "type") == "tool-call" {
				meta.ToolCalls++
			}
		}
	}
}

// cursorCLIInjected reports a user message that is the environment preamble
// the CLI injects, not a turn the user typed.
func cursorCLIInjected(content any) bool {
	if text, ok := content.(string); ok {
		return strings.Contains(text, cursorCLIInjectedTag)
	}
	for _, part := range cursorArray(content) {
		if strings.Contains(StringField(part, "text"), cursorCLIInjectedTag) {
			return true
		}
	}
	return false
}

type cursorCLISidecar struct {
	title, cwd       string
	created, updated time.Time
}

func cursorCLIShapeSidecar(src Source, rel string, artifact Artifact, sidecar *cursorCLISidecar) ArtifactShape {
	rec := NewShapeRecorder(artifact, rel)
	file, err := src.FS.Open(rel)
	if err != nil {
		rec.shape.Malformed++
		return rec.Shape()
	}
	defer file.Close()
	var each func(any) []any
	if sidecar == nil {
		each = cursorArray
	}
	document, err := ShapeJSON(file, rec, each, nil)
	if err != nil || sidecar == nil {
		return rec.Shape()
	}
	sidecar.title = StringField(document, "title")
	sidecar.cwd = StringField(document, "cwd")
	sidecar.created = TimeField(document, "createdAtMs")
	sidecar.updated = TimeField(document, "updatedAtMs")
	return rec.Shape()
}

func cursorArray(value any) []any {
	array, _ := value.([]any)
	return array
}

func cursorFirstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func init() {
	Register(Layout{
		Tool:        ToolCursorCLI,
		DisplayName: "Cursor CLI chat store",
		Sessions:    "One chat directory per session, holding a SQLite store.db and JSON sidecars.",
		Roots: []Root{
			{Path: "{home}/.cursor"},
			{OS: []OS{OSLinux}, Path: "{config}/cursor"},
		},
		Artifacts: []Artifact{cursorCLIChatStore, cursorCLIChatMeta, cursorCLIChatPrompts, cursorCLIACPStore, cursorCLIACPMeta},
		Sources: []string{
			"https://cursor.com/docs/cli/overview",
			"https://docs.rs/crate/txcript/latest/source/docs/formats/cursor.md",
			"https://vshulcz.github.io/deja-vu/registry/cursor.html",
			"https://github.com/marcus/sidecar/blob/main/docs/implemented/cursor-cli-adapter.md",
			"https://github.com/jnarowski/agentcmd/blob/main/.agent/docs/cursor-agent.md",
			"cursor-agent 2026.09.26-dd393fe bundle: state/index.ts, run-store/sqlite-blob-store.js, cursor-config/paths.js",
		},
		Probe: cursorCLIProbe{},
	})
}
