package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/image_blocks.yaml
var imageBlocksYAML []byte

//go:embed testdata/image_blocks.manifest.yaml
var imageBlocksManifestYAML []byte

type imageBlockFixture struct {
	Type      string `yaml:"type"`
	Text      string `yaml:"text"`
	MediaType string `yaml:"media_type"`
}

type imageBlockWant struct {
	Depth         int    `yaml:"depth"`
	EntryType     string `yaml:"entry_type"`
	PartType      string `yaml:"part_type"`
	Text          string `yaml:"text"`
	HasRef        bool   `yaml:"has_ref"`
	OmittedReason string `yaml:"omitted_reason"`
}

type imageBlockCase struct {
	Name   string              `yaml:"name"`
	Role   string              `yaml:"role"`
	Record string              `yaml:"record"`
	Blocks []imageBlockFixture `yaml:"blocks"`
	Want   []imageBlockWant    `yaml:"want"`
}

func TestImageBlocks(t *testing.T) {
	t.Parallel()
	var document struct {
		Cases []imageBlockCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(imageBlocksYAML, &document); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(imageBlocksManifestYAML, "image blocks")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, fixture := range document.Cases {
		names[i] = fixture.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "image blocks"); err != nil {
		t.Fatal(err)
	}
	pngData := base64.StdEncoding.EncodeToString(onePixelPNG(t))
	jpegData := base64.StdEncoding.EncodeToString(onePixelJPEG(t))
	for _, fixture := range document.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			line := imageBlockLine(t, fixture, pngData, jpegData)
			fs := testutil.NewMemFS()
			indexer := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true), ingest.WithClaudeFullContent(true))
			sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := indexer.IndexTranscriptBytes(context.Background(), ingest.DiscoveredSession{
				SessionID: sid, Harness: schema.HarnessClaudeCode, SourceFormat: ingest.SourceFormatJSONL,
			}, line)
			if err != nil {
				t.Fatalf("index image blocks: %v", err)
			}
			if len(entries) != len(fixture.Want) {
				t.Fatalf("got %d entries, want %d", len(entries), len(fixture.Want))
			}
			for i, want := range fixture.Want {
				got := entries[i]
				if got.Depth != want.Depth || got.EntryType.String() != want.EntryType {
					t.Fatalf("entry %d depth/type = %d/%s, want %d/%s", i, got.Depth, got.EntryType, want.Depth, want.EntryType)
				}
				if want.PartType != "" && (got.PartType == nil || *got.PartType != want.PartType) {
					t.Fatalf("entry %d part type = %v, want %s", i, got.PartType, want.PartType)
				}
				if want.Text != "" || (want.EntryType == "text" && want.Depth == 0 && fixture.Name == "image_only_keeps_empty_carrier") || (want.EntryType == "text" && want.Depth == 0 && want.Text == "" && fixture.Name == "webp_is_unsupported") {
					text := ""
					if got.ContentPreview != nil {
						text = *got.ContentPreview
					}
					if want.Text != "" && text != want.Text {
						t.Fatalf("entry %d text = %q, want %q", i, text, want.Text)
					}
				}
				if want.Text != "" && want.Depth == 1 {
					text := ""
					if got.ContentPreview != nil {
						text = *got.ContentPreview
					}
					if text != want.Text {
						t.Fatalf("entry %d text = %q, want %q", i, text, want.Text)
					}
				}
				if got.EntryType == schema.EntryTypeImage {
					var extra map[string]any
					if got.Extra == nil || json.Unmarshal([]byte(*got.Extra), &extra) != nil {
						t.Fatalf("entry %d has no image ref", i)
					}
					if len(extra) != 1 {
						t.Fatalf("entry %d extra = %v, want only the ref; media and size live on the attachment row", i, extra)
					}
					ref, _ := extra["ref"].(string)
					if want.HasRef && !imageContentRef(ref) {
						t.Fatalf("entry %d ref = %q, want img- plus a 64-hex content address", i, ref)
					}
					if !want.HasRef && ref != "" {
						t.Fatalf("entry %d ref = %q, want none", i, ref)
					}
					if want.OmittedReason != "" {
						text := ""
						if got.ContentPreview != nil {
							text = *got.ContentPreview
						}
						if !strings.Contains(text, "image omitted:") {
							t.Fatalf("entry %d preview = %q, want the omission note; the reason itself is on the attachment row", i, text)
						}
					}
					if _, ok := extra["data"]; ok || extra["mediaType"] != nil || extra["byteSize"] != nil {
						t.Fatal("image extra carries row metadata")
					}
				}
			}
		})
	}
}

func imageBlockLine(t *testing.T, fixture imageBlockCase, pngData, jpegData string) []byte {
	t.Helper()
	blocks := make([]any, 0, len(fixture.Blocks))
	for _, block := range fixture.Blocks {
		switch block.Type {
		case "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": block.Text})
		case "image":
			data := pngData
			if block.MediaType == "image/jpeg" {
				data = jpegData
			}
			if block.MediaType == "image/webp" || block.MediaType == "image/gif" {
				data = base64.StdEncoding.EncodeToString([]byte("not-an-image"))
			}
			blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": block.MediaType, "data": data}})
		}
	}
	record := fixture.Record
	if record == "" {
		record = fixture.Role
	}
	var line map[string]any
	if fixture.Record == "attachment" {
		source := blocks[0].(map[string]any)["source"]
		line = map[string]any{"type": "attachment", "attachment": map[string]any{"type": "image", "source": source}}
	} else if fixture.Record == "content_and_attachment" {
		content := blocks[0]
		attached := blocks[1].(map[string]any)["source"]
		line = map[string]any{
			"type":       fixture.Role,
			"message":    map[string]any{"role": fixture.Role, "content": []any{content}},
			"attachment": map[string]any{"type": "image", "source": attached},
		}
	} else {
		line = map[string]any{"type": record, "message": map[string]any{"role": fixture.Role, "content": blocks}}
	}
	encoded, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

func imageContentRef(ref string) bool {
	const prefix = "img-"
	if !strings.HasPrefix(ref, prefix) || len(ref) != len(prefix)+64 {
		return false
	}
	for _, r := range ref[len(prefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func onePixelJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 1, A: 255})
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
