package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/opencode_session_parts.yaml
var openCodeSessionPartsYAML []byte

type openCodeSessionPartsCase struct {
	Name                   string   `yaml:"name"`
	SourceFixture          string   `yaml:"source_fixture"`
	SessionID              string   `yaml:"session_id"`
	SplitMessageID         string   `yaml:"split_message_id"`
	LegacyMessageID        string   `yaml:"legacy_message_id"`
	ExpectedPartOrdinals   []int64  `yaml:"expected_part_ordinals"`
	ExpectedPartTypes      []string `yaml:"expected_part_types"`
	ExpectedThinking       string   `yaml:"expected_thinking"`
	ExpectedSplitText      string   `yaml:"expected_split_text"`
	ExpectedToolName       string   `yaml:"expected_tool_name"`
	ExpectedToolArguments  string   `yaml:"expected_tool_arguments"`
	ExpectedToolResult     string   `yaml:"expected_tool_result"`
	ExpectedInlineText     string   `yaml:"expected_inline_text"`
	ExpectContentOmitted   bool     `yaml:"expect_content_omitted"`
	MutatePartOrdinal      int64    `yaml:"mutate_part_ordinal"`
	ExpectedOmittedWarning string   `yaml:"expected_omitted_warning"`
}

type openCodeSessionPartsDocument struct {
	RequiredCases []string                   `yaml:"required_cases"`
	Cases         []openCodeSessionPartsCase `yaml:"cases"`
}

func loadOpenCodeSessionPartsDocument(t testing.TB, data []byte) openCodeSessionPartsDocument {
	t.Helper()
	var document openCodeSessionPartsDocument
	if err := testutil.DecodeFixtureYAML(data, &document); err != nil {
		t.Fatalf("decode OpenCode session parts fixture: %v", err)
	}
	if len(document.RequiredCases) == 0 {
		t.Fatal("OpenCode session parts fixture declares no required cases")
	}
	present := make(map[string]struct{}, len(document.Cases))
	for _, testCase := range document.Cases {
		if testCase.Name == "" || testCase.SourceFixture == "" || testCase.SessionID == "" || testCase.SplitMessageID == "" || testCase.LegacyMessageID == "" {
			t.Fatalf("OpenCode session parts fixture has an incomplete case: %+v", testCase)
		}
		if len(testCase.ExpectedPartOrdinals) != len(testCase.ExpectedPartTypes) {
			t.Fatalf("case %q declares %d part ordinals but %d part types", testCase.Name, len(testCase.ExpectedPartOrdinals), len(testCase.ExpectedPartTypes))
		}
		if _, duplicate := present[testCase.Name]; duplicate {
			t.Fatalf("OpenCode session parts fixture has a duplicate case name %q", testCase.Name)
		}
		present[testCase.Name] = struct{}{}
	}
	for _, name := range document.RequiredCases {
		if _, ok := present[name]; !ok {
			t.Fatalf("OpenCode session parts fixture is missing required case %q", name)
		}
	}
	return document
}

// openCodeSessionPartsProjection is the subset of the managed current
// projection this regression inspects: the omission flag and each message's
// ordered parts.
type openCodeSessionPartsProjection struct {
	ContentOmitted bool `json:"content_omitted"`
	Messages       []struct {
		ID    string `json:"id"`
		Parts []struct {
			Data json.RawMessage `json:"data"`
		} `json:"parts"`
	} `json:"messages"`
}

const (
	openCodeSessionPartsText      = "text"
	openCodeSessionPartsReasoning = "reasoning"
	openCodeSessionPartsTool      = "tool"
)

// TestOpenCodeCurrentSessionPartsAreReadAndBounded proves the source reader
// attaches split session_part rows to the assistant message they belong to, in
// ordinal order, while leaving a legacy inline row untouched. It also proves
// the part payloads are included in the paged read's byte accounting and the
// change marker, so a preview bounds and detects a split session correctly.
func TestOpenCodeCurrentSessionPartsAreReadAndBounded(t *testing.T) {
	document := loadOpenCodeSessionPartsDocument(t, openCodeSessionPartsYAML)
	for _, testCase := range document.Cases {
		if testCase.MutatePartOrdinal >= 0 {
			// The omission case is exercised through the projection and capture
			// path below, where the dropped row is observable.
			continue
		}
		t.Run(testCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, testCase.SourceFixture)
			before := testfixture.SnapshotSource(t, materialized)
			source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
			defer closeSyntheticSource(t, source)

			var rows []ingest.OpenCodeCurrentMessageRow
			var cursor *ingest.OpenCodeCurrentCursor
			for {
				page, err := source.CurrentMessages(t.Context(), ingest.OpenCodeCurrentPageRequest{
					SessionID: mustCurrentSessionID(t, testCase.SessionID),
					PageSize:  mustCurrentPageSize(t, 2),
					After:     cursor,
				})
				if err != nil {
					t.Fatalf("read current session parts page: %v", err)
				}
				if len(page.DroppedParts) != 0 {
					t.Fatalf("read current session parts page dropped %d part rows, want none", len(page.DroppedParts))
				}
				rows = append(rows, page.Messages...)
				if page.Next == nil {
					break
				}
				cursor = page.Next
			}
			if len(rows) != 3 {
				t.Fatalf("current session read %d messages, want the user row, the split assistant, and the legacy inline assistant", len(rows))
			}
			split := currentRowByID(t, rows, testCase.SplitMessageID)
			assertCurrentPartOrder(t, split, testCase.ExpectedPartOrdinals, testCase.ExpectedPartTypes)
			for _, part := range split.Parts {
				switch part.Type.String() {
				case openCodeSessionPartsReasoning:
					if !strings.Contains(part.Data, testCase.ExpectedThinking) {
						t.Errorf("split reasoning part data %q does not carry %q", part.Data, testCase.ExpectedThinking)
					}
				case openCodeSessionPartsText:
					if !strings.Contains(part.Data, testCase.ExpectedSplitText) {
						t.Errorf("split text part data %q does not carry %q", part.Data, testCase.ExpectedSplitText)
					}
				case openCodeSessionPartsTool:
					if !strings.Contains(part.Data, testCase.ExpectedToolResult) {
						t.Errorf("split tool part data %q does not carry %q", part.Data, testCase.ExpectedToolResult)
					}
				}
			}
			if got := currentRowByID(t, rows, testCase.LegacyMessageID); len(got.Parts) != 0 {
				t.Fatalf("legacy inline message carries %d split parts, want none", len(got.Parts))
			}

			var wantBytes int64
			var wantParts int64
			for _, row := range rows {
				wantBytes += int64(len(row.Data))
				for _, part := range row.Parts {
					wantBytes += int64(len(part.Data))
					wantParts++
				}
			}
			probed, err := source.CurrentSessionPayloadSize(t.Context(), mustCurrentSessionID(t, testCase.SessionID))
			if err != nil {
				t.Fatalf("probe current session payload size: %v", err)
			}
			if probed.Rows != int64(len(rows)) || probed.Bytes != wantBytes {
				t.Errorf("payload probe = %d rows / %d bytes, want %d message rows / %d bytes including split parts", probed.Rows, probed.Bytes, len(rows), wantBytes)
			}

			lengths, err := source.(ingest.OpenCodeCurrentRowLengthSource).CurrentMessageLengths(t.Context(), ingest.OpenCodeCurrentPageRequest{
				SessionID: mustCurrentSessionID(t, testCase.SessionID),
				PageSize:  mustCurrentPageSize(t, ingest.MaxOpenCodeCurrentPageSize),
			})
			if err != nil {
				t.Fatalf("probe current message lengths: %v", err)
			}
			for _, row := range rows {
				want := int64(len(row.Data))
				for _, part := range row.Parts {
					want += int64(len(part.Data))
				}
				got := int64(-1)
				for _, length := range lengths {
					if length.Seq == row.Seq {
						got = length.Bytes
						break
					}
				}
				if got != want {
					t.Errorf("length probe for message %q = %d, want %d including its split parts", row.ID.String(), got, want)
				}
			}

			through := rows[len(rows)-1].Seq
			marker, err := source.(ingest.OpenCodeCurrentChangeMarkerSource).CurrentMessagesMarker(t.Context(), mustCurrentSessionID(t, testCase.SessionID), through)
			if err != nil {
				t.Fatalf("read current session change marker: %v", err)
			}
			wantUpdated := int64(0)
			for _, row := range rows {
				if row.TimeUpdated > wantUpdated {
					wantUpdated = row.TimeUpdated
				}
				for _, part := range row.Parts {
					if part.TimeUpdated > wantUpdated {
						wantUpdated = part.TimeUpdated
					}
				}
			}
			if marker.Rows != int64(len(rows))+wantParts {
				t.Errorf("change marker rows = %d, want %d message rows plus %d split parts", marker.Rows, len(rows), wantParts)
			}
			if marker.Bytes != wantBytes {
				t.Errorf("change marker bytes = %d, want %d including split parts", marker.Bytes, wantBytes)
			}
			if marker.MaxUpdated != wantUpdated {
				t.Errorf("change marker max updated = %d, want the newest message or split-part time %d", marker.MaxUpdated, wantUpdated)
			}

			freshness, err := source.CurrentFreshnessBySession(t.Context())
			if err != nil {
				t.Fatalf("read current freshness by session: %v", err)
			}
			if got := freshness[testCase.SessionID]; got.UnixMilli() != wantUpdated {
				t.Errorf("freshness for %q = %d, want the newest split-part time %d", testCase.SessionID, got.UnixMilli(), wantUpdated)
			}

			testfixture.AssertUnchanged(t, materialized, before)
		})
	}
}

// TestOpenCodeCurrentSessionPartsSurviveMaterializationAndIndex proves a 2.4.x
// session materializes and indexes with its split reasoning, text, and tool
// parts, while a legacy inline assistant row in the same session keeps its
// content. The omission case mutates one part's data.type so the source and
// projection report an accounted omission instead of silently dropping it.
func TestOpenCodeCurrentSessionPartsSurviveMaterializationAndIndex(t *testing.T) {
	document := loadOpenCodeSessionPartsDocument(t, openCodeSessionPartsYAML)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, testCase.SourceFixture)
			if testCase.MutatePartOrdinal >= 0 {
				mutateOpenCodeCurrentPartData(t, materialized.Path, testCase.SplitMessageID, testCase.MutatePartOrdinal)
			}
			root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
			if err != nil {
				t.Fatalf("resolve synthetic OpenCode root: %v", err)
			}
			adapter := newUnknownVocabularyAdapter(t)
			discovered, err := adapter.Discover(context.Background(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
			if err != nil {
				t.Fatalf("discover synthetic 2.4 source: %v", err)
			}
			var session ingest.DiscoveredSession
			found := false
			for _, candidate := range discovered {
				if string(candidate.SessionID) == testCase.SessionID {
					session = candidate
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("discovery omitted session %q; discovered %d sessions", testCase.SessionID, len(discovered))
			}
			if session.TranscriptOrigin != ingest.TranscriptOriginOpenCodeCurrentSQLite {
				t.Fatalf("session %q origin = %v, want current SQLite", testCase.SessionID, session.TranscriptOrigin)
			}

			captured, err := adapter.MaterializeTranscript(context.Background(), session)
			if err != nil {
				t.Fatalf("materialize 2.4 session: %v", err)
			}
			var projection openCodeSessionPartsProjection
			if err := json.Unmarshal(captured.Data, &projection); err != nil {
				t.Fatalf("decode managed 2.4 projection: %v", err)
			}
			if projection.ContentOmitted != testCase.ExpectContentOmitted {
				t.Errorf("projection content_omitted = %v, want %v", projection.ContentOmitted, testCase.ExpectContentOmitted)
			}
			splitParts := projectionParts(t, projection, testCase.SplitMessageID)
			if len(splitParts) != len(testCase.ExpectedPartTypes) {
				t.Fatalf("projection split message carries %d parts, want %d: %+v", len(splitParts), len(testCase.ExpectedPartTypes), splitParts)
			}
			for index, part := range splitParts {
				var header struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(part, &header); err != nil {
					t.Fatalf("decode projected part %d: %v", index, err)
				}
				if header.Type != testCase.ExpectedPartTypes[index] {
					t.Errorf("projected part %d type = %q, want %q", index, header.Type, testCase.ExpectedPartTypes[index])
				}
			}
			if testCase.ExpectedOmittedWarning != "" {
				// The dropped part is an accounted omission: the projection is
				// partial and a warning names the undecodable part type.
				seen := false
				for _, warning := range captured.Metadata.Diagnostics.Warnings {
					if warning.ErrorType == testCase.ExpectedOmittedWarning {
						seen = true
						if !strings.Contains(warning.Message, testCase.SplitMessageID) || !strings.Contains(warning.Message, "ordinal "+itoa(int(testCase.MutatePartOrdinal))) {
							t.Errorf("omitted-part warning does not name %q ordinal %d: %q", testCase.SplitMessageID, testCase.MutatePartOrdinal, warning.Message)
						}
					}
				}
				if !seen {
					t.Fatalf("materialized 2.4 session has no %q warning for the dropped part; warnings=%+v", testCase.ExpectedOmittedWarning, captured.Metadata.Diagnostics.Warnings)
				}
			}

			indexer := ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{FullContent: true})[ingest.HarnessOpenCode]
			captureIndexer, ok := indexer.(ingest.AuthoritativeTranscriptIndexer)
			if !ok {
				t.Fatal("OpenCode registry indexer does not implement the authoritative capture contract")
			}
			_, captureErr := captureIndexer.IndexTranscriptBytesForCapture(context.Background(), session, captured.Data)
			if testCase.ExpectContentOmitted {
				if captureErr == nil {
					t.Fatalf("an omitted split part was certified as a complete capture")
				}
			} else if captureErr != nil {
				t.Fatalf("complete 2.4 capture was refused: %v", captureErr)
			}

			entries, err := indexer.IndexTranscriptBytes(context.Background(), session, captured.Data)
			if err != nil {
				t.Fatalf("index 2.4 session: %v", err)
			}
			var thought, splitText, inlineText, toolUse, toolResult bool
			for _, entry := range entries {
				preview := ""
				if entry.ContentPreview != nil {
					preview = *entry.ContentPreview
				}
				switch entry.EntryType {
				case schema.EntryTypeThinking:
					if strings.Contains(preview, testCase.ExpectedThinking) {
						thought = true
					}
				case schema.EntryTypeToolUse:
					toolUse = true
					if entry.ToolOutput != nil && strings.Contains(*entry.ToolOutput, testCase.ExpectedToolResult) {
						toolResult = true
					}
				case schema.EntryTypeToolResult:
					if entry.ToolOutput != nil && strings.Contains(*entry.ToolOutput, testCase.ExpectedToolResult) {
						toolResult = true
					}
				case schema.EntryTypeText:
					if strings.Contains(preview, testCase.ExpectedSplitText) && testCase.ExpectedSplitText != "" {
						splitText = true
					}
					if strings.Contains(preview, testCase.ExpectedInlineText) {
						inlineText = true
					}
				}
			}
			if !thought {
				t.Errorf("indexed 2.4 session lost its split reasoning text %q; entries=%+v", testCase.ExpectedThinking, entries)
			}
			if !toolUse {
				t.Errorf("indexed 2.4 session lost its split tool_use part; entries=%+v", entries)
			}
			if !toolResult {
				t.Errorf("indexed 2.4 session lost its split tool result %q; entries=%+v", testCase.ExpectedToolResult, entries)
			}
			if testCase.ExpectedSplitText != "" && !splitText {
				t.Errorf("indexed 2.4 session lost its split assistant text %q", testCase.ExpectedSplitText)
			}
			if !inlineText {
				t.Errorf("indexed 2.4 session lost its legacy inline assistant text %q", testCase.ExpectedInlineText)
			}

			turns := transcript.EntriesToTurns(entries)
			if len(turns) == 0 {
				t.Fatalf("indexed 2.4 session folded to no turns")
			}
			var toolName, toolArgs string
			for _, turn := range turns {
				for _, call := range turn.ToolCalls {
					if call.Name == testCase.ExpectedToolName {
						toolName = call.Name
						toolArgs = call.Arguments
						if !strings.Contains(call.Result, testCase.ExpectedToolResult) {
							t.Errorf("folded tool call result = %q, want %q", call.Result, testCase.ExpectedToolResult)
						}
					}
				}
			}
			if toolName != testCase.ExpectedToolName {
				t.Errorf("folded turns expose tool name %q, want %q", toolName, testCase.ExpectedToolName)
			}
			if !strings.Contains(toolArgs, testCase.ExpectedToolArguments) {
				t.Errorf("folded tool arguments = %q, want %q", toolArgs, testCase.ExpectedToolArguments)
			}
		})
	}
}

// TestOpenCodeCurrentPartPageStopsAtMessageBoundary proves one message page
// cannot pull an unbounded part table into memory. A synthetic message with
// more parts than the page budget causes the read to drop that message and
// every later one, return the last complete message as the cursor, and hand the
// oversized message back first on the next page, where it is admitted whole.
func TestOpenCodeCurrentPartPageStopsAtMessageBoundary(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "opencode-24-session-parts")
	writer, err := sqlite.OpenConn(materialized.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic bulk-part writer: %v", err)
	}
	script := `
INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq)
VALUES ('msg_24_bulk_assistant', 'ses_opencode24parts0000000001', 'assistant', 2500, 2500, '{"agent":"build","model":{"id":"synthetic-2.4","providerID":"synthetic-provider"},"content":[],"time":{"created":2500,"completed":2500}}', 3);
INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq)
VALUES ('msg_24_after_bulk', 'ses_opencode24parts0000000001', 'user', 2501, 2501, '{"text":"after bulk","time":{"created":2501}}', 4);
WITH RECURSIVE seq(n) AS (SELECT 0 UNION ALL SELECT n + 1 FROM seq WHERE n < 8299)
INSERT INTO session_part (message_id, session_id, ordinal, type, call_id, time_created, time_updated, data)
SELECT 'msg_24_bulk_assistant', 'ses_opencode24parts0000000001', n, 'text', NULL, 2500, 2500, '{"type":"text","text":"bulk part"}'
FROM seq;
`
	scriptErr := sqlitex.ExecuteScript(writer, script, nil)
	closeErr := writer.Close()
	if scriptErr != nil || closeErr != nil {
		t.Fatalf("insert synthetic bulk parts: %v", errors.Join(scriptErr, closeErr))
	}

	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	defer closeSyntheticSource(t, source)
	first, err := source.CurrentMessages(t.Context(), ingest.OpenCodeCurrentPageRequest{
		SessionID: mustCurrentSessionID(t, "ses_opencode24parts0000000001"),
		PageSize:  mustCurrentPageSize(t, 10),
	})
	if err != nil {
		t.Fatalf("read first bulk-part page: %v", err)
	}
	if got := currentMessageSeqs(first.Messages); !equalInt64s(got, []int64{0, 1, 2, 3}) {
		t.Fatalf("first bulk-part page seqs = %v, want the three messages before the oversized part window and that window itself", got)
	}
	if first.Next == nil || first.Next.Seq().Value() != 3 {
		t.Fatalf("first bulk-part page next = %+v, want a continuation after the admitted oversized message seq 3", first.Next)
	}
	bulk := currentRowByID(t, first.Messages, "msg_24_bulk_assistant")
	if len(bulk.Parts) != 8300 {
		t.Fatalf("oversized bulk message carries %d parts, want all 8300 admitted as the page's one partial-cap message", len(bulk.Parts))
	}
	second, err := source.CurrentMessages(t.Context(), ingest.OpenCodeCurrentPageRequest{
		SessionID: mustCurrentSessionID(t, "ses_opencode24parts0000000001"),
		PageSize:  mustCurrentPageSize(t, 10),
		After:     first.Next,
	})
	if err != nil {
		t.Fatalf("read second bulk-part page: %v", err)
	}
	if got := currentMessageSeqs(second.Messages); !equalInt64s(got, []int64{4}) {
		t.Fatalf("second bulk-part page seqs = %v, want the message after the oversized one", got)
	}
	if second.Next != nil {
		t.Fatalf("second bulk-part page next = %+v, want the end of the session", second.Next)
	}
	if got := currentRowByID(t, second.Messages, "msg_24_after_bulk"); len(got.Parts) != 0 {
		t.Fatalf("message after the bulk message carries %d parts, want none", len(got.Parts))
	}
}

// TestOpenCodeCurrentPartFreshnessDrivesClocklessDiscovery proves a clockless
// 2.4.x session's changed time includes its split parts. The newest split part
// is later than every message envelope, so a part-only edit must move discovery
// freshness; a message-only aggregate would leave the session stale.
func TestOpenCodeCurrentPartFreshnessDrivesClocklessDiscovery(t *testing.T) {
	const sessionID = "ses_opencode24parts0000000001"
	materialized := testfixture.MaterializeByName(t, "opencode-24-session-parts")
	updateSyntheticSessionClock(t, materialized.Path, sessionID, 0)
	// Keep the database and WAL file-time floor below the fixture's row times so
	// the row aggregate, not the floor, decides changed time.
	setDatabaseModTime(t, materialized.Path, time.UnixMilli(1))
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic 2.4 root: %v", err)
	}
	adapter := newUnknownVocabularyAdapter(t)
	discovered, err := adapter.Discover(context.Background(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover clockless 2.4 source: %v", err)
	}
	for _, session := range discovered {
		if string(session.SessionID) != sessionID {
			continue
		}
		if got := session.ModTime.UnixMilli(); got != 2404 {
			t.Fatalf("clockless 2.4 session changed time = %d, want the newest split-part time 2404 (the newest message is 2403)", got)
		}
		return
	}
	t.Fatalf("discovery omitted clockless 2.4 session %q", sessionID)
}

func currentRowByID(t testing.TB, rows []ingest.OpenCodeCurrentMessageRow, id string) ingest.OpenCodeCurrentMessageRow {
	t.Helper()
	for _, row := range rows {
		if row.ID.String() == id {
			return row
		}
	}
	t.Fatalf("current page does not contain message %q", id)
	return ingest.OpenCodeCurrentMessageRow{}
}

func assertCurrentPartOrder(t testing.TB, row ingest.OpenCodeCurrentMessageRow, ordinals []int64, types []string) {
	t.Helper()
	if len(row.Parts) != len(ordinals) {
		t.Fatalf("message %q carries %d split parts, want %d", row.ID.String(), len(row.Parts), len(ordinals))
	}
	for index, part := range row.Parts {
		if part.Ordinal != ordinals[index] {
			t.Errorf("split part %d ordinal = %d, want %d", index, part.Ordinal, ordinals[index])
		}
		if part.Type.String() != types[index] {
			t.Errorf("split part %d type = %q, want %q", index, part.Type.String(), types[index])
		}
	}
}

func projectionParts(t testing.TB, projection openCodeSessionPartsProjection, id string) []json.RawMessage {
	t.Helper()
	for _, message := range projection.Messages {
		if message.ID != id {
			continue
		}
		parts := make([]json.RawMessage, 0, len(message.Parts))
		for _, part := range message.Parts {
			parts = append(parts, part.Data)
		}
		return parts
	}
	t.Fatalf("managed projection does not contain message %q", id)
	return nil
}

func mutateOpenCodeCurrentPartData(t testing.TB, path, messageID string, ordinal int64) {
	t.Helper()
	writer, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic session_part mutation writer: %v", err)
	}
	// Make the part's data.type disagree with its authoritative type column.
	// The source reports this row as an accounted omission rather than guessing.
	updateErr := sqlitex.ExecuteTransient(writer, `UPDATE session_part SET data = '{"type":"reasoning","text":"corrupt"}' WHERE message_id = ?1 AND ordinal = ?2`, &sqlitex.ExecOptions{Args: []any{messageID, ordinal}})
	closeErr := writer.Close()
	if updateErr != nil || closeErr != nil {
		t.Fatalf("apply synthetic session_part mutation: %v", errors.Join(updateErr, closeErr))
	}
}
