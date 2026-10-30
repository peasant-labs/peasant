package pull_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
)

//go:embed testdata/served-etag.yaml
var servedETagFixtureYAML []byte

type servedETagCase struct {
	Name              string `yaml:"name"`
	ETag              string `yaml:"etag"`
	WantPulled        bool   `yaml:"wantPulled"`
	WantErrorContains string `yaml:"wantErrorContains"`
	Repull            bool   `yaml:"repull"`
	WantIfNoneMatch   string `yaml:"wantIfNoneMatch"`
}

func loadServedETagCases(t *testing.T) []servedETagCase {
	t.Helper()
	d := yaml.NewDecoder(bytes.NewReader(servedETagFixtureYAML))
	d.KnownFields(true)
	var f struct {
		RequiredNames []string         `yaml:"requiredNames"`
		Cases         []servedETagCase `yaml:"cases"`
	}
	if err := d.Decode(&f); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range f.Cases {
		seen[c.Name] = true
	}
	for _, name := range f.RequiredNames {
		if !seen[name] {
			t.Fatalf("required case %q missing", name)
		}
	}
	return f.Cases
}

// TestPullTranscript_ServedETagFixtures pulls through the production pipeline
// with each ETag shape the content endpoint can answer with, including the
// weak form a compressing proxy produces.
func TestPullTranscript_ServedETagFixtures(t *testing.T) {
	wrong := strings.Repeat("0", 64)
	expand := func(s string) string {
		return strings.NewReplacer("{hash}", testServedHash, "{wrong}", wrong).Replace(s)
	}
	for _, c := range loadServedETagCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			st := &testutil.StubPullStore{}
			reader := happyReader()
			reader.ContentETag = expand(c.ETag)
			p := newPipeline(reader, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot)

			res, err := p.PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
			if !c.WantPulled {
				if err == nil || !strings.Contains(err.Error(), c.WantErrorContains) {
					t.Fatalf("err = %v, want it to contain %q", err, c.WantErrorContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("PullTranscript: %v", err)
			}
			if res.Status != pull.PullStatusPulled {
				t.Fatalf("status = %q, want %q", res.Status, pull.PullStatusPulled)
			}
			dir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
			raw, err := fs.ReadFile(filepath.Join(dir, pull.ManifestFilename))
			if err != nil {
				t.Fatal(err)
			}
			var manifest pull.PullManifest
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.ServedBlobHash != testServedHash {
				t.Fatalf("manifest served hash = %q, want the content hash %q", manifest.ServedBlobHash, testServedHash)
			}
			if c.Repull {
				// The server omits its metadata hash, which forces the
				// conditional GET, and answers 304 Not Modified.
				meta := happyMeta()
				meta.ContentHash = ""
				reader2 := &testutil.StubVillageReader{Meta: meta, ContentErr: village.ErrNotModified}
				res2, err := newPipeline(reader2, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).
					PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
				if err != nil {
					t.Fatalf("re-pull: %v", err)
				}
				if res2.Status != pull.PullStatusUpToDate {
					t.Fatalf("re-pull status = %q, want %q", res2.Status, pull.PullStatusUpToDate)
				}
				if got, want := reader2.LastIfNoneMatch, expand(c.WantIfNoneMatch); got != want {
					t.Fatalf("If-None-Match = %q, want the ETag verbatim %q", got, want)
				}
			}
		})
	}
}
