package config

import (
	_ "embed"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/staleness_window.yaml
var stalenessWindowData []byte

func TestStalenessWindow(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name         string `yaml:"name"`
			NoConfig     bool   `yaml:"noConfig"`
			ThresholdSec int    `yaml:"thresholdSec"`
			WantSeconds  int    `yaml:"wantSeconds"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(stalenessWindowData, &fixtures); err != nil {
		t.Fatalf("decode staleness window fixture: %v", err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			var cfg *Config
			if !c.NoConfig {
				cfg = &Config{Output: OutputConfig{StalenessThresholdSec: c.ThresholdSec}}
			}
			if got, want := cfg.StalenessWindow(), time.Duration(c.WantSeconds)*time.Second; got != want {
				t.Fatalf("StalenessWindow() = %v, want %v", got, want)
			}
		})
	}
}
