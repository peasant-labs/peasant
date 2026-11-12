package harnesslayout

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The Antigravity command-line agent (agy) and the Antigravity desktop
// application keep one store each under ~/.gemini. The per-session layout of
// the two stores is the same; what differs is the root and a few entries that
// only one product writes. This file is the shared core: the per-session
// artifacts, discovery, capture, and the SQLite read guard. antigravity_cli.go
// and antigravity_ide.go each declare one product and register it.

// antigravityIDToken marks the conversation identifier in a per-session glob.
const antigravityIDToken = "{id}"

// antigravityRead says how the core captures one artifact.
type antigravityRead int

const (
	// antigravityReadBody shapes a conversation body database through the
	// read guard: tables, columns, row counts, and the step census.
	antigravityReadBody antigravityRead = iota + 1
	// antigravityReadIndex reads the five allowed columns of this session's
	// row in the conversation index, through the read guard.
	antigravityReadIndex
	// antigravityReadJSONL shapes every record of a JSONL file.
	antigravityReadJSONL
	// antigravityReadJSON shapes one JSON document.
	antigravityReadJSON
	// antigravityReadFieldNames records the field names of a protobuf text
	// file, never its values.
	antigravityReadFieldNames
	// antigravityReadSize counts and sizes files without opening them.
	antigravityReadSize
)

// antigravityArtifact is one declared artifact and how the core reads it.
type antigravityArtifact struct {
	Artifact
	// glob is the pattern for one session, with antigravityIDToken where the
	// conversation identifier goes. A root entry has no token: every session
	// of the root shares it. Artifact.Pattern is glob with the token replaced
	// by "*".
	glob string
	read antigravityRead
	// observe marks the artifact whose records fill Metadata counts.
	observe bool
}

func antigravityDeclare(name, glob string, format Format, role Role, read antigravityRead, description string) antigravityArtifact {
	return antigravityArtifact{
		Artifact: Artifact{
			Name:        name,
			Pattern:     strings.ReplaceAll(glob, antigravityIDToken, "*"),
			Format:      format,
			Role:        role,
			Description: description,
		},
		glob: glob,
		read: read,
	}
}

// pattern returns the glob for one session.
func (a antigravityArtifact) pattern(id string) string {
	return strings.ReplaceAll(a.glob, antigravityIDToken, id)
}

const antigravitySystemDir = "brain/" + antigravityIDToken + "/.system_generated"

var (
	antigravityBody = antigravityDeclare("conversation-body", "conversations/"+antigravityIDToken+".db",
		FormatSQLite, RoleTranscript, antigravityReadBody,
		"one SQLite database per conversation: steps (step_type, status, and protobuf blobs per step) and per-trajectory blob tables")
	antigravityTranscript = func() antigravityArtifact {
		a := antigravityDeclare("transcript-log", antigravitySystemDir+"/logs/transcript.jsonl",
			FormatJSONL, RoleTranscript, antigravityReadJSONL,
			"one record per step: type, source, status, step_index, created_at, content, thinking, tool_calls, token counts")
		a.observe = true
		return a
	}()
	antigravityTranscriptFull = antigravityDeclare("transcript-full-log", antigravitySystemDir+"/logs/transcript_full.jsonl",
		FormatJSONL, RoleTranscript, antigravityReadJSONL,
		"the transcript log without the truncated_fields marker")
	antigravityChunks = antigravityDeclare("transcript-chunks", antigravitySystemDir+"/logs/chunks/*/*.jsonl",
		FormatJSONL, RoleAuxiliary, antigravityReadJSONL,
		"the transcript logs split into chunk files, one directory per log")
	antigravityStepOutput = antigravityDeclare("step-output", antigravitySystemDir+"/steps/*/output.txt",
		FormatText, RoleAuxiliary, antigravityReadSize,
		"the output of one step; counted and sized, never read")
	antigravityAnnotation = antigravityDeclare("annotation", "annotations/"+antigravityIDToken+".pbtxt",
		FormatText, RoleMetadata, antigravityReadFieldNames,
		"protobuf text with the conversation title; field names only")
	antigravityPresence = antigravityDeclare("presence-lock", "presence/"+antigravityIDToken+".lock",
		FormatText, RoleAuxiliary, antigravityReadSize,
		"lock held while a product has the conversation open; counted and sized, never read")
	antigravityIndexDB = antigravityDeclare("conversation-index", "conversation_summaries.db",
		FormatSQLite, RoleIndex, antigravityReadIndex,
		"SQLite index of conversations with a full-text table; it lists the conversations of both products and is not a source of sessions")
	antigravityCrashes = antigravityDeclare("crash-logs", "crashes/*",
		FormatText, RoleAuxiliary, antigravityReadSize,
		"crash logs of the product; counted and sized, never read")
)

// antigravitySessionArtifacts are the per-session artifacts both products
// write, in capture order.
var antigravitySessionArtifacts = []antigravityArtifact{
	antigravityBody, antigravityTranscript, antigravityTranscriptFull, antigravityChunks,
	antigravityStepOutput, antigravityAnnotation, antigravityPresence,
}

// antigravitySharedRootArtifacts are the root entries both products write.
var antigravitySharedRootArtifacts = []antigravityArtifact{antigravityIndexDB, antigravityCrashes}

// antigravityProduct is what differs between the two Antigravity stores. The
// per-session layout is shared; see antigravityProbe.
type antigravityProduct struct {
	tool        Tool
	harness     schema.Harness
	displayName string
	root        string
	// rootEntries are the artifacts at the store root that only this product
	// writes.
	rootEntries []antigravityArtifact
	// sessionExtras are per-session artifacts that only this product writes.
	sessionExtras []antigravityArtifact
}

// artifacts returns every artifact of the product in capture order.
func (p antigravityProduct) artifacts() []antigravityArtifact {
	var out []antigravityArtifact
	out = append(out, antigravitySessionArtifacts...)
	out = append(out, p.sessionExtras...)
	out = append(out, antigravitySharedRootArtifacts...)
	return append(out, p.rootEntries...)
}

// antigravityLayout declares the layout of one product.
func antigravityLayout(product antigravityProduct) Layout {
	artifacts := product.artifacts()
	declared := make([]Artifact, len(artifacts))
	for i, a := range artifacts {
		declared[i] = a.Artifact
	}
	return Layout{
		Tool:        product.tool,
		Harness:     product.harness,
		DisplayName: product.displayName,
		Sessions: "One conversation per session: conversations/<id>.db or brain/<id>/ under this root. " +
			"The conversation index also lists the other Antigravity product's conversations and is not a source of sessions.",
		Roots:     []Root{{Path: product.root}},
		Artifacts: declared,
		Sources: []string{
			"https://github.com/peasant-labs/polyrepo/issues/77",
			"https://github.com/numtide/llm-agents.nix",
		},
		Probe: antigravityProbe{product: product},
	}
}

// antigravityProbe discovers and captures sessions of one Antigravity product.
// A session is a conversation body under this root. The conversation index is
// not a source of sessions: it also lists the other product's conversations.
type antigravityProbe struct{ product antigravityProduct }

var _ Probe = antigravityProbe{}

// antigravityValidID reports whether a conversation identifier is a plain
// name: letters, digits, '-' and '_'. Any other name is not a conversation,
// and the rule keeps glob metacharacters out of per-session patterns.
func antigravityValidID(id string) bool {
	if id == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !antigravityIDByte(id[i]) {
			return false
		}
	}
	return true
}

func antigravityIDByte(c byte) bool {
	switch {
	case c == '-', c == '_':
		return true
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'z':
		return true
	}
	return c >= 'A' && c <= 'Z'
}

// Discover returns one session per conversation body or brain directory under
// this root. It never reads the conversation index.
func (p antigravityProbe) Discover(_ context.Context, src Source) ([]SessionRef, error) {
	ids := map[string]bool{}
	bodies, err := fs.Glob(src.FS, antigravityBody.Pattern)
	if err != nil {
		return nil, err
	}
	for _, match := range bodies {
		id := strings.TrimSuffix(path.Base(match), ".db")
		if antigravityValidID(id) && antigravityIsFile(src.FS, match) {
			ids[id] = true
		}
	}
	entries, err := fs.ReadDir(src.FS, "brain")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() && antigravityValidID(entry.Name()) {
			ids[entry.Name()] = true
		}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	refs := make([]SessionRef, 0, len(sorted))
	for _, id := range sorted {
		ref := SessionRef{ID: id}
		for _, a := range p.product.artifacts() {
			matches, err := fs.Glob(src.FS, a.pattern(id))
			if err != nil {
				return nil, err
			}
			for _, match := range matches {
				if antigravityIsFile(src.FS, match) {
					ref.Paths = append(ref.Paths, match)
				}
			}
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// antigravityIsFile reports whether rel is a regular file. It stats the file
// and never opens it.
func antigravityIsFile(fsys fs.FS, rel string) bool {
	info, err := fs.Stat(fsys, rel)
	return err == nil && info.Mode().IsRegular()
}

// Capture shapes every artifact of the session. Metadata.Title is never set:
// an Antigravity title is derived from conversation text.
func (p antigravityProbe) Capture(ctx context.Context, src Source, ref SessionRef) (Capture, error) {
	if !antigravityValidID(ref.ID) {
		return Capture{}, fmt.Errorf("antigravity capture: session identifier %q is not a conversation identifier (letters, digits, '-' and '_' only); pass a reference returned by Discover", ref.ID)
	}
	meta := Metadata{SessionID: ref.ID}
	var shapes []ArtifactShape
	for _, a := range p.product.artifacts() {
		pattern := a.pattern(ref.ID)
		var files []string
		for _, rel := range ref.Paths {
			if ok, _ := path.Match(pattern, rel); ok {
				files = append(files, rel)
			}
		}
		if len(files) == 0 {
			continue
		}
		shapePath := pattern
		if len(files) == 1 {
			shapePath = files[0]
		}
		rec := NewShapeRecorder(a.Artifact, shapePath)
		if err := antigravityShape(ctx, src, a, ref.ID, files, rec, &meta); err != nil {
			return Capture{}, err
		}
		shapes = append(shapes, rec.Shape())
	}
	return Capture{Metadata: meta, Shapes: shapes}, nil
}

func antigravityShape(ctx context.Context, src Source, a antigravityArtifact, id string, files []string, rec *ShapeRecorder, meta *Metadata) error {
	switch a.read {
	case antigravityReadBody:
		return antigravityShapeBody(ctx, src, files[0], rec)
	case antigravityReadIndex:
		return antigravityShapeIndex(ctx, src, files[0], id, rec, meta)
	case antigravityReadSize:
		for _, rel := range files {
			info, err := fs.Stat(src.FS, rel)
			if err != nil {
				rec.shape.Malformed++
				continue
			}
			rec.shape.Records++
			rec.shape.Bytes += info.Size()
		}
		return nil
	}
	for _, rel := range files {
		if err := antigravityShapeFile(src, a, rel, rec, meta); err != nil {
			rec.shape.Malformed++
		}
	}
	return nil
}

// antigravityShapeFile shapes one JSONL, JSON, or protobuf text file. Values
// never leave this function: the shape keeps field paths and JSON types.
func antigravityShapeFile(src Source, a antigravityArtifact, rel string, rec *ShapeRecorder, meta *Metadata) error {
	file, err := src.FS.Open(rel)
	if err != nil {
		return err
	}
	defer file.Close()
	switch a.read {
	case antigravityReadJSONL:
		return antigravityShapeJSONL(file, rec, a.observe, meta)
	case antigravityReadJSON:
		raw, err := io.ReadAll(io.LimitReader(file, cursorMaxValueBytes+1))
		if err != nil {
			return err
		}
		if len(raw) > cursorMaxValueBytes {
			return fmt.Errorf("%s is larger than %d bytes", rel, cursorMaxValueBytes)
		}
		if value, ok := cursorDecode(rec, raw); ok {
			cursorRecord(rec, "", "$", value)
		}
		return nil
	case antigravityReadFieldNames:
		return antigravityShapeFieldNames(file, rec)
	}
	return fmt.Errorf("antigravity capture: artifact %q has no reader for its kind %d", a.Name, a.read)
}

// Transcript record kinds observed on antigravity-hub 2.29.0 and
// antigravity-cli 1.5.3.
const (
	antigravityUserInput       = "USER_INPUT"
	antigravityPlannerResponse = "PLANNER_RESPONSE"
)

// antigravityShapeJSONL records every record with its type and source as the
// kind. Object keys that are data, such as identifiers or paths, are collapsed
// to "*".
func antigravityShapeJSONL(r io.Reader, rec *ShapeRecorder, observe bool, meta *Metadata) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxJSONLLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		value, ok := cursorDecode(rec, []byte(line))
		if !ok {
			continue
		}
		kind := cursorKind(StringField(value, "type"), "other") + "/" + cursorKind(StringField(value, "source"), "other")
		cursorRecord(rec, kind, "$", value)
		if !observe {
			continue
		}
		switch StringField(value, "type") {
		case antigravityUserInput:
			meta.UserTurns++
		case antigravityPlannerResponse:
			meta.AssistantMsgs++
		}
		meta.ToolCalls += len(ArrayField(value, "tool_calls"))
		meta.Observe(TimeField(value, "created_at"), "")
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read JSONL: %w", err)
	}
	return nil
}

// antigravityShapeFieldNames records the field paths of a protobuf text file
// and the type of each scalar, never a value. A line that does not start with
// a field name, such as the continuation of a string, is skipped.
func antigravityShapeFieldNames(r io.Reader, rec *ShapeRecorder) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxJSONLLine)
	rec.shape.Records++
	stack := []string{"$"}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "}" || line == ">" {
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		end := 0
		for end < len(line) && cursorIdentifierPart(line[end]) {
			end++
		}
		if end == 0 || !cursorIdentifierStart(line[0]) {
			continue
		}
		field := cursorFieldPath(stack[len(stack)-1], line[:end])
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[end:]), ":"))
		switch {
		case rest == "{" || rest == "<":
			rec.AddField(field, JSONObject)
			stack = append(stack, field)
		case strings.HasPrefix(rest, "{") || strings.HasPrefix(rest, "<"):
			rec.AddField(field, JSONObject)
		case strings.HasPrefix(rest, "["):
			rec.AddField(field, JSONArray)
		default:
			rec.AddField(field, antigravityScalarType(rest))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read protobuf text: %w", err)
	}
	return nil
}

func antigravityScalarType(value string) JSONType {
	switch {
	case value == "true" || value == "false":
		return JSONBool
	case value == "":
		return JSONString
	case value[0] == '-' || value[0] == '.':
		return JSONNumber
	case value[0] >= '0' && value[0] <= '9':
		return JSONNumber
	}
	return JSONString
}

// antigravityShapeBody records the tables, columns, and row counts of a
// conversation body and a census of steps.step_type and steps.status. Every
// read goes through the read guard.
func antigravityShapeBody(ctx context.Context, src Source, rel string, rec *ShapeRecorder) error {
	db, err := antigravityOpen(ctx, src, rel, antigravityBodyDatabase)
	if err != nil {
		return err
	}
	defer db.Close()
	var tables []string
	err = db.query(`SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name LIMIT ?`,
		[]any{cursorMaxRows}, func(stmt *sqlite.Stmt) error {
			tables = append(tables, stmt.ColumnText(0))
			return nil
		})
	if err != nil {
		return err
	}
	hasSteps := false
	for _, table := range tables {
		tablePath := cursorFieldPath("$", table)
		err := db.query(`SELECT name, type FROM pragma_table_info(?)`, []any{table}, func(stmt *sqlite.Stmt) error {
			rec.AddField(cursorFieldPath(tablePath, stmt.ColumnText(0)), cursorColumnType(stmt.ColumnText(1)))
			return nil
		})
		if err != nil {
			return err
		}
		err = db.query(`SELECT count(*) FROM `+antigravityQuote(table), nil, func(stmt *sqlite.Stmt) error {
			rows := stmt.ColumnInt(0)
			rec.shape.Records += rows
			rec.shape.Kinds["rows:"+cursorKind(table, "table")] += rows
			return nil
		})
		if err != nil {
			return err
		}
		hasSteps = hasSteps || table == "steps"
	}
	if !hasSteps {
		return nil
	}
	return db.query(`SELECT step_type, status, count(*) FROM steps GROUP BY step_type, status LIMIT ?`,
		[]any{cursorMaxRows}, func(stmt *sqlite.Stmt) error {
			count := stmt.ColumnInt(2)
			rec.shape.Kinds["step_type:"+strconv.FormatInt(stmt.ColumnInt64(0), 10)] += count
			rec.shape.Kinds["status:"+strconv.FormatInt(stmt.ColumnInt64(1), 10)] += count
			return nil
		})
}

func antigravityQuote(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// antigravityIndexColumns are the only columns of conversation_summaries the
// probe reads, for the discovered session's row only.
var antigravityIndexColumns = []string{"step_count", "last_modified_time", "workspace_uris", "source", "project_id"}

// antigravityIndexTime is the layout of last_modified_time.
const antigravityIndexTime = "2006-01-02 15:04:05.999999999-07:00"

// antigravityShapeIndex reads this session's row of the conversation index:
// the step count, the update time, the workspace, the source, and whether a
// project is set. An index without the table, or without a row for this
// session, gives an empty shape.
func antigravityShapeIndex(ctx context.Context, src Source, rel, id string, rec *ShapeRecorder, meta *Metadata) error {
	db, err := antigravityOpen(ctx, src, rel, antigravityIndexDatabase)
	if err != nil {
		return err
	}
	defer db.Close()
	present := false
	err = db.query(`SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'conversation_summaries'`, nil, func(*sqlite.Stmt) error {
		present = true
		return nil
	})
	if err != nil || !present {
		return err
	}
	query := `SELECT ` + strings.Join(antigravityIndexColumns, ", ") + ` FROM conversation_summaries WHERE conversation_id = ? LIMIT 1`
	return db.query(query, []any{id}, func(stmt *sqlite.Stmt) error {
		rec.shape.Records++
		for i, column := range antigravityIndexColumns {
			rec.AddField(cursorFieldPath("$.conversation_summaries", column), antigravityValueType(stmt.ColumnType(i)))
		}
		rec.shape.Kinds["step_count"] += stmt.ColumnInt(0)
		if t, err := time.Parse(antigravityIndexTime, stmt.ColumnText(1)); err == nil {
			meta.Observe(t.UTC(), "")
		}
		meta.ProjectPath = antigravityWorkspacePath(stmt.ColumnText(2))
		source := stmt.ColumnText(3)
		if source == "" {
			source = "none"
		}
		rec.shape.Kinds["source:"+cursorKind(source, "other")]++
		return nil
	})
}

func antigravityValueType(columnType sqlite.ColumnType) JSONType {
	switch columnType {
	case sqlite.TypeInteger, sqlite.TypeFloat:
		return JSONNumber
	case sqlite.TypeNull:
		return JSONNull
	}
	return JSONString
}

// antigravityWorkspacePath returns the first workspace of a workspace_uris
// value, a JSON array of file URIs, as a path.
func antigravityWorkspacePath(raw string) string {
	value, err := decodeValue([]byte(raw))
	if err != nil {
		return ""
	}
	uris, _ := value.([]any)
	if len(uris) == 0 {
		return ""
	}
	uri, _ := uris[0].(string)
	return cursorIDEFolderPath(uri)
}

// antigravityDatabase names the kind of Antigravity SQLite file a read guard
// protects.
type antigravityDatabase string

const (
	// antigravityBodyDatabase is a conversation body, conversations/<id>.db.
	antigravityBodyDatabase antigravityDatabase = "body"
	// antigravityIndexDatabase is the conversation index,
	// conversation_summaries.db.
	antigravityIndexDatabase antigravityDatabase = "index"
)

// artifact names the declared artifact of the database kind, for errors.
func (d antigravityDatabase) artifact() string {
	if d == antigravityBodyDatabase {
		return antigravityBody.Name
	}
	return antigravityIndexDB.Name
}

// antigravityAnyColumn as a denial column means every column of the table,
// including a read that names no column, such as count(*).
const antigravityAnyColumn = "*"

// antigravityDenial names one read the Antigravity probes must never make.
// Column "*" means every column of the table. A Table that ends in "*" matches
// every table with that prefix.
type antigravityDenial struct {
	Database antigravityDatabase
	Table    string
	Column   string
}

// antigravityDeniedReads is the closed list of reads that can return
// conversation text. antigravityOpen refuses any statement that makes one.
// The test fixture testdata/antigravity_denied_reads.yaml holds the same set
// and one refused statement per entry.
var antigravityDeniedReads = []antigravityDenial{
	{antigravityBodyDatabase, "steps", "metadata"},
	{antigravityBodyDatabase, "steps", "error_details"},
	{antigravityBodyDatabase, "steps", "permissions"},
	{antigravityBodyDatabase, "steps", "task_details"},
	{antigravityBodyDatabase, "steps", "render_info"},
	{antigravityBodyDatabase, "steps", "step_payload"},
	{antigravityBodyDatabase, "battle_mode_infos", "data"},
	{antigravityBodyDatabase, "executor_metadata", "data"},
	{antigravityBodyDatabase, "gen_metadata", "data"},
	{antigravityBodyDatabase, "parent_references", "data"},
	{antigravityBodyDatabase, "trajectory_metadata_blob", "data"},
	{antigravityIndexDatabase, "conversation_summaries", "title"},
	{antigravityIndexDatabase, "conversation_summaries", "preview"},
	{antigravityIndexDatabase, "conversation_summaries", "raw_summary"},
	{antigravityIndexDatabase, "conversations_fts*", antigravityAnyColumn},
}

func (d antigravityDenial) matches(table, column string) bool {
	if prefix, ok := strings.CutSuffix(d.Table, "*"); ok {
		if len(table) < len(prefix) || !strings.EqualFold(table[:len(prefix)], prefix) {
			return false
		}
	} else if !strings.EqualFold(table, d.Table) {
		return false
	}
	return d.Column == antigravityAnyColumn || strings.EqualFold(column, d.Column)
}

// antigravityRule says why the read guard refuses a column.
type antigravityRule string

const (
	antigravityRuleList antigravityRule = "it is on the Antigravity deny list"
	antigravityRuleBlob antigravityRule = "it is a BLOB column of a conversation body"
)

type antigravityGuardEntry struct {
	denial antigravityDenial
	rule   antigravityRule
}

// antigravityRefusal is the first read the guard refused in one statement.
type antigravityRefusal struct {
	table, column string
	rule          antigravityRule
}

// antigravityGuard is the SQLite authorizer of one Antigravity connection.
// A denied read rejects the whole statement (AuthResultDeny); the guard never
// returns AuthResultIgnore, which would substitute NULL and hide the read.
type antigravityGuard struct {
	entries []antigravityGuardEntry
	refused *antigravityRefusal
}

func (g *antigravityGuard) Authorize(action sqlite.Action) sqlite.AuthResult {
	if action.Type() != sqlite.OpRead {
		return sqlite.AuthResultOK
	}
	for _, entry := range g.entries {
		if entry.denial.matches(action.Table(), action.Column()) {
			if g.refused == nil {
				g.refused = &antigravityRefusal{table: action.Table(), column: action.Column(), rule: entry.rule}
			}
			return sqlite.AuthResultDeny
		}
	}
	return sqlite.AuthResultOK
}

// antigravityDB is one guarded Antigravity connection. Every statement runs
// through query, which names a refused read in its error.
type antigravityDB struct {
	conn     *sqlite.Conn
	guard    *antigravityGuard
	rel      string
	database antigravityDatabase
}

// antigravityOpen opens one Antigravity SQLite file read-only, with a busy
// timeout and the caller's cancellation, and installs the read guard before it
// returns the connection. It is the only place the Antigravity code opens
// SQLite. For a body it first reads the table definitions and adds every BLOB
// column to the connection's denials, so a blob column a later product version
// adds is refused without a code change.
func antigravityOpen(ctx context.Context, src Source, rel string, database antigravityDatabase) (*antigravityDB, error) {
	if src.Dir == "" {
		return nil, fmt.Errorf("%s: reading SQLite needs an operating system directory", rel)
	}
	conn, err := sqlite.OpenConn(filepath.Join(src.Dir, filepath.FromSlash(rel)), sqlite.OpenReadOnly)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", rel, err)
	}
	conn.SetBusyTimeout(cursorBusyTimeout)
	conn.SetInterrupt(ctx.Done())
	guard := &antigravityGuard{}
	for _, denial := range antigravityDeniedReads {
		if denial.Database == database {
			guard.entries = append(guard.entries, antigravityGuardEntry{denial: denial, rule: antigravityRuleList})
		}
	}
	if database == antigravityBodyDatabase {
		blobs, err := antigravityBlobColumns(conn)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("read the table definitions of %s before installing the Antigravity read guard: %w", rel, err)
		}
		guard.entries = append(guard.entries, blobs...)
	}
	if err := conn.SetAuthorizer(guard); err != nil {
		conn.Close()
		return nil, fmt.Errorf("install the Antigravity read guard on %s: %w", rel, err)
	}
	return &antigravityDB{conn: conn, guard: guard, rel: rel, database: database}, nil
}

// antigravityBlobColumns returns a denial for every column of a body whose
// declared type is BLOB. It reads the schema only, never a row.
func antigravityBlobColumns(conn *sqlite.Conn) ([]antigravityGuardEntry, error) {
	var out []antigravityGuardEntry
	err := sqlitex.ExecuteTransient(conn,
		`SELECT m.name, p.name FROM sqlite_schema AS m JOIN pragma_table_info(m.name) AS p
		 WHERE m.type = 'table' AND upper(p.type) LIKE '%BLOB%' ORDER BY m.name, p.cid LIMIT ?`,
		&sqlitex.ExecOptions{Args: []any{cursorMaxRows}, ResultFunc: func(stmt *sqlite.Stmt) error {
			denial := antigravityDenial{Database: antigravityBodyDatabase, Table: stmt.ColumnText(0), Column: stmt.ColumnText(1)}
			out = append(out, antigravityGuardEntry{denial: denial, rule: antigravityRuleBlob})
			return nil
		}})
	return out, err
}

// query runs one statement. A statement the read guard refused fails with an
// error that names the refused table and column.
func (db *antigravityDB) query(query string, args []any, row func(*sqlite.Stmt) error) error {
	db.guard.refused = nil
	err := sqlitex.Execute(db.conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: row})
	if err == nil || db.guard.refused == nil || sqlite.ErrCode(err) != sqlite.ResultAuth {
		return err
	}
	refused := *db.guard.refused
	column := refused.column
	if column == "" {
		column = "(the whole table)"
	}
	return fmt.Errorf("the Antigravity read guard (internal/harnesslayout/antigravity_core.go) refused a read of column %s of table %s in %s while capturing the %s artifact, because %s: the column can hold conversation text. "+
		"No value was read and the capture of this artifact failed. Remove the column from the query; do not remove it from the guard: %w",
		column, refused.table, db.rel, db.database.artifact(), refused.rule, err)
}

// Close closes the connection.
func (db *antigravityDB) Close() error {
	return db.conn.Close()
}
