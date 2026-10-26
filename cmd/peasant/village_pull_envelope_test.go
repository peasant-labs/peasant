package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/village_pull_envelope.yaml
var villagePullEnvelopeYAML []byte

// TestVillagePullEnvelopeFixtures runs the production pull commands against a
// village double serving each supported pull envelope and asserts what lands
// on disk: the negotiated envelope, the stored owner, and the context header.
func TestVillagePullEnvelopeFixtures(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name             string                     `yaml:"name"`
			LegacyEnvelope   bool                       `yaml:"legacyEnvelope"`
			Owner            string                     `yaml:"owner"`
			Anonymous        bool                       `yaml:"anonymous"`
			ThenAnonymous    bool                       `yaml:"thenAnonymous"`
			WantEnvelope     schema.PushContractVersion `yaml:"wantEnvelope"`
			WantContextOwner string                     `yaml:"wantContextOwner"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(villagePullEnvelopeYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	const ownerID = "0a000000-0000-4000-8000-000000000001"
	single := schema.TranscriptID("aaaaaaaa-bbbb-cccc-dddd-000000000101")
	shared := schema.TranscriptID("aaaaaaaa-bbbb-cccc-dddd-000000000102")
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			server := newPullCollectiveDouble(t)
			server.ServeLegacyPullEnvelope(c.LegacyEnvelope)
			owner := schema.AnonymousVillageOwner()
			if !c.Anonymous {
				owner = testutil.PublicProfile(ownerID, c.Owner)
			}
			for _, id := range []schema.TranscriptID{single, shared} {
				server.SetPullTranscriptOwner(id, pullCollectiveTestBlob(t, id), nil, owner)
			}
			server.Share(shared, pullCollectiveAID, schema.VillageShareStatusApproved)
			writeCredsAt(t, dir, server.URL())

			pullBoth := func() {
				t.Helper()
				for _, args := range [][]string{
					{"transcripts", "pull", single.String()},
					{"transcripts", "pull", "--collective", pullCollectiveAName},
				} {
					if out, err := runVillage(t, dir, args...); err != nil {
						t.Fatalf("%v: %v (out=%q)", args, err, out)
					}
				}
			}
			pullBoth()
			if c.ThenAnonymous {
				for _, id := range []schema.TranscriptID{single, shared} {
					server.SetPullTranscriptOwner(id, pullCollectiveTestBlob(t, id), nil, schema.AnonymousVillageOwner())
				}
				pullBoth()
			}

			db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
			if err != nil {
				t.Fatal(err)
			}
			rows, err := db.ListPulledTranscripts(t.Context())
			_ = db.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 {
				t.Fatalf("pulled %d transcripts, want 2", len(rows))
			}
			wantID, wantName := ownerID, c.Owner
			if c.Anonymous || c.ThenAnonymous {
				wantID, wantName = "", ""
			}
			for _, row := range rows {
				if row.OwnerUserID != wantID || row.OwnerUsername != wantName {
					t.Errorf("index owner of %s = %q/%q, want %q/%q", row.TranscriptID, row.OwnerUserID, row.OwnerUsername, wantID, wantName)
				}
				data, err := os.ReadFile(filepath.Join(row.PullDir, pull.ManifestFilename))
				if err != nil {
					t.Fatal(err)
				}
				var manifest pull.PullManifest
				if err := json.Unmarshal(data, &manifest); err != nil {
					t.Fatal(err)
				}
				if manifest.PullEnvelopeVersion != c.WantEnvelope {
					t.Errorf("manifest envelope of %s = %q, want %q", row.TranscriptID, manifest.PullEnvelopeVersion, c.WantEnvelope)
				}
				if manifest.OwnerUserID != wantID || manifest.OwnerUsername != wantName {
					t.Errorf("manifest owner of %s = %q/%q, want %q/%q", row.TranscriptID, manifest.OwnerUserID, manifest.OwnerUsername, wantID, wantName)
				}
			}

			out, err := runVillage(t, dir, "transcripts", "context", single.String())
			if err != nil {
				t.Fatalf("context: %v (out=%q)", err, out)
			}
			if !strings.Contains(out, c.WantContextOwner+"\n") {
				t.Errorf("context header does not show %q: %q", c.WantContextOwner, out)
			}

			out, err = runVillage(t, dir, "transcripts", "context", "--json", single.String())
			if err != nil {
				t.Fatalf("context --json: %v (out=%q)", err, out)
			}
			var context struct {
				OwnerUsername *string `json:"ownerUsername"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &context); err != nil {
				t.Fatalf("decode context --json: %v (out=%q)", err, out)
			}
			switch {
			case wantName == "" && context.OwnerUsername != nil:
				t.Errorf("context --json ownerUsername = %q, want null", *context.OwnerUsername)
			case wantName != "" && (context.OwnerUsername == nil || *context.OwnerUsername != wantName):
				t.Errorf("context --json ownerUsername = %v, want %q", context.OwnerUsername, wantName)
			}
		})
	}
}
