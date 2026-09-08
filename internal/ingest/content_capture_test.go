package ingest_test

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_capture.yaml
var contentCaptureFixtures []byte

type captureFixtureAdapter struct {
	ingest.SourceAdapter
	fs ingest.FileSystem
}

var _ ingest.TranscriptMaterializer = (*captureFixtureAdapter)(nil)

func (a *captureFixtureAdapter) MaterializeTranscript(ctx context.Context, s ingest.DiscoveredSession) (ingest.MaterializedTranscript, error) {
	meta, err := a.ExtractMetadata(ctx, s)
	if err != nil {
		return ingest.MaterializedTranscript{}, err
	}
	data, err := a.fs.ReadFile(s.SourcePath.String())
	fingerprint := sha256.Sum256(data)
	return ingest.MaterializedTranscript{Metadata: meta, Data: data, SourceFingerprint: fingerprint[:], EventSeq: s.EventSeq}, err
}

type captureFixture struct {
	Text           string         `yaml:"text"`
	Contains       bool           `yaml:"contains"`
	ToolOutput     bool           `yaml:"tool_output"`
	Omitted        bool           `yaml:"omitted"`
	ProjectionPart string         `yaml:"projection_part"`
	SecondTail     bool           `yaml:"second_tail"`
	Origin         string         `yaml:"origin"`
	Name           string         `yaml:"name"`
	Harness        ingest.Harness `yaml:"harness"`
	Source         string         `yaml:"source"`
	Reject         bool           `yaml:"reject"`
	WantError      bool           `yaml:"want_error"`
}

func captureFixtureSource(t *testing.T, fixture captureFixture, fs *testutil.MemFS, text string) (ingest.DiscoveredSession, []byte) {
	t.Helper()
	encoded, _ := json.Marshal(text)
	data := []byte(strings.ReplaceAll(fixture.Source, "TEXT", string(encoded)))
	if fixture.Origin != "" {
		data = managedProjectionWithPartText(t, testutil.TestSessionUUID, text)
		if fixture.ProjectionPart != "" {
			data = managedProjectionWithParts(t, testutil.TestSessionUUID, "assistant", []string{strings.ReplaceAll(fixture.ProjectionPart, "TEXT", string(encoded))})
		}
		if fixture.Origin == "current" {
			data = []byte(strings.ReplaceAll(string(data), "peasant.opencode.legacy-sqlite", "peasant.opencode.current-sqlite"))
		}
	}
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, string(data))
	session.Harness = fixture.Harness
	session.ContentOmitted = fixture.Omitted
	switch fixture.Origin {
	case "":
	case "legacy":
		session.TranscriptOrigin = ingest.TranscriptOriginOpenCodeLegacySQLite
		session.SourceFormat = ingest.SourceFormatJSON
	case "current":
		session.TranscriptOrigin = ingest.TranscriptOriginOpenCodeCurrentSQLite
		session.SourceFormat = ingest.SourceFormatJSON
	default:
		t.Fatalf("unknown capture origin %s", fixture.Origin)
	}
	return session, data
}

func loadCaptureFixtures(t *testing.T) []captureFixture {
	t.Helper()
	var document struct {
		Required []string         `yaml:"required_names"`
		Cases    []captureFixture `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentCaptureFixtures, &document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range document.Cases {
		if names[fixture.Name] {
			t.Fatalf("duplicate fixture %s", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range document.Required {
		if !names[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}
	return document.Cases
}

func TestAuthoritativeCaptureFileAndBytes(t *testing.T) {
	for _, fixture := range loadCaptureFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			text := strings.Repeat("界", 1000) + "SAFE_CAPTURE_TAIL"
			if fixture.Text != "" {
				text = fixture.Text
			}
			session, data := captureFixtureSource(t, fixture, fs, text)
			idx, ok := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})[fixture.Harness].(ingest.AuthoritativeTranscriptIndexer)
			if !ok {
				t.Fatalf("missing capture indexer for %s", fixture.Harness)
			}
			check := func(t *testing.T, result ingest.TranscriptCaptureResult, err error) {
				t.Helper()
				if fixture.Reject {
					if err == nil || len(result.Entries) != 0 {
						t.Fatal("malformed source certified as complete")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, entry := range result.Entries {
					value := entry.ContentPreview
					if fixture.ToolOutput {
						value = entry.ToolOutput
					}
					if value != nil && (*value == text || fixture.Contains && strings.Contains(*value, text)) {
						if fixture.WantError && (!entry.IsError || entry.EntryType != ingest.EntryTypeError || entry.Role != ingest.RoleSystem) {
							t.Fatal("recorded abort lost its error semantics")
						}
						found = true
					}
				}
				if !found {
					t.Fatal("full Unicode tail missing")
				}
			}
			t.Run("file", func(t *testing.T) {
				result, err := idx.IndexTranscriptForCapture(context.Background(), session)
				check(t, result, err)
			})
			t.Run("bytes", func(t *testing.T) {
				result, err := idx.IndexTranscriptBytesForCapture(context.Background(), session, data)
				check(t, result, err)
			})
		})
	}
}

func TestNormalIngestStoresAuthoritativeContent(t *testing.T) {
	for _, fixture := range loadCaptureFixtures(t) {
		if fixture.Reject {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			text := strings.Repeat("界", 1000) + "SAFE_CAPTURE_TAIL"
			if fixture.Text != "" {
				text = fixture.Text
			}
			session, _ := captureFixtureSource(t, fixture, fs, text)
			session.ModTime = time.Now().Add(-time.Hour)
			meta := makeMinimalMeta(t, session.SessionID.String())
			meta.Project.Hash = testutil.TestProjectHash
			meta.ModelHarness = fixture.Harness
			meta.Source.FilePath = session.SourcePath.String()
			meta.Source.Format = session.SourceFormat
			path := t.TempDir() + "/content.db"
			database, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Sources = map[ingest.Harness]ingest.SourceConfig{fixture.Harness: {Enabled: true, Paths: []ingest.ResolvedPath{session.SourcePath}}}
			adapters := map[ingest.Harness]ingest.AdapterFactory{fixture.Harness: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta})}
			if fixture.Origin != "" {
				base := adapters[fixture.Harness]
				adapters[fixture.Harness] = func(fs ingest.FileSystem, git ingest.GitResolver, salt salt.Salt) ingest.SourceAdapter {
					return &captureFixtureAdapter{SourceAdapter: base(fs, git, salt), fs: fs}
				}
			}
			pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})), ingest.WithStore(database), ingest.WithMetricsStore(database))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.StoreError != nil || result.Summary.Indexed != 1 {
				t.Fatalf("authoritative ingest failed: %+v", result)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			database, err = store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			page, err := database.ReadSessionEntries(context.Background(), session.SessionID, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range page.Entries {
				value := entry.ContentPreview
				if fixture.ToolOutput {
					value = entry.ToolOutput
				}
				if value != nil && (*value == text || fixture.Contains && strings.Contains(*value, text)) {
					found = true
				}
			}
			if !found {
				t.Fatal("durable full content missing after close/reopen")
			}
			if fixture.SecondTail {
				oldHash := page.Capture.FullCaptureSHA256
				text = strings.Repeat("界", 1000) + "CHANGED_CAPTURE_TAIL"
				captureFixtureSource(t, fixture, fs, text)
				cfg.Force = true
				pipeline, err = ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})), ingest.WithStore(database), ingest.WithMetricsStore(database))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pipeline.Run(context.Background()); err != nil {
					t.Fatal(err)
				}
				page, err = database.ReadSessionEntries(context.Background(), session.SessionID, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent})
				if err != nil {
					t.Fatal(err)
				}
				if page.Capture.FullCaptureSHA256 == oldHash || page.Entries[0].ContentPreview == nil || *page.Entries[0].ContentPreview != text {
					t.Fatal("same-prefix changed tail was incorrectly skipped")
				}
				cfg.Force = false
			}
			previews, err := database.ListEntries(context.Background(), session.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.IndexSessionEntries(context.Background(), session.SessionID, previews); err != nil {
				t.Fatal(err)
			}
			if err := fs.Remove(session.SourcePath.String()); err != nil {
				t.Fatal(err)
			}
			cfg.Reindex = true
			pipeline, err = ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})), ingest.WithStore(database), ingest.WithMetricsStore(database))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pipeline.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := fs.RemoveAll(testOutputDir); err != nil {
				t.Fatal(err)
			}
			page, err = database.ReadSessionEntries(context.Background(), session.SessionID, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent})
			if err != nil {
				t.Fatal(err)
			}
			if page.Capture.SourceAuthority != ingest.ContentSourcePeasantSnapshot {
				t.Fatalf("backfill authority = %s", page.Capture.SourceAuthority)
			}
			found = false
			for _, entry := range page.Entries {
				value := entry.ContentPreview
				if fixture.ToolOutput {
					value = entry.ToolOutput
				}
				if value != nil && (*value == text || fixture.Contains && strings.Contains(*value, text)) {
					found = true
				}
			}
			if !found {
				t.Fatal("full content unavailable after all sources removed")
			}
		})
	}
}

func TestOpenCodeCapturePreservesPreviewAnchors(t *testing.T) {
	fs := testutil.NewMemFS()
	session := setupOpenCodeFixture(t, fs, testutil.TestOpenCodeSesID, "project")
	text := strings.Repeat("界", 1000) + "SAFE_CAPTURE_TAIL"
	addOpenCodeMessageWithContent(t, fs, session.SessionID.String(), "msg_first", "assistant", text)
	addOpenCodeTextPart(t, fs, "msg_first", "part_first", text)
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullDepth(true))
	preview, err := idx.IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	full, err := idx.IndexTranscriptForCapture(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != len(full.Entries) {
		t.Fatalf("entry sequence changed: preview=%d full=%d", len(preview), len(full.Entries))
	}
	for i := range preview {
		if preview[i].EntryIndex != full.Entries[i].EntryIndex {
			t.Fatal("annotation anchor moved")
		}
		if full.Entries[i].ContentPreview == nil || *full.Entries[i].ContentPreview != text {
			t.Fatal("full duplicate content lost")
		}
	}
}

func TestNativeOpenCodeOmissionSurvivesManagedProjection(t *testing.T) {
	native := testfixture.MaterializeByName(t, "current-unknown-conversation-omission")
	root, err := ingest.NewResolvedPath(filepath.Dir(native.Path))
	if err != nil {
		t.Fatal(err)
	}
	adapter := newUnknownVocabularyAdapter(t)
	sessions, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("discover synthetic native source: %v (%d sessions)", err, len(sessions))
	}
	session := sessions[0]
	materialized, err := adapter.MaterializeTranscript(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	data := materialized.Data
	idx := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{})
	// The artifact alone must refuse certification, even without its sidecar.
	if _, err := idx.IndexTranscriptBytesForCapture(t.Context(), session, data); err == nil {
		t.Fatal("native omitted row certified from bytes")
	}
	path := filepath.Join(t.TempDir(), "retained.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	session.SourcePath, err = ingest.NewResolvedPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.IndexTranscriptForCapture(t.Context(), session); err == nil {
		t.Fatal("native omitted row certified from retained file")
	}
}
