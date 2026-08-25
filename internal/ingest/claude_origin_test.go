package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/origin/first_entry_shapes.yaml
var claudeOriginFixtureBytes []byte

const claudeOriginRoot = "/claude"

// requiredClaudeOriginCaseNames is the deletion guard. Each name is a shape the
// capture has to keep handling, so a removed row is refused rather than quietly
// reducing what discovery proves.
var requiredClaudeOriginCaseNames = []string{
	"structured_identity_recorded_far_past_the_hint_line_limit",
	"programmatic_launch_pair_recorded_far_past_the_hint_line_limit",
	"command_wrapper_as_a_bare_string_first_user_content",
	"command_wrapper_inside_the_first_user_content_blocks",
	"teammate_brief_opens_the_first_user_text",
	"plain_prose_first_user_text_resolves_unknown",
	"a_transcript_with_no_user_record_resolves_unknown",
	"structured_identity_outranks_a_command_wrapper_in_the_same_file",
	"only_the_first_user_record_is_read_for_the_prompt",
	"subagent_transcript_is_classified_agent",
	"leading_caveat_then_a_command_wrapper_is_a_person",
	"leading_caveat_then_a_teammate_brief_is_a_program",
	"a_caveat_with_nothing_after_it_stays_undeclared",
	"several_stacked_injected_records_are_all_read_past",
	"an_injected_record_after_a_real_one_is_not_read_past",
	"the_same_command_wrapper_without_the_leading_caveat",
	"a_command_wrapper_is_never_read_past",
	"a_longer_wrapper_name_is_not_the_injected_one",
}

type claudeOriginFixture struct {
	Cases []claudeOriginCase `yaml:"cases"`
}

type claudeOriginCase struct {
	Name     string                    `yaml:"name"`
	Arm      string                    `yaml:"arm"`
	Files    []claudeEvidenceFile      `yaml:"files"`
	Expected []claudeOriginExpectation `yaml:"expected"`
}

type claudeOriginExpectation struct {
	SessionID string `yaml:"session_id"`
	Origin    string `yaml:"origin"`
}

// LoadClaudeOriginFixtures decodes the capture corpus and refuses a fixture that
// has lost a required case, repeated a name, or declared an origin outside the
// production closed menu.
func LoadClaudeOriginFixtures(data []byte) (claudeOriginFixture, error) {
	var fixture claudeOriginFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return claudeOriginFixture{}, fmt.Errorf("decode Claude origin fixture first document: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return claudeOriginFixture{}, fmt.Errorf("Claude origin fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]struct{}, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" || tc.Arm == "" {
			return claudeOriginFixture{}, errors.New("Claude origin fixture holds a case with no name or no arm")
		}
		if _, duplicate := names[tc.Name]; duplicate {
			return claudeOriginFixture{}, fmt.Errorf("Claude origin fixture repeats case name %q", tc.Name)
		}
		names[tc.Name] = struct{}{}
		if len(tc.Expected) == 0 {
			return claudeOriginFixture{}, fmt.Errorf("Claude origin fixture case %q expects nothing, so it proves nothing", tc.Name)
		}
		for _, want := range tc.Expected {
			if _, err := sessionorigin.Parse(want.Origin); err != nil {
				return claudeOriginFixture{}, fmt.Errorf("Claude origin fixture case %q expects origin %q: %w", tc.Name, want.Origin, err)
			}
		}
	}
	for _, required := range requiredClaudeOriginCaseNames {
		if _, ok := names[required]; !ok {
			return claudeOriginFixture{}, fmt.Errorf("Claude origin fixture is missing required case %q", required)
		}
	}
	return fixture, nil
}

// claudeOriginCorpus is one fixture corpus written to a counting filesystem,
// together with the adapter that discovers it. Keeping the two together lets a
// test run discovery more than once over the SAME bytes, which is what a cache
// measurement needs.
type claudeOriginCorpus struct {
	fs      *testutil.CountingFS
	adapter *ingest.ClaudeAdapter
}

func newClaudeOriginCorpus(t *testing.T, tc claudeOriginCase, cache ingest.ClaudeEvidenceCache) claudeOriginCorpus {
	t.Helper()
	fs := testutil.NewCountingFS(testutil.NewMemFS())
	for _, file := range tc.Files {
		path := claudeOriginRoot + "/" + file.Path
		body := strings.Join(file.Lines, "\n") + "\n"
		if err := fs.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write transcript fixture %q: %v", path, err)
		}
	}
	adapter := ingest.NewClaudeAdapter(fs, testutil.DefaultGitResolver(), salt.Salt{})
	if cache != nil {
		ingest.AttachClaudeEvidenceCache(adapter, cache)
	}
	return claudeOriginCorpus{fs: fs, adapter: adapter}
}

// discover runs the production discovery and keys the result by identifier.
func (c claudeOriginCorpus) discover(t *testing.T) map[string]ingest.DiscoveredSession {
	t.Helper()
	cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{claudeOriginRoot}, Enabled: true}
	sessions, err := c.adapter.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	byID := make(map[string]ingest.DiscoveredSession, len(sessions))
	for _, session := range sessions {
		byID[string(session.SessionID)] = session
	}
	return byID
}

// discoverClaudeOriginCase discovers one fixture corpus once, with no cache.
func discoverClaudeOriginCase(t *testing.T, tc claudeOriginCase) map[string]ingest.DiscoveredSession {
	t.Helper()
	return newClaudeOriginCorpus(t, tc, nil).discover(t)
}

// TestClaudeDiscoveryDeclaresAnOrigin runs the real discovery over raw transcript
// bytes and checks the origin it declares for each session.
func TestClaudeDiscoveryDeclaresAnOrigin(t *testing.T) {
	fixture, err := LoadClaudeOriginFixtures(claudeOriginFixtureBytes)
	if err != nil {
		t.Fatalf("load Claude origin fixture: %v", err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			byID := discoverClaudeOriginCase(t, tc)
			for _, want := range tc.Expected {
				session, ok := byID[want.SessionID]
				if !ok {
					t.Fatalf("discovery did not find session %q", want.SessionID)
				}
				if session.Origin.String() != want.Origin {
					t.Errorf("session %q origin = %q, want %q", want.SessionID, session.Origin, want.Origin)
				}
				if err := session.Origin.Validate(); err != nil {
					t.Errorf("session %q carries an origin outside the menu: %v", want.SessionID, err)
				}
			}
		})
	}
}

// TestClaudeSubagentOriginAgreesWithTheRule checks that the subagent path asks
// the rule instead of answering for it. A hardcoded pair would pass a plain
// equality check against the literal agent, so the expected value is taken from
// the rule itself: change step one and this assertion moves with it.
func TestClaudeSubagentOriginAgreesWithTheRule(t *testing.T) {
	fixture, err := LoadClaudeOriginFixtures(claudeOriginFixtureBytes)
	if err != nil {
		t.Fatalf("load Claude origin fixture: %v", err)
	}
	wantOrigin, wantSignal := sessionorigin.Classify(sessionorigin.Evidence{HasParent: true})
	if wantSignal != sessionorigin.SignalParentLinked {
		t.Fatalf("a child transcript decided on signal %q, want parent-linked", wantSignal)
	}
	exercised := false
	for _, tc := range fixture.Cases {
		if tc.Arm != "subagent-classified-agent" {
			continue
		}
		exercised = true
		byID := discoverClaudeOriginCase(t, tc)
		for _, want := range tc.Expected {
			session, ok := byID[want.SessionID]
			if !ok {
				t.Fatalf("discovery did not find session %q", want.SessionID)
			}
			if session.ParentUUID == nil {
				continue
			}
			if session.Origin != wantOrigin {
				t.Errorf("subagent %q origin = %q, want the rule's answer %q", want.SessionID, session.Origin, wantOrigin)
			}
		}
	}
	if !exercised {
		t.Fatal("no subagent case was exercised, so this guard proved nothing")
	}
}

// fakeClaudeEvidenceCache keeps mined records verbatim in memory. It is the
// adapter's own cache contract with no storage layer underneath, which is what
// makes it useful here: it isolates the adapter's half of the round trip.
type fakeClaudeEvidenceCache struct {
	records map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence
}

func newFakeClaudeEvidenceCache() *fakeClaudeEvidenceCache {
	return &fakeClaudeEvidenceCache{records: make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence)}
}

func (c *fakeClaudeEvidenceCache) LoadClaudeEvidence(context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	loaded := make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, len(c.records))
	for path, record := range c.records {
		loaded[path] = record
	}
	return loaded, nil
}

func (c *fakeClaudeEvidenceCache) SaveClaudeEvidence(_ context.Context, upserts []ingest.ClaudeTranscriptEvidence, deletes []ingest.ResolvedPath) error {
	for _, record := range upserts {
		c.records[record.SourcePath] = record
	}
	for _, path := range deletes {
		delete(c.records, path)
	}
	return nil
}

// TestClaudeMinedOriginSurvivesTheCacheContract checks that a record handed to
// the cache carries the origin the miner decided, and that a session rebuilt
// from a cached record declares the same origin as one rebuilt from a fresh
// mine, for BOTH record scopes.
//
// The limit of this test, stated so nobody later mistakes it for more than it
// is: the cache here is an in-memory fake, so this proves the ADAPTER's half of
// the round trip only. It does NOT prove that the stored form keeps the origin.
// The SQL round trip is proven where the columns are.
func TestClaudeMinedOriginSurvivesTheCacheContract(t *testing.T) {
	fixture, err := LoadClaudeOriginFixtures(claudeOriginFixtureBytes)
	if err != nil {
		t.Fatalf("load Claude origin fixture: %v", err)
	}
	var subagentCase *claudeOriginCase
	for i := range fixture.Cases {
		if fixture.Cases[i].Arm == "subagent-classified-agent" {
			subagentCase = &fixture.Cases[i]
			break
		}
	}
	if subagentCase == nil {
		t.Fatal("the corpus carries no case holding both a root and a subagent transcript")
	}

	fresh := discoverClaudeOriginCase(t, *subagentCase)

	cache := newFakeClaudeEvidenceCache()
	corpus := newClaudeOriginCorpus(t, *subagentCase, cache)
	first := corpus.discover(t)
	if len(cache.records) == 0 {
		t.Fatal("the first discovery cached no record, so the round trip proves nothing")
	}
	if corpus.fs.TotalReads() == 0 {
		t.Fatal("the first discovery read no transcript, so the cache measurement proves nothing")
	}

	scopes := make(map[ingest.ClaudeEvidenceScope]struct{}, 2)
	paths := make([]string, 0, len(cache.records))
	for path, record := range cache.records {
		paths = append(paths, path.String())
		scopes[record.Scope] = struct{}{}
		if err := record.Origin.Validate(); err != nil {
			t.Errorf("cached record %q carries an origin outside the menu: %v", path, err)
		}
	}
	sort.Strings(paths)
	for _, scope := range []ingest.ClaudeEvidenceScope{ingest.ClaudeEvidenceScopeRoot, ingest.ClaudeEvidenceScopeSubagent} {
		if _, ok := scopes[scope]; !ok {
			t.Fatalf("no cached record carries scope %q, so that scope is unproven; cached paths %v", scope, paths)
		}
	}

	// The second discovery runs over the same unchanged bytes, so it must answer
	// from the cached records rather than from the files.
	corpus.fs.ResetCounts()
	second := corpus.discover(t)
	if reads := corpus.fs.TotalReads(); reads != 0 {
		t.Errorf("the second discovery read %d transcripts, want 0: the cached records were not reused, so the values compared below did not come from the cache", reads)
	}
	for id, want := range fresh {
		if got, ok := first[id]; !ok || got.Origin != want.Origin {
			t.Errorf("session %q from the caching run declared %q, want the freshly mined %q", id, got.Origin, want.Origin)
		}
		if got, ok := second[id]; !ok || got.Origin != want.Origin {
			t.Errorf("session %q rebuilt from the cache declared %q, want the freshly mined %q", id, got.Origin, want.Origin)
		}
	}
}

func TestLoadClaudeOriginFixturesRejectsADeletedCase(t *testing.T) {
	fixture, err := LoadClaudeOriginFixtures(claudeOriginFixtureBytes)
	if err != nil {
		t.Fatalf("load Claude origin fixture: %v", err)
	}
	var trimmed claudeOriginFixture
	trimmed.Cases = append(trimmed.Cases, fixture.Cases[1:]...)
	encoded, err := yaml.Marshal(trimmed)
	if err != nil {
		t.Fatalf("marshal trimmed fixture: %v", err)
	}
	if _, err := LoadClaudeOriginFixtures(encoded); err == nil {
		t.Fatal("loader accepted a fixture with a required case removed")
	} else if !strings.Contains(err.Error(), "missing required case") {
		t.Fatalf("loader refused for the wrong reason: %v", err)
	}
}
