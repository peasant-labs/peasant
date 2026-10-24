package autopublish_test

import (
	"bytes"
	"context"
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/autopublish/autopublishtest"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestRulesDecideRepositories runs the matcher cases of
// testdata/auto-publish-rules.yaml: which rules publish the repository, which
// cover it paused, and the collectives it is shared with.
func TestRulesDecideRepositories(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.For(t, autopublishtest.DriverMatcher) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			for _, rule := range c.Rules {
				if err := rule.Validate(); err != nil {
					t.Fatalf("a matcher case holds valid rules: %v", err)
				}
			}
			root := c.Repository.Root
			if rest, ok := strings.CutPrefix(root, "~/"); ok {
				root = filepath.Join(home, rest)
			}
			decision := autopublish.Decide(c.Rules, autopublish.Repository{Root: root, Remote: c.Repository.Remote, Origin: c.Repository.Origin})
			if !sameList(decision.Rules, c.Expect.Applying) {
				t.Errorf("applying rules = %v, want %v", decision.Rules, c.Expect.Applying)
			}
			if !sameList(decision.Paused, c.Expect.Paused) {
				t.Errorf("paused rules = %v, want %v", decision.Paused, c.Expect.Paused)
			}
			if want := fixture.IDs(c.Expect.Collectives); !sameList(decision.Collectives, want) {
				t.Errorf("collectives = %v, want %v", decision.Collectives, want)
			}
			if decision.Covered() != (len(c.Expect.Applying)+len(c.Expect.Paused) > 0) {
				t.Errorf("Covered() = %v; a repository is covered exactly when a rule applies or is paused", decision.Covered())
			}
		})
	}
}

// TestInvalidRulesAreRefused runs the validate cases: a rule is refused alone
// and in a rules file, and a rules file is refused when it is read, so no push
// publishes under a rule that cannot be read.
func TestInvalidRulesAreRefused(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	for _, c := range fixture.For(t, autopublishtest.DriverValidate) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if (len(c.Rules) == 1) == (c.RulesFile != "") || c.Expect.Invalid == "" {
				t.Fatal("a validate case holds one rule or a rules file, and names why it is refused")
			}
			raw := []byte(c.RulesFile)
			if len(c.Rules) == 1 {
				if err := c.Rules[0].Validate(); err == nil || !strings.Contains(err.Error(), c.Expect.Invalid) {
					t.Fatalf("Validate() = %v, want an error saying %q", err, c.Expect.Invalid)
				}
				var err error
				raw, err = yaml.Marshal(map[string]any{"version": 1, "autoPublish": c.Rules})
				if err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), autopublish.FileName)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := autopublish.Load(path); err == nil || !strings.Contains(err.Error(), c.Expect.Invalid) {
				t.Fatalf("Load() = %v, want an error saying %q", err, c.Expect.Invalid)
			}
			changed := false
			err := autopublish.Update(path, func(rules []autopublish.Rule) ([]autopublish.Rule, error) {
				changed = true
				return rules, nil
			})
			if err == nil || !strings.Contains(err.Error(), c.Expect.Invalid) || changed {
				t.Fatalf("Update() = %v, change called = %t; an unreadable file must refuse before applying a change", err, changed)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, raw) {
				t.Fatalf("Update changed invalid file bytes: %v", err)
			}
		})
	}
}

// TestUpdateKeepsTheFileReadable pins the rules file: a missing file holds no
// rule, a saved rule reads back as saved in a file private to its owner, and a
// change that would leave an invalid rule writes nothing.
func TestUpdateKeepsTheFileReadable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "peasant", autopublish.FileName)
	if rules, err := autopublish.Load(path); err != nil || len(rules) != 0 {
		t.Fatalf("Load(missing) = %v, %v; want no rule", rules, err)
	}
	rule := autopublish.Rule{ID: "acme", Kind: schema.AutoPublishRuleRemote, Match: "github.com/acme/*", Events: []schema.AutoPublishEvent{schema.AutoPublishPrePush}, Collectives: []schema.VillageUUID{"11111111-1111-4111-8111-111111111111"}}
	if err := autopublish.Update(path, func(rules []autopublish.Rule) ([]autopublish.Rule, error) { return append(rules, rule), nil }); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := autopublish.Load(path)
	if err != nil || len(rules) != 1 || rules[0].ID != rule.ID || rules[0].Match != rule.Match || !slices.Equal(rules[0].Collectives, rule.Collectives) {
		t.Fatalf("Load(saved) = %+v, %v; want the saved rule", rules, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the rules file mode = %v, %v; it is private to its owner", info.Mode().Perm(), err)
	}
	broken := rule
	broken.Kind = "branch"
	if err := autopublish.Update(path, func(rules []autopublish.Rule) ([]autopublish.Rule, error) { return append(rules, broken), nil }); err == nil {
		t.Fatal("an update that adds an invalid rule was saved")
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(saved) {
		t.Fatal("a refused update changed the rules file")
	}
}

// TestFolderMatchNamesExactlyItsFolder pins the glob a rule for one folder
// uses: every glob character of the path is escaped, so the rule covers that
// folder and no sibling.
func TestFolderMatchNamesExactlyItsFolder(t *testing.T) {
	t.Parallel()
	root := "/home/dev/work/[old] a*b?"
	rule := autopublish.Rule{ID: "one", Kind: schema.AutoPublishRuleFolder, Match: autopublish.FolderMatch(root), Events: []schema.AutoPublishEvent{schema.AutoPublishPrePush}, Collectives: []schema.VillageUUID{}}
	if err := rule.Validate(); err != nil {
		t.Fatalf("the rule for %q is invalid: %v", root, err)
	}
	if !rule.Covers(autopublish.Repository{Root: root}) {
		t.Errorf("%q does not cover %q", rule.Match, root)
	}
	if rule.Covers(autopublish.Repository{Root: "/home/dev/work/o a-bc"}) {
		t.Errorf("%q covers a sibling folder", rule.Match)
	}
}

// TestEveryDriverHasOneTest guards the fixture against a deleted, renamed, or
// duplicated driver test: its cases would then run nowhere, or twice, while
// the required-name manifest stayed green.
func TestEveryDriverHasOneTest(t *testing.T) {
	t.Parallel()
	root := testutil.ModuleRoot(t)
	calls := map[string]int{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return err
			}
			source, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			for _, declaration := range source.Decls {
				fn, ok := declaration.(*ast.FuncDecl)
				if !ok || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Body == nil {
					continue
				}
				var owned []string
				skipped := false
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					selector, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if selector.Sel.Name == "Skip" || selector.Sel.Name == "SkipNow" || selector.Sel.Name == "Skipf" {
						skipped = true
					}
					if selector.Sel.Name != "For" || len(call.Args) != 2 {
						return true
					}
					argument, ok := call.Args[1].(*ast.SelectorExpr)
					if !ok {
						return true
					}
					qualifier, ok := argument.X.(*ast.Ident)
					if ok && qualifier.Name == "autopublishtest" {
						owned = append(owned, argument.Sel.Name)
					}
					return true
				})
				if skipped && len(owned) > 0 {
					t.Errorf("%s %s skips a fixture driver", path, fn.Name.Name)
				}
				for _, driver := range owned {
					calls[driver]++
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	names := map[autopublishtest.Driver]string{
		autopublishtest.DriverMatcher: "DriverMatcher", autopublishtest.DriverValidate: "DriverValidate",
		autopublishtest.DriverInstall: "DriverInstall", autopublishtest.DriverRuleBody: "DriverRuleBody",
		autopublishtest.DriverPush: "DriverPush", autopublishtest.DriverHook: "DriverHook",
		autopublishtest.DriverVillageAuto: "DriverVillageAuto",
	}
	for _, driver := range autopublishtest.AllDrivers {
		name, ok := names[driver]
		if !ok {
			t.Fatalf("driver %q has no constant name here; add it", driver)
		}
		if calls[name] != 1 {
			t.Errorf("autopublishtest.%s is run by %d tests; exactly one test runs each driver's cases", name, calls[name])
		}
	}
}

func sameList[T comparable](got, want []T) bool {
	return len(got) == len(want) && (len(got) == 0 || slices.Equal(got, want))
}

// recordedDirectories is a store that recorded sessions in dirs.
type recordedDirectories []string

func (d recordedDirectories) RecordedDirectories(context.Context) ([]string, error) { return d, nil }

// TestRecordedRepositoriesAreTheOnesThatExist pins where a rule may install:
// a recorded directory names the repository Git reports for it, a linked
// worktree names its main repository, and a directory that is gone names
// none, not even the repository that held it.
func TestRecordedRepositoriesAreTheOnesThatExist(t *testing.T) {
	t.Parallel()
	world, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, other := filepath.Join(world, "tools"), filepath.Join(world, "notes")
	for _, dir := range []string{filepath.Join(repo, "services"), other} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "init", "--quiet", "--initial-branch=main")
	git(t, repo, "-c", "user.name=dev", "-c", "user.email=dev@example.test", "commit", "--quiet", "--allow-empty", "-m", "start")
	git(t, repo, "worktree", "add", "--quiet", "-b", "feat", filepath.Join(repo, "feat"))
	recorded, err := autopublish.Recorded(t.Context(), recordedDirectories{
		filepath.Join(repo, "services"), filepath.Join(repo, "feat"), filepath.Join(repo, "gone"), other,
	}, &ingest.ExecGitResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0].Root != repo {
		t.Fatalf("Recorded() = %+v; want only %s, once", recorded, repo)
	}
	linked, err := autopublish.Resolve(t.Context(), &ingest.ExecGitResolver{}, filepath.Join(repo, "feat"), false)
	if err != nil || linked.MainRoot != repo {
		t.Fatalf("the linked worktree resolves to %+v, %v; want its main repository %s", linked, err, repo)
	}
	if _, err := autopublish.Resolve(t.Context(), &ingest.ExecGitResolver{}, filepath.Join(repo, "gone"), false); err == nil {
		t.Fatal("a directory that is gone resolved to a repository")
	}
}

// TestFolderGlobNamesAFolderThroughASymlink pins that a glob written through
// a symlinked folder covers a repository Git reports by its real path.
func TestFolderGlobNamesAFolderThroughASymlink(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real, link := filepath.Join(base, "real"), filepath.Join(base, "link")
	if err := os.MkdirAll(filepath.Join(real, "work", "tools"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	rule := autopublish.Rule{ID: "work", Kind: schema.AutoPublishRuleFolder, Match: link + "/work/*", Events: []schema.AutoPublishEvent{schema.AutoPublishPrePush}, Collectives: []schema.VillageUUID{}}
	if !rule.Covers(autopublish.Repository{Root: filepath.Join(real, "work", "tools")}) {
		t.Fatalf("%q does not cover the repository it names through the symlink", rule.Match)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Keep the host's git configuration out of the fixture repositories: a
	// global commit.gpgsign can block on a pinentry prompt, and host hooks or
	// URL rewrites would change what the test observes.
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

//go:embed testdata/file_update_boundaries.yaml
var fileUpdateBoundariesYAML []byte

type fileUpdateBoundary struct {
	Name            string             `yaml:"name"`
	Rules           []autopublish.Rule `yaml:"rules"`
	Rejected        autopublish.Rule   `yaml:"rejected"`
	ExpectedIDs     []string           `yaml:"expectedIDs"`
	ContentionError string             `yaml:"contentionError"`
	RefusalError    string             `yaml:"refusalError"`
}

func loadFileUpdateBoundary(t *testing.T, name string) fileUpdateBoundary {
	t.Helper()
	var f struct {
		RequiredNames []string             `yaml:"requiredNames"`
		Cases         []fileUpdateBoundary `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(fileUpdateBoundariesYAML, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("required file-update fixture %q is missing", name)
	return fileUpdateBoundary{}
}

func assertExactRuleIDs(t *testing.T, path string, expected []string) {
	t.Helper()
	rules, err := autopublish.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, id := range expected {
		if want[id] {
			t.Fatalf("duplicate expected rule identity %q", id)
		}
		want[id] = true
	}
	for _, rule := range rules {
		if !want[rule.ID] {
			t.Fatalf("unexpected or duplicate saved rule %q; expected=%v", rule.ID, expected)
		}
		delete(want, rule.ID)
	}
	if len(want) != 0 {
		t.Fatalf("concurrent edits lost exact rule identities: %v", want)
	}
}

// Two independent editors, never one writer per unrelated matcher case. The
// first actual callback holds its staged old view. A complete contended Update
// provides a real lock-attempt/refusal gate; only then is the first released.
// The refused editor explicitly retries after the first commit. This portable
// proof needs no production lock hook, sleep, or goroutine-start assumption.
func TestConcurrentUpdatesRetainEveryRule(t *testing.T) {
	t.Parallel()
	f := loadFileUpdateBoundary(t, "two-independent-edits-retain-exact-rule-identities")
	if len(f.Rules) != 2 {
		t.Fatal("the dedicated concurrency fixture must retain its declared two-editor contract")
	}
	path := filepath.Join(t.TempDir(), autopublish.FileName)
	if err := autopublish.Update(path, func(rules []autopublish.Rule) ([]autopublish.Rule, error) { return rules, nil }); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	staged, release := make(chan struct{}), make(chan struct{})
	done, result := make(chan struct{}), make(chan error, 1)
	var once sync.Once
	t.Cleanup(func() { cancel(); once.Do(func() { close(release) }); <-done })
	go func() {
		defer close(done)
		result <- autopublish.Update(path, func(rules []autopublish.Rule) ([]autopublish.Rule, error) {
			close(staged)
			select {
			case <-release:
				return append(rules, f.Rules[0]), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	}()
	select {
	case <-staged:
	case <-ctx.Done():
		t.Fatal("first editor never staged its actual old view")
	}
	secondCalled := false
	secondEdit := func(rules []autopublish.Rule) ([]autopublish.Rule, error) {
		secondCalled = true
		return append(rules, f.Rules[1]), nil
	}
	// Keep the production five-second lock deadline unchanged. A completed
	// refused acquisition, not a timing guess, proves meaningful overlap.
	secondErr := autopublish.Update(path, secondEdit)
	calledDuringContention := secondCalled
	afterContention, readErr := os.ReadFile(path)
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("first editor did not finish after its callback was released")
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if secondErr != nil {
		if err := autopublish.Update(path, secondEdit); err != nil {
			t.Fatal(err)
		}
	}
	// A no-lock mutant lets the second commit B before the first writes its
	// staged A. The exact membership check then fails for lost B, not a count.
	assertExactRuleIDs(t, path, f.ExpectedIDs)
	if secondErr == nil || !strings.Contains(secondErr.Error(), f.ContentionError) {
		t.Fatalf("contended edit did not observe real acquisition refusal: %v", secondErr)
	}
	if calledDuringContention {
		t.Fatal("refused lock acquisition entered the edit callback")
	}
	if readErr != nil || !bytes.Equal(afterContention, before) {
		t.Fatalf("contended refusal changed bytes before first commit: %q %v", afterContention, readErr)
	}
	// The second callback may run on the explicit retry, never on refusal.
	if !secondCalled {
		t.Fatal("second editor never completed its explicit retry")
	}
}

func TestUpdateValidationRefusalRetainsExactBytes(t *testing.T) {
	t.Parallel()
	f := loadFileUpdateBoundary(t, "validation-refusal-preserves-exact-file-bytes")
	path := filepath.Join(t.TempDir(), autopublish.FileName)
	if err := autopublish.Update(path, func([]autopublish.Rule) ([]autopublish.Rule, error) { return f.Rules, nil }); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = autopublish.Update(path, func(rules []autopublish.Rule) ([]autopublish.Rule, error) { return append(rules, f.Rejected), nil })
	if err == nil || !strings.Contains(err.Error(), f.RefusalError) {
		t.Fatalf("expected actual validation refusal: %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("refused validation changed exact file bytes: %q %v", after, readErr)
	}
	assertExactRuleIDs(t, path, f.ExpectedIDs)
}

func TestUpdatePreservesAnUnreadableFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks.yaml")
	raw := []byte("autoPublish: [unfinished")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	err := autopublish.Update(path, func(rules []autopublish.Rule) ([]autopublish.Rule, error) { called = true; return nil, nil })
	if err == nil || called {
		t.Fatalf("unreadable rules must refuse before the callback: %v, called=%v", err, called)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != string(raw) {
		t.Fatalf("unreadable file changed: %q, %v", after, readErr)
	}
}

// A missing recorded path is not the directory the command happens to run in.
func TestSessionRepositoryNeverUsesTheCurrentDirectory(t *testing.T) {
	t.Parallel()
	const remote = "https://github.com/acme/tools.git"
	repo := autopublish.SessionRepository(t.Context(), &ingest.ExecGitResolver{}, "", remote, false)
	if repo.Root != "" || repo.MainRoot != "" || repo.Gone != "" || repo.Remote != remote || repo.Origin != remote {
		t.Fatalf("empty recorded path resolved to a repository: %+v", repo)
	}
	if _, err := autopublish.Resolve(t.Context(), &ingest.ExecGitResolver{}, "", false); err == nil {
		t.Fatal("an empty directory resolved to the command's repository")
	}
}
