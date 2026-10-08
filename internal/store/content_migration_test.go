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
// plus, for the cases this migration executes, the predecessor seed and the
// expected backfill in the v62StatsCase shape. Cases owned by the conversion
// command carry ownedBy instead and assert nothing here.
type contentMigrationCase struct {
	v62StatsCase `yaml:",inline"`
	OwnedBy      string `yaml:"ownedBy,omitempty"`
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
// either copy fails a required-name manifest. native-extra-keys-promoted is
// owned by the conversion command (extra-key promotion is conversion-time);
// v62 carries those keys through untouched, so the case records that
// ownership and asserts nothing here. A case that carries no sessionId is a
// placeholder the conversion command fills later; the loader's manifest
// check still protects its name.
func TestContentMigrationBackfills(t *testing.T) {
	t.Parallel()
	fixtures := loadContentMigrationFixtures(t)
	for _, c := range fixtures.Cases {
		if c.OwnedBy != "" {
			t.Logf("%s: owned by %s; this migration carries the keys through untouched and asserts nothing", c.Name, c.OwnedBy)
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
