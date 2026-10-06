package store_test

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/latest_publication.yaml
var latestPublicationYAML []byte

// TestLatestPublicationIsTheAccountsLastReceipt runs
// testdata/latest_publication.yaml: the read `peasant village auto` takes the
// collectives from is the receipt Village updated last, for the one account on
// the one Village, never another account's.
func TestLatestPublicationIsTheAccountsLastReceipt(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Receipts []struct {
			Name       string `yaml:"name"`
			Origin     string `yaml:"origin"`
			Owner      string `yaml:"owner"`
			Session    string `yaml:"session"`
			Transcript string `yaml:"transcript"`
			UpdatedAt  int64  `yaml:"updatedAt"`
		} `yaml:"receipts"`
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name   string `yaml:"name"`
			Origin string `yaml:"origin"`
			Owner  string `yaml:"owner"`
			Want   string `yaml:"want"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(latestPublicationYAML, &fixture); err != nil {
		t.Fatalf("testdata/latest_publication.yaml: %v", err)
	}
	template := publicationRecordFromFixture(t, loadPublicationFixture(t).Records[0])
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	sessions := map[string]string{}
	for _, r := range fixture.Receipts {
		storetest.SeedSessionInProject(t, s, r.Session, template.ProjectHash)
		record := template
		record.VillageOrigin, record.OwnerUserID, record.SessionID = r.Origin, r.Owner, r.Session
		record.Receipt.TranscriptID = schema.TranscriptID(r.Transcript)
		record.Receipt.TranscriptURL = "https://village.example/transcripts/" + r.Transcript
		record.Receipt.UpdatedAt = r.UpdatedAt
		if err := s.SavePublication(ctx, record); err != nil {
			t.Fatalf("save receipt %s: %v", r.Name, err)
		}
		sessions[r.Name] = r.Session
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := s.LatestPublication(ctx, c.Origin, c.Owner)
			if err != nil {
				t.Fatal(err)
			}
			if c.Want == "" {
				if got != nil {
					t.Fatalf("LatestPublication() = %+v; the account has no receipt", got)
				}
				return
			}
			if got == nil || got.SessionID != sessions[c.Want] || got.VillageOrigin != c.Origin || got.OwnerUserID != c.Owner {
				t.Fatalf("LatestPublication() = %+v; want receipt %s", got, c.Want)
			}
		})
	}
}

// TestRecordedDirectoriesFallBackToTheProjectDirectory pins where a session
// was recorded: its git worktree when ingest captured one, else its project's
// working directory. Each directory is listed once.
func TestRecordedDirectoriesFallBackToTheProjectDirectory(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	project := schema.ProjectHash("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	withoutWorktree := []string{"30d59925-36bc-424c-a789-8be54d970201", "30d59925-36bc-424c-a789-8be54d970202"}
	for _, id := range withoutWorktree {
		storetest.SeedSessionInProject(t, s, id, project)
	}
	withWorktree := "30d59925-36bc-424c-a789-8be54d970203"
	worktree := "/work/tools/services"
	ingested := int64(3)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: &schema.UnifiedMetadata{
		SessionID: schema.SessionID(withWorktree), ModelHarness: ingest.HarnessClaudeCode, Model: schema.ModelID("claude-opus-4-6"),
		HostSlug:  schema.HostSlug("testslug"),
		Project:   schema.ProjectContext{Hash: project, Name: "testproj", FilePath: "/testproj"},
		Timestamp: schema.TimestampInfo{Start: 1, End: 2, Ingested: &ingested},
		Source:    schema.SourceInfo{FilePath: "/f", Format: schema.SourceFormatJSONL},
		Git:       schema.GitContext{Worktree: &worktree},
	}}}); err != nil {
		t.Fatal(err)
	}

	dirs, err := s.RecordedDirectories(ctx)
	if err != nil || !slices.Equal(dirs, []string{"/testproj", worktree}) {
		t.Fatalf("RecordedDirectories() = %v, %v; want the project directory once and the worktree", dirs, err)
	}
	// More identifiers than one batch reads, so the session in the second
	// batch is found too.
	ids := append([]string{}, withoutWorktree...)
	for i := 0; i < 300; i++ {
		ids = append(ids, fmt.Sprintf("40d59925-36bc-424c-a789-%012d", i))
	}
	byID, err := s.SessionDirectories(ctx, append(ids, withWorktree))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{withoutWorktree[0]: "/testproj", withoutWorktree[1]: "/testproj", withWorktree: worktree}
	if len(byID) != len(want) {
		t.Fatalf("SessionDirectories() = %v, want %v; an unknown session is absent", byID, want)
	}
	for id, dir := range want {
		if byID[id] != dir {
			t.Errorf("session %s directory = %q, want %q", id, byID[id], dir)
		}
	}
}
