//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// TestPostCommitHookPublishesConfiguredRepositoryToRealVillage is the only test
// that exercises the whole chain — real git, the generated hook, the real peasant
// binary, a real Village — so it is the release oracle for this feature. Every
// value it asserts on comes from testdata/hook-joined-evidence.yaml, and every
// piece of environment it needs comes from the one disposable substrate.
func TestPostCommitHookPublishesConfiguredRepositoryToRealVillage(t *testing.T) {
	document, err := LoadHookJoinedEvidenceFixtures()
	if err != nil {
		t.Fatal(err)
	}
	want := hookJoinedEvidenceCaseByOutcome(t, document, outcomePublishes)
	bins := resolveVillageBinaries(t)
	peasantBin := buildPeasant(t)
	stack := provisionHarnessStack(t, bins)
	if stack.external {
		t.Skip("joined hook evidence requires the harness-owned Village process and Postgres")
	}

	realDataDir := string(defaults.ResolveDataDirPath())
	realBefore := snapshotRealDataDir(realDataDir)
	t.Cleanup(func() { assertRealDataDirUnchanged(t, harnessOptions{assert: true}, realDataDir, realBefore) })

	sandbox := newDisposableSandbox(t, peasantBin)
	assertDeveloperStateUnchanged(t)

	configuredRepository := sandbox.initRepository(t, "configured-repository")
	otherRepository := sandbox.initRepository(t, "other-repository")
	configuredSource := reseedClaudeFixture(t, claudeReseed{
		Destination:              filepath.Join(sandbox.root, "fixtures", "configured-repository"),
		RecordedWorkingDirectory: configuredRepository,
	})
	otherSource := reseedClaudeFixture(t, claudeReseed{
		Destination:              filepath.Join(sandbox.root, "fixtures", "other-repository"),
		RecordedWorkingDirectory: otherRepository,
		RootSessionID:            want.SecondRootSessionID,
		SubagentSessionID:        want.SecondSubagentSessionID,
	})
	writeHookEvidenceConfig(t, sandbox, configuredSource, otherSource, want)
	_ = mintDemoCredentials(t, bins.setupDemo, stack.dsn, stack.villageURL, sandbox.configHome)
	runPeasantInSandbox(t, peasantBin, sandbox, "ingest", "--include-active")

	seeded := want.ExpectedTranscripts + want.ExcludedTranscripts
	if got := len(readSessionIDs(t, filepath.Join(sandbox.dataHome, string(defaults.AppName), "peasant.db"))); got != seeded {
		t.Fatalf("seeded sessions = %d, want %d", got, seeded)
	}
	status := runPeasantInSandbox(t, peasantBin, sandbox, "village", "push", "--dry-run", "--repository", configuredRepository)
	basis := requireNeedle(t, fmt.Sprintf("the rendered description of identity_basis %s", want.IdentityBasis), want.IdentityBasis.Describe())
	if !strings.Contains(status, basis) {
		t.Fatalf("repository scope did not report identity basis %q:\n%s", want.IdentityBasis, status)
	}
	installOut := runPeasantInSandbox(t, peasantBin, sandbox, "village", "hooks", "install", "--event", "post-commit", "--dir", configuredRepository, "--timeout", "45s")
	assertDisclosed(t, "hook install consent output", want.InstallDisclosureContains, installOut)

	commitOut, commitErr := gitCommand(sandbox.environment, configuredRepository, "commit", "--allow-empty", "-m", "publish configured repository")
	if commitErr != nil {
		t.Fatalf("real git commit with installed hook failed: %v\n%s", commitErr, commitOut)
	}
	assertDisclosed(t, "hook-triggered push output", want.DisclosureContains, commitOut)
	if strings.Contains(commitOut, "error(s)") {
		logs, _ := filepath.Glob(filepath.Join(sandbox.stateHome, string(defaults.AppName), "logs", "push--error--*.log"))
		for _, path := range logs {
			if raw, readErr := os.ReadFile(path); readErr == nil {
				t.Logf("hook publication diagnostic %s:\n%s", path, raw)
			}
		}
	}
	// Both surfaces are checked against the same list: the consent moment and the
	// per-commit output each restated the access claim independently, and a list
	// covering only one of them is how a false claim on the other survived.
	assertNeverEmitted(t, "hook install output", want.ForbiddenContains, installOut)
	assertNeverEmitted(t, "hook-triggered push output", want.ForbiddenContains, commitOut)
	if !strings.Contains(commitOut, requireNeedle(t, "success_result", want.SuccessResult)) {
		t.Errorf("repository-scoped hook did not report the exact successful upload result %q:\n%s", want.SuccessResult, commitOut)
	}

	ownerID := readDemoUserID(t, sandbox.configHome)
	rows := listVillageTranscripts(t, stack.db, ownerID)
	if len(rows) != want.ExpectedTranscripts {
		t.Fatalf("Village transcript count after post-commit = %d, want exactly %d", len(rows), want.ExpectedTranscripts)
	}
	for _, row := range rows {
		if row.Visibility != want.Visibility.String() {
			t.Errorf("Village transcript %s visibility read back from Postgres = %q, want configured %q", row.LocalID, row.Visibility, want.Visibility)
		}
		if row.LocalID == want.SecondRootSessionID || row.LocalID == want.SecondSubagentSessionID {
			t.Errorf("session %s recorded in the second repository escaped repository scope", row.LocalID)
		}
	}
	assertLocalPublicationReceiptsMatchVillage(t, sandbox, stack.villageURL, ownerID, rows)
	// Nothing is asserted about the redaction of published CONTENT. That is a
	// deliberate decision, not a gap; see the corpus.
	villageTranscriptByLocalID(t, rows, FixtureRootSessionID)

	stack.village.stop()
	beforeHead := strings.TrimSpace(runGit(t, sandbox.environment, configuredRepository, "rev-parse", "HEAD"))
	failureOut, failureErr := gitCommand(sandbox.environment, configuredRepository, "commit", "--allow-empty", "-m", "village unavailable")
	if failureErr != nil {
		t.Fatalf("git commit was blocked by unavailable Village: %v\n%s", failureErr, failureOut)
	}
	afterHead := strings.TrimSpace(runGit(t, sandbox.environment, configuredRepository, "rev-parse", "HEAD"))
	if beforeHead == afterHead {
		t.Fatal("git commit exited successfully but did not create a commit")
	}
	assertDisclosed(t, "unavailable-Village warning", want.WarningContains, failureOut)
	t.Logf("joined hook evidence: published=%d excluded=%d visibility=%s redaction=%s basis=%s",
		len(rows), want.ExcludedTranscripts, want.Visibility, want.RedactionLevel, want.IdentityBasis)
}

func assertLocalPublicationReceiptsMatchVillage(t *testing.T, sandbox disposableSandbox, origin, ownerID string, rows []villageTranscript) {
	t.Helper()
	dbPath := filepath.Join(sandbox.dataHome, string(defaults.AppName), "peasant.db")
	local, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open V43 receipt store %s: %v", dbPath, err)
	}
	defer local.Close()
	for _, remote := range rows {
		projectHash, hashErr := schema.NewProjectHash(readLocalProjectHash(t, dbPath, remote.LocalID))
		if hashErr != nil {
			t.Fatalf("validate project identity for %s: %v", remote.LocalID, hashErr)
		}
		record, readErr := local.Publication(context.Background(), origin, ownerID, projectHash, remote.LocalID)
		if readErr != nil || record == nil {
			t.Fatalf("read V43 receipt for %s: %#v, %v", remote.LocalID, record, readErr)
		}
		receipt := record.Receipt
		if receipt.TranscriptID.String() != remote.ID || receipt.Visibility.String() != remote.Visibility || receipt.ContentHash.String() != remote.ContentHash || receipt.RequestOperationFingerprint.String() != remote.OperationFingerprint || receipt.PublishedAt != remote.PublishedAtMillis || receipt.UpdatedAt != remote.UpdatedAtMillis {
			t.Errorf("V43 receipt for %s does not match authoritative Village read-back: receipt=%+v remote=%+v", remote.LocalID, receipt, remote)
		}
		if err := receipt.Validate(); err != nil {
			t.Errorf("persisted V43 receipt for %s is incomplete: %v", remote.LocalID, err)
		}
	}
}

// assertDisclosed requires every sentence a surface owes its user. The list form
// is what makes the requirement extensible without touching the test: a surface
// that gains a second downgrade to disclose gains a fixture row.
// TestPrePushHookRefusesUnsupportedRedactionWithoutBlockingThePush proves
// the guarantee that sits BETWEEN two packages, which is why neither covers it.
//
// The command layer proves an unsupported redaction level makes `village push`
// return an error. This slice's contract is the opposite direction: a Peasant
// failure prints an actionable warning and returns SUCCESS from the hook path,
// because Village availability must never block or undo ordinary Git work. A
// refusal is a NEW failure mode on that path. If it escapes as a non-zero hook
// exit, every commit in the repository breaks - which is worse than never
// publishing at all.
//
// The state is reachable and ordinary: installing under an unsupported level is
// refused outright, so the only way here is to change the setting AFTER the hook
// exists, which is exactly what a user does.
//
// It needs NO village stack: the refusal happens after the configuration is read
// and before the session store is opened or any request is made. The credential
// written below is never used against anything, and that is the point - it is
// what proves nothing was uploaded before the refusal.
//
// It carries the `e2e` tag despite needing no stack. It shares the substrate that
// builds the real binary and drives real git subprocesses, and un-tagging it
// would mean either pulling that substrate into the default gate or duplicating
// it into an untagged file - recreating the two-divergent-implementations defect
// this package exists to remove. Under `-tags=e2e` it runs anywhere the binary
// builds; the stack-dependent tests skip themselves.
func TestPrePushHookRefusesUnsupportedRedactionWithoutBlockingThePush(t *testing.T) {
	document, err := LoadHookJoinedEvidenceFixtures()
	if err != nil {
		t.Fatal(err)
	}
	want := hookJoinedEvidenceCaseByOutcome(t, document, outcomeRefuses)
	peasantBin := buildPeasant(t)
	sandbox := newDisposableSandbox(t, peasantBin)
	repository := sandbox.initRepository(t, "configured-repository")
	remote := sandbox.initBareRemote(t, "origin-remote")
	runGit(t, sandbox.environment, repository, "remote", "add", "origin", remote)
	runGit(t, sandbox.environment, repository, "push", "-u", "origin", "HEAD")

	// Install under the supported level: an unsupported one is refused outright,
	// so this is the only route to a repository whose hook cannot run.
	writeRedactionLevelConfig(t, sandbox, want.InstalledRedactionLevel)
	writeSandboxCredential(t, sandbox)
	runPeasantInSandbox(t, peasantBin, sandbox, "village", "hooks", "install",
		"--event", want.Event.String(), "--dir", repository, "--timeout", "45s")

	// Now the user edits the setting to one this version cannot apply.
	writeRedactionLevelConfig(t, sandbox, want.RedactionLevel)

	runGit(t, sandbox.environment, repository, "commit", "--allow-empty", "-m", "work to push under an unsupported redaction level")
	local := strings.TrimSpace(runGit(t, sandbox.environment, repository, "rev-parse", "HEAD"))
	pushOut, pushErr := gitCommand(sandbox.environment, repository, "push", "origin", "HEAD")
	if pushErr != nil {
		t.Fatalf("git push was BLOCKED by a redaction level the upload cannot apply: %v\n"+
			"Git honours a pre-push hook's exit status, so a refusal that escapes as a non-zero exit stops the user pushing at all.\n"+
			"The hook must warn and let Git proceed: a repository nobody can push from is worse than one that never publishes.\n%s",
			pushErr, pushOut)
	}
	// Exit status alone is not proof: a hook can exit 0 while the ref still does
	// not move, so the REMOTE is asked what it actually received.
	pushed := strings.TrimSpace(runGit(t, sandbox.environment, remote, "rev-parse", "HEAD"))
	if pushed != local {
		t.Fatalf("git push exited 0 but the remote is at %s while the local branch is at %s:\n%s", pushed, local, pushOut)
	}
	assertDisclosed(t, "the refusal a push surfaces", want.RefusalContains, pushOut)
	assertNeverEmitted(t, "the refusal a push surfaces", want.ForbiddenContains, pushOut)
	t.Logf("hook refusal evidence: event=%s configured=%s installed=%s git pushed successfully to %s",
		want.Event, want.RedactionLevel, want.InstalledRedactionLevel, pushed)
}

// writeRedactionLevelConfig writes a configuration carrying one redaction level,
// leaving everything else at its default.
func writeRedactionLevelConfig(t *testing.T, sandbox disposableSandbox, level redact.RedactionLevel) {
	t.Helper()
	writeDisposableSandboxConfig(t, sandbox, fmt.Sprintf(`version: 1
redaction:
  level: %s
output:
  basePath: %q
`, level, sandbox.transcriptOutputPath()))
}

// writeSandboxCredential writes a syntactically valid credential the run never
// uses. Credentials are read before the redaction level is checked, so without
// one the push would fail for the wrong reason and prove nothing about the
// refusal.
func writeSandboxCredential(t *testing.T, sandbox disposableSandbox) {
	t.Helper()
	directory := filepath.Join(sandbox.configHome, string(defaults.AppName))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create the sandbox credential directory %s: %v", directory, err)
	}
	// Every field auth.Credentials.IsValid() requires must be present, or the
	// push stops at the login gate and the refusal under test never runs.
	credential := `{"api_key":"peasant-e2e-unused-key","key_id":"peasant-e2e-unused","user_id":"peasant-e2e",` +
		`"username":"peasant-e2e","village_url":"http://127.0.0.1:1"}`
	if err := os.WriteFile(filepath.Join(directory, "credentials.json"), []byte(credential), 0o600); err != nil {
		t.Fatalf("write the sandbox credential in %s: %v", directory, err)
	}
}

func assertDisclosed(t *testing.T, surface string, required []string, output string) {
	t.Helper()
	for index, sentence := range required {
		sentence = requireNeedle(t, fmt.Sprintf("the sentence %s owes at index %d", surface, index), sentence)
		if !strings.Contains(output, sentence) {
			t.Errorf("%s did not disclose %q:\n%s", surface, sentence, output)
		}
	}
}

// assertNeverEmitted checks text a surface must never print. A blank needle here
// would fail rather than pass, so this direction is fail-safe; the hazard it
// guards is the opposite one, a needle pinned to wording that no longer exists,
// which the corpus handles by pinning short phrases.
func assertNeverEmitted(t *testing.T, surface string, forbidden []string, output string) {
	t.Helper()
	for _, phrase := range forbidden {
		if strings.Contains(output, phrase) {
			t.Errorf("%s emitted text it must never emit: %q\n%s", surface, phrase, output)
		}
	}
}

// hookJoinedEvidenceCaseByOutcome picks the single case of a kind, and fails if
// there is not exactly one - a corpus that gained a second case of a kind would
// otherwise have it silently ignored.
func hookJoinedEvidenceCaseByOutcome(t *testing.T, document hookJoinedEvidenceDocument, outcome hookJoinedEvidenceOutcome) hookJoinedEvidenceCase {
	t.Helper()
	var found []hookJoinedEvidenceCase
	for _, evidenceCase := range document.Cases {
		if evidenceCase.Outcome == outcome {
			found = append(found, evidenceCase)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the evidence corpus has %d cases with outcome %q, want exactly 1\n"+
			"why: each test drives one case of its kind, so a second would be silently ignored\n"+
			"where: internal/e2e/testdata/hook-joined-evidence.yaml\n"+
			"fix: keep one case per outcome, or teach the test which one it drives", len(found), outcome)
	}
	return found[0]
}

// requireNeedle refuses to assert on an empty expected substring.
//
// It is defence in depth behind the corpus loader, and it exists because the
// two directions fail differently. `strings.Contains(x, "")` is always true, so
// an empty needle under a NEGATED check is not a weaker assertion, it is a
// guaranteed pass, and the assertion silently stops existing. The loader keeps
// every corpus field non-blank, but a needle that is DERIVED - rendered from a
// typed value rather than read straight from the corpus - is outside those
// rules, so it needs the check at the point of use.
func requireNeedle(t *testing.T, what, needle string) string {
	t.Helper()
	if strings.TrimSpace(needle) == "" {
		t.Fatalf("refusing to assert on an empty expected substring: %s rendered nothing\n"+
			"why: strings.Contains against an empty needle is always true, so a negated check on it can never fail and the assertion stops existing\n"+
			"where: internal/e2e/hook_joined_e2e_test.go requireNeedle\n"+
			"when: about to assert that a surface's output contains it\n"+
			"means: this run would report success without checking anything\n"+
			"fix: give the value real text - either in testdata/hook-joined-evidence.yaml, or in whatever renders it", what)
	}
	return needle
}

func writeHookEvidenceConfig(t *testing.T, sandbox disposableSandbox, configuredSource, otherSource string, want hookJoinedEvidenceCase) {
	t.Helper()
	writeDisposableSandboxConfig(t, sandbox, fmt.Sprintf(`version: 1
redaction:
  level: %s
sources:
  claude-code:
    enabled: true
    paths: [%q, %q]
  opencode: {enabled: false}
  codex: {enabled: false}
  cursor: {enabled: false}
  strike: {enabled: false}
output:
  basePath: %q
push:
  visibility: %s
`, want.RedactionLevel, configuredSource, otherSource, sandbox.transcriptOutputPath(), want.Visibility))
}

// readAPIKey reads the sandbox credential the harness minted, so content is
// fetched as the same user the hook published as.
func readAPIKey(t *testing.T, configHome string) string {
	t.Helper()
	path := filepath.Join(configHome, string(defaults.AppName), "credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the sandbox Village credential at %s: %v", path, err)
	}
	var credentials struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(data, &credentials); err != nil || credentials.APIKey == "" {
		t.Fatalf("read the sandbox Village API key from %s: %v", path, err)
	}
	return credentials.APIKey
}

func getVillageTranscriptContent(t *testing.T, villageURL, apiKey, transcriptID string) string {
	t.Helper()
	status, body := villageAPIRequest(t, "GET", villageURL, "/api/v1/pull/transcripts/"+transcriptID+"/content", apiKey, nil)
	if status != 200 {
		t.Fatalf("Village content status = %d: %s", status, body)
	}
	return string(body)
}

// --- developer-state isolation guard ----------------------------------------

// developerStateLocations names every place on the developer's machine a joined
// hook run must leave byte-identical. They are RESOLVED paths, not raw
// environment reads: a guard built from `os.Getenv("XDG_STATE_HOME")` silently
// covers nothing on a machine that relies on the XDG defaults, which is the
// common case and was the case here.
type developerStateLocations struct {
	// GitHookDirectory is the peasant checkout's own hook directory, resolved
	// through Git. In a linked worktree `<checkout>/.git` is a FILE and the real
	// hook directory lives in the common directory, so joining `.git/hooks` by
	// hand names a path that does not exist — a guard over nothing.
	GitHookDirectory string
	// GlobalGitConfig is the developer's own global Git configuration.
	GlobalGitConfig string
	// ConfigDir, DataDir and StateDir are peasant's resolved XDG directories.
	ConfigDir string
	DataDir   string
	StateDir  string
	// ExcludedSubtree is the harness's own test area, which is excluded because
	// this run, concurrent runs, and the start-time stale-sandbox prune all
	// legitimately write there. Excluding it is what lets the guard fail on
	// developer state instead of on other test runs.
	ExcludedSubtree string
}

// roots returns the paths to fingerprint. A blank location is a FAILURE, not a
// skip: a silently-skipped root is a guard reporting success for work it never
// did, which is how this one came to cover three fewer directories than it said.
func (locations developerStateLocations) roots() ([]string, error) {
	named := []struct {
		field string
		path  string
	}{
		{"GitHookDirectory", locations.GitHookDirectory},
		{"GlobalGitConfig", locations.GlobalGitConfig},
		{"ConfigDir", locations.ConfigDir},
		{"DataDir", locations.DataDir},
		{"StateDir", locations.StateDir},
	}
	roots := make([]string, 0, len(named))
	for _, entry := range named {
		if strings.TrimSpace(entry.path) == "" {
			return nil, fmt.Errorf("developer-state isolation guard cannot run: %s resolved to an empty path\n"+
				"why: an empty root is silently walked as nothing, so the guard would report that state it never inspected was untouched\n"+
				"where: internal/e2e/hook_joined_e2e_test.go developerStateLocations.roots\n"+
				"when: resolving the directories to fingerprint, before the joined hook run starts\n"+
				"means: the claim that developer state outside the sandbox is unchanged would be unproven\n"+
				"fix: resolve %s (a peasant checkout with a Git directory, a readable HOME, and peasant's resolved XDG paths are all required) or run the joined evidence test from a full checkout", entry.field, entry.field)
		}
		roots = append(roots, entry.path)
	}
	if strings.TrimSpace(locations.ExcludedSubtree) == "" {
		return nil, fmt.Errorf("developer-state isolation guard cannot run: ExcludedSubtree resolved to an empty path\n" +
			"why: without it the guard walks the harness's own sandbox area, so this run's output, a concurrent run, or the stale-sandbox prune fails the guard instead of a real escape\n" +
			"where: internal/e2e/hook_joined_e2e_test.go developerStateLocations.roots\n" +
			"when: resolving the directories to fingerprint\n" +
			"means: the guard would false-fail whenever XDG_STATE_HOME is set, hiding real escapes behind noise\n" +
			"fix: pass sandboxBase(defaults.ResolveStateDirPath()) as the excluded subtree")
	}
	return roots, nil
}

// assertDeveloperStateUnchanged fingerprints developer state now and re-checks it
// when the test ends.
func assertDeveloperStateUnchanged(t *testing.T) {
	t.Helper()
	realStateDir := string(defaults.ResolveStateDirPath())
	locations := developerStateLocations{
		GitHookDirectory: resolveCheckoutHookDirectory(t),
		GlobalGitConfig:  resolveDeveloperGlobalGitConfig(t),
		ConfigDir:        string(defaults.ResolveConfigDirPath()),
		DataDir:          string(defaults.ResolveDataDirPath()),
		StateDir:         realStateDir,
		ExcludedSubtree:  sandboxBase(realStateDir),
	}
	roots, err := locations.roots()
	if err != nil {
		t.Fatal(err)
	}
	before := fingerprintPaths(t, roots, locations.ExcludedSubtree)
	t.Cleanup(func() {
		if after := fingerprintPaths(t, roots, locations.ExcludedSubtree); after.digest != before.digest {
			t.Errorf("joined hook evidence changed developer state outside its sandbox\nroots: %s\nbefore: %s\nafter: %s",
				strings.Join(roots, ", "), before.digest, after.digest)
		}
	})
}

func resolveCheckoutHookDirectory(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve the working directory of the joined hook evidence run: %v", err)
	}
	checkout := peasantRepoRoot(workingDirectory)
	if strings.TrimSpace(checkout) == "" {
		return ""
	}
	hooks, err := gitCommand(os.Environ(), checkout, "rev-parse", "--git-path", "hooks")
	if err != nil {
		t.Fatalf("resolve the peasant checkout's Git hook directory in %s: %v\n%s", checkout, err, hooks)
	}
	hooks = strings.TrimSpace(hooks)
	if hooks == "" {
		return ""
	}
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(checkout, hooks)
	}
	return hooks
}

func resolveDeveloperGlobalGitConfig(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		t.Fatalf("resolve the developer's home directory for the isolation guard: %v", err)
	}
	return filepath.Join(home, ".gitconfig")
}

type pathFingerprint struct {
	paths  []string
	digest string
}

// fingerprintPaths hashes every root, skipping anything inside excludedSubtree.
func fingerprintPaths(t *testing.T, paths []string, excludedSubtree string) pathFingerprint {
	t.Helper()
	h := sha256.New()
	for _, root := range paths {
		_, _ = h.Write([]byte(root))
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				_, _ = h.Write([]byte(err.Error()))
				return nil
			}
			if path == excludedSubtree || strings.HasPrefix(path, excludedSubtree+string(os.PathSeparator)) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				return nil
			}
			_, _ = h.Write([]byte(path + info.Mode().String() + fmt.Sprint(info.Size(), info.ModTime().UnixNano())))
			if !d.IsDir() {
				if b, readErr := os.ReadFile(path); readErr == nil {
					_, _ = h.Write(b)
				}
			}
			return nil
		})
	}
	return pathFingerprint{paths: paths, digest: hex.EncodeToString(h.Sum(nil))}
}

func TestDeveloperStateLocations_RejectEveryEmptyRoot(t *testing.T) {
	t.Parallel()
	complete := developerStateLocations{
		GitHookDirectory: "/checkout/.git/hooks",
		GlobalGitConfig:  "/home/dev/.gitconfig",
		ConfigDir:        "/home/dev/.config/peasant",
		DataDir:          "/home/dev/.local/share/peasant",
		StateDir:         "/home/dev/.local/state/peasant",
		ExcludedSubtree:  "/home/dev/.local/state/peasant/test/e2e",
	}
	if _, err := complete.roots(); err != nil {
		t.Fatalf("fully resolved locations must be accepted: %v", err)
	}
	blanked := map[string]func(*developerStateLocations){
		"GitHookDirectory": func(l *developerStateLocations) { l.GitHookDirectory = "" },
		"GlobalGitConfig":  func(l *developerStateLocations) { l.GlobalGitConfig = "" },
		"ConfigDir":        func(l *developerStateLocations) { l.ConfigDir = "" },
		"DataDir":          func(l *developerStateLocations) { l.DataDir = "" },
		"StateDir":         func(l *developerStateLocations) { l.StateDir = " " },
		"ExcludedSubtree":  func(l *developerStateLocations) { l.ExcludedSubtree = "" },
	}
	for field, blank := range blanked {
		locations := complete
		blank(&locations)
		roots, err := locations.roots()
		if err == nil {
			t.Errorf("%s resolved empty and the guard returned roots %v instead of failing", field, roots)
			continue
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("%s resolved empty and the failure did not name it: %v", field, err)
		}
	}
}
