package store

import (
	_ "embed"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Fixture scaffolding for the harmonized session content model (design
// llm/peasant--harmonized-content-model.md section 10, ratified revision 17).
//
// Each family below owns a <family>.yaml of typed cases and a
// <family>.manifest.yaml listing requiredNames, following the existing
// generation_inherited_content idiom: deletion protection by required NAME,
// never by bare count. No cases are filled yet; every entry is a name
// placeholder so the manifest guards the inventory from the start. The
// later issues extend the case shape and these loaders field-for-field.

// contentModelScaffoldCase is the minimal case shape: a name only. Later
// issues add typed expectation fields beside it.
type contentModelScaffoldCase struct {
	Name string `yaml:"name"`
}

type contentModelScaffoldFixture struct {
	Cases []contentModelScaffoldCase `yaml:"cases"`
}

// loadContentModelScaffoldFixture strictly decodes one scaffold family and
// enforces its required-names manifest in both directions: every required
// name must be present and every present case must be declared, so a
// deletion or rename goes red.
func loadContentModelScaffoldFixture(t *testing.T, label string, fixtureYAML, manifestYAML []byte) []string {
	t.Helper()
	var fixture contentModelScaffoldFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(fixtureYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode %s fixture: %v", label, err)
	}
	manifest, err := decodeRecoveryRequiredNames(manifestYAML)
	if err != nil {
		t.Fatalf("decode %s manifest: %v", label, err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, label); err != nil {
		t.Fatal(err)
	}
	return actual
}

//go:embed testdata/content_model_parity.yaml
var contentModelParityYAML []byte

//go:embed testdata/content_model_parity.manifest.yaml
var contentModelParityManifestYAML []byte

// loadContentModelParityFixture loads the content model parity scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentModelParityFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content model parity", contentModelParityYAML, contentModelParityManifestYAML)
}

// TestContentModelParityFixtureManifest pins the content model parity case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentModelParityFixtureManifest(t *testing.T) {
	loadContentModelParityFixture(t)
}

//go:embed testdata/content_promotion.yaml
var contentPromotionYAML []byte

//go:embed testdata/content_promotion.manifest.yaml
var contentPromotionManifestYAML []byte

// loadContentPromotionFixture loads the content promotion scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentPromotionFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content promotion", contentPromotionYAML, contentPromotionManifestYAML)
}

// TestContentPromotionFixtureManifest pins the content promotion case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentPromotionFixtureManifest(t *testing.T) {
	loadContentPromotionFixture(t)
}

//go:embed testdata/content_one_copy.yaml
var contentOneCopyYAML []byte

//go:embed testdata/content_one_copy.manifest.yaml
var contentOneCopyManifestYAML []byte

// loadContentOneCopyFixture loads the content one copy scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentOneCopyFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content one copy", contentOneCopyYAML, contentOneCopyManifestYAML)
}

// TestContentOneCopyFixtureManifest pins the content one copy case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentOneCopyFixtureManifest(t *testing.T) {
	loadContentOneCopyFixture(t)
}

// The content crash seams family graduated to a typed loader with seam
// expectations in harmonized_crash_test.go, which supersedes the name-only
// scaffold here (the manifest inventory it protects is unchanged).

// content_enospc is owned by the typed loader in
// content_migration_enospc_test.go, which enforces the required-name
// manifest over the full family; no scaffold loader remains here.

// content_write_budget is owned by the typed loader in
// content_write_budget_test.go; the full required-name inventory stays
// manifest-protected there.

//go:embed testdata/content_corruption.yaml
var contentCorruptionYAML []byte

//go:embed testdata/content_corruption.manifest.yaml
var contentCorruptionManifestYAML []byte

// contentCorruptionCase binds each required name to its executable owner and
// damage profile. Full-read cases refuse partial output, preview and routing
// deliberately remain unverified, and verify owns non-emitted blob integrity.
type contentCorruptionCase struct {
	Name                    string   `yaml:"name"`
	Owner                   string   `yaml:"owner"`
	Surfaces                []string `yaml:"surfaces,omitempty"`
	Expect                  string   `yaml:"expect,omitempty"`
	Why                     string   `yaml:"why,omitempty"`
	Query                   string   `yaml:"query,omitempty"`
	WantFoundOnce           bool     `yaml:"wantFoundOnce,omitempty"`
	WantStaleRawMatch       bool     `yaml:"wantStaleRawMatch,omitempty"`
	WantRefusedWhileFlagged bool     `yaml:"wantRefusedWhileFlagged,omitempty"`
	WantCleanAfterRebuild   bool     `yaml:"wantCleanAfterRebuild,omitempty"`
	// Damage names the corruption the repair runner applies before
	// verification: body-column, blob-bytes, descriptor-digest.
	Damage string `yaml:"damage,omitempty"`
}

type contentCorruptionFixtures struct {
	Cases []contentCorruptionCase `yaml:"cases"`
}

// loadContentCorruptionCases strictly decodes the typed content corruption
// family and enforces its required-names manifest in both directions.
func loadContentCorruptionCases(t *testing.T) []contentCorruptionCase {
	t.Helper()
	var fixtures contentCorruptionFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(contentCorruptionYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode content_corruption.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(contentCorruptionManifestYAML)
	if err != nil {
		t.Fatalf("decode content corruption manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		if strings.TrimSpace(c.Name) == "" {
			t.Fatal("content_corruption.yaml: a case has a blank name")
		}
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "content corruption"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

// loadContentCorruptionFixture loads the content corruption family names;
// the typed loader above carries the expectations the runners assert.
func loadContentCorruptionFixture(t *testing.T) []string {
	t.Helper()
	cases := loadContentCorruptionCases(t)
	names := make([]string, 0, len(cases))
	for _, c := range cases {
		names = append(names, c.Name)
	}
	return names
}

// TestContentCorruptionFixtureManifest pins the content corruption case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentCorruptionFixtureManifest(t *testing.T) {
	loadContentCorruptionFixture(t)
}

// The content garbage collection family graduated to a typed loader with
// count, flag, and MATCH expectations in content_gc_test.go, which
// supersedes the name-only scaffold here (the manifest inventory it protects
// is unchanged).

// content_migration is owned by the typed loader in content_migration_test.go;
// the full required-name inventory stays manifest-protected there.

// content_release_guard is owned by the typed loader in
// content_release_guard_test.go; the full required-name inventory stays
// manifest-protected there.

//go:embed testdata/search_recall.yaml
var searchRecallYAML []byte

//go:embed testdata/search_recall.manifest.yaml
var searchRecallManifestYAML []byte

// searchRecallCase is one search_recall case: the section-10 name plus the
// query term and the production result the consolidated index must return.
// Cases this runner covers carry typed expectations it asserts; the
// loader's manifest check still protects every name.
type searchRecallCase struct {
	Name                    string `yaml:"name"`
	Query                   string `yaml:"query,omitempty"`
	WantCount               *int   `yaml:"wantCount,omitempty"`
	WantEntryIndexes        []int  `yaml:"wantEntryIndexes,omitempty"`
	WantRawMatchCount       *int   `yaml:"wantRawMatchCount,omitempty"`
	Beyond2000              bool   `yaml:"beyond2000,omitempty"`
	WantPushdown            bool   `yaml:"wantPushdown,omitempty"`
	WantNoDuplicates        bool   `yaml:"wantNoDuplicates,omitempty"`
	WantReverseQuery        string `yaml:"wantReverseQuery,omitempty"`
	WantReverseCount        *int   `yaml:"wantReverseCount,omitempty"`
	WantReverseEntryIndexes []int  `yaml:"wantReverseEntryIndexes,omitempty"`
	WantFirstBodyID         *int64 `yaml:"wantFirstBodyID,omitempty"`
	WantCeilingRefused      bool   `yaml:"wantCeilingRefused,omitempty"`
	FallbackDualSource      bool   `yaml:"fallbackDualSource,omitempty"`
}

type searchRecallFixtures struct {
	Cases []searchRecallCase `yaml:"cases"`
}

// loadSearchRecallFixtures strictly decodes the typed search recall family
// and enforces its required-names manifest in both directions.
func loadSearchRecallFixtures(t *testing.T) []searchRecallCase {
	t.Helper()
	var fixtures searchRecallFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(searchRecallYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode search_recall.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(searchRecallManifestYAML)
	if err != nil {
		t.Fatalf("decode search recall manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		if strings.TrimSpace(c.Name) == "" {
			t.Fatal("search_recall.yaml: a case has a blank name")
		}
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "search recall"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

// loadSearchRecallFixture loads the search recall family names; the typed
// loader above carries the expectations the runners assert.
func loadSearchRecallFixture(t *testing.T) []string {
	t.Helper()
	cases := loadSearchRecallFixtures(t)
	names := make([]string, 0, len(cases))
	for _, c := range cases {
		names = append(names, c.Name)
	}
	return names
}

// TestSearchRecallFixtureManifest pins the search recall case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestSearchRecallFixtureManifest(t *testing.T) {
	loadSearchRecallFixture(t)
}
