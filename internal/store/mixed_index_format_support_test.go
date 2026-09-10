package store_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/mixed_index_formats.yaml
var mixedIndexFormatsYAML []byte

type mixedFormatOperation string

const (
	mixedReopen       mixedFormatOperation = "reopen"
	mixedRewrite      mixedFormatOperation = "rewrite"
	mixedRepair       mixedFormatOperation = "repair"
	mixedConvert      mixedFormatOperation = "convert"
	mixedBatch        mixedFormatOperation = "batch"
	mixedDowngrade    mixedFormatOperation = "downgrade"
	mixedUnsupported  mixedFormatOperation = "unsupported"
	mixedPipeline     mixedFormatOperation = "pipeline"
	mixedRegistration mixedFormatOperation = "registration"
)

type mixedFormatFault string

const (
	mixedNoFault          mixedFormatFault = ""
	mixedConvertError     mixedFormatFault = "convert-error"
	mixedWrongResult      mixedFormatFault = "wrong-result"
	mixedAlterHistory     mixedFormatFault = "alter-history"
	mixedNoEdge           mixedFormatFault = "no-edge"
	mixedNoIndex          mixedFormatFault = "no-index"
	mixedUnknownSource    mixedFormatFault = "unknown-source"
	mixedInvalidPayload   mixedFormatFault = "invalid-payload"
	mixedWriteError       mixedFormatFault = "write-error"
	mixedCommitError      mixedFormatFault = "commit-error"
	mixedPanic            mixedFormatFault = "panic"
	mixedDuplicateHandler mixedFormatFault = "duplicate-handler"
	mixedDuplicateEdge    mixedFormatFault = "duplicate-edge"
	mixedMissingHandler   mixedFormatFault = "missing-handler"
	mixedReverse          mixedFormatFault = "reverse"
	mixedNilConverter     mixedFormatFault = "nil-converter"
)

type mixedEncoding int

const (
	mixedReversed    mixedEncoding = 1
	mixedReversedXOR mixedEncoding = 2
)

type mixedSpeaker int

const (
	mixedSpeakerUser      mixedSpeaker = 1
	mixedSpeakerAssistant mixedSpeaker = 2
)

type mixedFormatRecord struct {
	Name        string       `yaml:"name"`
	Ordinal     int          `yaml:"ordinal"`
	Speaker     mixedSpeaker `yaml:"speaker"`
	Text        string       `yaml:"text"`
	EntryID     string       `yaml:"entryID"`
	Model       string       `yaml:"model"`
	Command     string       `yaml:"command"`
	CommandArgs string       `yaml:"commandArgs"`
	RawBytes    *int
	Timestamp   *int64
}
type mixedFormatCase struct {
	Name            string               `yaml:"name"`
	Operation       mixedFormatOperation `yaml:"operation"`
	Fault           mixedFormatFault     `yaml:"fault"`
	Producer        int                  `yaml:"producer"`
	UnknownProducer bool                 `yaml:"unknownProducer"`
	Empty           bool                 `yaml:"empty"`
	// UnboundCapture gives the session a CURRENT publication capture whose
	// index write never bound to it, which is the state a conversion must not
	// silently repair.
	UnboundCapture bool `yaml:"unboundCapture"`
}
type mixedFormatDocument struct {
	LegacySession       schema.SessionID    `yaml:"legacySession"`
	TargetSession       schema.SessionID    `yaml:"targetSession"`
	LaterSession        schema.SessionID    `yaml:"laterSession"`
	Project             schema.ProjectHash  `yaml:"project"`
	RequiredRecordNames []string            `yaml:"requiredRecordNames"`
	Records             []mixedFormatRecord `yaml:"records"`
	CodexTranscript     string              `yaml:"codexTranscript"`
	RequiredNames       []string            `yaml:"requiredNames"`
	Cases               []mixedFormatCase   `yaml:"cases"`
}

func loadMixedIndexFormatFixtures(t *testing.T) mixedFormatDocument {
	t.Helper()
	var document mixedFormatDocument
	decoder := yaml.NewDecoder(bytes.NewReader(mixedIndexFormatsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("mixed format fixture requires one document: %v", err)
	}
	names, records := make(map[string]bool), make(map[string]bool)
	for _, row := range document.Cases {
		if row.Name == "" || names[row.Name] {
			t.Fatalf("invalid mixed format case %+v", row)
		}
		switch row.Operation {
		case mixedReopen, mixedRewrite, mixedRepair, mixedConvert, mixedBatch, mixedDowngrade, mixedUnsupported, mixedPipeline, mixedRegistration:
		default:
			t.Fatalf("unknown mixed operation %q", row.Operation)
		}
		switch row.Fault {
		case mixedNoFault, mixedConvertError, mixedWrongResult, mixedAlterHistory, mixedNoEdge, mixedNoIndex, mixedUnknownSource, mixedInvalidPayload, mixedWriteError, mixedCommitError, mixedPanic, mixedDuplicateHandler, mixedDuplicateEdge, mixedMissingHandler, mixedReverse, mixedNilConverter:
		default:
			t.Fatalf("unknown mixed fault %q", row.Fault)
		}
		names[row.Name] = true
	}
	for _, row := range document.Records {
		if row.Name == "" || records[row.Name] || row.Ordinal < 0 || row.Text == "" || (row.Speaker != mixedSpeakerUser && row.Speaker != mixedSpeakerAssistant) {
			t.Fatalf("invalid mixed record %+v", row)
		}
		records[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("mixed index formats", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	if err := testutil.RequireFixtureNames("mixed index formats", "record", document.RequiredRecordNames, records); err != nil {
		t.Fatal(err)
	}
	return document
}

// mixedV2 stores role codes and encoded text in a separate relational table.
// It is deliberately not V1 plus a different number, and is never registered
// outside tests. Only its declared text-message subset can be converted.
type mixedV2 struct {
	Harness  schema.Harness
	Encoding mixedEncoding
	Records  []mixedFormatRecord
}

func (mixedV2) IndexVersion() int { return 2 }

var _ indexformat.Result = mixedV2{}

type mixedFormatHandler struct{ fault mixedFormatFault }

func (mixedFormatHandler) Version() int { return 2 }

var _ store.IndexFormat = mixedFormatHandler{}

const mixedFormatTables = `
CREATE TABLE test_packed_index (
 session_id TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
 ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
 speaker INTEGER NOT NULL CHECK (speaker IN (1,2)),
 body BLOB NOT NULL,
 encoding INTEGER NOT NULL CHECK (encoding IN (1,2)),
 harness TEXT NOT NULL,
 entry_id TEXT, model_id TEXT, command_name TEXT, command_args TEXT,
 raw_bytes INTEGER, timestamp_ms INTEGER,
 PRIMARY KEY (session_id, ordinal)
) STRICT;
CREATE TABLE test_deferred_index_constraint (
 session_id TEXT REFERENCES sessions(session_id) DEFERRABLE INITIALLY DEFERRED
) STRICT;
`

func (mixedFormatHandler) Validate(result indexformat.Result) error {
	value, ok := result.(mixedV2)
	if !ok {
		return fmt.Errorf("test format 2 requires mixedV2, got %T", result)
	}
	if !value.Harness.IsKnown() || (value.Encoding != mixedReversed && value.Encoding != mixedReversedXOR) {
		return fmt.Errorf("invalid packed representation header")
	}
	seen := make(map[int]bool)
	for _, row := range value.Records {
		if row.Ordinal < 0 || seen[row.Ordinal] || (row.Speaker != mixedSpeakerUser && row.Speaker != mixedSpeakerAssistant) {
			return fmt.Errorf("invalid packed record")
		}
		seen[row.Ordinal] = true
	}
	return nil
}

func (handler mixedFormatHandler) Write(ctx context.Context, conn *sqlite.Conn, sid schema.SessionID, result indexformat.Result) ([]schema.SessionEntry, error) {
	if err := handler.Validate(result); err != nil {
		return nil, err
	}
	value := result.(mixedV2)
	if err := handler.Delete(ctx, conn, sid); err != nil {
		return nil, err
	}
	for _, row := range value.Records {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO test_packed_index
(session_id, ordinal, speaker, body, encoding, harness, entry_id, model_id, command_name, command_args, raw_bytes, timestamp_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sid), row.Ordinal, int(row.Speaker), encodeMixedBody(row.Text, value.Encoding), int(value.Encoding), string(value.Harness),
			mixedOptionalString(row.EntryID), mixedOptionalString(row.Model), mixedOptionalString(row.Command), mixedOptionalString(row.CommandArgs), mixedOptionalInt(row.RawBytes), mixedOptionalInt64(row.Timestamp),
		}}); err != nil {
			return nil, err
		}
	}
	if value.Encoding == mixedReversedXOR {
		switch handler.fault {
		case mixedWriteError:
			return nil, fmt.Errorf("synthetic packed write failure")
		case mixedPanic:
			panic("synthetic packed handler panic")
		case mixedCommitError:
			if err := sqlitex.ExecuteTransient(conn, "INSERT INTO test_deferred_index_constraint VALUES ('a4000000-0000-4000-8000-ffffffffffff')", nil); err != nil {
				return nil, err
			}
		}
	}
	return mixedEntries(sid, value.Harness, value.Records), nil
}
func (mixedFormatHandler) Delete(_ context.Context, conn *sqlite.Conn, sid schema.SessionID) error {
	return sqlitex.ExecuteTransient(conn, "DELETE FROM test_packed_index WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(sid)}})
}
func encodeMixedBody(text string, encoding mixedEncoding) []byte {
	body := []byte(text)
	for i, j := 0, len(body)-1; i < j; i, j = i+1, j-1 {
		body[i], body[j] = body[j], body[i]
	}
	if encoding == mixedReversedXOR {
		for i := range body {
			body[i] ^= 0x55
		}
	}
	return body
}
func mixedOptionalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func mixedOptionalInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
func mixedOptionalInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func mixedEntries(sid schema.SessionID, harness schema.Harness, records []mixedFormatRecord) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, 0, len(records))
	for _, record := range records {
		role := schema.RoleUser
		if record.Speaker == mixedSpeakerAssistant {
			role = schema.RoleAssistant
		}
		entry := schema.SessionEntry{SessionID: sid, Harness: harness, EntryIndex: record.Ordinal, EntryType: schema.EntryTypeText, Role: role, ContentPreview: strPtr(record.Text), RawByteLength: record.RawBytes, TimestampMs: record.Timestamp}
		if record.EntryID != "" {
			entry.EntryID = strPtr(record.EntryID)
		}
		extra := make(map[string]string)
		if record.Model != "" {
			extra["model_id"] = record.Model
		}
		if record.Command != "" {
			extra["command_name"] = record.Command
		}
		if record.CommandArgs != "" {
			extra["command_args"] = record.CommandArgs
		}
		if len(extra) > 0 {
			data, _ := json.Marshal(extra)
			entry.Extra = strPtr(string(data))
		}
		entries = append(entries, entry)
	}
	return entries
}

func mixedConversion(fault mixedFormatFault) store.IndexFormatConversion {
	return store.IndexFormatConversion{FromVersion: 1, ToVersion: 2, Convert: func(_ context.Context, conn *sqlite.Conn, sid schema.SessionID) (indexformat.Result, error) {
		value := mixedV2{Encoding: mixedReversed}
		if err := sqlitex.ExecuteTransient(conn, "SELECT model_harness FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error { return value.Harness.UnmarshalText([]byte(stmt.ColumnText(0))) }}); err != nil {
			return nil, err
		}
		if err := sqlitex.ExecuteTransient(conn, `SELECT e.entry_index, e.provider, e.entry_type, e.role, e.content_preview,
e.entry_id, ext.value_text, cmd.command_name, cmd.command_args, e.raw_byte_length, e.timestamp_ms,
e.depth, e.has_tool_use, e.has_thinking, e.is_error, e.parent_index, e.parent_entry_id, e.tool_input, e.tool_output, e.tool_call_id,
e.tokens_in, e.tokens_out, e.tool_names_csv, e.tool_kind, e.stop_reason, e.part_type, e.extra,
EXISTS (SELECT 1 FROM session_entries_ext other WHERE other.session_id = e.session_id AND other.entry_index = e.entry_index AND other.key != 'model_id')
FROM session_entries e
LEFT JOIN session_entries_ext ext ON ext.session_id = e.session_id AND ext.entry_index = e.entry_index AND ext.key = 'model_id'
LEFT JOIN session_commands cmd ON cmd.session_id = e.session_id AND cmd.entry_index = e.entry_index
WHERE e.session_id = ? ORDER BY e.entry_index`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnText(2) != "text" || (stmt.ColumnText(3) != "user" && stmt.ColumnText(3) != "assistant") || stmt.ColumnType(4) == sqlite.TypeNull {
				return fmt.Errorf("test format cannot faithfully convert this entry kind")
			}
			for column := 11; column <= 14; column++ {
				if stmt.ColumnInt(column) != 0 {
					return fmt.Errorf("test format cannot discard entry flags/depth")
				}
			}
			for column := 15; column <= 19; column++ {
				if stmt.ColumnType(column) != sqlite.TypeNull {
					return fmt.Errorf("test format cannot discard tool or parent fields")
				}
			}
			for column := 20; column <= 25; column++ {
				if stmt.ColumnType(column) != sqlite.TypeNull {
					return fmt.Errorf("test format cannot discard token or tool fields")
				}
			}
			if stmt.ColumnInt(27) != 0 {
				return fmt.Errorf("test format cannot discard additional ext fields")
			}
			if stmt.ColumnType(26) != sqlite.TypeNull {
				var extra map[string]any
				if err := json.Unmarshal([]byte(stmt.ColumnText(26)), &extra); err != nil {
					return fmt.Errorf("test format cannot discard invalid extra metadata: %w", err)
				}
				for key := range extra {
					if key != "model_id" && key != "command_name" && key != "command_args" {
						return fmt.Errorf("test format cannot discard extra field %s", key)
					}
				}
			}
			if stmt.ColumnText(1) != string(value.Harness) {
				return fmt.Errorf("test format cannot discard per-entry harness identity")
			}
			record := mixedFormatRecord{Ordinal: stmt.ColumnInt(0), Speaker: mixedSpeakerUser, Text: stmt.ColumnText(4), EntryID: stmt.ColumnText(5), Model: stmt.ColumnText(6), Command: stmt.ColumnText(7), CommandArgs: stmt.ColumnText(8)}
			if stmt.ColumnText(3) == "assistant" {
				record.Speaker = mixedSpeakerAssistant
			}
			if stmt.ColumnType(9) != sqlite.TypeNull {
				record.RawBytes = intPtr(stmt.ColumnInt(9))
			}
			if stmt.ColumnType(10) != sqlite.TypeNull {
				record.Timestamp = int64Ptr(stmt.ColumnInt64(10))
			}
			value.Records = append(value.Records, record)
			return nil
		}}); err != nil {
			return nil, err
		}
		if fault == mixedConvertError || fault == mixedWrongResult || fault == mixedAlterHistory {
			if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET ingested_ms = ingested_ms + 1 WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
				return nil, err
			}
		}
		if fault == mixedConvertError {
			return nil, fmt.Errorf("synthetic conversion failure")
		}
		if fault == mixedWrongResult {
			return indexformat.V1{}, nil
		}
		if fault == mixedAlterHistory {
			if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET index_version = index_version + 1 WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
				return nil, err
			}
		}
		return value, nil
	}}
}

func seedMixedSession(t *testing.T, db *store.Store, sid schema.SessionID, harness schema.Harness, project schema.ProjectHash) *schema.UnifiedMetadata {
	t.Helper()
	ingestedAt := int64(3)
	meta := &schema.UnifiedMetadata{SchemaVersion: ingest.CurrentSchemaVersion, SessionID: sid, ModelHarness: harness, Model: schema.ModelID("gpt-4o"), HostSlug: schema.HostSlug("mixed-fixture"), Project: schema.ProjectContext{Hash: project, Name: "mixed", FilePath: "/synthetic/mixed"}, Timestamp: schema.TimestampInfo{Start: 1, End: 2, Ingested: &ingestedAt}, Source: schema.SourceInfo{FilePath: "/synthetic/source.jsonl", Format: schema.SourceFormatJSONL}}
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	return meta
}
