package store

import (
	_ "embed"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_migration.yaml
var contentMigrationYAML []byte

//go:embed testdata/content_migration.manifest.yaml
var contentMigrationManifestYAML []byte

// contentMigrationCase is one content_migration case: the section-10 name
// plus, for the v62-owned stats case, the predecessor seed and the
// expected backfill in the v62StatsCase shape. Cases owned by the
// conversion carry a seed profile, an optional single-dimension damage,
// and the expected disposition instead; the conversion runner builds
// the file-backed seed from the profile, applies the damage, runs the
// conversion, and asserts the end state.
type contentMigrationCase struct {
	v62StatsCase `yaml:",inline"`
	OwnedBy      string `yaml:"ownedBy,omitempty"`
	// Profile names the file-backed seed the runner builds: clean,
	// superseded, pending-intent, non-emitted, extra-keys, newer-stats,
	// rich-metadata, annotated, preview-only, non-native-full,
	// non-native-preview-only, settled-refusal, parent-only-metadata,
	// parent-row-wins.
	Profile string `yaml:"profile,omitempty"`
	// Expect is the conversion disposition: converted, rolled-back,
	// halted, skipped.
	Expect string `yaml:"expect,omitempty"`
	// Damage corrupts exactly one dimension of the seeded input or of
	// the conversion's derived output: field-blob, missing-blob,
	// damaged-blob, serialization, shim, aux-json, full-shape,
	// capture-hash, session-hash, null-hash, detail-bytes, unknown-key.
	// stale-hash is not a corruption: it backdates the stored hash the
	// way live resumes do, and the conversion still proceeds.
	Damage string `yaml:"damage,omitempty"`
	// ExpectDimension pins the rollback dimension for damages that must
	// refuse at one exact check.
	ExpectDimension string `yaml:"expectDimension,omitempty"`
	// Count seeds that many identical sessions for the driver-level
	// threshold case.
	Count int `yaml:"count,omitempty"`
	// Driver marks a driver-level case: threshold, consolidate,
	// consolidate-unconverted, consolidate-twice, dry-run, resume.
	Driver string `yaml:"driver,omitempty"`
	// Crash fabricates one crash artifact before the resume run:
	// stage-once and shadow-once fail their seam once; converted-with-dirs
	// converts, then restores owned files, a mirror row, and the flag.
	Crash string `yaml:"crash,omitempty"`
	// Heal restores the damage after the rollback and converts on the
	// re-run.
	Heal        bool   `yaml:"heal,omitempty"`
	Drain       string `yaml:"drain,omitempty"`
	DrainRow    bool   `yaml:"drainRow,omitempty"`
	DrainIntent bool   `yaml:"drainIntent,omitempty"`
	DrainCrash  bool   `yaml:"drainCrash,omitempty"`
	// AssertSkipState proves the conversion preserves the session's
	// skip and bookkeeping state.
	AssertSkipState bool `yaml:"assertSkipState,omitempty"`
}

type contentMigrationFixtures struct {
	Cases []contentMigrationCase `yaml:"cases"`
}

func loadContentMigrationFixtures(t *testing.T) contentMigrationFixtures {
	t.Helper()
	var fixtures contentMigrationFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(contentMigrationYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode content_migration.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(contentMigrationManifestYAML)
	if err != nil {
		t.Fatalf("decode content_migration manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "content migration"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestContentMigrationBackfills runs the content_migration cases this
// migration owns under their section-10 names. native-stats-backfilled
// aliases the v62 stats/sweep backfill: it seeds the same V61 predecessor
// shape and runs the same assertions as the migration_v62 family, so deleting
// either copy fails a required-name manifest. Conversion-owned cases with a
// sessionId run the v62 assertions; conversion-owned cases with a profile
// run the conversion runner (TestContentMigrationConversion), which builds
// the file-backed seed, applies the damage, converts, and asserts the end
// state. A case that carries neither is a placeholder the manifest still
// protects by name.
func TestContentMigrationBackfills(t *testing.T) {
	t.Parallel()
	fixtures := loadContentMigrationFixtures(t)
	for _, c := range fixtures.Cases {
		if c.OwnedBy == "conversion" && c.SessionID == "" && c.Profile == "" {
			t.Logf("%s: placeholder; the conversion runner fills this case", c.Name)
			continue
		}
		if c.OwnedBy != "" && c.SessionID == "" {
			continue
		}
		if c.SessionID == "" {
			t.Logf("%s: placeholder; the conversion command fills this case", c.Name)
			continue
		}
		runContentMigrationStatsCase(t, c.v62StatsCase)
	}
}

// runContentMigrationStatsCase migrates a V61 database seeded with one
// content_migration stats case to the head and asserts the stats row and the
// sweep flag, reusing the migration_v62 seed and assertion helpers so the two
// families prove the same backfill.
func runContentMigrationStatsCase(t *testing.T, c v62StatsCase) {
	t.Helper()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "content-migration.db")
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	predecessor := sqlitemigration.Schema{
		Migrations:       dbSchema.Migrations[:61],
		MigrationOptions: dbSchema.MigrationOptions[:61],
	}
	if err := sqlitemigration.Migrate(ctx, conn, predecessor); err != nil {
		t.Fatalf("migrate to V61: %v", err)
	}

	seed := v62Fixtures{StatsCases: []v62StatsCase{c}}
	seedV62Predecessor(t, conn, seed)

	if err := sqlitemigration.Migrate(ctx, conn, dbSchema); err != nil {
		t.Fatalf("migrate to V62: %v", err)
	}

	assertV62StatsBackfill(t, conn, seed)
}
