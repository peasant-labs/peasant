package push_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/control_extra_image_strip.yaml
var controlExtraImageStripYAML []byte

//go:embed testdata/control_extra_image_strip.manifest.yaml
var controlExtraImageStripManifestYAML []byte

type controlExtraImageStripCase struct {
	Name             string            `yaml:"name"`
	Source           string            `yaml:"source"`
	ImageDataBytes   int               `yaml:"image_data_bytes"`
	WantExtraMembers map[string]string `yaml:"want_extra_members"`
	WantAbsent       string            `yaml:"want_absent"`
}

func loadControlExtraImageStripCases(t *testing.T) []controlExtraImageStripCase {
	t.Helper()
	var document struct {
		Cases []controlExtraImageStripCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(controlExtraImageStripYAML, &document); err != nil {
		t.Fatalf("decode control_extra_image_strip.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(controlExtraImageStripManifestYAML, "control extra image strip")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(document.Cases))
	for _, fixture := range document.Cases {
		names = append(names, fixture.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "control extra image strip"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// TestControlExtraImageStripAtPublication proves an embedded image attachment
// reaches the publication request without its base64 bytes. The stored Extra is
// asserted first, then the real metadata request builder is run over those
// entries; neither document may contain the source data, and both keep the
// media type and decoded byte size.
func TestControlExtraImageStripAtPublication(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadControlExtraImageStripCases(t) {
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
			path := "/transcripts/" + testutil.TestSessionUUID + ".jsonl"
			if err := fs.WriteFile(path, []byte(source), 0o644); err != nil {
				t.Fatal(err)
			}
			sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			session := ingest.DiscoveredSession{
				SessionID:    sid,
				Harness:      schema.HarnessClaudeCode,
				SourcePath:   ingest.ResolvedPath(path),
				SourceFormat: ingest.SourceFormatJSONL,
			}
			capture, err := ingest.NewClaudeIndexer(fs).IndexTranscriptBytesForCapture(context.Background(), session, []byte(source))
			if err != nil {
				t.Fatalf("capture refused the image attachment: %v", err)
			}
			stored := findAttachmentEntry(t, capture.Entries)
			assertNoImageBytes(t, "stored Extra", stored.Extra, fixture)

			redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			meta := ingest.NewUnifiedMetadata()
			meta.SessionID = testutil.TestSessionUUID
			meta.SchemaVersion = 9
			meta.ModelHarness = schema.HarnessClaudeCode
			meta.Model = "claude-opus-4-6"
			meta.Timestamp.Start, meta.Timestamp.End = 1, 2
			meta.Source.Format = schema.SourceFormatJSONL
			meta.Project.Hash = schema.ProjectHash(strings.Repeat("a", 64))
			meta.Project.Name = "widgets"
			payload, err := push.MapMetadata(push.MapOptions{
				Meta: &meta, Entries: capture.Entries, Fields: config.PushFieldVisibility{}.Resolve(), Redactor: redactor,
			})
			if err != nil {
				t.Fatalf("build the publication request: %v", err)
			}
			if strings.Contains(string(payload), fixture.WantAbsent) {
				t.Fatalf("publication request carries the image source data")
			}
			var request struct {
				Entries []schema.SessionEntry `json:"entries"`
			}
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Fatalf("publication request is not JSON: %v", err)
			}
			published := findAttachmentEntry(t, request.Entries)
			assertNoImageBytes(t, "published Extra", published.Extra, fixture)
		})
	}
}

func findAttachmentEntry(t *testing.T, entries []schema.SessionEntry) *schema.SessionEntry {
	t.Helper()
	for i := range entries {
		if entries[i].PartType != nil && *entries[i].PartType == "attachment" {
			return &entries[i]
		}
	}
	t.Fatal("no attachment entry")
	return nil
}

func assertNoImageBytes(t *testing.T, seam string, extra *string, fixture controlExtraImageStripCase) {
	t.Helper()
	if extra == nil {
		t.Fatalf("%s dropped the attachment payload", seam)
	}
	if strings.Contains(*extra, fixture.WantAbsent) {
		t.Fatalf("%s carries the image source data: %s", seam, *extra)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*extra), &got); err != nil {
		t.Fatalf("%s is not JSON: %v", seam, err)
	}
	for key, want := range fixture.WantExtraMembers {
		raw, ok := got[key]
		if !ok {
			t.Fatalf("%s lacks %q: %s", seam, key, *extra)
		}
		var gotValue, wantValue any
		if err := json.Unmarshal(raw, &gotValue); err != nil {
			t.Fatalf("%s[%q] is not JSON: %v", seam, key, err)
		}
		if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
			t.Fatalf("fixture want is not JSON: %v", err)
		}
		gotEncoded, _ := json.Marshal(gotValue)
		wantEncoded, _ := json.Marshal(wantValue)
		if string(gotEncoded) != string(wantEncoded) {
			t.Fatalf("%s[%q] = %s, want %s", seam, key, gotEncoded, wantEncoded)
		}
	}
}
