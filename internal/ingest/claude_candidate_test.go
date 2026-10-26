package ingest_test

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/claude_producer.yaml
var claudeProducerYAML []byte

//go:embed testdata/claude_producer.manifest.yaml
var claudeProducerManifestYAML []byte

// claudeProducerEntry is one expected stored entry at the classifier seam. A
// parent or paired tool identity is named by the INDEX of the entry it must
// match inside the same case, so the fixture states structure without carrying
// opaque refs it cannot know.
type claudeProducerEntry struct {
	Depth               int                       `yaml:"depth"`
	Role                string                    `yaml:"role"`
	EntryType           string                    `yaml:"entryType"`
	PartType            string                    `yaml:"partType"`
	HasThinking         bool                      `yaml:"hasThinking"`
	ParentIndex         int                       `yaml:"parentIndex"`
	Preview             string                    `yaml:"preview"`
	PreviewAbsent       bool                      `yaml:"previewAbsent"`
	PreviewContains     string                    `yaml:"previewContains"`
	PreviewUnchecked    bool                      `yaml:"previewUnchecked"`
	ToolCallIDFromIndex int                       `yaml:"toolCallIDFromIndex"`
	ToolCallIDLiteral   string                    `yaml:"toolCallIDLiteral"`
	ExtraContains       string                    `yaml:"extraContains"`
	RawByteLength       int                       `yaml:"rawByteLength"`
	Provenance          *claudeProducerProvenance `yaml:"provenance"`
}

// claudeProducerProvenance is the six-dimension provenance tuple one admitted
// submission must carry, stated literally so no dimension can drift.
type claudeProducerProvenance struct {
	Origin        string `yaml:"origin"`
	Actor         string `yaml:"actor"`
	Delivery      string `yaml:"delivery"`
	Ownership     string `yaml:"ownership"`
	Evidence      string `yaml:"evidence"`
	InputModality string `yaml:"inputModality"`
}

type claudeProducerCase struct {
	Name string `yaml:"name"`
	// Transcript is the attempt's captured bytes.
	Transcript string `yaml:"transcript"`
	// Siblings holds structural proof files the legacy fallback may read, keyed
	// by absolute path.
	Siblings map[string]string `yaml:"siblings"`
	// RecordedSessionID defaults to the shared fixture session.
	RecordedSessionID string `yaml:"recordedSessionId"`
	// RecordedParent is the recorded parent identity, when the session has one.
	RecordedParent string `yaml:"recordedParent"`
	// MetadataSchemaVersion overrides the recorded sidecar revision (0 keeps
	// the current one).
	MetadataSchemaVersion int `yaml:"metadataSchemaVersion"`
	// SourcePath defaults to /native/<session>.jsonl.
	SourcePath string `yaml:"sourcePath"`
	// SentinelLocator, when set, becomes the recorded source path and must not
	// appear in a refusal.
	SentinelLocator string `yaml:"sentinelLocator"`
	// RecordedContextFrom carries a recorded context_from proof naming it.
	RecordedContextFrom string `yaml:"recordedContextFrom"`
	// RecordedStartedBy carries a recorded started_by proof naming it.
	RecordedStartedBy string `yaml:"recordedStartedBy"`
	// RecordedContentHash binds the recorded relationship to the transcript
	// bytes, as a stored artifact does.
	RecordedContentHash bool `yaml:"recordedContentHash"`
	// RecordedContentHashMismatch records a content hash that names other
	// bytes: the relationship is present but not bound to this transcript.
	RecordedContentHashMismatch bool `yaml:"recordedContentHashMismatch"`
	// MetadataSchemaVersionOmitted writes an absent (0) sidecar revision.
	MetadataSchemaVersionOmitted bool `yaml:"metadataSchemaVersionOmitted"`
	// Relationship and Target assert the emitted generation provenance.
	Relationship string `yaml:"relationship"`
	Target       string `yaml:"target"`
	// ContentOmitted marks the attempt as carrying the source-omitted flag.
	ContentOmitted bool `yaml:"contentOmitted"`
	// MaxRecordBytes overrides the per-record limit (0 keeps the production one).
	MaxRecordBytes int `yaml:"maxRecordBytes"`
	// SiblingReadCounts pins the exact read count of each structural sibling
	// proof file; the source path itself must never be read.
	SiblingReadCounts map[string]int `yaml:"siblingReadCounts"`

	WantRefusal            bool   `yaml:"wantRefusal"`
	WantRefusalLocatorFree bool   `yaml:"wantRefusalLocatorFree"`
	Completeness           string `yaml:"completeness"`
	TurnCount              int    `yaml:"turnCount"`
	InputSubmissionCount   int    `yaml:"inputSubmissionCount"`
	TitleRefCount          int    `yaml:"titleRefCount"`
	// DistinctSubmissionRefs requires the admitted submissions' refs to be
	// pairwise distinct, so two answers never collapse into one group.
	DistinctSubmissionRefs bool                  `yaml:"distinctSubmissionRefs"`
	Keys                   []string              `yaml:"keys"`
	AcceptKeys             []string              `yaml:"acceptKeys"`
	Entries                []claudeProducerEntry `yaml:"entries"`
}

type claudeProducerDocument struct {
	Cases []claudeProducerCase `yaml:"cases"`
}

func loadClaudeProducerCases(t *testing.T) []claudeProducerCase {
	t.Helper()
	var document claudeProducerDocument
	if err := testutil.DecodeFixtureYAML(claudeProducerYAML, &document); err != nil {
		t.Fatalf("decode claude producer fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(claudeProducerManifestYAML, "claude producer")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "claude producer"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// TestClaudeProducerFixture drives the production Claude managed-generation
// builder over the captured-bytes fixtures and asserts the literal classifier
// and generation outcomes each case pins.
func TestClaudeProducerFixture(t *testing.T) {
	t.Parallel()
	for _, row := range loadClaudeProducerCases(t) {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			runClaudeProducerCase(t, row)
		})
	}
}

func runClaudeProducerCase(t *testing.T, row claudeProducerCase) {
	t.Helper()
	sessionID := row.RecordedSessionID
	if sessionID == "" {
		sessionID = testutil.TestSessionUUID
	}
	sourcePath := row.SourcePath
	if sourcePath == "" {
		sourcePath = "/native/" + sessionID + ".jsonl"
	}
	if row.SentinelLocator != "" {
		sourcePath = "/private/" + row.SentinelLocator + "/" + sessionID + ".jsonl"
	}

	// The counting decorator proves the ordinary bytes-proven branch reads no
	// provider source at all, and that the legacy fallback reads only the
	// structural siblings it consults.
	mfs := testutil.NewCountingFS(testutil.NewMemFS())
	for path, content := range row.Siblings {
		if err := mfs.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write sibling proof %q: %v", path, err)
		}
	}

	metadata := makeReindexMeta(t, sessionID, sourcePath)
	if row.RecordedContentHash {
		metadata.ContentHash = schema.ComputeTranscriptHash([]byte(row.Transcript))
	}
	if row.RecordedContentHashMismatch {
		metadata.ContentHash = strings.Repeat("0", 64)
	}
	if row.MetadataSchemaVersionOmitted {
		metadata.SchemaVersion = 0
	} else if row.MetadataSchemaVersion != 0 {
		metadata.SchemaVersion = row.MetadataSchemaVersion
	}
	if row.RecordedParent != "" {
		parent, err := ingest.NewSessionID(row.RecordedParent)
		if err != nil {
			t.Fatalf("recorded parent %q: %v", row.RecordedParent, err)
		}
		metadata.ParentUUID = &parent
	}
	if row.RecordedContextFrom != "" {
		target, err := ingest.NewSessionID(row.RecordedContextFrom)
		if err != nil {
			t.Fatalf("recorded context_from %q: %v", row.RecordedContextFrom, err)
		}
		metadata.Relationships = []schema.SessionRelationship{{
			Kind:          schema.SessionRelationshipContextFrom,
			TargetState:   schema.RelationshipTargetKnown,
			TargetLocalID: &target,
			Evidence:      schema.EvidenceNativeTyped,
			Anchor:        &schema.PublicSourceAnchor{Kind: schema.PublicSourceAnchorGeneral},
		}}
	}

	if row.RecordedStartedBy != "" {
		owner, err := ingest.NewSessionID(row.RecordedStartedBy)
		if err != nil {
			t.Fatalf("recorded started_by %q: %v", row.RecordedStartedBy, err)
		}
		metadata.Relationships = append(metadata.Relationships, schema.SessionRelationship{
			Kind:          schema.SessionRelationshipStartedBy,
			TargetState:   schema.RelationshipTargetKnown,
			TargetLocalID: &owner,
			Evidence:      schema.EvidenceNativeTyped,
		})
	}

	idx := ingest.NewClaudeIndexer(mfs,
		ingest.WithClaudeFullDepth(true),
		ingest.WithClaudeFullContent(true),
		ingest.WithClaudeMaxRecordBytes(row.MaxRecordBytes),
	)
	sessionIDValue, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("fixture session id: %v", err)
	}
	candidate, err := idx.BuildGeneration(t.Context(), ingest.GenerationBuildInput{
		Session: ingest.DiscoveredSession{
			SessionID:      sessionIDValue,
			Harness:        ingest.HarnessClaudeCode,
			SourceFormat:   ingest.SourceFormatJSONL,
			SourcePath:     ingest.ResolvedPath(sourcePath),
			ContentOmitted: row.ContentOmitted,
		},
		GenerationID:       "gen-fixture-" + row.Name,
		RecordedMetadata:   metadata,
		CapturedTranscript: []byte(row.Transcript),
	})
	if got := mfs.Count(testutil.FSOpReadFile, sourcePath); got != 0 {
		t.Fatalf("provider-source reads of %q = %d, want 0: the ordinary branch proves the candidate from the captured bytes", sourcePath, got)
	}
	for path, want := range row.SiblingReadCounts {
		if got := mfs.Count(testutil.FSOpReadFile, path); got != want {
			t.Fatalf("sibling proof %q reads = %d, want %d", path, got, want)
		}
	}
	if row.WantRefusal {
		assertClaudeProducerRefusal(t, row, err, sessionID, sourcePath)
		return
	}
	if err != nil {
		t.Fatalf("BuildGeneration: %v", err)
	}
	assertClaudeProducerCandidate(t, row, candidate)
}

func assertClaudeProducerRefusal(t *testing.T, row claudeProducerCase, err error, sessionID, sourcePath string) {
	t.Helper()
	if err == nil {
		t.Fatalf("BuildGeneration succeeded, want the fixed-shape refusal")
	}
	message := err.Error()
	if !strings.HasPrefix(message, "ingest.ClaudeIndexer.BuildGeneration: harness claude-code session ") {
		t.Fatalf("refusal is not the fixed Claude shape: %s", message)
	}
	if !strings.Contains(message, sessionID) {
		t.Fatalf("refusal does not name the session identity: %s", message)
	}
	if row.WantRefusalLocatorFree {
		for _, secret := range []string{sourcePath, row.SentinelLocator, "/native/", "/private/"} {
			if secret == "" {
				continue
			}
			if strings.Contains(message, secret) {
				t.Fatalf("refusal leaks the recorded locator %q: %s", secret, message)
			}
		}
		for _, kernel := range []string{"transcript sessionId disagrees", "no capture was written", "sidechain", "agentId", "sessionId"} {
			if strings.Contains(message, kernel) {
				t.Fatalf("refusal leaks a raw kernel cause %q: %s", kernel, message)
			}
		}
	}
}

func assertClaudeProducerCandidate(t *testing.T, row claudeProducerCase, candidate ingest.GenerationCandidate) {
	t.Helper()
	generation := candidate.Result.Generation
	wantSchema := ingest.CurrentSchemaVersion
	if !row.MetadataSchemaVersionOmitted && ingest.MetadataSchemaVersionIsCurrent(row.MetadataSchemaVersion) {
		wantSchema = row.MetadataSchemaVersion
	}
	if generation.Metadata.SchemaVersion != wantSchema {
		t.Fatalf("metadata schemaVersion = %d, want %d", generation.Metadata.SchemaVersion, wantSchema)
	}
	if row.Relationship != "" {
		if len(generation.Metadata.Relationships) != 1 {
			t.Fatalf("relationships = %+v, want one %s", generation.Metadata.Relationships, row.Relationship)
		}
		relationship := generation.Metadata.Relationships[0]
		if string(relationship.Kind) != row.Relationship {
			t.Fatalf("relationship kind = %q, want %q", relationship.Kind, row.Relationship)
		}
		target := ""
		if relationship.TargetLocalID != nil {
			target = relationship.TargetLocalID.String()
		}
		if target != row.Target {
			t.Fatalf("relationship target = %q, want %q", target, row.Target)
		}
	} else if len(generation.Metadata.Relationships) != 0 {
		t.Fatalf("relationships = %+v, want none", generation.Metadata.Relationships)
	}
	if got := string(generation.Completeness); got != row.Completeness {
		t.Fatalf("completeness = %q, want %q", got, row.Completeness)
	}
	if got := generation.Metadata.Stats.TurnCount; got != row.TurnCount {
		t.Fatalf("turnCount = %d, want %d", got, row.TurnCount)
	}
	if row.InputSubmissionCount < 0 {
		if generation.Metadata.Stats.InputSubmissionCount != nil {
			t.Fatalf("inputSubmissionCount = %d, want unmeasured", *generation.Metadata.Stats.InputSubmissionCount)
		}
	} else if generation.Metadata.Stats.InputSubmissionCount == nil {
		t.Fatalf("inputSubmissionCount is unmeasured, want %d", row.InputSubmissionCount)
	} else if got := int(*generation.Metadata.Stats.InputSubmissionCount); got != row.InputSubmissionCount {
		t.Fatalf("inputSubmissionCount = %d, want %d", got, row.InputSubmissionCount)
	}
	if got := len(generation.TitleRefs); got != row.TitleRefCount {
		t.Fatalf("titleRefCount = %d, want %d", got, row.TitleRefCount)
	}
	assertClaudeProducerKeys(t, row, generation)
	assertClaudeProducerEntries(t, row, generation.Main.Entries)
	if row.DistinctSubmissionRefs {
		_, submissions := claudeAliasIndex(t, candidate)
		seen := make(map[schema.SubmissionRef]string, len(submissions))
		for key, ref := range submissions {
			if ref == "" {
				t.Fatalf("submission key %q has no submission ref", key)
			}
			if other, duplicate := seen[ref]; duplicate {
				t.Errorf("submission keys %q and %q share the submission ref %q", other, key, ref)
			}
			seen[ref] = key
		}
	}
}
func assertClaudeProducerKeys(t *testing.T, row claudeProducerCase, generation indexformat.Generation) {
	t.Helper()
	var blockKeys []string
	accept := map[string]bool{}
	for _, alias := range generation.Aliases {
		switch {
		case strings.HasPrefix(alias.NativeKey, "block:"):
			blockKeys = append(blockKeys, strings.TrimPrefix(alias.NativeKey, "block:"))
		case strings.HasPrefix(alias.NativeKey, "accept:"):
			accept[strings.TrimPrefix(alias.NativeKey, "accept:")] = true
		}
	}
	// The generation's alias table is normalized into sorted order, so the
	// classifier's key set is compared as a set: every key it emitted is
	// declared, and every declared key is emitted.
	sortedBlockKeys := slices.Clone(blockKeys)
	slices.Sort(sortedBlockKeys)
	wantKeys := slices.Clone(row.Keys)
	slices.Sort(wantKeys)
	if !slices.Equal(sortedBlockKeys, wantKeys) {
		t.Errorf("block native keys = %v, want %v", blockKeys, row.Keys)
	}
	wantAccept := make(map[string]bool, len(row.AcceptKeys))
	for _, key := range row.AcceptKeys {
		wantAccept[key] = true
	}
	for key := range wantAccept {
		if !accept[key] {
			t.Errorf("submission key %q is missing from the accept aliases %v", key, accept)
		}
	}
	for key := range accept {
		if !wantAccept[key] {
			t.Errorf("unexpected submission key %q in the accept aliases", key)
		}
	}
}

func assertClaudeProducerEntries(t *testing.T, row claudeProducerCase, entries []schema.SessionEntry) {
	t.Helper()
	if len(entries) != len(row.Entries) {
		t.Fatalf("stored entries = %d, want %d", len(entries), len(row.Entries))
	}
	for i, want := range row.Entries {
		entry := entries[i]
		where := fmt.Sprintf("entry[%d]", i)
		if entry.Depth != want.Depth {
			t.Errorf("%s depth = %d, want %d", where, entry.Depth, want.Depth)
		}
		if string(entry.Role) != want.Role {
			t.Errorf("%s role = %q, want %q", where, entry.Role, want.Role)
		}
		if string(entry.EntryType) != want.EntryType {
			t.Errorf("%s entryType = %q, want %q", where, entry.EntryType, want.EntryType)
		}
		if want.PartType == "" {
			if entry.PartType != nil {
				t.Errorf("%s partType = %q, want none", where, *entry.PartType)
			}
		} else if entry.PartType == nil || *entry.PartType != want.PartType {
			t.Errorf("%s partType = %v, want %q", where, entry.PartType, want.PartType)
		}
		if entry.HasThinking != want.HasThinking {
			t.Errorf("%s hasThinking = %t, want %t", where, entry.HasThinking, want.HasThinking)
		}
		if want.ParentIndex < 0 {
			if entry.ParentIndex != nil {
				t.Errorf("%s parentIndex = %d, want none", where, *entry.ParentIndex)
			}
		} else if entry.ParentIndex == nil || *entry.ParentIndex != want.ParentIndex {
			t.Errorf("%s parentIndex = %v, want %d", where, entry.ParentIndex, want.ParentIndex)
		}
		if want.PreviewAbsent {
			if entry.ContentPreview != nil {
				t.Errorf("%s contentPreview = %q, want none", where, *entry.ContentPreview)
			}
		} else if !want.PreviewUnchecked {
			if want.PreviewContains != "" {
				if entry.ContentPreview == nil || !strings.Contains(*entry.ContentPreview, want.PreviewContains) {
					t.Errorf("%s contentPreview = %v, want it to contain %q", where, entry.ContentPreview, want.PreviewContains)
				}
			} else if entry.ContentPreview == nil || *entry.ContentPreview != want.Preview {
				t.Errorf("%s contentPreview = %v, want %q", where, entry.ContentPreview, want.Preview)
			}
		}
		if want.ToolCallIDFromIndex >= 0 {
			if want.ToolCallIDFromIndex >= len(entries) {
				t.Fatalf("%s toolCallIDFromIndex %d is out of range", where, want.ToolCallIDFromIndex)
			}
			wantID := string(entries[want.ToolCallIDFromIndex].SourceEntryRef)
			if entry.ToolCallID == nil || *entry.ToolCallID != wantID {
				t.Errorf("%s toolCallID = %v, want the ref %q", where, entry.ToolCallID, wantID)
			}
		}
		if want.ToolCallIDLiteral != "" {
			if entry.ToolCallID == nil || *entry.ToolCallID != want.ToolCallIDLiteral {
				t.Errorf("%s toolCallID = %v, want literal %q", where, entry.ToolCallID, want.ToolCallIDLiteral)
			}
		}
		if want.ExtraContains != "" {
			if entry.Extra == nil || !strings.Contains(*entry.Extra, want.ExtraContains) {
				t.Errorf("%s extra = %v, want it to contain %q", where, entry.Extra, want.ExtraContains)
			}
		}
		if want.RawByteLength > 0 {
			if entry.RawByteLength == nil || *entry.RawByteLength != want.RawByteLength {
				t.Errorf("%s rawByteLength = %v, want %d", where, entry.RawByteLength, want.RawByteLength)
			}
		}
		if want.Provenance != nil {
			if entry.Provenance == nil {
				t.Errorf("%s has no provenance, want the submitted-input tuple", where)
			} else {
				got := claudeProducerProvenance{
					Origin:        string(entry.Provenance.Origin),
					Actor:         string(entry.Provenance.Actor),
					Delivery:      string(entry.Provenance.Delivery),
					Ownership:     string(entry.Provenance.Ownership),
					Evidence:      string(entry.Provenance.Evidence),
					InputModality: string(entry.Provenance.InputModality),
				}
				if got != *want.Provenance {
					t.Errorf("%s provenance = %+v, want %+v", where, got, *want.Provenance)
				}
			}
		}
	}
}
