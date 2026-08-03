package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

const fixtureIndexFile = "fixture-index.yaml"

type fixtureIndex struct {
	Path                 string                       `yaml:"-"`
	Root                 string                       `yaml:"-"`
	Harness              string                       `yaml:"harness"`
	SourcePath           string                       `yaml:"source_path"`
	IndexPath            string                       `yaml:"index_path,omitempty"`
	Sessions             []fixtureSession             `yaml:"sessions"`
	Slug                 *fixtureSlugPin              `yaml:"slug,omitempty"`
	Pinned               fixtureIndexPin              `yaml:"pinned"`
	AssociationRoundTrip *fixtureAssociationRoundTrip `yaml:"association_roundtrip,omitempty"`
}

type fixtureSlugPin struct {
	Workspace   string   `yaml:"workspace"`
	DecodedDir  string   `yaml:"decoded_dir"`
	DecodedName string   `yaml:"decoded_name,omitempty"`
	DirTree     []string `yaml:"dir_tree"`
}

type fixtureSession struct {
	ID       string `yaml:"id"`
	Model    string `yaml:"model,omitempty"`
	Kind     string `yaml:"kind"`
	ParentID string `yaml:"parent_id,omitempty"`
	Path     string `yaml:"path"`
}

type fixtureIndexPin struct {
	ScrubbedHome    string `yaml:"scrubbed_home"`
	SyntheticRemote bool   `yaml:"synthetic_remote"`
	CodeIdentifier  string `yaml:"code_identifier,omitempty"`
}

type fixtureAssociationRoundTrip struct {
	SessionID               string                       `yaml:"session_id"`
	ObservedCommitHash      string                       `yaml:"observed_commit_hash"`
	Subject                 string                       `yaml:"subject"`
	AuthorTime              int64                        `yaml:"author_time"`
	PushedAt                int64                        `yaml:"pushed_at"`
	Annotation              fixtureAssociationAnnotation `yaml:"annotation"`
	ExpectedAnnotationCount int                          `yaml:"expected_annotation_count"`
}

type fixtureAssociationAnnotation struct {
	TypeID    string `yaml:"type_id"`
	Value     string `yaml:"value"`
	Annotator string `yaml:"annotator"`
	Primary   bool   `yaml:"primary"`
}

func LoadFixtureIndex(path string) (*fixtureIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fixture index %s: %w", path, err)
	}
	var m fixtureIndex
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse fixture index %s: %w; fix the YAML schema or remove unknown fields", path, err)
	}
	var trailing any
	switch err := decoder.Decode(&trailing); err {
	case io.EOF:
	case nil:
		return nil, fmt.Errorf("fixture index %s contains a trailing YAML document; remove the extra document so the fixture contains exactly one YAML document", path)
	default:
		return nil, fmt.Errorf("parse trailing YAML in fixture index %s: %w; remove or repair the trailing YAML document", path, err)
	}
	m.Path = path
	m.Root = filepath.Dir(path)
	if err := validateFixtureIndex(&m); err != nil {
		return nil, fmt.Errorf("validate fixture index %s: %w", path, err)
	}
	return &m, nil
}

func validateFixtureIndex(m *fixtureIndex) error {
	if m.AssociationRoundTrip == nil {
		return nil
	}
	if m.Harness != schema.HarnessClaudeCode.String() {
		return fmt.Errorf("association_roundtrip is only supported on the claude-code fixture, got harness %q", m.Harness)
	}
	scenario := m.AssociationRoundTrip
	if scenario.SessionID != FixtureRootSessionID {
		return fmt.Errorf("association_roundtrip.session_id = %q, want root session %q", scenario.SessionID, FixtureRootSessionID)
	}
	if scenario.ObservedCommitHash != strings.Repeat("a", 40) {
		return fmt.Errorf("association_roundtrip.observed_commit_hash must be exactly 40 lowercase 'a' characters, got %q", scenario.ObservedCommitHash)
	}
	if strings.TrimSpace(scenario.Subject) == "" {
		return fmt.Errorf("association_roundtrip.subject is empty; provide the observed commit subject used by the durable association")
	}
	if scenario.AuthorTime <= 0 || scenario.PushedAt <= scenario.AuthorTime {
		return fmt.Errorf("association_roundtrip must define positive author_time and a later pushed_at, got author_time=%d pushed_at=%d", scenario.AuthorTime, scenario.PushedAt)
	}
	if scenario.Annotation.TypeID != "quality.session_outcome" || scenario.Annotation.Value != "resolved" || scenario.Annotation.Annotator != "outcome-classifier" || !scenario.Annotation.Primary {
		return fmt.Errorf("association_roundtrip.annotation must be type_id=quality.session_outcome, value=resolved, annotator=outcome-classifier, primary=true")
	}
	if scenario.ExpectedAnnotationCount != 1 {
		return fmt.Errorf("association_roundtrip.expected_annotation_count = %d, want 1 association annotation", scenario.ExpectedAnnotationCount)
	}
	return nil
}

func loadFixtureIndexes(t *testing.T) []*fixtureIndex {
	t.Helper()
	root := testdataRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read testdata root %s: %v", root, err)
	}
	var indexes []*fixtureIndex
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), fixtureIndexFile)
		m, err := LoadFixtureIndex(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Errorf("load fixture index for %q: %v", e.Name(), err)
			continue
		}
		indexes = append(indexes, m)
	}
	if len(indexes) == 0 {
		t.Fatalf("no fixture indexes under %s", root)
	}
	return indexes
}

func assertFixtureShape(t *testing.T, indexPath string) {
	t.Helper()
	m, err := LoadFixtureIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if m.Harness == "" {
		t.Errorf("%s: harness is empty", indexPath)
	}
	if m.Pinned.ScrubbedHome == "" {
		t.Errorf("%s: pinned.scrubbed_home is empty", indexPath)
	}
	if len(m.Sessions) == 0 {
		t.Fatalf("%s: sessions is empty", indexPath)
	}
	if m.IndexPath != "" {
		if _, err := os.Stat(filepath.Join(m.Root, m.IndexPath)); err != nil {
			t.Errorf("%s: index_path %q missing: %v", indexPath, m.IndexPath, err)
		}
	}
	if m.Harness == schema.HarnessCodex.String() {
		assertCodexIndexCoverage(t, m)
	}
	for _, s := range m.Sessions {
		assertIndexSession(t, m, s)
	}
}

func TestFixture_SlugDecodeInvariants(t *testing.T) {
	for _, m := range loadFixtureIndexes(t) {
		if m.Slug == nil {
			continue
		}
		t.Run(filepath.Base(m.Root), func(t *testing.T) {
			assertFixtureSlugDecode(t, m)
		})
	}
}

func assertFixtureSlugDecode(t *testing.T, m *fixtureIndex) {
	t.Helper()
	s := m.Slug
	if s.Workspace == "" {
		t.Fatalf("%s: slug.workspace is empty", m.Path)
	}
	if s.DecodedDir == "" {
		t.Fatalf("%s: slug.decoded_dir is empty", m.Path)
	}
	if len(s.DirTree) == 0 {
		t.Fatalf("%s: slug.dir_tree is empty", m.Path)
	}

	dirExists := dirExistsFromTree(s.DirTree)
	switch m.Harness {
	case schema.HarnessClaudeCode.String():
		got := ingest.DecodeClaudeSlug(s.Workspace, dirExists)
		if got != s.DecodedDir {
			t.Errorf("%s: DecodeClaudeSlug(%q) = %q, want %q", m.Path, s.Workspace, got, s.DecodedDir)
		}
	case schema.HarnessCursor.String():
		encoded := s.Workspace
		if !strings.HasPrefix(encoded, "-") {
			encoded = "-" + encoded
		}
		gotDir, gotName := ingest.DecodeCursorSlug(encoded, dirExists)
		if gotDir != s.DecodedDir {
			t.Errorf("%s: DecodeCursorSlug matched = %q, want %q", m.Path, gotDir, s.DecodedDir)
		}
		if s.DecodedName != "" && gotName != s.DecodedName {
			t.Errorf("%s: DecodeCursorSlug unmatched = %q, want %q", m.Path, gotName, s.DecodedName)
		}
	default:
		t.Errorf("%s: slug decode not defined for harness %q", m.Path, m.Harness)
	}
}

func dirExistsFromTree(paths []string) func(string) bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[filepath.Clean(p)] = true
	}
	return func(path string) bool {
		return set[filepath.Clean(path)]
	}
}

func TestFixtureIndex_CoverageFloors(t *testing.T) {
	indexes := loadFixtureIndexes(t)
	if len(indexes) < 2 {
		t.Errorf("fixture indexes = %d, want >= 2 provider fixture directories", len(indexes))
	}

	harnesses := map[string]int{}
	kinds := map[string]int{}
	totalSessions := 0
	for _, m := range indexes {
		harnesses[m.Harness]++
		for _, s := range m.Sessions {
			kinds[s.Kind]++
			totalSessions++
		}
	}
	if totalSessions < 3 {
		t.Errorf("index sessions = %d, want >= 3 across committed fixtures", totalSessions)
	}
	for _, h := range []string{schema.HarnessClaudeCode.String(), schema.HarnessCodex.String(), schema.HarnessCursor.String()} {
		if harnesses[h] == 0 {
			t.Errorf("fixture indexes have no %q harness", h)
		}
	}
	for _, kind := range []string{"claude-root", "claude-subagent", "tool-free", "tool-using", "cursor-root", "cursor-aborted"} {
		if kinds[kind] == 0 {
			t.Errorf("fixture indexes have no %q session kind", kind)
		}
	}
}

func TestFixtureIndex_AssociationRoundTrip(t *testing.T) {
	path := filepath.Join(FixtureSourcePath(), fixtureIndexFile)
	m, err := LoadFixtureIndex(path)
	if err != nil {
		t.Fatalf("load Claude fixture index: %v", err)
	}
	if m.AssociationRoundTrip == nil {
		t.Fatal("Claude fixture index has no association_roundtrip scenario")
	}
	if m.AssociationRoundTrip.SessionID != FixtureRootSessionID {
		t.Fatalf("association_roundtrip.session_id = %q, want %q", m.AssociationRoundTrip.SessionID, FixtureRootSessionID)
	}
}

func TestLoadFixtureIndex_RejectsUnknownField(t *testing.T) {
	path := filepath.Join(FixtureSourcePath(), fixtureIndexFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture index: %v", err)
	}
	data = append(data, []byte("\nunknown_fixture_field: true\n")...)
	tempPath := filepath.Join(t.TempDir(), fixtureIndexFile)
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		t.Fatalf("write temporary fixture index: %v", err)
	}

	_, err = LoadFixtureIndex(tempPath)
	if err == nil {
		t.Fatal("LoadFixtureIndex accepted an unknown field")
	}
	if !strings.Contains(err.Error(), "unknown_fixture_field") {
		t.Fatalf("LoadFixtureIndex error = %q, want unknown field name", err)
	}
}

func TestLoadFixtureIndex_RejectsTrailingDocument(t *testing.T) {
	path := filepath.Join(FixtureSourcePath(), fixtureIndexFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture index: %v", err)
	}
	data = append(data, []byte("\n---\n{}\n")...)
	tempPath := filepath.Join(t.TempDir(), fixtureIndexFile)
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		t.Fatalf("write temporary fixture index: %v", err)
	}

	_, err = LoadFixtureIndex(tempPath)
	if err == nil {
		t.Fatal("LoadFixtureIndex accepted a trailing YAML document")
	}
	if !strings.Contains(err.Error(), "trailing YAML document") {
		t.Fatalf("LoadFixtureIndex error = %q, want trailing document diagnostic", err)
	}
}

func assertIndexSession(t *testing.T, m *fixtureIndex, s fixtureSession) {
	t.Helper()
	if s.ID == "" {
		t.Errorf("%s: %s session has empty id", m.Path, s.Kind)
	}
	if s.Kind == "" {
		t.Errorf("%s: session %q has empty kind", m.Path, s.ID)
	}
	if s.Path == "" {
		t.Errorf("%s: session %q has empty path", m.Path, s.ID)
		return
	}
	path := filepath.Join(m.Root, s.Path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("%s: session file %q missing: %v", m.Path, s.Path, err)
		return
	}

	switch s.Kind {
	case "claude-root":
		if m.Pinned.CodeIdentifier == "" {
			t.Errorf("%s: claude-root session requires pinned.code_identifier", m.Path)
		} else if !strings.Contains(string(data), m.Pinned.CodeIdentifier) {
			t.Errorf("%s: pinned code identifier %q absent from %s", m.Path, m.Pinned.CodeIdentifier, s.Path)
		}
	case "claude-subagent":
		if s.ParentID == "" {
			t.Errorf("%s: claude-subagent %q has empty parent_id", m.Path, s.Path)
		} else if !strings.Contains(filepath.ToSlash(s.Path), "/"+s.ParentID+"/") {
			t.Errorf("%s: claude-subagent path %q is not nested under parent_id %q", m.Path, s.Path, s.ParentID)
		}
	case "tool-free", "tool-using":
		assertCodexRolloutSession(t, m, s, data)
	case "cursor-root":
		if !strings.Contains(string(data), `"role":"user"`) {
			t.Errorf("%s: cursor-root %q has no user turns", m.Path, s.Path)
		}
		if !strings.Contains(string(data), `"type":"tool_use"`) {
			t.Errorf("%s: cursor-root %q has no tool_use blocks (fixture must preserve full transcript)", m.Path, s.Path)
		}
	case "cursor-aborted":
		if !strings.Contains(string(data), `"type":"turn_ended"`) {
			t.Errorf("%s: cursor-aborted %q missing turn_ended marker", m.Path, s.Path)
		}
	default:
		t.Errorf("%s: session %q has unknown kind %q", m.Path, s.ID, s.Kind)
	}
}

func assertCodexIndexCoverage(t *testing.T, m *fixtureIndex) {
	t.Helper()
	sessionsDir := filepath.Join(m.Root, m.SourcePath)
	rollouts, err := filepath.Glob(filepath.Join(sessionsDir, "*", "*", "*", "rollout-*.jsonl"))
	if err != nil {
		t.Fatalf("%s: glob codex rollouts: %v", m.Path, err)
	}
	if len(rollouts) != len(m.Sessions) {
		t.Errorf("%s: codex rollout files = %d, index sessions = %d", m.Path, len(rollouts), len(m.Sessions))
	}
}

func assertCodexRolloutSession(t *testing.T, m *fixtureIndex, s fixtureSession, data []byte) {
	t.Helper()
	if s.Model == "" {
		t.Errorf("%s: codex session %q has empty model", m.Path, s.ID)
	}
	if !strings.Contains(filepath.Base(s.Path), s.ID) {
		t.Errorf("%s: codex rollout filename %q does not carry session id %q", m.Path, filepath.Base(s.Path), s.ID)
	}
	id, model := inspectIndexCodexRollout(t, filepath.Base(s.Path), data)
	if id != "" && id != s.ID {
		t.Errorf("%s: codex rollout %q session_meta id = %q, want %q", m.Path, s.Path, id, s.ID)
	}
	if model == "" {
		t.Errorf("%s: codex rollout %q has no turn_context with a non-empty model", m.Path, s.Path)
	} else if model != s.Model {
		t.Errorf("%s: codex rollout %q turn_context model = %q, want %q", m.Path, s.Path, model, s.Model)
	}
}

func inspectIndexCodexRollout(t *testing.T, name string, data []byte) (id, model string) {
	t.Helper()
	firstLine := ""
	for ln := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(ln) != "" {
			firstLine = ln
			break
		}
	}
	var env struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(firstLine), &env); err != nil {
		t.Errorf("codex rollout %q first line is not valid JSON: %v", name, err)
		return "", ""
	}
	if env.Type != "session_meta" {
		t.Errorf("codex rollout %q first line type = %q, want session_meta", name, env.Type)
		return "", ""
	}

	for ln := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var tc struct {
			Type    string `json:"type"`
			Payload struct {
				Model string `json:"model"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(ln), &tc); err != nil {
			continue
		}
		if tc.Type == "turn_context" && tc.Payload.Model != "" {
			model = tc.Payload.Model
			break
		}
	}
	return env.Payload.ID, model
}
