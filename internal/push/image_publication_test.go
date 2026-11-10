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
			for _, token := range forbidden {
				if strings.Contains(string(requestBody), token) {
					t.Fatalf("publication request still carries %q", token)
				}
				encoded, err := json.Marshal(content)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(encoded), token) && token != "image/png" {
					t.Fatalf("content document still carries %q", token)
				}
			}
			var request struct {
				Entries []schema.SessionEntry `json:"entries"`
			}
			if err := json.Unmarshal(requestBody, &request); err != nil {
				t.Fatal(err)
			}
			if len(request.Entries) == 0 {
				t.Fatal("publication request has no entries")
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
		})
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
