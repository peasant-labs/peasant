package api

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

// kickstartDroppedLabelsMax is how many vanished choices a scan names. The count
// carries the rest.
const kickstartDroppedLabelsMax = 3

// startScanLocked starts a discovery job for the session, replacing a running
// one. The job reports through the session, so a result that arrives after the
// session closed or a newer job started is dropped.
func (st *kickstartSessions) startScanLocked(sess *kickstartSession) {
	if sess.cancelScan != nil {
		sess.cancelScan()
	}
	sess.scanGen++
	gen := sess.scanGen
	sess.scan = kickstartScanRecord{state: onboarding.ScanRunning, lastPush: st.env.now()}
	ctx, cancel := context.WithCancel(st.env.ctx)
	sess.cancelScan = cancel
	cfg := sess.loaded
	st.env.spawn(func() { st.runScan(ctx, sess, gen, cfg) })
}

// rescan restarts the discovery scan of a session and returns the scan state at
// accept. A scan already running is left to finish: a second request does not
// restart it.
func (st *kickstartSessions) rescan(id string) (schema.LocalKickstartScan, error) {
	var out schema.LocalKickstartScan
	var failure error
	st.mutate(func() []schema.LocalKickstartEvent {
		sess, err := st.liveLocked(id)
		if err != nil {
			failure = err
			return nil
		}
		if sess.committing {
			failure = errKickstartCommitting(id)
			return nil
		}
		var events []schema.LocalKickstartEvent
		if sess.scan.state != onboarding.ScanRunning {
			st.startScanLocked(sess)
			revision := st.bumpLocked(sess)
			scan, err := sess.scan.wire()
			if err != nil {
				failure = err
				return nil
			}
			events = append(events, schema.LocalKickstartEvent{Kind: schema.LocalKickstartEventScan, SessionID: sess.id, Revision: revision, Scan: &scan})
		}
		out, failure = sess.scan.wire()
		return events
	})
	return out, failure
}

// kickstartScanProgress reports a running scan to its session. Phase changes
// are pushed at once; count changes are pushed at most once per interval, so a
// scan of a large library does not flood the topic.
type kickstartScanProgress struct {
	st   *kickstartSessions
	sess *kickstartSession
	gen  uint64
}

var _ discovery.ScanProgress = (*kickstartScanProgress)(nil)

func (p *kickstartScanProgress) SetPhase(phase string) {
	p.tick(true, func(r *kickstartScanRecord) { r.phase, r.done, r.total = phase, 0, 0 })
}

func (p *kickstartScanProgress) SetProgress(done, total int) {
	p.tick(false, func(r *kickstartScanRecord) { r.done, r.total = done, total })
}

func (p *kickstartScanProgress) tick(force bool, update func(*kickstartScanRecord)) {
	p.st.mutate(func() []schema.LocalKickstartEvent {
		sess := p.sess
		if sess.closed || sess.scanGen != p.gen || sess.scan.state != onboarding.ScanRunning {
			return nil
		}
		update(&sess.scan)
		now := p.st.env.now()
		if !force && now.Sub(sess.scan.lastPush) < p.st.env.scanPush {
			return nil
		}
		sess.scan.lastPush = now
		revision := p.st.bumpLocked(sess)
		scan, err := sess.scan.wire()
		if err != nil {
			slog.Error("kickstart: the scan state does not project onto the wire", "error", err)
			return nil
		}
		return []schema.LocalKickstartEvent{{Kind: schema.LocalKickstartEventScan, SessionID: sess.id, Revision: revision, Scan: &scan}}
	})
}

// kickstartScanResult is a finished scan, ready to replace the session's forest.
type kickstartScanResult struct {
	forest    onboarding.Forest
	residual  onboarding.Residual
	selection config.SelectionConfig
	listings  []onboarding.SessionListing
	claude    bool
	note      string
}

func (st *kickstartSessions) runScan(ctx context.Context, sess *kickstartSession, gen uint64, cfg *config.Config) {
	rows, err := st.env.stored(ctx)
	if err != nil {
		st.failScan(ctx, sess, gen, fmt.Errorf("read the sessions the local store already holds: %w", err))
		return
	}
	progress := &kickstartScanProgress{st: st, sess: sess, gen: gen}
	inventory, listings, subagents := st.env.scan(ctx, cfg, discovery.NewStoreKnownSessions(rows), progress)
	if ctx.Err() != nil {
		return
	}
	prepared := discovery.PrepareSessionListingsWith(ctx, listings, st.env.paths, st.env.repositories)
	ingested := make(map[string]bool, len(rows))
	for _, row := range rows {
		ingested[row.SessionID] = true
	}
	forest := discovery.BuildForest(prepared, ingested, subagents)
	baseline, err := onboarding.PrepareBaseline(cfg, listings, rows, st.env.paths)
	if err != nil {
		st.failScan(ctx, sess, gen, fmt.Errorf("prepare the saved selection for this scan: %w", err))
		return
	}
	restored, residual, err := onboarding.RestoreSelection(forest, baseline.Selection)
	if err != nil {
		st.failScan(ctx, sess, gen, fmt.Errorf("restore the saved selection onto the discovered sessions: %w", err))
		return
	}
	if ctx.Err() != nil {
		return
	}
	st.finishScan(sess, gen, kickstartScanResult{
		forest:    restored,
		residual:  residual,
		selection: baseline.Selection,
		listings:  listings,
		claude:    inventory[defaults.HarnessClaudeCode].SessionCount > 0,
		note:      providerNote(inventory),
	})
}

// finishScan installs a finished scan. It prunes the tree answer to the sessions
// the new forest holds, reports what vanished, and never adds a session to an
// answer: a session that appeared since the answer was given is not selected.
// Tabs learn of it twice, in order: a scan event that reports done, on which
// they reload the forest, then a session event that carries the pruned answer.
func (st *kickstartSessions) finishScan(sess *kickstartSession, gen uint64, result kickstartScanResult) {
	st.mutate(func() []schema.LocalKickstartEvent {
		if sess.closed || sess.scanGen != gen {
			return nil
		}
		before := sess.snapshot()
		oldLabels := forestLabels(sess.forest)

		dropped := onboarding.DroppedSelections{}
		var droppedLabels []string
		prunedTree := false
		if ids, answered := kickstartTreeIDs(sess.answers); answered {
			kept, found := onboarding.RescanIDs(ids, result.forest)
			dropped = found
			if found.Count > 0 {
				keptSet := make(map[string]struct{}, len(kept))
				for _, id := range kept {
					keptSet[id] = struct{}{}
				}
				for _, id := range ids {
					if _, ok := keptSet[id]; ok || len(droppedLabels) >= kickstartDroppedLabelsMax {
						continue
					}
					label := oldLabels[id]
					if label == "" {
						label = id
					}
					droppedLabels = append(droppedLabels, label)
				}
				value, err := kickstartTreeAnswer(kept)
				if err != nil {
					return st.failScanLocked(sess, err)
				}
				sess.answers[onboarding.FieldSelection] = value
				prunedTree = true
			}
		}

		sess.forest, sess.residual, sess.preparedSelection = result.forest, result.residual, result.selection
		sess.listings, sess.claudeSessions = result.listings, result.claude
		sess.scan.state = onboarding.ScanDone
		sess.scan.phase = result.note
		sess.scan.locked = onboarding.LockedRows(result.forest)
		sess.scan.droppedCount = dropped.Count
		sess.scan.droppedLabels = droppedLabels
		if sess.cancelScan != nil {
			sess.cancelScan()
			sess.cancelScan = nil
		}

		scanRevision := st.bumpLocked(sess)
		scan, err := sess.scan.wire()
		if err != nil {
			sess.restore(before)
			return st.failScanLocked(sess, err)
		}
		events := []schema.LocalKickstartEvent{{Kind: schema.LocalKickstartEventScan, SessionID: sess.id, Revision: scanRevision, Scan: &scan}}

		revision := st.bumpLocked(sess)
		if prunedTree {
			sess.fieldClock[onboarding.FieldSelection.String()] = revision
		}
		if err := st.rebuildLocked(sess); err != nil {
			sess.restore(before)
			return st.failScanLocked(sess, fmt.Errorf("rebuild the draft over the new forest: %w", err))
		}
		view, err := st.viewLocked(sess)
		if err != nil {
			sess.restore(before)
			return st.failScanLocked(sess, err)
		}
		return append(events, schema.LocalKickstartEvent{Kind: schema.LocalKickstartEventSession, SessionID: sess.id, Revision: revision, View: &view})
	})
}

// failScan records a scan that could not finish. The forest and the answers the
// session already held are kept.
func (st *kickstartSessions) failScan(ctx context.Context, sess *kickstartSession, gen uint64, cause error) {
	if ctx.Err() != nil {
		return
	}
	st.mutate(func() []schema.LocalKickstartEvent {
		if sess.closed || sess.scanGen != gen {
			return nil
		}
		return st.failScanLocked(sess, cause)
	})
}

func (st *kickstartSessions) failScanLocked(sess *kickstartSession, cause error) []schema.LocalKickstartEvent {
	slog.Warn("kickstart: the discovery scan failed", "session", sess.id, "error", cause)
	sess.scan.state = onboarding.ScanError
	sess.scan.err = fmt.Sprintf("The discovery scan could not finish: %v. Your answers and the sessions already listed were kept. Rescan, or check that the transcript folders in config.yaml are readable.", cause)
	if sess.cancelScan != nil {
		sess.cancelScan()
		sess.cancelScan = nil
	}
	revision := st.bumpLocked(sess)
	scan, err := sess.scan.wire()
	if err != nil {
		slog.Error("kickstart: the scan state does not project onto the wire", "error", err)
		return nil
	}
	return []schema.LocalKickstartEvent{{Kind: schema.LocalKickstartEventScan, SessionID: sess.id, Revision: revision, Scan: &scan}}
}

func forestLabels(forest onboarding.Forest) map[string]string {
	labels := make(map[string]string, len(forest.Nodes))
	for _, node := range forest.Nodes {
		labels[node.ID] = node.Label
	}
	return labels
}

// providerNote says which harnesses discovery could not read, so a scan that
// listed fewer sessions than expected explains itself. It is empty when every
// configured harness was inspected.
func providerNote(inventory discovery.ProviderInventory) string {
	harnesses := make([]string, 0, len(inventory))
	for harness, found := range inventory {
		if !found.State.IsOperational() {
			harnesses = append(harnesses, string(harness))
		}
	}
	sort.Strings(harnesses)
	notes := make([]string, 0, len(harnesses))
	for _, name := range harnesses {
		found := inventory[defaults.Harness(name)]
		detail := found.Detail
		if detail == "" {
			detail = string(found.State)
		}
		notes = append(notes, fmt.Sprintf("%s could not be read: %s", schema.HarnessDisplayName(defaults.Harness(name)), detail))
	}
	return strings.Join(notes, "; ")
}
