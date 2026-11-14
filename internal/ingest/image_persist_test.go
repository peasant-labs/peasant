package ingest_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/attachments"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestImagePersistFollowsActivation proves the production harvest writes an
// attachment row only after the generation is accepted. The row is keyed by
// the content address, and a second identical harvest does not leave a second
// row for the same bytes.
func TestImagePersistFollowsActivation(t *testing.T) {
	ctx := t.Context()
	fs := testutil.NewMemFS()
	root := filepath.Join(t.TempDir(), "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"), store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	id, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	transcript := []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + base64.StdEncoding.EncodeToString(encoded.Bytes()) + `"}}]}}` + "\n")
	nativePath := "/synthetic/native/" + id.String() + ".jsonl"
	if err := fs.WriteFile(nativePath, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := makeMinimalMeta(t, id.String())
	fresh.ModelHarness = ingest.HarnessClaudeCode
	fresh.Source.FilePath = nativePath
	fresh.Source.Format = ingest.SourceFormatJSONL
	fresh.ContentHash = schema.ComputeTranscriptHash(transcript)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{{
			SessionID: id, Harness: ingest.HarnessClaudeCode,
			SourcePath: ingest.ResolvedPath(nativePath), SourceFormat: ingest.SourceFormatJSONL,
		}}, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh}),
	}
	run := func() *ingest.PipelineResult {
		t.Helper()
		pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, makePipelineConfig(testOutputDir),
			ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database), ingest.WithLogger(database),
			ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
			ingest.WithHarvesterVersions(ingest.GenerationTargets(ingest.HarvesterVersionRegistry)))
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(ctx)
		if err != nil {
			t.Fatalf("harvest: %v", err)
		}
		return result
	}
	first := run()
	if first.Summary.Errors != 0 || first.Summary.Indexed != 1 {
		t.Fatalf("image harvest did not activate: %+v diagnostics=%+v", first.Summary, first.Diagnostics)
	}
	facts, err := database.ImageFacts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("attachment rows = %d, want the one accepted image", len(facts))
	}
	var fact ingest.ImageFacts
	for _, item := range facts {
		fact = item
	}
	if fact.SHA256 == "" || fact.MediaType != "image/png" || fact.PartitionID == "" {
		t.Fatalf("accepted image = %+v, want a stored png in its generation partition", fact)
	}
}

// TestImageHarvestCollapsesIdenticalBytes proves two identical images, including
// the same bytes in main and earlier history, install as one row after the
// generation commits. Both entries join that row. A different image at the
// earlier index keeps its own row.
func TestImageHarvestCollapsesIdenticalBytes(t *testing.T) {
	ctx := t.Context()
	fs := testutil.NewMemFS()
	root := filepath.Join(t.TempDir(), "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"), store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	id, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	pngA := tinyPNG(t, 1, 1)
	encoded := base64.StdEncoding.EncodeToString(pngA)
	transcript := []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + encoded + `"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + encoded + `"}}]}}` + "\n" +
		`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"text","text":"again"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + encoded + `"}}]}}` + "\n")
	nativePath := "/synthetic/native/" + id.String() + ".jsonl"
	if err := fs.WriteFile(nativePath, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := makeMinimalMeta(t, id.String())
	fresh.ModelHarness = ingest.HarnessClaudeCode
	fresh.Source.FilePath = nativePath
	fresh.Source.Format = ingest.SourceFormatJSONL
	fresh.ContentHash = schema.ComputeTranscriptHash(transcript)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{{
			SessionID: id, Harness: ingest.HarnessClaudeCode,
			SourcePath: ingest.ResolvedPath(nativePath), SourceFormat: ingest.SourceFormatJSONL,
		}}, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh}),
	}
	pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, makePipelineConfig(testOutputDir),
		ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database), ingest.WithLogger(database),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithHarvesterVersions(ingest.GenerationTargets(ingest.HarvesterVersionRegistry)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if result.Summary.Errors != 0 || result.Summary.Indexed != 1 {
		t.Fatalf("identical-image harvest did not activate: %+v diagnostics=%+v", result.Summary, result.Diagnostics)
	}
	facts, err := database.ImageFacts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("attachment rows = %d, want one row for the repeated png", len(facts))
	}
	var shared ingest.ImageFacts
	for _, item := range facts {
		shared = item
	}
	if err := database.WithSessionSnapshot(ctx, schema.SessionID(id), func(snapshot indexformat.ReadSnapshot) error {
		joined := joinedImageParts(t, snapshot.Main.Entries, facts)
		if len(joined) != 3 {
			t.Fatalf("joined image parts = %d, want the two blocks and the second message", len(joined))
		}
		for _, part := range joined {
			if part.Ref != shared.SHA256 || part.MediaType != schema.ImageMediaTypePNG || part.OmittedReason != "" {
				t.Fatalf("image part = %+v, want both entries to join %s", part, shared.SHA256)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	historyID := schema.SessionID("11111111-1111-4111-8111-111111111111")
	storetest.SeedSession(t, database, string(historyID))
	pngB := tinyPNG(t, 2, 1)
	installSharedHistoryImages(t, database, historyID, pngA, pngB)
	historyFacts, err := database.ImageFacts(ctx, ingest.SessionID(historyID))
	if err != nil {
		t.Fatal(err)
	}
	if len(historyFacts) != 2 {
		t.Fatalf("history attachment rows = %d, want the shared digest and the different earlier image", len(historyFacts))
	}
	sharedRef := "img-" + contentDigest(pngA)
	otherRef := "img-" + contentDigest(pngB)
	sharedFact, ok := historyFacts[sharedRef]
	if !ok || sharedFact.SHA256 != contentDigest(pngA) {
		t.Fatalf("shared row = %+v present=%v, want one row for the repeated digest", sharedFact, ok)
	}
	otherFact, ok := historyFacts[otherRef]
	if !ok || otherFact.SHA256 != contentDigest(pngB) || otherFact.SHA256 == sharedFact.SHA256 {
		t.Fatalf("different earlier image = %+v present=%v, want its own row", otherFact, ok)
	}
	if err := database.WithSessionSnapshot(ctx, historyID, func(snapshot indexformat.ReadSnapshot) error {
		if len(snapshot.Earlier) != 1 {
			t.Fatalf("earlier sections = %d, want 1", len(snapshot.Earlier))
		}
		mainJoined := joinedImageParts(t, snapshot.Main.Entries, historyFacts)
		earlierJoined := joinedImageParts(t, snapshot.Earlier[0].Content.Entries, historyFacts)
		if len(mainJoined) != 1 || len(earlierJoined) != 2 {
			t.Fatalf("joined main=%d earlier=%d, want 1 and 2", len(mainJoined), len(earlierJoined))
		}
		if mainJoined[0].Ref != sharedFact.SHA256 || earlierJoined[1].Ref != sharedFact.SHA256 {
			t.Fatalf("shared bytes did not join from both partitions: main=%+v earlier=%+v", mainJoined[0], earlierJoined[1])
		}
		if earlierJoined[0].Ref != otherFact.SHA256 || earlierJoined[0].MediaType != schema.ImageMediaTypePNG {
			t.Fatalf("different image at the earlier index = %+v, want its own joined row", earlierJoined[0])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func tinyPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func contentDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func joinedImageParts(t *testing.T, entries []schema.SessionEntry, facts map[string]ingest.ImageFacts) []schema.ImagePart {
	t.Helper()
	var joined []schema.ImagePart
	for _, turn := range ingest.EntriesToTurnsWithImages(entries, facts) {
		for _, part := range turn.Parts {
			if part.Kind != ingest.TurnPartImage || part.Image == nil {
				continue
			}
			joined = append(joined, *part.Image)
		}
	}
	return joined
}

func installSharedHistoryImages(t *testing.T, database *store.Store, sid schema.SessionID, shared, other []byte) {
	t.Helper()
	sharedRef := "img-" + contentDigest(shared)
	otherRef := "img-" + contentDigest(other)
	parent := 0
	imageEntry := func(index int, sourceRef, extraRef string) schema.SessionEntry {
		extra := `{"ref":"` + extraRef + `"}`
		part := "image"
		return schema.SessionEntry{
			SessionID: sid, EntryIndex: index, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeImage, Role: schema.RoleUser, Depth: 1, ParentIndex: &parent,
			PartType: &part, SourceEntryRef: schema.SourceEntryRef(sourceRef), Extra: &extra,
		}
	}
	textEntry := func(index int, sourceRef, text string) schema.SessionEntry {
		preview := text
		return schema.SessionEntry{
			SessionID: sid, EntryIndex: index, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &preview,
			SourceEntryRef: schema.SourceEntryRef(sourceRef),
		}
	}
	main := []schema.SessionEntry{
		textEntry(0, "e_main_text", "look"),
		imageEntry(1, "e_main_image", sharedRef),
	}
	earlier := []schema.SessionEntry{
		textEntry(0, "e_earlier_text", "old"),
		imageEntry(1, "e_earlier_other", otherRef),
		imageEntry(2, "e_earlier_shared", sharedRef),
	}
	entries := append(append([]schema.SessionEntry{}, main...), earlier...)
	content := make([]indexformat.ContentRecord, 0, len(entries))
	blobs := map[schema.SourceEntryRef][]byte{}
	for _, entry := range entries {
		body := []byte("image")
		if entry.ContentPreview != nil {
			body = []byte(*entry.ContentPreview)
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		content = append(content, indexformat.ContentRecord{
			Ref: entry.SourceEntryRef, RelativeBlob: "c_" + digest + ".blob", ByteLength: int64(len(body)), Digest: digest,
		})
		blobs[entry.SourceEntryRef] = body
	}
	inputCount := int64(1)
	generation := indexformat.V2{Generation: indexformat.Generation{
		ID:           "g-image-history",
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  schema.HarnessClaudeCode,
			Stats:         schema.SessionStats{TurnCount: 1, InputSubmissionCount: &inputCount},
		},
		Main:                 indexformat.Partition{Entries: main},
		Earlier:              []indexformat.EarlierPartition{{State: schema.EarlierHistoryUncertainMigrated, Content: indexformat.Partition{Entries: earlier}}},
		Content:              content,
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{"e_main_text"},
	}}
	if _, err := database.ActivateGeneration(t.Context(), store.GenerationActivation{
		Generation: generation, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 100,
	}); err != nil {
		t.Fatalf("activate history generation: %v", err)
	}
	pending := []attachments.Pending{
		{Row: attachments.Row{SourceEntryRef: sharedRef, PartitionID: "main", MediaType: "image/png", Width: 1, Height: 1}, Bytes: shared},
		{Row: attachments.Row{SourceEntryRef: sharedRef, PartitionID: string(schema.EarlierHistoryUncertainMigrated), MediaType: "image/png", Width: 1, Height: 1}, Bytes: shared},
		{Row: attachments.Row{SourceEntryRef: otherRef, PartitionID: string(schema.EarlierHistoryUncertainMigrated), MediaType: "image/png", Width: 1, Height: 1}, Bytes: other},
	}
	if err := database.ReplaceSessionImages(t.Context(), string(sid), pending); err != nil {
		t.Fatalf("install history images: %v", err)
	}
}
