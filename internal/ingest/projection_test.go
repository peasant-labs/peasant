package ingest_test

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/projection_layout.yaml
var projectionLayoutYAML []byte

//go:embed testdata/projection_layout.manifest.yaml
var projectionLayoutManifestYAML []byte

type fixtureProvenance struct {
	Origin        string `yaml:"origin"`
	Actor         string `yaml:"actor"`
	Delivery      string `yaml:"delivery"`
	Ownership     string `yaml:"ownership"`
	Evidence      string `yaml:"evidence"`
	InputModality string `yaml:"inputModality"`
}

type fixtureRepeat struct {
	Text  string `yaml:"text"`
	Times int    `yaml:"times"`
}

type fixtureUsage struct {
	TokensIn  *int `yaml:"tokensIn"`
	TokensOut *int `yaml:"tokensOut"`
}

type fixtureNativeAttachment struct {
	ID                    string `yaml:"id"`
	Kind                  string `yaml:"kind"`
	SourceType            string `yaml:"sourceType"`
	CustomType            string `yaml:"customType"`
	Data                  string `yaml:"data"`
	AttachmentToolCallKey string `yaml:"attachmentToolCallKey"`
	MessageRole           string `yaml:"messageRole"`
}

type fixtureSection struct {
	Earlier bool `yaml:"earlier"`
	Index   int  `yaml:"index"`
}

type fixtureBlock struct {
	NativeKey            string                    `yaml:"nativeKey"`
	SubmissionKey        string                    `yaml:"submissionKey"`
	AmbiguousPairKey     string                    `yaml:"ambiguousPairKey"`
	NativeCorrelationKey string                    `yaml:"nativeCorrelationKey"`
	Section              fixtureSection            `yaml:"section"`
	Uncertain            bool                      `yaml:"uncertain"`
	UncertainSubtree     bool                      `yaml:"uncertainSubtree"`
	Role                 string                    `yaml:"role"`
	EntryType            string                    `yaml:"entryType"`
	Depth                int                       `yaml:"depth"`
	CarrierNativeKey     string                    `yaml:"carrierNativeKey"`
	ToolCallKey          string                    `yaml:"toolCallKey"`
	ToolName             string                    `yaml:"toolName"`
	Content              string                    `yaml:"content"`
	ToolArguments        string                    `yaml:"toolArguments"`
	ToolResult           string                    `yaml:"toolResult"`
	ContentRepeat        *fixtureRepeat            `yaml:"contentRepeat"`
	ToolArgumentsRepeat  *fixtureRepeat            `yaml:"toolArgumentsRepeat"`
	ToolResultRepeat     *fixtureRepeat            `yaml:"toolResultRepeat"`
	Provenance           *fixtureProvenance        `yaml:"provenance"`
	Usage                *fixtureUsage             `yaml:"usage"`
	ObservedModel        *string                   `yaml:"observedModel"`
	NativeAttachments    []fixtureNativeAttachment `yaml:"nativeAttachments"`
}

type fixtureMetadata struct {
	SessionID     string `yaml:"sessionId"`
	ModelHarness  string `yaml:"modelHarness"`
	SchemaVersion int    `yaml:"schemaVersion"`
	RootSessionID string `yaml:"rootSessionId"`
	Purpose       string `yaml:"purpose"`
}

type fixtureCapture struct {
	ID                   string          `yaml:"id"`
	SessionID            string          `yaml:"sessionId"`
	Harness              string          `yaml:"harness"`
	SourceEvidenceDigest string          `yaml:"sourceEvidenceDigest"`
	Completeness         string          `yaml:"completeness"`
	EarlierStates        []string        `yaml:"earlierStates"`
	Metadata             fixtureMetadata `yaml:"metadata"`
	Blocks               []fixtureBlock  `yaml:"blocks"`
}

type fixtureEntryExpect struct {
	NativeKey        string         `yaml:"nativeKey"`
	Index            int            `yaml:"index"`
	Ref              string         `yaml:"ref"`
	Role             string         `yaml:"role"`
	EntryType        string         `yaml:"entryType"`
	Depth            int            `yaml:"depth"`
	ParentIndex      *int           `yaml:"parentIndex"`
	Content          string         `yaml:"content"`
	ToolInput        string         `yaml:"toolInput"`
	ToolOutput       string         `yaml:"toolOutput"`
	ContentRepeat    *fixtureRepeat `yaml:"contentRepeat"`
	ToolInputRepeat  *fixtureRepeat `yaml:"toolInputRepeat"`
	ToolOutputRepeat *fixtureRepeat `yaml:"toolOutputRepeat"`
	ToolCallID       string         `yaml:"toolCallId"`
	Submission       string         `yaml:"submissionRef"`
	Ownership        string         `yaml:"ownership"`
	ByteLength       int            `yaml:"byteLength"`
	TokensIn         *int           `yaml:"tokensIn"`
	TokensOut        *int           `yaml:"tokensOut"`
	ObservedModel    string         `yaml:"observedModel"`
}

type fixtureNativeExpect struct {
	ID               string `yaml:"id"`
	Kind             string `yaml:"kind"`
	SourceType       string `yaml:"sourceType"`
	EntryRef         string `yaml:"entryRef"`
	CustomType       string `yaml:"customType"`
	Data             string `yaml:"data"`
	AttachmentToolID string `yaml:"attachmentToolId"`
}

type fixtureEarlierExpect struct {
	State          string                `yaml:"state"`
	Entries        []fixtureEntryExpect  `yaml:"entries"`
	NativeMetadata []fixtureNativeExpect `yaml:"nativeMetadata"`
}

type fixtureWant struct {
	TurnCount             int                    `yaml:"turnCount"`
	InputSubmissionCount  *int64                 `yaml:"inputSubmissionCount"`
	InputSubmissionAbsent bool                   `yaml:"inputSubmissionAbsent"`
	TitleRefs             []string               `yaml:"titleRefs"`
	Main                  []fixtureEntryExpect   `yaml:"main"`
	MainNativeMetadata    []fixtureNativeExpect  `yaml:"mainNativeMetadata"`
	Earlier               []fixtureEarlierExpect `yaml:"earlier"`
}

type fixturePrior struct {
	Entries     map[string]string `yaml:"entries"`
	Submissions map[string]string `yaml:"submissions"`
}

type fixtureStep struct {
	Allocator         []string       `yaml:"allocator"`
	Prior             *fixturePrior  `yaml:"prior"`
	PriorFromPrevious bool           `yaml:"priorFromPrevious"`
	WantError         string         `yaml:"wantError"`
	Capture           fixtureCapture `yaml:"capture"`
	Want              fixtureWant    `yaml:"want"`
}

type fixtureCase struct {
	Name  string        `yaml:"name"`
	Steps []fixtureStep `yaml:"steps"`
}

type fixtureDocument struct {
	Cases []fixtureCase `yaml:"cases"`
}

// queueAllocator hands out a fixed reference sequence. An exhausted queue is an
// error, so a step that wrongly allocates a new identity fails instead of
// silently passing.
type queueAllocator struct {
	entry      []string
	submission []string
}

func (a *queueAllocator) NewEntryRef() (schema.SourceEntryRef, error) {
	if len(a.entry) == 0 {
		return "", errors.New("projection fixture allocator: unexpected block allocation; a prior identity should have been reused")
	}
	next := a.entry[0]
	a.entry = a.entry[1:]
	return schema.SourceEntryRef(next), nil
}

func (a *queueAllocator) NewSubmissionRef() (schema.SubmissionRef, error) {
	if len(a.submission) == 0 {
		return "", errors.New("projection fixture allocator: unexpected submission allocation; a prior identity should have been reused")
	}
	next := a.submission[0]
	a.submission = a.submission[1:]
	return schema.SubmissionRef(next), nil
}

func loadProjectionCases(t *testing.T) []fixtureCase {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(projectionLayoutYAML))
	decoder.KnownFields(true)
	var document fixtureDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode projection layout fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("projection layout fixture must contain exactly one YAML document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(projectionLayoutManifestYAML, "projection layout")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
		if len(row.Steps) == 0 {
			t.Fatalf("projection layout fixture case %q has no steps; a case without steps passes without invoking the builder; declare at least one step", row.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "projection layout"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func buildProjectionProvenance(in *fixtureProvenance) *schema.ContentProvenance {
	if in == nil {
		return nil
	}
	return &schema.ContentProvenance{
		Origin:        schema.ContentOrigin(in.Origin),
		Actor:         schema.ActorOrigin(in.Actor),
		Delivery:      schema.DeliveryOrigin(in.Delivery),
		Ownership:     schema.ContentOwnership(in.Ownership),
		Evidence:      schema.EvidenceKind(in.Evidence),
		InputModality: schema.InputModality(in.InputModality),
	}
}

func repeatText(repeat *fixtureRepeat, fallback string) string {
	if repeat == nil {
		return fallback
	}
	return strings.Repeat(repeat.Text, repeat.Times)
}

func buildProjectionCapture(in fixtureCapture) ingest.ClassifiedCapture {
	capture := ingest.ClassifiedCapture{
		ID:                   in.ID,
		SessionID:            ingest.SessionID(in.SessionID),
		Harness:              schema.Harness(in.Harness),
		SourceEvidenceDigest: in.SourceEvidenceDigest,
		Completeness:         indexformat.GenerationCompleteness(in.Completeness),
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: in.Metadata.SchemaVersion,
			SessionID:     schema.SessionID(in.Metadata.SessionID),
			ModelHarness:  schema.Harness(in.Metadata.ModelHarness),
			Purpose:       schema.SessionPurpose(in.Metadata.Purpose),
		},
	}
	if in.Metadata.RootSessionID != "" {
		root := schema.SessionID(in.Metadata.RootSessionID)
		capture.Metadata.RootSessionID = &root
	}
	for _, state := range in.EarlierStates {
		capture.EarlierStates = append(capture.EarlierStates, schema.EarlierHistoryState(state))
	}
	for _, row := range in.Blocks {
		block := ingest.ClassifiedBlock{
			NativeKey:            row.NativeKey,
			SubmissionKey:        row.SubmissionKey,
			AmbiguousPairKey:     row.AmbiguousPairKey,
			NativeCorrelationKey: row.NativeCorrelationKey,
			Section:              ingest.ProjectionSection{Earlier: row.Section.Earlier, Index: row.Section.Index},
			Uncertain:            row.Uncertain,
			UncertainSubtree:     row.UncertainSubtree,
			Role:                 schema.Role(row.Role),
			EntryType:            schema.EntryType(row.EntryType),
			Depth:                row.Depth,
			CarrierNativeKey:     row.CarrierNativeKey,
			ToolCallKey:          row.ToolCallKey,
			ToolName:             row.ToolName,
			Content:              repeatText(row.ContentRepeat, row.Content),
			ToolArguments:        repeatText(row.ToolArgumentsRepeat, row.ToolArguments),
			ToolResult:           repeatText(row.ToolResultRepeat, row.ToolResult),
			Provenance:           buildProjectionProvenance(row.Provenance),
		}
		if row.Usage != nil {
			block.Usage = &ingest.ClassifiedUsage{
				TokensIn:  row.Usage.TokensIn,
				TokensOut: row.Usage.TokensOut,
			}
		}
		if row.ObservedModel != nil {
			value := *row.ObservedModel
			block.ObservedModel = &value
		}
		for _, attachment := range row.NativeAttachments {
			block.NativeAttachments = append(block.NativeAttachments, ingest.ClassifiedNativeAttachment{
				ID:                    attachment.ID,
				Kind:                  schema.NativeMetadataKind(attachment.Kind),
				SourceType:            schema.NativeMetadataSourceType(attachment.SourceType),
				CustomType:            attachment.CustomType,
				Data:                  attachment.Data,
				AttachmentToolCallKey: attachment.AttachmentToolCallKey,
				MessageRole:           schema.NativePiMessageRole(attachment.MessageRole),
			})
		}
		capture.Blocks = append(capture.Blocks, block)
	}
	return capture
}

func buildProjectionAllocator(refs []string) *queueAllocator {
	allocator := &queueAllocator{}
	for _, ref := range refs {
		if strings.HasPrefix(ref, "s_") {
			allocator.submission = append(allocator.submission, ref)
		} else {
			allocator.entry = append(allocator.entry, ref)
		}
	}
	return allocator
}

// buildProjectionPrior builds the production prior alias state a caller supplies
// before allocation. A fixture uses it to model a persisted alias map that an
// earlier generation left behind, including aliases it had merged.
func buildProjectionPrior(in fixturePrior) ingest.ProjectionPriorState {
	prior := ingest.NewProjectionPriorState()
	for key, ref := range in.Entries {
		prior.Entries[key] = schema.SourceEntryRef(ref)
	}
	for key, ref := range in.Submissions {
		prior.Submissions[key] = schema.SubmissionRef(ref)
	}
	return prior
}

func gotEntryContent(entry schema.SessionEntry) string {
	if entry.ToolInput != nil {
		return *entry.ToolInput
	}
	if entry.ToolOutput != nil {
		return *entry.ToolOutput
	}
	if entry.ContentPreview != nil {
		return *entry.ContentPreview
	}
	return ""
}

func wantEntryContent(want fixtureEntryExpect) string {
	if want.ToolInputRepeat != nil {
		return strings.Repeat(want.ToolInputRepeat.Text, want.ToolInputRepeat.Times)
	}
	if want.ToolOutputRepeat != nil {
		return strings.Repeat(want.ToolOutputRepeat.Text, want.ToolOutputRepeat.Times)
	}
	if want.ContentRepeat != nil {
		return strings.Repeat(want.ContentRepeat.Text, want.ContentRepeat.Times)
	}
	if want.ToolInput != "" {
		return want.ToolInput
	}
	if want.ToolOutput != "" {
		return want.ToolOutput
	}
	return want.Content
}

func observedModelOf(entry schema.SessionEntry) string {
	if entry.Role != schema.RoleAssistant || entry.Extra == nil {
		return ""
	}
	var extra map[string]json.RawMessage
	if json.Unmarshal([]byte(*entry.Extra), &extra) != nil {
		return ""
	}
	raw, ok := extra["model_id"]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func assertProjectionEntry(t *testing.T, label string, want fixtureEntryExpect, got schema.SessionEntry, records map[string]indexformat.ContentRecord) {
	t.Helper()
	if got.EntryIndex != want.Index {
		t.Fatalf("%s entry index = %d, want %d", label, got.EntryIndex, want.Index)
	}
	if string(got.SourceEntryRef) != want.Ref {
		t.Fatalf("%s native key %q ref = %q, want %q", label, want.NativeKey, got.SourceEntryRef, want.Ref)
	}
	if string(got.Role) != want.Role {
		t.Fatalf("%s ref %q role = %q, want %q", label, want.Ref, got.Role, want.Role)
	}
	if string(got.EntryType) != want.EntryType {
		t.Fatalf("%s ref %q entryType = %q, want %q", label, want.Ref, got.EntryType, want.EntryType)
	}
	if got.Depth != want.Depth {
		t.Fatalf("%s ref %q depth = %d, want %d", label, want.Ref, got.Depth, want.Depth)
	}
	if !reflect.DeepEqual(got.ParentIndex, want.ParentIndex) {
		t.Fatalf("%s ref %q parentIndex = %v, want %v", label, want.Ref, got.ParentIndex, want.ParentIndex)
	}
	if want.ToolCallID != "" {
		if got.ToolCallID == nil || *got.ToolCallID != want.ToolCallID {
			t.Fatalf("%s ref %q toolCallId = %v, want %q", label, want.Ref, got.ToolCallID, want.ToolCallID)
		}
	} else if got.ToolCallID != nil {
		t.Fatalf("%s ref %q toolCallId = %q, want none", label, want.Ref, *got.ToolCallID)
	}
	content := gotEntryContent(got)
	expectedContent := wantEntryContent(want)
	if want.ByteLength > 0 {
		if len(content) != want.ByteLength {
			t.Fatalf("%s ref %q content bytes = %d, want %d", label, want.Ref, len(content), want.ByteLength)
		}
		if expectedContent != "" && content != expectedContent {
			t.Fatalf("%s ref %q content differs from fixture-source bytes; got %d bytes, want exact %d source bytes", label, want.Ref, len(content), len(expectedContent))
		}
		// Long-tool evidence must exceed the preview floor so truncation cannot
		// hide a same-length corruption.
		if len(content) <= defaults.ContentPreviewLimit+1024 && (want.ToolInputRepeat != nil || want.ToolOutputRepeat != nil) {
			t.Fatalf("%s ref %q content bytes = %d, want more than preview limit+1024 (%d)", label, want.Ref, len(content), defaults.ContentPreviewLimit+1024)
		}
	} else if content != expectedContent {
		t.Fatalf("%s ref %q content = %q, want %q", label, want.Ref, content, expectedContent)
	}
	if want.Ownership == "" {
		if got.Provenance != nil {
			t.Fatalf("%s ref %q has provenance, want none", label, want.Ref)
		}
	} else {
		if got.Provenance == nil {
			t.Fatalf("%s ref %q provenance is nil, want ownership %q", label, want.Ref, want.Ownership)
		}
		if string(got.Provenance.Ownership) != want.Ownership {
			t.Fatalf("%s ref %q ownership = %q, want %q", label, want.Ref, got.Provenance.Ownership, want.Ownership)
		}
	}
	if want.Submission != "" {
		if got.Provenance == nil || string(got.Provenance.SubmissionRef) != want.Submission {
			t.Fatalf("%s ref %q submissionRef = %v, want %q", label, want.Ref, got.Provenance, want.Submission)
		}
	} else if got.Provenance != nil && got.Provenance.SubmissionRef != "" {
		t.Fatalf("%s ref %q submissionRef = %q, want none", label, want.Ref, got.Provenance.SubmissionRef)
	}
	if !reflect.DeepEqual(got.TokensIn, want.TokensIn) {
		t.Fatalf("%s ref %q tokensIn = %v, want %v", label, want.Ref, got.TokensIn, want.TokensIn)
	}
	if !reflect.DeepEqual(got.TokensOut, want.TokensOut) {
		t.Fatalf("%s ref %q tokensOut = %v, want %v", label, want.Ref, got.TokensOut, want.TokensOut)
	}
	if gotObserved := observedModelOf(got); gotObserved != want.ObservedModel {
		t.Fatalf("%s ref %q observedModel = %q, want %q", label, want.Ref, gotObserved, want.ObservedModel)
	}
	if content != "" {
		record, ok := records[want.Ref]
		if !ok {
			t.Fatalf("%s ref %q has no content record", label, want.Ref)
		}
		if record.ByteLength != int64(len(content)) {
			t.Fatalf("%s ref %q content record byteLength = %d, want %d", label, want.Ref, record.ByteLength, len(content))
		}
		// The digest must match the fixture-source bytes, not just the
		// returned bytes, so a same-length corruption cannot evade the oracle.
		digestSource := content
		if expectedContent != "" {
			digestSource = expectedContent
			if content != expectedContent {
				t.Fatalf("%s ref %q content differs from expected source bytes", label, want.Ref)
			}
		}
		sum := sha256.Sum256([]byte(digestSource))
		if record.Digest != hex.EncodeToString(sum[:]) {
			t.Fatalf("%s ref %q content digest = %q, want the digest of the expected source bytes", label, want.Ref, record.Digest)
		}
		if record.RelativeBlob != "content/"+want.Ref {
			t.Fatalf("%s ref %q relativeBlob = %q, want %q", label, want.Ref, record.RelativeBlob, "content/"+want.Ref)
		}
	}
}

func assertProjectionWant(t *testing.T, label string, want fixtureWant, generation indexformat.Generation) {
	t.Helper()
	if generation.Metadata.Stats.TurnCount != want.TurnCount {
		t.Fatalf("%s turnCount = %d, want %d", label, generation.Metadata.Stats.TurnCount, want.TurnCount)
	}
	if want.InputSubmissionAbsent {
		if generation.Metadata.Stats.InputSubmissionCount != nil {
			t.Fatalf("%s inputSubmissionCount = %d, want absent", label, *generation.Metadata.Stats.InputSubmissionCount)
		}
	} else {
		if generation.Metadata.Stats.InputSubmissionCount == nil {
			t.Fatalf("%s inputSubmissionCount absent, want %v", label, want.InputSubmissionCount)
		}
		wantCount := int64(0)
		if want.InputSubmissionCount != nil {
			wantCount = *want.InputSubmissionCount
		}
		if *generation.Metadata.Stats.InputSubmissionCount != wantCount {
			t.Fatalf("%s inputSubmissionCount = %d, want %d", label, *generation.Metadata.Stats.InputSubmissionCount, wantCount)
		}
	}
	gotTitles := make([]string, len(generation.TitleRefs))
	for i, ref := range generation.TitleRefs {
		gotTitles[i] = string(ref)
	}
	if !reflect.DeepEqual(gotTitles, want.TitleRefs) {
		t.Fatalf("%s titleRefs = %v, want %v", label, gotTitles, want.TitleRefs)
	}
	if len(want.Main) != len(generation.Main.Entries) {
		t.Fatalf("%s main entry count = %d, want %d", label, len(generation.Main.Entries), len(want.Main))
	}
	records := make(map[string]indexformat.ContentRecord, len(generation.Content))
	for _, record := range generation.Content {
		records[string(record.Ref)] = record
	}
	for i, entryWant := range want.Main {
		assertProjectionEntry(t, label+" main", entryWant, generation.Main.Entries[i], records)
	}
	assertProjectionNativeMetadata(t, label+" main nativeMetadata", want.MainNativeMetadata, generation.Main.NativeMetadata)
	if len(want.Earlier) != len(generation.Earlier) {
		t.Fatalf("%s earlier section count = %d, want %d", label, len(generation.Earlier), len(want.Earlier))
	}
	usedRefs := make(map[string]bool)
	for i, earlierWant := range want.Earlier {
		section := generation.Earlier[i]
		if string(section.State) != earlierWant.State {
			t.Fatalf("%s earlier[%d] state = %q, want %q", label, i, section.State, earlierWant.State)
		}
		if len(earlierWant.Entries) != len(section.Content.Entries) {
			t.Fatalf("%s earlier[%d] entry count = %d, want %d", label, i, len(section.Content.Entries), len(earlierWant.Entries))
		}
		for j, entryWant := range earlierWant.Entries {
			assertProjectionEntry(t, fmt.Sprintf("%s earlier[%d]", label, i), entryWant, section.Content.Entries[j], records)
		}
		assertProjectionNativeMetadata(t, fmt.Sprintf("%s earlier[%d] nativeMetadata", label, i), earlierWant.NativeMetadata, section.Content.NativeMetadata)
	}
	for _, entry := range generation.Main.Entries {
		usedRefs[string(entry.SourceEntryRef)] = true
	}
	for i := range generation.Earlier {
		for _, entry := range generation.Earlier[i].Content.Entries {
			usedRefs[string(entry.SourceEntryRef)] = true
		}
	}
	for ref := range records {
		if !usedRefs[ref] {
			t.Fatalf("%s content record ref %q is not an emitted entry", label, ref)
		}
	}
}

func assertProjectionNativeMetadata(t *testing.T, label string, want []fixtureNativeExpect, got []schema.NativeMetadataRecord) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s record count = %d, want %d", label, len(got), len(want))
	}
	for i, recordWant := range want {
		record := got[i]
		if record.ID != recordWant.ID {
			t.Fatalf("%s[%d] id = %q, want %q", label, i, record.ID, recordWant.ID)
		}
		if string(record.Kind) != recordWant.Kind {
			t.Fatalf("%s[%d] kind = %q, want %q", label, i, record.Kind, recordWant.Kind)
		}
		if string(record.Source.SourceType) != recordWant.SourceType {
			t.Fatalf("%s[%d] sourceType = %q, want %q", label, i, record.Source.SourceType, recordWant.SourceType)
		}
		if string(record.Source.EntryRef) != recordWant.EntryRef {
			t.Fatalf("%s[%d] entryRef = %q, want %q", label, i, record.Source.EntryRef, recordWant.EntryRef)
		}
		if record.CustomType != recordWant.CustomType {
			t.Fatalf("%s[%d] customType = %q, want %q", label, i, record.CustomType, recordWant.CustomType)
		}
		if string(record.Data) != recordWant.Data {
			t.Fatalf("%s[%d] data = %q, want %q", label, i, string(record.Data), recordWant.Data)
		}
		gotTool := ""
		if record.Attachment != nil {
			gotTool = record.Attachment.ToolCallID
		}
		if gotTool != recordWant.AttachmentToolID {
			t.Fatalf("%s[%d] attachmentToolId = %q, want %q", label, i, gotTool, recordWant.AttachmentToolID)
		}
	}
}

func expectedProjectionAliases(step fixtureStep, want fixtureWant) map[string]string {
	refByNativeKey := make(map[string]string)
	for _, entry := range want.Main {
		if entry.NativeKey != "" {
			refByNativeKey[entry.NativeKey] = entry.Ref
		}
	}
	for _, earlier := range want.Earlier {
		for _, entry := range earlier.Entries {
			if entry.NativeKey != "" {
				refByNativeKey[entry.NativeKey] = entry.Ref
			}
		}
	}
	aliases := make(map[string]string)
	for _, block := range step.Capture.Blocks {
		if ref, ok := refByNativeKey[block.NativeKey]; ok {
			aliases["block:"+block.NativeKey] = ref
		}
	}
	// A mirror collapsed by an explicit native correlation proof aliases the
	// retained owner entry.
	correlationOwner := make(map[string]string)
	for _, block := range step.Capture.Blocks {
		if block.NativeCorrelationKey == "" {
			continue
		}
		if _, ok := correlationOwner[block.NativeCorrelationKey]; ok {
			continue
		}
		if ref, ok := refByNativeKey[block.NativeKey]; ok {
			correlationOwner[block.NativeCorrelationKey] = ref
		}
	}
	for _, block := range step.Capture.Blocks {
		if block.NativeCorrelationKey == "" {
			continue
		}
		if _, ok := aliases["block:"+block.NativeKey]; ok {
			continue
		}
		if ref, ok := correlationOwner[block.NativeCorrelationKey]; ok {
			aliases["block:"+block.NativeKey] = ref
		}
	}
	firstSubmission := make(map[string]string)
	for _, entry := range want.Main {
		if entry.Submission != "" {
			if _, ok := firstSubmission[entry.Submission]; !ok {
				firstSubmission[entry.Submission] = entry.Ref
			}
		}
	}
	for _, earlier := range want.Earlier {
		for _, entry := range earlier.Entries {
			if entry.Submission != "" {
				if _, ok := firstSubmission[entry.Submission]; !ok {
					firstSubmission[entry.Submission] = entry.Ref
				}
			}
		}
	}
	submissionKeyByRef := make(map[string]string)
	for _, block := range step.Capture.Blocks {
		if block.SubmissionKey == "" {
			continue
		}
		if _, ok := submissionKeyByRef[block.SubmissionKey]; !ok {
			if ref, ok := refByNativeKey[block.NativeKey]; ok {
				submissionKeyByRef[block.SubmissionKey] = ref
			}
		}
	}
	for key, ref := range submissionKeyByRef {
		aliases["accept:"+key] = ref
	}
	return aliases
}

func assertProjectionAliases(t *testing.T, label string, expected map[string]string, generation indexformat.Generation) {
	t.Helper()
	got := make(map[string]string, len(generation.Aliases))
	for _, alias := range generation.Aliases {
		got[alias.NativeKey] = string(alias.Ref)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("%s aliases = %v, want %v", label, got, expected)
	}
}

// TestProjectionLayoutFixtures drives the real shared candidate builder through
// every projected-identity, layout, folding, content and count invariant. Each
// case is a named YAML step sequence; a step may reuse the previous step's
// alias state to prove identity across reinterpretation, append and retry.
func TestProjectionLayoutFixtures(t *testing.T) {
	t.Parallel()
	for _, row := range loadProjectionCases(t) {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			prior := ingest.NewProjectionPriorState()
			for stepIndex, step := range row.Steps {
				label := fmt.Sprintf("%s step %d", row.Name, stepIndex)
				capture := buildProjectionCapture(step.Capture)
				switch {
				case step.Prior != nil:
					capture.Prior = buildProjectionPrior(*step.Prior)
				case step.PriorFromPrevious:
					capture.Prior = prior
				}
				generation, err := ingest.BuildGeneration(capture, buildProjectionAllocator(step.Allocator))
				if step.WantError != "" {
					if err == nil || !strings.Contains(err.Error(), step.WantError) {
						t.Fatalf("%s error = %v, want substring %q", label, err, step.WantError)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s BuildGeneration() = %v", label, err)
				}
				assertProjectionWant(t, label, step.Want, generation)
				assertProjectionAliases(t, label, expectedProjectionAliases(step, step.Want), generation)
				prior, err = ingest.PriorStateFromGeneration(generation)
				if err != nil {
					t.Fatalf("%s PriorStateFromGeneration() = %v", label, err)
				}
			}
		})
	}
}

func randomBuildCapture() ingest.ClassifiedCapture {
	return ingest.ClassifiedCapture{
		ID:                   "gen-random",
		SessionID:            ingest.SessionID("sess_randomidentity"),
		Harness:              schema.HarnessCodex,
		SourceEvidenceDigest: "sha256:random",
		Completeness:         indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: 11,
			SessionID:     schema.SessionID("sess_randomidentity"),
			ModelHarness:  schema.HarnessCodex,
			Purpose:       schema.SessionPurposeInteraction,
		},
		Blocks: []ingest.ClassifiedBlock{
			{
				NativeKey:     "u1",
				SubmissionKey: "accept-random",
				Section:       ingest.ProjectionSection{},
				Role:          schema.RoleUser,
				EntryType:     schema.EntryTypeText,
				Content:       "fix parser",
				Provenance: &schema.ContentProvenance{
					Origin:        schema.ContentOriginSubmittedInput,
					Actor:         schema.ActorOriginUnknown,
					Delivery:      schema.DeliveryOriginSessionAdmission,
					Ownership:     schema.ContentOwnershipLocal,
					Evidence:      schema.EvidenceNativeTyped,
					InputModality: schema.InputModalityText,
				},
			},
			{
				NativeKey: "a1",
				Section:   ingest.ProjectionSection{},
				Role:      schema.RoleAssistant,
				EntryType: schema.EntryTypeText,
				Content:   "answer",
			},
		},
	}
}

// TestProjectionRandomAllocatorReuse proves the production allocator produces a
// valid generation and that the prior alias state keeps every identity stable
// across a rebuild even though a fresh allocator mints different random refs.
func TestProjectionRandomAllocatorReuse(t *testing.T) {
	t.Parallel()
	capture := randomBuildCapture()
	first, err := ingest.BuildGeneration(capture, ingest.RandomRefAllocator{})
	if err != nil {
		t.Fatalf("first BuildGeneration() = %v", err)
	}
	prior, err := ingest.PriorStateFromGeneration(first)
	if err != nil {
		t.Fatalf("PriorStateFromGeneration() = %v", err)
	}
	capture.Prior = prior
	second, err := ingest.BuildGeneration(capture, ingest.RandomRefAllocator{})
	if err != nil {
		t.Fatalf("second BuildGeneration() = %v", err)
	}
	firstRef := first.Main.Entries[0].SourceEntryRef
	secondRef := second.Main.Entries[0].SourceEntryRef
	if firstRef != secondRef {
		t.Fatalf("block ref changed across reuse: %q then %q", firstRef, secondRef)
	}
	firstSubmission := first.Main.Entries[0].Provenance.SubmissionRef
	secondSubmission := second.Main.Entries[0].Provenance.SubmissionRef
	if firstSubmission == "" || firstSubmission != secondSubmission {
		t.Fatalf("submission ref changed across reuse: %q then %q", firstSubmission, secondSubmission)
	}
	if count := second.Metadata.Stats.InputSubmissionCount; count == nil || *count != 1 {
		t.Fatalf("input submission count = %v, want measured 1", count)
	}
}

// TestBuildV2WrapsValidatedGeneration pins the format-2 production exit: the
// projection returns a concrete V2 the existing version gate accepts.
func TestBuildV2WrapsValidatedGeneration(t *testing.T) {
	t.Parallel()
	result, err := ingest.BuildV2(randomBuildCapture(), ingest.RandomRefAllocator{})
	if err != nil {
		t.Fatalf("BuildV2() = %v", err)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("V2.Validate() = %v", err)
	}
	version, err := indexformat.VersionOf(result)
	if err != nil {
		t.Fatalf("VersionOf(V2) = %v", err)
	}
	if version != 2 {
		t.Fatalf("VersionOf(V2) = %d, want 2", version)
	}
}
