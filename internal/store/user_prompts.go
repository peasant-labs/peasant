package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// UserPrompt is one entry a person typed. Text is the stored text, whole when
// the session's capture holds it whole.
type UserPrompt struct {
	EntryIndex  int
	TimestampMS *int64
	Text        string
	Command     *PromptCommand // set when the entry is a slash-command or skill invocation
}

// PromptCommand is the command a prompt invoked. Name carries no leading slash.
type PromptCommand struct{ Name, Args string }

const (
	sqlUserPromptBodies = sqlShimBodyRows + `
WHERE m.session_id = ? AND m.partition_id = 0 AND m.generation_id = s.active_generation_id
AND b.role = 'user' AND b.depth = 0 AND b.entry_type = 'text'
ORDER BY m.entry_index`

	sqlUserPromptMirrorRows = `SELECT
    session_id, entry_index, provider, entry_type, role,
    timestamp_ms, content_preview, tokens_in, tokens_out,
    has_tool_use, tool_names_csv, has_thinking, is_error,
    raw_byte_length, tool_call_id, entry_id, parent_entry_id, extra,
    depth, parent_index, tool_input, tool_output,
    tool_kind, stop_reason, part_type
FROM session_entries
WHERE session_id = ? AND role = 'user' AND depth = 0 AND entry_type = 'text'
ORDER BY entry_index`
)

// ListUserPrompts returns the entries of one session that a person typed, in
// entry order. The selection is the stored fields role = user, depth = 0 and
// entry_type = text; this read adds no classification of its own. It reads
// either stored representation and validates the session's index format, as
// ListEntriesRange does, and it returns the whole stored text rather than the
// bounded preview. CaptureTextBounded says whether that text is the whole text
// the person typed.
func (s *Store) ListUserPrompts(ctx context.Context, sid schema.SessionID) (_ []UserPrompt, retErr error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)
	endSnapshot := sqlitex.Save(conn)
	defer endSnapshot(&retErr)
	if err := s.ValidateIndexFormatsOnConn(conn, []schema.SessionID{sid}); err != nil {
		return nil, err
	}
	shimmed, err := shimmedOnConn(conn, string(sid))
	if err != nil {
		return nil, err
	}
	var entries []schema.SessionEntry
	if shimmed {
		entries, err = userPromptBodiesOnConn(conn, sid)
	} else {
		entries, err = userPromptMirrorRowsOnConn(ctx, conn, sid)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list user prompts for %s: %w", sid, err)
	}
	prompts := make([]UserPrompt, 0, len(entries))
	for _, entry := range entries {
		prompt := UserPrompt{EntryIndex: entry.EntryIndex, TimestampMS: entry.TimestampMs}
		if entry.ContentPreview != nil {
			prompt.Text = *entry.ContentPreview
		}
		if command := ingest.CommandInvocationFromEntry(entry); command != nil {
			prompt.Command = &PromptCommand{Name: strings.TrimPrefix(command.Name, "/"), Args: command.Args}
		}
		prompts = append(prompts, prompt)
	}
	return prompts, nil
}

// userPromptBodiesOnConn reads the active generation's partition-0 body rows.
// A body row holds whatever text the capture stored, so the shim's preview
// bound is left off.
func userPromptBodiesOnConn(conn *sqlite.Conn, sid schema.SessionID) ([]schema.SessionEntry, error) {
	var records []EntryRecord
	if err := sqlitex.ExecuteTransient(conn, sqlUserPromptBodies, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			record, err := scanBodyRow(stmt, 0)
			if err != nil {
				return err
			}
			records = append(records, record)
			return nil
		},
	}); err != nil {
		return nil, err
	}
	return shimListEntries(records, false)
}

// userPromptMirrorRowsOnConn reads the mirror rows. A mirror row holds only the
// bounded preview, so a full capture's text comes from its verified chunks.
func userPromptMirrorRowsOnConn(ctx context.Context, conn *sqlite.Conn, sid schema.SessionID) ([]schema.SessionEntry, error) {
	capture, found, err := readCapture(conn, sid)
	if err != nil {
		return nil, err
	}
	var entries []schema.SessionEntry
	if err := sqlitex.ExecuteTransient(conn, sqlUserPromptMirrorRows, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			entry := scanSessionEntry(stmt)
			if _, _, err := ingest.DecodePiEntryExtra(entry); err != nil {
				return err
			}
			entries = append(entries, entry)
			return nil
		},
	}); err != nil {
		return nil, err
	}
	if !found || capture.CaptureFormat != ingest.ContentCaptureFormatFull {
		return entries, nil
	}
	for i := range entries {
		if err := hydrateContent(ctx, conn, &entries[i]); err != nil {
			return nil, err
		}
	}
	return entries, nil
}
