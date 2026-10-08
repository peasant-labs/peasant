package ingest

import (
	_ "embed"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/write_config_cases.yaml
var writeConfigCasesYAML []byte

// TestWriteConfigCases holds the write.* bounds at the configuration
// boundary: every fragment decodes through the YAML shape and resolves
// against the defaults, and validation accepts exactly the usable ones. The
// corpus names its cases in a manifest, so deleting one fails the load
// instead of quietly shrinking the run.
func TestWriteConfigCases(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name  string         `yaml:"name"`
			Write map[string]any `yaml:"write"`
			Valid bool           `yaml:"valid"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(writeConfigCasesYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		if present[fixture.Name] {
			t.Fatalf("duplicate fixture %s", fixture.Name)
		}
		present[fixture.Name] = true
	}
	requireFixtureNames(t, "write config", fixtures.Required, present)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			fragment, err := yaml.Marshal(map[string]any{"write": fixture.Write})
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Write WriteConfig `yaml:"write"`
			}
			if err := yaml.Unmarshal(fragment, &decoded); err != nil {
				t.Fatalf("decode write.* fragment: %v", err)
			}
			_, err = decoded.Write.validatedWithDefaults(8)
			if (err == nil) != fixture.Valid {
				t.Fatalf("validatedWithDefaults(8) err=%v, want valid=%v for fragment %v", err, fixture.Valid, fixture.Write)
			}
		})
	}
}

// TestDefaultWriteConfigDerivations pins the shipped budgets to the design's
// knob table (§0.5): the hold target at 0.8 × the open path's busy_timeout,
// the count caps, the byte caps, the flush interval, the sweep rows, the
// harvest target, and the staged-memory derivation at the worker count.
func TestDefaultWriteConfigDerivations(t *testing.T) {
	t.Parallel()
	cfg := DefaultWriteConfig(8)
	if cfg.HoldTarget != 4*time.Second {
		t.Fatalf("HoldTarget=%s, want 4s (0.8 × the 5 s open-path busy_timeout)", cfg.HoldTarget)
	}
	if cfg.ActivationSessions != 64 {
		t.Fatalf("ActivationSessions=%d, want 64", cfg.ActivationSessions)
	}
	if cfg.BatchBytes != 32<<20 {
		t.Fatalf("BatchBytes=%d, want 32 MiB", cfg.BatchBytes)
	}
	if cfg.BatchSessions != 64 {
		t.Fatalf("BatchSessions=%d, want 64", cfg.BatchSessions)
	}
	if cfg.BufferBytes != 4<<20 {
		t.Fatalf("BufferBytes=%d, want 4 MiB", cfg.BufferBytes)
	}
	if want := int64(2*(32<<20) + 8*(4<<20)); cfg.StagedMemoryBytes != want {
		t.Fatalf("StagedMemoryBytes=%d, want %d (2×BatchBytes + 8×BufferBytes)", cfg.StagedMemoryBytes, want)
	}
	if cfg.FlushIntervalMs != 500 {
		t.Fatalf("FlushIntervalMs=%d, want 500", cfg.FlushIntervalMs)
	}
	if cfg.SweepRows != 5000 {
		t.Fatalf("SweepRows=%d, want 5000", cfg.SweepRows)
	}
	if cfg.HarvestTarget != 20*time.Minute {
		t.Fatalf("HarvestTarget=%s, want 20m", cfg.HarvestTarget)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the shipped defaults must validate: %v", err)
	}
}

// TestWriteConfigDocumentedShape decodes the design's YAML shape verbatim, so
// the keys S3 promises are the keys the pipeline reads.
func TestWriteConfigDocumentedShape(t *testing.T) {
	t.Parallel()
	var decoded struct {
		Write WriteConfig `yaml:"write"`
	}
	if err := yaml.Unmarshal([]byte(`write:
  holdTargetMs: 4000
  activationSessions: 64
  batchBytes: 33554432
  batchSessions: 64
  bufferBytes: 4194304
  stagedMemoryBytes: 100663296
  flushIntervalMs: 500
  sweepRows: 5000
  harvestTargetMinutes: 20
`), &decoded); err != nil {
		t.Fatal(err)
	}
	want := WriteConfig{
		HoldTarget:         4 * time.Second,
		ActivationSessions: 64,
		BatchBytes:         33554432,
		BatchSessions:      64,
		BufferBytes:        4194304,
		StagedMemoryBytes:  100663296,
		FlushIntervalMs:    500,
		SweepRows:          5000,
		HarvestTarget:      20 * time.Minute,
	}
	if !reflect.DeepEqual(decoded.Write, want) {
		t.Fatalf("decoded=%+v, want %+v", decoded.Write, want)
	}
}

// TestWriteConfigRoundTrip saves the resolved defaults and reads them back,
// so a config the app writes is a config the app accepts with the same
// budgets.
func TestWriteConfigRoundTrip(t *testing.T) {
	t.Parallel()
	cfg := DefaultWriteConfig(8)
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded WriteConfig
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, cfg) {
		t.Fatalf("round trip changed the budgets: got %+v, want %+v", decoded, cfg)
	}
}

// TestPipelineWriteConfigZeroResolvesDefaults holds the production entry to
// the write path: a pipeline built by hand with no write.* section runs the
// shipped budgets at its effective worker count.
func TestPipelineWriteConfigZeroResolvesDefaults(t *testing.T) {
	t.Parallel()
	p := &Pipeline{config: PipelineConfig{OutputDir: ResolvedPath(t.TempDir())}}
	got := p.writeConfig()
	want := DefaultWriteConfig(runtime.NumCPU())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("writeConfig()=%+v, want shipped defaults %+v", got, want)
	}
	if got.BatchBytes != defaults.FullContentWriteBatchBytes {
		t.Fatalf("BatchBytes=%d, want the legacy drain budget %d", got.BatchBytes, defaults.FullContentWriteBatchBytes)
	}
}
