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

//go:embed testdata/generation_skip.yaml
var generationSkipYAML []byte

//go:embed testdata/generation_skip.manifest.yaml
var generationSkipManifestYAML []byte

// loadGenerationSkipFixture loads the generation skip scaffold family; later issues extend
// the returned shape with typed expectations.
func loadGenerationSkipFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "generation skip", generationSkipYAML, generationSkipManifestYAML)
}

// TestGenerationSkipFixtureManifest pins the generation skip case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestGenerationSkipFixtureManifest(t *testing.T) {
	loadGenerationSkipFixture(t)
}

//go:embed testdata/content_crash_seams.yaml
var contentCrashSeamsYAML []byte

//go:embed testdata/content_crash_seams.manifest.yaml
var contentCrashSeamsManifestYAML []byte

// loadContentCrashSeamsFixture loads the content crash seams scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentCrashSeamsFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content crash seams", contentCrashSeamsYAML, contentCrashSeamsManifestYAML)
}

// TestContentCrashSeamsFixtureManifest pins the content crash seams case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentCrashSeamsFixtureManifest(t *testing.T) {
	loadContentCrashSeamsFixture(t)
}

//go:embed testdata/content_enospc.yaml
var contentEnospcYAML []byte

//go:embed testdata/content_enospc.manifest.yaml
var contentEnospcManifestYAML []byte

// loadContentENOSPCFixture loads the content disk-full scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentENOSPCFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content disk-full", contentEnospcYAML, contentEnospcManifestYAML)
}

// TestContentENOSPCFixtureManifest pins the content disk-full case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentENOSPCFixtureManifest(t *testing.T) {
	loadContentENOSPCFixture(t)
}

//go:embed testdata/content_write_budget.yaml
var contentWriteBudgetYAML []byte

//go:embed testdata/content_write_budget.manifest.yaml
var contentWriteBudgetManifestYAML []byte

// loadContentWriteBudgetFixture loads the content write budget scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentWriteBudgetFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content write budget", contentWriteBudgetYAML, contentWriteBudgetManifestYAML)
}

// TestContentWriteBudgetFixtureManifest pins the content write budget case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentWriteBudgetFixtureManifest(t *testing.T) {
	loadContentWriteBudgetFixture(t)
}

//go:embed testdata/session_captured_stats.yaml
var sessionCapturedStatsYAML []byte

//go:embed testdata/session_captured_stats.manifest.yaml
var sessionCapturedStatsManifestYAML []byte

// loadSessionCapturedStatsFixture loads the session captured stats scaffold family; later issues extend
// the returned shape with typed expectations.
func loadSessionCapturedStatsFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "session captured stats", sessionCapturedStatsYAML, sessionCapturedStatsManifestYAML)
}

// TestSessionCapturedStatsFixtureManifest pins the session captured stats case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestSessionCapturedStatsFixtureManifest(t *testing.T) {
	loadSessionCapturedStatsFixture(t)
}

//go:embed testdata/content_corruption.yaml
var contentCorruptionYAML []byte

//go:embed testdata/content_corruption.manifest.yaml
var contentCorruptionManifestYAML []byte

// loadContentCorruptionFixture loads the content corruption scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentCorruptionFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content corruption", contentCorruptionYAML, contentCorruptionManifestYAML)
}

// TestContentCorruptionFixtureManifest pins the content corruption case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentCorruptionFixtureManifest(t *testing.T) {
	loadContentCorruptionFixture(t)
}

//go:embed testdata/content_concurrency.yaml
var contentConcurrencyYAML []byte

//go:embed testdata/content_concurrency.manifest.yaml
var contentConcurrencyManifestYAML []byte

// loadContentConcurrencyFixture loads the content concurrency scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentConcurrencyFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content concurrency", contentConcurrencyYAML, contentConcurrencyManifestYAML)
}

// TestContentConcurrencyFixtureManifest pins the content concurrency case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentConcurrencyFixtureManifest(t *testing.T) {
	loadContentConcurrencyFixture(t)
}

//go:embed testdata/content_hostile_input.yaml
var contentHostileInputYAML []byte

//go:embed testdata/content_hostile_input.manifest.yaml
var contentHostileInputManifestYAML []byte

// loadContentHostileInputFixture loads the content hostile input scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentHostileInputFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content hostile input", contentHostileInputYAML, contentHostileInputManifestYAML)
}

// TestContentHostileInputFixtureManifest pins the content hostile input case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentHostileInputFixtureManifest(t *testing.T) {
	loadContentHostileInputFixture(t)
}

//go:embed testdata/content_gc.yaml
var contentGcYAML []byte

//go:embed testdata/content_gc.manifest.yaml
var contentGcManifestYAML []byte

// loadContentGCFixture loads the content garbage collection scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentGCFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content garbage collection", contentGcYAML, contentGcManifestYAML)
}

// TestContentGCFixtureManifest pins the content garbage collection case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentGCFixtureManifest(t *testing.T) {
	loadContentGCFixture(t)
}

//go:embed testdata/content_migration.yaml
var contentMigrationYAML []byte

//go:embed testdata/content_migration.manifest.yaml
var contentMigrationManifestYAML []byte

// loadContentMigrationFixture loads the content migration scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentMigrationFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content migration", contentMigrationYAML, contentMigrationManifestYAML)
}

// TestContentMigrationFixtureManifest pins the content migration case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentMigrationFixtureManifest(t *testing.T) {
	loadContentMigrationFixture(t)
}

//go:embed testdata/content_release_guard.yaml
var contentReleaseGuardYAML []byte

//go:embed testdata/content_release_guard.manifest.yaml
var contentReleaseGuardManifestYAML []byte

// loadContentReleaseGuardFixture loads the content release guard scaffold family; later issues extend
// the returned shape with typed expectations.
func loadContentReleaseGuardFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "content release guard", contentReleaseGuardYAML, contentReleaseGuardManifestYAML)
}

// TestContentReleaseGuardFixtureManifest pins the content release guard case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestContentReleaseGuardFixtureManifest(t *testing.T) {
	loadContentReleaseGuardFixture(t)
}

//go:embed testdata/search_recall.yaml
var searchRecallYAML []byte

//go:embed testdata/search_recall.manifest.yaml
var searchRecallManifestYAML []byte

// loadSearchRecallFixture loads the search recall scaffold family; later issues extend
// the returned shape with typed expectations.
func loadSearchRecallFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "search recall", searchRecallYAML, searchRecallManifestYAML)
}

// TestSearchRecallFixtureManifest pins the search recall case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestSearchRecallFixtureManifest(t *testing.T) {
	loadSearchRecallFixture(t)
}

//go:embed testdata/annotation_targets_no_fk.yaml
var annotationTargetsNoFkYAML []byte

//go:embed testdata/annotation_targets_no_fk.manifest.yaml
var annotationTargetsNoFkManifestYAML []byte

// loadAnnotationTargetsNoFKFixture loads the annotation targets without foreign keys scaffold family; later issues extend
// the returned shape with typed expectations.
func loadAnnotationTargetsNoFKFixture(t *testing.T) []string {
	t.Helper()
	return loadContentModelScaffoldFixture(t, "annotation targets without foreign keys", annotationTargetsNoFkYAML, annotationTargetsNoFkManifestYAML)
}

// TestAnnotationTargetsNoFKFixtureManifest pins the annotation targets without foreign keys case inventory: the loader
// compiles, the manifest loads, and every required name is present.
func TestAnnotationTargetsNoFKFixtureManifest(t *testing.T) {
	loadAnnotationTargetsNoFKFixture(t)
}
