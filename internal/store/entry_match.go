package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// entryMatchBatchSize is how many entries one read of a session asks for. Rows
// are matched and dropped one at a time, so memory follows the largest entry
// rather than the session.
const entryMatchBatchSize = 200

// The fields a match can land in.
const (
	EntryMatchContent    = "content"
	EntryMatchToolInput  = "tool_input"
	EntryMatchToolOutput = "tool_output"
)

const snippetEllipsis = "…"

// EntryMatchQuery is one search. SessionIDs is the whole scope, in the order
// to read it: an empty scope reads nothing, so it never widens to every
// session. A limit of 0 means none.
type EntryMatchQuery struct {
	SessionIDs   []schema.SessionID
	Pattern      *regexp.Regexp
	Roles        []schema.Role      // empty = every role
	EntryTypes   []schema.EntryType // empty = every type
	MaxMatches   int
	PerSession   int
	SnippetChars int
}

// EntryMatch is the first hit of the pattern in one field of one entry.
type EntryMatch struct {
	SessionID        schema.SessionID
	EntryIndex       int
	Role             string
	Depth            int
	Field            string // "content" | "tool_input" | "tool_output"
	Offset           int    // byte offset of the match inside Field
	Snippet          string
	SnippetTruncated bool
}

// EntryMatchSkip is a session the search could not read to its end. Matches
// found before the failure stay in the result.
type EntryMatchSkip struct {
	SessionID schema.SessionID
	Reason    string
}

// EntryMatchResult is one search's outcome.
type EntryMatchResult struct {
	Matches         []EntryMatch
	SessionsScanned int
	// HitMaxMatches and HitPerSession say a limit dropped at least one match.
	HitMaxMatches bool
	HitPerSession bool
	// BoundedSessions holds the scanned sessions whose stored text is a
	// bounded preview, so a match past the bound cannot be found.
	BoundedSessions []schema.SessionID
	Skipped         []EntryMatchSkip
}

// errEntryMatchStop ends a row walk once a limit is met.
var errEntryMatchStop = errors.New("entry match limit reached")

// MatchEntries searches content, tool input and tool output in the given
// sessions, in the order given. It streams: each session is read in batches
// under its own snapshot and each row is dropped once matched, so a search
// never holds a whole session. It reads either stored representation, validates
// each session's index format as ListEntriesRange does, and reads the whole
// stored text rather than the bounded preview. A session that cannot be served
// is reported in Skipped and does not stop the search. The only errors returned
// are a cancelled context and a store that cannot give a connection.
func (s *Store) MatchEntries(ctx context.Context, q EntryMatchQuery) (EntryMatchResult, error) {
	result := EntryMatchResult{Matches: []EntryMatch{}}
	for _, id := range q.SessionIDs {
		if result.HitMaxMatches {
			break
		}
		if err := s.matchSession(ctx, id, q, &result); err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			result.Skipped = append(result.Skipped, EntryMatchSkip{SessionID: id, Reason: err.Error()})
			continue
		}
		result.SessionsScanned++
	}
	return result, nil
}

func (s *Store) matchSession(ctx context.Context, id schema.SessionID, q EntryMatchQuery, result *EntryMatchResult) (retErr error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)
	endSnapshot := sqlitex.Save(conn)
	defer endSnapshot(&retErr)
	if err := s.ValidateIndexFormatsOnConn(conn, []schema.SessionID{id}); err != nil {
		return err
	}
	shimmed, err := shimmedOnConn(conn, string(id))
	if err != nil {
		return err
	}
	effective, found, err := effectiveCaptureOnConn(conn, id)
	if err != nil {
		return err
	}
	if CaptureTextBounded(effective, found) {
		result.BoundedSessions = append(result.BoundedSessions, id)
	}
	stored, storedFound, err := readCapture(conn, id)
	if err != nil {
		return err
	}
	walk := entryWalk{
		ctx: ctx, conn: conn, id: id, shimmed: shimmed,
		chunks: storedFound && stored.CaptureFormat == ingest.ContentCaptureFormatFull,
		roles:  q.Roles, types: q.EntryTypes,
	}
	inSession := 0
	err = walk.each(func(entry schema.SessionEntry) error {
		for _, field := range matchableFields(entry) {
			loc := q.Pattern.FindStringIndex(field.text)
			if loc == nil {
				continue
			}
			if q.MaxMatches > 0 && len(result.Matches) == q.MaxMatches {
				result.HitMaxMatches = true
				return errEntryMatchStop
			}
			if q.PerSession > 0 && inSession == q.PerSession {
				result.HitPerSession = true
				return errEntryMatchStop
			}
			snippet, cut := matchSnippet(field.text, loc[0], loc[1], q.SnippetChars)
			result.Matches = append(result.Matches, EntryMatch{
				SessionID: id, EntryIndex: entry.EntryIndex, Role: string(entry.Role), Depth: entry.Depth,
				Field: field.name, Offset: loc[0], Snippet: snippet, SnippetTruncated: cut,
			})
			inSession++
		}
		return nil
	})
	if errors.Is(err, errEntryMatchStop) {
		return nil
	}
	return err
}

type matchableField struct{ name, text string }

// matchableFields lists an entry's searchable text. Ingest also stores a tool
// call's input and output as its content, whole or cut to a preview, so that
// copy is left out and one hit is reported once, under the tool field.
func matchableFields(entry schema.SessionEntry) []matchableField {
	fields := make([]matchableField, 0, 3)
	input, output := "", ""
	if entry.ToolInput != nil {
		input = *entry.ToolInput
	}
	if entry.ToolOutput != nil {
		output = *entry.ToolOutput
	}
	if entry.ContentPreview != nil {
		content := *entry.ContentPreview
		if content != "" && !strings.HasPrefix(input, content) && !strings.HasPrefix(output, content) {
			fields = append(fields, matchableField{EntryMatchContent, content})
		}
	}
	if input != "" {
		fields = append(fields, matchableField{EntryMatchToolInput, input})
	}
	if output != "" {
		fields = append(fields, matchableField{EntryMatchToolOutput, output})
	}
	return fields
}

// matchSnippet returns a window of at most maxRunes runes around the match at
// text[start:end], with an ellipsis on each side that was cut. A match longer
// than the window keeps its beginning.
func matchSnippet(text string, start, end, maxRunes int) (string, bool) {
	matchRunes := utf8.RuneCountInString(text[start:end])
	if matchRunes >= maxRunes {
		from, to := start, advanceRunes(text, start, maxRunes)
		return wrapSnippet(text, from, to)
	}
	slack := maxRunes - matchRunes
	from, used := retreatRunes(text, start, slack/2)
	to, usedRight := advanceRunesCounted(text, end, slack-used)
	if left := slack - used - usedRight; left > 0 {
		from, _ = retreatRunes(text, from, left)
	}
	return wrapSnippet(text, from, to)
}

func wrapSnippet(text string, from, to int) (string, bool) {
	snippet := strings.ToValidUTF8(text[from:to], "�")
	if from > 0 {
		snippet = snippetEllipsis + snippet
	}
	if to < len(text) {
		snippet += snippetEllipsis
	}
	return snippet, from > 0 || to < len(text)
}

func advanceRunes(text string, at, count int) int {
	to, _ := advanceRunesCounted(text, at, count)
	return to
}

func advanceRunesCounted(text string, at, count int) (int, int) {
	moved := 0
	for moved < count && at < len(text) {
		_, size := utf8.DecodeRuneInString(text[at:])
		at += size
		moved++
	}
	return at, moved
}

func retreatRunes(text string, at, count int) (int, int) {
	moved := 0
	for moved < count && at > 0 {
		_, size := utf8.DecodeLastRuneInString(text[:at])
		at -= size
		moved++
	}
	return at, moved
}

// entryWalk streams one session's entries inside the caller's snapshot.
type entryWalk struct {
	ctx     context.Context
	conn    *sqlite.Conn
	id      schema.SessionID
	shimmed bool
	chunks  bool // a mirror session's full text lives in verified chunks
	roles   []schema.Role
	types   []schema.EntryType
}

// each calls fn for every selected entry, in entry order, one batch at a time.
func (w entryWalk) each(fn func(schema.SessionEntry) error) error {
	after := -1
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		rows := 0
		visit := func(entry schema.SessionEntry) error {
			rows++
			after = entry.EntryIndex
			return fn(entry)
		}
		var err error
		if w.shimmed {
			err = w.readBodies(after, visit)
		} else {
			err = w.readMirror(after, visit)
		}
		if err != nil {
			return err
		}
		if rows < entryMatchBatchSize {
			return nil
		}
	}
}

func (w entryWalk) filterArgs(column string, args []any) (string, []any) {
	clause := ""
	if len(w.roles) > 0 {
		clause += " AND " + column + "role IN (?" + strings.Repeat(",?", len(w.roles)-1) + ")"
		for _, role := range w.roles {
			args = append(args, string(role))
		}
	}
	if len(w.types) > 0 {
		clause += " AND " + column + "entry_type IN (?" + strings.Repeat(",?", len(w.types)-1) + ")"
		for _, entryType := range w.types {
			args = append(args, string(entryType))
		}
	}
	return clause, args
}

func (w entryWalk) readBodies(after int, visit func(schema.SessionEntry) error) error {
	clause, args := w.filterArgs("b.", []any{string(w.id), after})
	query := sqlShimBodyRows + `
WHERE m.session_id = ? AND m.partition_id = 0 AND m.generation_id = s.active_generation_id
AND m.entry_index > ?` + clause + ` ORDER BY m.entry_index LIMIT ?`
	return sqlitex.ExecuteTransient(w.conn, query, &sqlitex.ExecOptions{
		Args: append(args, entryMatchBatchSize),
		ResultFunc: func(stmt *sqlite.Stmt) error {
			record, err := scanBodyRow(stmt, 0)
			if err != nil {
				return err
			}
			entries, err := shimListEntries([]EntryRecord{record}, false)
			if err != nil {
				return err
			}
			return visit(entries[0])
		},
	})
}

func (w entryWalk) readMirror(after int, visit func(schema.SessionEntry) error) error {
	clause, args := w.filterArgs("", []any{string(w.id), after})
	query := `SELECT
    session_id, entry_index, provider, entry_type, role,
    timestamp_ms, content_preview, tokens_in, tokens_out,
    has_tool_use, tool_names_csv, has_thinking, is_error,
    raw_byte_length, tool_call_id, entry_id, parent_entry_id, extra,
    depth, parent_index, tool_input, tool_output,
    tool_kind, stop_reason, part_type
FROM session_entries
WHERE session_id = ? AND entry_index > ?` + clause + ` ORDER BY entry_index LIMIT ?`
	return sqlitex.ExecuteTransient(w.conn, query, &sqlitex.ExecOptions{
		Args: append(args, entryMatchBatchSize),
		ResultFunc: func(stmt *sqlite.Stmt) error {
			entry := scanSessionEntry(stmt)
			if _, _, err := ingest.DecodePiEntryExtra(entry); err != nil {
				return err
			}
			if w.chunks {
				if err := hydrateContent(w.ctx, w.conn, &entry); err != nil {
					return err
				}
			}
			return visit(entry)
		},
	})
}
