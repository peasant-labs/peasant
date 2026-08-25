package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/opencode_slice_continuation.yaml
var openCodeSliceContinuationData []byte

// openCodeSliceContinuationCase is one session read from its first slice to
// exhaustion.
type openCodeSliceContinuationCase struct {
	Name          string   `yaml:"name"`
	SourceFixture string   `yaml:"source_fixture"`
	Origin        string   `yaml:"origin"`
	SessionIDs    []string `yaml:"session_ids"`
	BudgetBytes   int64    `yaml:"budget_bytes"`
	MaxSlices     int      `yaml:"max_slices"`
	// ExpectMultipleSlices is whether this session and budget must actually
	// exercise a continuation. Without it a case could pass by reading the whole
	// session in one slice and never touching the seam at all.
	ExpectMultipleSlices bool `yaml:"expect_multiple_slices"`
	// ExpectComplete is whether the sliced read must reach everything the
	// whole-session read holds. See the fixture's header for the one ordering
	// under which it cannot; such a case names what it leaves out.
	ExpectComplete     bool     `yaml:"expect_complete"`
	ExpectMissingParts []string `yaml:"expect_missing_parts"`
}

type openCodeSliceContinuationDoc struct {
	RequiredCases []string                        `yaml:"required_cases"`
	Cases         []openCodeSliceContinuationCase `yaml:"cases"`
}

func loadOpenCodeSliceContinuationDoc(t *testing.T) openCodeSliceContinuationDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeSliceContinuationData))
	decoder.KnownFields(true)
	var doc openCodeSliceContinuationDoc
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode the slice-continuation fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("the slice-continuation fixture must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("the slice-continuation fixture declares no required cases")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" || c.SourceFixture == "" || c.BudgetBytes <= 0 || c.MaxSlices <= 0 || len(c.SessionIDs) == 0 {
			t.Fatalf("the slice-continuation fixture has an incomplete case: %+v", c)
		}
		if c.ExpectComplete && len(c.ExpectMissingParts) > 0 {
			t.Fatalf("slice-continuation case %q expects a complete read yet names parts it leaves out", c.Name)
		}
		if !c.ExpectComplete && len(c.ExpectMissingParts) == 0 {
			t.Fatalf("slice-continuation case %q expects an incomplete read but names nothing it leaves out, so the gap is unpinned", c.Name)
		}
		if c.Origin != "legacy" && c.Origin != "current" {
			t.Fatalf("slice-continuation case %q names origin %q, which is neither legacy nor current", c.Name, c.Origin)
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("the slice-continuation fixture has a duplicate case name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := seen[name]; !ok {
			t.Fatalf("the slice-continuation fixture is missing required case %q", name)
		}
	}
	return doc
}

// TestOpenCodeSliceContinuation reads each fixture session one budget-sized
// slice at a time and proves the properties that let a caller simply APPEND one
// slice's turns to the previous slice's: no message is emitted twice, nothing
// the whole-session read holds is lost, the chain terminates, and the counts
// the pane's note quotes stay honest.
func TestOpenCodeSliceContinuation(t *testing.T) {
	t.Parallel()
	doc := loadOpenCodeSliceContinuationDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			materialized := testfixture.MaterializeByName(t, c.SourceFixture)
			adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
			origin := ingest.TranscriptOriginOpenCodeLegacySQLite
			if c.Origin == "current" {
				origin = ingest.TranscriptOriginOpenCodeCurrentSQLite
			}
			for _, sessionID := range c.SessionIDs {
				t.Run(sessionID, func(t *testing.T) {
					session := sliceFixtureSession(t, materialized.Path, sessionID, origin)
					assertSliceChain(t, adapter, session, c)
				})
			}
		})
	}
}

// assertSliceChain drives one session from its zero cursor to exhaustion.
func assertSliceChain(t *testing.T, adapter *ingest.OpenCodeAdapter, session ingest.DiscoveredSession, c openCodeSliceContinuationCase) {
	t.Helper()
	wholeMessages, wholeParts := sliceProjectionIdentifiers(t, wholeSessionProjection(t, adapter, session))

	sliceMessages := make(map[string]int)
	sliceParts := make(map[string]int)
	var cursor ingest.TranscriptSliceCursor
	var lastConsumedBytes, lastConsumedRows int64
	slices := 0
	exhausted := false
	for slices < c.MaxSlices {
		slice, err := adapter.MaterializeTranscriptSlice(t.Context(), session, c.BudgetBytes, cursor)
		if err != nil {
			t.Fatalf("slice %d of session %q failed: %v", slices+1, session.SessionID, err)
		}
		slices++
		if len(slice.Data) > 0 {
			messages, parts := sliceProjectionIdentifiers(t, slice.Data)
			for _, id := range messages {
				if sliceMessages[id]++; sliceMessages[id] > 1 {
					t.Errorf("message %q was emitted by %d slices; a message must belong to exactly one slice or the turn folded from it appears twice at the seam", id, sliceMessages[id])
				}
			}
			for _, id := range parts {
				if sliceParts[id]++; sliceParts[id] > 1 {
					t.Errorf("part %q was emitted by %d slices; the preview would show its content twice", id, sliceParts[id])
				}
			}
		}
		cursor = slice.Next
		if cursor.ConsumedBytes() < lastConsumedBytes || cursor.ConsumedRows() < lastConsumedRows {
			t.Errorf("after slice %d the cursor reports %d bytes and %d rows consumed, down from %d and %d; the note above the turns would count backwards",
				slices, cursor.ConsumedBytes(), cursor.ConsumedRows(), lastConsumedBytes, lastConsumedRows)
		}
		lastConsumedBytes, lastConsumedRows = cursor.ConsumedBytes(), cursor.ConsumedRows()
		if total := cursor.TotalBytes(); total > 0 && cursor.ConsumedBytes() > total {
			t.Errorf("after slice %d the cursor reports %d bytes consumed of a %d byte session; the note would claim more than the session holds",
				slices, cursor.ConsumedBytes(), total)
		}
		if !slice.More {
			exhausted = true
			break
		}
	}
	if !exhausted {
		t.Fatalf("session %q still reported more content after %d slices at a %d byte budget; the chain does not terminate",
			session.SessionID, slices, c.BudgetBytes)
	}
	if c.ExpectMultipleSlices && slices < 2 {
		t.Fatalf("session %q finished in %d slice at a %d byte budget, so this case never exercises a continuation",
			session.SessionID, slices, c.BudgetBytes)
	}
	assertSameIdentifiers(t, "message", wholeMessages, sliceMessages, nil)
	assertSameIdentifiers(t, "part", wholeParts, sliceParts, c.ExpectMissingParts)
}

// assertSameIdentifiers proves the sliced read holds exactly what the
// whole-session read holds: nothing lost, nothing invented.
func assertSameIdentifiers(t *testing.T, kind string, whole []string, sliced map[string]int, allowedMissing []string) {
	t.Helper()
	missing := make(map[string]struct{}, len(allowedMissing))
	for _, id := range allowedMissing {
		missing[id] = struct{}{}
	}
	expected := make(map[string]struct{}, len(whole))
	for _, id := range whole {
		expected[id] = struct{}{}
		_, sliceHasIt := sliced[id]
		_, mayBeMissing := missing[id]
		if !sliceHasIt && !mayBeMissing {
			t.Errorf("%s %q is in the whole-session read but in no slice; scrolling would never reach it", kind, id)
		}
		if sliceHasIt && mayBeMissing {
			t.Errorf("%s %q is named as left out but every slice reached it; the fixture overstates the gap", kind, id)
		}
	}
	for id := range sliced {
		if _, ok := expected[id]; !ok {
			t.Errorf("%s %q is in a slice but not in the whole-session read; the preview invented it", kind, id)
		}
	}
}

func wholeSessionProjection(t *testing.T, adapter *ingest.OpenCodeAdapter, session ingest.DiscoveredSession) []byte {
	t.Helper()
	_, data, err := adapter.MaterializeTranscript(t.Context(), session)
	if err != nil {
		t.Fatalf("read session %q whole: %v", session.SessionID, err)
	}
	return data
}

// sliceProjectionIdentifiers reads the message and part identifiers out of one
// managed projection. Both projection formats share the message shape, so one
// decode serves the legacy and the current representation alike.
func sliceProjectionIdentifiers(t *testing.T, data []byte) (messages, parts []string) {
	t.Helper()
	var projection struct {
		Messages []struct {
			ID    string `json:"id"`
			Parts []struct {
				ID string `json:"id"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := yaml.Unmarshal(data, &projection); err != nil {
		t.Fatalf("decode a managed projection: %v", err)
	}
	for _, message := range projection.Messages {
		messages = append(messages, message.ID)
		for _, part := range message.Parts {
			if part.ID == "" {
				// A current-schema part is nested inside its message row and some
				// kinds carry no identifier of their own. Such a part has no
				// identity to compare, and its message's identity already pins
				// which slice it belongs to.
				continue
			}
			parts = append(parts, part.ID)
		}
	}
	return messages, parts
}

func sliceFixtureSession(t *testing.T, databasePath, sessionID string, origin ingest.TranscriptOrigin) ingest.DiscoveredSession {
	t.Helper()
	id, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return ingest.DiscoveredSession{
		SessionID:        id,
		Harness:          ingest.HarnessOpenCode,
		SourcePath:       ingest.ResolvedPath(databasePath),
		OriginalRoot:     ingest.ResolvedPath(filepath.Dir(databasePath)),
		TranscriptOrigin: origin,
	}
}
