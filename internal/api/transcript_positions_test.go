package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

type segmentPositionRun struct {
	Count        int    `yaml:"count"`
	Role         string `yaml:"role"`
	ContentBytes int    `yaml:"contentBytes"`
}

type segmentPositionCorruption struct {
	Segment   int  `yaml:"segment"`
	AddBefore int  `yaml:"addBefore,omitempty"`
	SetCount  *int `yaml:"setCount,omitempty"`
}

type segmentPositionRef struct {
	Index  int `yaml:"index"`
	Before int `yaml:"before"`
	Count  int `yaml:"count"`
}

type segmentPositionWithheld struct {
	Rule    string `yaml:"rule"`
	Segment int    `yaml:"segment"`
}

type segmentPositionExpectation struct {
	TotalTurns    int `yaml:"totalTurns"`
	TotalSegments int `yaml:"totalSegments"`
	// TotalConversationTurns is absent exactly when the positions are withheld.
	TotalConversationTurns *int `yaml:"totalConversationTurns,omitempty"`
	Pages                  int  `yaml:"pages"`
	// PageSegments is the number of refs on the asserted page, when stated.
	PageSegments int                      `yaml:"pageSegments,omitempty"`
	Segments     []segmentPositionRef     `yaml:"segments,omitempty"`
	Withheld     *segmentPositionWithheld `yaml:"withheld,omitempty"`
}

type segmentPositionCase struct {
	Name        string                     `yaml:"name"`
	BudgetBytes int                        `yaml:"budgetBytes"`
	Page        int                        `yaml:"page,omitempty"`
	Runs        []segmentPositionRun       `yaml:"runs"`
	Corrupt     *segmentPositionCorruption `yaml:"corrupt,omitempty"`
	Expected    segmentPositionExpectation `yaml:"expected"`
}

// positionEntries builds one top-level entry per turn of the runs. A system
// run is typed as a system entry; every other turn is a text entry.
func positionEntries(t *testing.T, runs []segmentPositionRun) []schema.SessionEntry {
	t.Helper()
	var entries []schema.SessionEntry
	for _, run := range runs {
		for i := 0; i < run.Count; i++ {
			position := len(entries)
			name := run.Role
			if name == "alternating" {
				name = "assistant"
				if position%2 == 0 {
					name = "user"
				}
			}
			role, err := transcriptRole(name)
			if err != nil {
				t.Fatal(err)
			}
			entryType := schema.EntryTypeText
			if role == schema.RoleSystem {
				entryType = schema.EntryTypeSystem
			}
			content := strings.Repeat("t", run.ContentBytes)
			index := position * 4
			timestamp := int64(1_700_000_000_000 + index)
			entries = append(entries, schema.SessionEntry{
				SessionID:      ingest.SessionID(transcriptTestSessionID),
				EntryIndex:     index,
				Harness:        schema.HarnessClaudeCode,
				EntryType:      entryType,
				Role:           role,
				TimestampMs:    &timestamp,
				ContentPreview: &content,
				Depth:          0,
			})
		}
	}
	return entries
}

// recordingLogHandler keeps every record a producer logs, so a test can assert
// the one error a withheld position must leave behind.
type recordingLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

var _ slog.Handler = (*recordingLogHandler)(nil)

func (h *recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingLogHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingLogHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingLogHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

func logRecordAttrs(record slog.Record) map[string]slog.Value {
	attrs := make(map[string]slog.Value, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value
		return true
	})
	return attrs
}

func getTranscriptDirectory(t *testing.T, server *Server, page int) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	target := "/api/v1/sessions/" + transcriptTestSessionID + "/transcript/segments"
	if page > 0 {
		target += fmt.Sprintf("?page=%d", page)
	}
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.SetPathValue("id", transcriptTestSessionID)
	recorder := httptest.NewRecorder()
	server.handleTranscriptSegments(recorder, request)
	return recorder, recorder.Body.Bytes()
}

// TestTranscriptDirectoryPositionFixtureCases drives the directory handler over
// the production cut, with a lowered budget, and asserts the served position
// fields as literals. A row with a corruption wraps the cut, because a cut with
// wrong position data cannot be produced through the store; only the cut is
// wrapped, and the handler, the self-check and the validator are the real ones.
func TestTranscriptDirectoryPositionFixtureCases(t *testing.T) {
	t.Parallel()
	fixture := loadSegmentReadFixture(t)
	if len(fixture.PositionCases) == 0 {
		t.Fatal("the transcript segment fixture states no position case")
	}
	for _, fixtureCase := range fixture.PositionCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			expected := fixtureCase.Expected
			if fixtureCase.BudgetBytes <= 0 || expected.Pages < 1 {
				t.Fatal("a position case must state its budget and its page count")
			}
			if (expected.Withheld == nil) == (expected.TotalConversationTurns == nil) {
				t.Fatal("a position case states either a literal totalConversationTurns or a withheld rule, never both or neither")
			}
			if (expected.Withheld == nil) != (fixtureCase.Corrupt == nil) {
				t.Fatal("a position case is withheld exactly when it corrupts the cut")
			}
			server := newTestServerForSegments(t, newFakeSegmentReader(positionEntries(t, fixtureCase.Runs)))
			logs := &recordingLogHandler{}
			server.segments.budget = fixtureCase.BudgetBytes
			server.segments.logger = slog.New(logs)
			if corrupt := fixtureCase.Corrupt; corrupt != nil {
				server.segments.cut = func(turns []ingest.Turn, budget int) ([]schema.TranscriptSegmentRef, int) {
					refs, total := transcript.CutSegments(turns, budget)
					span := refs[corrupt.Segment].ConversationTurns
					span.Before += corrupt.AddBefore
					if corrupt.SetCount != nil {
						span.Count = *corrupt.SetCount
					}
					return refs, total
				}
			}

			asserted := fixtureCase.Page
			if asserted == 0 {
				asserted = 1
			}
			for page := 1; page <= expected.Pages; page++ {
				recorder, body := getTranscriptDirectory(t, server, page)
				if recorder.Code != http.StatusOK {
					t.Fatalf("directory page %d status = %d, body %s", page, recorder.Code, body)
				}
				var directory schema.TranscriptSegmentDirectory
				if err := json.Unmarshal(body, &directory); err != nil {
					t.Fatalf("decode directory page %d: %v", page, err)
				}
				if err := directory.Validate(); err != nil {
					t.Fatalf("directory page %d does not validate alone: %v", page, err)
				}
				if directory.TotalTurns != expected.TotalTurns || directory.TotalSegments != expected.TotalSegments {
					t.Fatalf("page %d states %d turns in %d segments, want %d in %d", page, directory.TotalTurns, directory.TotalSegments, expected.TotalTurns, expected.TotalSegments)
				}
				if expected.Pages > 1 && (directory.DirectoryPage == nil || *directory.DirectoryPage != page) {
					t.Fatalf("page %d answered as page %v", page, directory.DirectoryPage)
				}
				if expected.Withheld != nil {
					for _, key := range []string{`"conversationTurns"`, `"totalConversationTurns"`} {
						if strings.Contains(string(body), key) {
							t.Fatalf("page %d of a directory with withheld positions carries the key %s", page, key)
						}
					}
					continue
				}
				if directory.TotalConversationTurns == nil || *directory.TotalConversationTurns != *expected.TotalConversationTurns {
					t.Fatalf("page %d totalConversationTurns = %v, want %d on every page", page, directory.TotalConversationTurns, *expected.TotalConversationTurns)
				}
				for _, ref := range directory.Segments {
					if ref.ConversationTurns == nil {
						t.Fatalf("page %d segment %d carries no conversationTurns", page, ref.Index)
					}
				}
				if page != asserted {
					continue
				}
				if expected.PageSegments != 0 && len(directory.Segments) != expected.PageSegments {
					t.Fatalf("page %d holds %d refs, want %d", page, len(directory.Segments), expected.PageSegments)
				}
				if len(expected.Segments) == 0 {
					t.Fatal("a position case that serves positions must state literal spans")
				}
				for _, want := range expected.Segments {
					found := false
					for _, ref := range directory.Segments {
						if ref.Index != want.Index {
							continue
						}
						found = true
						if ref.ConversationTurns.Before != want.Before || ref.ConversationTurns.Count != want.Count {
							t.Fatalf("segment %d conversationTurns = %+v, want {before: %d, count: %d}", ref.Index, *ref.ConversationTurns, want.Before, want.Count)
						}
					}
					if !found {
						t.Fatalf("page %d does not hold segment %d", page, want.Index)
					}
				}
			}

			records := logs.snapshot()
			if expected.Withheld == nil {
				if len(records) != 0 {
					t.Fatalf("a sound cut logged %d record(s); the first says %q", len(records), records[0].Message)
				}
				return
			}
			// A position fault costs only the position fields: the segment the
			// directory names is still readable.
			segmentRecorder, segmentBody := getTranscriptSegment(t, server, "0")
			if segmentRecorder.Code != http.StatusOK {
				t.Fatalf("segment 0 status = %d after the positions were withheld, body %s", segmentRecorder.Code, segmentBody)
			}
			// The directory is cached per revision, so a second read logs nothing.
			if recorder, body := getTranscriptDirectory(t, server, 1); recorder.Code != http.StatusOK {
				t.Fatalf("second directory read status = %d, body %s", recorder.Code, body)
			}
			records = logs.snapshot()
			if len(records) != 1 {
				t.Fatalf("withheld positions logged %d record(s), want exactly one", len(records))
			}
			if records[0].Level != slog.LevelError {
				t.Fatalf("the withheld-positions record is at level %s, want ERROR", records[0].Level)
			}
			attrs := logRecordAttrs(records[0])
			if got := attrs["session"].String(); got != transcriptTestSessionID {
				t.Fatalf("log session = %q, want %q", got, transcriptTestSessionID)
			}
			if got := attrs["revision"].String(); got != "4213" {
				t.Fatalf("log revision = %q, want 4213", got)
			}
			if got := attrs["rule"].String(); got != expected.Withheld.Rule {
				t.Fatalf("log rule = %q, want %q", got, expected.Withheld.Rule)
			}
			if got := attrs["segment"]; got.Kind() != slog.KindInt64 || got.Int64() != int64(expected.Withheld.Segment) {
				t.Fatalf("log segment = %v, want %d", got, expected.Withheld.Segment)
			}
			if got := attrs["error"].String(); !strings.Contains(got, "transcript.CheckConversationTurns") {
				t.Fatalf("log error does not name the check: %q", got)
			}
		})
	}
}

// TestSegmentProducerDefaultsAreWired pins the production wiring: the default
// constructor sets the real cut and a logger, so no read can reach a nil seam.
func TestSegmentProducerDefaultsAreWired(t *testing.T) {
	t.Parallel()
	producer := newSegmentProducer(newFakeSegmentReader(nil))
	if producer.cut == nil {
		t.Fatal("newSegmentProducer left the cut unset; the directory build would panic")
	}
	if producer.logger == nil {
		t.Fatal("newSegmentProducer left the logger unset; a withheld position would panic instead of logging")
	}
	turns := []ingest.Turn{{Index: 4, Role: schema.RoleUser, EntryType: schema.EntryTypeText, Content: "hello"}}
	refs, total := producer.cut(turns, producer.budget)
	want, wantTotal := transcript.CutSegments(turns, producer.budget)
	if total != wantTotal || len(refs) != 1 || len(want) != 1 {
		t.Fatalf("the default cut returned %d ref(s) over %d turns, want the production cut's %d over %d", len(refs), total, len(want), wantTotal)
	}
	if refs[0].ConversationTurns == nil || *refs[0].ConversationTurns != *want[0].ConversationTurns {
		t.Fatalf("the default cut returned %+v (%d turns), want the production cut's %+v (%d turns)", refs, total, want, wantTotal)
	}
}
