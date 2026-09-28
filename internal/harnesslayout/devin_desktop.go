package harnesslayout

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const ToolDevinDesktop Tool = "devin-desktop"

var (
	devinACPEvents = Artifact{
		Name:        "acp-events",
		Pattern:     "User/acp-events/*.ndjson",
		Format:      FormatJSONL,
		Role:        RoleTranscript,
		Description: "one NDJSON stream of Agent Client Protocol session updates per agent session",
	}
	devinCascadeTrajectory = Artifact{
		Name:        "cascade-trajectory",
		Pattern:     "cascade/*.pb",
		Format:      FormatText,
		Role:        RoleTranscript,
		Description: "one opaque, obfuscated Cascade trajectory per conversation; never opened, only sized",
	}
	devinGlobalState = Artifact{
		Name:        "global-state",
		Pattern:     "User/globalStorage/state.vscdb",
		Format:      FormatSQLite,
		Role:        RoleIndex,
		Description: "VS Code ItemTable; the codeium.windsurf entry indexes cached Cascade trajectories per workspace",
	}
)

// devinStateKey is the ItemTable entry that holds the Windsurf extension
// state. Its value is a JSON object whose keys carry a workspace identifier
// after a colon.
const devinStateKey = "codeium.windsurf"

// devinOpaqueSizes buckets an opaque file by size so the kind census states
// how much data a trajectory holds without reading it.
var devinOpaqueSizes = []struct {
	below int64
	kind  string
}{
	{64 << 10, "opaque-under-64KiB"},
	{1 << 20, "opaque-64KiB-to-1MiB"},
	{16 << 20, "opaque-1MiB-to-16MiB"},
}

const devinOpaqueLargest = "opaque-16MiB-or-more"

type devinDesktopProbe struct{}

var _ Probe = devinDesktopProbe{}

func (devinDesktopProbe) Discover(_ context.Context, src Source) ([]SessionRef, error) {
	var refs []SessionRef
	for _, artifact := range []Artifact{devinACPEvents, devinCascadeTrajectory} {
		matches, err := fs.Glob(src.FS, artifact.Pattern)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			refs = append(refs, SessionRef{
				ID:    strings.TrimSuffix(path.Base(match), path.Ext(match)),
				Paths: []string{match},
			})
		}
	}
	if info, err := fs.Stat(src.FS, devinGlobalState.Pattern); err == nil && info.Mode().IsRegular() {
		refs = append(refs, SessionRef{ID: devinGlobalState.Pattern, Paths: []string{devinGlobalState.Pattern}})
	}
	return refs, nil
}

func (devinDesktopProbe) Capture(_ context.Context, src Source, ref SessionRef) (Capture, error) {
	if len(ref.Paths) == 0 {
		return Capture{}, errors.New("devin-desktop: session without paths")
	}
	rel := ref.Paths[0]
	switch {
	case matchArtifact(devinACPEvents, rel):
		return captureDevinACPEvents(src, ref.ID, rel)
	case matchArtifact(devinCascadeTrajectory, rel):
		return captureDevinCascade(src, ref.ID, rel)
	case rel == devinGlobalState.Pattern:
		return captureDevinGlobalState(src, rel)
	}
	return Capture{}, fmt.Errorf("devin-desktop: %s matches no declared artifact", rel)
}

func matchArtifact(artifact Artifact, rel string) bool {
	ok, _ := path.Match(artifact.Pattern, rel)
	return ok
}

func captureDevinACPEvents(src Source, id, rel string) (Capture, error) {
	file, err := src.FS.Open(rel)
	if err != nil {
		return Capture{}, err
	}
	defer file.Close()
	rec := NewShapeRecorder(devinACPEvents, rel)
	meta := Metadata{SessionID: id}
	var lastKind, lastMessage string
	err = ShapeJSONL(file, rec, devinACPKind, func(record any) {
		update := Field(record, "notification")
		kind := StringField(update, "sessionUpdate")
		message := StringField(update, "messageId")
		continues := kind == lastKind && message == lastMessage
		lastKind, lastMessage = kind, message
		switch kind {
		case "user_message_chunk":
			if !continues {
				meta.UserTurns++
			}
		case "agent_message_chunk":
			if !continues {
				meta.AssistantMsgs++
			}
		case "tool_call":
			meta.ToolCalls++
		case "session_info_update":
			if title := strings.TrimSpace(StringField(update, "title")); title != "" {
				meta.Title = title
			}
		}
		for _, keys := range [][]string{
			{"content", "metadata", "created_at"},
			{"metadata", "created_at"},
			{"created_at"},
			{"timestamp"},
			{"updatedAt"},
		} {
			meta.Observe(TimeField(update, keys...), "")
		}
		for _, keys := range [][]string{
			{"_meta", "cognition.ai/model"},
			{"metadata", "generation_model"},
			{"content", "metadata", "generation_model"},
		} {
			meta.Observe(time.Time{}, StringField(update, keys...))
		}
	})
	if err != nil {
		return Capture{}, err
	}
	return Capture{Session: id, Metadata: meta, Shapes: []ArtifactShape{rec.Shape()}}, nil
}

func devinACPKind(record any) string {
	return StringField(record, "notification", "sessionUpdate")
}

func captureDevinCascade(src Source, id, rel string) (Capture, error) {
	info, err := fs.Stat(src.FS, rel)
	if err != nil {
		return Capture{}, err
	}
	rec := NewShapeRecorder(devinCascadeTrajectory, rel)
	kind := devinOpaqueLargest
	for _, size := range devinOpaqueSizes {
		if info.Size() < size.below {
			kind = size.kind
			break
		}
	}
	rec.shape.Records++
	rec.shape.Kinds[kind]++
	meta := Metadata{SessionID: id}
	meta.Observe(info.ModTime().UTC(), "")
	return Capture{Session: id, Metadata: meta, Shapes: []ArtifactShape{rec.Shape()}}, nil
}

func captureDevinGlobalState(src Source, rel string) (Capture, error) {
	if src.Dir == "" {
		return Capture{}, errors.New("devin-desktop: the global state database needs an operating system directory")
	}
	conn, err := sqlite.OpenConn(filepath.Join(src.Dir, filepath.FromSlash(rel)), sqlite.OpenReadOnly)
	if err != nil {
		return Capture{}, fmt.Errorf("open %s: %w", rel, err)
	}
	defer conn.Close()
	rec := NewShapeRecorder(devinGlobalState, rel)
	tables, err := shapeSQLiteTables(conn, rec)
	if err != nil {
		return Capture{}, fmt.Errorf("read %s schema: %w", rel, err)
	}
	if slices.Contains(tables, "ItemTable") {
		if err := shapeDevinStateEntry(conn, rec); err != nil {
			return Capture{}, fmt.Errorf("read %s: %w", rel, err)
		}
	}
	return Capture{Shapes: []ArtifactShape{rec.Shape()}}, nil
}

func shapeSQLiteTables(conn *sqlite.Conn, rec *ShapeRecorder) ([]string, error) {
	var tables []string
	err := sqlitex.ExecuteTransient(conn,
		`SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			tables = append(tables, stmt.ColumnText(0))
			return nil
		}})
	if err != nil {
		return nil, err
	}
	for _, table := range tables {
		err := sqlitex.ExecuteTransient(conn,
			`SELECT name, type FROM pragma_table_info(?)`,
			&sqlitex.ExecOptions{Args: []any{table}, ResultFunc: func(stmt *sqlite.Stmt) error {
				rec.AddField("$."+table+"."+stmt.ColumnText(0), sqliteColumnType(stmt.ColumnText(1)))
				return nil
			}})
		if err != nil {
			return nil, err
		}
	}
	return tables, nil
}

// sqliteColumnType maps a declared column type to a JSON type by SQLite
// affinity. TEXT, BLOB, and untyped columns are recorded as strings.
func sqliteColumnType(declared string) JSONType {
	upper := strings.ToUpper(declared)
	switch {
	case strings.Contains(upper, "INT"),
		strings.Contains(upper, "REAL"),
		strings.Contains(upper, "FLOA"),
		strings.Contains(upper, "DOUB"),
		strings.Contains(upper, "NUM"),
		strings.Contains(upper, "DEC"):
		return JSONNumber
	}
	return JSONString
}

// shapeDevinStateEntry records the top-level keys of the codeium.windsurf
// entry. SQLite walks the JSON so no value reaches Go; each key is reduced
// to the part before its workspace identifier.
func shapeDevinStateEntry(conn *sqlite.Conn, rec *ShapeRecorder) error {
	present := false
	valid := false
	err := sqlitex.ExecuteTransient(conn,
		`SELECT json_valid(CAST(value AS TEXT)) FROM ItemTable WHERE key = ?`,
		&sqlitex.ExecOptions{Args: []any{devinStateKey}, ResultFunc: func(stmt *sqlite.Stmt) error {
			present = true
			valid = stmt.ColumnInt(0) == 1
			return nil
		}})
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if !valid {
		rec.shape.Malformed++
		return nil
	}
	base := "$." + devinStateKey
	rec.AddField(base, JSONObject)
	return sqlitex.ExecuteTransient(conn,
		`SELECT entry.key, entry.type FROM ItemTable, json_each(CAST(ItemTable.value AS TEXT)) AS entry WHERE ItemTable.key = ?`,
		&sqlitex.ExecOptions{Args: []any{devinStateKey}, ResultFunc: func(stmt *sqlite.Stmt) error {
			key := stmt.ColumnText(0)
			if prefix, _, found := strings.Cut(key, ":"); found {
				key = prefix + ":*"
			}
			rec.shape.Records++
			rec.shape.Kinds[key]++
			rec.AddField(base+"."+key, sqliteJSONType(stmt.ColumnText(1)))
			return nil
		}})
}

func sqliteJSONType(t string) JSONType {
	switch t {
	case "object":
		return JSONObject
	case "array":
		return JSONArray
	case "integer", "real":
		return JSONNumber
	case "true", "false":
		return JSONBool
	case "null":
		return JSONNull
	}
	return JSONString
}

func init() {
	Register(Layout{
		Tool:        ToolDevinDesktop,
		DisplayName: "Devin Desktop",
		Sessions:    "One ACP event stream per agent session, and one opaque Cascade trajectory file per legacy Cascade conversation; the shared global state database is captured once as its own entry.",
		Roots: []Root{
			{Path: "{config}/Devin"},
			{Path: "{home}/.codeium/windsurf"},
		},
		Artifacts: []Artifact{devinACPEvents, devinCascadeTrajectory, devinGlobalState},
		Sources: []string{
			"https://devin.ai/blog/windsurf-is-now-devin-desktop",
			"https://docs.devin.ai/desktop/devin-desktop-faq",
			"https://docs.devin.ai/desktop/cascade/memories",
			"https://github.com/junhoyeo/tokscale/blob/1d9a9395418efc6952944b794097935d7d6fa1e8/crates/tokscale-core/src/sessions/devin.rs",
			"https://github.com/junhoyeo/tokscale/blob/1d9a9395418efc6952944b794097935d7d6fa1e8/crates/tokscale-core/src/scanner.rs",
			"https://github.com/junhoyeo/tokscale/pull/885",
			"https://github.com/junhoyeo/tokscale/issues/894",
			"https://github.com/jijiamoer/windsurf-trajectory-extractor/blob/e7d5c04b3708e968a353ecf32020e034b97fbaf3/src/windsurf_trajectory/extractor.py",
			"https://agentclientprotocol.com/protocol/prompt-turn",
		},
		Probe: devinDesktopProbe{},
	})
}
