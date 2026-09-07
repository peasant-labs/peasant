package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pi_share_review.yaml
var piShareYAML []byte

//go:embed testdata/pi_share_review.manifest.yaml
var piShareManifest []byte

func TestPiShareReviewProductionRoute(t *testing.T) {
	var f struct {
		Cases []struct {
			Name    string `yaml:"name"`
			Data    string `yaml:"data"`
			Status  int    `yaml:"status"`
			Matches bool   `yaml:"matches"`
			Error   string `yaml:"error"`
		} `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(piShareYAML))
	d.KnownFields(true)
	if err := d.Decode(&f); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		t.Fatalf("trailing YAML: %v", err)
	}
	m, err := testutil.DecodeRequiredNamesManifest(piShareManifest, "Pi share review")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(f.Cases))
	for i, c := range f.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(m, names, "Pi share review"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx := context.Background()
			sid := schema.SessionID(testutil.TestSessionUUID)
			db := storetest.Open(t)
			storetest.SeedSession(t, db, string(sid))
			ingested := int64(3)
			meta := &ingest.UnifiedMetadata{SessionID: sid, ModelHarness: schema.HarnessPi, Model: "fixture-model", HostSlug: "testslug", Project: schema.ProjectContext{Hash: "testprojhash0000000000000000000000000000000000000000000000000000", Name: "testproj", FilePath: "/fixture"}, Timestamp: schema.TimestampInfo{Start: 1, End: 2, Ingested: &ingested}, Source: schema.SourceInfo{FilePath: "/fixture.jsonl", Format: schema.SourceFormatJSONL}}
			if err := db.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
				t.Fatal(err)
			}
			ref := ingest.PiPublicRef(string(sid), "entry", "custom")
			entry, err := ingest.NewPiCarrier(sid, 0, ingest.PiExtra{Kind: ingest.PiExtraCarrier, Harness: schema.HarnessPi, SourceRef: ref, Metadata: []schema.NativeMetadataRecord{{ID: ingest.PiPublicRef(string(sid), "metadata", "custom"), Kind: schema.NativeMetadataPiCustomData, Source: schema.NativeSourceRef{EntryRef: ref, SourceType: schema.NativeSourcePiCustom}, CustomType: "fixture", Data: json.RawMessage(c.Data)}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.IndexSessionEntries(ctx, sid, []schema.SessionEntry{entry}); err != nil {
				t.Fatal(err)
			}
			handler := &syncHandler{store: db, config: config.BaseConfig()}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/sync/redactions?session_id="+string(sid), nil)
			handler.handleSyncRedactions(response, request)
			if response.Code != c.Status {
				t.Fatalf("status=%d want %d body=%s", response.Code, c.Status, response.Body.String())
			}
			if c.Error != "" {
				if !strings.Contains(response.Body.String(), c.Error) {
					t.Fatalf("missing actionable collision: %s", response.Body.String())
				}
				return
			}
			var result groupedRedactionResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if (result.Total > 0) != c.Matches {
				t.Fatalf("matches=%d want present=%t response=%s", result.Total, c.Matches, response.Body.String())
			}
		})
	}
}
