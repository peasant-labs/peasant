package discovery_test

import (
	"context"
	_ "embed"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/discovery_progress.yaml
var scanProgressYAML []byte

//go:embed testdata/discovery_progress.manifest.yaml
var scanProgressManifest []byte

type scanProgressCase struct {
	Name  string `yaml:"name"`
	Root  string `yaml:"root"`
	Files []struct {
		Path string `yaml:"path"`
		Body string `yaml:"body"`
	} `yaml:"files"`
	Phase  string `yaml:"phase"`
	Counts []int  `yaml:"counts"`
	Total  int    `yaml:"total"`
	// Rereads is the required re-read count the display receives after each
	// source advance, in order.
	Rereads []int `yaml:"rereads"`
}

type scanProgressObservation struct {
	phase       string
	done, total int
}

type scanProgressRecorder struct {
	phase        string
	observations []scanProgressObservation
	rereads      []int
}

func (r *scanProgressRecorder) SetPhase(phase string) { r.phase = phase }
func (r *scanProgressRecorder) SetProgress(done, total int) {
	r.observations = append(r.observations, scanProgressObservation{phase: r.phase, done: done, total: total})
}

func (r *scanProgressRecorder) SetDiscoveryRequiredRereads(count int) {
	r.rereads = append(r.rereads, count)
}

var _ discovery.ScanProgress = (*scanProgressRecorder)(nil)
var _ discovery.ScanRereadProgress = (*scanProgressRecorder)(nil)

func TestScanReportsDiscoveryProgress(t *testing.T) {
	var fixture struct {
		Cases []scanProgressCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(scanProgressYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(scanProgressManifest, "scan discovery progress")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "scan discovery progress"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			for _, file := range c.Files {
				if err := fs.WriteFile(c.Root+"/"+file.Path, []byte(file.Body+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{Sources: config.SourcesConfig{ClaudeCode: config.SourceProviderConfig{Enabled: true, Paths: []string{c.Root}}}}
			var recorder scanProgressRecorder
			_, sessions, _ := discovery.Scan(context.Background(), cfg, fs, nil, nil, nil, &recorder)
			if len(sessions) != c.Total {
				t.Fatalf("discovered %d root sessions, want %d", len(sessions), c.Total)
			}
			var got []int
			for _, observed := range recorder.observations {
				if observed.phase == c.Phase {
					if observed.total != c.Total {
						t.Fatalf("discovery total = %d, want %d", observed.total, c.Total)
					}
					got = append(got, observed.done)
				}
			}
			if !reflect.DeepEqual(got, c.Counts) {
				t.Fatalf("counts during %q = %v, want %v (all observations: %+v)", c.Phase, got, c.Counts, recorder.observations)
			}
			if !reflect.DeepEqual(recorder.rereads, c.Rereads) {
				t.Fatalf("required re-reads during %q = %v, want %v", c.Phase, recorder.rereads, c.Rereads)
			}
		})
	}
}
