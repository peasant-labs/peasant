package ingest

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/peasant-labs/schema"
)

// stagedBytesGate bounds admitted native candidates, not parser scratch. An
// oversized candidate is admitted only when no other candidate owns bytes.
type stagedBytesGate struct {
	mu              sync.Mutex
	cond            *sync.Cond
	cap, used, peak int64
	blocked         chan struct{}
}

func newStagedBytesGate(cap int64) *stagedBytesGate {
	g := &stagedBytesGate{cap: cap, blocked: make(chan struct{}, 1)}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *stagedBytesGate) acquire(ctx context.Context, n int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	stop := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer stop()
	for g.used != 0 && (n > g.cap-g.used) {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The drain must release a partial batch before waiting for a parser
		// whose admission is blocked by that very batch.
		select {
		case g.blocked <- struct{}{}:
		default:
		}
		g.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	g.used += n
	g.peak = max(g.peak, g.used)
	return nil
}

func (g *stagedBytesGate) release(n int64) {
	g.mu.Lock()
	g.used -= n
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *stagedBytesGate) Peak() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

func nativeCandidateWriteBytes(candidate *NativeGenerationCandidate) int64 {
	if candidate == nil {
		return 0
	}
	g := candidate.Result.Generation
	emitted := make(map[schema.SourceEntryRef]bool)
	var bytes int64
	count := func(entries []schema.SessionEntry) {
		for _, entry := range entries {
			body, _ := json.Marshal(entry)
			bytes += int64(len(body))
			emitted[entry.SourceEntryRef] = true
		}
	}
	count(g.Main.Entries)
	for _, earlier := range g.Earlier {
		count(earlier.Content.Entries)
	}
	for ref, blob := range candidate.Blobs {
		if !emitted[ref] {
			bytes += int64(len(blob))
		}
	}
	return bytes
}

func (p *Pipeline) admitNativeResult(ctx context.Context, result indexParseResult, gate *stagedBytesGate) indexParseResult {
	if result.nativeCandidate == nil {
		return result
	}
	n := nativeCandidateWriteBytes(result.nativeCandidate)
	if err := gate.acquire(ctx, n); err != nil {
		_, result.logEntry, _ = p.refuseNativeGeneration(result, result.entryCount, "harvest", err)
		result.output, result.nativeCandidate = nil, nil
		return result
	}
	var once sync.Once
	result.releaseStaged = func() {
		once.Do(func() {
			gate.release(n)
		})
	}
	return result
}
