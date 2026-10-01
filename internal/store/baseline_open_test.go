package store

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/salt"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/baseline_open_cases.yaml
var baselineOpenCasesYAML []byte

// baselineOpenCase kinds form a closed set. Each kind names one observable the
// fresh path must satisfy.
var baselineOpenCaseKinds = map[string]struct{}{
	"fresh-open":      {},
	"clock-interval":  {},
	"foreign-app-id":  {},
	"v0-objects":      {},
	"predecessor":     {},
	"stale-probe":     {},
	"concurrent-open": {},
}

type baselineOpenCaseFixture struct {
	Name            string `yaml:"name"`
	Kind            string `yaml:"kind"`
	ApplicationID   int64  `yaml:"applicationID"`
	PredecessorSlot int    `yaml:"predecessorSlot"`
}

type baselineClockCellFixture struct {
	Name        string `yaml:"name"`
	Table       string `yaml:"table"`
	KeyColumn   string `yaml:"keyColumn"`
	Key         string `yaml:"key"`
	ClockColumn string `yaml:"clockColumn"`
}

type baselineClockCellsFixture struct {
	RequiredNames []string                   `yaml:"requiredNames"`
	Cells         []baselineClockCellFixture `yaml:"cells"`
}

type baselineOpenCaseFixtures struct {
	RequiredNames []string                  `yaml:"requiredNames"`
	Cases         []baselineOpenCaseFixture `yaml:"cases"`
	ClockCells    baselineClockCellsFixture `yaml:"clockCells"`
}

// LoadBaselineOpenCaseFixtures decodes the committed case family.
func LoadBaselineOpenCaseFixtures() (baselineOpenCaseFixtures, error) {
	return loadBaselineOpenCaseFixtures(baselineOpenCasesYAML)
}

// loadBaselineOpenCaseFixtures decodes the fixture with strict, single-document
// semantics. It mirrors the migration-slot loader; testutil.DecodeNamedFixtureYAML
// cannot be used because internal/testutil imports internal/store.
func loadBaselineOpenCaseFixtures(data []byte) (baselineOpenCaseFixtures, error) {
	var fixtures baselineOpenCaseFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		return fixtures, fmt.Errorf("decode internal/store/testdata/baseline_open_cases.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fixtures, fmt.Errorf("decode internal/store/testdata/baseline_open_cases.yaml: fixture must contain exactly one YAML document")
		}
		return fixtures, fmt.Errorf("decode internal/store/testdata/baseline_open_cases.yaml: %w", err)
	}
	return fixtures, nil
}

// validateOpenCaseNames enforces required-name coverage in both directions.
func validateOpenCaseNames(required, actual []string, label string) error {
	if len(required) == 0 {
		return fmt.Errorf("%s manifest is empty; add every named fixture to its required-name list", label)
	}
	seen := make(map[string]struct{}, len(required))
	for _, name := range required {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s manifest contains a blank required name; name every fixture identity", label)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("%s manifest repeats required name %q; keep one declaration", label, name)
		}
		seen[name] = struct{}{}
	}
	actualSet := make(map[string]struct{}, len(actual))
	for _, name := range actual {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s fixture contains a blank name; give every case a stable identity", label)
		}
		if _, duplicate := actualSet[name]; duplicate {
			return fmt.Errorf("%s fixture repeats name %q; keep one row per observable", label, name)
		}
		actualSet[name] = struct{}{}
	}
	for _, name := range required {
		if _, ok := actualSet[name]; !ok {
			return fmt.Errorf("%s manifest requires %q but the fixture has no such row; add the row or drop the required name", label, name)
		}
	}
	for name := range actualSet {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("%s fixture carries undeclared name %q; add it to requiredNames", label, name)
		}
	}
	return nil
}

// validateBaselineOpenCaseFixtures validates the closed kinds, the case
// parameters, and the clock-cell set.
func validateBaselineOpenCaseFixtures(fixtures baselineOpenCaseFixtures) error {
	names := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		names = append(names, c.Name)
		if _, ok := baselineOpenCaseKinds[c.Kind]; !ok {
			return fmt.Errorf("internal/store/testdata/baseline_open_cases.yaml: case %q names unknown kind %q; use one of the closed case kinds", c.Name, c.Kind)
		}
		switch c.Kind {
		case "foreign-app-id":
			if c.ApplicationID == 0 {
				return fmt.Errorf("internal/store/testdata/baseline_open_cases.yaml: case %q must carry a nonzero applicationID; a zero id is not foreign", c.Name)
			}
		case "predecessor":
			if want := CurrentSchemaVersion() - 1; c.PredecessorSlot != want {
				return fmt.Errorf("internal/store/testdata/baseline_open_cases.yaml: case %q pins predecessorSlot %d but the immediate predecessor of schema version %d is %d; update the pin when a migration is added", c.Name, c.PredecessorSlot, CurrentSchemaVersion(), want)
			}
		}
	}
	if err := validateOpenCaseNames(fixtures.RequiredNames, names, "baseline open case"); err != nil {
		return err
	}

	cellNames := make([]string, 0, len(fixtures.ClockCells.Cells))
	cellIndex := make(map[string]baselineClockCellFixture, len(fixtures.ClockCells.Cells))
	for _, cell := range fixtures.ClockCells.Cells {
		cellNames = append(cellNames, cell.Name)
		if strings.TrimSpace(cell.Table) == "" || strings.TrimSpace(cell.KeyColumn) == "" || strings.TrimSpace(cell.Key) == "" || strings.TrimSpace(cell.ClockColumn) == "" {
			return fmt.Errorf("internal/store/testdata/baseline_open_cases.yaml: clock cell %q has a blank field; name the table, key column, key, and clock column", cell.Name)
		}
		cellIndex[cell.Name] = cell
	}
	if err := validateOpenCaseNames(fixtures.ClockCells.RequiredNames, cellNames, "baseline clock cell"); err != nil {
		return err
	}
	return nil
}

// baselineCaseIsObservation reports whether a case observes the process-level
// application counter and therefore must run nonparallel.
func baselineCaseIsObservation(kind string) bool {
	switch kind {
	case "fresh-open", "v0-objects", "predecessor", "stale-probe":
		return true
	}
	return false
}

// openFreshStore opens the production fresh path on path and registers cleanup.
func openFreshStore(t *testing.T, path string) *Store {
	t.Helper()
	// ast-grep-ignore: no-migrating-store-open-in-tests -- the case's subject is the production fresh-open path.
	s, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatalf("store.Open(%s): %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close(%s): %v", path, err)
		}
	})
	return s
}

// mustSchemaVersion reads the database's user_version without opening it for
// write.
func mustSchemaVersion(t *testing.T, path string) int {
	t.Helper()
	version, err := SchemaVersionAt(path)
	if err != nil {
		t.Fatalf("SchemaVersionAt(%s): %v", path, err)
	}
	return version
}

// dumpDatabase returns the canonical comparison dump of the database at path.
func dumpDatabase(t *testing.T, path string) []byte {
	t.Helper()
	conn, err := sqlite.OpenConn(path, 0)
	if err != nil {
		t.Fatalf("open %s for canonical dump: %v", path, err)
	}
	defer conn.Close()
	dump, err := canonicalSchemaDump(conn)
	if err != nil {
		t.Fatalf("canonical dump of %s: %v", path, err)
	}
	return dump
}

// buildChainReference materializes the shipped chain plus the runtime salt
// initialization the production Open performs, so the reference lifecycle is
// post-Open exactly like the baseline side.
func buildChainReference(t *testing.T, path string) {
	t.Helper()
	if err := buildChainDatabase(path); err != nil {
		t.Fatalf("build chain reference %s: %v", path, err)
	}
	pool, err := sqlitex.NewPool(path, sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
	if err != nil {
		t.Fatalf("open chain reference pool %s: %v", path, err)
	}
	if _, _, err := salt.Load(pool); err != nil {
		_ = pool.Close()
		t.Fatalf("load salt into chain reference %s: %v", path, err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("close chain reference %s: %v", path, err)
	}
}

// TestBaselineOpenCases drives every case row in the committed family. The test
// does not call t.Parallel: the observation cases read a process-level counter
// and must not race unrelated opens.
func TestBaselineOpenCases(t *testing.T) {
	fixtures, err := LoadBaselineOpenCaseFixtures()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBaselineOpenCaseFixtures(fixtures); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			if baselineCaseIsObservation(c.Kind) {
				// Observation cases run nonparallel and assert a global delta.
				t.Log("observation case: nonparallel")
			}
			switch c.Kind {
			case "fresh-open":
				runFreshOpenCase(t)
			case "clock-interval":
				assertFreshClockCellsWithinOpenInterval(t, fixtures.ClockCells.Cells)
			case "foreign-app-id":
				runForeignApplicationIDCase(t, c)
			case "v0-objects":
				runVersionZeroWithObjectsCase(t)
			case "predecessor":
				runPredecessorUpgradeCase(t, c)
			case "stale-probe":
				runStaleProbeRecheckCase(t)
			case "concurrent-open":
				runConcurrentFreshOpenCase(t)
			default:
				t.Fatalf("case %q has kind %q with no driver", c.Name, c.Kind)
			}
		})
	}
}

func runFreshOpenCase(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fresh.db")
	before := baselineApplicationTotal()
	s := openFreshStore(t, path)
	after := baselineApplicationTotal()
	if after != before+1 {
		t.Fatalf("fresh open did not apply the baseline snapshot: application counter moved %d -> %d; removing the fresh-path dispatch must fail this case", before, after)
	}
	if got := mustSchemaVersion(t, path); got != CurrentSchemaVersion() {
		t.Fatalf("fresh open left user_version %d, want %d", got, CurrentSchemaVersion())
	}
	conn, err := s.Pool().Take(t.Context())
	if err != nil {
		t.Fatalf("take connection from fresh store: %v", err)
	}
	defer s.Pool().Put(conn)
	if got := scalarText(t, conn, "SELECT COUNT(*) FROM annotation_types"); got != "11" {
		t.Fatalf("fresh store smoke query returned %s seeded annotation types, want 11", got)
	}
}

func runForeignApplicationIDCase(t *testing.T, c baselineOpenCaseFixture) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foreign-appid.db")
	conn, err := sqlite.OpenConn(path, 0)
	if err != nil {
		t.Fatalf("create empty database %s: %v", path, err)
	}
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf("PRAGMA application_id = %d", c.ApplicationID), nil); err != nil {
		_ = conn.Close()
		t.Fatalf("stamp foreign application_id: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close foreign-application-id database: %v", err)
	}

	before := baselineApplicationTotal()
	// ast-grep-ignore: no-migrating-store-open-in-tests -- the case's subject is the open-time refusal.
	opened, openErr := Open(path, WithPoolSize(1))
	if openErr == nil {
		_ = opened.Close()
		t.Fatal("store opened a database carrying a foreign application_id")
	}
	want := fmt.Sprintf("database application_id = %#x (expected %#x)", int32(c.ApplicationID), int32(0))
	if !strings.Contains(openErr.Error(), want) {
		t.Fatalf("foreign application_id refusal %q does not carry the library message %q", openErr, want)
	}
	if after := baselineApplicationTotal(); after != before {
		t.Fatalf("a refused foreign-application_id open applied the baseline snapshot: counter %d -> %d", before, after)
	}

	check, err := sqlite.OpenConn(path, 0)
	if err != nil {
		t.Fatalf("reopen refused database: %v", err)
	}
	defer check.Close()
	if got := scalarText(t, check, "PRAGMA application_id"); got != fmt.Sprintf("%d", c.ApplicationID) {
		t.Fatalf("refused open changed application_id to %s, want %d", got, c.ApplicationID)
	}
	if got := scalarText(t, check, "PRAGMA user_version"); got != "0" {
		t.Fatalf("refused open changed user_version to %s, want 0", got)
	}
	if got := scalarText(t, check, "SELECT COUNT(*) FROM sqlite_master"); got != "0" {
		t.Fatalf("refused open wrote %s schema objects, want 0", got)
	}
	if got := scalarText(t, check, "SELECT COUNT(*) FROM sqlite_master WHERE name='_install_salt'"); got != "0" {
		t.Fatalf("refused open created the runtime salt table")
	}
}

func runVersionZeroWithObjectsCase(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v0-objects.db")
	conn, err := sqlite.OpenConn(path, 0)
	if err != nil {
		t.Fatalf("create v0 database %s: %v", path, err)
	}
	if err := sqlitex.ExecuteTransient(conn, "CREATE TABLE stray_probe (id INTEGER PRIMARY KEY, note TEXT)", nil); err != nil {
		_ = conn.Close()
		t.Fatalf("create stray table: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, "INSERT INTO stray_probe (id, note) VALUES (1, 'kept')", nil); err != nil {
		_ = conn.Close()
		t.Fatalf("seed stray table: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close v0 database: %v", err)
	}

	before := baselineApplicationTotal()
	s := openFreshStore(t, path)
	after := baselineApplicationTotal()
	if after != before {
		t.Fatalf("a version-zero database carrying user objects applied the baseline snapshot: counter %d -> %d; it must fall through to the chain", before, after)
	}
	if got := mustSchemaVersion(t, path); got != CurrentSchemaVersion() {
		t.Fatalf("chain upgrade left user_version %d, want %d", got, CurrentSchemaVersion())
	}
	conn2, err := s.Pool().Take(t.Context())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.Pool().Put(conn2)
	if got := scalarText(t, conn2, "SELECT note FROM stray_probe WHERE id = 1"); got != "kept" {
		t.Fatalf("chain upgrade lost the pre-existing user row: note=%q", got)
	}
}

func runPredecessorUpgradeCase(t *testing.T, c baselineOpenCaseFixture) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "predecessor.db")
	pool, err := sqlitex.NewPool(path, sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
	if err != nil {
		t.Fatalf("open predecessor pool: %v", err)
	}
	conn, err := pool.Take(t.Context())
	if err != nil {
		_ = pool.Close()
		t.Fatalf("take predecessor connection: %v", err)
	}
	if err := migrateFrozenSchema(t, conn, c.PredecessorSlot); err != nil {
		pool.Put(conn)
		_ = pool.Close()
		t.Fatalf("freeze predecessor schema %d: %v", c.PredecessorSlot, err)
	}
	if err := sqlitex.ExecuteTransient(conn, "INSERT INTO projects (project_hash, canonical_cwd, canonical_remote) VALUES ('predecessor-project-hash', '/tmp/predecessor', NULL)", nil); err != nil {
		pool.Put(conn)
		_ = pool.Close()
		t.Fatalf("seed predecessor projects row: %v", err)
	}
	pool.Put(conn)
	if err := pool.Close(); err != nil {
		t.Fatalf("close predecessor pool: %v", err)
	}

	before := baselineApplicationTotal()
	s := openFreshStore(t, path)
	after := baselineApplicationTotal()
	if after != before {
		t.Fatalf("a predecessor database applied the baseline snapshot: counter %d -> %d; only the chain may upgrade it", before, after)
	}
	if got := mustSchemaVersion(t, path); got != CurrentSchemaVersion() {
		t.Fatalf("predecessor upgrade left user_version %d, want %d", got, CurrentSchemaVersion())
	}
	conn2, err := s.Pool().Take(t.Context())
	if err != nil {
		t.Fatalf("take upgraded connection: %v", err)
	}
	defer s.Pool().Put(conn2)
	if got := scalarText(t, conn2, "SELECT canonical_cwd FROM projects WHERE project_hash = 'predecessor-project-hash'"); got != "/tmp/predecessor" {
		t.Fatalf("predecessor upgrade lost the seeded project row: canonical_cwd=%q", got)
	}
}

func runStaleProbeRecheckCase(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stale-probe.db")
	conn, err := sqlite.OpenConn(path, 0)
	if err != nil {
		t.Fatalf("open stale-probe database: %v", err)
	}
	defer conn.Close()

	fresh, err := databaseIsFresh(conn)
	if err != nil {
		t.Fatalf("probe fresh file: %v", err)
	}
	if !fresh {
		t.Fatal("the read-only probe reported a brand-new file as not fresh")
	}
	// A second connection applies the chain while the first stays open and
	// fresh-probed, making the transactional re-check deterministic.
	if err := buildChainDatabase(path); err != nil {
		t.Fatalf("apply chain on a second connection: %v", err)
	}
	before := baselineApplicationTotal()
	applied, err := applyBaselineIfStillFresh(conn)
	if err != nil {
		t.Fatalf("re-check after the chain landed: %v", err)
	}
	if applied {
		t.Fatal("the transactional re-check applied the baseline to a database that stopped being fresh")
	}
	if after := baselineApplicationTotal(); after != before {
		t.Fatalf("a rejected re-check moved the application counter: %d -> %d", before, after)
	}
	if got := mustSchemaVersion(t, path); got != CurrentSchemaVersion() {
		t.Fatalf("the stale-probe case left user_version %d, want %d", got, CurrentSchemaVersion())
	}
}

func runConcurrentFreshOpenCase(t *testing.T) {
	t.Helper()
	const rounds = 3
	for round := 0; round < rounds; round++ {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("concurrent-%d.db", round))
		const openers = 2
		stores := make([]*Store, openers)
		errs := make([]error, openers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < openers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				// ast-grep-ignore: no-migrating-store-open-in-tests -- the case races the production fresh-open path.
				stores[i], errs[i] = Open(path, WithPoolSize(1))
			}(i)
		}
		close(start)
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Fatal("concurrent fresh opens did not finish within the bounded wait")
		}
		for i := 0; i < openers; i++ {
			if errs[i] != nil {
				t.Fatalf("concurrent opener %d failed: %v", i, errs[i])
			}
			store := stores[i]
			if got := mustSchemaVersion(t, path); got != CurrentSchemaVersion() {
				_ = store.Close()
				t.Fatalf("concurrent open round %d left user_version %d, want %d", round, got, CurrentSchemaVersion())
			}
			conn, err := store.Pool().Take(t.Context())
			if err != nil {
				_ = store.Close()
				t.Fatalf("concurrent opener %d could not take a connection: %v", i, err)
			}
			if got := scalarText(t, conn, "SELECT COUNT(*) FROM annotation_types"); got != "11" {
				store.Pool().Put(conn)
				_ = store.Close()
				t.Fatalf("concurrent opener %d saw %s seeded annotation types, want 11", i, got)
			}
			store.Pool().Put(conn)
			if err := store.Close(); err != nil {
				t.Fatalf("close concurrent opener %d: %v", i, err)
			}
		}
	}
}

// migrateFrozenSchema applies the first n shipped migrations to conn.
func migrateFrozenSchema(t *testing.T, conn *sqlite.Conn, n int) error {
	t.Helper()
	return sqlitemigration.Migrate(t.Context(), conn, frozenSchema(n))
}

// TestBaselineSchemaMatchesMigratedChain proves a baseline-created database is
// canonically identical to a chain-created one under the generated-value policy,
// with only the random _install_salt row excluded.
func TestBaselineSchemaMatchesMigratedChain(t *testing.T) {
	dir := t.TempDir()
	baselinePath := filepath.Join(dir, "baseline.db")
	openFreshStore(t, baselinePath)
	chainPath := filepath.Join(dir, "chain.db")
	buildChainReference(t, chainPath)

	baselineDump := dumpDatabase(t, baselinePath)
	chainDump := dumpDatabase(t, chainPath)
	if bytes.Equal(baselineDump, chainDump) {
		return
	}
	baselineLines := strings.Split(string(baselineDump), "\n")
	chainLines := strings.Split(string(chainDump), "\n")
	shown := 0
	for i := 0; i < len(baselineLines) || i < len(chainLines); i++ {
		var a, b string
		if i < len(baselineLines) {
			a = baselineLines[i]
		}
		if i < len(chainLines) {
			b = chainLines[i]
		}
		if a != b {
			t.Errorf("canonical dump diverges at line %d:\n  baseline: %s\n  chain:    %s", i+1, a, b)
			shown++
			if shown >= 20 {
				break
			}
		}
	}
	t.Fatalf("a baseline-created database differs from the migrated chain (%d vs %d dump lines)", len(baselineLines), len(chainLines))
}

// TestBaselineSchemaIsCurrent is the pin: two independent chain builds
// serialize byte-identically, and the committed artifact matches a fresh one.
func TestBaselineSchemaIsCurrent(t *testing.T) {
	first, err := GenerateSchemaBaselineSQL()
	if err != nil {
		t.Fatalf("GenerateSchemaBaselineSQL (first build): %v", err)
	}
	second, err := GenerateSchemaBaselineSQL()
	if err != nil {
		t.Fatalf("GenerateSchemaBaselineSQL (second build): %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("two independent chain builds serialized different artifacts; the generator depends on uncaptured nondeterminism")
	}
	if !bytes.Equal(first, []byte(baselineSchemaSQL)) {
		t.Fatalf("internal/store/baseline_schema.sql is stale; regenerate it with `go generate ./internal/store`")
	}
}
