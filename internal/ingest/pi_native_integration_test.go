package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pi_source_v3_structure.yaml
var piSourceFixtures []byte

//go:embed testdata/pi_sanitized_recording.yaml
var piSanitizedRecording []byte

func TestPiSanitizedNativeRecording(t *testing.T) {
	var fixture struct {
		Name   string `yaml:"name"`
		Source string `yaml:"source"`
	}
	if err := yaml.Unmarshal(piSanitizedRecording, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Name != "sanitized-native-recording" {
		t.Fatal("required sanitized recording missing")
	}
	sessionID, err := ingest.NewSessionID("11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{FullContent: true})[schema.HarnessPi].IndexTranscriptBytes(context.Background(), ingest.DiscoveredSession{SessionID: sessionID}, []byte(fixture.Source))
	if err != nil {
		t.Fatal(err)
	}
	projection, err := transcript.EntriesToProjectionValidated(entries, transcript.ProjectionOptions{Harness: schema.HarnessPi})
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Turns) == 0 || len(projection.UsageOwners) == 0 {
		t.Fatal("native recording lost its transcript or usage owners")
	}
}

type piSourceCase struct {
	Name                      string   `yaml:"name"`
	Source                    string   `yaml:"source"`
	Reject                    bool     `yaml:"reject"`
	ProjectionReject          bool     `yaml:"projectionReject"`
	MetadataStringBytes       int      `yaml:"metadataStringBytes"`
	PaddingStringBytes        int      `yaml:"paddingStringBytes"`
	SelectedMetadataBytes     int      `yaml:"selectedMetadataBytes"`
	AssertOrdinaryLongContent bool     `yaml:"assertOrdinaryLongContent"`
	RejectContains            string   `yaml:"rejectContains"`
	Title                     string   `yaml:"title"`
	MetadataModel             string   `yaml:"metadataModel"`
	Turns                     int      `yaml:"turns"`
	Owners                    int      `yaml:"owners"`
	Metadata                  int      `yaml:"metadata"`
	Warnings                  int      `yaml:"warnings"`
	Contains                  []string `yaml:"contains"`
	Excludes                  []string `yaml:"excludes"`
}

func assertPiSourceRejectionPipeline(t *testing.T, source ingest.ResolvedPath, body, wantReason string) {
	t.Helper()
	ctx := context.Background()
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	fs := &ingest.OSFileSystem{}
	indexers := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})
	_, err = indexers[schema.HarnessPi].IndexTranscriptBytes(ctx, ingest.DiscoveredSession{SessionID: sid}, []byte(body))
	if err == nil || !strings.Contains(err.Error(), wantReason) {
		t.Fatal("native index operation did not enforce the selected-metadata bound")
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	output, err := ingest.NewResolvedPath(filepath.Join(t.TempDir(), "managed"))
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{schema.HarnessPi: {Enabled: true, Paths: []ingest.ResolvedPath{source}}}, OutputDir: output, IncludeActive: true, Parallelism: 1,
	}, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexers(indexers))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.DiscoveryDiagnostics) != 1 || !strings.Contains(result.DiscoveryDiagnostics[0].Detail, wantReason) {
		t.Fatal("pipeline did not report the selected-metadata rejection")
	}
	stored, err := db.SessionSourceInfo(ctx, sid.String())
	if err != nil || stored != nil {
		t.Fatal("rejected metadata source created a session")
	}
	entries, err := db.ListEntries(ctx, sid)
	if err != nil || len(entries) != 0 {
		t.Fatal("rejected metadata source created indexed entries")
	}
	artifacts, err := filepath.Glob(filepath.Join(output.String(), "*", "*", "*"))
	if err != nil || len(artifacts) != 0 {
		t.Fatal("rejected metadata source created managed transcript artifacts")
	}
}

func TestPiNativeRegistryProjection(t *testing.T) {
	var fixture struct {
		RequiredNames []string       `yaml:"requiredNames"`
		Cases         []piSourceCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(piSourceFixtures)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, tc := range fixture.Cases {
		tc.Source = strings.ReplaceAll(tc.Source, "pi-native-fixture", testutil.TestSessionUUID)
		if tc.MetadataStringBytes > 0 {
			tc.Source = strings.ReplaceAll(tc.Source, "native-boundary-string", strings.Repeat("x", tc.MetadataStringBytes))
		}
		if tc.PaddingStringBytes > 0 {
			tc.Source = strings.ReplaceAll(tc.Source, "native-boundary-padding", strings.Repeat("p", tc.PaddingStringBytes))
		}
		if tc.Name == "" || seen[tc.Name] {
			t.Fatal("duplicate or empty fixture name")
		}
		seen[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "recording.jsonl")
			if err := os.WriteFile(path, []byte(tc.Source), 0600); err != nil {
				t.Fatal(err)
			}
			resolved, err := ingest.NewResolvedPath(path)
			if err != nil {
				t.Fatal(err)
			}
			fs := &ingest.OSFileSystem{}
			adapter := ingest.DefaultAdapterRegistry[schema.HarnessPi](fs, &testutil.StubGitResolver{}, salt.Salt{})
			sessions, err := adapter.Discover(ctx, ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{resolved}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.Reject {
				if tc.SelectedMetadataBytes > 0 {
					var result struct {
						Message struct {
							Details json.RawMessage `json:"details"`
						} `json:"message"`
					}
					lines := strings.Split(strings.TrimSpace(tc.Source), "\n")
					if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
						t.Fatal(err)
					}
					if len(result.Message.Details) != tc.SelectedMetadataBytes {
						t.Fatalf("synthetic selected subtree has %d bytes, want %d", len(result.Message.Details), tc.SelectedMetadataBytes)
					}
				}
				if len(sessions) != 0 {
					t.Fatal("invalid source accepted")
				}
				if len(adapter.(ingest.DiscoveryDiagnosticReporter).DiscoveryDiagnostics()) == 0 {
					t.Fatal("missing rejection diagnostic")
				}
				if tc.RejectContains != "" && !strings.Contains(adapter.(ingest.DiscoveryDiagnosticReporter).DiscoveryDiagnostics()[0].Detail, tc.RejectContains) {
					t.Fatal("source rejected for the wrong reason")
				}
				if tc.RejectContains != "" {
					assertPiSourceRejectionPipeline(t, resolved, tc.Source, tc.RejectContains)
				}
				return
			}
			if len(sessions) != 1 {
				t.Fatalf("discovered %d sessions: %+v", len(sessions), adapter.(ingest.DiscoveryDiagnosticReporter).DiscoveryDiagnostics())
			}
			session := sessions[0]
			if session.Title != tc.Title || session.CWD != "/synthetic/project" || session.ParentUUID != nil {
				t.Fatalf("wrong native identity: %+v", session)
			}
			if len(session.DiscoveryWarnings) != tc.Warnings {
				t.Fatalf("warnings: %+v", session.DiscoveryWarnings)
			}
			meta, err := adapter.ExtractMetadata(ctx, session)
			if err != nil {
				t.Fatal(err)
			}
			if string(meta.Model) != tc.MetadataModel {
				t.Fatalf("publication metadata model = %q, want first assistant observation %q", meta.Model, tc.MetadataModel)
			}
			indexer := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{FullContent: true})[schema.HarnessPi]
			entries, err := indexer.IndexTranscript(ctx, session)
			if err != nil {
				t.Fatal(err)
			}
			if tc.AssertOrdinaryLongContent {
				payload := strings.Repeat("x", tc.MetadataStringBytes)
				thinking, arguments, output, placeholders := false, false, false, 0
				for _, entry := range entries {
					if entry.ContentPreview != nil {
						placeholders += strings.Count(*entry.ContentPreview, "[image omitted]")
						if entry.Role == schema.RoleAssistant && entry.Depth == 0 && strings.Contains(*entry.ContentPreview, payload) && entry.HasThinking {
							thinking = true
						}
					}
					if entry.ToolInput != nil && strings.Contains(*entry.ToolInput, payload) {
						arguments = true
					}
					if entry.ToolOutput != nil && strings.Contains(*entry.ToolOutput, payload) {
						output = true
					}
				}
				if !thinking || !arguments || !output || placeholders != 4 {
					t.Fatal("ordinary long content was constrained by a selected-metadata budget or an image location lost its placeholder")
				}
			}
			dbPath := filepath.Join(t.TempDir(), "index.db")
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			output, err := ingest.NewResolvedPath(filepath.Join(t.TempDir(), "managed"))
			if err != nil {
				t.Fatal(err)
			}
			pipeline, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, ingest.PipelineConfig{
				Sources: map[ingest.Harness]ingest.SourceConfig{schema.HarnessPi: {Enabled: true, Paths: []ingest.ResolvedPath{resolved}}}, OutputDir: output, IncludeActive: true, Parallelism: 1,
			}, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithAnalyzer(metrics.NewEngine(db)), ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			entries, err = db.ListEntries(ctx, session.SessionID)
			if err != nil || len(entries) == 0 {
				t.Fatalf("pipeline did not persist entries: %v result=%+v", err, result)
			}
			if len(entries) == 0 {
				t.Fatal("valid session must retain a carrier or conversational row")
			}
			if tc.Name == "final-leaf-and-global-name" || tc.Name == "whitespace-clears-name" {
				computed, err := db.GetMetrics(ctx, session.SessionID)
				if err != nil || computed == nil || computed.TitleGenerated == nil || *computed.TitleGenerated != tc.Title {
					t.Fatalf("native title not retained: %+v (%v)", computed, err)
				}
			}
			projection, err := transcript.EntriesToProjectionValidated(entries, transcript.ProjectionOptions{Harness: schema.HarnessPi})
			if tc.ProjectionReject {
				if err != nil {
					t.Fatal(err)
				}
				_, err = transcript.SessionToDetailValidatedWithProjection(&ingest.Session{Harness: schema.HarnessPi}, projection)
				if err == nil {
					t.Fatal("unrepresentable separate namespace must not be silently dropped")
				}
				found := false
				for _, entry := range entries {
					extra, _, decodeErr := ingest.DecodePiEntryExtra(entry)
					if decodeErr != nil {
						t.Fatal(decodeErr)
					}
					if extra.Namespace == "native.extension" && entry.ToolNamesCSV != nil && *entry.ToolNamesCSV == "original_name" {
						found = true
					}
				}
				if !found {
					t.Fatal("native name and namespace were not stored separately")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(projection.Turns) != tc.Turns || len(projection.UsageOwners) != tc.Owners || len(projection.NativeMetadata) != tc.Metadata {
				t.Fatalf("projection: %d turns, %d owners, %d metadata", len(projection.Turns), len(projection.UsageOwners), len(projection.NativeMetadata))
			}
			detail, err := transcript.SessionToDetailValidatedWithProjection(&ingest.Session{Harness: schema.HarnessPi}, projection)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(detail)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range tc.Contains {
				if !strings.Contains(string(raw), text) {
					t.Errorf("missing %q", text)
				}
			}
			for _, text := range tc.Excludes {
				if strings.Contains(string(raw), text) {
					t.Errorf("unexpected %q", text)
				}
			}
			if strings.Contains(string(raw), "synthetic-image") || strings.Contains(string(raw), "private-fork") {
				t.Fatal("private source bytes leaked")
			}
			if tc.Name == "active-history" && strings.Count(string(raw), "think once") != 1 {
				t.Fatal("thinking must appear exactly once")
			}
			if tc.Name == "active-history" {
				assistant := detail.Turns[1]
				if assistant.ObservedModel != "observed-model" || !assistant.HasThinking || assistant.SourceEntryRef != ingest.PiPublicRef(session.SessionID.String(), "entry", "a") {
					t.Fatalf("assistant source/model/thinking attribution lost: %+v", assistant)
				}
				if assistant.Usage == nil || assistant.Usage.Completeness != schema.UsageComplete || assistant.Usage.Cost == nil || assistant.Usage.Cost.Total == nil || string(*assistant.Usage.Cost.Total) != "1e-7" {
					t.Fatal("native JS cost spelling or complete usage lost")
				}
				if len(assistant.ToolCalls) != 1 {
					t.Fatal("native tool did not survive")
				}
				tool := assistant.ToolCalls[0]
				if tool.ID != ingest.PiPublicRef(session.SessionID.String(), "tool", "call") || tool.CallEntryRef != assistant.SourceEntryRef || tool.ResultEntryRef != ingest.PiPublicRef(session.SessionID.String(), "entry", "r") || tool.Usage == nil || tool.Usage.Completeness != schema.UsageUnknown || !tool.IsError {
					t.Fatalf("tool source/result/unknown owner not preserved: %+v", tool)
				}
				if strings.Count(string(raw), "[image omitted]") != 4 {
					t.Fatal("all four native image locations must survive as placeholders")
				}
				for _, record := range detail.NativeMetadata {
					if record.Kind == schema.NativeMetadataPiToolResultDetails && (record.Attachment == nil || record.Attachment.ToolCallID != tool.ID || record.Source.EntryRef != tool.ResultEntryRef) {
						t.Fatal("tool metadata attached to wrong folded call")
					}
				}
			}
			original, err := os.ReadFile(path)
			if err != nil || string(original) != tc.Source {
				t.Fatal("source changed")
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Errorf("missing required fixture %q", name)
		}
	}
}
