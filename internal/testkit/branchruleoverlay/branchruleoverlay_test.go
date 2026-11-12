package branchruleoverlay

import (
	_ "embed"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/rejections.yaml
var rejectionsData []byte

func TestLoadRefusesAnOverlayThatIsNotWhatItSays(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name      string                            `yaml:"name"`
			Overlay   string                            `yaml:"overlay"`
			Captured  map[string]config.SelectionConfig `yaml:"captured"`
			WantError string                            `yaml:"wantError"`
			Want      config.SelectionConfig            `yaml:"want"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(rejectionsData, &fixtures); err != nil {
		t.Fatalf("decode testdata/rejections.yaml: %v", err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			overlay, err := load([]byte(c.Overlay), c.Captured)
			if c.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), c.WantError) {
					t.Fatalf("load error = %v, want one containing %q", err, c.WantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("load refused a well-formed overlay: %v", err)
			}
			if got := overlay.Want("whole", c.Captured["whole"]); !reflect.DeepEqual(got, c.Want) {
				t.Fatalf("Want = %+v, want %+v", got, c.Want)
			}
			if got := overlay.Want("not-overlaid", c.Captured["whole"]); !reflect.DeepEqual(got, c.Captured["whole"]) {
				t.Fatalf("a scenario with no entry is measured against its capture, got %+v", got)
			}
		})
	}
}
