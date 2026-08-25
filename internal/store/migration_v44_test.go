package store_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v44_claude_evidence.yaml
var claudeEvidenceFixture []byte

type claudeEvidenceFixtureFile struct {
	// RequiredRecordNames and RequiredRejectionNames are deletion-protection
	// manifests: every listed name must be present among Records /
	// Rejections respectively. They do not bound how many rows exist, so
	// adding a new record or rejection never requires touching them.
	RequiredRecordNames    []string                      `yaml:"required_record_names"`
	RequiredRejectionNames []string                      `yaml:"required_rejection_names"`
	Records                []claudeEvidenceFixtureRecord `yaml:"records"`
	Rejections             []claudeEvidenceRejection     `yaml:"rejections"`
}

type claudeEvidenceFixtureRecord struct {
	Name            string                      `yaml:"name"`
	SourcePath      string                      `yaml:"source_path"`
	Scope           ingest.ClaudeEvidenceScope  `yaml:"scope"`
	ModTimeUnixNano int64                       `yaml:"mod_time_unix_nano"`
	SizeBytes       int64                       `yaml:"size_bytes"`
	HasConversation bool                        `yaml:"has_conversation"`
	IdentityTeam    string                      `yaml:"identity_team"`
	IdentityName    string                      `yaml:"identity_name"`
	Spawns          []claudeEvidenceSpawnRecord `yaml:"spawns"`
	Title           string                      `yaml:"title"`
	Branch          string                      `yaml:"branch"`
	CWD             string                      `yaml:"cwd"`
}

type claudeEvidenceSpawnRecord struct {
	Team string `yaml:"team"`
	Name string `yaml:"name"`
}

type claudeEvidenceRejection struct {
	Name string `yaml:"name"`
	SQL  string `yaml:"sql"`
}

func loadClaudeEvidenceFixture(t *testing.T) claudeEvidenceFixtureFile {
	t.Helper()
	fixture, err := decodeClaudeEvidenceFixture(claudeEvidenceFixture)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func decodeClaudeEvidenceFixture(source []byte) (claudeEvidenceFixtureFile, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	var fixture claudeEvidenceFixtureFile
	if err := decoder.Decode(&fixture); err != nil {
		return claudeEvidenceFixtureFile{}, fmt.Errorf("decode Claude evidence fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return claudeEvidenceFixtureFile{}, fmt.Errorf("Claude evidence fixture must contain exactly one YAML document: %v", err)
	}
	if err := testutil.RequireFixtureNames("Claude evidence fixture", "record", fixture.RequiredRecordNames, recordNames(fixture.Records)); err != nil {
		return claudeEvidenceFixtureFile{}, err
	}
	if err := testutil.RequireFixtureNames("Claude evidence fixture", "rejection", fixture.RequiredRejectionNames, rejectionNames(fixture.Rejections)); err != nil {
		return claudeEvidenceFixtureFile{}, err
	}
	return fixture, nil
}

func recordNames(records []claudeEvidenceFixtureRecord) map[string]bool {
	names := make(map[string]bool, len(records))
	for _, record := range records {
		names[record.Name] = true
	}
	return names
}

func rejectionNames(rejections []claudeEvidenceRejection) map[string]bool {
	names := make(map[string]bool, len(rejections))
	for _, rejection := range rejections {
		names[rejection.Name] = true
	}
	return names
}

// toEvidence converts one fixture row into the discovery record the store writes.
func (r claudeEvidenceFixtureRecord) toEvidence() ingest.ClaudeTranscriptEvidence {
	record := ingest.ClaudeTranscriptEvidence{
		SourcePath:            ingest.ResolvedPath(r.SourcePath),
		Scope:                 r.Scope,
		ModTimeUnixNano:       r.ModTimeUnixNano,
		SizeBytes:             r.SizeBytes,
		HasConversationRecord: r.HasConversation,
		Title:                 r.Title,
		Branch:                r.Branch,
		CWD:                   r.CWD,
	}
	if r.IdentityTeam != "" && r.IdentityName != "" {
		record.Identity = &ingest.ClaudeTeammateIdentity{Team: r.IdentityTeam, Name: r.IdentityName}
	}
	for _, spawn := range r.Spawns {
		record.Spawns = append(record.Spawns, ingest.ClaudeTeammateIdentity{Team: spawn.Team, Name: spawn.Name})
	}
	return record
}

// TestMigrationV44ClaudeEvidenceRoundTrip verifies that the discovery evidence
// cache returns every mined value unchanged, that a delete removes a record,
// and that the closed constraints refuse a wrong row.
func TestMigrationV44ClaudeEvidenceRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := loadClaudeEvidenceFixture(t)
	s := openTestStore(t)

	upserts := make([]ingest.ClaudeTranscriptEvidence, 0, len(fixture.Records))
	for _, row := range fixture.Records {
		upserts = append(upserts, row.toEvidence())
	}
	if err := s.SaveClaudeEvidence(ctx, upserts, nil); err != nil {
		t.Fatalf("SaveClaudeEvidence: %v", err)
	}

	records, err := s.LoadClaudeEvidence(ctx)
	if err != nil {
		t.Fatalf("LoadClaudeEvidence: %v", err)
	}
	if len(records) != len(upserts) {
		t.Fatalf("LoadClaudeEvidence returned %d records, want %d", len(records), len(upserts))
	}
	for _, want := range upserts {
		got, ok := records[want.SourcePath]
		if !ok {
			t.Fatalf("transcript %q missing from the cache", want.SourcePath)
		}
		assertClaudeEvidenceEqual(t, got, want)
	}

	// A record whose transcript is gone must be removable.
	removed := upserts[0].SourcePath
	if err := s.SaveClaudeEvidence(ctx, nil, []ingest.ResolvedPath{removed}); err != nil {
		t.Fatalf("SaveClaudeEvidence delete: %v", err)
	}
	records, err = s.LoadClaudeEvidence(ctx)
	if err != nil {
		t.Fatalf("LoadClaudeEvidence after delete: %v", err)
	}
	if _, ok := records[removed]; ok {
		t.Errorf("transcript %q is still cached after a delete", removed)
	}
	if len(records) != len(upserts)-1 {
		t.Errorf("cache holds %d records after a delete, want %d", len(records), len(upserts)-1)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	for _, rejection := range fixture.Rejections {
		if err := sqlitex.ExecuteTransient(conn, rejection.SQL, nil); err == nil {
			t.Errorf("the table accepted %s, want a constraint failure", rejection.Name)
		}
	}
}

// TestClaudeEvidenceFixtureGuardsRequiredRowDeletion mutation-proves the
// required-name manifests: deleting a required record row, or a required
// rejection row, must fail the load with a message naming the missing row.
// This replaces the old declared_records/declared_rejections count guard,
// which would have also failed on any addition to the fixture.
func TestClaudeEvidenceFixtureGuardsRequiredRowDeletion(t *testing.T) {
	t.Parallel()

	// Baseline: the real, unmutated fixture must load cleanly first, so a
	// failure below is known to come from the mutation and not a broken
	// manifest.
	if _, err := decodeClaudeEvidenceFixture(claudeEvidenceFixture); err != nil {
		t.Fatalf("baseline fixture failed to decode before mutation: %v", err)
	}

	t.Run("deleted required record", func(t *testing.T) {
		t.Parallel()
		mutated := bytes.Replace(
			claudeEvidenceFixture,
			[]byte("  - name: subagent transcript carries only the conversation answer\n    source_path: /home/example/.claude/projects/-workspace/11111111-1111-4111-8111-111111111111/subagents/agent-a3aee4f.jsonl\n    scope: subagent\n    mod_time_unix_nano: 1750000000000000004\n    size_bytes: 512\n    has_conversation: true\n    spawns: []\n    title: \"\"\n    branch: \"\"\n    cwd: \"\"\n"),
			nil,
			1,
		)
		_, err := decodeClaudeEvidenceFixture(mutated)
		if err == nil {
			t.Fatal("fixture decoder accepted a corpus missing a required record row")
		}
		if !strings.Contains(err.Error(), `missing required record "subagent transcript carries only the conversation answer"`) {
			t.Fatalf("deleted-required-record error = %v, want it to name the missing record", err)
		}
	})

	t.Run("deleted required rejection", func(t *testing.T) {
		t.Parallel()
		mutated := bytes.Replace(
			claudeEvidenceFixture,
			[]byte("  - name: half an identity\n    sql: >-\n      INSERT INTO claude_transcript_evidence\n      (source_path, scope, mod_time_unix_nano, size_bytes, has_conversation, identity_team, spawns_json, title, branch, cwd)\n      VALUES ('/tmp/e.jsonl', 'root', 1, 1, 1, 'migration', '[]', '', '', '')\n"),
			nil,
			1,
		)
		_, err := decodeClaudeEvidenceFixture(mutated)
		if err == nil {
			t.Fatal("fixture decoder accepted a corpus missing a required rejection row")
		}
		if !strings.Contains(err.Error(), `missing required rejection "half an identity"`) {
			t.Fatalf("deleted-required-rejection error = %v, want it to name the missing rejection", err)
		}
	})
}

// TestClaudeEvidenceRejectsUnknownScope proves the write boundary fails closed
// on a scope this build does not know.
func TestClaudeEvidenceRejectsUnknownScope(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	err := s.SaveClaudeEvidence(context.Background(), []ingest.ClaudeTranscriptEvidence{{
		SourcePath: "/tmp/unknown-scope.jsonl",
		Scope:      ingest.ClaudeEvidenceScope("teammate"),
	}}, nil)
	if err == nil {
		t.Fatal("SaveClaudeEvidence accepted an unknown scope, want an error")
	}
}

// assertClaudeEvidenceEqual compares every stored field of one record.
func assertClaudeEvidenceEqual(t *testing.T, got, want ingest.ClaudeTranscriptEvidence) {
	t.Helper()
	if got.Scope != want.Scope {
		t.Errorf("transcript %q scope = %q, want %q", want.SourcePath, got.Scope, want.Scope)
	}
	if got.ModTimeUnixNano != want.ModTimeUnixNano {
		t.Errorf("transcript %q modification time = %d, want %d", want.SourcePath, got.ModTimeUnixNano, want.ModTimeUnixNano)
	}
	if got.SizeBytes != want.SizeBytes {
		t.Errorf("transcript %q size = %d, want %d", want.SourcePath, got.SizeBytes, want.SizeBytes)
	}
	if got.HasConversationRecord != want.HasConversationRecord {
		t.Errorf("transcript %q conversation flag = %t, want %t", want.SourcePath, got.HasConversationRecord, want.HasConversationRecord)
	}
	if (got.Identity == nil) != (want.Identity == nil) {
		t.Fatalf("transcript %q identity presence = %t, want %t", want.SourcePath, got.Identity != nil, want.Identity != nil)
	}
	if got.Identity != nil && *got.Identity != *want.Identity {
		t.Errorf("transcript %q identity = %+v, want %+v", want.SourcePath, *got.Identity, *want.Identity)
	}
	if len(got.Spawns) != len(want.Spawns) {
		t.Fatalf("transcript %q holds %d spawns, want %d", want.SourcePath, len(got.Spawns), len(want.Spawns))
	}
	for index := range want.Spawns {
		if got.Spawns[index] != want.Spawns[index] {
			t.Errorf("transcript %q spawn %d = %+v, want %+v", want.SourcePath, index, got.Spawns[index], want.Spawns[index])
		}
	}
	if got.Title != want.Title || got.Branch != want.Branch || got.CWD != want.CWD {
		t.Errorf("transcript %q hints = (%q, %q, %q), want (%q, %q, %q)",
			want.SourcePath, got.Title, got.Branch, got.CWD, want.Title, want.Branch, want.CWD)
	}
}
