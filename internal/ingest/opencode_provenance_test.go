package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/opencode_provenance.yaml
var openCodeProvenanceYAML []byte

//go:embed testdata/opencode_provenance.manifest.yaml
var openCodeProvenanceManifestYAML []byte

type ocProvScope struct {
	SessionID        string `yaml:"sessionId"`
	Shape            string `yaml:"shape"`
	ParentNullProven bool   `yaml:"parentNullProven"`
	HasParent        bool   `yaml:"hasParent"`
}

type ocProvRow struct {
	ID          string `yaml:"id"`
	Type        string `yaml:"type"`
	Seq         int64  `yaml:"seq"`
	TimeCreated int64  `yaml:"timeCreated"`
	TimeUpdated int64  `yaml:"timeUpdated"`
	Data        string `yaml:"data"`
}

type ocProvAllocator struct {
	Entries     []string `yaml:"entries"`
	Submissions []string `yaml:"submissions"`
}

type ocProvEntryExpect struct {
	Ref         string `yaml:"ref"`
	Index       int    `yaml:"index"`
	Role        string `yaml:"role"`
	EntryType   string `yaml:"entryType"`
	Depth       int    `yaml:"depth"`
	ParentIndex *int   `yaml:"parentIndex"`
	Content     string `yaml:"content"`
	ToolInput   string `yaml:"toolInput"`
	ToolOutput  string `yaml:"toolOutput"`
	ToolCallID  string `yaml:"toolCallId"`
	Submission  string `yaml:"submission"`
	Origin      string `yaml:"origin"`
	Actor       string `yaml:"actor"`
	Delivery    string `yaml:"delivery"`
	Ownership   string `yaml:"ownership"`
	Evidence    string `yaml:"evidence"`
	Modality    string `yaml:"modality"`
}

type ocProvWant struct {
	Own          *int64              `yaml:"own"`
	TurnCount    int                 `yaml:"turnCount"`
	TitleRefs    []string            `yaml:"titleRefs"`
	SkippedRows  int                 `yaml:"skippedRows"`
	Completeness string              `yaml:"completeness"`
	Main         []ocProvEntryExpect `yaml:"main"`
}

type ocProvCase struct {
	Name         string          `yaml:"name"`
	Scope        ocProvScope     `yaml:"scope"`
	AgentDeliver bool            `yaml:"agentDelivered"`
	Allocator    ocProvAllocator `yaml:"allocator"`
	Rows         []ocProvRow     `yaml:"rows"`
	Want         ocProvWant      `yaml:"want"`
}

type ocProvHistoricalPart struct {
	ID   string `yaml:"id"`
	Data string `yaml:"data"`
}

type ocProvHistoricalMessage struct {
	ID    string                 `yaml:"id"`
	Data  string                 `yaml:"data"`
	Parts []ocProvHistoricalPart `yaml:"parts"`
}

type ocProvHistoricalCase struct {
	Name      string                    `yaml:"name"`
	SessionID string                    `yaml:"sessionId"`
	Allocator ocProvAllocator           `yaml:"allocator"`
	Messages  []ocProvHistoricalMessage `yaml:"messages"`
	Want      ocProvWant                `yaml:"want"`
}

type ocProvDocument struct {
	Cases          []ocProvCase           `yaml:"cases"`
	HistoricalCase []ocProvHistoricalCase `yaml:"historicalCases"`
}

func loadOpenCodeProvenanceDocument(t *testing.T) ocProvDocument {
	t.Helper()
	var document ocProvDocument
	if err := testutil.DecodeFixtureYAML(openCodeProvenanceYAML, &document); err != nil {
		t.Fatalf("decode OpenCode provenance fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(openCodeProvenanceManifestYAML, "OpenCode provenance")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(document.Cases)+len(document.HistoricalCase))
	for _, row := range document.Cases {
		names = append(names, row.Name)
		if len(row.Rows) == 0 {
			t.Fatalf("OpenCode provenance fixture case %q has no rows; a case without native rows passes without invoking a decoder; declare at least one row", row.Name)
		}
	}
	for _, row := range document.HistoricalCase {
		names = append(names, row.Name)
		if len(row.Messages) == 0 {
			t.Fatalf("OpenCode provenance fixture historical case %q has no messages; a case without native messages passes without invoking a decoder; declare at least one message", row.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "OpenCode provenance"); err != nil {
		t.Fatal(err)
	}
	return document
}

func loadOpenCodeProvenanceCases(t *testing.T) []ocProvCase {
	t.Helper()
	return loadOpenCodeProvenanceDocument(t).Cases
}

func TestOpenCodeHistoricalProvenanceThroughFiles(t *testing.T) {
	for _, row := range loadOpenCodeProvenanceDocument(t).HistoricalCase {
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			storage := filepath.Join(root, "storage")
			sessionDir := filepath.Join(storage, "session", "synthetic")
			if err := os.MkdirAll(sessionDir, 0o700); err != nil {
				t.Fatal(err)
			}
			sessionJSON := fmt.Sprintf(`{"id":%q,"time":{"created":1000}}`, row.SessionID)
			if err := os.WriteFile(filepath.Join(sessionDir, row.SessionID+".json"), []byte(sessionJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, message := range row.Messages {
				msgDir := filepath.Join(storage, "message", row.SessionID)
				if err := os.MkdirAll(msgDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(msgDir, message.ID+".json"), []byte(message.Data), 0o600); err != nil {
					t.Fatal(err)
				}
				if len(message.Parts) == 0 {
					continue
				}
				partDir := filepath.Join(storage, "part", message.ID)
				if err := os.MkdirAll(partDir, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, part := range message.Parts {
					if err := os.WriteFile(filepath.Join(partDir, part.ID+".json"), []byte(part.Data), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{})
			session := ingest.DiscoveredSession{
				SessionID:        ingest.SessionID(row.SessionID),
				Harness:          ingest.HarnessOpenCode,
				SourcePath:       ingest.ResolvedPath(filepath.Join(sessionDir, row.SessionID+".json")),
				OriginalRoot:     ingest.ResolvedPath(root),
				TranscriptOrigin: ingest.TranscriptOriginFile,
			}
			blocks, err := indexer.OpenCodeHistoricalProvenanceBlocks(context.Background(), session)
			if err != nil {
				t.Fatalf("OpenCodeHistoricalProvenanceBlocks: %v", err)
			}
			capture := ingest.ClassifiedCapture{
				ID:                   "gen-" + row.Name,
				SessionID:            ingest.SessionID(row.SessionID),
				Harness:              ingest.HarnessOpenCode,
				SourceEvidenceDigest: "synthetic-evidence-digest",
				Completeness:         indexformat.GenerationCompletenessComplete,
				Metadata: schema.UnifiedMetadata{
					SchemaVersion: 1,
					SessionID:     schema.SessionID(row.SessionID),
					ModelHarness:  schema.HarnessOpenCode,
				},
				Blocks: blocks,
			}
			allocator := &queueAllocator{entry: append([]string(nil), row.Allocator.Entries...), submission: append([]string(nil), row.Allocator.Submissions...)}
			built, err := ingest.BuildV2(capture, allocator)
			if err != nil {
				t.Fatalf("BuildV2: %v", err)
			}
			assertOpenCodeCounts(t, built.Generation, row.Want)
			assertOpenCodeEntries(t, built.Generation.Main.Entries, row.Want.Main)
		})
	}
}

func assertOpenCodeCounts(t *testing.T, generation indexformat.Generation, want ocProvWant) {
	t.Helper()
	if want.Own == nil {
		if generation.Metadata.Stats.InputSubmissionCount != nil {
			t.Fatalf("inputSubmissionCount = %d, want absent", *generation.Metadata.Stats.InputSubmissionCount)
		}
	} else if generation.Metadata.Stats.InputSubmissionCount == nil {
		t.Fatalf("inputSubmissionCount = absent, want %d", *want.Own)
	} else if *generation.Metadata.Stats.InputSubmissionCount != *want.Own {
		t.Fatalf("inputSubmissionCount = %d, want %d", *generation.Metadata.Stats.InputSubmissionCount, *want.Own)
	}
	if generation.Metadata.Stats.TurnCount != want.TurnCount {
		t.Fatalf("turnCount = %d, want %d", generation.Metadata.Stats.TurnCount, want.TurnCount)
	}
	gotTitle := make([]string, len(generation.TitleRefs))
	for i, ref := range generation.TitleRefs {
		gotTitle[i] = string(ref)
	}
	if !equalStringSlices(gotTitle, want.TitleRefs) {
		t.Fatalf("titleRefs = %v, want %v", gotTitle, want.TitleRefs)
	}
}

func assertOpenCodeEntries(t *testing.T, entries []schema.SessionEntry, want []ocProvEntryExpect) {
	t.Helper()
	if len(entries) != len(want) {
		t.Fatalf("main entries = %d, want %d (%+v)", len(entries), len(want), entries)
	}
	for i, expected := range want {
		entry := entries[i]
		label := "main[" + itoa(i) + "]"
		if string(entry.SourceEntryRef) != expected.Ref {
			t.Fatalf("%s ref = %q, want %q", label, entry.SourceEntryRef, expected.Ref)
		}
		if entry.EntryIndex != expected.Index {
			t.Fatalf("%s index = %d, want %d", label, entry.EntryIndex, expected.Index)
		}
		if string(entry.Role) != expected.Role {
			t.Fatalf("%s role = %q, want %q", label, entry.Role, expected.Role)
		}
		if string(entry.EntryType) != expected.EntryType {
			t.Fatalf("%s entryType = %q, want %q", label, entry.EntryType, expected.EntryType)
		}
		if entry.Depth != expected.Depth {
			t.Fatalf("%s depth = %d, want %d", label, entry.Depth, expected.Depth)
		}
		assertOptionalInt(t, label+" parentIndex", entry.ParentIndex, expected.ParentIndex)
		if expected.EntryType != "tool_use" && expected.EntryType != "tool_result" {
			if got := previewOrEmpty(entry.ContentPreview); got != expected.Content {
				t.Fatalf("%s content = %q, want %q", label, got, expected.Content)
			}
		}
		if got := previewOrEmpty(entry.ToolInput); got != expected.ToolInput {
			t.Fatalf("%s toolInput = %q, want %q", label, got, expected.ToolInput)
		}
		if got := previewOrEmpty(entry.ToolOutput); got != expected.ToolOutput {
			t.Fatalf("%s toolOutput = %q, want %q", label, got, expected.ToolOutput)
		}
		if got := previewOrEmpty(entry.ToolCallID); got != expected.ToolCallID {
			t.Fatalf("%s toolCallId = %q, want %q", label, got, expected.ToolCallID)
		}
		if entry.Provenance == nil {
			t.Fatalf("%s has no provenance, want %+v", label, expected)
		}
		provenance := entry.Provenance
		if string(provenance.SubmissionRef) != expected.Submission {
			t.Fatalf("%s submissionRef = %q, want %q", label, provenance.SubmissionRef, expected.Submission)
		}
		if string(provenance.Origin) != expected.Origin {
			t.Fatalf("%s origin = %q, want %q", label, provenance.Origin, expected.Origin)
		}
		if string(provenance.Actor) != expected.Actor {
			t.Fatalf("%s actor = %q, want %q", label, provenance.Actor, expected.Actor)
		}
		if string(provenance.Delivery) != expected.Delivery {
			t.Fatalf("%s delivery = %q, want %q", label, provenance.Delivery, expected.Delivery)
		}
		if string(provenance.Ownership) != expected.Ownership {
			t.Fatalf("%s ownership = %q, want %q", label, provenance.Ownership, expected.Ownership)
		}
		if string(provenance.Evidence) != expected.Evidence {
			t.Fatalf("%s evidence = %q, want %q", label, provenance.Evidence, expected.Evidence)
		}
		if string(provenance.InputModality) != expected.Modality {
			t.Fatalf("%s modality = %q, want %q", label, provenance.InputModality, expected.Modality)
		}
	}
}

func previewOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func assertOptionalInt(t *testing.T, label string, got, want *int) {
	t.Helper()
	if got == nil && want == nil {
		return
	}
	if got == nil || want == nil {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	if *got != *want {
		t.Fatalf("%s = %d, want %d", label, *got, *want)
	}
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

//go:embed testdata/opencode_provenance_flow.yaml
var openCodeProvenanceFlowYAML []byte

//go:embed testdata/opencode_provenance_flow.manifest.yaml
var openCodeProvenanceFlowManifestYAML []byte

const openCodeFlowSourceFixture = "native-current-rows"

type ocFlowRow struct {
	ID          string `yaml:"id"`
	Type        string `yaml:"type"`
	Seq         int64  `yaml:"seq"`
	TimeCreated int64  `yaml:"timeCreated"`
	TimeUpdated int64  `yaml:"timeUpdated"`
	Data        string `yaml:"data"`
}

type ocFlowFork struct {
	SourceSessionID  string `yaml:"sourceSessionId"`
	BeforeSeq        *int64 `yaml:"beforeSeq"`
	ThroughSeq       *int64 `yaml:"throughSeq"`
	ThroughCompleted bool   `yaml:"throughCompleted"`
}

type ocFlowParent struct {
	ID   string      `yaml:"id"`
	Rows []ocFlowRow `yaml:"rows"`
}

type ocFlowCase struct {
	Name               string        `yaml:"name"`
	Kind               string        `yaml:"kind"`
	SessionID          string        `yaml:"sessionId"`
	Source             string        `yaml:"source"`
	Sentinel           string        `yaml:"sentinel"`
	Fork               *ocFlowFork   `yaml:"fork"`
	BoundedFork        *ocFlowFork   `yaml:"boundedFork"`
	UnprovenFork       *ocFlowFork   `yaml:"unprovenFork"`
	ForkParent         *ocFlowParent `yaml:"forkParent"`
	AppendRows         []ocFlowRow   `yaml:"appendRows"`
	ReplaceRows        []ocFlowRow   `yaml:"replaceRows"`
	DecodeRows         []ocFlowRow   `yaml:"decodeRows"`
	DeliveryMessageIDs []string      `yaml:"deliveryMessageIds"`
	PayloadEditID      string        `yaml:"payloadEditId"`
	PayloadEditData    string        `yaml:"payloadEditData"`
	LargeRowPrefix     string        `yaml:"largeRowPrefix"`
	LargeRowPadding    int           `yaml:"largeRowPadding"`
	ExpectCopies       int           `yaml:"expectCopies"`
	ExpectStep         string        `yaml:"expectStep"`
	AuthorityStatus    string        `yaml:"authorityStatus"`
	AuthoritySource    string        `yaml:"authoritySource"`
	AuthorityOrigin    string        `yaml:"authorityOrigin"`
	AuthorityFormat    string        `yaml:"authorityFormat"`
	AuthorityCode      string        `yaml:"authorityFailureCode"`
	ExpectMessage      string        `yaml:"expectMessage"`
	ExpectSequence     int64         `yaml:"expectSequence"`
}

type ocFlowDocument struct {
	Cases []ocFlowCase `yaml:"cases"`
}

func loadOpenCodeProvenanceFlowDocument(t *testing.T) ocFlowDocument {
	t.Helper()
	var document ocFlowDocument
	if err := testutil.DecodeFixtureYAML(openCodeProvenanceFlowYAML, &document); err != nil {
		t.Fatalf("decode OpenCode provenance flow fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(openCodeProvenanceFlowManifestYAML, "OpenCode provenance flow")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
		if err := validateOpenCodeFlowCase(row); err != nil {
			t.Fatalf("OpenCode provenance flow fixture %q: %v", row.Name, err)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "OpenCode provenance flow"); err != nil {
		t.Fatal(err)
	}
	return document
}

// validateOpenCodeFlowCase requires that every case carries the data its named
// kind needs, so a case cannot pass by omitting the mutation it claims to cover.
func validateOpenCodeFlowCase(row ocFlowCase) error {
	if strings.TrimSpace(row.Name) == "" || strings.TrimSpace(row.SessionID) == "" || strings.TrimSpace(row.Kind) == "" {
		return errors.New("name, kind and sessionId are required")
	}
	switch row.Kind {
	case "identity":
		if len(row.AppendRows) == 0 {
			return errors.New("the identity kind requires appendRows to prove an append keeps existing identities")
		}
	case "incomplete":
		if row.Fork == nil || row.Fork.SourceSessionID == "" || row.ForkParent == nil || len(row.ForkParent.Rows) == 0 {
			return errors.New("the incomplete kind requires an unproven fork and native fork-parent rows")
		}
	case "bounded-fork":
		if row.BoundedFork == nil || row.BoundedFork.BeforeSeq == nil || row.UnprovenFork == nil || row.ForkParent == nil || len(row.ForkParent.Rows) == 0 || len(row.AppendRows) == 0 || row.ExpectCopies <= 0 {
			return errors.New("the bounded-fork kind requires bounded and unproven forks, native rows, an appended suffix and the expected copy count")
		}
	case "digest":
		if len(row.DeliveryMessageIDs) == 0 || row.PayloadEditID == "" || row.PayloadEditData == "" {
			return errors.New("the digest kind requires a delivery correlation and a payload edit")
		}
	case "projection-refusal":
		if row.Sentinel == "" || len(row.ReplaceRows) == 0 || row.ExpectStep == "" {
			return errors.New("the projection-refusal kind requires a sentinel, replacement native rows and the expected refusal step")
		}
	case "allocator-refusal", "dependency-refusal":
		if row.Sentinel == "" || row.ExpectStep == "" {
			return errors.New("a refusal kind requires a sentinel and the expected refusal step")
		}
	case "missing-source", "unreadable-source":
		if row.Sentinel == "" || row.ExpectStep == "" {
			return errors.New("a source-refusal kind requires a sentinel and the expected refusal step")
		}
	case "decode-refusal":
		if row.Sentinel == "" || len(row.DecodeRows) == 0 {
			return errors.New("the decode-refusal kind requires a sentinel and native decode rows")
		}
	case "large-row":
		if row.LargeRowPrefix == "" || row.LargeRowPadding <= 0 {
			return errors.New("the large-row kind requires a prefix and a positive padding size")
		}
	case "authority-bridge":
		if row.AuthorityStatus == "" || row.AuthoritySource == "" || row.AuthorityFormat == "" {
			return errors.New("the authority-bridge kind requires the certificate status, source, and format")
		}
		if row.AuthorityOrigin == "" {
			return errors.New("the authority-bridge kind requires the certificate transcript origin")
		}
		if row.ExpectMessage == "" || row.ExpectSequence <= 0 {
			return errors.New("the authority-bridge kind requires the unsettled message and a positive sequence")
		}
	case "first-discovery-preview":
		if row.Source == "" {
			return errors.New("the first-discovery-preview kind requires the native source fixture that carries an unfinished own row")
		}
	default:
		return errors.New("kind is outside the closed set")
	}
	return nil
}

// TestOpenCodeProvenanceFlow drives the named flow cases over the real
// production path. Each case materializes real synthetic native SQLite, calls
// the production snapshot/classifier/indexer entry point, and asserts the
// configured outcome. No case supplies raw SQL from the fixture: the loader
// carries row data and the test applies it through the same typed writes the
// native reader consumes.
func TestOpenCodeProvenanceFlow(t *testing.T) {
	for _, row := range loadOpenCodeProvenanceFlowDocument(t).Cases {
		t.Run(row.Name, func(t *testing.T) {
			sourceName := row.Source
			if sourceName == "" {
				sourceName = openCodeFlowSourceFixture
			}
			source := testfixture.MaterializeByName(t, sourceName)
			switch row.Kind {
			case "identity":
				runOpenCodeFlowIdentity(t, source, row)
			case "incomplete":
				runOpenCodeFlowIncomplete(t, source, row)
			case "bounded-fork":
				runOpenCodeFlowBoundedFork(t, source, row)
			case "digest":
				runOpenCodeFlowDigest(t, source, row)
			case "projection-refusal":
				runOpenCodeFlowProjectionRefusal(t, source, row)
			case "allocator-refusal":
				runOpenCodeFlowAllocatorRefusal(t, source, row)
			case "dependency-refusal":
				runOpenCodeFlowDependencyRefusal(t, source, row)
			case "missing-source":
				runOpenCodeFlowMissingSource(t, source, row)
			case "unreadable-source":
				runOpenCodeFlowUnreadableSource(t, source, row)
			case "large-row":
				runOpenCodeFlowLargeRow(t, source, row)
			case "decode-refusal":
				runOpenCodeFlowDecodeRefusal(t, row)
			case "authority-bridge":
				runOpenCodeFlowAuthorityBridge(t, source, row)
			case "first-discovery-preview":
				runOpenCodeFlowFirstDiscoveryPreview(t, source, row)
			default:
				t.Fatalf("unsupported flow kind %q", row.Kind)
			}
		})
	}
}

func runOpenCodeFlowIdentity(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	session := openCodeFlowSession(row)
	first := indexOpenCodeNative(t, source, session, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()}, nil)
	state, err := ingest.PriorStateFromGeneration(first.Generation)
	if err != nil {
		t.Fatalf("PriorStateFromGeneration: %v", err)
	}
	prior := ingest.OpenCodeProvenancePrior{Aliases: state, HasCompleteGeneration: true}
	repeated := indexOpenCodeNative(t, source, session, prior, nil)
	assertOpenCodeExistingIdentitiesStable(t, "repeat", first.Generation, repeated.Generation)
	retried := indexOpenCodeNative(t, source, session, prior, nil)
	assertOpenCodeExistingIdentitiesStable(t, "retry", first.Generation, retried.Generation)

	applyOpenCodeFlowRows(t, source, row.SessionID, row.AppendRows)
	appended := indexOpenCodeNative(t, source, session, prior, nil)
	assertOpenCodeExistingIdentitiesStable(t, "append", first.Generation, appended.Generation)
	if len(appended.Generation.Main.Entries) <= len(first.Generation.Main.Entries) {
		t.Fatalf("append did not add a main entry: before %d, after %d", len(first.Generation.Main.Entries), len(appended.Generation.Main.Entries))
	}
}

func runOpenCodeFlowIncomplete(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	seedOpenCodeFlowForkParent(t, source, row)
	fork := openCodeFlowFork(row.Fork)
	session := openCodeFlowSession(row)

	first := indexOpenCodeNative(t, source, session, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()}, fork)
	if first.Generation.Completeness != indexformat.GenerationCompletenessIncompleteNew {
		t.Fatalf("first incomplete completeness = %q, want incomplete_new", first.Generation.Completeness)
	}
	if first.Generation.Metadata.Stats.InputSubmissionCount != nil {
		t.Fatalf("first incomplete inputSubmissionCount = %v, want absent", *first.Generation.Metadata.Stats.InputSubmissionCount)
	}
	if len(first.Generation.Main.Entries) == 0 {
		t.Fatal("first incomplete capture dropped the child's readable own work")
	}
	if len(first.Generation.Earlier) == 0 || len(first.Generation.Earlier[0].Content.Entries) == 0 {
		t.Fatal("first incomplete capture dropped the uncertain copied evidence")
	}

	completePrior := ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState(), HasCompleteGeneration: true}
	config := openCodeFlowProvenanceConfig(source, row.SessionID, completePrior, fork)
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	_, err := indexer.IndexTranscriptResult(context.Background(), session)
	var incomplete *ingest.OpenCodeIncompleteProvenanceError
	if !errors.As(err, &incomplete) {
		t.Fatalf("incomplete replacement error = %v, want OpenCodeIncompleteProvenanceError", err)
	}
}

// runOpenCodeFlowAuthorityBridge drives the production candidate exit with a
// bridged stored-capture authority and an unfinished own native row: the
// incomplete candidate is refused early with an actionable last-good-held
// result that names the unsettled message and sequence, and the certificate's
// own origin and format are preserved.
func runOpenCodeFlowAuthorityBridge(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	session := openCodeFlowSession(row)
	prior := ingest.OpenCodeProvenancePrior{
		Aliases: ingest.NewProjectionPriorState(),
		CaptureAuthority: &ingest.StoredCaptureAuthority{
			Status:           ingest.ContentCaptureStatus(row.AuthorityStatus),
			SourceAuthority:  ingest.ContentSourceAuthority(row.AuthoritySource),
			TranscriptOrigin: openCodeFlowOrigin(t, row.AuthorityOrigin),
			CaptureFormat:    ingest.ContentCaptureFormat(row.AuthorityFormat),
			FailureCode:      ingest.ContentCaptureFailureCode(row.AuthorityCode),
		},
	}
	config := openCodeFlowProvenanceConfig(source, row.SessionID, prior, nil)
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	_, err := indexer.IndexTranscriptResult(context.Background(), session)
	var incomplete *ingest.OpenCodeIncompleteProvenanceError
	if !errors.As(err, &incomplete) {
		t.Fatalf("authority bridge error = %v, want OpenCodeIncompleteProvenanceError", err)
	}
	diagnostics := strings.Join(incomplete.Diagnostics, "; ")
	if !strings.Contains(diagnostics, row.ExpectMessage) {
		t.Fatalf("refusal diagnostics %q do not name the unsettled message %q", diagnostics, row.ExpectMessage)
	}
	if !strings.Contains(diagnostics, fmt.Sprintf("sequence %d", row.ExpectSequence)) {
		t.Fatalf("refusal diagnostics %q do not name the unsettled sequence %d", diagnostics, row.ExpectSequence)
	}
	for label, want := range map[string]string{
		"status":  row.AuthorityStatus,
		"origin":  row.AuthorityOrigin,
		"format":  row.AuthorityFormat,
		"source":  row.AuthoritySource,
		"failure": row.AuthorityCode,
	} {
		if want == "" {
			continue
		}
		if !strings.Contains(diagnostics, want) {
			t.Fatalf("refusal diagnostics %q do not preserve the certificate %s %q", diagnostics, label, want)
		}
	}
}

// openCodeFlowOrigin maps a fixture origin name to the closed-set origin the
// bridged certificate carries and the refusal must state.
func openCodeFlowOrigin(t *testing.T, name string) ingest.TranscriptOrigin {
	t.Helper()
	switch name {
	case "file":
		return ingest.TranscriptOriginFile
	case "opencode-legacy-sqlite":
		return ingest.TranscriptOriginOpenCodeLegacySQLite
	case "opencode-current-sqlite":
		return ingest.TranscriptOriginOpenCodeCurrentSQLite
	default:
		t.Fatalf("authority-bridge case names an unknown transcript origin %q", name)
		return ingest.TranscriptOriginFile
	}
}

// runOpenCodeFlowFirstDiscoveryPreview drives the same unfinished native source
// with no active generation and no stored certificate: the incomplete candidate
// is a valid first-discovery preview, not a refusal, and the settled own work
// is still emitted.
func runOpenCodeFlowFirstDiscoveryPreview(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	session := openCodeFlowSession(row)
	config := openCodeFlowProvenanceConfig(source, row.SessionID, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()}, nil)
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	candidate, err := indexer.BuildNativeGeneration(context.Background(), session)
	if err != nil {
		t.Fatalf("first-discovery preview was refused: %v", err)
	}
	if candidate.Result.Generation.Completeness != indexformat.GenerationCompletenessIncompleteNew {
		t.Fatalf("first-discovery completeness = %q, want incomplete_new", candidate.Result.Generation.Completeness)
	}
	if len(candidate.Result.Generation.Main.Entries) == 0 {
		t.Fatal("first-discovery preview dropped the settled own work")
	}
}

func runOpenCodeFlowBoundedFork(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	seedOpenCodeFlowForkParent(t, source, row)
	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	bounded := openCodeFlowFork(row.BoundedFork)
	snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, row.SessionID, ingest.OpenCodeSnapshotOptions{Fork: bounded})
	if err != nil {
		t.Fatalf("bounded fork snapshot: %v", err)
	}
	if len(snapshot.Copied) != row.ExpectCopies {
		t.Fatalf("bounded captured copies = %d, want %d", len(snapshot.Copied), row.ExpectCopies)
	}
	digest := snapshot.SourceEvidenceDigest
	applyOpenCodeFlowRows(t, source, row.ForkParent.ID, row.AppendRows)
	again, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, row.SessionID, ingest.OpenCodeSnapshotOptions{Fork: bounded})
	if err != nil {
		t.Fatalf("bounded fork snapshot after append: %v", err)
	}
	if again.SourceEvidenceDigest != digest {
		t.Fatal("appending a parent suffix changed the bounded child proof")
	}
	unproven := openCodeFlowFork(row.UnprovenFork)
	if _, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, row.SessionID, ingest.OpenCodeSnapshotOptions{Fork: unproven}); err == nil {
		t.Fatal("unproven fork snapshot succeeded despite a malformed suffix row")
	}
}

func runOpenCodeFlowDigest(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	baseline, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, row.SessionID, ingest.OpenCodeSnapshotOptions{})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
	}
	if baseline.SourceEvidenceDigest == "" {
		t.Fatal("snapshot carries no source evidence digest")
	}
	delivered := make(map[string]bool, len(row.DeliveryMessageIDs))
	for _, id := range row.DeliveryMessageIDs {
		delivered[id] = true
	}
	correlated, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, row.SessionID, ingest.OpenCodeSnapshotOptions{AgentDeliveredIDs: delivered})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance with delivery correlation: %v", err)
	}
	if correlated.SourceEvidenceDigest == baseline.SourceEvidenceDigest {
		t.Fatal("changing the delivery correlation did not change the source evidence digest")
	}
	applyOpenCodeFlowPayloadEdit(t, source, row.PayloadEditID, row.PayloadEditData)
	mutated, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, row.SessionID, ingest.OpenCodeSnapshotOptions{})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance after payload edit: %v", err)
	}
	if mutated.SourceEvidenceDigest == baseline.SourceEvidenceDigest {
		t.Fatal("editing the captured payload did not change the source evidence digest")
	}
}

func runOpenCodeFlowProjectionRefusal(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	applyOpenCodeFlowRowsReplacing(t, source, row.SessionID, row.ReplaceRows)
	session := openCodeFlowSession(row)
	config := openCodeFlowProvenanceConfig(source, row.SessionID, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()}, nil)
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	_, err := indexer.IndexTranscriptResult(context.Background(), session)
	if err == nil {
		t.Fatal("duplicate native part identities produced a V2 candidate instead of a refusal")
	}
	assertOpenCodeSanitizedRefusal(t, err, row)
}

func runOpenCodeFlowAllocatorRefusal(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	applyOpenCodeFlowRowsReplacing(t, source, row.SessionID, row.ReplaceRows)
	session := openCodeFlowSession(row)
	config := openCodeFlowProvenanceConfig(source, row.SessionID, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()}, nil)
	config.Allocator = openCodeInvalidRefAllocator{}
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	_, err := indexer.IndexTranscriptResult(context.Background(), session)
	if err == nil {
		t.Fatal("an allocator that returns no valid ref produced a V2 candidate instead of a refusal")
	}
	assertOpenCodeSanitizedRefusal(t, err, row)
}

func runOpenCodeFlowDependencyRefusal(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	session := openCodeFlowSession(row)
	config := openCodeFlowProvenanceConfig(source, row.SessionID, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()}, nil)
	config.Prior = func(context.Context, ingest.DiscoveredSession) (ingest.OpenCodeProvenancePrior, error) {
		return ingest.OpenCodeProvenancePrior{}, errors.New("prior evidence load failed while reading " + row.Sentinel)
	}
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	_, err := indexer.IndexTranscriptResult(context.Background(), session)
	if err == nil {
		t.Fatal("a failing prior dependency produced a V2 candidate instead of a refusal")
	}
	assertOpenCodeSanitizedRefusal(t, err, row)
}

func runOpenCodeFlowMissingSource(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	missing := filepath.Join(filepath.Dir(source.Path), row.Sentinel+".db")
	_, err := adapter.SnapshotOpenCodeProvenance(context.Background(), missing, row.SessionID, ingest.OpenCodeSnapshotOptions{})
	if err == nil {
		t.Fatal("a missing native database produced a snapshot instead of a refusal")
	}
	assertOpenCodeSanitizedRefusal(t, err, row)
}

// runOpenCodeFlowUnreadableSource points the production adapter at a real
// directory in place of a native database. The real SQLite open fails and the
// returned refusal must stay free of the private locator.
func runOpenCodeFlowUnreadableSource(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	directory := filepath.Join(filepath.Dir(source.Path), row.Sentinel+".dir")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("create unreadable-source locator: %v", err)
	}
	_, err := adapter.SnapshotOpenCodeProvenance(context.Background(), directory, row.SessionID, ingest.OpenCodeSnapshotOptions{})
	if err == nil {
		t.Fatal("a directory used as the native database produced a snapshot instead of a refusal")
	}
	assertOpenCodeSanitizedRefusal(t, err, row)
}

// runOpenCodeFlowLargeRow proves that a single large OpenCode row is read and
// retained in full. OpenCode rows have no per-record size limit, so no snapshot
// or whole-capture bound may refuse or drop this row.
func runOpenCodeFlowLargeRow(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	text := row.LargeRowPrefix + strings.Repeat("x", row.LargeRowPadding)
	payload, err := json.Marshal(map[string]any{
		"id":   "msg_large_row",
		"type": "user",
		"text": text,
		"time": map[string]any{"created": 1000},
	})
	if err != nil {
		t.Fatalf("encode large native row: %v", err)
	}
	applyOpenCodeFlowRowsReplacing(t, source, row.SessionID, []ocFlowRow{{
		ID: "msg_large_row", Type: "user", Seq: 0, TimeCreated: 1000, TimeUpdated: 1000, Data: string(payload),
	}})
	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, row.SessionID, ingest.OpenCodeSnapshotOptions{})
	if err != nil {
		t.Fatalf("large-row snapshot: %v", err)
	}
	if len(snapshot.Messages) != 1 {
		t.Fatalf("large-row snapshot messages = %d, want 1", len(snapshot.Messages))
	}
	if got := snapshot.Messages[0].Message.Text; got != text {
		t.Fatalf("large-row text length = %d, want %d; the row was truncated or dropped", len(got), len(text))
	}
	metadata := schema.UnifiedMetadata{SchemaVersion: ingest.CurrentSchemaVersion, SessionID: schema.SessionID(row.SessionID), ModelHarness: schema.HarnessOpenCode}
	capture, err := ingest.BuildOpenCodeProvenanceCapture(snapshot, "gen-large-row", metadata, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()})
	if err != nil {
		t.Fatalf("large-row capture: %v", err)
	}
	built, err := ingest.BuildV2(capture, ingest.RandomRefAllocator{})
	if err != nil {
		t.Fatalf("large-row generation: %v", err)
	}
	for _, record := range built.Generation.Content {
		if record.ByteLength == int64(len(text)) {
			return
		}
	}
	t.Fatalf("large-row content of %d bytes was not retained in the generation", len(text))
}

// runOpenCodeFlowDecodeRefusal drives the real row decoder with the named
// native payload conflicts and requires every refusal to stay free of the
// arbitrary payload identity or type value it rejected.
func runOpenCodeFlowDecodeRefusal(t *testing.T, row ocFlowCase) {
	t.Helper()
	scope := ingest.OpenCodeProvenanceScope{SessionID: row.SessionID, Shape: ingest.OpenCodeProvenanceCurrent, ParentNullProven: true}
	for _, native := range row.DecodeRows {
		_, _, err := ingest.DecodeOpenCodeProvenanceRow(ingest.OpenCodeProvenanceRow{
			ID:        native.ID,
			SessionID: row.SessionID,
			Type:      native.Type,
			Data:      native.Data,
		}, scope)
		if err == nil {
			t.Fatalf("decode row %q accepted a conflicting native payload instead of refusing it", native.ID)
		}
		if strings.Contains(err.Error(), row.Sentinel) {
			t.Fatalf("decode refusal for row %q leaked the sentinel %q: %v", native.ID, row.Sentinel, err)
		}
	}
}

// assertOpenCodeSanitizedRefusal requires a refusal whose fixed category names
// the failed step and whose text never carries the case sentinel.
func assertOpenCodeSanitizedRefusal(t *testing.T, err error, row ocFlowCase) {
	t.Helper()
	if strings.Contains(err.Error(), row.Sentinel) {
		t.Fatalf("refusal leaked the sentinel %q: %v", row.Sentinel, err)
	}
	if row.ExpectStep != "" && !strings.Contains(err.Error(), row.ExpectStep) {
		t.Fatalf("refusal step = %v, want it to name %q", err, row.ExpectStep)
	}
}

func openCodeFlowSession(row ocFlowCase) ingest.DiscoveredSession {
	session := ingest.DiscoveredSession{
		SessionID:        ingest.SessionID(row.SessionID),
		Harness:          ingest.HarnessOpenCode,
		TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
	}
	if row.Sentinel != "" {
		// A refusal case carries a private sentinel locator so the registered
		// exit is proven never to add the managed source path back onto an
		// already-sanitized candidate refusal.
		session.SourcePath = ingest.ResolvedPath(filepath.Join("/", row.Sentinel))
	}
	return session
}

func openCodeFlowFork(spec *ocFlowFork) *ingest.OpenCodeForkProof {
	if spec == nil {
		return nil
	}
	return &ingest.OpenCodeForkProof{
		SourceSessionID:  spec.SourceSessionID,
		BeforeSeq:        spec.BeforeSeq,
		ThroughSeq:       spec.ThroughSeq,
		ThroughCompleted: spec.ThroughCompleted,
	}
}

func openCodeFlowProvenanceConfig(source testfixture.MaterializedSource, sessionID string, prior ingest.OpenCodeProvenancePrior, fork *ingest.OpenCodeForkProof) ingest.OpenCodeProvenanceIndexerConfig {
	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	return ingest.OpenCodeProvenanceIndexerConfig{
		Enabled: true,
		Snapshot: func(ctx context.Context, _ ingest.DiscoveredSession) (ingest.OpenCodeHistorySnapshot, error) {
			return adapter.SnapshotOpenCodeProvenance(ctx, source.Path, sessionID, ingest.OpenCodeSnapshotOptions{Fork: fork})
		},
		Metadata: func(_ ingest.DiscoveredSession) (schema.UnifiedMetadata, error) {
			return schema.UnifiedMetadata{SchemaVersion: 1, SessionID: schema.SessionID(sessionID), ModelHarness: schema.HarnessOpenCode}, nil
		},
		GenerationID: func(_ ingest.DiscoveredSession) string { return "gen-flow-" + sessionID },
		Prior: func(context.Context, ingest.DiscoveredSession) (ingest.OpenCodeProvenancePrior, error) {
			return prior, nil
		},
	}
}

// openCodeInvalidRefAllocator returns an invalid entry ref so the shared
// projection refuses allocation. It is a dependency double for the allocator
// seam, never a replacement for the code under test.
type openCodeInvalidRefAllocator struct{}

func (openCodeInvalidRefAllocator) NewEntryRef() (schema.SourceEntryRef, error) {
	return schema.SourceEntryRef(""), nil
}

func (openCodeInvalidRefAllocator) NewSubmissionRef() (schema.SubmissionRef, error) {
	return schema.SubmissionRef(""), nil
}

// seedOpenCodeFlowForkParent inserts the native parent session row and its
// messages and links the child to that parent. The row data comes from the
// fixture; the statement is the same parameterized write the native store uses,
// never a SQL string carried by the fixture.
func seedOpenCodeFlowForkParent(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	connection := openOpenCodeFlowConnection(t, source)
	defer func() { _ = connection.Close() }()
	if err := sqlitex.Execute(connection, "INSERT INTO session (id, parent_id, time_created, time_updated) VALUES (?1, '', ?2, ?3)", &sqlitex.ExecOptions{Args: []any{row.ForkParent.ID, int64(1), int64(1)}}); err != nil {
		t.Fatalf("insert native fork parent row: %v", err)
	}
	insertOpenCodeFlowRows(t, connection, row.ForkParent.ID, row.ForkParent.Rows)
	if err := sqlitex.Execute(connection, "UPDATE session SET parent_id = ?1 WHERE id = ?2", &sqlitex.ExecOptions{Args: []any{row.ForkParent.ID, row.SessionID}}); err != nil {
		t.Fatalf("link child session to its native fork parent: %v", err)
	}
}

func applyOpenCodeFlowRows(t *testing.T, source testfixture.MaterializedSource, sessionID string, rows []ocFlowRow) {
	t.Helper()
	connection := openOpenCodeFlowConnection(t, source)
	defer func() { _ = connection.Close() }()
	insertOpenCodeFlowRows(t, connection, sessionID, rows)
}

func applyOpenCodeFlowRowsReplacing(t *testing.T, source testfixture.MaterializedSource, sessionID string, rows []ocFlowRow) {
	t.Helper()
	connection := openOpenCodeFlowConnection(t, source)
	defer func() { _ = connection.Close() }()
	if err := sqlitex.Execute(connection, "DELETE FROM session_message WHERE session_id = ?1", &sqlitex.ExecOptions{Args: []any{sessionID}}); err != nil {
		t.Fatalf("clear native rows for session: %v", err)
	}
	insertOpenCodeFlowRows(t, connection, sessionID, rows)
}

func insertOpenCodeFlowRows(t *testing.T, connection *sqlite.Conn, sessionID string, rows []ocFlowRow) {
	t.Helper()
	for _, row := range rows {
		if err := sqlitex.Execute(connection, "INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", &sqlitex.ExecOptions{
			Args: []any{row.ID, sessionID, row.Type, row.TimeCreated, row.TimeUpdated, row.Data, row.Seq},
		}); err != nil {
			t.Fatalf("insert native row %q: %v", row.ID, err)
		}
	}
}

func applyOpenCodeFlowPayloadEdit(t *testing.T, source testfixture.MaterializedSource, messageID, data string) {
	t.Helper()
	connection := openOpenCodeFlowConnection(t, source)
	defer func() { _ = connection.Close() }()
	if err := sqlitex.Execute(connection, "UPDATE session_message SET data = ?1 WHERE id = ?2", &sqlitex.ExecOptions{Args: []any{data, messageID}}); err != nil {
		t.Fatalf("edit native payload for %q: %v", messageID, err)
	}
	if connection.Changes() != 1 {
		t.Fatalf("payload edit did not update exactly the named row %q", messageID)
	}
}

func openOpenCodeFlowConnection(t *testing.T, source testfixture.MaterializedSource) *sqlite.Conn {
	t.Helper()
	_ = testfixture.SnapshotSource(t, source)
	connection, err := sqlite.OpenConn(source.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic native source for a fixture write: %v", err)
	}
	return connection
}

const openCodeNativeChild = "ses_3cd91f52effeXd3QAJ54jOyzn1"

// TestOpenCodeProvenanceNativeReadOnlySnapshot proves the production snapshot
// path over a real synthetic OpenCode SQLite source: the typed user row with an
// explicit parent null admits one submission, the read issues SELECTs only, and
// the source's bytes and logical rows are unchanged.
func TestOpenCodeProvenanceNativeReadOnlySnapshot(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")
	before := testfixture.SnapshotSource(t, source)
	rowsBefore := countMaterializedCurrentRows(t, source.Path, openCodeNativeChild)

	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
	}
	if !snapshot.ParentNullProven || snapshot.HasParent {
		t.Fatalf("parentage = hasParent %t parentNullProven %t, want an explicit parent null", snapshot.HasParent, snapshot.ParentNullProven)
	}
	if len(snapshot.Messages) != 2 {
		t.Fatalf("snapshot rows = %d, want 2", len(snapshot.Messages))
	}
	if snapshot.SourceEvidenceDigest == "" {
		t.Fatal("snapshot carries no source evidence digest")
	}
	built := buildOpenCodeNativeGeneration(t, snapshot)
	if built.Generation.Metadata.Stats.InputSubmissionCount == nil || *built.Generation.Metadata.Stats.InputSubmissionCount != 1 {
		t.Fatalf("inputSubmissionCount = %v, want 1", built.Generation.Metadata.Stats.InputSubmissionCount)
	}
	if len(built.Generation.TitleRefs) != 1 {
		t.Fatalf("titleRefs = %v, want one admitted prose ref", built.Generation.TitleRefs)
	}

	testfixture.AssertUnchanged(t, source, before)
	if got := countMaterializedCurrentRows(t, source.Path, openCodeNativeChild); got != rowsBefore {
		t.Fatalf("child source rows = %d after the read, want %d", got, rowsBefore)
	}
}

// TestOpenCodeProvenanceIndexerWiring proves the candidate leaves the existing
// indexer entry point as a validated format-2 result. It also proves the path
// fails closed when its dependencies are missing rather than degrading to a
// thinner V1 result.
func TestOpenCodeProvenanceIndexerWiring(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")
	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	session := ingest.DiscoveredSession{
		SessionID:        ingest.SessionID(openCodeNativeChild),
		Harness:          ingest.HarnessOpenCode,
		TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite,
	}
	config := ingest.OpenCodeProvenanceIndexerConfig{
		Enabled: true,
		Snapshot: func(ctx context.Context, _ ingest.DiscoveredSession) (ingest.OpenCodeHistorySnapshot, error) {
			return adapter.SnapshotOpenCodeProvenance(ctx, source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{})
		},
		Metadata: func(_ ingest.DiscoveredSession) (schema.UnifiedMetadata, error) {
			return schema.UnifiedMetadata{SchemaVersion: 1, SessionID: schema.SessionID(openCodeNativeChild), ModelHarness: schema.HarnessOpenCode}, nil
		},
		GenerationID: func(_ ingest.DiscoveredSession) string { return "gen-native-wiring" },
	}
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	result, err := indexer.IndexTranscriptResult(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscriptResult: %v", err)
	}
	format, ok := result.(indexformat.V2)
	if !ok {
		t.Fatalf("IndexTranscriptResult returned %T, want indexformat.V2", result)
	}
	if format.Generation.Metadata.Stats.InputSubmissionCount == nil || *format.Generation.Metadata.Stats.InputSubmissionCount != 1 {
		t.Fatalf("inputSubmissionCount = %v, want 1", format.Generation.Metadata.Stats.InputSubmissionCount)
	}
	if format.Generation.ID != "gen-native-wiring" {
		t.Fatalf("generation id = %q, want gen-native-wiring", format.Generation.ID)
	}

	missing := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(ingest.OpenCodeProvenanceIndexerConfig{Enabled: true}))
	if _, err := missing.IndexTranscriptResult(context.Background(), session); err == nil || !strings.Contains(err.Error(), "misses its snapshot, metadata, or generation dependency") {
		t.Fatalf("IndexTranscriptResult with missing dependencies = %v, want a fail-closed dependency refusal", err)
	}
}

// TestOpenCodeProvenanceAdmissionThroughNativeSQL binds the admitted,
// agent-delivery, and unproved-child corpus to real native SQL acquisition: the
// session row proves parentage, the session_message rows carry the payloads,
// and the classified projection keeps the exact fixture assertions.
func TestOpenCodeProvenanceAdmissionThroughNativeSQL(t *testing.T) {
	for _, row := range loadOpenCodeProvenanceCases(t) {
		t.Run(row.Name, func(t *testing.T) {
			source := testfixture.MaterializeByName(t, "native-current-rows")
			seedOpenCodeProvenanceCase(t, source, row)
			options := ingest.OpenCodeSnapshotOptions{}
			if row.AgentDeliver {
				delivered := make(map[string]bool, len(row.Rows))
				for _, native := range row.Rows {
					delivered[native.ID] = true
				}
				options.AgentDeliveredIDs = delivered
			}
			adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
			snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, row.Scope.SessionID, options)
			if err != nil {
				t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
			}
			if snapshot.ParentNullProven != row.Scope.ParentNullProven || snapshot.HasParent != row.Scope.HasParent {
				t.Fatalf("snapshot parentage = nullProven %t hasParent %t, want %t/%t", snapshot.ParentNullProven, snapshot.HasParent, row.Scope.ParentNullProven, row.Scope.HasParent)
			}
			skipped := 0
			for _, msg := range snapshot.Messages {
				if !msg.Settled {
					skipped++
				}
			}
			if skipped != row.Want.SkippedRows {
				t.Fatalf("skipped unfinished rows = %d, want %d", skipped, row.Want.SkippedRows)
			}
			capture, err := ingest.BuildOpenCodeProvenanceCapture(snapshot, "gen-"+row.Name, schema.UnifiedMetadata{
				SchemaVersion: 1,
				SessionID:     schema.SessionID(row.Scope.SessionID),
				ModelHarness:  schema.HarnessOpenCode,
			}, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()})
			if err != nil {
				t.Fatalf("BuildOpenCodeProvenanceCapture: %v", err)
			}
			allocator := &queueAllocator{entry: append([]string(nil), row.Allocator.Entries...), submission: append([]string(nil), row.Allocator.Submissions...)}
			built, err := ingest.BuildV2(capture, allocator)
			if err != nil {
				t.Fatalf("BuildV2: %v", err)
			}
			assertOpenCodeCounts(t, built.Generation, row.Want)
			assertOpenCodeEntries(t, built.Generation.Main.Entries, row.Want.Main)
		})
	}
}

func indexOpenCodeNative(t *testing.T, source testfixture.MaterializedSource, session ingest.DiscoveredSession, prior ingest.OpenCodeProvenancePrior, fork *ingest.OpenCodeForkProof) indexformat.V2 {
	t.Helper()
	config := openCodeNativeProvenanceConfig(source, session.SessionID.String(), prior, fork)
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeProvenanceCapture(config))
	result, err := indexer.IndexTranscriptResult(context.Background(), session)
	if err != nil {
		t.Fatalf("IndexTranscriptResult: %v", err)
	}
	built, ok := result.(indexformat.V2)
	if !ok {
		t.Fatalf("IndexTranscriptResult returned %T, want indexformat.V2", result)
	}
	return built
}

func openCodeNativeProvenanceConfig(source testfixture.MaterializedSource, sessionID string, prior ingest.OpenCodeProvenancePrior, fork *ingest.OpenCodeForkProof) ingest.OpenCodeProvenanceIndexerConfig {
	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	return ingest.OpenCodeProvenanceIndexerConfig{
		Enabled: true,
		Snapshot: func(ctx context.Context, _ ingest.DiscoveredSession) (ingest.OpenCodeHistorySnapshot, error) {
			return adapter.SnapshotOpenCodeProvenance(ctx, source.Path, sessionID, ingest.OpenCodeSnapshotOptions{Fork: fork})
		},
		Metadata: func(_ ingest.DiscoveredSession) (schema.UnifiedMetadata, error) {
			return schema.UnifiedMetadata{SchemaVersion: 1, SessionID: schema.SessionID(sessionID), ModelHarness: schema.HarnessOpenCode}, nil
		},
		GenerationID: func(_ ingest.DiscoveredSession) string { return "gen-native-" + sessionID },
		Prior: func(context.Context, ingest.DiscoveredSession) (ingest.OpenCodeProvenancePrior, error) {
			return prior, nil
		},
	}
}

func seedOpenCodeProvenanceCase(t *testing.T, source testfixture.MaterializedSource, row ocProvCase) {
	t.Helper()
	_ = testfixture.SnapshotSource(t, source)
	connection, err := sqlite.OpenConn(source.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	parentID := ""
	if row.Scope.HasParent {
		parentID = "ses_opencodeFixtureParent"
	}
	if err := sqlitex.Execute(connection, "INSERT OR REPLACE INTO session (id, parent_id, time_created, time_updated) VALUES (?1, ?2, 1000, 1000)", &sqlitex.ExecOptions{Args: []any{row.Scope.SessionID, parentID}}); err != nil {
		t.Fatalf("seed session row for %q: %v", row.Scope.SessionID, err)
	}
	if err := sqlitex.Execute(connection, "DELETE FROM session_message WHERE session_id = ?1", &sqlitex.ExecOptions{Args: []any{row.Scope.SessionID}}); err != nil {
		t.Fatalf("clear session_message rows for %q: %v", row.Scope.SessionID, err)
	}
	for _, native := range row.Rows {
		if err := sqlitex.Execute(connection, "INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", &sqlitex.ExecOptions{Args: []any{native.ID, row.Scope.SessionID, native.Type, native.TimeCreated, native.TimeUpdated, native.Data, native.Seq}}); err != nil {
			t.Fatalf("seed message row %q: %v", native.ID, err)
		}
	}
}

// assertOpenCodeExistingIdentitiesStable requires every alias and title ref the
// earlier generation proved to keep its exact identity in the later one.
func assertOpenCodeExistingIdentitiesStable(t *testing.T, label string, before, after indexformat.Generation) {
	t.Helper()
	afterAliases := make(map[string]schema.SourceEntryRef, len(after.Aliases))
	for _, alias := range after.Aliases {
		afterAliases[alias.NativeKey] = alias.Ref
	}
	for _, alias := range before.Aliases {
		got, ok := afterAliases[alias.NativeKey]
		if !ok || got != alias.Ref {
			t.Fatalf("%s alias %q = %q (present %t), want %q", label, alias.NativeKey, got, ok, alias.Ref)
		}
	}
	for _, ref := range before.TitleRefs {
		found := false
		for _, candidate := range after.TitleRefs {
			if candidate == ref {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s dropped title ref %q", label, ref)
		}
	}
}

func buildOpenCodeNativeGeneration(t *testing.T, snapshot ingest.OpenCodeHistorySnapshot) indexformat.V2 {
	t.Helper()
	return buildOpenCodeNativeGenerationPrior(t, snapshot, ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()})
}

func buildOpenCodeNativeGenerationPrior(t *testing.T, snapshot ingest.OpenCodeHistorySnapshot, prior ingest.OpenCodeProvenancePrior) indexformat.V2 {
	t.Helper()
	capture, err := ingest.BuildOpenCodeProvenanceCapture(snapshot, "gen-native", schema.UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     schema.SessionID(openCodeNativeChild),
		ModelHarness:  schema.HarnessOpenCode,
	}, prior)
	if err != nil {
		t.Fatalf("BuildOpenCodeProvenanceCapture: %v", err)
	}
	built, err := ingest.BuildV2(capture, ingest.RandomRefAllocator{})
	if err != nil {
		t.Fatalf("BuildV2: %v", err)
	}
	return built
}
