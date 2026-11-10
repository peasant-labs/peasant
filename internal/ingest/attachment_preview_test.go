package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/attachment_preview.yaml
var attachmentPreviewYAML []byte

//go:embed testdata/attachment_preview.manifest.yaml
var attachmentPreviewManifestYAML []byte

type attachmentPreviewCase struct {
	Name              string            `yaml:"name"`
	Source            string            `yaml:"source"`
	ImageDataBytes    int               `yaml:"image_data_bytes"`
	WantPartType      string            `yaml:"want_part_type"`
	WantPreview       string            `yaml:"want_preview"`
	WantPreviewAbsent bool              `yaml:"want_preview_absent"`
	WantPreviewPrefix string            `yaml:"want_preview_prefix"`
	WantPreviewMaxLen int               `yaml:"want_preview_max_len"`
	WantExtraKeys     []string          `yaml:"want_extra_keys"`
	WantExtraMembers  map[string]string `yaml:"want_extra_members"`
	WantExtraAbsent   []string          `yaml:"want_extra_absent"`
	WantAbsentText    string            `yaml:"want_absent_text"`
	WantRawBytesMin   int               `yaml:"want_raw_bytes_min"`
}

func loadAttachmentPreviewCases(t *testing.T) []attachmentPreviewCase {
	t.Helper()
	var document struct {
		Cases []attachmentPreviewCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(attachmentPreviewYAML, &document); err != nil {
		t.Fatalf("decode attachment_preview.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(attachmentPreviewManifestYAML, "attachment preview")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(document.Cases))
	for _, fixture := range document.Cases {
		names = append(names, fixture.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "attachment preview"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// TestAttachmentPreviewShape drives the production Claude Code capture path for
// every whitelisted attachment sub-kind and its fallbacks. The assertion is the
// observable entry a reader receives: the part type, the shape-only preview and
// the retained payload. Image bytes are absent from the payload; the media type
// and decoded size remain.
func TestAttachmentPreviewShape(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadAttachmentPreviewCases(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			source := fixture.Source
			if strings.Contains(source, "IMAGE_BYTES") {
				if fixture.ImageDataBytes == 0 || fixture.ImageDataBytes%4 != 0 {
					t.Fatalf("image_data_bytes must be a positive multiple of 4, got %d", fixture.ImageDataBytes)
				}
				source = strings.ReplaceAll(source, "IMAGE_BYTES", strings.Repeat("A", fixture.ImageDataBytes))
			}
			fs := testutil.NewMemFS()
			session := makeClaudeSession(t, fs, testutil.TestSessionUUID, source)
			indexer := ingest.NewClaudeIndexer(fs)
			capture, err := indexer.IndexTranscriptBytesForCapture(context.Background(), session, []byte(source))
			if err != nil {
				t.Fatalf("capture refused the attachment fixture: %v", err)
			}
			wantPartType := fixture.WantPartType
			if wantPartType == "" {
				wantPartType = "attachment"
			}
			entry := findControlEntry(capture.Entries, captureFixture{WantPartType: wantPartType})
			if entry == nil {
				t.Fatalf("capture produced no %q entry for %s", wantPartType, source)
			}
			assertAttachmentPreview(t, fixture, entry)
		})
	}
}

func assertAttachmentPreview(t *testing.T, fixture attachmentPreviewCase, entry *schema.SessionEntry) {
	t.Helper()
	if fixture.WantPreviewAbsent {
		if entry.ContentPreview != nil {
			t.Fatalf("preview = %q, want none", *entry.ContentPreview)
		}
	} else {
		if entry.ContentPreview == nil {
			t.Fatalf("preview absent, want %q", fixture.WantPreview)
		}
		preview := *entry.ContentPreview
		if fixture.WantPreviewPrefix != "" && !strings.HasPrefix(preview, fixture.WantPreviewPrefix) {
			t.Fatalf("preview = %q, want prefix %q", preview, fixture.WantPreviewPrefix)
		}
		if fixture.WantPreviewMaxLen > 0 && len(preview) > fixture.WantPreviewMaxLen {
			t.Fatalf("preview length = %d, want <= %d (%q)", len(preview), fixture.WantPreviewMaxLen, preview)
		}
		if fixture.WantPreviewPrefix == "" && fixture.WantPreviewMaxLen == 0 && preview != fixture.WantPreview {
			t.Fatalf("preview = %q, want %q", preview, fixture.WantPreview)
		}
	}
	if entry.Extra == nil {
		t.Fatal("attachment control lost its retained payload")
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*entry.Extra), &extra); err != nil {
		t.Fatalf("retained payload is not JSON: %v", err)
	}
	assertAttachmentExtra(t, fixture, entry, extra)
	// Every reader of Extra (the share scan, export, publication) runs the
	// strict evidence decode; a stored control payload must pass it.
	if _, err := ingest.RetainedUnknownOf(*entry); err != nil {
		t.Fatalf("the retained payload is refused by the strict Extra reader: %v", err)
	}
}

func assertAttachmentExtra(t *testing.T, fixture attachmentPreviewCase, entry *schema.SessionEntry, extra map[string]json.RawMessage) {
	t.Helper()
	for key, want := range fixture.WantExtraMembers {
		raw, ok := extra[key]
		if !ok {
			t.Fatalf("retained payload lacks %q: %s", key, *entry.Extra)
		}
		if got := canonicalJSONValue(t, string(raw)); got != canonicalJSONValue(t, want) {
			t.Fatalf("retained payload[%q] = %s, want %s", key, got, want)
		}
	}
	for _, key := range fixture.WantExtraAbsent {
		if _, ok := extra[key]; ok {
			t.Fatalf("retained payload leaks %q: %s", key, *entry.Extra)
		}
	}
	if len(fixture.WantExtraKeys) > 0 {
		got := make([]string, 0, len(extra))
		for key := range extra {
			got = append(got, key)
		}
		sort.Strings(got)
		want := append([]string(nil), fixture.WantExtraKeys...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("retained payload keys = %v, want %v (%s)", got, want, *entry.Extra)
		}
	}
	if fixture.WantAbsentText != "" && strings.Contains(*entry.Extra, fixture.WantAbsentText) {
		t.Fatalf("retained payload carries %q: %s", fixture.WantAbsentText, *entry.Extra)
	}
	if fixture.WantRawBytesMin > 0 {
		raw, ok := extra["rawBytes"]
		if !ok {
			t.Fatalf("capped payload lacks rawBytes: %s", *entry.Extra)
		}
		var bytes int
		if err := json.Unmarshal(raw, &bytes); err != nil || bytes < fixture.WantRawBytesMin {
			t.Fatalf("rawBytes = %s, want >= %d", raw, fixture.WantRawBytesMin)
		}
	}
}
