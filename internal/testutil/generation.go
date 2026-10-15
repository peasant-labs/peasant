package testutil

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// CompleteGeneration builds a complete managed generation whose single user
// entry carries the supplied text, plus the source-blob map for that entry, so
// a read that served the wrong generation is observable in the turn content.
// The metadata takes the canonical fixture host, model, and project, and the
// session identity the caller supplies, so the generation is a valid activation
// candidate for a golden-copy store.
func CompleteGeneration(t testing.TB, sid schema.SessionID, genID, text string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = sid
	meta.ModelHarness = schema.HarnessClaudeCode
	meta.HostSlug = TestHostSlug
	meta.Model = TestModel
	meta.CWD = "/home/test/myapp"
	meta.Project = ingest.ProjectInfo{Hash: TestProjectHash, Name: "myapp", FilePath: "/home/test/myapp"}
	ingested := int64(1700000060000)
	meta.Timestamp = ingest.TimestampInfo{Start: 1700000000000, End: 1700000060000, Ingested: &ingested}
	meta.Source = ingest.SourceInfo{FilePath: "/source/file.jsonl", Format: ingest.SourceFormatJSONL}
	meta.Stats = ingest.StatsInfo{TurnCount: 1, DurationMs: 60000, TokensIn: 1, TokensOut: 1}
	submissions := int64(1)
	meta.Stats.InputSubmissionCount = &submissions

	ref := schema.SourceEntryRef(genID + "_user")
	entry := schema.SessionEntry{
		SessionID: sid, EntryIndex: 0, Harness: schema.HarnessClaudeCode,
		EntryType: schema.EntryTypeText, Role: schema.RoleUser, SourceEntryRef: ref, ContentPreview: &text,
	}
	generation := indexformat.Generation{
		ID:                   genID,
		Completeness:         indexformat.GenerationCompletenessComplete,
		Metadata:             meta,
		Main:                 indexformat.Partition{Entries: []schema.SessionEntry{entry}},
		Content:              []indexformat.ContentRecord{{Ref: ref}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
	}
	return indexformat.V2{Generation: generation}, map[schema.SourceEntryRef][]byte{ref: []byte(text)}
}
