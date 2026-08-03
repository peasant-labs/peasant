package perf

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"
)

// PhaseStat is the aggregate timing for one phase across a run.
type PhaseStat struct {
	Phase   Phase
	Count   int           // number of samples
	P50     time.Duration // median (nearest-rank)
	P95     time.Duration // 95th percentile (nearest-rank)
	Total   time.Duration // sum of samples
	Percent float64       // Total as a percentage of the grand total across phases
}

// Rollup is the end-of-run summary: per-phase stats (in a fixed, deterministic
// order) plus connection-reuse counts over the transcript uploads.
type Rollup struct {
	Phases      []PhaseStat
	UploadCount int // transcript uploads that reached a server (Connected)
	ReusedCount int // of those, how many reused a connection
}

// Rollup computes the per-phase summary from the collected samples. Phases with
// no samples are omitted. Percent is each phase's share of the grand total of all
// phase time (0 when nothing was recorded).
func (c *Collector) Rollup() Rollup {
	c.mu.Lock()
	defer c.mu.Unlock()

	var grand time.Duration
	totals := make(map[Phase]time.Duration, len(c.phases))
	for phase, samples := range c.phases {
		var sum time.Duration
		for _, d := range samples {
			sum += d
		}
		totals[phase] = sum
		grand += sum
	}

	var stats []PhaseStat
	for _, phase := range phaseOrder {
		samples := c.phases[phase]
		if len(samples) == 0 {
			continue
		}
		p50, p95 := percentiles(samples)
		pct := 0.0
		if grand > 0 {
			pct = float64(totals[phase]) / float64(grand) * 100
		}
		stats = append(stats, PhaseStat{
			Phase:   phase,
			Count:   len(samples),
			P50:     p50,
			P95:     p95,
			Total:   totals[phase],
			Percent: pct,
		})
	}

	reused := 0
	for _, u := range c.uploads {
		if u.Reused {
			reused++
		}
	}

	return Rollup{Phases: stats, UploadCount: len(c.uploads), ReusedCount: reused}
}

// percentiles returns the nearest-rank p50 and p95 of samples. The input is not
// mutated (it is copied before sorting). An empty input yields (0, 0).
func percentiles(samples []time.Duration) (p50, p95 time.Duration) {
	n := len(samples)
	if n == 0 {
		return 0, 0
	}
	sorted := make([]time.Duration, n)
	copy(sorted, samples)
	slices.Sort(sorted)
	return nearestRank(sorted, 50), nearestRank(sorted, 95)
}

// nearestRank returns the p-th percentile (1..100) of an ALREADY-sorted slice
// using the nearest-rank method: rank = ceil(p/100 * n), 1-indexed, clamped.
func nearestRank(sorted []time.Duration, p int) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	// ceil(p*n/100) without floating point, clamped to [1, n].
	rank := (p*n + 99) / 100
	rank = max(rank, 1)
	rank = min(rank, n)
	return sorted[rank-1]
}

// WriteRollup writes a human-readable per-phase summary to w (the push command
// sends this to stderr). The format is stable and greppable: one header line, one
// line per phase with count / p50 / p95 / total / percent, and a connection-reuse
// line. It is a no-op-safe call: an empty rollup still prints the header and the
// reuse line so the user sees that timing ran.
func WriteRollup(w io.Writer, r Rollup) error {
	if _, err := fmt.Fprintln(w, "push timing — per-phase rollup:"); err != nil {
		return err
	}
	for _, s := range r.Phases {
		if _, err := fmt.Fprintf(w,
			"  %-10s count=%-4d p50=%-8s p95=%-8s total=%-9s %5.1f%%\n",
			s.Phase, s.Count, roundMs(s.P50), roundMs(s.P95), roundMs(s.Total), s.Percent,
		); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w,
		"  uploads=%d reused=%d\n", r.UploadCount, r.ReusedCount,
	); err != nil {
		return err
	}
	return nil
}

// roundMs renders a duration in milliseconds with one decimal place for compact,
// readable rollup output (e.g. "12.3ms").
func roundMs(d time.Duration) string {
	return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
}

// uploadLogLine is the JSON shape of one per-upload log entry. Field names are
// the wire contract for the --timing JSONL: session id + the setup/server split
// (milliseconds) + connection reuse.
type uploadLogLine struct {
	SessionID string  `json:"sessionId"`
	SetupMs   float64 `json:"setupMs"`
	ServerMs  float64 `json:"serverMs"`
	Reused    bool    `json:"reused"`
}

// WriteJSONL writes one JSON object per transcript upload to w, newline-delimited
// (JSONL). Only Connected uploads are present (the Collector drops the rest), so
// every line carries a real setup/server measurement. Lines are emitted in
// arrival order.
func WriteJSONL(w io.Writer, c *Collector) error {
	c.mu.Lock()
	uploads := make([]UploadSample, len(c.uploads))
	copy(uploads, c.uploads)
	c.mu.Unlock()

	enc := json.NewEncoder(w)
	for _, u := range uploads {
		line := uploadLogLine{
			SessionID: u.SessionID,
			SetupMs:   float64(u.Setup.Microseconds()) / 1000,
			ServerMs:  float64(u.Server.Microseconds()) / 1000,
			Reused:    u.Reused,
		}
		if err := enc.Encode(line); err != nil {
			return fmt.Errorf("encode timing line for session %s: %w", u.SessionID, err)
		}
	}
	return nil
}
