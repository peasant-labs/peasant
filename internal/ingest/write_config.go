package ingest

import (
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

// WriteConfig is the write.* configuration section: every write budget the
// pipeline enforces, read from config instead of compiled in. The defaults
// below are derived in the harmonized content-model design (§0.5) and
// re-derived by the sandbox run; the gates assert the invariants against the
// configured knobs, never the constants.
//
// The zero value means "every default": a PipelineConfig built by hand (as
// most tests do) resolves through withDefaults at the use sites, so existing
// callers keep the shipped behavior without naming a knob. A user-facing
// config fills the section through BaseConfig and Parse, which validate it.
//
// YAML keys are the design's write.* shape (durations as milliseconds and
// minutes); the Go fields are Durations so the write path reads them without
// converting at every use.
type WriteConfig struct {
	// HoldTarget bounds any writer-lane transaction's hold: 0.8 × the open
	// path's busy_timeout (4 s with today's fixed 5 s), leaving scheduling
	// headroom. YAML: holdTargetMs.
	HoldTarget time.Duration `yaml:"holdTargetMs"`
	// ActivationSessions bounds an activation batch: hold_target ÷ the
	// measured per-session commit cost. YAML: activationSessions.
	ActivationSessions int `yaml:"activationSessions"`
	// BatchBytes is the staging memory bound: a session whose objects exceed
	// it stages alone in budget-sized transactions. YAML: batchBytes.
	BatchBytes int64 `yaml:"batchBytes"`
	// BatchSessions is a convenience cap alongside the byte cap; the byte
	// cap is the binding bound. YAML: batchSessions.
	BatchSessions int `yaml:"batchSessions"`
	// BufferBytes is per-worker parser scratch headroom in the derived staged
	// budget. It does not allocate a separate buffer pool. YAML: bufferBytes.
	BufferBytes int64 `yaml:"bufferBytes"`
	// StagedMemoryBytes bounds admitted native candidates. Parser scratch has
	// separate per-worker headroom. Zero means derive: 2×BatchBytes (staging and
	// activation lanes' in-flight bounded batches) plus one BufferBytes per
	// effective worker. The prepare side blocks at the cap. YAML:
	// stagedMemoryBytes (omitted when derived).
	StagedMemoryBytes int64 `yaml:"stagedMemoryBytes,omitempty"`
	// FlushIntervalMs bounds the write lane's idle wait before it commits a
	// partial batch. YAML: flushIntervalMs.
	FlushIntervalMs int `yaml:"flushIntervalMs"`
	// SweepRows bounds a per-session sweep batch: hold_target ÷ the measured
	// delete rate. YAML: sweepRows.
	SweepRows int `yaml:"sweepRows"`
	// HarvestTarget bounds the cohort's warm-harvest wall time: half the
	// measured baseline. YAML: harvestTargetMinutes.
	HarvestTarget time.Duration `yaml:"harvestTargetMinutes"`
}

// writeConfigYAML is the on-disk shape: durations as plain integers so a
// config file says milliseconds and minutes, never Go duration strings.
// Pointer fields tell "absent" (keep the caller's value) from an explicit
// zero (invalid for every knob but the derived staged-memory cap).
type writeConfigYAML struct {
	HoldTargetMs       *int   `yaml:"holdTargetMs"`
	ActivationSessions *int   `yaml:"activationSessions"`
	BatchBytes         *int64 `yaml:"batchBytes"`
	BatchSessions      *int   `yaml:"batchSessions"`
	BufferBytes        *int64 `yaml:"bufferBytes"`
	StagedMemoryBytes  *int64 `yaml:"stagedMemoryBytes"`
	FlushIntervalMs    *int   `yaml:"flushIntervalMs"`
	SweepRows          *int   `yaml:"sweepRows"`
	HarvestTargetMins  *int   `yaml:"harvestTargetMinutes"`
}

// UnmarshalYAML decodes the design's write.* keys, converting milliseconds
// and minutes to Durations. Keys the document does not name keep the
// caller's value, so Parse-over-BaseConfig only overrides what the user
// wrote; an explicitly negative value is kept and refused by Validate.
func (w *WriteConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			key := value.Content[i].Value
			switch key {
			case "holdTargetMs", "activationSessions", "batchBytes", "batchSessions", "bufferBytes", "stagedMemoryBytes", "flushIntervalMs", "sweepRows", "harvestTargetMinutes":
			default:
				return fmt.Errorf("ingest: unknown write.%s at config load (line %d); write keys are case-sensitive, so this budget was not applied; use the camelCase write key or remove it", key, value.Content[i].Line)
			}
		}
	}
	var raw writeConfigYAML
	if err := value.Decode(&raw); err != nil {
		return err
	}
	if raw.HoldTargetMs != nil {
		w.HoldTarget = time.Duration(*raw.HoldTargetMs) * time.Millisecond
	}
	if raw.ActivationSessions != nil {
		w.ActivationSessions = *raw.ActivationSessions
	}
	if raw.BatchBytes != nil {
		w.BatchBytes = *raw.BatchBytes
	}
	if raw.BatchSessions != nil {
		w.BatchSessions = *raw.BatchSessions
	}
	if raw.BufferBytes != nil {
		w.BufferBytes = *raw.BufferBytes
	}
	if raw.StagedMemoryBytes != nil {
		w.StagedMemoryBytes = *raw.StagedMemoryBytes
	}
	if raw.FlushIntervalMs != nil {
		w.FlushIntervalMs = *raw.FlushIntervalMs
	}
	if raw.SweepRows != nil {
		w.SweepRows = *raw.SweepRows
	}
	if raw.HarvestTargetMins != nil {
		w.HarvestTarget = time.Duration(*raw.HarvestTargetMins) * time.Minute
	}
	return nil
}

// MarshalYAML emits the design's write.* keys, converting Durations back to
// milliseconds and minutes, so a saved config round-trips through Parse.
func (w WriteConfig) MarshalYAML() (any, error) {
	return writeConfigYAML{
		HoldTargetMs:       intPtr(int(w.HoldTarget / time.Millisecond)),
		ActivationSessions: intPtr(w.ActivationSessions),
		BatchBytes:         int64Ptr(w.BatchBytes),
		BatchSessions:      intPtr(w.BatchSessions),
		BufferBytes:        int64Ptr(w.BufferBytes),
		StagedMemoryBytes:  int64Ptr(w.StagedMemoryBytes),
		FlushIntervalMs:    intPtr(w.FlushIntervalMs),
		SweepRows:          intPtr(w.SweepRows),
		HarvestTargetMins:  intPtr(int(w.HarvestTarget / time.Minute)),
	}, nil
}

func intPtr(v int) *int       { return &v }
func int64Ptr(v int64) *int64 { return &v }

// DefaultWriteConfig returns the shipped write budgets for the given worker
// count: every default from the design's knob table, with the total
// staged-memory cap derived as 2×BatchBytes plus one per-worker buffer each.
// workers <= 0 means one worker.
func DefaultWriteConfig(workers int) WriteConfig {
	if workers < 1 {
		workers = 1
	}
	cfg := WriteConfig{
		HoldTarget:         defaults.WriteDefaultHoldTarget,
		ActivationSessions: defaults.WriteDefaultActivationSessions,
		BatchBytes:         defaults.WriteDefaultBatchBytes,
		BatchSessions:      defaults.WriteDefaultBatchSessions,
		BufferBytes:        defaults.WriteDefaultBufferBytes,
		FlushIntervalMs:    defaults.WriteDefaultFlushIntervalMs,
		SweepRows:          defaults.WriteDefaultSweepRows,
		HarvestTarget:      defaults.WriteDefaultHarvestTarget,
	}
	cfg.StagedMemoryBytes = cfg.derivedStagedMemoryBytes(workers)
	return cfg
}

// derivedStagedMemoryBytes is the structural bound both lanes share: the
// staging and activation lanes' in-flight bounded batches (2×BatchBytes) plus
// the pool's buffers at the effective worker count.
func (w WriteConfig) derivedStagedMemoryBytes(workers int) int64 {
	if workers < 1 {
		workers = 1
	}
	return 2*w.BatchBytes + int64(workers)*w.BufferBytes
}

// WithDefaults resolves the zero value to the shipped budgets: any knob left
// at zero takes its default, and an unset staged-memory cap derives from the
// effective worker count. Explicit values — including invalid ones — are kept
// so Validate refuses them instead of silently repairing them. Config load
// calls it with runtime.NumCPU as the worker estimate; the pipeline
// re-resolves at its own effective worker count through writeConfig.
func (w WriteConfig) WithDefaults(workers int) WriteConfig {
	def := DefaultWriteConfig(workers)
	if w.HoldTarget == 0 {
		w.HoldTarget = def.HoldTarget
	}
	if w.ActivationSessions == 0 {
		w.ActivationSessions = def.ActivationSessions
	}
	if w.BatchBytes == 0 {
		w.BatchBytes = def.BatchBytes
	}
	if w.BatchSessions == 0 {
		w.BatchSessions = def.BatchSessions
	}
	if w.BufferBytes == 0 {
		w.BufferBytes = def.BufferBytes
	}
	if w.StagedMemoryBytes == 0 {
		w.StagedMemoryBytes = w.derivedStagedMemoryBytes(workers)
	}
	if w.FlushIntervalMs == 0 {
		w.FlushIntervalMs = def.FlushIntervalMs
	}
	if w.SweepRows == 0 {
		w.SweepRows = def.SweepRows
	}
	if w.HarvestTarget == 0 {
		w.HarvestTarget = def.HarvestTarget
	}
	return w
}

// validatedWithDefaults fills the unset knobs for the given worker count and
// refuses the result when any bound is unusable. Config load and pipeline
// construction both enter here, so a bad write.* section fails before the
// first session is read. workers <= 0 means one worker.
func (w WriteConfig) validatedWithDefaults(workers int) (WriteConfig, error) {
	resolved := w.WithDefaults(workers)
	if err := resolved.Validate(); err != nil {
		return WriteConfig{}, err
	}
	return resolved, nil
}

// Validate refuses a write configuration whose bounds the write path cannot
// honor. Every error names what failed, why the bound matters, and how to
// fix it.
func (w WriteConfig) Validate() error {
	if w.HoldTarget <= 0 {
		return fmt.Errorf("ingest: write.holdTargetMs must be positive, got %s; without a hold bound a writer-lane transaction can outwait the open path and stall every other process; set holdTargetMs to a positive millisecond count at or below %d (the open path's busy_timeout)",
			w.HoldTarget, int(defaults.SQLiteBusyTimeout/time.Millisecond))
	}
	if w.HoldTarget > defaults.SQLiteBusyTimeout {
		return fmt.Errorf("ingest: write.holdTargetMs is %s but the open path waits at most %s (its busy_timeout) for the single SQLite writer, so a transaction holding longer stalls every other process past its wait; lower holdTargetMs to %d or less",
			w.HoldTarget, defaults.SQLiteBusyTimeout, int(defaults.SQLiteBusyTimeout/time.Millisecond))
	}
	if w.ActivationSessions < 1 {
		return fmt.Errorf("ingest: write.activationSessions must be at least 1, got %d; a batch must admit a session before it can bound anything; set activationSessions to a positive session count", w.ActivationSessions)
	}
	if w.BatchBytes < 1 {
		return fmt.Errorf("ingest: write.batchBytes must be at least 1, got %d; without a byte bound one giant session stages unbounded and the memory bound is fiction; set batchBytes to a positive byte count", w.BatchBytes)
	}
	if w.BatchSessions < 1 {
		return fmt.Errorf("ingest: write.batchSessions must be at least 1, got %d; a batch must admit a session before it can bound anything; set batchSessions to a positive session count", w.BatchSessions)
	}
	if w.BufferBytes < 1 {
		return fmt.Errorf("ingest: write.bufferBytes must be at least 1, got %d; parser workers need bounded scratch headroom; set bufferBytes to a positive byte count", w.BufferBytes)
	}
	minStaged := 2*w.BatchBytes + w.BufferBytes
	if w.StagedMemoryBytes < minStaged {
		return fmt.Errorf("ingest: write.stagedMemoryBytes is %d but the lanes need room for their in-flight bounded batches (2×batchBytes) plus at least one worker buffer, %d bytes at these settings; raise stagedMemoryBytes to %d or more, or leave it unset to derive it from the worker count",
			w.StagedMemoryBytes, minStaged, minStaged)
	}
	if w.FlushIntervalMs < 1 {
		return fmt.Errorf("ingest: write.flushIntervalMs must be at least 1, got %d; without an idle-wait bound a slow input leaves the writer waiting on a batch that never fills; set flushIntervalMs to a positive millisecond count", w.FlushIntervalMs)
	}
	if w.SweepRows < 1 {
		return fmt.Errorf("ingest: write.sweepRows must be at least 1, got %d; a sweep batch must delete a row before it can bound anything; set sweepRows to a positive row count", w.SweepRows)
	}
	if w.HarvestTarget <= 0 {
		return fmt.Errorf("ingest: write.harvestTargetMinutes must be positive, got %s; without a wall-time target the harvest gate asserts against nothing; set harvestTargetMinutes to a positive minute count", w.HarvestTarget)
	}
	return nil
}

// FlushInterval returns the lane's idle-wait fallback as a Duration: a batch
// flushes when it is full, or when this long passes on the lane's idle wait.
func (w WriteConfig) FlushInterval() time.Duration {
	return time.Duration(w.FlushIntervalMs) * time.Millisecond
}

// writeConfig resolves the pipeline's effective write budgets: the configured
// knobs with every unset value defaulted at the effective worker count. It is
// the single entry from the pipeline's config to the write path — the
// splitter, the buffers, and the flush interval all read through here, so no
// budget literal remains downstream of it.
func (p *Pipeline) writeConfig() WriteConfig {
	return p.config.Write.WithDefaults(parallelWorkers(p.config))
}
