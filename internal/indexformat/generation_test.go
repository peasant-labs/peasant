package indexformat_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/enum_membership.yaml
var enumMembershipYAML []byte

//go:embed testdata/enum_membership.manifest.yaml
var enumMembershipManifestYAML []byte

//go:embed testdata/generation_validation.yaml
var generationValidationYAML []byte

//go:embed testdata/generation_validation.manifest.yaml
var generationValidationManifestYAML []byte

//go:embed testdata/snapshot_validation.yaml
var snapshotValidationYAML []byte

//go:embed testdata/snapshot_validation.manifest.yaml
var snapshotValidationManifestYAML []byte

// decodeFixture strictly decodes exactly one YAML document and rejects unknown
// fields so a fixture cannot drift from its typed loader unnoticed.
func decodeFixture[T any](t *testing.T, data []byte, label string) T {
	t.Helper()
	var out T
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&out); err != nil {
		t.Fatalf("decode %s fixture: %v", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("%s fixture must contain exactly one YAML document: %v", label, err)
	}
	return out
}

func loadRequiredNames(t *testing.T, data []byte, label string) testutil.RequiredNamesManifest {
	t.Helper()
	manifest, err := testutil.DecodeRequiredNamesManifest(data, label)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// --- Closed enum membership ---

type enumMembershipCase struct {
	Set     string   `yaml:"set"`
	Values  []string `yaml:"values"`
	Invalid []string `yaml:"invalid"`
}

type enumMembershipDocument struct {
	Cases []enumMembershipCase `yaml:"cases"`
}

func loadEnumMembershipCases(t *testing.T) []enumMembershipCase {
	t.Helper()
	document := decodeFixture[enumMembershipDocument](t, enumMembershipYAML, "enum membership")
	manifest := loadRequiredNames(t, enumMembershipManifestYAML, "enum membership")
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Set
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "enum membership"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// TestClosedEnumMembership pins each closed local enum to its exact published
// member set and proves that no out-of-set value is accepted, in both the
// constructor and the type's own IsValid check.
func TestClosedEnumMembership(t *testing.T) {
	t.Parallel()
	type enumUnderTest struct {
		all      []string
		isValid  func(string) bool
		validate func(string) error
	}
	enums := map[string]enumUnderTest{
		"coordinate-kind": {
			all:     stringsOf(indexformat.AllCoordinateKinds),
			isValid: func(raw string) bool { return indexformat.CoordinateKind(raw).IsValid() },
			validate: func(raw string) error {
				_, err := indexformat.NewCoordinateKind(raw)
				return err
			},
		},
		"segment-inclusion": {
			all:     stringsOf(indexformat.AllSegmentInclusions),
			isValid: func(raw string) bool { return indexformat.SegmentInclusion(raw).IsValid() },
			validate: func(raw string) error {
				_, err := indexformat.NewSegmentInclusion(raw)
				return err
			},
		},
		"generation-completeness": {
			all:     stringsOf(indexformat.AllGenerationCompletenesses),
			isValid: func(raw string) bool { return indexformat.GenerationCompleteness(raw).IsValid() },
			validate: func(raw string) error {
				_, err := indexformat.NewGenerationCompleteness(raw)
				return err
			},
		},
	}
	for _, row := range loadEnumMembershipCases(t) {
		enum, ok := enums[row.Set]
		if !ok {
			t.Fatalf("enum membership fixture names unknown set %q", row.Set)
		}
		if !slices.Equal(row.Values, enum.all) {
			t.Errorf("set %q values = %v, want exact %v", row.Set, row.Values, enum.all)
		}
		for _, value := range row.Values {
			if !enum.isValid(value) {
				t.Errorf("set %q IsValid(%q) = false, want true", row.Set, value)
			}
			if err := enum.validate(value); err != nil {
				t.Errorf("set %q constructor(%q) = %v, want nil", row.Set, value, err)
			}
		}
		for _, value := range row.Invalid {
			if enum.isValid(value) {
				t.Errorf("set %q IsValid(%q) = true, want false", row.Set, value)
			}
			if err := enum.validate(value); err == nil {
				t.Errorf("set %q constructor(%q) = nil, want error", row.Set, value)
			}
		}
	}
}

func stringsOf[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = string(value)
	}
	return out
}

// --- Generation validation ---

type fixtureEntry struct {
	EntryIndex     int                `yaml:"entryIndex"`
	Role           string             `yaml:"role"`
	SourceEntryRef string             `yaml:"sourceEntryRef"`
	SessionID      *string            `yaml:"sessionId"`
	Harness        *string            `yaml:"harness"`
	EntryType      *string            `yaml:"entryType"`
	Depth          *int               `yaml:"depth"`
	ParentIndex    *int               `yaml:"parentIndex"`
	Provenance     *fixtureProvenance `yaml:"provenance"`
	ToolKind       *string            `yaml:"toolKind"`
	StopReason     *string            `yaml:"stopReason"`
}

type fixtureProvenance struct {
	Origin        string `yaml:"origin"`
	Actor         string `yaml:"actor"`
	Delivery      string `yaml:"delivery"`
	Ownership     string `yaml:"ownership"`
	Evidence      string `yaml:"evidence"`
	InputModality string `yaml:"inputModality"`
	SubmissionRef string `yaml:"submissionRef"`
}

type fixtureMetadataRecord struct {
	ID         string `yaml:"id"`
	Kind       string `yaml:"kind"`
	SourceType string `yaml:"sourceType"`
	EntryRef   string `yaml:"entryRef"`
	CustomType string `yaml:"customType"`
	Data       string `yaml:"data"`
}

type fixturePartition struct {
	Entries        []fixtureEntry          `yaml:"entries"`
	NativeMetadata []fixtureMetadataRecord `yaml:"nativeMetadata"`
}

type fixtureEarlierPartition struct {
	State   string           `yaml:"state"`
	Content fixturePartition `yaml:"content"`
}

type fixtureRelationship struct {
	Kind          string `yaml:"kind"`
	TargetState   string `yaml:"targetState"`
	TargetLocalID string `yaml:"targetLocalId"`
	Evidence      string `yaml:"evidence"`
}

type fixtureMetadata struct {
	SessionID            string                `yaml:"sessionId"`
	ModelHarness         string                `yaml:"modelHarness"`
	InputSubmissionCount *int64                `yaml:"inputSubmissionCount"`
	TurnCount            int                   `yaml:"turnCount"`
	RootSessionID        string                `yaml:"rootSessionId"`
	Purpose              string                `yaml:"purpose"`
	Relationships        []fixtureRelationship `yaml:"relationships"`
}

type fixtureContentRecord struct {
	Ref          string `yaml:"ref"`
	RelativeBlob string `yaml:"relativeBlob"`
	ByteLength   int64  `yaml:"byteLength"`
	Digest       string `yaml:"digest"`
}

type fixtureAlias struct {
	NativeKey string `yaml:"nativeKey"`
	Ref       string `yaml:"ref"`
}

type fixtureSegment struct {
	Ordinal                 int      `yaml:"ordinal"`
	Inclusion               string   `yaml:"inclusion"`
	PhysicalSourceID        string   `yaml:"physicalSourceId"`
	CoordinateKind          string   `yaml:"coordinateKind"`
	Start                   *int64   `yaml:"start"`
	EndExclusive            *int64   `yaml:"endExclusive"`
	DecodedByteStart        *int64   `yaml:"decodedByteStart"`
	DecodedByteEndExclusive *int64   `yaml:"decodedByteEndExclusive"`
	CapturedRefs            []string `yaml:"capturedRefs"`
}

type fixtureGeneration struct {
	ID                   string                    `yaml:"id"`
	Completeness         string                    `yaml:"completeness"`
	SourceEvidenceDigest string                    `yaml:"sourceEvidenceDigest"`
	Metadata             fixtureMetadata           `yaml:"metadata"`
	Main                 fixturePartition          `yaml:"main"`
	Earlier              []fixtureEarlierPartition `yaml:"earlier"`
	Segments             []fixtureSegment          `yaml:"segments"`
	Content              []fixtureContentRecord    `yaml:"content"`
	Aliases              []fixtureAlias            `yaml:"aliases"`
	TitleRefs            []string                  `yaml:"titleRefs"`
}

type generationCase struct {
	Name                             string            `yaml:"name"`
	Generation                       fixtureGeneration `yaml:"generation"`
	WantError                        string            `yaml:"wantError"`
	ExpectInputSubmissionCountAbsent bool              `yaml:"expectInputSubmissionCountAbsent"`
	WantInputSubmissionCount         *int64            `yaml:"wantInputSubmissionCount"`
}

type generationDocument struct {
	Cases []generationCase `yaml:"cases"`
}

func loadGenerationCases(t *testing.T) []generationCase {
	t.Helper()
	document := decodeFixture[generationDocument](t, generationValidationYAML, "generation validation")
	manifest := loadRequiredNames(t, generationValidationManifestYAML, "generation validation")
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "generation validation"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func buildMetadata(in fixtureMetadata) schema.UnifiedMetadata {
	metadata := schema.UnifiedMetadata{
		SchemaVersion: schema.MetadataSchemaVersion,
		SessionID:     schema.SessionID(in.SessionID),
		ModelHarness:  schema.Harness(in.ModelHarness),
		Purpose:       schema.SessionPurpose(in.Purpose),
		Stats: schema.SessionStats{
			TurnCount:            in.TurnCount,
			InputSubmissionCount: in.InputSubmissionCount,
		},
		Relationships: buildRelationships(in.Relationships),
	}
	if in.RootSessionID != "" {
		id := schema.SessionID(in.RootSessionID)
		metadata.RootSessionID = &id
	}
	return metadata
}

func buildRelationships(in []fixtureRelationship) []schema.SessionRelationship {
	if len(in) == 0 {
		return nil
	}
	out := make([]schema.SessionRelationship, 0, len(in))
	for _, row := range in {
		relationship := schema.SessionRelationship{
			Kind:        schema.SessionRelationshipKind(row.Kind),
			TargetState: schema.RelationshipTargetState(row.TargetState),
			Evidence:    schema.EvidenceKind(row.Evidence),
		}
		if row.TargetLocalID != "" {
			id := schema.SessionID(row.TargetLocalID)
			relationship.TargetLocalID = &id
		}
		out = append(out, relationship)
	}
	return out
}

func buildPartition(in fixturePartition, sessionID, harness string) indexformat.Partition {
	partition := indexformat.Partition{}
	for _, row := range in.Entries {
		entry := schema.SessionEntry{
			EntryIndex:     row.EntryIndex,
			Role:           schema.Role(row.Role),
			SourceEntryRef: schema.SourceEntryRef(row.SourceEntryRef),
			SessionID:      schema.SessionID(sessionID),
			Harness:        schema.Harness(harness),
			EntryType:      schema.EntryTypeText,
		}
		if row.SessionID != nil {
			entry.SessionID = schema.SessionID(*row.SessionID)
		}
		if row.Harness != nil {
			entry.Harness = schema.Harness(*row.Harness)
		}
		if row.EntryType != nil {
			entry.EntryType = schema.EntryType(*row.EntryType)
		}
		if row.Depth != nil {
			entry.Depth = *row.Depth
		}
		if row.ParentIndex != nil {
			parent := *row.ParentIndex
			entry.ParentIndex = &parent
		}
		if row.Provenance != nil {
			provenance := &schema.ContentProvenance{
				Origin:        schema.ContentOrigin(row.Provenance.Origin),
				Actor:         schema.ActorOrigin(row.Provenance.Actor),
				Delivery:      schema.DeliveryOrigin(row.Provenance.Delivery),
				Ownership:     schema.ContentOwnership(row.Provenance.Ownership),
				Evidence:      schema.EvidenceKind(row.Provenance.Evidence),
				InputModality: schema.InputModality(row.Provenance.InputModality),
			}
			if row.Provenance.SubmissionRef != "" {
				provenance.SubmissionRef = schema.SubmissionRef(row.Provenance.SubmissionRef)
			}
			entry.Provenance = provenance
		}
		if row.ToolKind != nil {
			kind := schema.ToolCallKind(*row.ToolKind)
			entry.ToolKind = &kind
		}
		if row.StopReason != nil {
			reason := schema.StopReason(*row.StopReason)
			entry.StopReason = &reason
		}
		partition.Entries = append(partition.Entries, entry)
	}
	for _, row := range in.NativeMetadata {
		partition.NativeMetadata = append(partition.NativeMetadata, schema.NativeMetadataRecord{
			ID:   row.ID,
			Kind: schema.NativeMetadataKind(row.Kind),
			Source: schema.NativeSourceRef{
				EntryRef:   schema.SourceEntryRef(row.EntryRef),
				SourceType: schema.NativeMetadataSourceType(row.SourceType),
			},
			CustomType: row.CustomType,
			Data:       json.RawMessage(row.Data),
		})
	}
	return partition
}

func buildGeneration(in fixtureGeneration) indexformat.Generation {
	generation := indexformat.Generation{
		ID:                   in.ID,
		Completeness:         indexformat.GenerationCompleteness(in.Completeness),
		Metadata:             buildMetadata(in.Metadata),
		Main:                 buildPartition(in.Main, in.Metadata.SessionID, in.Metadata.ModelHarness),
		SourceEvidenceDigest: in.SourceEvidenceDigest,
	}
	for _, row := range in.Earlier {
		generation.Earlier = append(generation.Earlier, indexformat.EarlierPartition{
			State:   schema.EarlierHistoryState(row.State),
			Content: buildPartition(row.Content, in.Metadata.SessionID, in.Metadata.ModelHarness),
		})
	}
	for _, row := range in.Segments {
		generation.Segments = append(generation.Segments, indexformat.ContextSegment{
			Ordinal:          row.Ordinal,
			Inclusion:        indexformat.SegmentInclusion(row.Inclusion),
			PhysicalSourceID: row.PhysicalSourceID,
			Coordinates: indexformat.SegmentCoordinates{
				Kind:                    indexformat.CoordinateKind(row.CoordinateKind),
				Start:                   row.Start,
				EndExclusive:            row.EndExclusive,
				DecodedByteStart:        row.DecodedByteStart,
				DecodedByteEndExclusive: row.DecodedByteEndExclusive,
			},
			CapturedRefs: refsOf(row.CapturedRefs),
		})
	}
	for _, row := range in.Content {
		generation.Content = append(generation.Content, indexformat.ContentRecord{
			Ref:          schema.SourceEntryRef(row.Ref),
			RelativeBlob: row.RelativeBlob,
			ByteLength:   row.ByteLength,
			Digest:       row.Digest,
		})
	}
	for _, row := range in.Aliases {
		generation.Aliases = append(generation.Aliases, indexformat.NativeAlias{
			NativeKey: row.NativeKey,
			Ref:       schema.SourceEntryRef(row.Ref),
		})
	}
	generation.TitleRefs = refsOf(in.TitleRefs)
	return generation
}

func refsOf(values []string) []schema.SourceEntryRef {
	if len(values) == 0 {
		return nil
	}
	out := make([]schema.SourceEntryRef, len(values))
	for i, value := range values {
		out[i] = schema.SourceEntryRef(value)
	}
	return out
}

// TestGenerationValidationFixtures drives every ratified generation invariant
// through the real Generation.Validate, including the absent-versus-zero count
// distinction.
func TestGenerationValidationFixtures(t *testing.T) {
	t.Parallel()
	for _, row := range loadGenerationCases(t) {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			generation := buildGeneration(row.Generation)
			err := generation.Validate()
			if row.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), row.WantError) {
					t.Fatalf("Validate() error = %v, want substring %q", err, row.WantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			count := generation.Metadata.Stats.InputSubmissionCount
			if row.ExpectInputSubmissionCountAbsent && count != nil {
				t.Fatalf("input submission count = %d, want absent", *count)
			}
			if row.WantInputSubmissionCount != nil {
				if count == nil {
					t.Fatalf("input submission count absent, want %d", *row.WantInputSubmissionCount)
				}
				if *count != *row.WantInputSubmissionCount {
					t.Fatalf("input submission count = %d, want %d", *count, *row.WantInputSubmissionCount)
				}
			}
		})
	}
}

// --- Snapshot validation ---

type fixtureSession struct {
	ID                   string                `yaml:"id"`
	Harness              string                `yaml:"harness"`
	TurnCount            int                   `yaml:"turnCount"`
	InputSubmissionCount *int64                `yaml:"inputSubmissionCount"`
	ParentSessionID      string                `yaml:"parentSessionId"`
	RootSessionID        string                `yaml:"rootSessionId"`
	Purpose              string                `yaml:"purpose"`
	Relationships        []fixtureRelationship `yaml:"relationships"`
}

type fixtureSnapshot struct {
	IndexVersion  int                       `yaml:"indexVersion"`
	GenerationID  string                    `yaml:"generationId"`
	Completeness  string                    `yaml:"completeness"`
	LegacyHarness string                    `yaml:"legacyHarness"`
	LegacyPath    string                    `yaml:"legacyPath"`
	Session       fixtureSession            `yaml:"session"`
	Metadata      fixtureMetadata           `yaml:"metadata"`
	Main          fixturePartition          `yaml:"main"`
	Earlier       []fixtureEarlierPartition `yaml:"earlier"`
	Content       []fixtureContentRecord    `yaml:"content"`
	TitleRefs     []string                  `yaml:"titleRefs"`
}

type snapshotCase struct {
	Name      string          `yaml:"name"`
	Snapshot  fixtureSnapshot `yaml:"snapshot"`
	WantError string          `yaml:"wantError"`
}

type snapshotDocument struct {
	Cases []snapshotCase `yaml:"cases"`
}

func loadSnapshotCases(t *testing.T) []snapshotCase {
	t.Helper()
	document := decodeFixture[snapshotDocument](t, snapshotValidationYAML, "snapshot validation")
	manifest := loadRequiredNames(t, snapshotValidationManifestYAML, "snapshot validation")
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "snapshot validation"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func buildSnapshot(in fixtureSnapshot) indexformat.ReadSnapshot {
	snapshot := indexformat.ReadSnapshot{
		IndexVersion: in.IndexVersion,
		GenerationID: in.GenerationID,
		Completeness: indexformat.GenerationCompleteness(in.Completeness),
		LegacySource: indexformat.LegacySource{
			Harness: schema.Harness(in.LegacyHarness),
			Path:    in.LegacyPath,
		},
		Session: schema.SessionDetailPayload{
			ID:                   in.Session.ID,
			Harness:              schema.Harness(in.Session.Harness),
			TurnCount:            in.Session.TurnCount,
			InputSubmissionCount: in.Session.InputSubmissionCount,
			Purpose:              schema.SessionPurpose(in.Session.Purpose),
			Relationships:        buildRelationships(in.Session.Relationships),
		},
		Metadata:  buildMetadata(in.Metadata),
		Main:      buildPartition(in.Main, in.Metadata.SessionID, in.Metadata.ModelHarness),
		TitleRefs: refsOf(in.TitleRefs),
	}
	if in.Session.RootSessionID != "" {
		id := schema.SessionID(in.Session.RootSessionID)
		snapshot.Session.RootSessionID = &id
	}
	if in.Session.ParentSessionID != "" {
		id := schema.SessionID(in.Session.ParentSessionID)
		snapshot.Session.ParentSessionID = &id
	}
	for _, row := range in.Earlier {
		snapshot.Earlier = append(snapshot.Earlier, indexformat.EarlierPartition{
			State:   schema.EarlierHistoryState(row.State),
			Content: buildPartition(row.Content, in.Metadata.SessionID, in.Metadata.ModelHarness),
		})
	}
	for _, row := range in.Content {
		snapshot.Content = append(snapshot.Content, indexformat.ContentRecord{
			Ref:          schema.SourceEntryRef(row.Ref),
			RelativeBlob: row.RelativeBlob,
			ByteLength:   row.ByteLength,
			Digest:       row.Digest,
		})
	}
	return snapshot
}

// TestSnapshotValidationFixtures drives the snapshot metadata/session equality
// and the legacy-versus-generation read-state invariants.
func TestSnapshotValidationFixtures(t *testing.T) {
	t.Parallel()
	for _, row := range loadSnapshotCases(t) {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			snapshot := buildSnapshot(row.Snapshot)
			err := snapshot.Validate()
			if row.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), row.WantError) {
					t.Fatalf("Validate() error = %v, want substring %q", err, row.WantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

// --- Concrete result and resolver signatures ---

// TestV2ResultSignature proves format 2 is a concrete result version that the
// existing version gate accepts, and that a typed-nil result stays refused.
func TestV2ResultSignature(t *testing.T) {
	t.Parallel()
	generation := buildGeneration(loadGenerationCases(t)[0].Generation)
	version, err := indexformat.VersionOf(indexformat.V2{Generation: generation})
	if err != nil {
		t.Fatalf("VersionOf(V2) = %v", err)
	}
	if version != 2 {
		t.Fatalf("VersionOf(V2) = %d, want 2", version)
	}
	if _, err := indexformat.VersionOf((*indexformat.V2)(nil)); err == nil {
		t.Fatal("VersionOf((*V2)(nil)) = nil, want refusal")
	}
}

type snapshotReaderFunc func(context.Context, schema.SessionID, func(indexformat.ReadSnapshot) error) error

func (f snapshotReaderFunc) WithSessionSnapshot(ctx context.Context, id schema.SessionID, fn func(indexformat.ReadSnapshot) error) error {
	return f(ctx, id, fn)
}

type contentResolverFunc func(context.Context, schema.SessionID, string, indexformat.ContentRecord) ([]byte, error)

func (f contentResolverFunc) ReadFullContent(ctx context.Context, id schema.SessionID, generationID string, record indexformat.ContentRecord) ([]byte, error) {
	return f(ctx, id, generationID, record)
}

var (
	_ indexformat.SnapshotReader  = snapshotReaderFunc(nil)
	_ indexformat.ContentResolver = contentResolverFunc(nil)
)

// TestResolverInterfaceSignatures pins the injected reader/resolver method
// sets so a future signature drift fails to compile.
func TestResolverInterfaceSignatures(t *testing.T) {
	t.Parallel()
	var reader indexformat.SnapshotReader = snapshotReaderFunc(func(context.Context, schema.SessionID, func(indexformat.ReadSnapshot) error) error {
		return nil
	})
	var resolver indexformat.ContentResolver = contentResolverFunc(func(context.Context, schema.SessionID, string, indexformat.ContentRecord) ([]byte, error) {
		return nil, nil
	})
	if reader == nil || resolver == nil {
		t.Fatal("resolver interface adapters must be non-nil")
	}
}
