package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/sync_publication_mirrors.yaml
var syncPublicationMirrorsYAML []byte

type syncPublicationMirrorCase struct {
	Name         string `yaml:"name"`
	Managed      bool   `yaml:"managed"`
	Count        *int64 `yaml:"count"`
	Graph        bool   `yaml:"graph"`
	StaleCapture bool   `yaml:"staleCapture"`
}

func loadSyncPublicationMirrorFixtures(t *testing.T) []syncPublicationMirrorCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string                    `yaml:"requiredNames"`
		Cases         []syncPublicationMirrorCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(syncPublicationMirrorsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("blank or duplicate fixture name %q", c.Name)
		}
		seen[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("share review mirrors", "case", fixture.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// Exercise the production review producer over a real generation store, then
// request the mounted scan route that consumes it. Capture metadata deliberately
// differs from the generation, including stale values that must become absent.
func TestSyncReviewPublicationMirrors(t *testing.T) {
	for _, c := range loadSyncPublicationMirrorFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Setenv(defaults.EnvXDGConfigHome.String(), t.TempDir())
			db := seedSyncMirrorCase(t, c)
			cfg := config.BaseConfig()
			h := &syncHandler{store: db, config: cfg}
			raw, err := h.readReviewContent(t.Context(), testutil.TestSessionUUID, nil)
			if err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(strings.NewReader(raw))
			// Review uses the pre-upload metadata document (no content hash yet).
			var metadata struct {
				Stats    schema.SessionStats                 `json:"stats"`
				Identity schema.AuthoritativeSessionIdentity `json:"identity"`
			}
			if err := decoder.Decode(&metadata); err != nil {
				t.Fatal(err)
			}
			var envelope schema.TranscriptContent
			if err := decoder.Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatalf("extra review content: %v", err)
			}
			detail := envelope.SessionDetail
			if detail == nil {
				t.Fatal("review has no session detail")
			}
			if !reflect.DeepEqual(detail.InputSubmissionCount, c.Count) {
				t.Fatalf("detail count=%v, want %v", detail.InputSubmissionCount, c.Count)
			}
			if (detail.RootSessionID != nil) != c.Graph || (detail.Purpose != "") != c.Graph || (len(detail.Relationships) != 0) != c.Graph {
				t.Fatalf("detail graph does not match fixture: root=%v purpose=%s relationships=%v", detail.RootSessionID, detail.Purpose, detail.Relationships)
			}
			if !reflect.DeepEqual(metadata.Stats.InputSubmissionCount, detail.InputSubmissionCount) ||
				!reflect.DeepEqual(metadata.Identity.RootSessionID, detail.RootSessionID) || metadata.Identity.Purpose != detail.Purpose ||
				!reflect.DeepEqual(metadata.Identity.Relationships, detail.Relationships) {
				t.Errorf("review metadata mirrors differ from built detail: metadata=%+v detail=%+v", metadata, detail)
			}

			// Observe both occurrences of the count through the real redaction scan.
			// These keys are not in entry strings, so pre-scan entry redaction does
			// not affect the observation. Total counts raw matches before UI dedup.
			cfg.Redaction.CustomPatterns = []config.CustomPattern{{
				ID: "review-count", Category: config.CategoryProject,
				Pattern: `"inputSubmissionCount":\s*[0-9]+`, Replacement: "<COUNT>",
			}}
			ctx, cancel := context.WithCancel(t.Context())
			server := NewServer(ServerConfig{Port: 0, Store: db, Config: cfg})
			if err := server.Listen(ctx); err != nil {
				cancel()
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(ctx) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			response, err := http.Get("http://" + server.Addr().String() + defaults.RouteSyncRedactions.String() + "?session_id=" + testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("scan status=%d body=%s", response.StatusCode, body)
			}
			var scan groupedRedactionResponse
			if err := json.Unmarshal(body, &scan); err != nil {
				t.Fatal(err)
			}
			want := 0
			if c.Count != nil {
				want = 2
			}
			if scan.Total != want {
				t.Fatalf("scan matches=%d, want %d (one count per part); body=%s", scan.Total, want, body)
			}
		})
	}
}

func seedSyncMirrorCase(t *testing.T, c syncPublicationMirrorCase) *store.Store {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "mirror.db"), store.WithPoolSize(2), store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = testutil.TestSessionUUID
	meta.HostSlug = testutil.TestHostSlug
	meta.ModelHarness = defaults.HarnessClaudeCode
	meta.Model = testutil.TestModel
	ingested := int64(1700000060000)
	meta.Timestamp = ingest.TimestampInfo{Start: 1700000000000, End: ingested, Ingested: &ingested}
	meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "fixture"}
	meta.Source = ingest.SourceInfo{FilePath: "fixture.jsonl", Format: ingest.SourceFormatJSONL}
	meta.Stats = ingest.StatsInfo{TurnCount: 1}
	text := "recorded input"
	entries := []schema.SessionEntry{{SessionID: meta.SessionID, EntryIndex: 0, Harness: meta.ModelHarness, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text}}
	if !c.Managed {
		testutil.SeedReadyPublication(t, db, &meta, entries)
		return db
	}
	generationMeta := meta
	generationMeta.Stats.InputSubmissionCount = c.Count
	setGraph := func(m *schema.UnifiedMetadata) {
		rootID := m.SessionID
		m.RootSessionID = &rootID
		m.Purpose = schema.SessionPurposeInteraction
		m.Relationships = []schema.SessionRelationship{{Kind: schema.SessionRelationshipContextFrom, TargetState: schema.RelationshipTargetUnknown, Evidence: schema.EvidenceNativeTyped}}
	}
	if c.Graph {
		setGraph(&generationMeta)
	}
	if c.StaleCapture {
		count := int64(9)
		meta.Stats.InputSubmissionCount = &count
		setGraph(&meta)
	}
	storetest.SeedGenerationPublication(t, db, &meta, indexformat.V2{Generation: indexformat.Generation{
		ID: "g-review", Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: generationMeta, Main: indexformat.Partition{Entries: entries},
		SourceEvidenceDigest: strings.Repeat("a", 64),
	}}, nil)
	return db
}
