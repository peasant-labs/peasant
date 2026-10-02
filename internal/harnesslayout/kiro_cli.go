package harnesslayout

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const ToolKiroCLI Tool = "kiro-cli"

var (
	kiroCLISessionHeader = Artifact{
		Name:        "session-header",
		Pattern:     "*.json",
		Format:      FormatJSON,
		Role:        RoleMetadata,
		Description: "CLI 2.x session header beside the transcript: session_id, cwd, created_at, updated_at, title, session_state",
	}
	kiroCLISessionTranscript = Artifact{
		Name:        "session-transcript",
		Pattern:     "*.jsonl",
		Format:      FormatJSONL,
		Role:        RoleTranscript,
		Description: "CLI 2.x transcript of {version, kind, data} records: Prompt, AssistantMessage, ToolResults, Clear",
	}
	kiroCLIConversationRow = Artifact{
		Name:        "conversation-row",
		Pattern:     "data.sqlite3",
		Format:      FormatSQLite,
		Role:        RoleIndex,
		Description: "one row of conversations_v2 (key, conversation_id, created_at, updated_at, value) or of the Amazon Q conversations (key, value) table",
	}
	kiroCLIConversationValue = Artifact{
		Name:        "conversation-value",
		Pattern:     "data.sqlite3",
		Format:      FormatJSON,
		Role:        RoleTranscript,
		Description: "the value column: a serialized ConversationState with history[] of {user, assistant, request_metadata}",
	}
)

// kiroCLITables are the conversation tables in the order Kiro CLI and its
// Amazon Q Developer CLI predecessor introduced them. Only these names are
// ever interpolated into SQL.
var kiroCLITables = []string{"conversations_v2", "conversations"}

const kiroCLIDatabase = "data.sqlite3"

type kiroCLIProbe struct{}

var _ Probe = kiroCLIProbe{}

func init() {
	Register(Layout{
		Tool:        ToolKiroCLI,
		DisplayName: "Kiro CLI",
		Sessions: "CLI 2.x writes a <uuid>.json header and a <uuid>.jsonl transcript per session under ~/.kiro/sessions/cli; " +
			"the older chat store is one row per conversation in data.sqlite3. CLI 3.0 sessions are captured by the kiro layout.",
		Roots: []Root{
			{Path: "{home}/.kiro/sessions/cli"},
			{OS: []OS{OSLinux}, Path: "{home}/.local/share/kiro-cli"},
			{OS: []OS{OSDarwin}, Path: "{config}/kiro-cli"},
			{OS: []OS{OSWindows}, Path: "{home}/AppData/Local/kiro-cli"},
			{OS: []OS{OSWindows}, Path: "{config}/kiro-cli"},
			{OS: []OS{OSLinux}, Path: "{home}/.local/share/amazon-q"},
			{OS: []OS{OSDarwin}, Path: "{config}/amazon-q"},
			{OS: []OS{OSWindows}, Path: "{home}/AppData/Local/amazon-q"},
			{OS: []OS{OSWindows}, Path: "{config}/amazon-q"},
		},
		Artifacts: []Artifact{kiroCLISessionHeader, kiroCLISessionTranscript, kiroCLIConversationRow, kiroCLIConversationValue},
		Sources: []string{
			"https://kiro.dev/docs/reference/cli-commands/",
			"https://kiro.dev/docs/cli/v3/",
			"https://github.com/aws/amazon-q-developer-cli/blob/main/crates/chat-cli/src/database/sqlite_migrations/007_conversations_table.sql",
			"https://github.com/aws/amazon-q-developer-cli/blob/main/crates/chat-cli/src/database/mod.rs",
			"https://github.com/aws/amazon-q-developer-cli/blob/main/crates/chat-cli/src/cli/chat/conversation.rs",
			"https://github.com/aws/amazon-q-developer-cli/blob/main/crates/chat-cli/src/cli/chat/message.rs",
			"https://github.com/aws/amazon-q-developer-cli/blob/main/crates/chat-cli/src/util/paths.rs",
			"https://github.com/junbaor/kiro-cli-chat-viewer/blob/main/database-schema.md",
			"https://github.com/DevOps-Nirvana/Kiro-Ception/blob/main/src/kiro_ception/cli_loader.py",
			"https://github.com/getagentseal/codeburn/blob/main/src/providers/kiro.ts",
			"https://openusage.sh/docs/providers/kiro/",
			"https://github.com/pajaydev/kiro-history",
			"https://github.com/kirodotdev/KiroCrew/blob/2e09b10304baee7c245b10992a3be1dc4374cb1f/src/kiro_crew/config/paths.py",
			"https://github.com/kirodotdev/KiroCrew/blob/2e09b10304baee7c245b10992a3be1dc4374cb1f/src/kiro_crew/identity_stores.py",
			"https://github.com/kirodotdev/KiroCrew/blob/2e09b10304baee7c245b10992a3be1dc4374cb1f/src/kiro_crew/acp/client.py",
			"https://github.com/kirodotdev/KiroCrew/blob/2e09b10304baee7c245b10992a3be1dc4374cb1f/src/kiro_crew/dashboard/handlers/usage.py",
		},
		Probe: kiroCLIProbe{},
	})
}

func (kiroCLIProbe) Discover(ctx context.Context, src Source) ([]SessionRef, error) {
	var refs []SessionRef
	var errs []error
	sessions := map[string][]string{}
	for _, pattern := range []string{kiroCLISessionHeader.Pattern, kiroCLISessionTranscript.Pattern} {
		matches, err := fs.Glob(src.FS, pattern)
		if err != nil {
			errs = append(errs, err)
		}
		for _, match := range matches {
			id := strings.TrimSuffix(match, path.Ext(match))
			if !kiroCLIIsUUID(id) {
				continue
			}
			sessions[id] = append(sessions[id], match)
		}
	}
	for id, paths := range sessions {
		refs = append(refs, SessionRef{ID: id, Paths: paths})
	}

	if src.Dir == "" || !kiroIsFile(src.FS, kiroCLIDatabase) {
		return refs, errors.Join(errs...)
	}
	conn, err := kiroCLIOpenDatabase(src.Dir)
	if err != nil {
		return refs, errors.Join(append(errs, err)...)
	}
	defer conn.Close()
	tables, err := kiroCLIPresentTables(conn)
	if err != nil {
		errs = append(errs, err)
	}
	for _, table := range tables {
		err := sqlitex.ExecuteTransient(conn, "SELECT rowid FROM "+table+" ORDER BY rowid", &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				refs = append(refs, SessionRef{
					ID:    kiroCLIDatabase + "/" + table + "/" + strconv.FormatInt(stmt.ColumnInt64(0), 10),
					Paths: []string{kiroCLIDatabase},
				})
				return nil
			},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("list %s: %w", table, err))
		}
	}
	return refs, errors.Join(errs...)
}

func (p kiroCLIProbe) Capture(_ context.Context, src Source, ref SessionRef) (Capture, error) {
	if table, rowid, ok := kiroCLIParseRowRef(ref.ID); ok {
		return p.captureRow(src, table, rowid)
	}
	meta := Metadata{SessionID: path.Base(ref.ID)}
	var shapes []ArtifactShape
	for _, rel := range ref.Paths {
		switch path.Ext(rel) {
		case ".json":
			rec := NewShapeRecorder(kiroCLISessionHeader, rel)
			doc, err := kiroShapeJSONFile(src.FS, rel, rec, nil, nil)
			if err == nil {
				if id := StringField(doc, "session_id"); id != "" {
					meta.SessionID = id
				}
				meta.Title = StringField(doc, "title")
				meta.ProjectPath = StringField(doc, "cwd")
				meta.Observe(TimeField(doc, "created_at"), StringField(doc, "session_state", "rts_model_state", "model_info", "model_id"))
				meta.Observe(TimeField(doc, "updated_at"), "")
				for _, turn := range ArrayField(doc, "session_state", "conversation_metadata", "user_turn_metadatas") {
					meta.Observe(TimeField(turn, "end_timestamp"), "")
				}
			} else if !errors.Is(err, errKiroMalformed) {
				return Capture{}, err
			}
			shapes = append(shapes, rec.Shape())
		case ".jsonl":
			rec := NewShapeRecorder(kiroCLISessionTranscript, rel)
			file, err := src.FS.Open(rel)
			if err != nil {
				return Capture{}, err
			}
			err = ShapeJSONL(file, rec, KindField("kind"), func(record any) {
				meta.Observe(TimeField(record, "data", "meta", "timestamp"), "")
				meta.Observe(TimeField(record, "timestamp"), "")
				switch StringField(record, "kind") {
				case "Prompt":
					meta.UserTurns++
				case "AssistantMessage":
					meta.AssistantMsgs++
					for _, block := range ArrayField(record, "data", "content") {
						if StringField(block, "kind") == "toolUse" {
							meta.ToolCalls++
						}
					}
				}
			})
			file.Close()
			if err != nil {
				return Capture{}, err
			}
			shapes = append(shapes, rec.Shape())
		}
	}
	return Capture{Metadata: meta, Shapes: shapes}, nil
}

func (kiroCLIProbe) captureRow(src Source, table string, rowid int64) (Capture, error) {
	if src.Dir == "" {
		return Capture{}, errors.New("kiro-cli: a SQLite conversation needs an operating system source directory")
	}
	conn, err := kiroCLIOpenDatabase(src.Dir)
	if err != nil {
		return Capture{}, err
	}
	defer conn.Close()

	row := NewShapeRecorder(kiroCLIConversationRow, kiroCLIDatabase)
	value := NewShapeRecorder(kiroCLIConversationValue, kiroCLIDatabase)
	meta := Metadata{}
	var raw string
	found := false
	err = sqlitex.ExecuteTransient(conn, "SELECT * FROM "+table+" WHERE rowid = ?", &sqlitex.ExecOptions{
		Args: []any{rowid},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			for col := 0; col < stmt.ColumnCount(); col++ {
				name := stmt.ColumnName(col)
				row.AddField("$."+table+"."+name, kiroCLIColumnType(stmt.ColumnType(col)))
				switch name {
				case "key":
					meta.ProjectPath = stmt.ColumnText(col)
				case "conversation_id":
					meta.SessionID = stmt.ColumnText(col)
				case "created_at", "updated_at":
					if stmt.ColumnType(col) == sqlite.TypeText {
						meta.Observe(ParseTime(stmt.ColumnText(col)), "")
					} else {
						meta.Observe(ParseTime(stmt.ColumnInt64(col)), "")
					}
				case "value":
					raw = stmt.ColumnText(col)
				}
			}
			return nil
		},
	})
	if err != nil {
		return Capture{}, fmt.Errorf("read %s row %d: %w", table, rowid, err)
	}
	if !found {
		return Capture{}, fmt.Errorf("%s row %d: %w", table, rowid, fs.ErrNotExist)
	}
	doc, err := ShapeJSON(strings.NewReader(raw), value, kiroCLIHistoryMessages, kiroCLIMessageKind)
	if err == nil {
		kiroCLIObserveConversation(&meta, doc)
	}
	return Capture{Metadata: meta, Shapes: []ArtifactShape{row.Shape(), value.Shape()}}, nil
}

// kiroCLIHistoryMessages returns each history entry's user and assistant
// message wrapped in a one-key object that names its side.
func kiroCLIHistoryMessages(document any) []any {
	var out []any
	for _, entry := range ArrayField(document, "history") {
		for _, side := range []string{"user", "assistant"} {
			if message := Field(entry, side); message != nil {
				out = append(out, map[string]any{side: message})
			}
		}
	}
	return out
}

func kiroCLIMessageKind(record any) string {
	if message := Field(record, "user"); message != nil {
		return "user:" + kiroCLIVariant(Field(message, "content"))
	}
	if message := Field(record, "assistant"); message != nil {
		return "assistant:" + kiroCLIVariant(message)
	}
	return ""
}

// kiroCLIVariant names a serde externally tagged enum by its single key,
// such as Prompt, ToolUseResults, Response, or ToolUse.
func kiroCLIVariant(value any) string {
	switch v := value.(type) {
	case string:
		return "text"
	case []any:
		return "parts"
	case map[string]any:
		if len(v) == 1 {
			for key := range v {
				return key
			}
		}
		return "object"
	}
	return "absent"
}

func kiroCLIObserveConversation(meta *Metadata, doc any) {
	if meta.SessionID == "" {
		meta.SessionID = StringField(doc, "conversation_id")
	}
	meta.Observe(time.Time{}, StringField(doc, "model_info", "model_id"))
	meta.Observe(time.Time{}, StringField(doc, "model"))
	for _, entry := range ArrayField(doc, "history") {
		user := Field(entry, "user")
		if meta.ProjectPath == "" {
			meta.ProjectPath = StringField(user, "env_context", "env_state", "current_working_directory")
		}
		meta.Observe(TimeField(user, "timestamp"), "")
		switch kiroCLIVariant(Field(user, "content")) {
		case "Prompt", "text", "parts":
			meta.UserTurns++
		}
		if assistant := Field(entry, "assistant"); assistant != nil {
			meta.AssistantMsgs++
			meta.ToolCalls += len(ArrayField(assistant, "ToolUse", "tool_uses"))
		}
		request := Field(entry, "request_metadata")
		meta.Observe(TimeField(request, "request_start_timestamp_ms"), StringField(request, "model_id"))
		meta.Observe(TimeField(request, "stream_end_timestamp_ms"), "")
	}
	if meta.ProjectPath == "" {
		meta.ProjectPath = StringField(doc, "env_context", "env_state", "current_working_directory")
	}
}

func kiroCLIParseRowRef(id string) (string, int64, bool) {
	rest, ok := strings.CutPrefix(id, kiroCLIDatabase+"/")
	if !ok {
		return "", 0, false
	}
	table, rowText, ok := strings.Cut(rest, "/")
	if !ok {
		return "", 0, false
	}
	known := false
	for _, candidate := range kiroCLITables {
		known = known || candidate == table
	}
	rowid, err := strconv.ParseInt(rowText, 10, 64)
	return table, rowid, known && err == nil
}

func kiroCLIOpenDatabase(dir string) (*sqlite.Conn, error) {
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(dir, kiroCLIDatabase))}
	query := uri.Query()
	query.Set("mode", "ro")
	uri.RawQuery = query.Encode()
	conn, err := sqlite.OpenConn(uri.String(), sqlite.OpenReadOnly|sqlite.OpenURI)
	if err != nil {
		return nil, fmt.Errorf("open %s read-only: %w", kiroCLIDatabase, err)
	}
	return conn, nil
}

func kiroCLIPresentTables(conn *sqlite.Conn) ([]string, error) {
	present := map[string]bool{}
	err := sqlitex.ExecuteTransient(conn, "SELECT name FROM sqlite_master WHERE type = 'table'", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			present[stmt.ColumnText(0)] = true
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list tables in %s: %w", kiroCLIDatabase, err)
	}
	var tables []string
	for _, table := range kiroCLITables {
		if present[table] {
			tables = append(tables, table)
		}
	}
	return tables, nil
}

func kiroCLIIsUUID(name string) bool {
	if len(name) != 36 {
		return false
	}
	for i, r := range name {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}

func kiroCLIColumnType(typ sqlite.ColumnType) JSONType {
	switch typ {
	case sqlite.TypeInteger, sqlite.TypeFloat:
		return JSONNumber
	case sqlite.TypeNull:
		return JSONNull
	default:
		return JSONString
	}
}
