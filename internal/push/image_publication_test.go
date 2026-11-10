package push_test

import (
	_ "embed"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/image_publication.yaml
var imagePublicationYAML []byte

//go:embed testdata/image_publication.manifest.yaml
var imagePublicationManifestYAML []byte

type imagePublicationCase struct {
	Name           string   `yaml:"name"`
	Images         int      `yaml:"images"`
	Advertisement  []string `yaml:"advertisement"`
	WantOmitted    string   `yaml:"want_omitted"`
	WantCapability string   `yaml:"want_capability"`
	Refused        bool     `yaml:"refused"`
	Earlier        bool     `yaml:"earlier"`
}

func TestImagePublication(t *testing.T) {
	t.Parallel()
	var document struct {
		Cases []imagePublicationCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(imagePublicationYAML, &document); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(imagePublicationManifestYAML, "image publication")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, fixture := range document.Cases {
		names[i] = fixture.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "image publication"); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range document.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			entries, turns := imagePublicationEntries(fixture.Images)
			redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			meta := imagePublicationMeta()
			requestBody, err := push.MapMetadata(push.MapOptions{Meta: meta, Entries: entries, Fields: config.PushFieldVisibility{}.Resolve(), Redactor: redactor})
			if err != nil {
				t.Fatal(err)
			}
			content, err := push.BuildTranscriptContentValidated(meta, entries, defaults.PublishSchemaVersion, config.PushFieldVisibility{}, sessionorigin.Unknown)
			if err != nil {
				t.Fatalf("build published content: %v", err)
			}
			forbidden := []string{strings.Repeat("b", 64), "image/png", "byteSize", "recordedSize"}
			encodedContent, err := json.Marshal(content)
			if err != nil {
				t.Fatal(err)
			}
			for _, token := range forbidden {
				if strings.Contains(string(requestBody), token) {
					t.Fatalf("publication request still carries %q", token)
				}
				if strings.Contains(string(encodedContent), token) {
					t.Fatalf("content document still carries %q", token)
				}
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(requestBody, &document); err != nil {
				t.Fatal(err)
			}
			hash, err := json.Marshal(schema.ComputeTranscriptContentHash(encodedContent))
			if err != nil {
				t.Fatal(err)
			}
			document["contentHash"] = hash
			authoritative, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			request, err := schema.DecodeAuthoritativePublishRequest(authoritative)
			if err != nil {
				t.Fatalf("decode publication request: %v", err)
			}
			if len(request.Entries) != 1+fixture.Images {
				t.Fatalf("publication entries = %d, want %d", len(request.Entries), 1+fixture.Images)
			}
			for i, entry := range request.Entries {
				if entry.Extra != nil {
					t.Fatalf("entry %d extra = %s, want the image descriptor stripped", i, *entry.Extra)
				}
				if entry.ContentPreview != nil && strings.Contains(*entry.ContentPreview, "recordedSize") {
					t.Fatalf("entry %d preview still carries a recorded size", i)
				}
			}
			images := 0
			for _, turn := range content.SessionDetail.Turns {
				for _, part := range turn.ContentParts {
					if part.Type != schema.TurnPartTypeImage {
						continue
					}
					images++
					if part.Image == nil || part.Image.OmittedReason != schema.OmittedReason(fixture.WantOmitted) || part.Image.Ref != "" || part.Image.MediaType != "" || part.Image.ByteSize != 0 {
						t.Fatalf("published image = %+v, want only omittedReason %s", part.Image, fixture.WantOmitted)
					}
				}
			}
			if images != fixture.Images {
				t.Fatalf("published images = %d, want %d", images, fixture.Images)
			}
			_ = turns
			scan, err := push.ScanPublication(content)
			if err != nil {
				t.Fatal(err)
			}
			if !containsCapability(scan.Required, schema.ContentCapability(fixture.WantCapability)) {
				t.Fatalf("required = %v, want %s", scan.Required, fixture.WantCapability)
			}
			advertised := make([]schema.ContentCapability, len(fixture.Advertisement))
			for i, token := range fixture.Advertisement {
				advertised[i] = schema.ContentCapability(token)
			}
			support := push.RemoteCapabilitySupport{Known: true, Capabilities: advertised}
			missing := support.Shortfall(scan.Required)
			if fixture.Refused && len(missing) == 0 {
				t.Fatal("preflight accepted a receiver without content_parts_v1; no upload may proceed")
			}
			if !fixture.Refused && len(missing) != 0 {
				t.Fatalf("preflight refused a capable receiver: %v", missing)
			}
			if fixture.Refused && support.UnsupportedReason() == "" {
				t.Fatal("refusal has no reason, so the caller cannot explain the zero-egress stop")
			}
			if content.SessionDetail == nil || len(content.SessionDetail.Turns) == 0 || len(content.SessionDetail.Turns[0].ContentParts) == 0 {
				return
			}
			localDetail := *content.SessionDetail
			localDetail.Turns = append([]schema.TurnDetail(nil), content.SessionDetail.Turns...)
			localDetail.Turns[0].ContentParts = append([]schema.TurnContentPart(nil), content.SessionDetail.Turns[0].ContentParts...)
			image := &schema.ImagePart{Ref: strings.Repeat("a", 64), MediaType: schema.ImageMediaTypePNG, ByteSize: 8}
			localDetail.Turns[0].ContentParts[0].Image = image
			localDetail.Turns[0].ContentParts[0].Type = schema.TurnPartTypeImage
			if _, err := push.ScanPublication(schema.TranscriptContent{SessionDetail: &localDetail}); err == nil {
				t.Fatal("preflight accepted a local image ref; publication must refuse every local field")
			}
			before := content.SessionDetail.Turns[0].ContentParts[0].Image
			if _, err := push.BuildPublishTranscriptContent(content.SessionDetail, meta, entries, defaults.PublishSchemaVersion, config.PushFieldVisibility{}, sessionorigin.Unknown); err != nil {
				t.Fatalf("publish committed detail: %v", err)
			}
			if content.SessionDetail.Turns[0].ContentParts[0].Image != before || (before != nil && before.OmittedReason != schema.OmittedReasonNotPublished) {
				t.Fatal("publication rewrote the caller's detail")
			}
			if fixture.Earlier {
				assertEarlierHistoryImageOmitted(t, meta, entries)
			}
		})
	}
}

func assertEarlierHistoryImageOmitted(t *testing.T, meta *ingest.UnifiedMetadata, entries []schema.SessionEntry) {
	t.Helper()
	localRef := strings.Repeat("c", 64)
	local := &schema.ImagePart{Ref: localRef, MediaType: schema.ImageMediaTypePNG, ByteSize: 9, Width: 2, Height: 3}
	detail := &schema.SessionDetailPayload{
		Turns: []schema.TurnDetail{{Role: schema.RoleUser, Content: "look"}},
		EarlierHistory: []schema.EarlierHistorySection{{
			State: schema.EarlierHistoryUncertainMigrated,
			Turns: []schema.TurnDetail{{
				Role: schema.RoleUser,
				ContentParts: []schema.TurnContentPart{{
					Type:  schema.TurnPartTypeImage,
					Image: local,
				}},
			}},
		}},
	}
	published, err := push.BuildPublishTranscriptContent(detail, meta, entries, defaults.PublishSchemaVersion, config.PushFieldVisibility{}, sessionorigin.Unknown)
	if err != nil {
		t.Fatalf("publish earlier-history image: %v", err)
	}
	if local.Ref != localRef || local.MediaType != schema.ImageMediaTypePNG || local.ByteSize != 9 {
		t.Fatal("publication rewrote the caller's earlier-history image")
	}
	if published.SessionDetail == nil || len(published.SessionDetail.EarlierHistory) != 1 || len(published.SessionDetail.EarlierHistory[0].Turns) != 1 {
		t.Fatal("published detail lost the earlier-history image part")
	}
	part := published.SessionDetail.EarlierHistory[0].Turns[0].ContentParts
	if len(part) != 1 || part[0].Type != schema.TurnPartTypeImage || part[0].Image == nil {
		t.Fatalf("earlier-history part = %+v, want one image part", part)
	}
	image := part[0].Image
	if image.OmittedReason != schema.OmittedReasonNotPublished || image.Ref != "" || image.MediaType != "" || image.ByteSize != 0 || image.Width != 0 || image.Height != 0 {
		t.Fatalf("earlier-history image = %+v, want not_published with no local fields", image)
	}
	encoded, err := json.Marshal(part[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{localRef, `"mediaType"`, `"byteSize"`, `"width"`, `"height"`, `"data"`, `"ref"`} {
		if strings.Contains(string(encoded), token) {
			t.Fatalf("earlier-history image JSON still carries %s: %s", token, encoded)
		}
	}
}

func imagePublicationEntries(images int) ([]schema.SessionEntry, []ingest.Turn) {
	ref := strings.Repeat("b", 64)
	extra := `{"ref":"` + ref + `","mediaType":"image/png","byteSize":8,"width":1,"height":1}`
	entries := []schema.SessionEntry{{
		SessionID: schema.SessionID(testutil.TestSessionUUID), Harness: schema.HarnessClaudeCode,
		EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: strPtr("look"), Extra: &extra,
	}}
	parts := make([]ingest.TurnPart, 0, images+1)
	parts = append(parts, ingest.TurnPart{Kind: ingest.TurnPartText, Text: "look"})
	for i := 0; i < images; i++ {
		parts = append(parts, ingest.TurnPart{Kind: ingest.TurnPartImage, ImageRef: ref, Image: &schema.ImagePart{Ref: ref, MediaType: schema.ImageMediaTypePNG, ByteSize: 8, Width: 1, Height: 1}})
		entries = append(entries, schema.SessionEntry{
			SessionID: schema.SessionID(testutil.TestSessionUUID), Harness: schema.HarnessClaudeCode,
			EntryIndex: i + 1, Depth: 1, ParentIndex: intPtr(0), EntryType: schema.EntryTypeImage, Role: schema.RoleUser, Extra: &extra,
		})
	}
	turns := []ingest.Turn{{Index: 0, Role: schema.RoleUser, Content: "look", Parts: parts, Timestamp: time.Unix(1, 0)}}
	return entries, turns
}

func imagePublicationMeta() *ingest.UnifiedMetadata {
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = schema.SessionID(testutil.TestSessionUUID)
	meta.SchemaVersion = ingest.CurrentSchemaVersion
	meta.ModelHarness = schema.HarnessClaudeCode
	meta.Model = "claude-opus-4-6"
	meta.Timestamp.Start, meta.Timestamp.End = 1, 2
	meta.Source.Format = schema.SourceFormatJSONL
	meta.Project.Hash = schema.ProjectHash(strings.Repeat("a", 64))
	meta.Project.Name = "widgets"
	return &meta
}

func containsCapability(list []schema.ContentCapability, want schema.ContentCapability) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func strPtr(value string) *string { return &value }
func intPtr(value int) *int       { return &value }
