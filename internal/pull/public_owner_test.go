package pull_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/public-owner.yaml
var publicOwnerFixtureYAML []byte

type publicOwnerFixtures struct {
	RequiredNames []string                    `yaml:"requiredNames"`
	Pull          []publicOwnerPullFixture    `yaml:"pull"`
	Refresh       []publicOwnerRefreshFixture `yaml:"refresh"`
}

type publicPersonFixture struct {
	Kind     schema.VillagePublicOwnerKind `yaml:"kind"`
	ID       schema.VillageUUID            `yaml:"id"`
	Username string                        `yaml:"username"`
}

// person builds the value a reader double hands the pipeline, including a kind
// the contract does not define, exactly as given.
func (p publicPersonFixture) person() schema.VillagePublicOwner {
	return schema.VillagePublicOwner{Kind: p.Kind, ID: p.ID, Username: p.Username}
}

type storedPersonFixture struct {
	ID       string `yaml:"id"`
	Username string `yaml:"username"`
}

type publicOwnerPullFixture struct {
	Name              string                     `yaml:"name"`
	Envelope          schema.PushContractVersion `yaml:"envelope"`
	PreviousOwner     *publicPersonFixture       `yaml:"previousOwner"`
	PreviousAuthors   []publicPersonFixture      `yaml:"previousAuthors"`
	Owner             publicPersonFixture        `yaml:"owner"`
	ProjectName       *string                    `yaml:"projectName"`
	Authors           []publicPersonFixture      `yaml:"authors"`
	AnnotationTargets bool                       `yaml:"annotationTargets"`
	WantStatus        pull.PullStatus            `yaml:"wantStatus"`
	WantOwner         storedPersonFixture        `yaml:"wantOwner"`
	WantAuthors       []storedPersonFixture      `yaml:"wantAuthors"`
	WantDownloads     int                        `yaml:"wantDownloads"`
	Forbidden         []string                   `yaml:"forbidden"`
}

type publicOwnerRefreshFixture struct {
	Name         string                `yaml:"name"`
	Owner        publicPersonFixture   `yaml:"owner"`
	Authors      []publicPersonFixture `yaml:"authors"`
	WantScanned  int                   `yaml:"wantScanned"`
	WantExcluded int                   `yaml:"wantExcluded"`
	WantAuthors  []storedPersonFixture `yaml:"wantAuthors"`
}

func loadPublicOwnerFixtures(t *testing.T) publicOwnerFixtures {
	t.Helper()
	var f publicOwnerFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(publicOwnerFixtureYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&f); err != nil {
		t.Fatalf("decode public owner fixtures: %v", err)
	}
	present := map[string]bool{}
	for _, c := range f.Pull {
		present[c.Name] = true
	}
	for _, c := range f.Refresh {
		present[c.Name] = true
	}
	for _, name := range f.RequiredNames {
		if !present[name] {
			t.Fatalf("public owner fixture %q is required but missing", name)
		}
	}
	return f
}

func authoredAnnotations(authors []publicPersonFixture) []schema.PullAnnotation {
	out := make([]schema.PullAnnotation, 0, len(authors))
	for i, author := range authors {
		a := foreignAnnotation(strings.Repeat(string(rune('a'+i)), 64))
		a.Author = author.person()
		out = append(out, a)
	}
	return out
}

// The manifest, the metadata snapshot and the local index keep a person's id
// and username only when the village shows this viewer their profile.
func TestPullTranscript_StoresPublicOwnerFixtures(t *testing.T) {
	for _, c := range loadPublicOwnerFixtures(t).Pull {
		t.Run(c.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			st := &testutil.StubPullStore{}
			reader := happyReader()
			reader.NegotiatedEnvelope = c.Envelope
			p := newPipeline(reader, fs, st, testutil.NewFixedClock(), testCreds(), testPullsRoot)

			if c.PreviousOwner != nil {
				reader.Meta.Owner = c.PreviousOwner.person()
				reader.Annotations = authoredAnnotations(c.PreviousAuthors)
				if _, err := p.PullTranscript(context.Background(), newRef(t), pull.PullOptions{}); err != nil {
					t.Fatalf("previous pull: %v", err)
				}
			}
			meta := *happyMeta()
			meta.Owner = c.Owner.person()
			if c.ProjectName != nil {
				meta.ProjectName = c.ProjectName
			}
			reader.Meta = &meta
			reader.Annotations = authoredAnnotations(c.Authors)
			if c.AnnotationTargets {
				for i := range reader.Annotations {
					hash, path := schema.ProjectHash("6f1ed002ab5595859014ebf0951522d9"), "private/secret-file.go"
					reader.Annotations[i].TargetProjectHash = &hash
					reader.Annotations[i].TargetFilePath = &path
				}
			}
			res, err := p.PullTranscript(context.Background(), newRef(t), pull.PullOptions{})
			if err != nil {
				t.Fatalf("PullTranscript: %v", err)
			}
			wantStatus := c.WantStatus
			if wantStatus == "" {
				wantStatus = pull.PullStatusPulled
			}
			if res.Status != wantStatus {
				t.Fatalf("status = %q, want %q", res.Status, wantStatus)
			}
			if want := max(c.WantDownloads, 1); reader.ContentCalls != want {
				t.Errorf("content downloads = %d, want %d", reader.ContentCalls, want)
			}

			dir := filepath.Join(testPullsRoot, testutil.TestVillageHost, testutil.TestTranscriptUUID)
			manifestBytes, err := fs.ReadFile(filepath.Join(dir, pull.ManifestFilename))
			if err != nil {
				t.Fatal(err)
			}
			var manifest pull.PullManifest
			if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.OwnerUserID != c.WantOwner.ID || manifest.OwnerUsername != c.WantOwner.Username {
				t.Errorf("manifest owner = %q/%q, want %q/%q", manifest.OwnerUserID, manifest.OwnerUsername, c.WantOwner.ID, c.WantOwner.Username)
			}
			wantEnvelope := c.Envelope
			if wantEnvelope == "" {
				wantEnvelope = defaults.PullContractVersion
			}
			if manifest.PullEnvelopeVersion != wantEnvelope {
				t.Errorf("manifest pull envelope = %q, want %q", manifest.PullEnvelopeVersion, wantEnvelope)
			}
			if len(manifest.Annotations) != len(c.WantAuthors) {
				t.Fatalf("manifest annotations = %d, want %d", len(manifest.Annotations), len(c.WantAuthors))
			}
			commit := st.Commits[len(st.Commits)-1]
			if res.Status == pull.PullStatusUpToDate {
				// No new install: the last commit is the previous pull's.
				if len(st.Commits) != 1 {
					t.Errorf("an up-to-date pull committed again: %d commits", len(st.Commits))
				}
			}
			if commit.Transcript.OwnerUserID != c.WantOwner.ID || commit.Transcript.OwnerUsername != c.WantOwner.Username {
				t.Errorf("index owner = %q/%q, want %q/%q", commit.Transcript.OwnerUserID, commit.Transcript.OwnerUsername, c.WantOwner.ID, c.WantOwner.Username)
			}
			for i, want := range c.WantAuthors {
				got := manifest.Annotations[i]
				if got.AuthorUserID != want.ID || got.AuthorUsername != want.Username {
					t.Errorf("manifest author %d = %q/%q, want %q/%q", i, got.AuthorUserID, got.AuthorUsername, want.ID, want.Username)
				}
				row := commit.Annotations[i]
				if row.AuthorUserID != want.ID || row.AuthorUsername != want.Username {
					t.Errorf("index author %d = %q/%q, want %q/%q", i, row.AuthorUserID, row.AuthorUsername, want.ID, want.Username)
				}
			}

			// The metadata snapshot reads back as the person the pipeline stored.
			metaBytes, err := fs.ReadFile(filepath.Join(dir, pull.MetadataFilename))
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := village.DecodePullTranscriptInfo(metaBytes)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Owner.IsProfile() != (c.WantOwner.ID != "") || snapshot.Owner.ID.String() != c.WantOwner.ID {
				t.Errorf("metadata snapshot owner = %+v, want id %q", snapshot.Owner, c.WantOwner.ID)
			}

			committed, err := json.Marshal(commit)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range c.Forbidden {
				for name, data := range map[string][]byte{"manifest": manifestBytes, "metadata": metaBytes, "index": committed} {
					if strings.Contains(string(data), secret) {
						t.Errorf("%s keeps %q, an identity the village no longer shows", name, secret)
					}
				}
			}
		})
	}
}

// The annotation refresh treats only the requester's own profile as theirs: an
// anonymous owner is never the requester, and an anonymous author stays foreign.
func TestRefreshOwnAnnotations_PublicOwnerFixtures(t *testing.T) {
	for _, c := range loadPublicOwnerFixtures(t).Refresh {
		t.Run(c.Name, func(t *testing.T) {
			st := &testutil.StubPullStore{}
			reader := &testutil.StubVillageReader{
				ListResponses: []*schema.PullListResponse{{
					Transcripts: []schema.PullTranscriptInfo{{TranscriptID: testutil.TestTranscriptID, LocalID: testutil.TestSessionUUID, Owner: c.Owner.person()}},
					Page:        1, Limit: 100, Total: 1,
				}},
				Annotations: authoredAnnotations(c.Authors),
			}
			p := newPipeline(reader, testutil.NewMemFS(), st, testutil.NewFixedClock(), testCreds(), testPullsRoot)
			res, err := p.RefreshOwnAnnotations(context.Background(), pull.RefreshOptions{})
			if err != nil {
				t.Fatalf("RefreshOwnAnnotations: %v", err)
			}
			if res.TranscriptsScanned != c.WantScanned || res.Excluded != c.WantExcluded {
				t.Errorf("scanned/excluded = %d/%d, want %d/%d", res.TranscriptsScanned, res.Excluded, c.WantScanned, c.WantExcluded)
			}
			var rows []store.PulledAnnotationRow
			for _, batch := range st.Upserts {
				rows = append(rows, batch...)
			}
			if len(rows) != len(c.WantAuthors) {
				t.Fatalf("stored rows = %d, want %d", len(rows), len(c.WantAuthors))
			}
			for i, want := range c.WantAuthors {
				if rows[i].AuthorUserID != want.ID || rows[i].AuthorUsername != want.Username {
					t.Errorf("row %d author = %q/%q, want %q/%q", i, rows[i].AuthorUserID, rows[i].AuthorUsername, want.ID, want.Username)
				}
			}
		})
	}
}
