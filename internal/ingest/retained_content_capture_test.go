package ingest

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/retained_content_capture.yaml
var retainedContentCaptureYAML []byte

type retainedContentCaptureFixtures struct {
	Required         []string `yaml:"required_names"`
	StrikeTranscript string   `yaml:"strike_transcript"`
	OpenCodeMessage  string   `yaml:"opencode_message"`
	OpenCodePart     string   `yaml:"opencode_part"`
	Cases            []struct {
		Name                 string `yaml:"name"`
		Harness              string `yaml:"harness"`
		ContentOmitted       bool   `yaml:"content_omitted"`
		MissingPartDirectory bool   `yaml:"missing_part_directory"`
		Complete             bool   `yaml:"complete"`
	} `yaml:"cases"`
}

func TestRetainedContentCaptureProducers(t *testing.T) {
	var fixtures retainedContentCaptureFixtures
	if err := yaml.Unmarshal(retainedContentCaptureYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate retained capture fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing retained capture fixture %s", name)
		}
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			var harness schema.Harness
			if err := harness.UnmarshalText([]byte(fixture.Harness)); err != nil || !harness.IsKnown() {
				t.Fatalf("fixture harness %q is not a known harness: %v", fixture.Harness, err)
			}
			root := t.TempDir()
			filesystem := &OSFileSystem{}
			var capturer RetainedContentCapturer
			var session DiscoveredSession
			switch harness {
			case HarnessStrike:
				sid, err := NewSessionID("11111111-1111-4111-8111-111111111111")
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, sid.String()+"--transcript.jsonl")
				if err := os.WriteFile(path, []byte(fixtures.StrikeTranscript), 0600); err != nil {
					t.Fatal(err)
				}
				session = DiscoveredSession{SessionID: sid, Harness: harness, SourcePath: ResolvedPath(path), SourceFormat: SourceFormatJSONL, ContentOmitted: fixture.ContentOmitted}
				capturer = NewStrikeIndexer(filesystem, WithStrikeFullContent(true))
			case HarnessOpenCode:
				sid, err := NewSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
				if err != nil {
					t.Fatal(err)
				}
				messagePath := filepath.Join(root, "storage", "message", sid.String(), "msg_1.json")
				files := map[string]string{messagePath: fixtures.OpenCodeMessage}
				if !fixture.MissingPartDirectory {
					files[filepath.Join(root, "storage", "part", "msg_1", "part_1.json")] = fixtures.OpenCodePart
				}
				for path, data := range files {
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
				session = DiscoveredSession{SessionID: sid, Harness: harness, OriginalRoot: ResolvedPath(root), SourcePath: ResolvedPath(root), TranscriptOrigin: TranscriptOriginFile}
				capturer = NewOpenCodeIndexer(filesystem, WithOpenCodeFullDepth(true))
			default:
				t.Fatalf("fixture harness %s has no retained capture producer under test", harness)
			}
			capture, err := capturer.CaptureRetainedContent(context.Background(), session)
			if err != nil {
				t.Fatalf("retained capture failed: %v", err)
			}
			if capture.Complete != fixture.Complete {
				t.Fatalf("complete = %t, want %t: %+v", capture.Complete, fixture.Complete, capture)
			}
			if capture.InputHash == "" {
				t.Fatal("retained capture reported no input identity")
			}
			if fixture.Complete && len(capture.Entries) == 0 {
				t.Fatal("complete capture carries no entries")
			}
			if !fixture.Complete && len(capture.Entries) != 0 {
				t.Fatalf("incomplete capture must not certify a surviving subset: %d entries", len(capture.Entries))
			}
		})
	}
}
