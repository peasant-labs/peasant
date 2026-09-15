package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/published_provenance_envelope.yaml
var publishedProvenanceEnvelopeYAML []byte

//go:embed testdata/published_provenance_envelope.manifest.yaml
var publishedProvenanceEnvelopeManifestYAML []byte

// Closed sets for the fixture. A new axis value must be added here and handled
// in the test, so an unknown value fails loudly instead of being skipped.
var (
	ppePaths          = []string{"snapshot", "legacy"}
	ppeStats          = []string{"absent", "zero", "positive"}
	ppeRelationships  = []string{"none", "known", "known-retained", "unknown"}
	ppeRootIdentities = []string{"present", "absent"}
	ppePurposes       = []string{"interaction", "absent"}
	ppeEarlier        = []string{"none", "one", "two"}
	ppeVisibilities   = []string{"default", "label-sent", "path-hidden", "git-hidden"}
	ppeAdvertisements = []string{"graph", "plain"}
	ppeRedactions     = []string{"none", "tool-body", "pi-metadata"}
	ppeEntries        = []string{"plain", "provenance", "tool-block"}
	ppeHarnesses      = []string{"claude-code", "pi"}
)

const (
	ppeTargetID   = "11111111-1111-4111-8111-111111111111"
	ppePreviousID = "22222222-2222-4222-8222-222222222222"
	ppeRemote     = "git@github.com:acme/repo.git"
	ppeSecret     = "sk-ant-api03-PROVENANCEENVELOPE0000000000x"
	ppeObserved   = "pi-observed-model-1"
	// The recorded Pi assistant usage deliberately differs from the capture
	// totals ppeBuild records (tokensIn 10, tokensOut 5). The published
	// envelope must carry the usage-derived mirrors, so a publication that
	// copied the capture totals instead fails the token assertions.
	ppePiUsageTokensIn  = 7
	ppePiUsageTokensOut = 3
)

var ppeSessionID = schema.SessionID(testutil.TestSessionUUID)

type publishedProvenanceEnvelopeFixture struct {
	Cases []publishedProvenanceEnvelopeCase `yaml:"cases"`
}

type publishedProvenanceEnvelopeCase struct {
	Name          string `yaml:"name"`
	Path          string `yaml:"path"`
	Stats         string `yaml:"stats"`
	Relationships string `yaml:"relationships"`
	RootIdentity  string `yaml:"rootIdentity"`
	Purpose       string `yaml:"purpose"`
	Earlier       string `yaml:"earlier"`
	Visibility    string `yaml:"visibility"`
	Advertisement string `yaml:"advertisement"`
	Redaction     string `yaml:"redaction"`
	Entries       string `yaml:"entries"`
	Harness       string `yaml:"harness"`
	BlobBytes     int    `yaml:"blobBytes"`
	// GenerationTurnCount overrides the generation's durable turn mirror when
	// set, so a case can prove the published turnCount is the captured mirror
	// rather than len(Turns).
	GenerationTurnCount int `yaml:"generationTurnCount"`

	Expect publishedProvenanceEnvelopeExpect `yaml:"expect"`
}

type publishedProvenanceEnvelopeExpect struct {
	Uploads                 *int                       `yaml:"uploads"`
	Errors                  *bool                      `yaml:"errors"`
	SavedPublications       *int                       `yaml:"savedPublications"`
	Attempts                *int                       `yaml:"attempts"`
	RequiredCapabilities    []schema.ContentCapability `yaml:"requiredCapabilities"`
	Mirrors                 bool                       `yaml:"mirrors"`
	ContentHashMatchesBytes bool                       `yaml:"contentHashMatchesBytes"`
	FingerprintMatches      bool                       `yaml:"fingerprintMatches"`
	SecretAbsent            bool                       `yaml:"secretAbsent"`
	ObservedModel           string                     `yaml:"observedModel"`
	Detail                  *ppeDetailExpect           `yaml:"detail"`
}

type ppeDetailExpect struct {
	TurnCount                     *int                         `yaml:"turnCount"`
	InputSubmissionCount          *int64                       `yaml:"inputSubmissionCount"`
	InputSubmissionCountAbsent    bool                         `yaml:"inputSubmissionCountAbsent"`
	RootSessionID                 *string                      `yaml:"rootSessionId"`
	RootSessionIDAbsent           bool                         `yaml:"rootSessionIdAbsent"`
	Purpose                       *string                      `yaml:"purpose"`
	Relationships                 []schema.SessionRelationship `yaml:"relationships"`
	EarlierHistory                []ppeEarlierExpect           `yaml:"earlierHistory"`
	Project                       *string                      `yaml:"project"`
	WorkingDirectory              *string                      `yaml:"workingDirectory"`
	GitBranch                     *string                      `yaml:"gitBranch"`
	GitRemote                     *string                      `yaml:"gitRemote"`
	ToolArgumentEqualsManagedBlob bool                         `yaml:"toolArgumentEqualsManagedBlob"`
	ToolArgumentBytes             int                          `yaml:"toolArgumentBytes"`
	CallProvenanceRef             string                       `yaml:"callProvenanceRef"`
	ResultProvenanceRef           string                       `yaml:"resultProvenanceRef"`
	// DurationMins and the token members pin the session-level totals the
	// consent overlay copies from the capture metadata. Duration has no
	// usage-derived equivalent, so it must reach every harness; the token
	// mirrors are usage-derived for Pi and must not be overwritten there.
	DurationMins *float64 `yaml:"durationMins"`
	TokensIn     *int     `yaml:"tokensIn"`
	TokensOut    *int     `yaml:"tokensOut"`
	TotalTokens  *int     `yaml:"totalTokens"`
}

type ppeEarlierExpect struct {
	State             schema.EarlierHistoryState `yaml:"state"`
	TurnIndexes       []int                      `yaml:"turnIndexes"`
	NativeMetadataIDs []string                   `yaml:"nativeMetadataIds"`
}

func loadPublishedProvenanceEnvelopeFixture(t *testing.T) publishedProvenanceEnvelopeFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(publishedProvenanceEnvelopeYAML))
	decoder.KnownFields(true)
	var fixture publishedProvenanceEnvelopeFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode published provenance envelope fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("published provenance envelope fixture must contain exactly one document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(publishedProvenanceEnvelopeManifestYAML, "published provenance envelope")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index := range fixture.Cases {
		fixtureCase := &fixture.Cases[index]
		names[index] = fixtureCase.Name
		if fixtureCase.Name == "" {
			t.Fatalf("published provenance envelope case %d has no name", index)
		}
		if fixtureCase.Harness == "" {
			fixtureCase.Harness = "claude-code"
		}
		for label, pair := range map[string]struct {
			value  string
			closed []string
		}{
			"path":          {fixtureCase.Path, ppePaths},
			"stats":         {fixtureCase.Stats, ppeStats},
			"relationships": {fixtureCase.Relationships, ppeRelationships},
			"rootIdentity":  {fixtureCase.RootIdentity, ppeRootIdentities},
			"purpose":       {fixtureCase.Purpose, ppePurposes},
			"earlier":       {fixtureCase.Earlier, ppeEarlier},
			"visibility":    {fixtureCase.Visibility, ppeVisibilities},
			"advertisement": {fixtureCase.Advertisement, ppeAdvertisements},
			"redaction":     {fixtureCase.Redaction, ppeRedactions},
			"entries":       {fixtureCase.Entries, ppeEntries},
			"harness":       {fixtureCase.Harness, ppeHarnesses},
		} {
			if !containsString(pair.closed, pair.value) {
				t.Fatalf("case %q has unknown %s %q", fixtureCase.Name, label, pair.value)
			}
		}
		if fixtureCase.BlobBytes < 0 {
			t.Fatalf("case %q declares a negative blob size", fixtureCase.Name)
		}
		if fixtureCase.Redaction == "pi-metadata" && fixtureCase.Harness != "pi" {
			t.Fatalf("case %q pins Pi metadata redaction without the pi harness", fixtureCase.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "published provenance envelope"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestPublishedProvenanceEnvelopeCarriage drives the real publish service over a
// real generation-capable store for every fixture case and asserts the emitted
// envelope and metadata mirrors member-for-member.
func TestPublishedProvenanceEnvelopeCarriage(t *testing.T) {
	for _, fixtureCase := range loadPublishedProvenanceEnvelopeFixture(t).Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			db, built := ppeSeedCase(t, fixtureCase)
			publisher := ppePublisher(fixtureCase)
			pipeline, err := push.NewPipeline(db, publisher, baseCreds(), ppeConfig(fixtureCase.Visibility), nil,
				push.PipelineConfig{Force: true, Concurrency: 1, FilterSessionIDs: []string{string(ppeSessionID)}}, ppeRedactor(t, fixtureCase), &bytes.Buffer{})
			if err != nil {
				t.Fatalf("NewPipeline: %v", err)
			}
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			ppeAssertCase(t, fixtureCase, built, publisher, result, db)
		})
	}
}

// ppeSeedCase builds one case's snapshot from its axes and seeds it into a real
// generation-capable store, returning the store and the built input.
func ppeSeedCase(t *testing.T, c publishedProvenanceEnvelopeCase) (*store.Store, ppeBuilt) {
	t.Helper()
	db := ppeOpenStore(t)
	built := ppeBuild(t, c)
	if c.Path == "legacy" {
		testutil.SeedReadyPublication(t, db, built.meta, built.legacyEntries)
		return db, built
	}
	if c.Relationships == "known" || c.Relationships == "known-retained" {
		storetest.SeedSession(t, db, ppeTargetID)
		storetest.SeedSession(t, db, ppePreviousID)
	}
	storetest.SeedGenerationPublication(t, db, built.meta, built.generation, built.blobs)
	return db, built
}

func ppeOpenStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(
		filepath.Join(dir, "generations.db"),
		store.WithPoolSize(2),
		store.WithIndexFormats(store.V2IndexFormat()),
		store.WithGenerationArtifacts(artifacts, locker),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type ppeBuilt struct {
	meta            *schema.UnifiedMetadata
	generation      indexformat.V2
	blobs           map[schema.SourceEntryRef][]byte
	legacyEntries   []schema.SessionEntry
	harness         schema.Harness
	managedCallBody string
}

func ppeBuild(t *testing.T, c publishedProvenanceEnvelopeCase) ppeBuilt {
	t.Helper()
	harness := defaults.HarnessClaudeCode
	if c.Harness == "pi" || c.Redaction == "pi-metadata" {
		harness = schema.HarnessPi
	}
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = ppeSessionID
	meta.HostSlug = testutil.TestHostSlug
	meta.ModelHarness = harness
	meta.Model = testutil.TestModel
	meta.CWD = "/home/test/myapp"
	ingested := int64(1740312400000)
	meta.Timestamp = ingest.TimestampInfo{Start: 1740312000000, End: 1740312360000, Ingested: &ingested}
	meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "myapp", FilePath: "/home/test/myapp"}
	meta.Source = ingest.SourceInfo{FilePath: "/source/file.jsonl", Format: ingest.SourceFormatJSONL}
	meta.Stats = ingest.StatsInfo{TurnCount: 1, DurationMs: 60000, TokensIn: 10, TokensOut: 5}

	remote := ""
	if c.Visibility == "label-sent" || c.Visibility == "git-hidden" {
		remote = ppeRemote
	}
	branch := "feature/x"
	if remote != "" {
		meta.Git.Remote = &remote
		meta.Git.Branch = &branch
	}

	var inputCount *int64
	switch c.Stats {
	case "zero":
		zero := int64(0)
		inputCount = &zero
	case "positive":
		positive := int64(3)
		inputCount = &positive
	}
	meta.Stats.InputSubmissionCount = inputCount

	target := schema.SessionID(ppeTargetID)
	previous := schema.SessionID(ppePreviousID)
	if c.RootIdentity == "present" {
		meta.RootSessionID = &target
	}
	if c.Purpose == "interaction" {
		meta.Purpose = schema.SessionPurposeInteraction
	}
	meta.Relationships = ppeRelationshipsFor(c.Relationships, target, previous)
	if c.Relationships == "known" || c.Relationships == "known-retained" {
		meta.ParentUUID = &target
	}

	built := ppeBuilt{meta: &meta, harness: harness}
	ppeBuildPartitions(t, &built, c, inputCount)
	built.legacyEntries = built.legacyMainEntries()
	return built
}

func ppeRelationshipsFor(axis string, target, previous schema.SessionID) []schema.SessionRelationship {
	anchor := &schema.PublicSourceAnchor{
		Kind:              schema.PublicSourceAnchorBefore,
		SourceEntryRef:    schema.SourceEntryRef("e_ctx"),
		SourceRevisionRef: schema.PublicRevisionRef("rev_1"),
	}
	switch axis {
	case "known":
		return []schema.SessionRelationship{
			{Kind: schema.SessionRelationshipStartedBy, TargetState: schema.RelationshipTargetKnown, TargetLocalID: &target, Evidence: schema.EvidenceNativeTyped},
			{Kind: schema.SessionRelationshipContextFrom, TargetState: schema.RelationshipTargetKnown, TargetLocalID: &previous, Evidence: schema.EvidenceNativeTyped, Anchor: anchor},
		}
	case "known-retained":
		return []schema.SessionRelationship{
			{Kind: schema.SessionRelationshipStartedBy, TargetState: schema.RelationshipTargetKnownRetained, TargetLocalID: &target, Evidence: schema.EvidenceNativeTyped},
			{Kind: schema.SessionRelationshipContextFrom, TargetState: schema.RelationshipTargetKnownRetained, TargetLocalID: &previous, Evidence: schema.EvidenceNativeTyped, Anchor: anchor},
		}
	case "unknown":
		return []schema.SessionRelationship{
			{Kind: schema.SessionRelationshipContextFrom, TargetState: schema.RelationshipTargetUnknown, Evidence: schema.EvidenceNativeTyped},
		}
	default:
		return nil
	}
}

func ppeProvenance(submission string) *schema.ContentProvenance {
	return &schema.ContentProvenance{
		Origin:        schema.ContentOriginSubmittedInput,
		Actor:         schema.ActorOriginUnknown,
		Delivery:      schema.DeliveryOriginSessionAdmission,
		Ownership:     schema.ContentOwnershipLocal,
		Evidence:      schema.EvidenceNativeTyped,
		InputModality: schema.InputModalityText,
		SubmissionRef: schema.SubmissionRef(submission),
	}
}

// ppeBuildPartitions builds the main partition, managed blobs and (for the
// earlier axis) the retained earlier partitions.
func ppeBuildPartitions(t *testing.T, built *ppeBuilt, c publishedProvenanceEnvelopeCase, inputCount *int64) {
	t.Helper()
	meta := built.meta
	userText := "recorded input"
	user := schema.SessionEntry{
		SessionID: meta.SessionID, EntryIndex: 0, Harness: built.harness,
		EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &userText,
	}
	content := []indexformat.ContentRecord{}
	blobs := map[schema.SourceEntryRef][]byte{}

	switch c.Entries {
	case "provenance":
		user.SourceEntryRef = schema.SourceEntryRef("e_u1")
		user.Provenance = ppeProvenance("s_u1")
		content = append(content, indexformat.ContentRecord{Ref: user.SourceEntryRef})
		blobs[user.SourceEntryRef] = []byte(userText)
	case "tool-block":
		callID := "call-1"
		carrier := 1
		preview := "preview only"
		assistantText := "assistant reply"
		user.SourceEntryRef = schema.SourceEntryRef("e_u1")
		user.Provenance = ppeProvenance("s_u1")
		content = append(content, indexformat.ContentRecord{Ref: user.SourceEntryRef})
		blobs[user.SourceEntryRef] = []byte(userText)

		longBody := strings.Repeat("L", c.BlobBytes)
		assistant := schema.SessionEntry{
			SessionID: meta.SessionID, EntryIndex: 1, Harness: built.harness,
			EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, ContentPreview: &assistantText,
		}
		call := schema.SessionEntry{
			SessionID: meta.SessionID, EntryIndex: 2, Harness: built.harness,
			EntryType: schema.EntryTypeToolUse, Role: schema.RoleAssistant, Depth: 1, ParentIndex: &carrier,
			ToolInput: &preview, ToolCallID: &callID,
			SourceEntryRef: schema.SourceEntryRef("e_call1"), Provenance: ppeProvenance("s_call1"),
		}
		result := schema.SessionEntry{
			SessionID: meta.SessionID, EntryIndex: 3, Harness: built.harness,
			EntryType: schema.EntryTypeToolResult, Role: schema.RoleTool, Depth: 1, ParentIndex: &carrier,
			ToolOutput: &preview, ToolCallID: &callID,
			SourceEntryRef: schema.SourceEntryRef("e_result1"), Provenance: ppeProvenance("s_result1"),
		}
		content = append(content,
			indexformat.ContentRecord{Ref: call.SourceEntryRef},
			indexformat.ContentRecord{Ref: result.SourceEntryRef},
		)
		blobs[call.SourceEntryRef] = []byte(longBody)
		blobs[result.SourceEntryRef] = []byte("result body")
		built.managedCallBody = longBody
		mainEntries := []schema.SessionEntry{user, assistant, call, result}
		built.generation = indexformat.V2{Generation: ppeGeneration(c, meta, mainEntries, content, inputCount, nil)}
		built.blobs = blobs
		return
	default:
		// plain: no ref, no provenance.
	}

	if built.harness == schema.HarnessPi {
		user.Extra = ppePiEntryExtra(t, ingest.PiExtraState, "", "main-user")
	}
	mainEntries := []schema.SessionEntry{user}
	if c.Redaction == "pi-metadata" {
		user.SourceEntryRef = schema.SourceEntryRef("e_u1")
		mainEntries[0] = user
		content = append(content, indexformat.ContentRecord{Ref: user.SourceEntryRef})
		blobs[user.SourceEntryRef] = []byte(userText)
		assistantSource := ingest.PiPublicRef(string(ppeSessionID), "usage", "main-assistant")
		usageIn, usageOut := int64(ppePiUsageTokensIn), int64(ppePiUsageTokensOut)
		assistantExtra, extraErr := ingest.EncodePiExtra(ingest.PiExtra{
			Kind: ingest.PiExtraUsage, Harness: schema.HarnessPi, SourceRef: assistantSource,
			ModelID: schema.ObservedModelID(ppeObserved),
			Usage: &schema.UsageDetail{
				OwnerID:        schema.UsageOwnerID(ingest.PiPublicRef(string(ppeSessionID), "owner", "main-assistant")),
				SourceEntryRef: schema.SourceEntryRef(assistantSource),
				Scope:          schema.UsageScopeAssistant,
				Completeness:   schema.UsagePartial,
				Tokens:         &schema.TokenUsageDetail{Input: &usageIn, Output: &usageOut},
			},
		})
		if extraErr != nil {
			t.Fatalf("encode Pi assistant evidence: %v", extraErr)
		}
		assistant := schema.SessionEntry{
			SessionID: meta.SessionID, EntryIndex: 1, Harness: built.harness,
			EntryType: schema.EntryTypeText, Role: schema.RoleAssistant,
			SourceEntryRef: schema.SourceEntryRef(assistantSource), Extra: assistantExtra,
		}
		content = append(content, indexformat.ContentRecord{Ref: assistant.SourceEntryRef})
		blobs[assistant.SourceEntryRef] = []byte("assistant reply")
		mainEntries = append(mainEntries, assistant)
	}

	var earlier []indexformat.EarlierPartition
	appendEarlier := func(state schema.EarlierHistoryState, turns int, nativeID string) {
		partition, partitionBlobs := ppeEarlierPartition(t, state, turns, nativeID, built.harness, meta.SessionID)
		earlier = append(earlier, partition)
		for ref, body := range partitionBlobs {
			blobs[ref] = body
			content = append(content, indexformat.ContentRecord{Ref: ref})
		}
	}
	switch c.Earlier {
	case "one":
		appendEarlier(schema.EarlierHistoryUncertainMigrated, 1, "n_earlier_1")
	case "two":
		appendEarlier(schema.EarlierHistoryUncertainMigrated, 1, "n_earlier_1")
		appendEarlier(schema.EarlierHistoryUncertainUnresolved, 2, "n_earlier_2")
	}
	built.blobs = blobs
	built.generation = indexformat.V2{Generation: ppeGeneration(c, meta, mainEntries, content, inputCount, earlier)}
}

func ppeGeneration(c publishedProvenanceEnvelopeCase, meta *schema.UnifiedMetadata, entries []schema.SessionEntry, content []indexformat.ContentRecord, inputCount *int64, earlier []indexformat.EarlierPartition) indexformat.Generation {
	turnCount := len(entries)
	if c.GenerationTurnCount > 0 {
		turnCount = c.GenerationTurnCount
	}
	generationMetadata := *meta
	generationMetadata.Stats.TurnCount = turnCount
	generationMetadata.Stats.InputSubmissionCount = inputCount
	generation := indexformat.Generation{
		ID:                   "g-" + c.Name,
		Completeness:         indexformat.GenerationCompletenessComplete,
		Metadata:             generationMetadata,
		Main:                 indexformat.Partition{Entries: entries},
		Earlier:              earlier,
		Content:              content,
		SourceEvidenceDigest: strings.Repeat("a", 64),
	}
	if c.Redaction == "pi-metadata" {
		secretBody, _ := json.Marshal(map[string]string{"token": ppeSecret})
		generation.Main.NativeMetadata = []schema.NativeMetadataRecord{{
			ID:         "n_main",
			Kind:       schema.NativeMetadataPiCustomData,
			Source:     schema.NativeSourceRef{EntryRef: "e_u1", SourceType: schema.NativeSourcePiCustom},
			CustomType: "pi.test",
			Data:       secretBody,
		}}
	}
	return generation
}

// ppePiEntryExtra builds the typed Pi evidence every Pi indexed row must carry.
// A conversational row must also name its source reference, so the projection
// can attribute the row to its native message without publishing the raw id.
func ppePiEntryExtra(t *testing.T, kind ingest.PiExtraKind, modelID schema.ObservedModelID, nativeID string) *string {
	t.Helper()
	sourceRef := ingest.PiPublicRef(string(ppeSessionID), string(kind), nativeID)
	extra, err := ingest.EncodePiExtra(ingest.PiExtra{Kind: kind, Harness: schema.HarnessPi, SourceRef: sourceRef, ModelID: modelID})
	if err != nil {
		t.Fatalf("encode Pi evidence: %v", err)
	}
	return extra
}

func ppeEarlierPartition(t *testing.T, state schema.EarlierHistoryState, turnCount int, nativeID string, harness schema.Harness, sid schema.SessionID) (indexformat.EarlierPartition, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	turns := make([]schema.SessionEntry, 0, turnCount)
	blobs := map[schema.SourceEntryRef][]byte{}
	var firstRef schema.SourceEntryRef
	for index := 0; index < turnCount; index++ {
		text := "retained turn"
		ref := schema.SourceEntryRef(nativeID + "_turn_" + string(rune('0'+index)))
		if index == 0 {
			firstRef = ref
		}
		entry := schema.SessionEntry{
			SessionID: sid, EntryIndex: index, Harness: harness,
			EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text, SourceEntryRef: ref,
		}
		if harness == schema.HarnessPi {
			entry.Extra = ppePiEntryExtra(t, ingest.PiExtraState, "", nativeID+"-"+strconv.Itoa(index))
		}
		turns = append(turns, entry)
		blobs[ref] = []byte(text)
	}
	nativeData, _ := json.Marshal(map[string]string{"note": nativeID})
	partition := indexformat.EarlierPartition{
		State: state,
		Content: indexformat.Partition{
			Entries: turns,
			NativeMetadata: []schema.NativeMetadataRecord{{
				ID: nativeID, Kind: schema.NativeMetadataPiCustomData,
				Source:     schema.NativeSourceRef{EntryRef: firstRef, SourceType: schema.NativeSourcePiCustom},
				CustomType: "pi.test",
				Data:       nativeData,
			}},
		},
	}
	return partition, blobs
}

// legacyMainEntries is the V1 entry list the preserved legacy builder folds.
func (b ppeBuilt) legacyMainEntries() []schema.SessionEntry {
	userText := "recorded input"
	user := schema.SessionEntry{
		SessionID: b.meta.SessionID, EntryIndex: 0, Harness: b.harness,
		EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &userText,
		SourceEntryRef: schema.SourceEntryRef("e_u1"), Provenance: ppeProvenance("s_u1"),
	}
	return []schema.SessionEntry{user}
}

func ppeConfig(visibility string) *config.Config {
	cfg := baseTestConfig()
	switch visibility {
	case "path-hidden":
		hidden := false
		cfg.Push.Fields.ProjectPath = &hidden
	case "git-hidden":
		hidden := false
		cfg.Push.Fields.GitRemote = &hidden
	case "default", "label-sent":
		// Defaults: git remote, project path and project name are all sent.
	}
	return cfg
}

func ppeRedactor(t *testing.T, c publishedProvenanceEnvelopeCase) ingest.TextRedactor {
	t.Helper()
	// A case that pins redaction content uses the real engine at the offered
	// level; every other case uses the no-op double so the asserted identity
	// fields are the raw consent-gated values rather than redacted forms.
	if c.Redaction != "pi-metadata" {
		return &testutil.NoopRedactor{}
	}
	engine, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("create redactor: %v", err)
	}
	return engine
}

func ppePublisher(c publishedProvenanceEnvelopeCase) *testutil.StubPublisher {
	capabilities := []schema.ContentCapability(nil)
	if c.Advertisement == "graph" {
		capabilities = append(capabilities, schema.AllContentCapabilities...)
	}
	return &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{
		MinPushContractVersion: schema.PushContractVersion("0.0.1"),
		PushContractVersion:    defaults.PublishSchemaVersion,
		ContentCapabilities:    capabilities,
	}}
}

func ppeAssertCase(t *testing.T, c publishedProvenanceEnvelopeCase, built ppeBuilt, publisher *testutil.StubPublisher, result *push.PushResult, db *store.Store) {
	t.Helper()
	expect := c.Expect
	if expect.Uploads != nil && len(publisher.Calls) != *expect.Uploads {
		t.Fatalf("uploads=%d, want %d; result=%+v", len(publisher.Calls), *expect.Uploads, result)
	}
	if expect.Errors != nil && (result.Errors > 0) != *expect.Errors {
		t.Fatalf("errors=%t, want %t; result=%+v", result.Errors > 0, *expect.Errors, result)
	}
	if len(expect.RequiredCapabilities) != 0 || expect.Uploads != nil {
		requireCapabilities(t, result.Sessions, expect.RequiredCapabilities)
	}
	if expect.Attempts != nil {
		projectHash, err := schema.NewProjectHash(string(built.meta.Project.Hash))
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := db.LatestPublicationAttempt(context.Background(), baseCreds().VillageURL, baseCreds().UserID, projectHash, string(ppeSessionID))
		if err != nil {
			t.Fatalf("read latest publication attempt: %v", err)
		}
		if (attempt != nil) != (*expect.Attempts > 0) {
			t.Fatalf("publication attempt present=%t, want %d attempts", attempt != nil, *expect.Attempts)
		}
	}
	projectHash, err := schema.NewProjectHash(string(built.meta.Project.Hash))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := db.Publication(context.Background(), baseCreds().VillageURL, baseCreds().UserID, projectHash, string(ppeSessionID))
	if err != nil {
		t.Fatalf("read publication: %v", err)
	}
	if expect.SavedPublications != nil && (receipt != nil) != (*expect.SavedPublications > 0) {
		t.Fatalf("saved publication present=%t, want %d", receipt != nil, *expect.SavedPublications)
	}
	if len(publisher.Calls) == 0 {
		return
	}
	body := publisher.Calls[0].TranscriptBody
	var envelope schema.TranscriptContent
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode uploaded envelope: %v", err)
	}
	if envelope.SessionDetail == nil {
		t.Fatal("uploaded envelope has no sessionDetail")
	}
	detail := envelope.SessionDetail

	if expect.ContentHashMatchesBytes {
		if len(publisher.AuthoritativeCalls) != 1 {
			t.Fatalf("authoritative calls=%d", len(publisher.AuthoritativeCalls))
		}
		if got, want := schema.ComputeTranscriptContentHash(body), publisher.AuthoritativeCalls[0].ContentHash; got != want {
			t.Fatalf("content hash of published bytes=%s, want %s", got, want)
		}
	}
	if expect.FingerprintMatches {
		if receipt == nil {
			t.Fatal("no receipt to bind the operation fingerprint to")
		}
		operation, err := schema.CanonicalizePublishRequest(publisher.AuthoritativeCalls[0])
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, err := schema.FingerprintPublishOperation(operation)
		if err != nil {
			t.Fatal(err)
		}
		if got := receipt.Receipt.RequestOperationFingerprint; got != fingerprint {
			t.Fatalf("receipt fingerprint=%s, want recomputed %s", got, fingerprint)
		}
	}
	if expect.SecretAbsent {
		if bytes.Contains(body, []byte(ppeSecret)) {
			t.Fatal("published envelope still carries the raw secret from native metadata")
		}
		if !bytes.Contains(body, []byte("ANTHROPIC_KEY")) {
			t.Fatal("published envelope did not rewrite the native-metadata secret")
		}
	}
	if expect.ObservedModel != "" {
		found := false
		for _, turn := range detail.Turns {
			if string(turn.ObservedModel) == expect.ObservedModel {
				found = true
			}
		}
		if !found {
			t.Fatalf("observedModel %q did not survive redaction", expect.ObservedModel)
		}
	}
	if expect.Mirrors {
		if len(publisher.AuthoritativeCalls) != 1 {
			t.Fatalf("authoritative calls=%d", len(publisher.AuthoritativeCalls))
		}
		ppeAssertMirrors(t, publisher.AuthoritativeCalls[0], detail)
	}
	if expect.Detail != nil {
		ppeAssertDetail(t, *expect.Detail, built, detail)
	}
}

func ppeAssertMirrors(t *testing.T, request schema.AuthoritativePublishRequest, detail *schema.SessionDetailPayload) {
	t.Helper()
	if !optionalSessionIDEqual(request.Identity.RootSessionID, detail.RootSessionID) {
		t.Fatalf("metadata rootSessionId=%v, detail=%v", request.Identity.RootSessionID, detail.RootSessionID)
	}
	if request.Identity.Purpose != detail.Purpose {
		t.Fatalf("metadata purpose=%q, detail=%q", request.Identity.Purpose, detail.Purpose)
	}
	if !reflect.DeepEqual(request.Identity.Relationships, detail.Relationships) {
		t.Fatalf("metadata relationships=%+v, detail=%+v", request.Identity.Relationships, detail.Relationships)
	}
	if !optionalInt64Equal(request.Stats.InputSubmissionCount, detail.InputSubmissionCount) {
		t.Fatalf("metadata inputSubmissionCount=%v, detail=%v", request.Stats.InputSubmissionCount, detail.InputSubmissionCount)
	}
}

func ppeAssertDetail(t *testing.T, expect ppeDetailExpect, built ppeBuilt, detail *schema.SessionDetailPayload) {
	t.Helper()
	if expect.TurnCount != nil && detail.TurnCount != *expect.TurnCount {
		t.Fatalf("turnCount=%d, want %d (durable mirror, not len(turns)=%d)", detail.TurnCount, *expect.TurnCount, len(detail.Turns))
	}
	if expect.InputSubmissionCountAbsent && detail.InputSubmissionCount != nil {
		t.Fatalf("inputSubmissionCount=%v, want absent", *detail.InputSubmissionCount)
	}
	if expect.InputSubmissionCount != nil {
		if detail.InputSubmissionCount == nil {
			t.Fatalf("inputSubmissionCount absent, want %d", *expect.InputSubmissionCount)
		}
		if *detail.InputSubmissionCount != *expect.InputSubmissionCount {
			t.Fatalf("inputSubmissionCount=%d, want %d", *detail.InputSubmissionCount, *expect.InputSubmissionCount)
		}
	}
	if expect.RootSessionIDAbsent && detail.RootSessionID != nil {
		t.Fatalf("rootSessionId=%v, want absent", detail.RootSessionID)
	}
	if expect.RootSessionID != nil {
		want := ppeResolveID(*expect.RootSessionID)
		if detail.RootSessionID == nil || string(*detail.RootSessionID) != want {
			t.Fatalf("rootSessionId=%v, want %s", detail.RootSessionID, want)
		}
	}
	if expect.Purpose != nil && string(detail.Purpose) != *expect.Purpose {
		t.Fatalf("purpose=%q, want %q", detail.Purpose, *expect.Purpose)
	}
	if expect.Relationships != nil {
		want := make([]schema.SessionRelationship, len(expect.Relationships))
		for index, rel := range expect.Relationships {
			want[index] = rel
			if rel.TargetLocalID != nil {
				resolved := schema.SessionID(ppeResolveID(string(*rel.TargetLocalID)))
				want[index].TargetLocalID = &resolved
			}
		}
		if !relationshipListsEqual(want, detail.Relationships) {
			t.Fatalf("relationships=%+v, want %+v", detail.Relationships, want)
		}
	}
	if expect.EarlierHistory != nil {
		if len(detail.EarlierHistory) != len(expect.EarlierHistory) {
			t.Fatalf("earlierHistory sections=%d, want %d", len(detail.EarlierHistory), len(expect.EarlierHistory))
		}
		for index, section := range detail.EarlierHistory {
			want := expect.EarlierHistory[index]
			if section.State != want.State {
				t.Fatalf("earlierHistory[%d].state=%q, want %q", index, section.State, want.State)
			}
			gotIndexes := make([]int, len(section.Turns))
			for turn := range section.Turns {
				gotIndexes[turn] = section.Turns[turn].Index
			}
			if !reflect.DeepEqual(gotIndexes, want.TurnIndexes) {
				t.Fatalf("earlierHistory[%d].turnIndexes=%v, want %v", index, gotIndexes, want.TurnIndexes)
			}
			gotIDs := make([]string, len(section.NativeMetadata))
			for meta := range section.NativeMetadata {
				gotIDs[meta] = section.NativeMetadata[meta].ID
			}
			if !reflect.DeepEqual(gotIDs, want.NativeMetadataIDs) {
				t.Fatalf("earlierHistory[%d].nativeMetadataIds=%v, want %v", index, gotIDs, want.NativeMetadataIDs)
			}
		}
	}
	if expect.Project != nil && detail.Project != *expect.Project {
		t.Fatalf("project=%q, want %q", detail.Project, *expect.Project)
	}
	if expect.WorkingDirectory != nil && detail.WorkingDirectory != *expect.WorkingDirectory {
		t.Fatalf("workingDirectory=%q, want %q", detail.WorkingDirectory, *expect.WorkingDirectory)
	}
	if expect.GitBranch != nil && detail.GitBranch != *expect.GitBranch {
		t.Fatalf("gitBranch=%q, want %q", detail.GitBranch, *expect.GitBranch)
	}
	if expect.GitRemote != nil && detail.GitRemote != *expect.GitRemote {
		t.Fatalf("gitRemote=%q, want %q", detail.GitRemote, *expect.GitRemote)
	}
	if expect.DurationMins != nil && detail.DurationMins != *expect.DurationMins {
		t.Fatalf("durationMins=%v, want %v", detail.DurationMins, *expect.DurationMins)
	}
	if expect.TokensIn != nil && detail.TokensIn != *expect.TokensIn {
		t.Fatalf("tokensIn=%d, want %d (Pi mirrors come from per-turn usage, not capture totals)", detail.TokensIn, *expect.TokensIn)
	}
	if expect.TokensOut != nil && detail.TokensOut != *expect.TokensOut {
		t.Fatalf("tokensOut=%d, want %d (Pi mirrors come from per-turn usage, not capture totals)", detail.TokensOut, *expect.TokensOut)
	}
	if expect.TotalTokens != nil && detail.TotalTokens != *expect.TotalTokens {
		t.Fatalf("totalTokens=%d, want %d (Pi mirrors come from per-turn usage, not capture totals)", detail.TotalTokens, *expect.TotalTokens)
	}
	if expect.ToolArgumentEqualsManagedBlob {
		var arguments string
		for _, turn := range detail.Turns {
			for _, call := range turn.ToolCalls {
				arguments = call.Arguments
			}
		}
		if arguments != built.managedCallBody {
			t.Fatalf("tool arguments (%d bytes) are not the hydrated managed blob (%d bytes)", len(arguments), len(built.managedCallBody))
		}
	}
	if expect.ToolArgumentBytes != 0 {
		var arguments string
		for _, turn := range detail.Turns {
			for _, call := range turn.ToolCalls {
				arguments = call.Arguments
			}
		}
		if len(arguments) < expect.ToolArgumentBytes {
			t.Fatalf("tool arguments=%d bytes, want at least %d past the stored preview", len(arguments), expect.ToolArgumentBytes)
		}
	}
	if expect.CallProvenanceRef != "" || expect.ResultProvenanceRef != "" {
		var callRef, resultRef schema.SubmissionRef
		for _, turn := range detail.Turns {
			for _, call := range turn.ToolCalls {
				if call.CallProvenance != nil {
					callRef = call.CallProvenance.SubmissionRef
				}
				if call.ResultProvenance != nil {
					resultRef = call.ResultProvenance.SubmissionRef
				}
			}
		}
		if string(callRef) != expect.CallProvenanceRef || string(resultRef) != expect.ResultProvenanceRef {
			t.Fatalf("tool provenance call=%q result=%q, want call=%q result=%q", callRef, resultRef, expect.CallProvenanceRef, expect.ResultProvenanceRef)
		}
	}
}

func ppeResolveID(value string) string {
	switch value {
	case "target":
		return ppeTargetID
	case "previous":
		return ppePreviousID
	default:
		return value
	}
}

func relationshipListsEqual(a, b []schema.SessionRelationship) bool {
	normalize := func(in []schema.SessionRelationship) []schema.SessionRelationship {
		out := append([]schema.SessionRelationship(nil), in...)
		sort.Slice(out, func(i, j int) bool {
			if out[i].Kind != out[j].Kind {
				return out[i].Kind < out[j].Kind
			}
			return out[i].TargetState < out[j].TargetState
		})
		return out
	}
	return reflect.DeepEqual(normalize(a), normalize(b))
}

func optionalSessionIDEqual(a, b *schema.SessionID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func optionalInt64Equal(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
