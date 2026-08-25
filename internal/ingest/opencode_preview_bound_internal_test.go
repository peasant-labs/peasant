package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_preview_bound.yaml
var openCodePreviewBoundData []byte

// openCodePreviewBoundCase declares one bounded preview read: the payload byte
// length of every row the source serves, in service order, the budget the read
// runs under, and what the read must produce.
type openCodePreviewBoundCase struct {
	Name                 string `yaml:"name"`
	Origin               string `yaml:"origin"`
	MessageBytes         int    `yaml:"message_bytes"`
	SecondMessageBytes   int    `yaml:"second_message_bytes"`
	SecondMessageRows    []int  `yaml:"second_message_row_bytes"`
	ExpectOrphanMessages int    `yaml:"expect_orphan_messages"`
	RowBytes             []int  `yaml:"row_bytes"`
	OrphanRows           int    `yaml:"orphan_rows"`
	BudgetBytes          int64  `yaml:"budget_bytes"`
	TotalBytes           int64  `yaml:"total_bytes"`
	TotalRows            int64  `yaml:"total_rows"`
	ExpectTruncated      bool   `yaml:"expect_truncated"`
	ExpectIncludedRows   int64  `yaml:"expect_included_rows"`
	ExpectIncludedBytes  int64  `yaml:"expect_included_bytes"`
	ExpectProjectionRows int    `yaml:"expect_projection_rows"`
}

type openCodePreviewBoundDoc struct {
	RequiredCases []string                   `yaml:"required_cases"`
	Cases         []openCodePreviewBoundCase `yaml:"cases"`
}

func loadOpenCodePreviewBoundDoc(t *testing.T) openCodePreviewBoundDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodePreviewBoundData))
	decoder.KnownFields(true)
	var doc openCodePreviewBoundDoc
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode preview bound fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("preview bound fixture must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("preview bound fixture declares no required cases")
	}
	present := make(map[string]struct{}, len(doc.Cases))
	for _, testCase := range doc.Cases {
		if testCase.Origin == "legacy" && testCase.MessageBytes <= 0 {
			t.Fatalf("legacy preview bound case %q declares no message payload, so the message read would not spend the budget", testCase.Name)
		}
		if testCase.Name == "" || testCase.Origin == "" || len(testCase.RowBytes) == 0 {
			t.Fatalf("preview bound fixture has an incomplete case: %+v", testCase)
		}
		if _, duplicate := present[testCase.Name]; duplicate {
			t.Fatalf("preview bound fixture has a duplicate case name %q", testCase.Name)
		}
		present[testCase.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := present[name]; !ok {
			t.Fatalf("preview bound fixture is missing required case %q", name)
		}
	}
	return doc
}

// previewBoundSource serves one synthetic session's rows in a single page. It
// inherits the unused reads from the shared negative source, so a case declares
// only the rows the bounded read consumes.
type previewBoundSource struct {
	semanticNegativeSource
	messages []OpenCodeLegacyMessageRow
	parts    []OpenCodeLegacyPartRow
	current  []OpenCodeCurrentMessageRow
}

var _ OpenCodeSQLiteSource = previewBoundSource{}

func (source previewBoundSource) LegacyMessages(context.Context, OpenCodeLegacyMessagePageRequest) (OpenCodeLegacyMessagePage, error) {
	return OpenCodeLegacyMessagePage{Messages: source.messages}, nil
}

func (source previewBoundSource) LegacySessionParts(context.Context, OpenCodeLegacySessionPartPageRequest) (OpenCodeLegacyPartPage, error) {
	return OpenCodeLegacyPartPage{Parts: source.parts}, nil
}

func (source previewBoundSource) CurrentMessages(context.Context, OpenCodeCurrentPageRequest) (OpenCodeCurrentPage, error) {
	return OpenCodeCurrentPage{Messages: source.current}, nil
}

// paddedJSONPayload builds a valid JSON object whose encoded form is exactly
// size bytes, so a case states a row's cost in bytes rather than in text.
func paddedJSONPayload(t *testing.T, template string, size int) string {
	t.Helper()
	payload := fmt.Sprintf(template, "")
	if len(payload) > size {
		t.Fatalf("payload template %q is already %d bytes, which exceeds the requested %d", template, len(payload), size)
	}
	return fmt.Sprintf(template, strings.Repeat("a", size-len(payload)))
}

func TestOpenCodePreviewBoundedReadStopsAtItsByteBudget(t *testing.T) {
	t.Parallel()
	doc := loadOpenCodePreviewBoundDoc(t)
	for _, testCase := range doc.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			switch testCase.Origin {
			case "legacy":
				assertLegacyPreviewBound(t, testCase)
			case "current":
				assertCurrentPreviewBound(t, testCase)
			default:
				t.Fatalf("preview bound case %q has an unsupported origin %q", testCase.Name, testCase.Origin)
			}
		})
	}
}

func assertLegacyPreviewBound(t *testing.T, testCase openCodePreviewBoundCase) {
	t.Helper()
	sessionID, err := NewOpenCodeLegacySessionID("ses_preview_bound_case")
	if err != nil {
		t.Fatalf("build legacy session identifier: %v", err)
	}
	pageSize, err := NewOpenCodeLegacyPageSize(openCodeLegacyMaterializePage)
	if err != nil {
		t.Fatalf("build legacy page size: %v", err)
	}
	parentID, err := NewOpenCodeLegacyMessageID("msg_preview_bound_parent")
	if err != nil {
		t.Fatalf("build legacy message identifier: %v", err)
	}
	absentID, err := NewOpenCodeLegacyMessageID("msg_preview_bound_absent")
	if err != nil {
		t.Fatalf("build absent legacy message identifier: %v", err)
	}
	source := previewBoundSource{
		messages: []OpenCodeLegacyMessageRow{{
			ID: parentID, SessionID: sessionID, TimeCreated: 1, TimeUpdated: 1,
			Data: paddedJSONPayload(t, `{"role":"user","summary":"%s"}`, testCase.MessageBytes),
		}},
	}
	for index, size := range testCase.RowBytes {
		partID, idErr := NewOpenCodeLegacyPartID(fmt.Sprintf("prt_preview_bound_attached_%d", index))
		if idErr != nil {
			t.Fatalf("build legacy part identifier: %v", idErr)
		}
		source.parts = append(source.parts, OpenCodeLegacyPartRow{
			ID: partID, MessageID: parentID, SessionID: sessionID, TimeCreated: 1, TimeUpdated: 1,
			Data: paddedJSONPayload(t, `{"type":"text","text":"%s"}`, size),
		})
	}
	if testCase.SecondMessageBytes > 0 {
		secondID, idErr := NewOpenCodeLegacyMessageID("msg_preview_bound_second")
		if idErr != nil {
			t.Fatalf("build second legacy message identifier: %v", idErr)
		}
		source.messages = append(source.messages, OpenCodeLegacyMessageRow{
			ID: secondID, SessionID: sessionID, TimeCreated: 2, TimeUpdated: 2,
			Data: paddedJSONPayload(t, `{"role":"user","summary":"%s"}`, testCase.SecondMessageBytes),
		})
		for index, size := range testCase.SecondMessageRows {
			partID, partErr := NewOpenCodeLegacyPartID(fmt.Sprintf("prt_preview_bound_second_%d", index))
			if partErr != nil {
				t.Fatalf("build second message part identifier: %v", partErr)
			}
			source.parts = append(source.parts, OpenCodeLegacyPartRow{
				ID: partID, MessageID: secondID, SessionID: sessionID, TimeCreated: 2, TimeUpdated: 2,
				Data: paddedJSONPayload(t, `{"type":"text","text":"%s"}`, size),
			})
		}
	}
	for index := range testCase.OrphanRows {
		partID, idErr := NewOpenCodeLegacyPartID(fmt.Sprintf("prt_preview_bound_orphan_%d", index))
		if idErr != nil {
			t.Fatalf("build orphan legacy part identifier: %v", idErr)
		}
		source.parts = append(source.parts, OpenCodeLegacyPartRow{
			ID: partID, MessageID: absentID, SessionID: sessionID, TimeCreated: 1, TimeUpdated: 1,
			Data: paddedJSONPayload(t, `{"type":"text","text":"%s"}`, testCase.RowBytes[0]),
		})
	}
	size := OpenCodePayloadSize{Rows: testCase.TotalRows, Bytes: testCase.TotalBytes}
	projection, _, truncation, err := readOpenCodeLegacyProjectionCore(t.Context(), source, sessionID, pageSize, testCase.BudgetBytes, size)
	if err != nil {
		t.Fatalf("read bounded legacy projection: %v", err)
	}
	partCount := 0
	orphanMessages := 0
	for _, message := range projection.Messages {
		partCount += len(message.Parts)
		if message.Orphan {
			orphanMessages++
		}
	}
	if orphanMessages != testCase.ExpectOrphanMessages {
		t.Errorf("bounded legacy projection carries %d synthetic orphan messages, want %d", orphanMessages, testCase.ExpectOrphanMessages)
	}
	if partCount != testCase.ExpectProjectionRows {
		t.Errorf("bounded legacy projection carries %d parts, want %d", partCount, testCase.ExpectProjectionRows)
	}
	assertPreviewTruncation(t, testCase, truncation, MaterializeUnitRows)
}

func assertCurrentPreviewBound(t *testing.T, testCase openCodePreviewBoundCase) {
	t.Helper()
	sessionID, err := NewOpenCodeCurrentSessionID("ses_preview_bound_case")
	if err != nil {
		t.Fatalf("build current session identifier: %v", err)
	}
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		t.Fatalf("build current page size: %v", err)
	}
	messageType, err := NewOpenCodeCurrentMessageType("user")
	if err != nil {
		t.Fatalf("build current message type: %v", err)
	}
	var source previewBoundSource
	for index, size := range testCase.RowBytes {
		messageID, idErr := NewOpenCodeCurrentMessageID(fmt.Sprintf("msg_preview_bound_%d", index))
		if idErr != nil {
			t.Fatalf("build current message identifier: %v", idErr)
		}
		seq, seqErr := NewOpenCodeCurrentSeq(int64(index + 1))
		if seqErr != nil {
			t.Fatalf("build current sequence: %v", seqErr)
		}
		data := paddedJSONPayload(t, `{"id":"`+messageID.String()+`","time":{"created":1},"files":[],"agents":[],"text":"%s"}`, size)
		if !json.Valid([]byte(data)) {
			t.Fatalf("synthetic current row %d is not valid JSON: %s", index, data)
		}
		source.current = append(source.current, OpenCodeCurrentMessageRow{
			ID: messageID, SessionID: sessionID, Type: messageType, TimeCreated: 1, TimeUpdated: 1, Data: data, Seq: seq,
		})
	}
	size := OpenCodePayloadSize{Rows: testCase.TotalRows, Bytes: testCase.TotalBytes}
	projection, _, truncation, err := readOpenCodeCurrentProjectionCore(t.Context(), source, sessionID, pageSize, testCase.BudgetBytes, size)
	if err != nil {
		t.Fatalf("read bounded current projection: %v", err)
	}
	if len(projection.Messages) != testCase.ExpectProjectionRows {
		t.Errorf("bounded current projection carries %d messages, want %d", len(projection.Messages), testCase.ExpectProjectionRows)
	}
	assertPreviewTruncation(t, testCase, truncation, MaterializeUnitMessages)
}

// assertPreviewTruncation checks the typed marker the preview renders. An
// untruncated read must report the zero marker, so a preview cannot show a note
// about a session it read whole.
func assertPreviewTruncation(t *testing.T, testCase openCodePreviewBoundCase, truncation MaterializeTruncation, unit MaterializeTruncationUnit) {
	t.Helper()
	if truncation.Truncated != testCase.ExpectTruncated {
		t.Fatalf("bounded read reported truncated=%v, want %v (marker %+v)", truncation.Truncated, testCase.ExpectTruncated, truncation)
	}
	if !testCase.ExpectTruncated {
		if truncation != (MaterializeTruncation{}) {
			t.Errorf("untruncated read reported a non-zero marker %+v", truncation)
		}
		return
	}
	if truncation.Unit != unit {
		t.Errorf("truncation unit = %q, want %q", truncation.Unit, unit)
	}
	if truncation.IncludedRows != testCase.ExpectIncludedRows {
		t.Errorf("truncation included %d rows, want %d", truncation.IncludedRows, testCase.ExpectIncludedRows)
	}
	if truncation.TotalRows != testCase.TotalRows || truncation.TotalBytes != testCase.TotalBytes {
		t.Errorf("truncation totals = %d rows / %d bytes, want %d / %d", truncation.TotalRows, truncation.TotalBytes, testCase.TotalRows, testCase.TotalBytes)
	}
	if truncation.IncludedBytes != testCase.ExpectIncludedBytes {
		t.Errorf("truncation included %d bytes, want %d", truncation.IncludedBytes, testCase.ExpectIncludedBytes)
	}
	if truncation.BudgetBytes != testCase.BudgetBytes {
		t.Errorf("truncation budget = %d bytes, want %d", truncation.BudgetBytes, testCase.BudgetBytes)
	}
}
