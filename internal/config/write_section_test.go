package config

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestParse_WriteSection holds the write.* wiring at the config boundary: a
// document naming the section lands its knobs on the pipeline's config, with
// durations converted and the unnamed staged-memory cap left to derive.
func TestParse_WriteSection(t *testing.T) {
	cfg, err := Parse([]byte(`version: 1
write:
  holdTargetMs: 4000
  activationSessions: 64
  batchBytes: 33554432
  batchSessions: 64
  bufferBytes: 4194304
  stagedMemoryBytes: 100663296
  flushIntervalMs: 500
  sweepRows: 5000
  harvestTargetMinutes: 20
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Write.HoldTarget != 4*time.Second {
		t.Fatalf("HoldTarget=%s, want 4s", cfg.Write.HoldTarget)
	}
	if cfg.Write.BatchBytes != 33554432 {
		t.Fatalf("BatchBytes=%d, want 33554432", cfg.Write.BatchBytes)
	}
	if cfg.Write.StagedMemoryBytes != 100663296 {
		t.Fatalf("StagedMemoryBytes=%d, want 100663296", cfg.Write.StagedMemoryBytes)
	}
	if cfg.Write.HarvestTarget != 20*time.Minute {
		t.Fatalf("HarvestTarget=%s, want 20m", cfg.Write.HarvestTarget)
	}
	if cfg.Write.FlushIntervalMs != 500 {
		t.Fatalf("FlushIntervalMs=%d, want 500", cfg.Write.FlushIntervalMs)
	}
}

// TestParse_WriteSectionInvalid fails the load before the first session is
// read: a hold past the open path's busy_timeout refuses with the bound.
func TestParse_WriteSectionInvalid(t *testing.T) {
	_, err := Parse([]byte("version: 1\nwrite:\n  holdTargetMs: 99999\n"))
	if err == nil {
		t.Fatal("a hold past the open path's busy_timeout must refuse the config")
	}
	if !strings.Contains(err.Error(), "holdTargetMs") {
		t.Fatalf("the refusal must name the knob, got: %v", err)
	}
}

// TestBaseConfig_WriteDefaults holds what a fresh config carries: the
// shipped scalar budgets, and an unset staged-memory cap that derives from
// the run's worker count instead of pinning this machine's count.
func TestBaseConfig_WriteDefaults(t *testing.T) {
	write := BaseConfig().Write
	if write.HoldTarget != defaults.WriteDefaultHoldTarget {
		t.Fatalf("HoldTarget=%s, want the shipped default %s", write.HoldTarget, defaults.WriteDefaultHoldTarget)
	}
	if write.BatchBytes != defaults.FullContentWriteBatchBytes {
		t.Fatalf("BatchBytes=%d, want the shipped budget %d", write.BatchBytes, defaults.FullContentWriteBatchBytes)
	}
	if write.BufferBytes != int64(defaults.WriteDefaultBufferBytes) {
		t.Fatalf("BufferBytes=%d, want the shipped default %d", write.BufferBytes, defaults.WriteDefaultBufferBytes)
	}
	if write.StagedMemoryBytes != 0 {
		t.Fatalf("StagedMemoryBytes=%d, want unset (it derives at the run's worker count)", write.StagedMemoryBytes)
	}
	if err := write.WithDefaults(runtime.NumCPU()).Validate(); err != nil {
		t.Fatalf("the base write section must resolve clean: %v", err)
	}
}
