package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/store/storetest/residuefixture"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/store_upgrade_policy.yaml
var storeUpgradePolicyYAML []byte

//go:embed testdata/store_upgrade_policy.manifest.yaml
var storeUpgradePolicyManifestYAML []byte

type storeUpgradePolicyCase struct {
	Name                string `yaml:"name"`
	Entry               string `yaml:"entry"`
	Mode                string `yaml:"mode"`
	Store               string `yaml:"store"`
	Confirm             bool   `yaml:"confirm,omitempty"`
	TightDisk           bool   `yaml:"tightDisk,omitempty"`
	Answer              string `yaml:"answer,omitempty"`
	Want                string `yaml:"want"`
	WantRan             bool   `yaml:"wantRan,omitempty"`
	Exit                int    `yaml:"exit"`
	HookScriptExitsZero bool   `yaml:"hookScriptExitsZero,omitempty"`
}

type storeUpgradeStep76Case struct {
	Name       string `yaml:"name"`
	LongStore  string `yaml:"longStore"`
	QuickStore string `yaml:"quickStore"`
}

type storeUpgradePushCase struct {
	Name   string `yaml:"name"`
	Caller string `yaml:"caller"`
}

type storeUpgradeEntryCase struct {
	Name       string `yaml:"name"`
	Entry      string `yaml:"entry"`
	Store      string `yaml:"store"`
	Confirm    bool   `yaml:"confirm,omitempty"`
	WantRefuse bool   `yaml:"wantRefuse"`
	WantOpened bool   `yaml:"wantOpened,omitempty"`
}

type storeUpgradeNamedCase struct {
	Name string `yaml:"name"`
}

type storeUpgradePolicyFixtures struct {
	PolicyCases    []storeUpgradePolicyCase `yaml:"policyCases"`
	Step76Cases    []storeUpgradeStep76Case `yaml:"step76Cases"`
	PushCases      []storeUpgradePushCase   `yaml:"pushCases"`
	EntryCases     []storeUpgradeEntryCase  `yaml:"entryCases"`
	KickstartCases []storeUpgradeNamedCase  `yaml:"kickstartCases"`
	SeamCases      []storeUpgradeNamedCase  `yaml:"seamCases"`
}

// loadStoreUpgradePolicyFixtures decodes the policy fixture and checks every
// case name against the required-name manifest, in both directions.
func loadStoreUpgradePolicyFixtures(t *testing.T) storeUpgradePolicyFixtures {
	t.Helper()
	var fixtures storeUpgradePolicyFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(storeUpgradePolicyYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode store_upgrade_policy.yaml: %v", err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decoder = yaml.NewDecoder(bytes.NewReader(storeUpgradePolicyManifestYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode store_upgrade_policy manifest: %v", err)
	}
	actual := map[string]bool{}
	add := func(name string) {
		if actual[name] {
			t.Fatalf("store_upgrade_policy.yaml repeats case name %q", name)
		}
		actual[name] = true
	}
	for _, c := range fixtures.PolicyCases {
		add(c.Name)
	}
	for _, c := range fixtures.Step76Cases {
		add(c.Name)
	}
	for _, c := range fixtures.PushCases {
		add(c.Name)
	}
	for _, c := range fixtures.EntryCases {
		add(c.Name)
	}
	for _, c := range fixtures.KickstartCases {
		add(c.Name)
	}
	for _, c := range fixtures.SeamCases {
		add(c.Name)
	}
	required := map[string]bool{}
	for _, name := range manifest.RequiredNames {
		required[name] = true
		if !actual[name] {
			t.Fatalf("store upgrade policy required case %q is missing from the fixture", name)
		}
	}
	for name := range actual {
		if !required[name] {
			t.Fatalf("store upgrade policy case %q is missing from the required-names manifest", name)
		}
	}
	return fixtures
}

func policyFileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// stubTerminal makes the one terminal check answer available for the rest
// of the test. Tests that use it never run in parallel.
func stubTerminal(t *testing.T, available bool) {
	t.Helper()
	previous := requireTerminal
	requireTerminal = func(command string) error {
		if available {
			return nil
		}
		return noTerminalError{command: command}
	}
	t.Cleanup(func() { requireTerminal = previous })
}

// buildLongV75Store writes a real schema-75 store whose plan has one long
// step that a real upgrade can run: the retired search index is missing, so
// the repair rebuilds the index.
func buildLongV75Store(t *testing.T, path string) {
	t.Helper()
	storetest.BuildCleanV75Store(t, path)
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `DROP TABLE session_entries_fts`, nil); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

// seedPolicyStore seeds the named fixture store under dir's data directory
// and returns the database path.
func seedPolicyStore(t *testing.T, dir, kind string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, string(defaults.AppName)), 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	switch kind {
	case "long", "affected":
		residuefixture.SeedAffected(t, dbPath)
	case "quick", "clean":
		residuefixture.SeedClean(t, dbPath)
	case "real-v75":
		storetest.BuildCleanV75Store(t, dbPath)
	case "real-v75-long":
		buildLongV75Store(t, dbPath)
	case "current":
		storetest.CopyGoldenTo(t, dbPath)
	case "missing":
	default:
		t.Fatalf("unknown policy fixture store %q", kind)
	}
	return dbPath
}

// policyTestCmd is a command scoped to dir, with the flag a generated hook
// passes when nonInteractive is set.
func policyTestCmd(t *testing.T, dir string, nonInteractive bool, stdin string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := &cobra.Command{Use: "probe"}
	cmd.Flags().String("data-dir", "", "")
	if err := cmd.Flags().Set("data-dir", dir); err != nil {
		t.Fatal(err)
	}
	if nonInteractive {
		cmd.Flags().Bool("non-interactive", false, "")
		if err := cmd.Flags().Set("non-interactive", "true"); err != nil {
			t.Fatal(err)
		}
	}
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(stdin))
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	return cmd, &stderr
}

// TestStoreUpgradePolicy drives the entry-point table through the one
// helper. The upgrade itself is replaced by a recorder: the store package
// tests the upgrade; this test pins who may start one.
func TestStoreUpgradePolicy(t *testing.T) {
	for _, c := range loadStoreUpgradePolicyFixtures(t).PolicyCases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			mode, err := newStoreEntryMode(c.Mode)
			if err != nil {
				t.Fatal(err)
			}
			want, err := newStoreUpgradeAction(c.Want)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			dbPath := seedPolicyStore(t, dir, c.Store)
			before := ""
			if c.Store != "missing" {
				before = policyFileHash(t, dbPath)
			}

			stubTerminal(t, mode == storeEntryTerminal)
			cmd, stderr := policyTestCmd(t, dir, mode == storeEntryHook, c.Answer+"\n")
			resolved := storeEntryServer
			if mode != storeEntryServer {
				// Every command entry point classes itself with this one
				// function; a server entry point passes the server mode.
				resolved = storeEntryModeFor(cmd)
			}
			if resolved != mode {
				t.Fatalf("entry %s resolved to mode %q, want %q", c.Entry, resolved, mode)
			}

			if c.TightDisk {
				previousPlan := planStoreUpgrade
				planStoreUpgrade = func(path string) (store.UpgradePlan, error) {
					plan, err := previousPlan(path)
					plan.FreeBytesKnown = true
					plan.RequiredFreeBytes = 1000
					plan.FreeBytes = defaults.StoreUpgradeTightDiskMultiple*plan.RequiredFreeBytes - 1
					return plan, err
				}
				t.Cleanup(func() { planStoreUpgrade = previousPlan })
			}
			ran := 0
			previousUpgrade := upgradeStore
			upgradeStore = func(context.Context, string, store.UpgradeOptions) (store.UpgradeReport, error) {
				ran++
				return store.UpgradeReport{}, nil
			}
			t.Cleanup(func() { upgradeStore = previousUpgrade })

			plan, err := planStoreUpgrade(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := decideStoreUpgrade(mode, plan, c.Confirm); got != want {
				t.Fatalf("policy for %s on a %s store = %q, want %q", mode, c.Store, got, want)
			}

			err = ensureStoreCurrentConfirmed(cmd, dbPath, mode, c.Confirm)
			got := 0
			if err != nil {
				got = int(exitCodeFor(err))
			}
			if got != c.Exit {
				t.Errorf("exit status = %d (error: %v), want %d", got, err, c.Exit)
			}
			prompted := strings.Contains(stderr.String(), "upgrade the store now?")
			if prompted != (want == storeUpgradeAsk) {
				t.Errorf("prompt shown = %v, want %v; stderr:\n%s", prompted, want == storeUpgradeAsk, stderr.String())
			}
			wantRan := want == storeUpgradeRun || (want == storeUpgradeAsk && c.WantRan)
			if (ran == 1) != wantRan || ran > 1 {
				t.Errorf("the upgrade started %d time(s), want started = %v", ran, wantRan)
			}
			if wantRan {
				if err != nil {
					t.Errorf("the upgrade ran but the helper returned %v", err)
				}
				if !strings.Contains(stderr.String(), "needs an upgrade: schema 75 to") {
					t.Errorf("the steps were not named before the upgrade ran; stderr:\n%s", stderr.String())
				}
			}
			var refusal *storeUpgradeRefusal
			refused := errors.As(err, &refusal)
			switch want {
			case storeUpgradeNone:
				if err != nil || stderr.Len() != 0 {
					t.Errorf("a store with no pending step produced error %v and output %q", err, stderr.String())
				}
			case storeUpgradeSkip, storeUpgradeRefuse:
				if !refused {
					t.Fatalf("error = %v, want the typed refusal", err)
				}
				var nothingAttempted *pushNothingAttemptedError
				if errors.As(err, &nothingAttempted) != (want == storeUpgradeSkip) {
					t.Errorf("nothing-attempted wrap = %v, want %v: only a hook skip uses that status", !errors.As(err, &nothingAttempted), want == storeUpgradeSkip)
				}
			case storeUpgradeAsk:
				if !c.WantRan && (!refused || !refusal.Declined) {
					t.Errorf("a declined prompt returned %v, want the declined refusal", err)
				}
			}
			if refused {
				for _, part := range append([]string{"what:", "why:", "where:", "when:", "meaning:", "fix:"}, residuefixture.RefusalFragments...) {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("refusal misses %q:\n%s", part, err)
					}
				}
				if mode == storeEntryHook && !strings.Contains(err.Error(), "your git command is not affected") {
					t.Errorf("the hook refusal does not say git is unaffected:\n%s", err)
				}
			}
			if !wantRan && before != "" {
				if got := policyFileHash(t, dbPath); got != before {
					t.Error("the database file changed although the upgrade never started")
				}
				for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
					if _, statErr := os.Stat(sidecar); !os.IsNotExist(statErr) {
						t.Errorf("%s exists although the upgrade never started", sidecar)
					}
				}
			}
			if c.HookScriptExitsZero {
				for _, event := range githooks.AllEvents {
					script, scriptErr := githooks.Script(event, "/tmp/repo", "/tmp/repo/.git/hooks/"+event.String(), githooks.Binding{})
					if scriptErr != nil {
						t.Fatal(scriptErr)
					}
					if !strings.Contains(script, "\nfi\n\nexit 0\n# END GENERATED") || strings.Contains(script, "exit \"$peasant_hook_status\"") {
						t.Errorf("the generated %s hook does not end in exit 0; a skipped upgrade would block git", event)
					}
					if !strings.Contains(script, `-eq `+defaults.ExitNothingAttempted.String()+` ]`) {
						t.Errorf("the generated %s hook does not recognize the nothing-attempted status %s", event, defaults.ExitNothingAttempted)
					}
				}
			}
		})
	}
}

// TestStoreUpgradeStep76Cost ties the search repair's cost to the policy:
// the same schema-75 store is a long plan only with stale search results.
func TestStoreUpgradeStep76Cost(t *testing.T) {
	for _, c := range loadStoreUpgradePolicyFixtures(t).Step76Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			long, err := store.PlanUpgrade(seedPolicyStore(t, t.TempDir(), c.LongStore))
			if err != nil {
				t.Fatal(err)
			}
			quick, err := store.PlanUpgrade(seedPolicyStore(t, t.TempDir(), c.QuickStore))
			if err != nil {
				t.Fatal(err)
			}
			if len(long.Steps) != 1 || long.Steps[0].Version != 76 {
				t.Fatalf("long store plan = %+v, want the one step that produces schema 76", long.Steps)
			}
			if len(quick.Steps) != 1 || quick.Steps[0].Version != 76 {
				t.Fatalf("quick store plan = %+v, want the one step that produces schema 76", quick.Steps)
			}
			if long.Steps[0].Cost != store.UpgradeCostScalesWithText || !long.HasLongStep() {
				t.Errorf("step 76 on a store with stale search results costs %q, want scales_with_text", long.Steps[0].Cost)
			}
			if quick.Steps[0].Cost != store.UpgradeCostQuick || quick.HasLongStep() {
				t.Errorf("step 76 on a store without stale search results costs %q, want quick", quick.Steps[0].Cost)
			}
			if got := decideStoreUpgrade(storeEntryPiped, long, false); got != storeUpgradeRefuse {
				t.Errorf("piped policy on the long plan = %q, want refused", got)
			}
			if got := decideStoreUpgrade(storeEntryPiped, quick, false); got != storeUpgradeRun {
				t.Errorf("piped policy on the quick plan = %q, want ran", got)
			}
		})
	}
}

// pushPolicyArgs builds the argument list of a push caller. The hook callers
// are rendered by the real hook generator, so the gate is tested against the
// exact command a generated hook runs.
func pushPolicyArgs(t *testing.T, caller string) []string {
	t.Helper()
	var binding githooks.Binding
	switch caller {
	case "script":
		return []string{"--non-interactive"}
	case "hook-with-rule":
		binding = githooks.Binding{RequireAutoPublishRule: true, AutoPublishEvent: githooks.EventPostCommit}
	case "hook-without-rule":
		binding = githooks.Binding{}
	default:
		t.Fatalf("unknown push caller %q", caller)
	}
	argv := githooks.RepositoryArgv("", binding)
	if len(argv) < 3 || argv[0] != "peasant" || argv[1] != "village" || argv[2] != "push" {
		t.Fatalf("generated hook argv = %q; want it to start with peasant village push", argv)
	}
	return argv[3:]
}

// TestPushOnABehindStoreIsNothingAttempted runs the real push command on a
// store with a long step: every caller is refused before any village
// request and exits with the nothing-attempted status.
func TestPushOnABehindStoreIsNothingAttempted(t *testing.T) {
	stubTerminal(t, false)
	for _, c := range loadStoreUpgradePolicyFixtures(t).PushCases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := seedPolicyStore(t, dir, "affected")
			writeTestCredentials(t, dir)
			before := policyFileHash(t, dbPath)
			_, _, err := executeWithDataDirStreams(t, BuildPushCommand(), dir, pushPolicyArgs(t, c.Caller))
			if err == nil {
				t.Fatal("push on a store with a long step succeeded; want the refusal")
			}
			var nothingAttempted *pushNothingAttemptedError
			if !errors.As(err, &nothingAttempted) {
				t.Fatalf("push err = %v; want it marked nothing attempted", err)
			}
			var refusal *storeUpgradeRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("push err = %v; want the store upgrade refusal as its cause", err)
			}
			if refusal.Mode != storeEntryHook {
				t.Errorf("refusal mode = %q, want hook: every caller here passes --non-interactive", refusal.Mode)
			}
			if got := exitCodeFor(err); got != defaults.ExitNothingAttempted {
				t.Errorf("exit code = %d, want the nothing-attempted status %d", got, defaults.ExitNothingAttempted)
			}
			for _, fragment := range residuefixture.RefusalFragments {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("refusal misses %q:\n%s", fragment, err)
				}
			}
			if got := policyFileHash(t, dbPath); got != before {
				t.Error("refused push modified the database file")
			}
			if version, verr := store.SchemaVersionAt(dbPath); verr != nil || version != 75 {
				t.Errorf("store version = %d, err = %v; want the untouched 75", version, verr)
			}
		})
	}
}

func policySidecars(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, suffix := range []string{"-wal", "-shm"} {
		data, err := os.ReadFile(dbPath + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		found[suffix] = string(data)
	}
	return found
}

// TestStoreUpgradeEntryPoints runs the real entry functions with no
// terminal: each one refuses a store with a long step without touching it,
// and each one that may upgrade does not refuse a quick plan.
func TestStoreUpgradeEntryPoints(t *testing.T) {
	stubTerminal(t, false)
	for _, c := range loadStoreUpgradePolicyFixtures(t).EntryCases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := seedPolicyStore(t, dir, c.Store)
			before := policyFileHash(t, dbPath)
			sidecars := policySidecars(t, dbPath)
			cmd, _ := policyTestCmd(t, dir, false, "")
			var err error
			switch c.Entry {
			case "openDB":
				var closeDB func()
				_, closeDB, err = openDB(cmd)
				if err == nil {
					closeDB()
				}
			case "openRunStore":
				var db *store.Store
				db, err = openRunStore(cmd, false, t.TempDir(), ingest.DefaultWriteConfig(1))
				if err == nil {
					_ = db.Close()
				}
			case "openVerifyContentStore":
				var closeDB func()
				_, closeDB, err = openVerifyContentStore(cmd, t.TempDir(), dbPath, false)
				if err == nil {
					closeDB()
				}
			case "openKickstartStore":
				var closeDB func()
				_, closeDB, err = openKickstartStoreWithError(cmd, t.TempDir())
				if err == nil {
					closeDB()
				}
			case "webPreopen":
				err = prepareStoreForWebStart(cmd, resolveWebStoreTarget(cmd, nil), cmd.ErrOrStderr())
			case "runWebForeground":
				cfgPath := writeTestConfigFile(t, dir)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				cmd.SetContext(ctx)
				err = runWebForeground(cmd, cfgPath, 0, false, "", false)
			case "ftueRunner":
				cfgPath := writeTestConfigFile(t, dir)
				runner, _ := buildFTUEIngestRunnerWithProgress(cmd, cfgPath)
				_, err = runner(context.Background(), ftue.WizardAnswers{})
			case "runVerify":
				err = runVerify(cmd, false, false, false)
			case "openMigrateStore":
				var closeDB func()
				_, closeDB, err = openMigrateStore(cmd, false, c.Confirm, t.TempDir(), dbPath)
				if err == nil {
					closeDB()
				}
			case "openReclaimStore":
				var closeDB func()
				_, closeDB, err = openReclaimStore(cmd, false, c.Confirm, t.TempDir(), dbPath)
				if err == nil {
					closeDB()
				}
			default:
				t.Fatalf("store_upgrade_policy.yaml: case %q names entry %q; add it to the runner", c.Name, c.Entry)
			}
			var refusal *storeUpgradeRefusal
			refused := errors.As(err, &refusal) || store.IsSchemaBehind(err)
			if c.WantRefuse {
				if !refused {
					t.Fatalf("%s on the %s store: err = %v; want a refusal of the behind store", c.Entry, c.Store, err)
				}
				for _, fragment := range residuefixture.RefusalFragments {
					if !strings.Contains(err.Error(), fragment) {
						t.Errorf("refusal misses %q:\n%s", fragment, err)
					}
				}
				if got := policyFileHash(t, dbPath); got != before {
					t.Errorf("%s refused but modified the database file", c.Entry)
				}
				after := policySidecars(t, dbPath)
				if len(after) != len(sidecars) {
					t.Errorf("%s refused but changed which sidecar files exist: before %d, after %d", c.Entry, len(sidecars), len(after))
				}
				for suffix, data := range sidecars {
					if after[suffix] != data {
						t.Errorf("%s refused but modified the %s sidecar", c.Entry, suffix)
					}
				}
				return
			}
			if refused {
				t.Fatalf("%s on the %s store refused: %v", c.Entry, c.Store, err)
			}
			if c.WantOpened {
				if err != nil {
					t.Fatalf("%s on the %s store: err = %v; want a clean open", c.Entry, c.Store, err)
				}
				version, verr := store.SchemaVersionAt(dbPath)
				if verr != nil {
					t.Fatal(verr)
				}
				if version != store.CurrentSchemaVersion() {
					t.Errorf("%s on the %s store left schema %d, want the build schema %d", c.Entry, c.Store, version, store.CurrentSchemaVersion())
				}
			}
		})
	}
}

// TestKickstartStoreUpgrade runs the mounted kickstart command: the store is
// upgraded before discovery reads it, a declined upgrade ends the command
// with nothing changed, and the mount writes nothing to a current store.
func TestKickstartStoreUpgrade(t *testing.T) {
	drivers := map[string]func(*testing.T){
		"kickstart-upgrades-before-discovery":                   runKickstartUpgradesBeforeDiscovery,
		"kickstart-mount-opens-read-only":                       runKickstartMountOpensReadOnly,
		"kickstart-declined-upgrade-exits-with-nothing-changed": runKickstartDeclinedUpgrade,
	}
	for _, c := range loadStoreUpgradePolicyFixtures(t).KickstartCases {
		driver, ok := drivers[c.Name]
		if !ok {
			t.Fatalf("store_upgrade_policy.yaml: kickstart case %q has no driver", c.Name)
		}
		t.Run(c.Name, driver)
	}
}

// kickstartUpgradeProbe is a production kickstart command whose discovery
// and flow are recorders: the upgrade check in front of them is the real one.
type kickstartUpgradeProbe struct {
	discoveredAt []int
	flowRuns     int
}

func (p *kickstartUpgradeProbe) command(t *testing.T) *cobra.Command {
	t.Helper()
	deps := defaultKickstartCommandDeps()
	deps.existingUser = func(string) string { return "" }
	deps.discover = func(_ context.Context, _ string, dbPath string, _ *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing, kickstart.SubagentRelation) {
		version, err := store.SchemaVersionAt(dbPath)
		if err != nil {
			t.Errorf("read the schema version at discovery: %v", err)
		}
		p.discoveredAt = append(p.discoveredAt, version)
		return ftue.ProviderInventory{}, nil, nil
	}
	deps.runFlow = func(*cobra.Command, kickstartCommandDeps, string, ftue.ProviderInventory, []ftue.SessionListing, kickstart.SubagentRelation) error {
		p.flowRuns++
		return nil
	}
	return buildKickstartCommand(deps)
}

func runKickstartUpgradesBeforeDiscovery(t *testing.T) {
	stubTerminal(t, true)
	dir := t.TempDir()
	dbPath := seedPolicyStore(t, dir, "real-v75")
	probe := &kickstartUpgradeProbe{}
	out, err := executeWithDataDir(t, probe.command(t), dir, nil)
	if err != nil {
		t.Fatalf("kickstart on a behind store with a quick plan: %v\n%s", err, out)
	}
	if len(probe.discoveredAt) != 1 || probe.discoveredAt[0] != store.CurrentSchemaVersion() {
		t.Fatalf("discovery saw schema versions %v, want one read at the head schema %d: the upgrade runs first, so discovery can reuse the store", probe.discoveredAt, store.CurrentSchemaVersion())
	}
	if openReusableStoreForTest(t, dbPath) == false {
		t.Error("the store is not reusable by discovery after kickstart upgraded it")
	}
	if probe.flowRuns != 1 {
		t.Errorf("the flow ran %d times, want once after the upgrade", probe.flowRuns)
	}
	if !strings.Contains(out, "store upgrade finished") {
		t.Errorf("kickstart did not report the upgrade; output:\n%s", out)
	}
}

// openReusableStoreForTest reports whether discovery's reuse check accepts
// the store.
func openReusableStoreForTest(t *testing.T, dbPath string) bool {
	t.Helper()
	db := openReusableStore(dbPath)
	if db == nil {
		return false
	}
	_ = db.Close()
	return true
}

func runKickstartDeclinedUpgrade(t *testing.T) {
	stubTerminal(t, true)
	dir := t.TempDir()
	dbPath := seedPolicyStore(t, dir, "real-v75-long")
	before := policyFileHash(t, dbPath)
	probe := &kickstartUpgradeProbe{}
	root := newTestRoot()
	sub := probe.command(t)
	root.AddCommand(sub)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader("n\n"))
	root.SetArgs([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, sub.Name()})
	err := root.Execute()
	var refusal *storeUpgradeRefusal
	if !errors.As(err, &refusal) || !refusal.Declined {
		t.Fatalf("kickstart after a declined upgrade returned %v, want the declined refusal", err)
	}
	if !strings.Contains(err.Error(), "peasant migrate --schema-only") {
		t.Errorf("the refusal does not name the command to run: %v", err)
	}
	if !strings.Contains(out.String(), "upgrade the store now?") || !strings.Contains(out.String(), "long:") {
		t.Errorf("kickstart did not show the plan and the prompt; output:\n%s", out.String())
	}
	if len(probe.discoveredAt) != 0 || probe.flowRuns != 0 {
		t.Errorf("discovery ran %d times and the flow %d times after a declined upgrade, want neither", len(probe.discoveredAt), probe.flowRuns)
	}
	if got := policyFileHash(t, dbPath); got != before {
		t.Error("a declined upgrade changed the database file")
	}
	if version, verr := store.SchemaVersionAt(dbPath); verr != nil || version != 75 {
		t.Errorf("store version = %d, err = %v; want the untouched 75", version, verr)
	}
}

func runKickstartMountOpensReadOnly(t *testing.T) {
	stubTerminal(t, true)
	dir := t.TempDir()
	dbPath := seedPolicyStore(t, dir, "current")
	// An idle connection with checkpoints off keeps the write-ahead log, so
	// every transaction the mount commits would stay countable in it.
	holder, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	for _, pragma := range []string{"PRAGMA journal_mode = WAL", "PRAGMA wal_autocheckpoint = 0"} {
		if err := sqlitex.ExecuteTransient(holder, pragma, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := sqlitex.ExecuteTransient(holder, "SELECT count(*) FROM sqlite_master", nil); err != nil {
		t.Fatal(err)
	}
	before, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	hashBefore := policyFileHash(t, dbPath)

	cmd, _ := policyTestCmd(t, dir, false, "")
	if err := ensureStoreCurrent(cmd, dbPath, storeEntryModeFor(cmd)); err != nil {
		t.Fatalf("the upgrade check on a current store: %v", err)
	}
	db, closeStore, err := openKickstartStoreWithError(cmd, t.TempDir())
	if err != nil {
		t.Fatalf("mount a current store: %v", err)
	}
	if _, err := readKickstartStoredSessions(context.Background(), db); err != nil {
		t.Fatalf("read the stored sessions through the mount: %v", err)
	}
	closeStore()

	after, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("the mount committed %d write transaction(s) on a current store, want none", after-before)
	}
	if got := policyFileHash(t, dbPath); got != hashBefore {
		t.Error("the mount changed the database file of a current store")
	}
}

// TestStoreUpgradeSeams pins the clean-ups carried into the upgrade from the
// review of the 0.16.2 search-repair gate.
func TestStoreUpgradeSeams(t *testing.T) {
	drivers := map[string]func(*testing.T){
		"web-start-parent-and-child-share-open-options": runWebStartSharesOpenOptions,
		"terminal-check-is-one-stubbable-function":      runTerminalCheckIsOneFunction,
	}
	for _, c := range loadStoreUpgradePolicyFixtures(t).SeamCases {
		driver, ok := drivers[c.Name]
		if !ok {
			t.Fatalf("store_upgrade_policy.yaml: seam case %q has no driver", c.Name)
		}
		t.Run(c.Name, driver)
	}
}

func runWebStartSharesOpenOptions(t *testing.T) {
	stubTerminal(t, false)
	dir := t.TempDir()
	seedPolicyStore(t, dir, "current")
	cfgPath := writeTestConfigFile(t, dir)
	var targets []webStoreTarget
	previous := openWebStore
	openWebStore = func(target webStoreTarget) (*store.Store, error) {
		targets = append(targets, target)
		return nil, errors.New("recorded by the test; no store was opened")
	}
	t.Cleanup(func() { openWebStore = previous })

	// The parent: the same resolution `peasant web start` runs before it
	// forks.
	parentCmd, _ := policyTestCmd(t, dir, false, "")
	if err := prepareStoreForWebStart(parentCmd, webStartStoreTarget(parentCmd, cfgPath), parentCmd.ErrOrStderr()); err != nil {
		t.Fatalf("parent pre-open: %v", err)
	}
	// The child: the real foreground server, which stops on the short
	// context after it tried to open its store.
	childCmd, _ := policyTestCmd(t, dir, false, "")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	childCmd.SetContext(ctx)
	_ = runWebForeground(childCmd, cfgPath, 0, false, "", false)

	if len(targets) != 2 {
		t.Fatalf("the store was opened %d times, want once by the parent and once by the child", len(targets))
	}
	parent, child := targets[0], targets[1]
	if parent.dbPath != child.dbPath || parent.dataDir != child.dataDir {
		t.Errorf("parent opens %s in %s, child opens %s in %s; they must be the same store", parent.dbPath, parent.dataDir, child.dbPath, child.dataDir)
	}
	if len(parent.options) == 0 || len(parent.options) != len(child.options) {
		t.Errorf("parent passes %d open options and child %d; both must pass the same managed-generation options", len(parent.options), len(child.options))
	}
	if (parent.optionsErr == nil) != (child.optionsErr == nil) {
		t.Errorf("parent options error %v, child options error %v; they must agree", parent.optionsErr, child.optionsErr)
	}
}

func runTerminalCheckIsOneFunction(t *testing.T) {
	dir := t.TempDir()
	seedPolicyStore(t, dir, "current")
	cmd, _ := policyTestCmd(t, dir, false, "")

	stubTerminal(t, false)
	if got := storeEntryModeFor(cmd); got != storeEntryPiped {
		t.Errorf("upgrade policy mode with no terminal = %q, want piped", got)
	}
	_, err := executeWithDataDir(t, BuildTUICommand(), dir, nil)
	var noTTY noTerminalError
	if !errors.As(err, &noTTY) {
		t.Errorf("tui with no terminal returned %v, want the no-terminal error from the same check", err)
	}

	stubTerminal(t, true)
	if got := storeEntryModeFor(cmd); got != storeEntryTerminal {
		t.Errorf("upgrade policy mode with a terminal = %q, want terminal", got)
	}
	if err := requireTerminal("tui"); err != nil {
		t.Errorf("the TUI check with a terminal = %v, want nil from the same stub", err)
	}
}

// TestStoreEntryModeClosedSet pins the two closed sets of the policy.
func TestStoreEntryModeClosedSet(t *testing.T) {
	t.Parallel()
	for _, mode := range allStoreEntryModes {
		if got, err := newStoreEntryMode(mode.String()); err != nil || got != mode {
			t.Errorf("newStoreEntryMode(%q) = %q, %v", mode, got, err)
		}
	}
	if _, err := newStoreEntryMode("daemon"); err == nil {
		t.Error("newStoreEntryMode accepted a mode outside the set")
	}
	for _, action := range allStoreUpgradeActions {
		if got, err := newStoreUpgradeAction(string(action)); err != nil || got != action {
			t.Errorf("newStoreUpgradeAction(%q) = %q, %v", action, got, err)
		}
	}
	if _, err := newStoreUpgradeAction("deferred"); err == nil {
		t.Error("newStoreUpgradeAction accepted an action outside the set")
	}
}
