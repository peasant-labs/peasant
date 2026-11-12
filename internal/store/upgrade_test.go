package store

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/filelock"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/store_upgrade.yaml
var storeUpgradeYAML []byte

//go:embed testdata/store_upgrade.manifest.yaml
var storeUpgradeManifestYAML []byte

//go:embed testdata/upgrade_step_catalog.manifest.yaml
var upgradeStepCatalogManifestYAML []byte

type storeUpgradeCase struct {
	Name        string `yaml:"name"`
	Kind        string `yaml:"kind"`
	From        int    `yaml:"from,omitempty"`
	StopAt      int    `yaml:"stopAt,omitempty"`
	Fault       string `yaml:"fault,omitempty"`
	WantVersion int    `yaml:"wantVersion,omitempty"`
}

// LoadStoreUpgradeFixtures decodes the store upgrade cases and checks them
// against the required-name manifest.
func LoadStoreUpgradeFixtures() ([]storeUpgradeCase, error) {
	var decoded struct {
		Cases []storeUpgradeCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(storeUpgradeYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode store_upgrade.yaml: %w", err)
	}
	manifest, err := decodeRecoveryRequiredNames(storeUpgradeManifestYAML)
	if err != nil {
		return nil, fmt.Errorf("decode store_upgrade manifest: %w", err)
	}
	actual := make([]string, 0, len(decoded.Cases))
	for _, c := range decoded.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "store upgrade"); err != nil {
		return nil, err
	}
	return decoded.Cases, nil
}

// buildBehindStore builds a store on the frozen chain at schema version
// from, in WAL mode like every store peasant writes, and closes it so the
// file is checkpointed and has no sidecar.
func buildBehindStore(t *testing.T, path string, from int, seed func(*sqlite.Conn)) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA journal_mode = WAL", nil); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(from)); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if seed != nil {
		seed(conn)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustVersionAt(t *testing.T, path string) int {
	t.Helper()
	version, err := SchemaVersionAt(path)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func assertNoSidecars(t *testing.T, path, after string) {
	t.Helper()
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Errorf("%s left %s behind; a refusal or a read-only plan must create no sidecar", after, sidecar)
		}
	}
}

func assertHashUnchanged(t *testing.T, path string, before []byte, after string) {
	t.Helper()
	if got := fileHash(t, path); !bytes.Equal(got, before) {
		t.Errorf("%s changed the database file; it must change no byte", after)
	}
}

// TestStoreUpgrade drives every case of the store upgrade family. It does
// not call t.Parallel: the disk cases replace the package's free-disk probe,
// and a sequential top-level test never overlaps a parallel one.
func TestStoreUpgrade(t *testing.T) {
	cases, err := LoadStoreUpgradeFixtures()
	if err != nil {
		t.Fatal(err)
	}
	drivers := map[string]func(*testing.T, storeUpgradeCase){
		"fresh":                    runUpgradeFreshCase,
		"current":                  runUpgradeCurrentCase,
		"plan":                     runUpgradePlanCase,
		"open-refuses":             runUpgradeOpenRefusesCase,
		"newer":                    runUpgradeNewerCase,
		"interrupted":              runUpgradeInterruptedCase,
		"data-fill":                runUpgradeDataFillCase,
		"disk-insufficient":        runUpgradeDiskInsufficientCase,
		"disk-unknown":             runUpgradeDiskUnknownCase,
		"wal-truncated":            runUpgradeWALTruncatedCase,
		"run-lock":                 runUpgradeRunLockCase,
		"catalog":                  runUpgradeCatalogCase,
		"precheck-retired-missing": runUpgradePrecheckRetiredMissingCase,
		"precheck-live-missing":    runUpgradePrecheckLiveMissingCase,
		"notice-option":            runUpgradeNoticeOptionCase,
		"events":                   runUpgradeEventsCase,
	}
	for _, c := range cases {
		c := c
		driver, ok := drivers[c.Kind]
		if !ok {
			t.Fatalf("store_upgrade.yaml: case %q has kind %q with no driver", c.Name, c.Kind)
		}
		t.Run(c.Name, func(t *testing.T) { driver(t, c) })
	}
}

func runUpgradeFreshCase(t *testing.T, _ storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	plan, err := PlanUpgrade(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 0 || plan.From != plan.To {
		t.Fatalf("plan of a missing store = %+v, want no steps and From == To", plan)
	}
	report, err := Upgrade(t.Context(), path, UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Steps) != 0 {
		t.Fatalf("upgrade of a missing store ran %d steps, want none", len(report.Steps))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("planning or upgrading a missing store created the file (stat error %v); only Open creates a store", err)
	}
	before := baselineApplicationTotal()
	s, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if baselineApplicationTotal() != before+1 {
		t.Error("Open did not create the new store from the baseline")
	}
	if got := mustVersionAt(t, path); got != CurrentSchemaVersion() {
		t.Errorf("new store is at schema %d, want head %d", got, CurrentSchemaVersion())
	}
}

func runUpgradeCurrentCase(t *testing.T, _ storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "current.db")
	s, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before := fileHash(t, path)
	plan, err := PlanUpgrade(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 0 || plan.From != CurrentSchemaVersion() || plan.To != CurrentSchemaVersion() {
		t.Fatalf("plan of a current store = %+v, want no steps at head", plan)
	}
	report, err := Upgrade(t.Context(), path, UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Steps) != 0 {
		t.Fatalf("upgrade of a current store ran %d steps", len(report.Steps))
	}
	assertHashUnchanged(t, path, before, "planning and upgrading a current store")
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), ingest.RunLockFileName)); !os.IsNotExist(err) {
		t.Error("upgrading a current store took the data directory lock; a current store must return before it")
	}
}

func runUpgradePlanCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "behind.db")
	buildBehindStore(t, path, c.From, nil)
	before := fileHash(t, path)
	plan, err := PlanUpgrade(path)
	if err != nil {
		t.Fatal(err)
	}
	head := CurrentSchemaVersion()
	if plan.DBPath != path || plan.From != c.From || plan.To != head {
		t.Fatalf("plan header = %q %d -> %d, want %q %d -> %d", plan.DBPath, plan.From, plan.To, path, c.From, head)
	}
	if len(plan.Steps) != head-c.From {
		t.Fatalf("plan lists %d steps, want one per version between %d and %d", len(plan.Steps), c.From, head)
	}
	for i, step := range plan.Steps {
		version := c.From + 1 + i
		entry := upgradeStepCatalog[version]
		if step.Version != version || step.Kind != UpgradeStepSchema {
			t.Errorf("step %d = version %d kind %q, want version %d kind schema", i, step.Version, step.Kind, version)
		}
		if step.Title != entry.title || step.Cost != entry.cost || step.Counts != entry.counts {
			t.Errorf("step for version %d = %+v, want the catalog entry %+v", version, step, entry)
		}
	}
	if plan.StoreBytes <= 0 {
		t.Errorf("plan StoreBytes = %d, want the database file size", plan.StoreBytes)
	}
	if !plan.HasLongStep() || plan.RequiredFreeBytes <= 0 {
		t.Errorf("plan from %d has long steps and must ask for free disk; got long=%v required=%d", c.From, plan.HasLongStep(), plan.RequiredFreeBytes)
	}
	assertHashUnchanged(t, path, before, "PlanUpgrade")
	assertNoSidecars(t, path, "PlanUpgrade")
}

func runUpgradeOpenRefusesCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "behind.db")
	buildBehindStore(t, path, c.From, nil)
	before := fileHash(t, path)
	s, err := Open(path, WithPoolSize(1))
	if err == nil {
		_ = s.Close()
		t.Fatal("Open accepted a behind store; it must refuse and leave the upgrade to Upgrade")
	}
	var behind *SchemaBehindError
	if !errors.As(err, &behind) {
		t.Fatalf("Open error = %v, want a *SchemaBehindError", err)
	}
	if behind.DBPath != path || behind.From != c.From || behind.To != CurrentSchemaVersion() {
		t.Errorf("SchemaBehindError = %+v, want %q %d -> %d", behind, path, c.From, CurrentSchemaVersion())
	}
	if !IsSchemaBehind(fmt.Errorf("open analytics store: %w", err)) {
		t.Error("IsSchemaBehind misses a wrapped SchemaBehindError")
	}
	for _, part := range []string{"why:", "where:", "when:", "meaning:", "fix:", "peasant migrate --schema-only"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("SchemaBehindError message misses %q: %s", part, err)
		}
	}
	assertHashUnchanged(t, path, before, "the refused Open")
	assertNoSidecars(t, path, "the refused Open")
	if got := mustVersionAt(t, path); got != c.From {
		t.Errorf("store is at schema %d after the refused Open, want %d", got, c.From)
	}
}

func runUpgradeNewerCase(t *testing.T, _ storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "newer.db")
	s, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf("PRAGMA user_version = %d", CurrentSchemaVersion()+1), nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	before := fileHash(t, path)
	const want = "upgraded by a newer Peasant"
	if _, err := PlanUpgrade(path); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("PlanUpgrade on a newer store = %v, want the newer-schema refusal", err)
	}
	var events int
	report, err := Upgrade(t.Context(), path, UpgradeOptions{Progress: func(UpgradeEvent) { events++ }})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Upgrade on a newer store = %v, want the newer-schema refusal", err)
	}
	if events != 0 || len(report.Steps) != 0 {
		t.Errorf("a refused newer store reported %d events and %d steps, want none", events, len(report.Steps))
	}
	if opened, err := Open(path, WithPoolSize(1)); err == nil {
		_ = opened.Close()
		t.Error("Open accepted a newer store")
	} else if !strings.Contains(err.Error(), want) {
		t.Errorf("Open on a newer store = %v, want the newer-schema refusal", err)
	}
	assertHashUnchanged(t, path, before, "refusing a newer store")
	assertNoSidecars(t, path, "refusing a newer store")
}

func runUpgradeInterruptedCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "behind.db")
	buildBehindStore(t, path, c.From, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var finished []int
	report, err := Upgrade(ctx, path, UpgradeOptions{
		Progress: func(event UpgradeEvent) {
			if event.Kind == UpgradeEventStepFinished {
				finished = append(finished, event.Step.Version)
			}
		},
		afterDataFill: func(version int) error {
			if version == c.StopAt {
				cancel()
			}
			return nil
		},
	})
	var interrupted *UpgradeInterruptedError
	if !errors.As(err, &interrupted) {
		t.Fatalf("Upgrade error = %v, want an *UpgradeInterruptedError", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the interruption does not wrap context.Canceled: %v", err)
	}
	if interrupted.Step.Version != c.StopAt || interrupted.At != c.WantVersion || interrupted.Finished != c.WantVersion-c.From {
		t.Errorf("interruption = step %d at schema %d after %d finished, want step %d at %d after %d",
			interrupted.Step.Version, interrupted.At, interrupted.Finished, c.StopAt, c.WantVersion, c.WantVersion-c.From)
	}
	if !strings.Contains(err.Error(), "peasant migrate --schema-only") {
		t.Errorf("the interruption does not say how to resume: %v", err)
	}
	if got := mustVersionAt(t, path); got != c.WantVersion {
		t.Fatalf("store is at schema %d after the cancelled step, want %d: the cancelled step rolls back and finished steps stay", got, c.WantVersion)
	}
	if len(report.Steps) != c.WantVersion-c.From || len(finished) != len(report.Steps) {
		t.Errorf("report lists %d finished steps and %d finished events, want %d each", len(report.Steps), len(finished), c.WantVersion-c.From)
	}
	resumed, err := Upgrade(t.Context(), path, UpgradeOptions{})
	if err != nil {
		t.Fatalf("resume after the cancelled step: %v", err)
	}
	if resumed.From != c.WantVersion || len(resumed.Steps) != CurrentSchemaVersion()-c.WantVersion {
		t.Errorf("resume ran from %d with %d steps, want from %d with %d", resumed.From, len(resumed.Steps), c.WantVersion, CurrentSchemaVersion()-c.WantVersion)
	}
	s, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatalf("Open after the resumed upgrade: %v", err)
	}
	_ = s.Close()
}

func runUpgradeDataFillCase(t *testing.T, c storeUpgradeCase) {
	fixture := loadConversationTurnCountMigrationFixture(t)
	path := filepath.Join(t.TempDir(), "fold.db")
	buildBehindStore(t, path, c.From, func(conn *sqlite.Conn) {
		seedConversationTurnCountSessions(t, conn, fixture)
	})
	killed := errors.New("process killed inside the turn count fold")
	_, err := Upgrade(t.Context(), path, UpgradeOptions{afterDataFill: func(version int) error {
		if version == c.StopAt {
			return killed
		}
		return nil
	}})
	if !errors.Is(err, killed) {
		t.Fatalf("Upgrade error = %v, want the injected fault", err)
	}
	if got := mustVersionAt(t, path); got != c.WantVersion {
		t.Fatalf("store is at schema %d after dying inside the fold, want %d: the fold and its schema step are one transaction", got, c.WantVersion)
	}
	probe, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	columnPresent := false
	if err := sqlitex.ExecuteTransient(probe, `SELECT 1 FROM pragma_table_info('session_captured_stats') WHERE name = 'conversation_turn_count'`, &sqlitex.ExecOptions{
		ResultFunc: func(*sqlite.Stmt) error { columnPresent = true; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if columnPresent {
		t.Fatal("the count column survived the rolled-back step; the schema change and the fold did not share a transaction")
	}

	s, err := openUpgraded(path, WithPoolSize(1))
	if err != nil {
		t.Fatalf("second upgrade after the interrupted fold: %v", err)
	}
	defer s.Close()
	conn, err := s.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool().Put(conn)
	for _, session := range fixture.Sessions {
		got := -1
		if err := sqlitex.ExecuteTransient(conn, `SELECT conversation_turn_count FROM session_captured_stats WHERE session_id = ?`,
			&sqlitex.ExecOptions{Args: []any{session.SessionID}, ResultFunc: func(stmt *sqlite.Stmt) error {
				got = stmt.ColumnInt(0)
				return nil
			}}); err != nil {
			t.Fatal(err)
		}
		// The expected value is the one an uninterrupted upgrade writes
		// (TestMigrationV74BackfillsConversationTurnCounts pins it).
		if got != session.Expected {
			t.Errorf("%s: conversation_turn_count = %d after the second upgrade, want %d as an uninterrupted upgrade gives", session.Name, got, session.Expected)
		}
	}
}

// stubFreeBytes replaces the package's free-disk probe for one sequential
// case.
func stubFreeBytes(t *testing.T, free uint64, known bool) {
	t.Helper()
	previous := migrateFreeBytes
	migrateFreeBytes = func(string) (uint64, bool) { return free, known }
	t.Cleanup(func() { migrateFreeBytes = previous })
}

func runUpgradeDiskInsufficientCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "behind.db")
	buildBehindStore(t, path, c.From, nil)
	// One byte less than the plan asks for: the test pins the invariant
	// (refuse when free < required), never a factor.
	stubFreeBytes(t, 0, false)
	required := mustPlan(t, path).RequiredFreeBytes
	if required <= 0 {
		t.Fatalf("plan from %d asks for %d free bytes; the case needs a long step", c.From, required)
	}
	stubFreeBytes(t, uint64(required-1), true)
	before := fileHash(t, path)
	var events int
	_, err := Upgrade(t.Context(), path, UpgradeOptions{Progress: func(UpgradeEvent) { events++ }})
	var disk *UpgradeDiskError
	if !errors.As(err, &disk) {
		t.Fatalf("Upgrade error = %v, want an *UpgradeDiskError", err)
	}
	if disk.FreeBytes != required-1 || disk.RequiredBytes != required {
		t.Errorf("UpgradeDiskError = %+v, want free %d required %d", disk, required-1, required)
	}
	if events != 0 {
		t.Errorf("the refused upgrade reported %d events; it must refuse before the first step", events)
	}
	if got := mustVersionAt(t, path); got != c.From {
		t.Errorf("store is at schema %d after the disk refusal, want %d", got, c.From)
	}
	assertHashUnchanged(t, path, before, "the disk refusal")

	// Exactly the required bytes is enough.
	stubFreeBytes(t, uint64(required), true)
	if _, err := Upgrade(t.Context(), path, UpgradeOptions{}); err != nil {
		t.Fatalf("Upgrade with exactly the required free bytes: %v", err)
	}
}

func mustPlan(t *testing.T, path string) UpgradePlan {
	t.Helper()
	plan, err := PlanUpgrade(path)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func runUpgradeDiskUnknownCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "behind.db")
	buildBehindStore(t, path, c.From, nil)
	stubFreeBytes(t, 0, false)
	if plan := mustPlan(t, path); plan.FreeBytesKnown {
		t.Fatal("plan reports known free disk under the unknown probe")
	}
	var notices bytes.Buffer
	report, err := Upgrade(t.Context(), path, UpgradeOptions{Notices: &notices})
	if err != nil {
		t.Fatalf("Upgrade with unknown free disk: %v", err)
	}
	if !strings.Contains(notices.String(), "warning: could not read the free disk space") {
		t.Errorf("notices = %q, want the unknown-free-disk warning", notices.String())
	}
	if len(report.Steps) != CurrentSchemaVersion()-c.From || mustVersionAt(t, path) != CurrentSchemaVersion() {
		t.Errorf("upgrade with unknown free disk ran %d steps to schema %d, want the whole plan", len(report.Steps), mustVersionAt(t, path))
	}
}

func runUpgradeWALTruncatedCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "behind.db")
	buildBehindStore(t, path, c.From, nil)
	// An idle second connection keeps the log file alive after the upgrade
	// closes its own, so the file's size after the upgrade is evidence: a
	// log that was never truncated would still hold every step's frames.
	holder, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := sqlitex.ExecuteTransient(holder, "PRAGMA wal_autocheckpoint = 0", nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(holder, "SELECT count(*) FROM sqlite_master", nil); err != nil {
		t.Fatal(err)
	}
	report, err := Upgrade(t.Context(), path, UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var written int64
	for _, step := range report.Steps {
		written += step.WALBytes
	}
	if written == 0 {
		t.Fatal("the report records no write-ahead-log growth; the case would prove nothing")
	}
	info, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("stat the write-ahead log after the upgrade: %v; the holder connection should have kept it", err)
	}
	if info.Size() != 0 {
		t.Errorf("write-ahead log is %d bytes after the upgrade, want 0: the last step is followed by a truncating checkpoint", info.Size())
	}
}

func runUpgradeRunLockCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "behind.db")
	conn := buildV75Store(t, path, false)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(filepath.Dir(path), ingest.RunLockFileName)
	release, err := filelock.Acquire(lockPath, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_ = release()
		}
	}()
	before := fileHash(t, path)
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	var notices bytes.Buffer
	_, err = Upgrade(ctx, path, UpgradeOptions{Notices: &notices})
	if err == nil {
		t.Fatal("Upgrade ran while another run held the data directory lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("Upgrade error = %v, want the cancelled wait that changed nothing", err)
	}
	if got := strings.Count(notices.String(), upgradeLockWaitNotice); got != 1 {
		t.Errorf("the waiting notice was printed %d times, want once: %q", got, notices.String())
	}
	if got := mustVersionAt(t, path); got != c.From {
		t.Errorf("store is at schema %d after waiting on the lock, want %d", got, c.From)
	}
	assertHashUnchanged(t, path, before, "waiting on the run lock")

	// The waiter proceeds as soon as the holder releases.
	done := make(chan error, 1)
	go func() {
		_, err := Upgrade(t.Context(), path, UpgradeOptions{})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Upgrade returned (%v) while the lock was still held", err)
	default:
	}
	released = true
	if err := release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Upgrade after the lock was released: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Upgrade did not proceed after the lock was released")
	}
	if got := mustVersionAt(t, path); got != CurrentSchemaVersion() {
		t.Errorf("store is at schema %d after the waiter ran, want head", got)
	}
}

func runUpgradeCatalogCase(t *testing.T, _ storeUpgradeCase) {
	required, err := decodeRecoveryRequiredNames(upgradeStepCatalogManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	chain := make([]string, 0, len(dbSchema.Migrations))
	for version := 1; version <= len(dbSchema.Migrations); version++ {
		chain = append(chain, strconv.Itoa(version))
		entry, ok := upgradeStepCatalog[version]
		if !ok {
			t.Errorf("migration %d has no entry in upgradeStepCatalog; give it a title and a cost class", version)
			continue
		}
		if entry.title == "" || entry.title != strings.ToLower(entry.title) || strings.HasSuffix(entry.title, ".") {
			t.Errorf("catalog title for migration %d = %q, want a lowercase reader-facing phrase with no final period", version, entry.title)
		}
		if _, err := NewUpgradeCost(string(entry.cost)); err != nil {
			t.Errorf("catalog cost for migration %d: %v", version, err)
		}
		if _, err := NewUpgradeUnit(string(entry.counts)); err != nil {
			t.Errorf("catalog unit for migration %d: %v", version, err)
		}
	}
	if err := validateRecoveryRequiredNames(required, chain, "upgrade step catalog"); err != nil {
		t.Errorf("the catalog manifest and the migration chain disagree: %v", err)
	}
	for version := range upgradeStepCatalog {
		if version < 1 || version > len(dbSchema.Migrations) {
			t.Errorf("upgradeStepCatalog has an entry for version %d, which is not a migration in the chain", version)
		}
	}
	for version := range upgradeDataFills {
		if version < 1 || version > len(dbSchema.Migrations) {
			t.Errorf("upgradeDataFills registers a fill for version %d, which is not a migration in the chain", version)
		}
	}
}

func runUpgradePrecheckRetiredMissingCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "tampered.db")
	conn := buildV75Store(t, path, false)
	applyMigrationV76Window(t, conn, "tampered")
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	plan := mustPlan(t, path)
	if len(plan.Steps) != 1 || !plan.Steps[0].Cost.Long() {
		t.Fatalf("plan = %+v, want the one repair step classified long: the repair will rebuild", plan.Steps)
	}
	var notices bytes.Buffer
	if _, err := Upgrade(t.Context(), path, UpgradeOptions{Notices: &notices}); err != nil {
		t.Fatalf("Upgrade of a store missing the retired index: %v", err)
	}
	got := notices.String()
	if !strings.HasPrefix(got, searchIndexPrecheckWarning) || strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Errorf("notices = %q, want exactly one warning line starting %q", got, searchIndexPrecheckWarning)
	}
	if v := mustVersionAt(t, path); v != CurrentSchemaVersion() {
		t.Errorf("store is at schema %d, want head: the upgrade runs after the warning", v)
	}
	_ = c
}

func runUpgradePrecheckLiveMissingCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "no-live-index.db")
	conn := buildV75Store(t, path, false)
	if err := sqlitex.ExecuteTransient(conn, `DROP TABLE session_search_fts`, nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	before := fileHash(t, path)
	check := func(label string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s accepted a store with no live search index", label)
		}
		message := err.Error()
		for _, part := range []string{"session_search_fts is missing", "why:", "where:", "when:", "meaning:", "fix:"} {
			if !strings.Contains(message, part) {
				t.Errorf("%s error misses %q: %s", label, part, message)
			}
		}
		if strings.Contains(message, "no such table") {
			t.Errorf("%s error leaks SQLite's raw message: %s", label, message)
		}
	}
	_, err := PlanUpgrade(path)
	check("PlanUpgrade", err)
	var notices bytes.Buffer
	_, err = Upgrade(t.Context(), path, UpgradeOptions{Notices: &notices})
	check("Upgrade", err)
	if notices.Len() != 0 {
		t.Errorf("notices = %q, want none: the error is the report", notices.String())
	}
	if got := mustVersionAt(t, path); got != c.From {
		t.Errorf("store is at schema %d after the refusal, want %d", got, c.From)
	}
	assertHashUnchanged(t, path, before, "the refusal")
}

func runUpgradeNoticeOptionCase(t *testing.T, _ storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "affected.db")
	conn := buildV75Store(t, path, false)
	applyMigrationV76Window(t, conn, "affected")
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	plan := mustPlan(t, path)
	if len(plan.Steps) != 1 || plan.Steps[0].Cost != UpgradeCostScalesWithText || plan.Steps[0].Title != searchRepairLongTitle {
		t.Fatalf("plan = %+v, want the repair as one long step over the stored text", plan.Steps)
	}
	var notices bytes.Buffer
	if _, err := Upgrade(t.Context(), path, UpgradeOptions{Notices: &notices}); err != nil {
		t.Fatal(err)
	}
	if notices.String() != searchIndexRebuildNotice+"\n" {
		t.Errorf("notices = %q, want the one-line rebuild notice through the option", notices.String())
	}
	// A nil writer is allowed and discards the notice.
	silent := filepath.Join(t.TempDir(), "affected-silent.db")
	conn = buildV75Store(t, silent, false)
	applyMigrationV76Window(t, conn, "affected")
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Upgrade(t.Context(), silent, UpgradeOptions{}); err != nil {
		t.Fatalf("Upgrade with no notice writer: %v", err)
	}
}

func runUpgradeEventsCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "behind.db")
	buildBehindStore(t, path, c.From, nil)
	var events []UpgradeEvent
	report, err := Upgrade(t.Context(), path, UpgradeOptions{Progress: func(event UpgradeEvent) { events = append(events, event) }})
	if err != nil {
		t.Fatal(err)
	}
	total := CurrentSchemaVersion() - c.From
	if len(report.Steps) != total || report.From != c.From || report.To != CurrentSchemaVersion() {
		t.Fatalf("report = %d steps %d -> %d, want %d steps %d -> %d", len(report.Steps), report.From, report.To, total, c.From, CurrentSchemaVersion())
	}
	index, open := 0, false
	for _, event := range events {
		if _, err := NewUpgradeEventKind(string(event.Kind)); err != nil {
			t.Fatal(err)
		}
		if event.Total != total {
			t.Errorf("event %s for step %d carries Total %d, want %d", event.Kind, event.Index, event.Total, total)
		}
		switch event.Kind {
		case UpgradeEventStepStarted:
			if open {
				t.Fatalf("step %d started before step %d finished", event.Index, index)
			}
			index++
			open = true
		case UpgradeEventStepProgress:
			if !open {
				t.Fatalf("progress for step %d arrived outside a running step", event.Index)
			}
		case UpgradeEventStepFinished:
			if !open {
				t.Fatalf("step %d finished without starting", event.Index)
			}
			open = false
		}
		if event.Index != index || event.Step.Version != c.From+index {
			t.Errorf("event %s carries index %d version %d, want index %d version %d", event.Kind, event.Index, event.Step.Version, index, c.From+index)
		}
		if event.Unit != event.Step.Counts {
			t.Errorf("event for version %d carries unit %q, want the step's %q", event.Step.Version, event.Unit, event.Step.Counts)
		}
	}
	if open || index != total {
		t.Errorf("events covered %d steps (last still open: %v), want %d finished steps", index, open, total)
	}
	for i, result := range report.Steps {
		if result.Step.Version != c.From+1+i {
			t.Errorf("report step %d is version %d, want %d", i, result.Step.Version, c.From+1+i)
		}
	}
}

// TestUpgradeClosedSets pins each closed string type: every listed value is
// accepted and an unknown one is refused.
func TestUpgradeClosedSets(t *testing.T) {
	t.Parallel()
	for _, kind := range AllUpgradeStepKinds {
		if got, err := NewUpgradeStepKind(kind.String()); err != nil || got != kind {
			t.Errorf("NewUpgradeStepKind(%q) = %q, %v", kind, got, err)
		}
	}
	for _, cost := range AllUpgradeCosts {
		if got, err := NewUpgradeCost(cost.String()); err != nil || got != cost {
			t.Errorf("NewUpgradeCost(%q) = %q, %v", cost, got, err)
		}
		if cost.Long() != (cost != UpgradeCostQuick) {
			t.Errorf("%q.Long() = %v; every cost except quick is long", cost, cost.Long())
		}
	}
	for _, unit := range AllUpgradeUnits {
		if got, err := NewUpgradeUnit(unit.String()); err != nil || got != unit {
			t.Errorf("NewUpgradeUnit(%q) = %q, %v", unit, got, err)
		}
	}
	for _, kind := range AllUpgradeEventKinds {
		if got, err := NewUpgradeEventKind(kind.String()); err != nil || got != kind {
			t.Errorf("NewUpgradeEventKind(%q) = %q, %v", kind, got, err)
		}
	}
	if _, err := NewUpgradeStepKind("files"); err == nil {
		t.Error("NewUpgradeStepKind accepted a kind outside the set")
	}
	if _, err := NewUpgradeCost("slow"); err == nil {
		t.Error("NewUpgradeCost accepted a cost outside the set")
	}
	if _, err := NewUpgradeUnit("rows"); err == nil {
		t.Error("NewUpgradeUnit accepted a unit outside the set")
	}
	if _, err := NewUpgradeEventKind("step_failed"); err == nil {
		t.Error("NewUpgradeEventKind accepted a kind outside the set")
	}
}

// TestOnlyUpgradeCallsTheMigrationEngine is the source guard for the rule
// that Upgrade is the only path that migrates: outside tests and the
// vendored driver, the migration engine is called from exactly one place,
// applyUpgradeStep. A second call site would be a second way to change a
// store's schema.
func TestOnlyUpgradeCallsTheMigrationEngine(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	var callers []string
	for _, tree := range []string{"cmd", "internal", "scripts", "web/scripts"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "node_modules" || entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(data), "\n") {
				code, _, _ := strings.Cut(line, "//")
				if strings.Contains(code, "sqlitemigration.Migrate") || strings.Contains(code, "sqlitemigration.NewPool") {
					rel, relErr := filepath.Rel(root, path)
					if relErr != nil {
						return relErr
					}
					callers = append(callers, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if len(callers) != 1 || callers[0] != "internal/store/upgrade.go" {
		t.Fatalf("the migration engine is called from %v; want exactly one call, in internal/store/upgrade.go (applyUpgradeStep)", callers)
	}
}
