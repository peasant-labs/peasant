package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/strike_graph.yaml
var strikeGraphFixtureData []byte

const strikeGraphFixturePath = "internal/ingest/testdata/strike_graph.yaml"

type strikeGraphFixtures struct {
	Sessions []strikeGraphSessionFixture `yaml:"sessions"`
}

type strikeGraphSessionFixture struct {
	ID                        string `yaml:"id"`
	ParentID                  string `yaml:"parentId"`
	TranscriptModified        string `yaml:"transcriptModified"`
	SidecarModified           string `yaml:"sidecarModified"`
	ExpectedEffectiveModified string `yaml:"expectedEffectiveModified"`
	Cycle                     bool   `yaml:"cycle"`
}

func TestStrikeCanonicalProtocolWire(t *testing.T) {
	t.Parallel()

	fixtureDir := filepath.Join("testdata")
	transcript, err := os.ReadFile(filepath.Join(fixtureDir, "strike_protocol.jsonl"))
	if err != nil {
		t.Fatalf("read canonical Strike transcript fixture: %v", err)
	}
	sidecar, err := os.ReadFile(filepath.Join(fixtureDir, "strike_protocol.meta.json"))
	if err != nil {
		t.Fatalf("read canonical Strike sidecar fixture: %v", err)
	}

	sourceDir := t.TempDir()
	transcriptPath := filepath.Join(sourceDir, testutil.TestSessionUUID+".jsonl")
	if err := os.WriteFile(transcriptPath, transcript, 0o600); err != nil {
		t.Fatalf("write isolated canonical Strike transcript: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, testutil.TestSessionUUID+".meta.json"), sidecar, 0o600); err != nil {
		t.Fatalf("write isolated canonical Strike sidecar: %v", err)
	}

	resolvedSource, err := ingest.NewResolvedPath(sourceDir)
	if err != nil {
		t.Fatalf("resolve canonical Strike source: %v", err)
	}
	adapter := ingest.NewStrikeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
	discovered, err := adapter.Discover(context.Background(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{resolvedSource}, Enabled: true})
	if err != nil {
		t.Fatalf("discover canonical Strike fixture: %v", err)
	}
	if len(discovered) != 1 {
		t.Fatalf("discovered sessions = %d, want 1", len(discovered))
	}
	session := discovered[0]
	if session.Title != "canonical protocol title" {
		t.Errorf("discovered title = %q, want final session.titled value", session.Title)
	}
	if session.ProjectName != "strike-canonical-project" || session.CWD != "/tmp/strike-canonical-project" {
		t.Errorf("discovered project = (%q, %q), want canonical project key path", session.ProjectName, session.CWD)
	}
	if session.Branch != "fixture/canonical-protocol" {
		t.Errorf("discovered branch = %q, want worktreeBranch", session.Branch)
	}

	metadata, err := adapter.ExtractMetadata(context.Background(), session)
	if err != nil {
		t.Fatalf("extract canonical Strike metadata: %v", err)
	}
	if metadata.Model.String() != "gpt-5.6-sol" {
		t.Errorf("metadata model = %q, want model.selected value", metadata.Model)
	}
	if metadata.Stats.TokensIn != 0 || metadata.Stats.TokensOut != 3 {
		t.Errorf("metadata tokens = (%d, %d), want known counts (0, 3)", metadata.Stats.TokensIn, metadata.Stats.TokensOut)
	}

	entries, err := ingest.NewStrikeIndexer(&ingest.OSFileSystem{}, ingest.WithStrikeFullContent(true)).IndexTranscript(context.Background(), session)
	if err != nil {
		t.Fatalf("index canonical Strike transcript: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("indexed entries = %d, want user, assistant, tool use, and tool result", len(entries))
	}
	parent := entries[1]
	if parent.ContentPreview == nil || *parent.ContentPreview != "internal thought\nanswer" || !parent.HasThinking {
		t.Errorf("assistant parent = %+v, want source-ordered reasoning and text", parent)
	}
	if parent.TokensIn == nil || *parent.TokensIn != 0 || parent.TokensOut == nil || *parent.TokensOut != 3 {
		t.Errorf("assistant tokens = (%v, %v), want known counts (0, 3)", parent.TokensIn, parent.TokensOut)
	}
	if parent.StopReason == nil || *parent.StopReason != schema.StopReasonEndTurn {
		t.Errorf("assistant stop reason = %v, want end_turn", parent.StopReason)
	}
	toolUse := entries[2]
	if toolUse.ToolInput == nil || *toolUse.ToolInput != `{"path":"README.md","content":"replacement"}` {
		t.Errorf("tool input = %v, want complete canonical args", toolUse.ToolInput)
	}
	toolResult := entries[3]
	if toolResult.ToolOutput == nil || *toolResult.ToolOutput != "stream chunk\nprocess chunk\nfinal output" {
		t.Errorf("tool output = %v, want stream, process, then final output", toolResult.ToolOutput)
	}
	if !toolResult.IsError {
		t.Error("tool result IsError = false, want timeout process status retained")
	}
	foundMalformedData := false
	for _, warning := range metadata.Diagnostics.Warnings {
		if warning.ErrorType == "event_data_parse_error" {
			foundMalformedData = strings.Contains(warning.Location, "line 14") && warning.Remediation != ""
		}
	}
	if !foundMalformedData {
		t.Errorf("metadata warnings = %+v, want actionable malformed event-data diagnostic", metadata.Diagnostics.Warnings)
	}
}

func TestStrikeAdapterRetainsTranscriptWithMalformedSidecar(t *testing.T) {
	t.Parallel()

	fixturePath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "peasant", "testdata", "strike", "20260728T123456.123456789Z-AAAAAAAAAAAAAAAAAAAAAAAAAA.jsonl"))
	if err != nil {
		t.Fatalf("resolve shared Strike fixture: %v", err)
	}
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read shared Strike fixture: %v", err)
	}

	sourceDir := t.TempDir()
	transcriptPath := filepath.Join(sourceDir, filepath.Base(fixturePath))
	if err := os.WriteFile(transcriptPath, fixture, 0o600); err != nil {
		t.Fatalf("write isolated Strike transcript: %v", err)
	}
	sidecarPath := transcriptPath[:len(transcriptPath)-len(filepath.Ext(transcriptPath))] + ".meta.json"
	if err := os.WriteFile(sidecarPath, []byte("{"), 0o600); err != nil {
		t.Fatalf("write malformed Strike sidecar: %v", err)
	}

	resolvedSource, err := ingest.NewResolvedPath(sourceDir)
	if err != nil {
		t.Fatalf("resolve isolated Strike source: %v", err)
	}
	adapter := ingest.NewStrikeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
	discovered, err := adapter.Discover(context.Background(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{resolvedSource}, Enabled: true})
	if err != nil {
		t.Fatalf("discover Strike fixture with malformed sidecar: %v", err)
	}
	if len(discovered) != 1 {
		t.Fatalf("discovered sessions = %d, want 1 valid transcript retained", len(discovered))
	}
	if discovered[0].Title != "fixture root final title" {
		t.Errorf("discovered title = %q, want final session.titled value", discovered[0].Title)
	}

	metadata, err := adapter.ExtractMetadata(context.Background(), discovered[0])
	if err != nil {
		t.Fatalf("extract metadata with malformed sidecar: %v", err)
	}
	foundSidecarDiagnostic := false
	foundRecordDiagnostic := false
	for _, warning := range metadata.Diagnostics.Warnings {
		switch warning.ErrorType {
		case "sidecar_parse_error":
			foundSidecarDiagnostic = warning.Location == sidecarPath && warning.Remediation != ""
		case "parse_error":
			foundRecordDiagnostic = warning.Location != "" && warning.Remediation != ""
		}
	}
	if !foundSidecarDiagnostic || !foundRecordDiagnostic {
		t.Errorf("metadata warnings = %+v, want actionable sidecar and record diagnostics", metadata.Diagnostics.Warnings)
	}

	entries, err := ingest.NewStrikeIndexer(&ingest.OSFileSystem{}).IndexTranscript(context.Background(), discovered[0])
	if err != nil {
		t.Fatalf("index retained Strike transcript: %v", err)
	}
	depthZero := make(map[int]bool)
	for _, entry := range entries {
		if entry.Depth == 0 {
			depthZero[entry.EntryIndex] = true
		}
	}
	for _, entry := range entries {
		if entry.Depth != 1 {
			continue
		}
		if entry.ParentIndex == nil || !depthZero[*entry.ParentIndex] {
			t.Errorf("depth-1 entry %d has invalid parent %v", entry.EntryIndex, entry.ParentIndex)
		}
	}
}

func TestStrikeDiscoveryNormalizesGraphAndEffectiveModTime(t *testing.T) {
	t.Parallel()

	var fixtures strikeGraphFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(strikeGraphFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode committed fixture %s: %v", strikeGraphFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("committed fixture %s must contain exactly one YAML document, trailing decode: %v", strikeGraphFixturePath, err)
	}
	if len(fixtures.Sessions) != 6 {
		t.Fatalf("committed fixture %s must define six graph sessions, got %d", strikeGraphFixturePath, len(fixtures.Sessions))
	}

	sourceDir := t.TempDir()
	for _, row := range fixtures.Sessions {
		transcriptPath := filepath.Join(sourceDir, row.ID+".jsonl")
		if err := os.WriteFile(transcriptPath, []byte(`{"type":"user.message","time":"2026-07-28T10:00:00Z","data":{"text":"fixture"}}`+"\n"), 0o600); err != nil {
			t.Fatalf("write graph transcript %s: %v", row.ID, err)
		}
		sidecar, err := json.Marshal(map[string]string{"sessionId": row.ID, "parentSessionId": row.ParentID})
		if err != nil {
			t.Fatalf("marshal graph sidecar %s: %v", row.ID, err)
		}
		sidecarPath := filepath.Join(sourceDir, row.ID+".meta.json")
		if err := os.WriteFile(sidecarPath, sidecar, 0o600); err != nil {
			t.Fatalf("write graph sidecar %s: %v", row.ID, err)
		}
		transcriptModified, err := time.Parse(time.RFC3339, row.TranscriptModified)
		if err != nil {
			t.Fatalf("parse transcriptModified for %s: %v", row.ID, err)
		}
		sidecarModified, err := time.Parse(time.RFC3339, row.SidecarModified)
		if err != nil {
			t.Fatalf("parse sidecarModified for %s: %v", row.ID, err)
		}
		if err := os.Chtimes(transcriptPath, transcriptModified, transcriptModified); err != nil {
			t.Fatalf("set graph transcript mtime %s: %v", row.ID, err)
		}
		if err := os.Chtimes(sidecarPath, sidecarModified, sidecarModified); err != nil {
			t.Fatalf("set graph sidecar mtime %s: %v", row.ID, err)
		}
	}

	resolvedSource, err := ingest.NewResolvedPath(sourceDir)
	if err != nil {
		t.Fatalf("resolve graph fixture source: %v", err)
	}
	adapter := ingest.NewStrikeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
	discovered, err := adapter.Discover(context.Background(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{resolvedSource}, Enabled: true})
	if err != nil {
		t.Fatalf("discover graph fixture: %v", err)
	}
	if len(discovered) != len(fixtures.Sessions) {
		t.Fatalf("discovered graph sessions = %d, want %d", len(discovered), len(fixtures.Sessions))
	}

	positions := make(map[string]int, len(discovered))
	byID := make(map[string]ingest.DiscoveredSession, len(discovered))
	for position, session := range discovered {
		positions[session.SessionID.String()] = position
		byID[session.SessionID.String()] = session
	}
	for _, row := range fixtures.Sessions {
		session := byID[row.ID]
		if session.SessionID == "" {
			t.Fatalf("graph session %s was not discovered", row.ID)
		}
		if row.Cycle {
			if session.ParentUUID != nil {
				t.Errorf("cyclic session %s retained parent %v", row.ID, session.ParentUUID)
			}
			if len(session.DiscoveryWarnings) != 1 || session.DiscoveryWarnings[0].ErrorType != "invalid_parent_cycle" || session.DiscoveryWarnings[0].Remediation == "" {
				t.Errorf("cyclic session %s warnings = %+v, want one actionable cycle warning", row.ID, session.DiscoveryWarnings)
			}
		} else if row.ParentID != "" {
			if session.ParentUUID == nil || session.ParentUUID.String() != row.ParentID {
				t.Errorf("session %s parent = %v, want %s", row.ID, session.ParentUUID, row.ParentID)
			} else if positions[row.ParentID] >= positions[row.ID] {
				t.Errorf("session order places child %s before parent %s: %+v", row.ID, row.ParentID, positions)
			}
		}
		if row.ExpectedEffectiveModified != "" {
			want, err := time.Parse(time.RFC3339, row.ExpectedEffectiveModified)
			if err != nil {
				t.Fatalf("parse expectedEffectiveModified for %s: %v", row.ID, err)
			}
			if !session.ModTime.Equal(want) {
				t.Errorf("session %s effective mtime = %s, want %s", row.ID, session.ModTime, want)
			}
		}
	}
}
