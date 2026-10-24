package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_identity.yaml
var claudeIdentityYAML []byte

type claudeIdentityFixtures struct {
	RequiredCacheCorruptionNames []string                             `yaml:"required_cache_corruption_names"`
	CacheCorruptions             []claudeCacheCorruption              `yaml:"cache_corruptions"`
	RetainedExpectations         map[string]claudeRetainedExpectation `yaml:"retained_expectations"`
	RequiredCacheUpgradeNames    []string                             `yaml:"required_cache_upgrade_names"`
	CacheUpgrades                []claudeCacheUpgrade                 `yaml:"cache_upgrades"`
	RequiredCaseNames            []string                             `yaml:"required_case_names"`
	RequiredRetainedRefusalNames []string                             `yaml:"required_retained_refusal_names"`
	Cases                        []claudeIdentityCase                 `yaml:"cases"`
}

type claudeCacheCorruption struct {
	Name        string `yaml:"name"`
	RecordsJSON string `yaml:"records_json"`
}

type claudeRetainedExpectation struct {
	TargetState schema.RelationshipTargetState `yaml:"target_state"`
	Evidence    schema.EvidenceKind            `yaml:"evidence"`
	Anchor      *schema.PublicSourceAnchor     `yaml:"anchor"`
}

type claudeCacheUpgrade struct {
	Name         string `yaml:"name"`
	ProofVersion int    `yaml:"proof_version"`
}

// TestPipelineClaudeIdentityOwnership protects the discovery-to-scheduling-to-
// store boundary: adapter-only tests cannot observe a later parent overwrite.
func TestPipelineClaudeIdentityOwnership(t *testing.T) {
	for _, fixture := range LoadClaudeIdentityFixtures(t).Cases {
		if fixture.Refuse {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			root := t.TempDir()
			database := openEvidenceStore(t, filepath.Join(root, "peasant.db"))
			fs := testutil.NewCountingFS(testutil.NewMemFS())
			if fixture.NativeOnly {
				parent := makeMinimalMeta(t, fixture.Parent)
				if err := database.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: parent}}); err != nil {
					t.Fatal(err)
				}
			}
			writeClaudeEvidenceFiles(t, fs, fixture.Files)
			for _, file := range fixture.Files {
				fs.ModTimes[claudeEvidenceCacheRoot+"/"+file.Path] = time.Now().Add(-2 * time.Hour)
			}
			cfg := ingest.PipelineConfig{
				Sources: map[ingest.Harness]ingest.SourceConfig{
					ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{claudeEvidenceCacheRoot}, Enabled: true},
				},
				OutputDir: ingest.ResolvedPath(filepath.Join(root, "sync")), StalenessThreshold: time.Minute,
			}
			pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), ingest.DefaultAdapterRegistry, cfg,
				ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.Errors != 0 {
				t.Fatalf("harvest errors = %+v", result)
			}
			id := strings.TrimSuffix(filepath.Base(fixture.Artifact), ".jsonl")
			row, err := database.SessionByID(t.Context(), id)
			if err != nil || row == nil {
				t.Fatalf("stored artifact %s: row=%+v error=%v", id, row, err)
			}
			parent := ""
			if row.ParentID != nil {
				parent = *row.ParentID
			}
			if parent != fixture.Parent {
				t.Fatalf("stored parent = %q, want %q", parent, fixture.Parent)
			}
			sid, err := ingest.NewSessionID(id)
			if err != nil {
				t.Fatal(err)
			}
			locations, err := database.BulkLookupSessionLocations(t.Context(), []ingest.SessionID{sid})
			if err != nil {
				t.Fatal(err)
			}
			path := ingest.SessionMetadataPath(cfg.OutputDir.String(), string(locations[sid].HostSlug), id, fixture.Parent)
			if _, err := ingest.ReadManagedPair(fs, cfg.OutputDir.String(), path, sid); err != nil {
				t.Fatalf("stored ownership does not match installed pair: %v", err)
			}
			capture, found, err := database.GetSessionContentCapture(t.Context(), sid)
			wantStatus := fixture.CaptureStatus
			if wantStatus == "" {
				wantStatus = string(ingest.ContentCaptureComplete)
			}
			if err != nil || !found || string(capture.Status) != wantStatus {
				t.Fatalf("full capture: found=%t capture=%+v error=%v", found, capture, err)
			}
			if capture.CaptureFormat != ingest.ContentCaptureFormatFull || capture.FullCaptureSHA256 == "" || string(capture.FailureCode) != fixture.CaptureFailure {
				t.Fatalf("capture authority changed: %+v", capture)
			}
			if len(fixture.RetainedRefusals) == 0 {
				return
			}
			original, err := fs.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := database.ListEntries(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) == 0 {
				t.Fatal("initial native capture did not establish stored entries")
			}
			for _, file := range fixture.Files {
				if err := fs.Remove(claudeEvidenceCacheRoot + "/" + file.Path); err != nil {
					t.Fatal(err)
				}
			}
			for _, refusal := range fixture.RetainedRefusals {
				t.Run(refusal.Name, func(t *testing.T) {
					metadata := original
					if refusal.ClearDigest {
						var meta ingest.UnifiedMetadata
						if err := json.Unmarshal(original, &meta); err != nil {
							t.Fatal(err)
						}
						meta.ContentHash = ""
						meta.MetadataHash = schema.ComputeMetadataHash(&meta)
						metadata, err = json.Marshal(&meta)
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := fs.WriteFile(path, metadata, 0600); err != nil {
						t.Fatal(err)
					}
					transcriptPath := filepath.Join(filepath.Dir(path), id+"--transcript.jsonl")
					if err := fs.WriteFile(transcriptPath, []byte(refusal.Transcript), 0600); err != nil {
						t.Fatal(err)
					}
					versions := maps.Clone(ingest.HarvesterVersionRegistry)
					target := versions[ingest.HarnessClaudeCode]
					target.AdapterVersion++
					versions[ingest.HarnessClaudeCode] = target
					retainedCfg := cfg
					retainedCfg.Sources = nil
					retainedCfg.Reindex = true
					maintenance, err := newTestPipeline(fs, testutil.DefaultGitResolver(), ingest.DefaultAdapterRegistry, retainedCfg,
						ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithHarvesterVersions(versions), ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
					if err != nil {
						t.Fatal(err)
					}
					rejected, err := maintenance.Run(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if len(rejected.Diagnostics) == 0 {
						t.Fatal("mounted corrupt pair refusal was not reported")
					}
					if indexedOutcomeFor(rejected, sid) != ingest.IndexOutcomeError {
						t.Fatalf("mounted corrupt pair did not refuse indexing: %+v", rejected.IndexLog)
					}
					current, err := fs.ReadFile(path)
					if err != nil || !bytes.Equal(current, metadata) {
						t.Fatalf("corrupt pair metadata was replaced or stamped: %v", err)
					}
					currentEntries, err := database.ListEntries(t.Context(), sid)
					if err != nil || !reflect.DeepEqual(currentEntries, entries) {
						t.Fatalf("corrupt retained bytes replaced stored entries: %v", err)
					}
					currentCapture, found, err := database.GetSessionContentCapture(t.Context(), sid)
					if err != nil || !found || !reflect.DeepEqual(currentCapture, capture) {
						t.Fatalf("corrupt pair changed capture authority: %+v %v", currentCapture, err)
					}
				})
			}
		})
	}
}

type claudeIdentityCase struct {
	Name             string                  `yaml:"name"`
	Artifact         string                  `yaml:"artifact"`
	Parent           string                  `yaml:"parent"`
	Relationship     string                  `yaml:"relationship"`
	Target           string                  `yaml:"target"`
	Refuse           bool                    `yaml:"refuse"`
	Files            []claudeEvidenceFile    `yaml:"files"`
	CaptureStatus    string                  `yaml:"capture_status"`
	CaptureFailure   string                  `yaml:"capture_failure"`
	NativeOnly       bool                    `yaml:"native_only"`
	SkipRetained     bool                    `yaml:"skip_retained"`
	TargetState      string                  `yaml:"target_state"`
	RetainedRefusals []claudeRetainedRefusal `yaml:"retained_refusals"`
}

type claudeRetainedRefusal struct {
	Name        string `yaml:"name"`
	ClearDigest bool   `yaml:"clear_digest"`
	Transcript  string `yaml:"transcript"`
}

func LoadClaudeIdentityFixtures(t *testing.T) claudeIdentityFixtures {
	t.Helper()
	var fixtures claudeIdentityFixtures
	if err := yaml.Unmarshal(claudeIdentityYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	retainedNames := make(map[string]bool)
	cacheNames := make(map[string]bool)
	corruptionNames := make(map[string]bool)
	for _, corruption := range fixtures.CacheCorruptions {
		corruptionNames[corruption.Name] = true
	}
	if err := testutil.RequireFixtureNames("Claude cached identity", "corruption", fixtures.RequiredCacheCorruptionNames, corruptionNames); err != nil {
		t.Fatal(err)
	}
	for _, upgrade := range fixtures.CacheUpgrades {
		cacheNames[upgrade.Name] = true
	}
	if err := testutil.RequireFixtureNames("Claude cached identity", "upgrade", fixtures.RequiredCacheUpgradeNames, cacheNames); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures.Cases {
		names[fixture.Name] = true
		for _, refusal := range fixture.RetainedRefusals {
			retainedNames[refusal.Name] = true
		}
	}
	if err := testutil.RequireFixtureNames("Claude identity", "case", fixtures.RequiredCaseNames, names); err != nil {
		t.Fatal(err)
	}
	if err := testutil.RequireFixtureNames("Claude retained identity", "case", fixtures.RequiredRetainedRefusalNames, retainedNames); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func TestClaudeIdentityDiscoveryExtractionAndEvidence(t *testing.T) {
	fixtures := LoadClaudeIdentityFixtures(t)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := context.Background()
			fs := testutil.NewCountingFS(testutil.NewMemFS())
			writeClaudeEvidenceFiles(t, fs, fixture.Files)
			database := openEvidenceStore(t, filepath.Join(t.TempDir(), "peasant.db"))
			adapter := ingest.NewClaudeAdapter(fs, testutil.DefaultGitResolver(), salt.Salt{})
			adapter.SetClaudeEvidenceCache(database)
			cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{claudeEvidenceCacheRoot}, Enabled: true}
			sessions, err := adapter.Discover(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			var artifact *ingest.DiscoveredSession
			id := strings.TrimSuffix(filepath.Base(fixture.Artifact), ".jsonl")
			for i := range sessions {
				if sessions[i].SessionID.String() == id {
					if artifact != nil {
						t.Fatalf("duplicate artifact %s", id)
					}
					artifact = &sessions[i]
				}
			}
			if artifact == nil {
				t.Fatal("artifact not discovered")
			}
			if !fixture.Refuse {
				wantSource := claudeEvidenceCacheRoot + "/" + fixture.Artifact
				if artifact.SourcePath.String() != wantSource {
					t.Fatalf("discovered source = %q, want %q", artifact.SourcePath, wantSource)
				}
			}
			parent := ""
			if artifact.ParentUUID != nil {
				parent = artifact.ParentUUID.String()
			}
			if parent != fixture.Parent {
				t.Fatalf("discovered parent = %q, want %q", parent, fixture.Parent)
			}
			metadata, err := adapter.ExtractMetadata(ctx, *artifact)
			if fixture.Refuse {
				if err == nil {
					t.Fatal("substitution accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if metadata.SessionID != artifact.SessionID {
				t.Fatal("artifact identity changed")
			}
			if fixture.Relationship != "" {
				if len(metadata.Relationships) != 1 {
					t.Fatalf("relationships = %+v", metadata.Relationships)
				}
				relation := metadata.Relationships[0]
				target := ""
				if relation.TargetLocalID != nil {
					target = relation.TargetLocalID.String()
				}
				if string(relation.Kind) != fixture.Relationship || target != fixture.Target {
					t.Fatalf("provenance = %+v", relation)
				}
				if fixture.TargetState != "" && string(relation.TargetState) != fixture.TargetState {
					t.Fatalf("target state = %q, want %q", relation.TargetState, fixture.TargetState)
				}
				if fixture.Relationship == "context_from" && fixture.Target != "" && relation.Anchor == nil {
					t.Fatal("missing source anchor")
				}
			}
			// The real persisted cache must preserve discovery ownership without
			// a warm read, and legacy proof markers must cause exactly one re-mine.
			fs.ResetCounts()
			if _, err := adapter.Discover(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			if adapter.ReminedCount() != 0 {
				t.Fatal("unchanged proof re-mined")
			}
			for _, file := range fixture.Files {
				if fs.ReadCount(claudeEvidenceCacheRoot+"/"+file.Path) != 0 {
					t.Fatal("warm discovery read transcript")
				}
			}
			records, err := database.LoadClaudeEvidence(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, upgrade := range fixtures.CacheUpgrades {
				t.Run(upgrade.Name, func(t *testing.T) {
					record := records[artifact.SourcePath]
					record.ProofVersion = upgrade.ProofVersion
					if err := database.SaveClaudeEvidence(ctx, []ingest.ClaudeTranscriptEvidence{record}, nil); err != nil {
						t.Fatal(err)
					}
					if _, err := adapter.Discover(ctx, cfg); err != nil {
						t.Fatal(err)
					}
					if adapter.ReminedCount() != 1 {
						t.Fatalf("prior proof re-mines = %d", adapter.ReminedCount())
					}
					if _, err := adapter.Discover(ctx, cfg); err != nil {
						t.Fatal(err)
					}
					if adapter.ReminedCount() != 0 {
						t.Fatal("proof upgrade repeated")
					}
				})
			}
			data, err := fs.ReadFile(artifact.SourcePath.String())
			if err != nil {
				t.Fatal(err)
			}
			for _, corruption := range fixtures.CacheCorruptions {
				t.Run(corruption.Name, func(t *testing.T) {
					conn, err := database.Pool().Take(ctx)
					if err != nil {
						t.Fatal(err)
					}
					err = sqlitex.ExecuteTransient(conn, `UPDATE claude_transcript_evidence SET identity_records_json=? WHERE source_path=?`, &sqlitex.ExecOptions{Args: []any{corruption.RecordsJSON, artifact.SourcePath.String()}})
					database.Pool().Put(conn)
					if err != nil {
						t.Fatal(err)
					}
					cached, err := database.LoadClaudeEvidence(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if _, found := cached[artifact.SourcePath]; found {
						t.Fatal("corrupt session identity remained cached")
					}
					recovered, err := adapter.Discover(ctx, cfg)
					if err != nil {
						t.Fatal(err)
					}
					if adapter.ReminedCount() != 1 {
						t.Fatalf("corrupt identity re-mines = %d", adapter.ReminedCount())
					}
					var found bool
					for _, session := range recovered {
						if session.SessionID != artifact.SessionID {
							continue
						}
						found = true
						parent := ""
						if session.ParentUUID != nil {
							parent = session.ParentUUID.String()
						}
						if parent != fixture.Parent {
							t.Fatalf("recovered owner = %q, want %q", parent, fixture.Parent)
						}
					}
					if !found {
						t.Fatal("artifact disappeared after cache recovery")
					}
					fs.ResetCounts()
					if _, err := adapter.Discover(ctx, cfg); err != nil {
						t.Fatal(err)
					}
					if adapter.ReminedCount() != 0 {
						t.Fatal("recovered identity re-mined on warm discovery")
					}
					for _, file := range fixture.Files {
						if fs.ReadCount(claudeEvidenceCacheRoot+"/"+file.Path) != 0 {
							t.Fatal("warm discovery after corruption recovery read transcript")
						}
					}
				})
			}
			if fixture.SkipRetained {
				// The fork's retained proof requires the native snapshot; a
				// transcript-only pair is left to the pair-recovery follow-up.
				return
			}
			metadata.ContentHash = schema.ComputeTranscriptHash(data)
			for _, file := range fixture.Files {
				if err := fs.Remove(claudeEvidenceCacheRoot + "/" + file.Path); err != nil {
					t.Fatal(err)
				}
			}
			// Native files and their evidence rows may both rotate. Managed
			// bytes retain only their previously proved source relationships.
			if _, err := adapter.Discover(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			retained, err := adapter.ExtractMetadataFromTranscript(ctx, data, metadata)
			if err != nil {
				t.Fatalf("retained capture after native removal: %v", err)
			}
			if retained.SessionID != metadata.SessionID {
				t.Fatal("retained identity changed")
			}
			var expected []schema.SessionRelationship
			if fixture.Relationship != "" {
				key := fixture.Relationship
				if fixture.TargetState != "" {
					key = fixture.TargetState
				}
				want, ok := fixtures.RetainedExpectations[key]
				if !ok {
					t.Fatalf("missing retained expectation %q", key)
				}
				var target *schema.SessionID
				if fixture.Target != "" {
					id, err := ingest.NewSessionID(fixture.Target)
					if err != nil {
						t.Fatal(err)
					}
					target = &id
				}
				expected = []schema.SessionRelationship{{Kind: schema.SessionRelationshipKind(fixture.Relationship), TargetLocalID: target, TargetState: want.TargetState, Evidence: want.Evidence, Anchor: want.Anchor}}
			}
			if !reflect.DeepEqual(retained.Relationships, expected) {
				t.Fatalf("retained provenance = %+v, want %+v", retained.Relationships, expected)
			}
			for _, refusal := range fixture.RetainedRefusals {
				t.Run(refusal.Name, func(t *testing.T) {
					original := *metadata
					if refusal.ClearDigest {
						original.ContentHash = ""
					}
					if _, err := adapter.ExtractMetadataFromTranscript(ctx, []byte(refusal.Transcript), &original); err == nil {
						t.Fatal("unbound multi-source capture reused last-good provenance")
					}
				})
			}
		})
	}
}
