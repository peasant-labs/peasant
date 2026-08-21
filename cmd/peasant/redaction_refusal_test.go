package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/redact"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/redaction_refusals.yaml
var redactionRefusalFixtureData []byte

const redactionRefusalFixturePath = "cmd/peasant/testdata/redaction_refusals.yaml"

// expectedOverclaimCount anchors the shared over-claim list's size from
// THIS package.
//
// A forbid-list cannot be anchored the way a case corpus is: the phrases are
// absent by design, so removing one changes no behaviour and fails nothing -
// measured, all nine deleted green. Both consumers carry their own count, so
// retiring a phrase takes an edit in three files across three packages rather
// than one line of YAML. It does not make removal hard; it makes it visible.
const expectedOverclaimCount = 9

// refusalSurface is a production entry point that reads a redaction level. The
// set is closed because the whole point is that EVERY door is covered: the level
// arrives from the configuration, from a flag, and from a request body, and a
// check applied at one door leaves the others open.
type refusalSurface string

const (
	surfacePush       refusalSurface = "push"
	surfaceHarvest    refusalSurface = "harvest"
	surfaceRedactFlag refusalSurface = "redact-flag"
	// surfaceRedactConfig is `peasant redact` with NO --level, so the level comes
	// from the configuration. It is a separate surface from surfaceRedactFlag
	// because they read different things, and only this one exercises the config
	// path - which is where the --config-dir bypass lived. Measured: with only the
	// flag surface covered, reverting the config read to the raw --config flag was
	// green, because the flag refusal fires first and hides it.
	surfaceRedactConfig refusalSurface = "redact-config"
	surfaceHooksInstall refusalSurface = "hooks-install"
	surfaceHooksStatus  refusalSurface = "hooks-status"
)

var allRefusalSurfaces = [...]refusalSurface{
	surfacePush, surfaceHarvest, surfaceRedactFlag, surfaceRedactConfig, surfaceHooksInstall, surfaceHooksStatus,
}

type redactionRefusalDocument struct {
	ExpectedCaseCount int                    `yaml:"expectedCaseCount"`
	Cases             []redactionRefusalCase `yaml:"cases"`
}

type redactionRefusalCase struct {
	Name          string                `yaml:"name"`
	Surface       refusalSurface        `yaml:"surface"`
	Level         redact.RedactionLevel `yaml:"level"`
	ExpectRefusal bool                  `yaml:"expectRefusal"`
	// ConfigDoor is HOW the configuration was selected. Blank means the flag door.
	ConfigDoor configDoor `yaml:"configDoor,omitempty"`
}

// configDoor is how a run is pointed at its configuration.
//
// Both are supported and they resolve differently, which is the point: --config
// names a FILE and --config-dir names the directory a file is found under. Two
// surfaces read the raw --config flag instead of the helper that honours
// --config-dir, and reading the flag directly returns its DEFAULT when unset - so
// under --config-dir they loaded a different configuration than the one the user
// pointed at and ran to completion under a level this version refuses.
//
// This corpus could not see that, because every case passed BOTH flags, and
// --config wins when both are present. It was driving the door that already
// worked.
type configDoor string

const (
	// doorConfigFile selects the configuration by path, with --config.
	doorConfigFile configDoor = "flag"
	// doorConfigDir selects it by directory, with --config-dir and NO --config.
	doorConfigDir configDoor = "directory"
)

var allConfigDoors = [...]configDoor{doorConfigFile, doorConfigDir}

// resolvedDoor is the row's door, defaulting to the flag so existing rows keep
// their meaning.
func (c redactionRefusalCase) resolvedDoor() configDoor {
	if c.ConfigDoor == "" {
		return doorConfigFile
	}
	return c.ConfigDoor
}

// loadRedactionRefusalFixture decodes and fully validates the corpus.
func loadRedactionRefusalFixture(data []byte) (redactionRefusalDocument, error) {
	var document redactionRefusalDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, redactionRefusalRuleError(
			"typed YAML fields must match the document schema", "loader=first-document decode",
			fmt.Sprintf("fix=remove unknown fields and match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, redactionRefusalRuleError(
			"exactly one YAML document is allowed; cases below a second one prove nothing",
			"loader=end-of-document check",
			fmt.Sprintf("fix=remove the second document so the next decode returns EOF: %v", err))
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, redactionRefusalRuleError(
			fmt.Sprintf("declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d",
				document.ExpectedCaseCount, len(document.Cases)),
			"loader=case-count validation",
			"fix=set expectedCaseCount to the number of cases present")
	}
	seen := map[string]bool{}
	refused := map[refusalSurface]bool{}
	refusedByDoor := map[refusalSurface]map[configDoor]bool{}
	warnedByDoor := map[configDoor]bool{}
	unaffectedBySurface := map[refusalSurface]map[redact.RedactionLevel]bool{}
	sawUnaffected := false
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, redactionRefusalRuleError(
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name),
				fmt.Sprintf("loader=case index %d", index),
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		if door := testCase.resolvedDoor(); door != doorConfigFile && door != doorConfigDir {
			return document, redactionRefusalRuleError(
				fmt.Sprintf("case %q selects its configuration through %q, which is not a door this product has", testCase.Name, door),
				fmt.Sprintf("loader=case index %d", index),
				fmt.Sprintf("fix=use %s or %s", doorConfigFile, doorConfigDir))
		}
		if !containsRefusalSurface(allRefusalSurfaces[:], testCase.Surface) {
			return document, redactionRefusalRuleError(
				fmt.Sprintf("case %q names an unknown surface %q", testCase.Name, testCase.Surface),
				fmt.Sprintf("loader=case index %d", index),
				"fix=use one of the surfaces the driver knows how to run")
		}
		if !testCase.Level.IsValid() {
			return document, redactionRefusalRuleError(
				fmt.Sprintf("case %q names an unknown redaction level %q", testCase.Name, testCase.Level),
				fmt.Sprintf("loader=case index %d", index),
				"fix=use one of minimal, standard, maximum")
		}
		// Self-consistency: a case cannot expect a refusal for a level the product
		// supports, or expect success for one it does not. Getting this wrong would
		// silently invert what the case proves.
		supported := config.RedactionLevelSupported(testCase.Level)
		if testCase.ExpectRefusal && supported {
			return document, redactionRefusalRuleError(
				fmt.Sprintf("case %q expects a refusal for the supported level %q", testCase.Name, testCase.Level),
				fmt.Sprintf("loader=case index %d", index),
				"fix=expect a refusal only for a level absent from config.SupportedRedactionLevels")
		}
		if !testCase.ExpectRefusal && !supported && testCase.Surface != surfaceHooksStatus {
			return document, redactionRefusalRuleError(
				fmt.Sprintf("case %q expects success for the unsupported level %q", testCase.Name, testCase.Level),
				fmt.Sprintf("loader=case index %d", index),
				"fix=expect a refusal, or use a supported level; only the read-only status surface reports instead of refusing")
		}
		if testCase.ExpectRefusal {
			refused[testCase.Surface] = true
			if refusedByDoor[testCase.Surface] == nil {
				refusedByDoor[testCase.Surface] = map[configDoor]bool{}
			}
			refusedByDoor[testCase.Surface][testCase.resolvedDoor()] = true
		} else if testCase.Surface == surfaceHooksStatus {
			// The read-only surface's obligation is to WARN, not to refuse, and it
			// is tracked per door for the same reason the refusals are: the warning
			// loads the configuration too.
			warnedByDoor[testCase.resolvedDoor()] = true
		} else if supported {
			sawUnaffected = true
			if unaffectedBySurface[testCase.Surface] == nil {
				unaffectedBySurface[testCase.Surface] = map[redact.RedactionLevel]bool{}
			}
			unaffectedBySurface[testCase.Surface][testCase.Level] = true
		}
	}
	for _, surface := range allRefusalSurfaces {
		if surface == surfaceHooksStatus {
			// Read-only: it reports the condition rather than refusing.
			continue
		}
		if !refused[surface] {
			return document, redactionRefusalRuleError(
				fmt.Sprintf("no case proves the %q surface refuses", surface),
				"loader=surface coverage",
				"fix=add one; the level reaches each surface through a different door, so an uncovered surface is an open door")
		}
	}
	if !sawUnaffected {
		return document, redactionRefusalRuleError(
			"no case proves a supported level still works",
			"loader=over-refusal coverage",
			"fix=add one; a hard failure is blunt, so the regression risk is refusing runs that should have proceeded")
	}
	// Over-refusal, per SURFACE and per SUPPORTED LEVEL rather than once anywhere.
	//
	// A single unaffected case satisfied the rule above, so every one of those rows
	// was individually deletable - measured, six of them, by the deletion pass.
	// Over-refusal is the live regression risk here, because the refusal is a hard
	// failure, and it is per-surface by nature: one surface starting to refuse a
	// level it must apply is exactly what "somebody proved it once" cannot see.
	for _, surface := range allRefusalSurfaces {
		if surface == surfaceHooksStatus {
			continue
		}
		for _, level := range config.SupportedRedactionLevels {
			if !unaffectedBySurface[surface][level] {
				return document, redactionRefusalRuleError(
					fmt.Sprintf("no case proves the %q surface is UNAFFECTED at the supported level %q", surface, level),
					"loader=per-surface over-refusal coverage",
					"fix=add one; a surface that starts refusing a level it must apply is invisible to a single unaffected "+
						"case proved somewhere else")
			}
		}
	}
	// The read-only surface is excluded from the REFUSAL rules because it does not
	// refuse - it reports the condition and exits 0, which is correct, since a hook
	// installed before the level changed is still sitting there. That exclusion was
	// right and was also too wide: it left hooks-status skipped by EVERY
	// corpus-level rule, so it was the one row of the corpus that deleted green,
	// and reverting its configuration read to the raw --config flag - verbatim the
	// defect closed on the other five surfaces - stayed green too. Under
	// --config-dir the warning simply vanished while status went on telling the
	// user to install a hook the install surface would refuse.
	//
	// So it is excluded from refusing, and covered for what it actually owes.
	for _, door := range allConfigDoors {
		if !warnedByDoor[door] {
			return document, redactionRefusalRuleError(
				fmt.Sprintf("no case proves the %q surface WARNS when its configuration is selected through the %q door",
					surfaceHooksStatus, door),
				"loader=read-only surface coverage",
				"fix=add one; this surface reports rather than refuses, so the refusal rules skip it - which left it "+
					"skipped by all of them. It still loads a configuration, and a warning that vanishes under a supported "+
					"flag sends the user into an install that refuses")
		}
	}
	for _, surface := range allRefusalSurfaces {
		if surface == surfaceHooksStatus {
			continue
		}
		// Every refusing surface must be proven through EVERY door that selects a
		// configuration. Surface coverage alone was not enough: two surfaces read
		// the raw --config flag, so under --config-dir they loaded a different
		// configuration than the one they were given and ran to completion under a
		// level this version refuses - while this corpus, which passed both flags
		// at once and let --config win, stayed green.
		for _, door := range allConfigDoors {
			if !refusedByDoor[surface][door] {
				return document, redactionRefusalRuleError(
					fmt.Sprintf("no case proves the %q surface refuses when its configuration is selected through the %q door",
						surface, door),
					"loader=config-door coverage",
					"fix=add one; --config names a file and --config-dir names a directory, and a surface reading the raw "+
						"flag honours only the first - which is a supported flag silently bypassing the refusal")
			}
		}
	}
	return document, nil
}

func redactionRefusalRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"redaction refusal fixture rule failed: %s; a malformed corpus invalidates the only evidence that an unsupported "+
			"redaction level stops every surface instead of quietly weakening one; where=%s %s; when=test fixture loading; "+
			"impact=content could be published at a protection level the user did not choose; %s",
		what, redactionRefusalFixturePath, where, fix)
}

func containsRefusalSurface(surfaces []refusalSurface, want refusalSurface) bool {
	for _, surface := range surfaces {
		if surface == want {
			return true
		}
	}
	return false
}

// --- loader guards ----------------------------------------------------------

func TestLoadRedactionRefusalFixture_RejectsACaseThatContradictsTheSupportedSet(t *testing.T) {
	t.Parallel()
	_, err := loadRedactionRefusalFixture([]byte(`expectedCaseCount: 1
cases:
  - name: refuse-a-supported-level
    surface: push
    level: standard
    expectRefusal: true
`))
	if err == nil || !strings.Contains(err.Error(), "expects a refusal for the supported level") {
		t.Fatalf("error = %v, want rejection of a case whose expectation contradicts the supported set", err)
	}
}

func TestLoadRedactionRefusalFixture_RejectsACorpusWithNoUnaffectedRun(t *testing.T) {
	t.Parallel()
	_, err := loadRedactionRefusalFixture([]byte(`expectedCaseCount: 5
cases:
  - name: push
    surface: push
    level: maximum
    expectRefusal: true
  - name: harvest
    surface: harvest
    level: maximum
    expectRefusal: true
  - name: redact
    surface: redact-flag
    level: maximum
    expectRefusal: true
  - name: redact-config-refuses
    surface: redact-config
    level: maximum
    expectRefusal: true
  - name: hooks
    surface: hooks-install
    level: maximum
    expectRefusal: true
`))
	if err == nil || !strings.Contains(err.Error(), "no case proves a supported level still works") {
		t.Fatalf("error = %v, want rejection of a corpus that cannot detect over-refusal", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestRedactionLevel_UnsupportedRefusesEverySurface drives the real commands.
//
// The user's ruling is that an unsupported redaction level hard-fails and says to
// change the setting, rather than running under weaker protection than was asked
// for. That has to hold at every door the level arrives through, and it must not
// leak into the levels that do work.
func TestRedactionLevel_UnsupportedRefusesEverySurface(t *testing.T) {
	t.Parallel()
	document, err := loadRedactionRefusalFixture(redactionRefusalFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			output, runErr := driveRefusalSurface(t, testCase)

			if !testCase.ExpectRefusal {
				if runErr != nil && errors.Is(runErr, config.ErrUnsupportedRedactionLevel) {
					t.Fatalf("a supported level must be entirely unaffected, but %s refused: %v", testCase.Surface, runErr)
				}
				if testCase.Surface == surfaceHooksStatus {
					// Read-only surface: it must report the broken condition rather
					// than fail, because a hook installed before the level changed
					// is still sitting there.
					for _, want := range []string{
						"redaction.level is",
						"hook can be installed until this changes",
						config.RecommendedRedactionLevel.String(),
					} {
						if !strings.Contains(output, want) {
							t.Errorf("status must report the broken hook, stating %q; got:\n%s", want, output)
						}
					}
				}
				return
			}

			if runErr == nil {
				t.Fatalf("%s must refuse an unsupported level; output:\n%s", testCase.Surface, output)
			}
			if !errors.Is(runErr, config.ErrUnsupportedRedactionLevel) {
				t.Fatalf("%s failed for the wrong reason: %v", testCase.Surface, runErr)
			}
			message := runErr.Error()
			for _, want := range []string{
				testCase.Level.String(),
				"not supported in this version",
				"redaction.level",
				config.RecommendedRedactionLevel.String(),
				config.RedactionLevelMenu(),
			} {
				if !strings.Contains(message, want) {
					t.Errorf("the refusal must state %q so the user can act on it; got:\n%s", want, message)
				}
			}
			if strings.Contains(output, "Usage:") {
				t.Errorf("a refusal must not print the flag listing into every commit; got:\n%s", output)
			}
		})
	}
}

// writeCfgUnder writes a configuration at the layout --config-dir implies:
// <config-dir>/peasant/config.yaml. It is a separate helper from writeCfg
// because the whole point of the directory door is that the PATH is derived
// rather than given.
func writeCfgUnder(t *testing.T, configHome, body string) {
	t.Helper()
	dir := filepath.Join(configHome, defaults.AppName.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the directory --config-dir points at: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write the config the directory door must find: %v", err)
	}
}

// driveRefusalSurface runs one production entry point under a configuration
// carrying the case's level, and returns what the user would see plus the error.
func driveRefusalSurface(t *testing.T, testCase redactionRefusalCase) (string, error) {
	t.Helper()
	dir := t.TempDir()
	configLevel := testCase.Level
	if testCase.Surface == surfaceRedactFlag {
		// The flag has to be proven to work PAST the configuration, so the
		// configuration deliberately carries a supported level here.
		configLevel = config.RecommendedRedactionLevel
	}
	cfgPath := writeCfg(t, dir, "refusal.yaml", fmt.Sprintf(
		"version: 1\npush:\n  method: all\n  visibility: private\nredaction:\n  level: %s\n", configLevel))

	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("state-dir", "", "")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	// The door under test. Passing BOTH flags is what hid the defect: --config
	// wins when both are present, so every case drove the door that already
	// worked. The directory door passes NO --config, exactly as a user would.
	scoped := []string{"--config", cfgPath, "--config-dir", dir, "--data-dir", dir, "--state-dir", dir}
	if testCase.resolvedDoor() == doorConfigDir {
		// Same directory, so ONLY the door changes: credentials and state resolve
		// exactly as in the flag case, and a failure can only be about which
		// configuration was found.
		writeCfgUnder(t, dir, fmt.Sprintf(
			"version: 1\npush:\n  method: all\n  visibility: private\nredaction:\n  level: %s\n", configLevel))
		scoped = []string{"--config-dir", dir, "--data-dir", dir, "--state-dir", dir}
	}

	switch testCase.Surface {
	case surfacePush:
		// Credentials are read before the level is checked, so the run has to get
		// that far to prove anything about the refusal.
		writeTestCredentials(t, dir)
		root.AddCommand(BuildPushCommand())
		root.SetArgs(append(append([]string{"push"}, scoped...), "--non-interactive", "--timeout", "30s"))
	case surfaceHarvest:
		root.AddCommand(BuildHarvestCommand())
		root.SetArgs(append(append([]string{"harvest"}, scoped...), "--source-provider=claude-code",
			"--source-path="+t.TempDir(), "--output="+filepath.Join(dir, "sync")))
	case surfaceRedactFlag:
		root.AddCommand(BuildRedactCommand())
		root.SetArgs(append(append([]string{"redact"}, scoped...), "--all", "--dry-run",
			"--level", testCase.Level.String()))
	case surfaceRedactConfig:
		// NO --level: the level must come from the configuration, which is the
		// read that ignored --config-dir.
		root.AddCommand(BuildRedactCommand())
		root.SetArgs(append(append([]string{"redact"}, scoped...), "--all", "--dry-run"))
	case surfaceHooksInstall:
		repo := hooksTestRepo(t)
		root.AddCommand(BuildVillageCommand())
		root.SetArgs(append(append([]string{"village", "hooks", "install"}, scoped...),
			"--dir", repo, "--event", "post-commit"))
		err := root.Execute()
		// The untouched-slot guarantee belongs to a REFUSAL: nothing may have been
		// written. An unaffected case is the opposite claim - the install must be
		// allowed to proceed - so asserting it there would require the command to
		// fail at a supported level, which is the over-refusal this row exists to
		// rule out.
		if testCase.ExpectRefusal {
			assertHookSlotUntouched(t, repo, githooks.EventPostCommit)
		}
		return out.String(), err
	case surfaceHooksStatus:
		repo := hooksTestRepo(t)
		root.AddCommand(BuildVillageCommand())
		root.SetArgs(append(append([]string{"village", "hooks", "status"}, scoped...), "--dir", repo))
	}
	// Execute FIRST, then read the buffer.
	//
	// `return out.String(), root.Execute()` does NOT work: Go evaluates return
	// operands left to right, so the buffer is read before Execute has written
	// anything and every assertion runs against "".
	//
	// Worth stating because of how it generalises: here it failed loudly, since the
	// assertions demanded content. THE SAME SHAPE SILENTLY PASSES IN ANY TEST THAT
	// ASSERTS THE ABSENCE OF A STRING - a forbidden-phrase check against an empty
	// buffer always succeeds. Nothing in a fixture or a loader can see that,
	// because the emptiness is produced by the harness rather than by the data.
	err := root.Execute()
	return out.String(), err
}

// assertHookSlotUntouched proves a refused install created nothing. A refusal that
// still wrote the hook would be worse than no refusal, because the user would be
// told nothing happened while a hook that fails on every commit sat there.
func assertHookSlotUntouched(t *testing.T, repo string, event githooks.Event) {
	t.Helper()
	slot := filepath.Join(repo, ".git", "hooks", event.String())
	if _, err := os.Stat(slot); !os.IsNotExist(err) {
		t.Errorf("a refused install must leave the hook slot empty, but %s exists (stat error: %v)", slot, err)
	}
}

// TestKickstart_CannotProduceAnUnsupportedRedactionLevel holds the wizard to the
// levels the product can run.
//
// Hard-fail plus a still-selectable maximum is the worst combination available: a
// user would complete onboarding and find that the very next import and upload
// refuse. The wizard's own option list is the guard, and it is checked against the
// supported set rather than against a hardcoded expectation.
func TestKickstart_CannotProduceAnUnsupportedRedactionLevel(t *testing.T) {
	t.Parallel()
	offered := ftue.PrivacyLevels()
	if len(offered) == 0 {
		t.Fatal("the wizard must offer at least one redaction level")
	}
	for _, level := range offered {
		if !config.RedactionLevelSupported(level) {
			t.Errorf("the wizard offers %q, which every command refuses to run; a config it writes would break the next "+
				"import and the next upload", level)
		}
	}
	if slicesContainsLevel(offered, redact.Maximum) {
		t.Error("maximum must not be selectable: it is not supported in this version")
	}
	// The recommended level has to remain reachable, or onboarding cannot produce
	// a working configuration at all.
	if !slicesContainsLevel(offered, config.RecommendedRedactionLevel) {
		t.Errorf("the wizard must still offer the recommended level %q; got %v", config.RecommendedRedactionLevel, offered)
	}
}

func slicesContainsLevel(levels []redact.RedactionLevel, want redact.RedactionLevel) bool {
	for _, level := range levels {
		if level == want {
			return true
		}
	}
	return false
}

// --- accuracy of the claims the product still makes -------------------------

// TestRedactionClaims_AreBestEffortNotAbsolute guards a promise the product
// cannot keep.
//
// Redaction is pattern matching: it removes the shapes it recognizes. It cannot
// guarantee it found every piece of personal data or every identifying path, and a
// user who reads "removes personal data" may publish something believing it was
// cleaned. Secrets are the one place more confidence is warranted, and not because
// of this matching - the village runs its own scan and rejects a publish carrying
// them - so a confident verb belongs on secrets alone.
//
// This is a WORDING guard on purpose: the next person to edit these strings will
// not have read the decision, and the failure mode is a sentence that reads well
// and is not true.
func TestRedactionClaims_AreBestEffortNotAbsolute(t *testing.T) {
	t.Parallel()
	// Every user-facing rendering of a redaction claim, gathered here so the check
	// cannot pass by testing only the string someone remembered.
	surfaces := map[string]string{
		"the refusal": (&config.UnsupportedRedactionLevelError{
			Level: redact.Maximum, Source: "s", Operation: "o", Step: "w", Impact: "i.",
		}).Error(),
	}
	for index, level := range ftue.PrivacyLevels() {
		surfaces[fmt.Sprintf("the wizard's %s description", level)] = ftue.PrivacyDescriptions()[index]
	}

	// The phrases come from the shared corpus, and the comparison is
	// case-INSENSITIVE. Both matter, and the second is why the first exists.
	//
	// This list used to be written out here and again in internal/config, and the
	// two drifted on exactly the detail that decides whether the guard fires: the
	// other one lower-cased the text before comparing and this one did not.
	// Measured: `Removes personal data.` planted at the START of the wizard
	// description - which is how an author would actually write it - was GREEN,
	// while the identical words lower-cased mid-sentence were RED. The likeliest
	// form of the regression was the one form invisible to the guard.
	overclaims, err := testutil.Overclaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(overclaims) < expectedOverclaimCount {
		t.Fatalf("the shared over-claim list holds %d phrases, below the %d this surface expects. Each names a sentence "+
			"that promises a category is fully redacted, which pattern matching cannot deliver; if one was retired, say "+
			"why in the corpus header and update the count here and in internal/config too.",
			len(overclaims), expectedOverclaimCount)
	}
	for name, text := range surfaces {
		for _, claim := range overclaims {
			if claim.Asserts(text) {
				t.Errorf("%s claims completeness with %q, which pattern matching cannot deliver; say it redacts KNOWN "+
					"PATTERNS instead.\nthat phrasing usually arrives as: %s\ngot:\n%s", name, claim.Needle, claim.Why, text)
			}
		}
	}

	// And the refusal has to say the best-effort part out loud, not merely avoid
	// the absolute phrasing.
	refusal := surfaces["the refusal"]
	for _, want := range []string{"known patterns", "best effort"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal must state that redaction is best effort, containing %q; got:\n%s", want, refusal)
		}
	}
	// And it must NOT credit a backstop wider than the one that exists.
	//
	// This assertion used to REQUIRE the sentence to credit the village's scan for
	// secrets, as the one protection independent of pattern matching. The village
	// scans the transcript part and not the metadata part published beside it, so
	// the credit was broader than the check: a reader was told a second net was
	// under everything when it is under part of it. The clause is gone from the
	// shared sentence, and this is the guard on its return - it reached six
	// surfaces at once from one constant, so a re-add would be as cheap and as
	// wide as the removal.
	for _, overClaim := range []string{"village's own scan", "second, independent check"} {
		if strings.Contains(refusal, overClaim) {
			t.Errorf("the refusal credits %q. The village scans only the transcript part, so a promise of a second net "+
				"over the whole publish is wider than the check; state what Peasant does and leave the village's own "+
				"behaviour to the village.\ngot:\n%s", overClaim, refusal)
		}
	}
}

// TestRefusalKeepsTheAssertedTokensLiteral protects a DOWNSTREAM assertion from
// vanishing.
//
// The end-to-end oracle pins `redaction.level` and the recommended level in this
// message. strings.Contains is satisfied by an empty needle, so if either token
// were assembled by something that can render nothing, that assertion would
// silently stop existing - and no loader on either side of the package boundary
// could see it, because a loader validating its inputs does not protect a needle
// computed from them.
//
// Both are therefore fixed text or a constant: `redaction.level` is a literal, and
// the level comes from config.RecommendedRedactionLevel rather than from a
// renderer or a Describe().
func TestRefusalKeepsTheAssertedTokensLiteral(t *testing.T) {
	t.Parallel()
	refusal := (&config.UnsupportedRedactionLevelError{
		Level: redact.Maximum, Source: "s", Operation: "o", Step: "w", Impact: "i.",
	}).Error()
	// Located in the FIX clause specifically, not anywhere in the message. Both
	// tokens also occur in the supported-levels list, so a whole-message check
	// cannot tell which occurrence it found - and the remedy losing them while the
	// list still carries them is exactly the regression worth catching.
	fixClause := ""
	for _, line := range strings.Split(refusal, "\n") {
		if strings.HasPrefix(line, "Fix: ") {
			fixClause = line
		}
	}
	if fixClause == "" {
		t.Fatalf("the refusal has no Fix line, so there is no remedy to pin; got:\n%s", refusal)
	}
	// One exact substring, joining the two tokens in the order the remedy states
	// them. Checking them separately is not enough: both also occur in the
	// supported-levels list on this same line, so an empty level would leave each
	// token findable while the remedy said "set redaction.level to  ".
	remedy := "set redaction.level to " + config.RecommendedRedactionLevel.String()
	if config.RecommendedRedactionLevel.String() == "" {
		t.Fatal("the recommended level rendered empty, so asserting it downstream would prove nothing")
	}
	if !strings.Contains(fixClause, remedy) {
		t.Errorf("the Fix clause must name the setting and the value together, as the literal %q, because that pair is what a "+
			"downstream assertion pins and what a user acts on; got: %s", remedy, fixClause)
	}
}

// TestRedactionRefusal_CannotBreakGitUnderAnInstalledHook holds the refusal to the
// one guarantee a Git hook must never violate.
//
// The reachable sequence is the one the status warning describes: a hook installed
// while the level was supported, then a configuration edit to an unsupported one.
// From that point every commit runs a push that refuses. Village availability must
// never block or undo ordinary Git work, so what the generated hook needs from this
// command is exact:
//
//   - a status of ExitNothingAttempted, NOT a generic failure. The hook branches on
//     it, and on a generic failure it tells the user "whatever the upload finished
//     is on the village and is recorded as published" - which would be false for a
//     refusal that never contacted anything. The hook itself always exits 0, so git
//     completes either way; the risk here is a true exit code carrying a false
//     message.
//   - no usage block. Cobra prints one on error by default, and a flag listing
//     dumped into every commit is a user-facing defect even with a correct status.
//   - a message that survives --quiet, which is what the hook always passes.
//
// Driven at the command level rather than through a real hook: the hook's own
// always-exit-0 structure is proven in the githooks package, and this is the half
// that is new.
func TestRedactionRefusal_CannotBreakGitUnderAnInstalledHook(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)
	cfgPath := writeCfg(t, dir, "hooked.yaml",
		"version: 1\npush:\n  method: all\n  visibility: private\nredaction:\n  level: maximum\n")

	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("state-dir", "", "")
	root.AddCommand(BuildPushCommand())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	// Exactly the flags the generated hook passes.
	root.SetArgs([]string{
		"push", "--config", cfgPath, "--config-dir", dir, "--data-dir", dir, "--state-dir", dir,
		"--non-interactive", "--quiet", "--timeout", "30s",
	})
	err := root.Execute()
	if err == nil {
		t.Fatalf("the push must refuse; output:\n%s", out.String())
	}

	// The status the hook reads. Anything else makes it describe a partial publish
	// that never happened.
	if got := exitCodeFor(err); got != defaults.ExitNothingAttempted {
		t.Errorf("exit status = %v, want %v: the hook uses this to tell the user whether anything reached the village, and a "+
			"refusal reached nothing", got, defaults.ExitNothingAttempted)
	}
	if strings.Contains(out.String(), "Usage:") || strings.Contains(err.Error(), "Usage:") {
		t.Errorf("a refusal must not print the flag listing into every commit; got:\n%s", out.String())
	}
	// Still actionable at the output level a hook runs with.
	for _, want := range []string{
		redact.Maximum.String(),
		"set redaction.level to " + config.RecommendedRedactionLevel.String(),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must remain actionable under --quiet, stating %q; got:\n%s", want, err.Error())
		}
	}
}
