package ingest

import "time"

type indexDrainTimer struct {
	c    <-chan time.Time
	stop func()
}

func newIndexDrainTimer(interval time.Duration) indexDrainTimer {
	timer := time.NewTimer(interval)
	return indexDrainTimer{c: timer.C, stop: func() { timer.Stop() }}
}

func drainIndexParseResultsWithGate(ch <-chan indexParseResult, pending []indexParseResult, cfg WriteConfig, flush func([]indexParseResult), blocked <-chan struct{}) {
	drainIndexWithTimer(ch, pending, cfg, flush, blocked, newIndexDrainTimer)
}

// drainIndexWithTimer checks the interval only when no parsed result is ready.
// Busy input fills the batch even if its timer expired during a previous write.
// Byte-admission pressure wakes this wait and flushes the partial batch, so a
// parser never waits for bytes owned by a drain waiting for that parser.
func drainIndexWithTimer(ch <-chan indexParseResult, pending []indexParseResult, cfg WriteConfig, flush func([]indexParseResult), blocked <-chan struct{}, newTimer func(time.Duration) indexDrainTimer) {
	var bytes int64
	var timer indexDrainTimer
	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		timer.stop()
		flush(pending)
		clear(pending)
		pending, bytes = pending[:0], 0
		timer = indexDrainTimer{}
	}
	admit := func(result indexParseResult) {
		n := indexParseWriteBytes(result)
		if ExceedsWriteBudget(cfg, len(pending), bytes, n) {
			flushPending()
		}
		if len(pending) == 0 {
			timer = newTimer(cfg.FlushInterval())
		}
		pending = append(pending, result)
		bytes += n
		if ExceedsWriteBudget(cfg, len(pending), bytes, 0) || bytes >= cfg.BatchBytes {
			flushPending()
		}
	}
	for {
		// Prefer filling input over the fallback timer, deterministically.
		select {
		case result, ok := <-ch:
			if !ok {
				flushPending()
				return
			}
			admit(result)
			continue
		default:
		}
		if len(pending) == 0 {
			result, ok := <-ch
			if !ok {
				return
			}
			admit(result)
			continue
		}
		select {
		case result, ok := <-ch:
			if !ok {
				flushPending()
				return
			}
			admit(result)
		case <-timer.c:
			flushPending()
		case <-blocked:
			flushPending()
		}
	}
}
