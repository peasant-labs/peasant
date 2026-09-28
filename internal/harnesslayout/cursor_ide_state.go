package harnesslayout

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"path"
	"strings"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ToolCursorIDEState is the composer store of the Cursor IDE: the VS Code
// state databases under the Cursor User directory. The agent-transcripts
// JSONL that the IDE also writes is ingested by the cursor harness and is not
// part of this layout.
const ToolCursorIDEState Tool = "cursor-ide-state"

// The probes query only these key families. Every other key, including the
// cursorAuth/* credentials in ItemTable, is never selected.
const (
	cursorIDEComposerPrefix = "composerData:"
	cursorIDEBubblePrefix   = "bubbleId:"
	cursorIDEWorkspaceDir   = "workspaceStorage"
	cursorIDEWorkspaceFile  = "workspace.json"
	// cursorIDEPlaceholderModel is the model picker's "default" entry, which
	// names no model.
	cursorIDEPlaceholderModel = "default"
)

// cursorIDEAuxPrefixes are per-composer key families the probe counts by key
// without reading their values.
var cursorIDEAuxPrefixes = []string{"checkpointId:", "messageRequestContext:", "codeBlockDiff:", "composerVirtualRowHeights:"}

// cursorIDEWorkspaceKeys are the ItemTable keys of a workspace database that
// list the composers opened in that workspace; newer builds renamed the first
// to the second.
var cursorIDEWorkspaceKeys = []string{"composer.composerData", "composer.composerHeaders"}

var (
	cursorIDEGlobalState = Artifact{
		Name:    "global-state",
		Pattern: "globalStorage/state.vscdb",
		Format:  FormatSQLite,
		Role:    RoleTranscript,
		Description: "global SQLite key-value store: cursorDiskKV holds composerData:<composerId> (session document) " +
			"and bubbleId:<composerId>:<bubbleId> (one message) as JSON; newer builds add a composerHeaders table",
	}
	cursorIDEWorkspaceState = Artifact{
		Name:        "workspace-state",
		Pattern:     cursorIDEWorkspaceDir + "/*/state.vscdb",
		Format:      FormatSQLite,
		Role:        RoleIndex,
		Description: "per-workspace SQLite store: ItemTable composer.composerData or composer.composerHeaders lists the workspace's composers",
	}
	cursorIDEWorkspaceFolder = Artifact{
		Name:        "workspace-folder",
		Pattern:     cursorIDEWorkspaceDir + "/*/" + cursorIDEWorkspaceFile,
		Format:      FormatJSON,
		Role:        RoleIndex,
		Description: "the folder (or .code-workspace) URI of the workspace hash",
	}
)

type cursorIDEProbe struct{}

var _ Probe = cursorIDEProbe{}

func (cursorIDEProbe) Discover(ctx context.Context, src Source) ([]SessionRef, error) {
	global := cursorIDEGlobalState.Pattern
	if _, err := fs.Stat(src.FS, global); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	conn, err := cursorOpenReadOnly(ctx, src, global)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if !cursorHasTable(conn, "cursorDiskKV") {
		return nil, nil
	}
	var ids []string
	err = cursorKeyRange(conn, `SELECT substr(key, ?) FROM cursorDiskKV WHERE key >= ? AND key < ? ORDER BY key LIMIT ?`,
		[]any{len(cursorIDEComposerPrefix) + 1}, cursorIDEComposerPrefix, func(stmt *sqlite.Stmt) {
			if id := stmt.ColumnText(0); id != "" {
				ids = append(ids, id)
			}
		})
	if err != nil {
		return nil, err
	}
	workspaces := cursorIDEWorkspaceComposers(ctx, src)
	refs := make([]SessionRef, 0, len(ids))
	for _, id := range ids {
		ref := SessionRef{ID: id, Paths: []string{global}}
		if dir, ok := workspaces[id]; ok {
			ref.Paths = append(ref.Paths, cursorIDEWorkspacePaths(src, dir)...)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// cursorIDEWorkspaceComposers maps each composer listed by a workspace
// database to that workspace's directory. An unreadable workspace is skipped.
func cursorIDEWorkspaceComposers(ctx context.Context, src Source) map[string]string {
	out := map[string]string{}
	matches, _ := fs.Glob(src.FS, cursorIDEWorkspaceState.Pattern)
	for _, match := range matches {
		conn, err := cursorOpenReadOnly(ctx, src, match)
		if err != nil {
			continue
		}
		_ = cursorIDEWorkspaceValues(conn, func(_ string, raw []byte) {
			value, err := decodeValue(raw)
			if err != nil {
				return
			}
			for _, composer := range ArrayField(value, "allComposers") {
				if id := StringField(composer, "composerId"); id != "" {
					out[id] = path.Dir(match)
				}
			}
		})
		conn.Close()
	}
	return out
}

func cursorIDEWorkspaceValues(conn *sqlite.Conn, visit func(key string, raw []byte)) error {
	if !cursorHasTable(conn, "ItemTable") {
		return nil
	}
	for _, key := range cursorIDEWorkspaceKeys {
		err := sqlitex.ExecuteTransient(conn,
			`SELECT CASE WHEN length(value) <= ? THEN CAST(value AS TEXT) END FROM ItemTable WHERE key = ?`,
			&sqlitex.ExecOptions{Args: []any{cursorMaxValueBytes, key}, ResultFunc: func(stmt *sqlite.Stmt) error {
				if raw, ok := cursorBoundedValue(stmt, 0); ok {
					visit(key, raw)
				}
				return nil
			}})
		if err != nil {
			return err
		}
	}
	return nil
}

func cursorIDEWorkspacePaths(src Source, dir string) []string {
	var out []string
	for _, name := range []string{path.Base(cursorIDEWorkspaceState.Pattern), cursorIDEWorkspaceFile} {
		rel := path.Join(dir, name)
		if _, err := fs.Stat(src.FS, rel); err == nil {
			out = append(out, rel)
		}
	}
	return out
}

func (cursorIDEProbe) Capture(ctx context.Context, src Source, ref SessionRef) (Capture, error) {
	if len(ref.Paths) == 0 {
		return Capture{}, errors.New("session has no store")
	}
	meta := Metadata{SessionID: ref.ID}
	var hints cursorIDEHints
	global, err := cursorIDEShapeGlobal(ctx, src, ref, &meta, &hints)
	if err != nil {
		return Capture{}, err
	}
	shapes := []ArtifactShape{global}
	paths := ref.Paths[1:]
	if len(paths) == 0 && hints.workspaceID != "" && fs.ValidPath(hints.workspaceID) && !strings.ContainsAny(hints.workspaceID, `/\`) {
		paths = cursorIDEWorkspacePaths(src, path.Join(cursorIDEWorkspaceDir, hints.workspaceID))
	}
	for _, rel := range paths {
		switch path.Base(rel) {
		case cursorIDEWorkspaceFile:
			shapes = append(shapes, cursorIDEShapeFolder(src, rel, &hints))
		default:
			shapes = append(shapes, cursorIDEShapeWorkspace(ctx, src, rel))
		}
	}
	meta.Title = cursorFirstNonBlank(hints.name, hints.headerName)
	meta.ProjectPath = cursorFirstNonBlank(hints.headerPath, hints.folderPath, hints.bubblePath)
	return Capture{Metadata: meta, Shapes: shapes}, nil
}

type cursorIDEHints struct {
	name, headerName                   string
	headerPath, folderPath, bubblePath string
	workspaceID                        string
}

func cursorIDEShapeGlobal(ctx context.Context, src Source, ref SessionRef, meta *Metadata, hints *cursorIDEHints) (ArtifactShape, error) {
	rel := ref.Paths[0]
	conn, err := cursorOpenReadOnly(ctx, src, rel)
	if err != nil {
		return ArtifactShape{}, err
	}
	defer conn.Close()
	rec := NewShapeRecorder(cursorIDEGlobalState, rel)
	tables, err := cursorShapeTables(conn, rec, "cursorDiskKV", "composerHeaders")
	if err != nil {
		return ArtifactShape{}, err
	}
	if !tables["cursorDiskKV"] {
		return ArtifactShape{}, errors.New("global state has no cursorDiskKV table")
	}

	var inline []any
	err = sqlitex.ExecuteTransient(conn,
		`SELECT CASE WHEN length(value) <= ? THEN CAST(value AS TEXT) END FROM cursorDiskKV WHERE key = ?`,
		&sqlitex.ExecOptions{Args: []any{cursorMaxValueBytes, cursorIDEComposerPrefix + ref.ID}, ResultFunc: func(stmt *sqlite.Stmt) error {
			raw, ok := cursorBoundedValue(stmt, 0)
			if !ok {
				rec.shape.Malformed++
				return nil
			}
			document, ok := cursorDecode(rec, raw)
			if !ok {
				return nil
			}
			cursorRecord(rec, "composerData", "$.cursorDiskKV.composerData", document)
			hints.name = StringField(document, "name")
			meta.Observe(TimeField(document, "createdAt"), "")
			meta.Observe(TimeField(document, "lastUpdatedAt"), "")
			cursorIDEObserveModel(meta, StringField(document, "modelConfig", "modelName"))
			inline = ArrayField(document, "conversation")
			return nil
		}})
	if err != nil {
		return ArtifactShape{}, err
	}

	bubbles := 0
	err = cursorKeyRange(conn, `SELECT CASE WHEN length(value) <= ? THEN CAST(value AS TEXT) END FROM cursorDiskKV WHERE key >= ? AND key < ? ORDER BY key LIMIT ?`,
		[]any{cursorMaxValueBytes}, cursorIDEBubblePrefix+ref.ID+":", func(stmt *sqlite.Stmt) {
			bubbles++
			raw, ok := cursorBoundedValue(stmt, 0)
			if !ok {
				cursorRecord(rec, "oversized", "", nil)
				return
			}
			bubble, ok := cursorDecode(rec, raw)
			if !ok {
				return
			}
			cursorRecord(rec, cursorIDEBubbleKind(bubble), "$.cursorDiskKV.bubbleId", bubble)
			cursorIDEObserveBubble(meta, bubble, hints)
		})
	if err != nil {
		return ArtifactShape{}, err
	}
	if bubbles == 0 {
		for _, bubble := range inline {
			cursorIDEObserveBubble(meta, bubble, hints)
		}
	}

	for _, prefix := range cursorIDEAuxPrefixes {
		count := 0
		key := prefix + ref.ID
		err := sqlitex.ExecuteTransient(conn, `SELECT count(*) FROM cursorDiskKV WHERE key = ? OR (key >= ? AND key < ?)`,
			&sqlitex.ExecOptions{Args: []any{key, key + ":", key + ";"}, ResultFunc: func(stmt *sqlite.Stmt) error {
				count = stmt.ColumnInt(0)
				return nil
			}})
		if err != nil {
			return ArtifactShape{}, err
		}
		if count > 0 {
			rec.shape.Records += count
			rec.shape.Kinds[strings.TrimSuffix(prefix, ":")] += count
		}
	}

	if tables["composerHeaders"] {
		// A header table from a build with other columns is shaped by
		// cursorShapeTables and otherwise skipped.
		_ = sqlitex.ExecuteTransient(conn,
			`SELECT workspaceId, CASE WHEN length(value) <= ? THEN CAST(value AS TEXT) END FROM composerHeaders WHERE composerId = ?`,
			&sqlitex.ExecOptions{Args: []any{cursorMaxValueBytes, ref.ID}, ResultFunc: func(stmt *sqlite.Stmt) error {
				hints.workspaceID = stmt.ColumnText(0)
				raw, ok := cursorBoundedValue(stmt, 1)
				if !ok {
					return nil
				}
				header, ok := cursorDecode(rec, raw)
				if !ok {
					return nil
				}
				cursorRecord(rec, "composerHeaders", "$.composerHeaders.value", header)
				hints.headerName = StringField(header, "name")
				hints.headerPath = StringField(header, "workspaceIdentifier", "uri", "fsPath")
				meta.Observe(TimeField(header, "createdAt"), "")
				meta.Observe(TimeField(header, "lastUpdatedAt"), "")
				return nil
			}})
	}
	return rec.Shape(), nil
}

// cursorIDEBubbleKind names a bubble by its numeric type: 1 is a user
// message, 2 an assistant message, tool call, or thinking segment.
func cursorIDEBubbleKind(bubble any) string {
	switch cursorIDEBubbleType(bubble) {
	case "1":
		return "bubble:user"
	case "2":
		return "bubble:assistant"
	case "":
		return "bubble"
	default:
		return "bubble:type-" + cursorIDEBubbleType(bubble)
	}
}

func cursorIDEBubbleType(bubble any) string {
	if n, ok := Field(bubble, "type").(json.Number); ok {
		if _, err := n.Int64(); err == nil {
			return n.String()
		}
	}
	return ""
}

func cursorIDEObserveBubble(meta *Metadata, bubble any, hints *cursorIDEHints) {
	switch cursorIDEBubbleType(bubble) {
	case "1":
		meta.UserTurns++
	case "2":
		meta.AssistantMsgs++
	}
	if tool, ok := Field(bubble, "toolFormerData").(map[string]any); ok && len(tool) > 0 {
		meta.ToolCalls++
	}
	meta.Observe(TimeField(bubble, "createdAt"), "")
	cursorIDEObserveModel(meta, StringField(bubble, "modelInfo", "modelName"))
	if hints.bubblePath == "" {
		hints.bubblePath = StringField(bubble, "workspaceProjectDir")
	}
}

func cursorIDEObserveModel(meta *Metadata, model string) {
	if model != cursorIDEPlaceholderModel {
		meta.Observe(time.Time{}, model)
	}
}

// cursorIDEShapeWorkspace shapes a workspace database. One that does not open
// or query is counted as malformed rather than failing the session.
func cursorIDEShapeWorkspace(ctx context.Context, src Source, rel string) ArtifactShape {
	rec := NewShapeRecorder(cursorIDEWorkspaceState, rel)
	conn, err := cursorOpenReadOnly(ctx, src, rel)
	if err != nil {
		rec.shape.Malformed++
		return rec.Shape()
	}
	defer conn.Close()
	if _, err := cursorShapeTables(conn, rec, "ItemTable"); err != nil {
		rec.shape.Malformed++
		return rec.Shape()
	}
	err = cursorIDEWorkspaceValues(conn, func(key string, raw []byte) {
		if value, ok := cursorDecode(rec, raw); ok {
			cursorRecord(rec, key, "$.ItemTable."+key, value)
		}
	})
	if err != nil {
		rec.shape.Malformed++
	}
	return rec.Shape()
}

func cursorIDEShapeFolder(src Source, rel string, hints *cursorIDEHints) ArtifactShape {
	rec := NewShapeRecorder(cursorIDEWorkspaceFolder, rel)
	file, err := src.FS.Open(rel)
	if err != nil {
		rec.shape.Malformed++
		return rec.Shape()
	}
	defer file.Close()
	document, err := ShapeJSON(file, rec, nil, nil)
	if err == nil {
		hints.folderPath = cursorIDEFolderPath(StringField(document, "folder"))
	}
	return rec.Shape()
}

// cursorIDEFolderPath converts a file:// folder URI to a path. A remote URI
// such as vscode-remote://... is returned unchanged.
func cursorIDEFolderPath(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return uri
	}
	p := parsed.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return p
}

func cursorHasTable(conn *sqlite.Conn, table string) bool {
	found := false
	_ = sqlitex.ExecuteTransient(conn, `SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = ?`,
		&sqlitex.ExecOptions{Args: []any{table}, ResultFunc: func(*sqlite.Stmt) error {
			found = true
			return nil
		}})
	return found
}

// cursorKeyRange runs query over every key that starts with prefix, which
// must end in ':'. The range scan [prefix, prefix with ':' replaced by ';')
// uses the key index. query takes args, then the two bounds, then the row
// limit.
func cursorKeyRange(conn *sqlite.Conn, query string, args []any, prefix string, visit func(*sqlite.Stmt)) error {
	upper := strings.TrimSuffix(prefix, ":") + ";"
	all := append(append([]any{}, args...), prefix, upper, cursorMaxRows)
	return sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: all, ResultFunc: func(stmt *sqlite.Stmt) error {
		visit(stmt)
		return nil
	}})
}

func init() {
	Register(Layout{
		Tool:        ToolCursorIDEState,
		DisplayName: "Cursor IDE composer store",
		Sessions:    "One composer per session: a composerData:<composerId> row and its bubbleId:<composerId>:* rows in the global state.vscdb.",
		Roots: []Root{
			{Path: "{config}/Cursor/User"},
		},
		Artifacts: []Artifact{cursorIDEGlobalState, cursorIDEWorkspaceState, cursorIDEWorkspaceFolder},
		Sources: []string{
			"https://docs.rs/crate/txcript/latest/source/docs/formats/cursor-desktop.md",
			"https://docs.rs/clustervision-core/latest/cv_core/harness/cursor/index.html",
			"https://tokenuse.app/docs/development/tools/cursor/",
			"https://vshulcz.github.io/deja-vu/registry/cursor.html",
			"https://stackcone.com/blog/posts/export-cursor-chat-history-vscdb/",
			"https://github.com/markwroberts0/cursor-chat-recovery/blob/main/README.md",
			"https://forum.cursor.com/t/cant-access-chat-history-since-latest-update/158688",
			"https://forum.cursor.com/t/how-to-retrieve-records-from-a-previously-closed-workspace/158695",
		},
		Probe: cursorIDEProbe{},
	})
}
