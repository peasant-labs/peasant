package store_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/annotation_targets_no_fk.yaml
var annotationTargetsYAML []byte

//go:embed testdata/annotation_targets_no_fk.manifest.yaml
var annotationTargetsManifestYAML []byte

type annotationTargetCase struct {
	Name         string   `yaml:"name"`
	Action       string   `yaml:"action"`
	SessionIDs   []string `yaml:"sessionIDs"`
	EntryIndex   int      `yaml:"entryIndex"`
	BatchIndexes []int    `yaml:"batchIndexes"`
	WantRefused  bool     `yaml:"wantRefused"`
	Human        bool     `yaml:"human"`
	WantState    string   `yaml:"wantState"`
	Previews     []string `yaml:"previews"`
	Replacement  []string `yaml:"replacement"`
	WantIndex    int      `yaml:"wantIndex"`
}

func LoadAnnotationTargetsNoFKFixtures(t *testing.T) []annotationTargetCase {
	t.Helper()
	var fixtures struct {
		Cases []annotationTargetCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(annotationTargetsYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(annotationTargetsManifestYAML, "annotation targets")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixtures.Cases {
		if c.Action == "" || len(c.SessionIDs) == 0 {
			t.Fatalf("annotation target %q lacks action or session IDs", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "annotation targets"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

func TestAnnotationTargetsNoFK(t *testing.T) {
	for _, c := range LoadAnnotationTargetsNoFKFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Action {
			case "create":
				runAnnotationCreate(t, c)
			case "supersede", "batch":
				runAnnotationSupersedeOrBatch(t, c)
			case "classifier":
				runClassifierInsertRefused(t, c)
			case "remap":
				runAnnotationRemap(t, c)
			case "restore":
				runAnnotationRestore(t, c)
			case "prune":
				runAnnotationPrune(t, c)
			default:
				t.Fatalf("unknown annotation action %q", c.Action)
			}
		})
	}
}
