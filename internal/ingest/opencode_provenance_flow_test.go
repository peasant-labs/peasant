package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

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
}

type ocFlowDocument struct {
	Cases []ocFlowCase `yaml:"cases"`
}

func loadOpenCodeProvenanceFlowDocument(t *testing.T) ocFlowDocument {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeProvenanceFlowYAML))
	decoder.KnownFields(true)
	var document ocFlowDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode OpenCode provenance flow fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("OpenCode provenance flow fixture must contain exactly one YAML document: %v", err)
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
	case "missing-source":
		if row.Sentinel == "" || row.ExpectStep == "" {
			return errors.New("the missing-source kind requires a sentinel and the expected refusal step")
		}
	case "decode-refusal":
		if row.Sentinel == "" || len(row.DecodeRows) == 0 {
			return errors.New("the decode-refusal kind requires a sentinel and native decode rows")
		}
	case "large-row":
		if row.LargeRowPrefix == "" || row.LargeRowPadding <= 0 {
			return errors.New("the large-row kind requires a prefix and a positive padding size")
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
			source := testfixture.MaterializeByName(t, openCodeFlowSourceFixture)
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
			case "large-row":
				runOpenCodeFlowLargeRow(t, source, row)
			case "decode-refusal":
				runOpenCodeFlowDecodeRefusal(t, row)
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

func runOpenCodeFlowBoundedFork(t *testing.T, source testfixture.MaterializedSource, row ocFlowCase) {
	t.Helper()
	seedOpenCodeFlowForkParent(t, source, row)
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
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
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
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
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	missing := filepath.Join(filepath.Dir(source.Path), row.Sentinel+".db")
	_, err := adapter.SnapshotOpenCodeProvenance(context.Background(), missing, row.SessionID, ingest.OpenCodeSnapshotOptions{})
	if err == nil {
		t.Fatal("a missing native database produced a snapshot instead of a refusal")
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
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
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
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
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
