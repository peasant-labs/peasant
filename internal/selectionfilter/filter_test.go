package selectionfilter

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/filter.yaml
var filterFixtureYAML []byte

type filterFixture struct {
	RequiredNames []string     `yaml:"requiredNames"`
	Cases         []filterCase `yaml:"cases"`
}

type filterCase struct {
	Name                    string                     `yaml:"name"`
	Harness                 string                     `yaml:"harness"`
	Projects                []config.ProjectSelection  `yaml:"projects"`
	ExplicitSessions        []string                   `yaml:"explicitSessions"`
	Exclusions              config.SelectionExclusions `yaml:"exclusions"`
	AutoIngestNewBranches   bool                       `yaml:"autoIngestNewBranches"`
	CompareDisabled         bool                       `yaml:"compareDisabled"`
	Pathless                bool                       `yaml:"pathless"`
	Sessions                []filterSession            `yaml:"sessions"`
	GitRemote               string                     `yaml:"gitRemote"`
	GitBranch               string                     `yaml:"gitBranch"`
	RepeatMatch             bool                       `yaml:"repeatMatch"`
	UnknownSession          string                     `yaml:"unknownSession"`
	ExcludesBeforePrepare   bool                       `yaml:"excludesBeforePrepare"`
	ExpectPrepared          int                        `yaml:"expectPrepared"`
	ExpectGitCalls          int                        `yaml:"expectGitCalls"`
	CheckExcludes           bool                       `yaml:"checkExcludes"`
	CheckSecondMatch        bool                       `yaml:"checkSecondMatch"`
	CheckSecondExcludes     bool                       `yaml:"checkSecondExcludes"`
	ExpectMatch             bool                       `yaml:"expectMatch"`
	ExpectSecondMatch       bool                       `yaml:"expectSecondMatch"`
	ExpectExcludes          bool                       `yaml:"expectExcludes"`
	ExpectSecondExcludes    bool                       `yaml:"expectSecondExcludes"`
	ExpectUnknownMatch      bool                       `yaml:"expectUnknownMatch"`
	ExpectConflictCount     int                        `yaml:"expectConflictCount"`
	ExpectMatchWhenDisabled bool                       `yaml:"expectMatchWhenDisabled"`
	NoticeContains          []string                   `yaml:"noticeContains"`
	SummaryContains         []string                   `yaml:"summaryContains"`
}

type filterSession struct {
	ID            string `yaml:"id"`
	CWD           string `yaml:"cwd"`
	ProjectName   string `yaml:"projectName"`
	Branch        string `yaml:"branch"`
	UnrelatedSlug bool   `yaml:"unrelatedSlug"`
}

func loadFilterFixture(t *testing.T) filterFixture {
	t.Helper()
	var fixture filterFixture
	if err := testutil.DecodeFixtureYAML(filterFixtureYAML, &fixture); err != nil {
		t.Fatalf("decode selection filter fixture: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, item := range fixture.Cases {
		if item.Name == "" || present[item.Name] {
			t.Fatalf("selection filter fixture has an empty or duplicate case name %q", item.Name)
		}
		present[item.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/selectionfilter/testdata/filter.yaml", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestFilterFixtureRejectsUnknownField(t *testing.T) {
	t.Parallel()
	mutated := append([]byte(nil), filterFixtureYAML...)
	mutated = append(mutated, []byte("\nunknown: true\n")...)
	if err := testutil.DecodeFixtureYAML(mutated, &filterFixture{}); err == nil {
		t.Fatal("selection filter fixture accepted an unknown field")
	}
}

func TestFilterFixtureRejectsMissingRequiredName(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(filterFixtureYAML, []byte("name: match-admits-selected-session"), []byte("name: renamed-selected-session"), 1)
	var fixture filterFixture
	if err := testutil.DecodeFixtureYAML(mutated, &fixture); err != nil {
		t.Fatalf("decode mutated fixture: %v", err)
	}
	present := map[string]bool{}
	for _, item := range fixture.Cases {
		present[item.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/selectionfilter/testdata/filter.yaml", "case", fixture.RequiredNames, present); err == nil {
		t.Fatal("removing a required case name was accepted")
	}
}

type countingGit struct {
	remote string
	branch string
	calls  int
	dirs   []string
}

func (g *countingGit) RemoteURL(_ context.Context, dir string) (string, error) {
	g.calls++
	g.dirs = append(g.dirs, "remote:"+dir)
	if g.remote == "" {
		return "", fmt.Errorf("no remote configured for this case")
	}
	return g.remote, nil
}

func (g *countingGit) Branch(_ context.Context, dir string) (string, error) {
	g.calls++
	g.dirs = append(g.dirs, "branch:"+dir)
	if g.branch == "" {
		return "", fmt.Errorf("no branch configured for this case")
	}
	return g.branch, nil
}

func (g *countingGit) Worktree(context.Context, string) (string, error) {
	return "", fmt.Errorf("worktree is not used by selection preparation")
}

func (g *countingGit) TrackingBranch(context.Context, string) (string, error) {
	return "", fmt.Errorf("tracking branch is not used by selection preparation")
}

func (g *countingGit) UserEmail(context.Context) (string, error) {
	return "", fmt.Errorf("user email is not used by selection preparation")
}

func (g *countingGit) WalkUpRemoteURL(context.Context, string) (string, string, error) {
	return "", "", fmt.Errorf("walk-up remote is not used by selection preparation")
}

type cleanPathResolver struct{}

func (cleanPathResolver) Resolve(raw string) (ingest.ClonePath, error) {
	if raw == "" || !filepath.IsAbs(raw) {
		return "", fmt.Errorf("selection path %q is not an absolute directory", raw)
	}
	return ingest.ClonePath(filepath.Clean(raw)), nil
}

func TestFilterFixture(t *testing.T) {
	t.Parallel()
	for _, item := range loadFilterFixture(t).Cases {
		item := item
		t.Run(item.Name, func(t *testing.T) {
			t.Parallel()
			runFilterCase(t, item)
		})
	}
}

func runFilterCase(t *testing.T, item filterCase) {
	t.Helper()
	projects := append([]config.ProjectSelection(nil), item.Projects...)
	sessions := make([]ingest.DiscoveredSession, len(item.Sessions))
	if item.Pathless {
		root := t.TempDir()
		slug := strings.ReplaceAll(filepath.Clean(root), string(filepath.Separator), "-")
		projects = []config.ProjectSelection{{ClonePaths: []string{filepath.Clean(root)}}}
		for index, row := range item.Sessions {
			sessionSlug := slug
			if row.UnrelatedSlug {
				sessionSlug = "-this-slug-decodes-to-nothing"
			}
			id := mustSessionID(t, row.ID)
			session := ingest.DiscoveredSession{
				SessionID: id,
				Harness:   ingest.Harness(item.Harness),
			}
			if !row.UnrelatedSlug {
				session.SourcePath = ingest.ResolvedPath(filepath.Join("/sources", sessionSlug, row.ID+".jsonl"))
			}
			sessions[index] = session
		}
	} else {
		for index, row := range item.Sessions {
			sessions[index] = ingest.DiscoveredSession{
				SessionID:   mustSessionID(t, row.ID),
				Harness:     ingest.Harness(item.Harness),
				ProjectName: row.ProjectName,
				CWD:         row.CWD,
				Branch:      row.Branch,
			}
		}
	}

	git := &countingGit{remote: item.GitRemote, branch: item.GitBranch}
	filter, recorder := newFilter(t, item, projects, item.AutoIngestNewBranches, git)
	if item.ExcludesBeforePrepare {
		if !filter.Excludes(sessions[0]) {
			t.Fatal("Excludes admitted a session before the cohort was prepared; an unprepared session must fail closed")
		}
		if filter.Match(sessions[0]) {
			t.Fatal("Match admitted a session before the cohort was prepared")
		}
	}
	if err := filter.Prepare(context.Background(), sessions); err != nil {
		t.Fatalf("prepare cohort: %v", err)
	}
	assertMatch(t, "first", filter.Match(sessions[0]), item.ExpectMatch)
	if item.CheckExcludes {
		if got := filter.Excludes(sessions[0]); got != item.ExpectExcludes {
			t.Fatalf("Excludes = %v, want %v", got, item.ExpectExcludes)
		}
	}
	if item.CheckSecondMatch {
		if len(sessions) < 2 {
			t.Fatal("case checks a second session but the cohort has one")
		}
		assertMatch(t, "second", filter.Match(sessions[1]), item.ExpectSecondMatch)
	}
	if item.CheckSecondExcludes {
		if got := filter.Excludes(sessions[1]); got != item.ExpectSecondExcludes {
			t.Fatalf("second Excludes = %v, want %v", got, item.ExpectSecondExcludes)
		}
	}
	if item.UnknownSession != "" {
		unknown := sessions[0]
		unknown.SessionID = mustSessionID(t, item.UnknownSession)
		if got := filter.Match(unknown); got != item.ExpectUnknownMatch {
			t.Fatalf("Match of a session outside the prepared cohort = %v, want %v", got, item.ExpectUnknownMatch)
		}
		if !filter.Excludes(unknown) {
			t.Fatal("Excludes admitted a session outside the prepared cohort")
		}
	}
	if item.RepeatMatch {
		if filter.Match(sessions[0]) {
			t.Fatal("a repeated match admitted a withheld session")
		}
	}
	if git.calls != item.ExpectGitCalls {
		t.Fatalf("git calls = %d, want %d (%v)", git.calls, item.ExpectGitCalls, git.dirs)
	}
	if recorder.Recorded() != item.ExpectConflictCount {
		t.Fatalf("recorded conflicts = %d, want %d", recorder.Recorded(), item.ExpectConflictCount)
	}
	var notice bytes.Buffer
	recorder.Notice(&notice, "/config/chosen.yaml")
	if len(item.NoticeContains) == 0 {
		if notice.Len() != 0 {
			t.Fatalf("notice = %q, want empty", notice.String())
		}
	}
	for _, needle := range item.NoticeContains {
		if !strings.Contains(notice.String(), needle) {
			t.Fatalf("notice does not contain %q:\n%s", needle, notice.String())
		}
	}
	summary := recorder.Summary("/config/chosen.yaml")
	if len(item.SummaryContains) == 0 && summary != "" {
		t.Fatalf("summary = %q, want empty when nothing was withheld", summary)
	}
	for _, needle := range item.SummaryContains {
		if !strings.Contains(summary, needle) {
			t.Fatalf("summary does not contain %q: %q", needle, summary)
		}
	}
	if item.RepeatMatch && strings.Count(notice.String(), "was withheld during ingest") != 1 {
		t.Fatalf("withheld notice was recorded more than once:\n%s", notice.String())
	}
	if item.CompareDisabled {
		disabledGit := &countingGit{remote: item.GitRemote, branch: item.GitBranch}
		disabled, _ := newFilter(t, item, projects, false, disabledGit)
		if err := disabled.Prepare(context.Background(), sessions); err != nil {
			t.Fatalf("prepare cohort with auto-ingest disabled: %v", err)
		}
		assertMatch(t, "auto-ingest disabled", disabled.Match(sessions[0]), item.ExpectMatchWhenDisabled)
	}
}

func newFilter(t *testing.T, item filterCase, projects []config.ProjectSelection, auto bool, git ingest.GitResolver) (*Filter, *ConflictRecorder) {
	t.Helper()
	cfg := config.BaseConfig()
	cfg.Selection = config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: auto,
		Harnesses: map[string]config.SelectionHarnessConfig{
			item.Harness: {
				Projects:   projects,
				Sessions:   item.ExplicitSessions,
				Exclusions: item.Exclusions,
			},
		},
	}
	if item.Harness != defaults.HarnessClaudeCode.String() && item.Harness != ingest.HarnessOpenCode.String() && item.Harness != "" {
		t.Fatalf("fixture harness %q is not a harness this case knows how to build", item.Harness)
	}
	return New(cfg, git, cleanPathResolver{})
}

func assertMatch(t *testing.T, which string, got, want bool) {
	t.Helper()
	if got != want {
		t.Fatalf("%s Match = %v, want %v", which, got, want)
	}
}

func mustSessionID(t *testing.T, raw string) ingest.SessionID {
	t.Helper()
	id, err := ingest.NewSessionID(raw)
	if err != nil {
		t.Fatalf("session id %q: %v", raw, err)
	}
	return id
}
