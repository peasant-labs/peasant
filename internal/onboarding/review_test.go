package onboarding_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/review.yaml
var reviewData []byte

type reviewWant struct {
	Conflicts             []commitConflictWant `yaml:"conflicts"`
	Unresolved            []string             `yaml:"unresolved"`
	DiffFile              string               `yaml:"diffFile"`
	HunkHeaders           []string             `yaml:"hunkHeaders"`
	DiffLines             []string             `yaml:"diffLines"`
	FirstHunkHeaderStarts string               `yaml:"firstHunkHeaderStarts"`
	FirstDiffLineStarts   string               `yaml:"firstDiffLineStarts"`
	FileUnchanged         bool                 `yaml:"fileUnchanged"`
	Failure               string               `yaml:"failure"`
	ErrorContains         string               `yaml:"errorContains"`
	Saved                 *reviewSavedWant     `yaml:"saved"`
}

// reviewSavedWant is what ReviewResult.Saved must hold: the configuration the
// save would produce.
type reviewSavedWant struct {
	License         string `yaml:"license"`
	SharePreference string `yaml:"sharePreference"`
	RetentionDays   int    `yaml:"retentionDays"`
}

type reviewCase struct {
	Name         string `yaml:"name"`
	Opened       string `yaml:"opened"`
	OpenedAbsent bool   `yaml:"openedAbsent"`
	OnDisk       string `yaml:"onDisk"`
	Scope        string `yaml:"scope"`
	// RetentionSeed is the period the session showed on its retention stop.
	RetentionSeed int                         `yaml:"retentionSeed"`
	Answers       map[string]string           `yaml:"answers"`
	Resolutions   map[string]commitResolution `yaml:"resolutions"`
	HoldLock      bool                        `yaml:"holdLock"`
	Want          reviewWant                  `yaml:"want"`
}

type reviewDocument struct {
	RequiredNames []string     `yaml:"requiredNames"`
	Cases         []reviewCase `yaml:"cases"`
}

func TestDraftReview(t *testing.T) {
	t.Parallel()
	var document reviewDocument
	decodeFixture(t, "review.yaml", reviewData, &document)
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	requireNames(t, document.RequiredNames, names)
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			runReviewCase(t, row)
		})
	}
}

func runReviewCase(t *testing.T, row reviewCase) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "peasant", "config.yaml")
	loaded := config.LoadDefaults(ctx, nil)
	if !row.OpenedAbsent {
		writeFixtureFile(t, path, row.Opened)
		parsed, err := config.Parse([]byte(row.Opened))
		if err != nil {
			t.Fatalf("the opened document does not parse: %v", err)
		}
		loaded = parsed
	}
	draft, err := onboarding.OpenDraft(path, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if row.RetentionSeed > 0 {
		days := row.RetentionSeed
		if err := draft.SeedTransient(func(cfg *config.Config) { cfg.ClaudeRetentionDays = days }); err != nil {
			t.Fatal(err)
		}
	}
	if row.OnDisk != "" {
		writeFixtureFile(t, path, row.OnDisk)
	}
	before, _ := os.ReadFile(path)
	if row.HoldLock {
		holder, err := config.OpenConfigFile(path, time.Now().Add(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = holder.Close() })
	}
	scope := onboarding.ScopeFull
	if row.Scope != "" {
		if scope, err = onboarding.NewModelScope(row.Scope); err != nil {
			t.Fatal(err)
		}
	}
	answers := onboarding.Answers{}
	for key, text := range row.Answers {
		field, err := onboarding.NewFieldKey(key)
		if err != nil {
			t.Fatal(err)
		}
		answers[field] = schema.LocalSettingValue(text)
	}
	resolutions := onboarding.Resolutions{}
	for key, resolution := range row.Resolutions {
		field, err := onboarding.NewFieldKey(key)
		if err != nil {
			t.Fatal(err)
		}
		choice, err := schema.NewLocalKickstartResolution(resolution.Choice)
		if err != nil {
			t.Fatal(err)
		}
		resolutions[field] = onboarding.Resolution{Choice: choice, Seen: schema.LocalSettingValue(resolution.Seen)}
	}

	review, reviewErr := draft.Review(ctx, onboarding.ReviewInput{
		Model:       onboarding.BuildModel(onboarding.Options{Scope: scope}),
		Answers:     answers,
		Resolutions: resolutions,
		Context:     onboarding.WriteContext{ScanState: onboarding.ScanDone, ClaudeSessions: row.RetentionSeed > 0},
	})

	want := row.Want
	after, _ := os.ReadFile(path)
	if want.FileUnchanged && !bytes.Equal(before, after) {
		t.Errorf("Review changed the file:\nbefore: %q\n after: %q", before, after)
	}
	if _, err := os.Stat(path + ".lock"); !row.HoldLock && err == nil {
		t.Errorf("Review created the lock file; a review takes no lock")
	}
	if want.Failure != "" {
		var failed *onboarding.CommitError
		if !errors.As(reviewErr, &failed) || failed.Kind.String() != want.Failure {
			t.Fatalf("Review error = %v, want a *CommitError of kind %s", reviewErr, want.Failure)
		}
		if !strings.Contains(reviewErr.Error(), want.ErrorContains) {
			t.Errorf("Review error = %v, want it to contain %q", reviewErr, want.ErrorContains)
		}
		return
	}
	if reviewErr != nil {
		t.Fatalf("Review: %v", reviewErr)
	}
	if want.Conflicts != nil {
		assertConflicts(t, want.Conflicts, review.Conflicts)
	}
	var unresolved []string
	for _, key := range review.Unresolved {
		unresolved = append(unresolved, key.String())
	}
	if want.Unresolved != nil && !slices.Equal(unresolved, want.Unresolved) {
		t.Errorf("unresolved = %v, want %v", unresolved, want.Unresolved)
	}
	if saved := want.Saved; saved != nil {
		if review.Saved == nil {
			t.Fatal("the review returned no saved configuration")
		}
		if saved.License != "" && string(review.Saved.Push.License) != saved.License {
			t.Errorf("saved license = %q, want %q", review.Saved.Push.License, saved.License)
		}
		if saved.SharePreference != "" && string(review.Saved.Push.SharePreference) != saved.SharePreference {
			t.Errorf("saved share preference = %q, want %q", review.Saved.Push.SharePreference, saved.SharePreference)
		}
		if review.Saved.ClaudeRetentionDays != saved.RetentionDays {
			t.Errorf("saved retention = %d days, want %d", review.Saved.ClaudeRetentionDays, saved.RetentionDays)
		}
	}
	if want.DiffFile != "" {
		if err := review.Diff.Validate(); err != nil {
			t.Errorf("the diff is not a valid contract diff: %v", err)
		}
		if review.Diff.File != want.DiffFile {
			t.Errorf("diff file = %q, want %q", review.Diff.File, want.DiffFile)
		}
	}
	var headers, lines []string
	for _, hunk := range review.Diff.Hunks {
		headers = append(headers, hunk.Header)
		for _, line := range hunk.Lines {
			lines = append(lines, line.Type.String()+":"+line.Text)
		}
	}
	if want.HunkHeaders != nil && !slices.Equal(headers, want.HunkHeaders) {
		t.Errorf("hunk headers = %v, want %v", headers, want.HunkHeaders)
	}
	if want.DiffLines != nil && !slices.Equal(lines, want.DiffLines) {
		t.Errorf("diff lines = %q, want %q", lines, want.DiffLines)
	}
	if want.FirstHunkHeaderStarts != "" && (len(headers) == 0 || !strings.HasPrefix(headers[0], want.FirstHunkHeaderStarts)) {
		t.Errorf("first hunk header = %v, want prefix %q", headers, want.FirstHunkHeaderStarts)
	}
	if want.FirstDiffLineStarts != "" && (len(lines) == 0 || !strings.HasPrefix(lines[0], want.FirstDiffLineStarts)) {
		t.Errorf("first diff line = %q, want prefix %q", lines, want.FirstDiffLineStarts)
	}
}
