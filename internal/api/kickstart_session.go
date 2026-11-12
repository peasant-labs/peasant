package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/claudesettings"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// A kickstart session is one live setup draft for one scope. The server keeps
// at most one per scope, in memory, and every tab of that scope shares it.
//
// This file holds the session and the store that owns the sessions. The other
// kickstart files project a session onto the wire (kickstart_view.go,
// kickstart_tree.go, kickstart_preview.go), run its discovery scan
// (kickstart_scan.go), publish it on the kickstart topic (kickstart_topic.go),
// and answer HTTP for it (kickstart_handler.go).
//
// # What a commit reads and does
//
// The commit owner builds on these methods of kickstartSessions. Each one takes
// the session id the commit route received.
//
//	beginCommit(id) (kickstartDraft, error)
//	    Freezes the session for one commit and returns what the commit reads:
//	    the model, the answers a configuration write applies (ConfigAnswers),
//	    the resolutions with the file value each saw, the draft, and a write
//	    context. The draft's baseline is the configuration file parsed as it was
//	    when the session opened; a legacy selection conversion lives in the
//	    working copy only, so the commit's comparison against the file is not
//	    skewed by it. While a commit holds the session, an answer or a rescan is
//	    refused, and a second beginCommit is refused with its reason.
//	abortCommit(id)
//	    Releases the session after a commit that wrote nothing, such as a 409.
//	publishConflicts(id, rows) (revision, error)
//	    Replaces the session's conflict rows, bumps the revision, and publishes
//	    a conflicts event that carries the full view.
//	closeCommitted(id, body) (*kickstartFinish, error)
//	    Ends the session with reason committed and publishes the closed event
//	    that carries body. The returned value publishes the ingest events that
//	    follow, after the session itself is gone.
//	finishFor(id) *kickstartFinish
//	    The same publisher, found again by session id. An import the page starts
//	    later with "import now" publishes through it, so its pushed ingest events
//	    reach every tab that still holds the finished session's id.
//
// A tab learns of every accepted change from the kickstart topic. A change
// bumps the session revision once and is published once.

// kickstartConnectKey names the boundary connect offer in a field clock and in an
// overwritten entry. It is not a configuration field.
const kickstartConnectKey = "connect"

// defaultKickstartIdle is how long a session may go without an accepted change
// before it is dropped.
const defaultKickstartIdle = 30 * time.Minute

// defaultKickstartScanPush is the shortest interval between two pushed scan
// progress ticks. A phase change and the end of a scan are always pushed.
const defaultKickstartScanPush = 200 * time.Millisecond

// kickstartError is a refusal with the contract's code it is answered with. The
// HTTP status is the code's own (schema.LocalKickstartErrorCode.HTTPStatus), so
// no refusal states a status beside its code.
type kickstartError struct {
	code    schema.LocalKickstartErrorCode
	message string
}

func (e *kickstartError) Error() string { return e.message }

func newKickstartError(code schema.LocalKickstartErrorCode, format string, args ...any) *kickstartError {
	return &kickstartError{code: code, message: fmt.Sprintf(format, args...)}
}

func errKickstartNoSession(id string) *kickstartError {
	return newKickstartError(schema.LocalKickstartErrorSessionNotFound,
		"No setup session has the id %q in internal/api.kickstartSessions. It was never opened, it was saved, or it expired after %s without a change, and a server restart drops every session. Nothing was changed. Open a session again with POST %s.",
		id, defaultKickstartIdle, defaults.RouteKickstartSessions)
}

func errKickstartCommitting(id string) *kickstartError {
	return newKickstartError(schema.LocalKickstartErrorSessionCommitting,
		"Setup session %q is being saved, so this request was not applied in internal/api.kickstartSessions. Nothing was changed. Wait for the save to finish, then reload the session.", id)
}

// kickstartFacts are the facts a view reads fresh each time, because they change
// without a request to the session.
type kickstartFacts struct {
	villageConnected bool
	villageURL       string
	villageUserID    string
}

// kickstartEnv is everything a session store needs from the process around it.
// Production wires the real server; a test replaces any member.
type kickstartEnv struct {
	// ctx bounds every scan job. Cancelling it stops the jobs.
	ctx context.Context
	// now is the clock the session, the idle timer, and the scan throttle read.
	now func() time.Time
	// newID mints a session id.
	newID func() string
	// afterFunc runs f once after d and returns the function that stops it.
	afterFunc func(d time.Duration, f func()) (stop func() bool)
	// idleAfter is how long a session may go without an accepted change.
	idleAfter time.Duration
	// scanPush is the shortest interval between two pushed scan progress ticks.
	scanPush time.Duration
	// emit publishes one event on the kickstart topic. Nil publishes nothing.
	emit func(schema.LocalKickstartEvent)
	// spawn runs a scan job on a tracked goroutine.
	spawn func(fn func())
	// configPath is the file a session starts from and a commit writes.
	configPath string
	// loadConfig reads the configuration file as the terminal draft does, or the
	// defaults when it is absent. Its result is the draft's baseline: a legacy
	// conversion never edits it.
	loadConfig func(ctx context.Context) (*config.Config, error)
	// village reads the sign-in fact.
	village func() kickstartFacts
	// retentionSeed is the Claude retention value the retention stop starts on.
	retentionSeed func() int
	// stored reads the sessions the local store already holds.
	stored func(ctx context.Context) ([]store.IngestedSessionRow, error)
	// scan runs discovery. known may be nil.
	scan func(ctx context.Context, cfg *config.Config, known ingest.KnownSessionIndex, progress discovery.ScanProgress) (discovery.ProviderInventory, []onboarding.SessionListing, discovery.SubagentRelation)
	// paths and repositories resolve a working directory to its clone and its
	// repository for the forest.
	paths        ingest.PathIdentityResolver
	repositories ingest.RepositoryIdentityResolver
	// reader reads a session's first transcript page for a preview.
	reader func(listings []onboarding.SessionListing) kickstartPreviewReader
	// writeRetention writes Claude Code's cleanupPeriodDays after a save. Nil
	// writes the real Claude settings file.
	writeRetention func(days int) error
	// lockWait bounds how long a save waits for the configuration lock. Zero
	// waits defaults.ConfigLockWait.
	lockWait time.Duration
	// review previews a save of a draft. It reads the configuration file, which
	// is why it is never run under the store lock; a test holds it to prove that.
	review func(ctx context.Context, draft *onboarding.Draft, input onboarding.ReviewInput) (onboarding.ReviewResult, error)
	// nextSteps is the finish menu a save answers with. The setup model owns it;
	// a test replaces it to reach a menu the contract refuses.
	nextSteps func() []onboarding.NextStep
}

func (e kickstartEnv) withDefaults() kickstartEnv {
	if e.ctx == nil {
		e.ctx = context.Background()
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.newID == nil {
		e.newID = newKickstartSessionID
	}
	if e.afterFunc == nil {
		e.afterFunc = func(d time.Duration, f func()) func() bool {
			timer := time.AfterFunc(d, f)
			return timer.Stop
		}
	}
	if e.idleAfter <= 0 {
		e.idleAfter = defaultKickstartIdle
	}
	if e.scanPush <= 0 {
		e.scanPush = defaultKickstartScanPush
	}
	if e.spawn == nil {
		e.spawn = func(fn func()) { go fn() }
	}
	if e.village == nil {
		e.village = func() kickstartFacts { return kickstartFacts{} }
	}
	if e.retentionSeed == nil {
		e.retentionSeed = func() int { return onboarding.RecommendedRetentionDays }
	}
	if e.stored == nil {
		e.stored = func(context.Context) ([]store.IngestedSessionRow, error) { return nil, nil }
	}
	if e.paths == nil {
		e.paths = ingest.NewPhysicalPathResolver()
	}
	if e.scan == nil {
		e.scan = func(ctx context.Context, cfg *config.Config, known ingest.KnownSessionIndex, progress discovery.ScanProgress) (discovery.ProviderInventory, []onboarding.SessionListing, discovery.SubagentRelation) {
			return discovery.Scan(ctx, cfg, &ingest.OSFileSystem{}, &ingest.ExecGitResolver{}, known, nil, progress)
		}
	}
	if e.writeRetention == nil {
		e.writeRetention = claudesettings.WriteCleanupDays
	}
	if e.lockWait <= 0 {
		e.lockWait = defaults.ConfigLockWait
	}
	if e.review == nil {
		e.review = func(ctx context.Context, draft *onboarding.Draft, input onboarding.ReviewInput) (onboarding.ReviewResult, error) {
			return draft.Review(ctx, input)
		}
	}
	if e.nextSteps == nil {
		e.nextSteps = onboarding.NextStepsWeb
	}
	if e.reader == nil {
		e.reader = func(listings []onboarding.SessionListing) kickstartPreviewReader {
			return discovery.NewSourceReader(&ingest.OSFileSystem{}, &ingest.ExecGitResolver{}, listings)
		}
	}
	return e
}

func newKickstartSessionID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// The operating system's entropy source failed; a session id must not be
		// guessable, so there is no weaker fallback.
		panic(fmt.Errorf("mint a setup session id in internal/api.newKickstartSessionID: %w; no session was opened; check the operating system's random source", err))
	}
	return hex.EncodeToString(raw[:])
}

// kickstartScanRecord is the discovery job as the session last saw it.
type kickstartScanRecord struct {
	state         onboarding.ScanState
	phase         string
	notice        string
	done, total   int
	err           string
	locked        int
	droppedCount  int
	droppedLabels []string
	lastPush      time.Time
}

// kickstartSession is one live setup draft. Every field is guarded by the
// store's mutex.
type kickstartSession struct {
	id       string
	scope    onboarding.ModelScope
	model    onboarding.Model
	openedAt time.Time

	// revision is the session clock. Every accepted change bumps it once.
	revision uint64
	// fieldClock records the revision at which each answered field, or the
	// connect offer, last changed. It answers the one question a stale write
	// needs: was this field changed after the caller last looked?
	fieldClock map[string]uint64

	// loaded is the configuration file as read when the session opened.
	loaded *config.Config
	// preparedSelection is the saved selection after the legacy conversion, which
	// the working copy starts from and the tree is restored from.
	preparedSelection config.SelectionConfig
	retentionSeed     int

	// answers holds every answer by field. A tree answer is {"ids": [...]}, the
	// session ids it selects.
	answers     onboarding.Answers
	connect     onboarding.ConnectAnswer
	resolutions onboarding.Resolutions
	conflicts   []schema.LocalKickstartConflict

	// forest is the last finished scan restored from preparedSelection. tree is
	// the forest read under the answer.
	forest         onboarding.Forest
	residual       onboarding.Residual
	tree           onboarding.TreeAnswer
	listings       []onboarding.SessionListing
	claudeSessions bool

	working *config.Config

	scan       kickstartScanRecord
	scanGen    uint64
	cancelScan context.CancelFunc

	committing bool
	closed     bool
	idleGen    uint64
	stopIdle   func() bool
}

// kickstartSessions is the store of live sessions, one per scope.
type kickstartSessions struct {
	env kickstartEnv

	// mu guards every session and is held only for work on memory. line orders
	// publication: a mutation joins it while it holds mu, so events leave in the
	// order the changes were made, while the slow part of a view (the file read,
	// the redactor) and the hub are never reached with mu held.
	mu      sync.Mutex
	line    publishLine
	byScope map[onboarding.ModelScope]*kickstartSession
	byID    map[string]*kickstartSession
	// finished keeps the publishers of sessions a commit ended, so an import
	// started after the session is gone still publishes under its id.
	finished      map[string]*kickstartFinish
	finishedOrder []string
}

// kickstartFinishedKept bounds how many ended sessions keep a publisher.
const kickstartFinishedKept = 32

func newKickstartSessions(env kickstartEnv) *kickstartSessions {
	return &kickstartSessions{
		env:      env.withDefaults(),
		byScope:  map[onboarding.ModelScope]*kickstartSession{},
		byID:     map[string]*kickstartSession{},
		finished: map[string]*kickstartFinish{},
	}
}

// publishLine puts publication in the order the changes were made without
// holding the store lock while an earlier change finishes its view.
type publishLine struct {
	mu   sync.Mutex
	last chan struct{}
}

// join takes the next place in the line. wait is closed once everything before
// this place has been published (nil for the first place); the caller closes
// done when its own publication is out.
func (l *publishLine) join() (wait <-chan struct{}, done chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	wait, done = l.last, make(chan struct{})
	l.last = done
	return wait, done
}

// mutate runs fn under the store lock and publishes the events it returns, in
// order, after the lock is released.
func (st *kickstartSessions) mutate(fn func() []schema.LocalKickstartEvent) {
	st.mutateViews(func() ([]schema.LocalKickstartEvent, []*kickstartViewDraft) { return fn(), nil })
}

// mutateViews runs fn under the store lock. fn changes the session and returns
// the events the change publishes, and the drafts of the views those events (or
// the caller) carry. Everything fn can refuse happens under the lock, so a
// refused change is rolled back there. The slow parts of each draft are then
// added after the lock is released, and the events are published in the order
// the changes were made.
func (st *kickstartSessions) mutateViews(fn func() ([]schema.LocalKickstartEvent, []*kickstartViewDraft)) {
	st.mu.Lock()
	events, drafts := fn()
	if len(events) == 0 && len(drafts) == 0 {
		st.mu.Unlock()
		return
	}
	wait, done := st.line.join()
	st.mu.Unlock()
	defer close(done)
	for _, draft := range drafts {
		st.completeView(draft)
	}
	if wait != nil {
		<-wait
	}
	for _, event := range events {
		st.publish(event)
	}
}

// publish sends one event on the topic after checking it against the contract.
// An event that breaks the contract is logged and dropped, so a defect here
// never reaches a tab as a payload the client cannot parse.
func (st *kickstartSessions) publish(event schema.LocalKickstartEvent) {
	if st.env.emit == nil {
		return
	}
	if err := event.Validate(); err != nil {
		slog.Error("kickstart: an event breaks the Local API contract and was not published", "kind", event.Kind, "session", event.SessionID, "error", err)
		return
	}
	st.env.emit(event)
}

// shutdown stops every scan job and idle timer. The sessions stay readable.
func (st *kickstartSessions) shutdown() {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, sess := range st.byID {
		if sess.cancelScan != nil {
			sess.cancelScan()
		}
		if sess.stopIdle != nil {
			sess.stopIdle()
			sess.idleGen++
		}
	}
}

func (st *kickstartSessions) liveLocked(id string) (*kickstartSession, error) {
	sess, ok := st.byID[id]
	if !ok || sess.closed {
		return nil, errKickstartNoSession(id)
	}
	return sess, nil
}

// touchLocked restarts the idle timer. Only an accepted change calls it.
func (st *kickstartSessions) touchLocked(sess *kickstartSession) {
	if sess.stopIdle != nil {
		sess.stopIdle()
	}
	sess.idleGen++
	gen := sess.idleGen
	sess.stopIdle = st.env.afterFunc(st.env.idleAfter, func() { st.expire(sess.id, gen) })
}

// bumpLocked starts a new revision for one accepted change.
func (st *kickstartSessions) bumpLocked(sess *kickstartSession) uint64 {
	sess.revision++
	st.touchLocked(sess)
	return sess.revision
}

// expire drops a session that went idle. A timer that fires for an older
// generation, or for a session a commit holds, does nothing.
func (st *kickstartSessions) expire(id string, gen uint64) {
	st.mutate(func() []schema.LocalKickstartEvent {
		sess, ok := st.byID[id]
		if !ok || sess.closed {
			return nil
		}
		if sess.idleGen != gen || sess.committing {
			return nil
		}
		return []schema.LocalKickstartEvent{st.closeLocked(sess, schema.LocalKickstartClosedExpired, nil)}
	})
}

// closeLocked removes a session and returns its closed event.
func (st *kickstartSessions) closeLocked(sess *kickstartSession, reason schema.LocalKickstartCloseReason, committed *schema.LocalKickstartCommitted) schema.LocalKickstartEvent {
	sess.closed = true
	if sess.cancelScan != nil {
		sess.cancelScan()
		sess.cancelScan = nil
	}
	if sess.stopIdle != nil {
		sess.stopIdle()
		sess.stopIdle = nil
	}
	sess.idleGen++
	if st.byScope[sess.scope] == sess {
		delete(st.byScope, sess.scope)
	}
	delete(st.byID, sess.id)
	sess.revision++
	return schema.LocalKickstartEvent{
		Kind:      schema.LocalKickstartEventClosed,
		SessionID: sess.id,
		Revision:  sess.revision,
		Reason:    reason,
		Committed: committed,
	}
}

// open returns the live session for scope, or opens one. The same scope returns
// the same session; the other scope's session is never touched. The
// configuration file a new session starts from is read before the store lock is
// taken.
func (st *kickstartSessions) open(ctx context.Context, scope onboarding.ModelScope) (schema.LocalKickstartSession, error) {
	if !scope.IsValid() {
		return schema.LocalKickstartSession{}, errKickstartScopeInvalid(scope)
	}
	facts := st.env.village()
	var loaded *config.Config
	for {
		var draft *kickstartViewDraft
		var failure error
		needsFile := false
		st.mutateViews(func() ([]schema.LocalKickstartEvent, []*kickstartViewDraft) {
			sess, ok := st.byScope[scope]
			if !ok {
				if loaded == nil {
					needsFile = true
					return nil, nil
				}
				var err error
				if sess, err = st.createLocked(scope, loaded, facts); err != nil {
					failure = err
					return nil, nil
				}
			}
			if draft, failure = st.draftView(sess.frozen(), facts); failure != nil {
				return nil, nil
			}
			return nil, []*kickstartViewDraft{draft}
		})
		switch {
		case needsFile:
			var err error
			if loaded, err = st.env.loadConfig(ctx); err != nil {
				return schema.LocalKickstartSession{}, newKickstartError(schema.LocalKickstartErrorConfigUnreadable,
					"The setup session could not start because the configuration file %s could not be read in internal/api.kickstartSessions.open: %v. No session was opened and the file was not changed. Fix or restore the file, then open the session again.", st.env.configPath, err)
			}
		case failure != nil:
			return schema.LocalKickstartSession{}, failure
		default:
			return draft.view, nil
		}
	}
}

func errKickstartScopeInvalid(scope onboarding.ModelScope) *kickstartError {
	return newKickstartError(schema.LocalKickstartErrorScopeInvalid,
		"The setup scope %q is not full or selection in internal/api.kickstartSessions.open. No session was opened. Send full or selection.", scope)
}

// createLocked opens a session for scope over the configuration file as loaded.
func (st *kickstartSessions) createLocked(scope onboarding.ModelScope, loaded *config.Config, facts kickstartFacts) (*kickstartSession, error) {
	sess := &kickstartSession{
		id:                st.env.newID(),
		scope:             scope,
		model:             onboarding.BuildModel(onboarding.Options{Scope: scope}),
		openedAt:          st.env.now(),
		revision:          1,
		fieldClock:        map[string]uint64{},
		loaded:            loaded,
		preparedSelection: loaded.Selection,
		retentionSeed:     st.env.retentionSeed(),
		answers:           onboarding.Answers{},
		resolutions:       onboarding.Resolutions{},
		conflicts:         []schema.LocalKickstartConflict{},
	}
	st.byScope[scope] = sess
	st.byID[sess.id] = sess
	if err := st.rebuildLocked(sess, facts); err != nil {
		delete(st.byScope, scope)
		delete(st.byID, sess.id)
		return nil, newKickstartError(schema.LocalKickstartErrorSessionUnbuildable,
			"The setup session could not start because its first draft could not be built in internal/api.kickstartSessions.open: %v. No session was opened. Report this defect.", err)
	}
	st.touchLocked(sess)
	st.startScanLocked(sess)
	return sess, nil
}

// get returns the reconnect snapshot of a session. The view reads the
// configuration file and runs the redactor, so it is built from a frozen copy
// after the store lock is released: a tab that reconnects never holds up a scan
// tick or an answer from another tab.
func (st *kickstartSessions) get(id string) (schema.LocalKickstartSession, error) {
	st.mu.Lock()
	sess, err := st.liveLocked(id)
	var frozen *kickstartSession
	if err == nil {
		frozen = sess.frozen()
	}
	st.mu.Unlock()
	if err != nil {
		return schema.LocalKickstartSession{}, err
	}
	return st.viewOf(frozen)
}

// activeFullSessionID is the id of the live full-scope session, or empty.
func (st *kickstartSessions) activeFullSessionID() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if sess, ok := st.byScope[onboarding.ScopeFull]; ok && !sess.closed {
		return sess.id
	}
	return ""
}

// configAnswers is the part of the answers a configuration write applies. An
// action answer is a choice made in the session, and a notice has none.
func (s *kickstartSession) configAnswers() onboarding.Answers {
	out := onboarding.Answers{}
	for _, field := range s.model.Fields() {
		value, ok := s.answers[field.Key()]
		if !ok {
			continue
		}
		switch field.Kind() {
		case onboarding.FieldKindToggle, onboarding.FieldKindChoice, onboarding.FieldKindTree:
			out[field.Key()] = append(schema.LocalSettingValue(nil), value...)
		}
	}
	return out
}

// freshWorking returns a new working copy: the file as read when the session
// opened, the transient retention seed, and the prepared selection. Nothing is
// shared with an earlier copy, so a write to it cannot reach the session.
func (s *kickstartSession) freshWorking(configPath string) (*config.Config, *onboarding.Draft, error) {
	draft, err := onboarding.OpenDraft(configPath, s.loaded)
	if err != nil {
		return nil, nil, err
	}
	days := s.retentionSeed
	if err := draft.SeedTransient(func(cfg *config.Config) { cfg.ClaudeRetentionDays = days }); err != nil {
		return nil, nil, err
	}
	working := draft.Working()
	working.Selection = s.preparedSelection
	return working, draft, nil
}

// writeContext assembles the facts a field, a condition, or a validator reads.
func (s *kickstartSession) writeContext(cfg *config.Config, answers onboarding.Answers, facts kickstartFacts) onboarding.WriteContext {
	var scanErr error
	if s.scan.err != "" {
		scanErr = errors.New(s.scan.err)
	}
	return onboarding.WriteContext{
		Config:           cfg,
		Answers:          answers,
		Forest:           s.forest,
		Residual:         s.residual,
		VillageConnected: facts.villageConnected,
		LoginAvailable:   true,
		ConnectAnswer:    s.connect,
		ClaudeSessions:   s.claudeSessions,
		ScanState:        s.scan.state,
		ScanErr:          scanErr,
		LockedNodes:      s.scan.locked,
	}
}

// rebuildLocked recomputes what the answers imply: the forest under the tree
// answer, the answers that a later answer hid, and the working copy. A hidden
// answer is dropped and its clock moves to the current revision.
func (st *kickstartSessions) rebuildLocked(sess *kickstartSession, facts kickstartFacts) error {
	if ids, answered := onboarding.SelectionIDs(sess.answers[onboarding.FieldSelection]); answered {
		sess.tree = onboarding.ApplyTreeAnswer(sess.forest, ids)
	} else {
		sess.tree = onboarding.TreeAnswer{Forest: sess.forest, SessionIDs: onboarding.SelectedSessionIDs(sess.forest)}
	}
	start, _, err := sess.freshWorking(st.env.configPath)
	if err != nil {
		return err
	}
	configAnswers := sess.configAnswers()
	kept, err := sess.model.DropHidden(start, configAnswers, sess.writeContext(start, configAnswers, facts))
	if err != nil {
		return err
	}
	for key := range configAnswers {
		if _, ok := kept[key]; !ok {
			delete(sess.answers, key)
			delete(sess.resolutions, key)
			sess.fieldClock[key.String()] = sess.revision
		}
	}
	working, _, err := sess.freshWorking(st.env.configPath)
	if err != nil {
		return err
	}
	if err := sess.model.ApplyAnswers(working, kept, sess.writeContext(working, kept, facts)); err != nil {
		return err
	}
	sess.working = working
	return nil
}

// kickstartMutableState is what an answer can change, kept so a refused change
// leaves the session exactly as it was.
type kickstartMutableState struct {
	revision    uint64
	fieldClock  map[string]uint64
	answers     onboarding.Answers
	connect     onboarding.ConnectAnswer
	resolutions onboarding.Resolutions
	conflicts   []schema.LocalKickstartConflict
	forest      onboarding.Forest
	residual    onboarding.Residual
	prepared    config.SelectionConfig
	listings    []onboarding.SessionListing
	claude      bool
	scan        kickstartScanRecord
	tree        onboarding.TreeAnswer
	working     *config.Config
}

func (s *kickstartSession) snapshot() kickstartMutableState {
	clocks := make(map[string]uint64, len(s.fieldClock))
	for key, rev := range s.fieldClock {
		clocks[key] = rev
	}
	resolutions := make(onboarding.Resolutions, len(s.resolutions))
	for key, resolution := range s.resolutions {
		resolutions[key] = onboarding.Resolution{Choice: resolution.Choice, Seen: append(schema.LocalSettingValue(nil), resolution.Seen...)}
	}
	answers := onboarding.Answers{}
	for key, value := range s.answers {
		answers[key] = append(schema.LocalSettingValue(nil), value...)
	}
	scan := s.scan
	scan.droppedLabels = append([]string(nil), s.scan.droppedLabels...)
	return kickstartMutableState{
		revision: s.revision, fieldClock: clocks, answers: answers, connect: s.connect,
		resolutions: resolutions, conflicts: append([]schema.LocalKickstartConflict{}, s.conflicts...),
		forest: s.forest, residual: s.residual, prepared: s.preparedSelection,
		listings: s.listings, claude: s.claudeSessions, scan: scan, tree: s.tree, working: s.working,
	}
}

// frozen returns a copy of the session that a view can be built from after the
// store lock is released. What a view reads and a later change could edit in
// place is copied; the rest is replaced whole by a change, never edited, so the
// copy and the session share it.
func (s *kickstartSession) frozen() *kickstartSession {
	state := s.snapshot()
	copied := *s
	copied.fieldClock, copied.answers, copied.resolutions = state.fieldClock, state.answers, state.resolutions
	copied.conflicts, copied.scan = state.conflicts, state.scan
	return &copied
}

func (s *kickstartSession) restore(state kickstartMutableState) {
	s.revision, s.fieldClock, s.answers, s.connect = state.revision, state.fieldClock, state.answers, state.connect
	s.resolutions, s.conflicts = state.resolutions, state.conflicts
	s.forest, s.residual, s.preparedSelection = state.forest, state.residual, state.prepared
	s.listings, s.claudeSessions, s.scan, s.tree, s.working = state.listings, state.claude, state.scan, state.tree, state.working
}

// patch applies one answer request. A request at an older revision is applied
// and reported, never refused: last edit wins, and the writer is told which
// fields it replaced.
//
// A choice on a conflict row records the file value the user saw. That value
// comes from a review of the file, which is read from a copy of the session
// before the store lock is taken and used only if the session has not changed
// since; a session that keeps changing is reviewed under the lock instead.
func (st *kickstartSessions) patch(id string, request schema.LocalKickstartAnswers) (schema.LocalKickstartPatchResult, error) {
	facts := st.env.village()
	var seen *kickstartSeenRows
	for attempt := 0; ; attempt++ {
		var result schema.LocalKickstartPatchResult
		var failure error
		var toReview *kickstartSession
		st.mutateViews(func() ([]schema.LocalKickstartEvent, []*kickstartViewDraft) {
			sess, err := st.liveLocked(id)
			if err != nil {
				failure = err
				return nil, nil
			}
			current := seen
			if !seen.reviews(sess) {
				current = nil
				if len(request.Resolutions) > 0 && attempt < kickstartReviewAttempts {
					toReview = sess.frozen()
					return nil, nil
				}
			}
			var events []schema.LocalKickstartEvent
			var drafts []*kickstartViewDraft
			result, events, drafts, failure = st.patchLocked(sess, request, current, facts)
			return events, drafts
		})
		if toReview == nil {
			return result, failure
		}
		seen = st.seenRowsOf(toReview, facts)
	}
}

// kickstartReviewAttempts is how many times a request that resolves conflict
// rows reviews the file outside the lock before it reviews under the lock.
const kickstartReviewAttempts = 3

// kickstartSeenRows are the conflict rows a review of a session found, taken at
// one revision of it. They say what file value a row shows.
type kickstartSeenRows struct {
	revision uint64
	rows     []schema.LocalKickstartConflict
}

// reviews reports whether the rows were taken from this session as it is now.
func (r *kickstartSeenRows) reviews(sess *kickstartSession) bool {
	return r != nil && r.revision == sess.revision
}

func (st *kickstartSessions) seenRowsOf(frozen *kickstartSession, facts kickstartFacts) *kickstartSeenRows {
	seen := &kickstartSeenRows{revision: frozen.revision}
	if review, ok := st.reviewOf(frozen, facts); ok {
		seen.rows = review.Conflicts
	}
	return seen
}

type kickstartPlan struct {
	answers     map[onboarding.FieldKey]schema.LocalSettingValue
	connect     *onboarding.ConnectAnswer
	resolutions map[onboarding.FieldKey]schema.LocalKickstartResolution
}

// patchLocked applies one request to a live session. seen, when it is not nil,
// holds the conflict rows of a review of the session at its current revision.
// It returns the result, the events to publish, and the drafts of the views they
// carry; the drafts are completed after the lock is released.
func (st *kickstartSessions) patchLocked(sess *kickstartSession, request schema.LocalKickstartAnswers, seen *kickstartSeenRows, facts kickstartFacts) (schema.LocalKickstartPatchResult, []schema.LocalKickstartEvent, []*kickstartViewDraft, error) {
	if sess.committing {
		return schema.LocalKickstartPatchResult{}, nil, nil, errKickstartCommitting(sess.id)
	}
	if request.Revision > sess.revision {
		return schema.LocalKickstartPatchResult{}, nil, nil, newKickstartError(schema.LocalKickstartErrorRevisionAhead,
			"The answer was sent at revision %d, which is ahead of setup session %q at revision %d, in internal/api.kickstartSessions.patch. Nothing was changed. Send the revision of the last view you rendered.", request.Revision, sess.id, sess.revision)
	}
	stale := request.Revision < sess.revision
	plan, err := st.planLocked(sess, request, facts)
	if err != nil {
		return schema.LocalKickstartPatchResult{}, nil, nil, err
	}

	changed := st.changedKeysLocked(sess, plan, seen, facts)
	if len(changed) == 0 {
		draft, err := st.draftView(sess.frozen(), facts)
		if err != nil {
			return schema.LocalKickstartPatchResult{}, nil, nil, err
		}
		result := schema.LocalKickstartPatchResult{Revision: sess.revision, Stale: stale, View: &draft.view}
		if stale {
			result.Overwritten = []schema.LocalKickstartOverwrite{}
		}
		return result, nil, []*kickstartViewDraft{draft}, nil
	}

	var overwritten []schema.LocalKickstartOverwrite
	if stale {
		overwritten = st.overwrittenLocked(sess, changed, request.Revision)
	}
	conflictsBefore := len(sess.conflicts)
	before := sess.snapshot()
	// The revision moves now, so the rebuild stamps dropped answers with it; the
	// idle timer restarts only once the change is accepted, below.
	sess.revision++
	revision := sess.revision
	for key, value := range plan.answers {
		sess.answers[key] = value
		delete(sess.resolutions, key)
	}
	if plan.connect != nil {
		sess.connect = *plan.connect
	}
	for key, choice := range plan.resolutions {
		if st.coupledRowLocked(sess, key, seen, facts) {
			// The draft never answered this field: another answer moves it. Both
			// sides are recorded with the file value the user saw, and the save
			// keeps the file's value for "theirs" instead of giving an answer up.
			sess.resolutions[key] = onboarding.Resolution{Choice: choice, Seen: st.servedConflictSide(sess, seen, key, facts)}
			sess.conflicts = withoutConflict(sess.conflicts, key)
			continue
		}
		switch choice {
		case schema.LocalKickstartResolutionTheirs:
			// Taking the file's side gives the draft's answer up.
			delete(sess.answers, key)
			delete(sess.resolutions, key)
		default:
			// Keeping the draft's side records which file value the user saw, so
			// the choice never settles a value its owner did not see.
			sess.resolutions[key] = onboarding.Resolution{Choice: choice, Seen: st.servedConflictSide(sess, seen, key, facts)}
		}
		sess.conflicts = withoutConflict(sess.conflicts, key)
	}
	for _, key := range changed {
		sess.fieldClock[key] = revision
	}
	if err := st.rebuildLocked(sess, facts); err != nil {
		sess.restore(before)
		return schema.LocalKickstartPatchResult{}, nil, nil, newKickstartError(schema.LocalKickstartErrorAnswerRefused,
			"The answer was refused when the draft was rebuilt in internal/api.kickstartSessions.patch: %v. Nothing was changed. Send an answer the setup model accepts.", err)
	}
	if err := st.refuseHiddenAnswerLocked(sess, plan, facts); err != nil {
		sess.restore(before)
		return schema.LocalKickstartPatchResult{}, nil, nil, err
	}
	draft, err := st.draftView(sess.frozen(), facts)
	if err != nil {
		sess.restore(before)
		return schema.LocalKickstartPatchResult{}, nil, nil, err
	}
	st.touchLocked(sess)
	kind := schema.LocalKickstartEventAnswers
	if len(sess.conflicts) != conflictsBefore {
		kind = schema.LocalKickstartEventConflicts
	}
	event := schema.LocalKickstartEvent{Kind: kind, SessionID: sess.id, Revision: revision, View: &draft.view}
	result := schema.LocalKickstartPatchResult{Revision: revision, Stale: stale, View: &draft.view}
	if stale {
		result.Overwritten = append([]schema.LocalKickstartOverwrite{}, overwritten...)
	}
	return result, []schema.LocalKickstartEvent{event}, []*kickstartViewDraft{draft}, nil
}

// refuseHiddenAnswerLocked refuses a request that answers a field the setup
// does not show. It judges the session as the request leaves it, so a request
// that answers a field together with the answer that reveals it is accepted,
// and one that answers a field its own other answers hide is not. The condition
// is the one the page's own view reads.
func (st *kickstartSessions) refuseHiddenAnswerLocked(sess *kickstartSession, plan kickstartPlan, facts kickstartFacts) error {
	write := sess.writeContext(sess.working, sess.configAnswers(), facts)
	for _, field := range sess.model.Fields() {
		if _, answered := plan.answers[field.Key()]; !answered {
			continue
		}
		if condition, hidden := sess.model.HiddenBecause(field, write); hidden {
			return newKickstartError(schema.LocalKickstartErrorFieldHidden,
				"The answer for a hidden field was refused in internal/api.kickstartSessions.patch: %s is shown only when %s, and the %s setup session does not meet that now. Nothing was changed. Answer a field the setup shows, or send the answer that reveals this one in the same request.",
				field.Key(), condition, sess.scope)
		}
	}
	return nil
}

// planLocked checks a request against the model and returns the changes it asks
// for, without applying any of them.
func (st *kickstartSessions) planLocked(sess *kickstartSession, request schema.LocalKickstartAnswers, facts kickstartFacts) (kickstartPlan, error) {
	plan := kickstartPlan{
		answers:     map[onboarding.FieldKey]schema.LocalSettingValue{},
		resolutions: map[onboarding.FieldKey]schema.LocalKickstartResolution{},
	}
	keys := make([]string, 0, len(request.Values))
	for key := range request.Values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, raw := range keys {
		field, err := st.answerableFieldLocked(sess, raw)
		if err != nil {
			return plan, err
		}
		value, err := st.canonicalAnswerLocked(sess, field, request.Values[raw], facts)
		if err != nil {
			return plan, err
		}
		plan.answers[field.Key()] = value
	}
	if request.SelectedIDs != nil {
		if sess.scan.state != onboarding.ScanDone {
			return plan, newKickstartError(schema.LocalKickstartErrorScanNotDone,
				"The selected sessions were not applied because the discovery scan has not finished (it is %s) in internal/api.kickstartSessions.patch. Nothing was changed. Answer the tree after the scan reports done, or rescan.", sess.scan.state)
		}
		answer := onboarding.ApplyTreeAnswer(sess.forest, request.SelectedIDs)
		value, err := onboarding.SelectionIDsValue(answer.SessionIDs)
		if err != nil {
			return plan, newKickstartError(schema.LocalKickstartErrorAnswerUnencodable, "%v in internal/api.kickstartSessions.patch. Nothing was changed. Report this defect.", err)
		}
		plan.answers[onboarding.FieldSelection] = value
	}
	if request.ConnectAnswer != "" {
		answer, err := st.connectPlanLocked(sess, request.ConnectAnswer, facts)
		if err != nil {
			return plan, err
		}
		plan.connect = &answer
	}
	resolutionKeys := make([]string, 0, len(request.Resolutions))
	for key := range request.Resolutions {
		resolutionKeys = append(resolutionKeys, key)
	}
	sort.Strings(resolutionKeys)
	for _, raw := range resolutionKeys {
		field, err := st.answerableFieldLocked(sess, raw)
		if err != nil {
			return plan, err
		}
		plan.resolutions[field.Key()] = request.Resolutions[raw]
	}
	return plan, nil
}

// answerableFieldLocked resolves a field key named in a request to a field of
// this scope's model.
func (st *kickstartSessions) answerableFieldLocked(sess *kickstartSession, raw string) (onboarding.Field, error) {
	key, err := onboarding.NewFieldKey(raw)
	if err != nil {
		return nil, newKickstartError(schema.LocalKickstartErrorFieldUnknown,
			"The field %q is not a setup field in internal/api.kickstartSessions.patch, so the request was not applied. Nothing was changed. %v", raw, err)
	}
	field, ok := sess.model.Field(key)
	if !ok {
		return nil, newKickstartError(schema.LocalKickstartErrorFieldOutOfScope,
			"The field %q is not part of the %s setup session in internal/api.kickstartSessions.patch, so the request was not applied. Nothing was changed. Answer only a field this session's stops carry.", raw, sess.scope)
	}
	return field, nil
}

// canonicalAnswerLocked validates one value answer and returns it in the form
// the field reads back, so a string "90" and the number 90 are one answer.
func (st *kickstartSessions) canonicalAnswerLocked(sess *kickstartSession, field onboarding.Field, raw schema.LocalSettingValue, facts kickstartFacts) (schema.LocalSettingValue, error) {
	refuse := func(why string) error {
		return newKickstartError(schema.LocalKickstartErrorAnswerRefused,
			"The answer for %s was refused in internal/api.kickstartSessions.patch: %s. Nothing was changed. Send a value this field accepts.", field.Key(), why)
	}
	switch field.Kind() {
	case onboarding.FieldKindTree:
		return nil, newKickstartError(schema.LocalKickstartErrorTreeAnswerShape,
			"The answer for %s was refused in internal/api.kickstartSessions.patch: the selection tree is answered with selectedIds, the session ids it selects, and never with a value. Nothing was changed. Send selectedIds.", field.Key())
	case onboarding.FieldKindNotice:
		return nil, refuse("a notice has no answer")
	case onboarding.FieldKindAction:
		token, err := onboarding.StringToken(raw)
		if err != nil {
			return nil, refuse(err.Error())
		}
		for _, option := range field.Options() {
			if option.Value == token {
				return onboarding.StringValue(token), nil
			}
		}
		return nil, refuse(fmt.Sprintf("%q is not one of the field's actions", token))
	default:
		scratch, _, err := sess.freshWorking(st.env.configPath)
		if err != nil {
			return nil, newKickstartError(schema.LocalKickstartErrorSessionUnbuildable, "%v in internal/api.kickstartSessions.patch. Nothing was changed. Report this defect.", err)
		}
		if err := field.Write(scratch, sess.writeContext(scratch, nil, facts), raw); err != nil {
			return nil, refuse(err.Error())
		}
		canonical, err := field.Read(scratch)
		if err != nil {
			return nil, refuse(err.Error())
		}
		return canonical, nil
	}
}

func (st *kickstartSessions) connectPlanLocked(sess *kickstartSession, answer schema.LocalKickstartConnectAnswer, facts kickstartFacts) (onboarding.ConnectAnswer, error) {
	if len(sess.model.ConnectOffers()) == 0 {
		return onboarding.ConnectUnanswered, newKickstartError(schema.LocalKickstartErrorConnectOutOfScope,
			"The connect answer was refused because the %s setup session has no connect offer in internal/api.kickstartSessions.patch. Nothing was changed. Answer the connect offer only in the full session.", sess.scope)
	}
	switch answer {
	case schema.LocalKickstartConnectSkip:
		return onboarding.ConnectSkipped, nil
	case schema.LocalKickstartConnectAccept:
		if !facts.villageConnected {
			return onboarding.ConnectUnanswered, newKickstartError(schema.LocalKickstartErrorConnectNotConnected,
				"The connect offer cannot be accepted because this computer is not connected to peasant online yet, in internal/api.kickstartSessions.patch. Nothing was changed. Sign in with POST /api/v1/sync/login, wait until GET /api/v1/sync/auth reports authenticated, then accept.")
		}
		return onboarding.ConnectConnected, nil
	default:
		return onboarding.ConnectUnanswered, newKickstartError(schema.LocalKickstartErrorConnectUnknown,
			"The connect answer %q is not accept or skip in internal/api.kickstartSessions.patch. Nothing was changed. Send accept or skip.", answer)
	}
}

// changedKeysLocked lists, in order, the field keys a plan really changes. An
// answer equal to the one the session holds changes nothing.
func (st *kickstartSessions) changedKeysLocked(sess *kickstartSession, plan kickstartPlan, seen *kickstartSeenRows, facts kickstartFacts) []string {
	changed := map[string]struct{}{}
	for key, value := range plan.answers {
		if current, ok := sess.answers[key]; ok && onboarding.SameSettingValue(current, value) {
			continue
		}
		changed[key.String()] = struct{}{}
	}
	if plan.connect != nil && *plan.connect != sess.connect {
		changed[kickstartConnectKey] = struct{}{}
	}
	for key, resolution := range plan.resolutions {
		_, answered := sess.answers[key]
		open := hasConflict(sess.conflicts, key)
		recorded := sess.resolutions[key].Choice == resolution
		if st.coupledRowLocked(sess, key, seen, facts) {
			// A field an answer moves as a side effect has no answer to give up
			// or keep: the choice itself is what changes.
			if !recorded {
				changed[key.String()] = struct{}{}
			}
			continue
		}
		switch resolution {
		case schema.LocalKickstartResolutionTheirs:
			// Taking the file's side drops the draft's answer, if there is one.
			if answered || open {
				changed[key.String()] = struct{}{}
			}
		case schema.LocalKickstartResolutionMine:
			// Keeping the draft's side records the choice, if there is an answer
			// to keep and the choice is new.
			if (answered && !recorded) || open {
				changed[key.String()] = struct{}{}
			}
		}
	}
	keys := make([]string, 0, len(changed))
	for key := range changed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// overwrittenLocked names the changed fields that another writer changed after
// the revision the caller last rendered.
func (st *kickstartSessions) overwrittenLocked(sess *kickstartSession, changed []string, since uint64) []schema.LocalKickstartOverwrite {
	var out []schema.LocalKickstartOverwrite
	for _, key := range changed {
		clock := sess.fieldClock[key]
		if clock <= since {
			continue
		}
		previous := schema.LocalSettingNull()
		switch {
		case key == kickstartConnectKey:
			previous = onboarding.StringValue(connectToken(sess.connect))
		case onboarding.FieldKey(key) == onboarding.FieldSelection:
			// The tree is shown as its array of ids, so that is what it replaced.
			if ids, answered := onboarding.SelectionIDs(sess.answers[onboarding.FieldSelection]); answered {
				if value, err := onboarding.SelectionIDsListValue(ids); err == nil {
					previous = value
				}
			}
		default:
			if value, ok := sess.answers[onboarding.FieldKey(key)]; ok {
				previous = append(schema.LocalSettingValue(nil), value...)
			}
		}
		out = append(out, schema.LocalKickstartOverwrite{Key: key, Previous: previous, Revision: clock})
	}
	return out
}

func connectToken(answer onboarding.ConnectAnswer) string {
	switch answer {
	case onboarding.ConnectConnected:
		return string(schema.LocalKickstartConnectAccept)
	case onboarding.ConnectSkipped:
		return string(schema.LocalKickstartConnectSkip)
	default:
		return ""
	}
}

// coupledRowLocked reports whether key is a field the draft did not answer that
// a row is shown for: another answer moves it as a side effect, and the file
// moved it since the session opened.
func (st *kickstartSessions) coupledRowLocked(sess *kickstartSession, key onboarding.FieldKey, seen *kickstartSeenRows, facts kickstartFacts) bool {
	if _, answered := sess.answers[key]; answered {
		return false
	}
	return st.servedConflictSide(sess, seen, key, facts) != nil
}

// seenConflictSide is the file value a served conflict row showed for key.
func seenConflictSide(rows []schema.LocalKickstartConflict, key onboarding.FieldKey) schema.LocalSettingValue {
	for _, row := range rows {
		if row.Field == key.String() {
			return append(schema.LocalSettingValue(nil), row.Theirs...)
		}
	}
	return nil
}

func hasConflict(rows []schema.LocalKickstartConflict, key onboarding.FieldKey) bool {
	for _, row := range rows {
		if row.Field == key.String() {
			return true
		}
	}
	return false
}

func withoutConflict(rows []schema.LocalKickstartConflict, key onboarding.FieldKey) []schema.LocalKickstartConflict {
	out := make([]schema.LocalKickstartConflict, 0, len(rows))
	for _, row := range rows {
		if row.Field != key.String() {
			out = append(out, row)
		}
	}
	return out
}

// kickstartDraft is what a commit reads from a live session: the model, the
// answers a configuration write applies, the sides the user chose for conflict
// rows already shown, the draft that holds the file as it was when the session
// opened, and the write context for the answers as they stand. The session's id,
// scope and revision are not repeated: the caller holds the id, the model holds
// the scope, and the commit is judged against the file, not the revision.
type kickstartDraft struct {
	Model onboarding.Model
	// ConfigAnswers are the answers ApplyAnswers applies. An action answer is a
	// choice made in the session and never reaches the configuration.
	ConfigAnswers onboarding.Answers
	// Resolutions are the sides the user chose for conflict rows already shown,
	// each with the file value the user saw when choosing.
	Resolutions onboarding.Resolutions
	// Draft holds the configuration file as it was when the session opened, as
	// the baseline, and the working copy the answers start from.
	Draft *onboarding.Draft
	// Write is the write context for the answers as they stand: Config is a
	// working copy with every configuration answer applied.
	Write onboarding.WriteContext
}

// beginCommit freezes a session for one commit and returns what the commit
// reads. It refuses a second commit with its reason.
func (st *kickstartSessions) beginCommit(id string) (kickstartDraft, error) {
	facts := st.env.village()
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, err := st.liveLocked(id)
	if err != nil {
		return kickstartDraft{}, err
	}
	if sess.committing {
		return kickstartDraft{}, newKickstartError(schema.LocalKickstartErrorSessionCommitting,
			"Setup session %q is already being saved, so a second save was refused in internal/api.kickstartSessions.beginCommit. Nothing was changed. Wait for the first save to finish.", id)
	}
	_, draft, err := sess.freshWorking(st.env.configPath)
	if err != nil {
		return kickstartDraft{}, err
	}
	configAnswers := sess.configAnswers()
	applied := draft.Working()
	if err := sess.model.ApplyAnswers(applied, configAnswers, sess.writeContext(applied, configAnswers, facts)); err != nil {
		return kickstartDraft{}, newKickstartError(schema.LocalKickstartErrorSessionUnbuildable,
			"The save could not read setup session %q because its answers no longer apply in internal/api.kickstartSessions.beginCommit: %v. Nothing was changed. Report this defect.", id, err)
	}
	sess.committing = true
	resolutions := make(onboarding.Resolutions, len(sess.resolutions))
	for key, resolution := range sess.resolutions {
		resolutions[key] = onboarding.Resolution{Choice: resolution.Choice, Seen: append(schema.LocalSettingValue(nil), resolution.Seen...)}
	}
	return kickstartDraft{
		Model: sess.model, ConfigAnswers: configAnswers, Resolutions: resolutions,
		Draft: draft,
		Write: sess.writeContext(applied, configAnswers, facts),
	}, nil
}

// abortCommit releases a session after a commit that wrote nothing.
func (st *kickstartSessions) abortCommit(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if sess, ok := st.byID[id]; ok && sess.committing {
		sess.committing = false
		// The idle timer did nothing while the commit held the session, so it
		// starts again from now.
		st.touchLocked(sess)
	}
}

// publishConflicts replaces a session's conflict rows and publishes them with
// the full view. It returns the new revision.
func (st *kickstartSessions) publishConflicts(id string, rows []schema.LocalKickstartConflict) (uint64, error) {
	facts := st.env.village()
	var revision uint64
	var failure error
	st.mutateViews(func() ([]schema.LocalKickstartEvent, []*kickstartViewDraft) {
		sess, err := st.liveLocked(id)
		if err != nil {
			failure = err
			return nil, nil
		}
		before := sess.snapshot()
		sess.conflicts = append([]schema.LocalKickstartConflict{}, rows...)
		revision = st.bumpLocked(sess)
		draft, err := st.draftView(sess.frozen(), facts)
		if err != nil {
			sess.restore(before)
			failure = err
			return nil, nil
		}
		return []schema.LocalKickstartEvent{{Kind: schema.LocalKickstartEventConflicts, SessionID: sess.id, Revision: revision, View: &draft.view}}, []*kickstartViewDraft{draft}
	})
	return revision, failure
}

// kickstartFinish publishes the ingest events that follow a commit, after the
// session itself is gone. Its revision continues the session's. It does not
// depend on any open session, so an import that outlives its page, or one the
// page starts again with "import now", publishes under the same id.
type kickstartFinish struct {
	st        *kickstartSessions
	sessionID string

	mu       sync.Mutex
	revision uint64
}

// SessionID is the id of the session that was saved.
func (f *kickstartFinish) SessionID() string { return f.sessionID }

// publishIngest publishes one ingest state on the topic.
func (f *kickstartFinish) publishIngest(ingest schema.LocalKickstartIngest) {
	wait, done := f.st.line.join()
	defer close(done)
	if wait != nil {
		<-wait
	}
	f.mu.Lock()
	f.revision++
	revision := f.revision
	f.mu.Unlock()
	f.st.publish(schema.LocalKickstartEvent{Kind: schema.LocalKickstartEventIngest, SessionID: f.sessionID, Revision: revision, Ingest: &ingest})
}

func (st *kickstartSessions) rememberFinishLocked(finish *kickstartFinish) *kickstartFinish {
	if _, ok := st.finished[finish.sessionID]; !ok {
		st.finishedOrder = append(st.finishedOrder, finish.sessionID)
		for len(st.finishedOrder) > kickstartFinishedKept {
			delete(st.finished, st.finishedOrder[0])
			st.finishedOrder = st.finishedOrder[1:]
		}
	}
	st.finished[finish.sessionID] = finish
	return finish
}

// finishFor returns the publisher of the ingest events of a session id. A
// session a commit ended gets the publisher that continues its revisions; an id
// the store never ended gets a new one, so an import can always publish.
func (st *kickstartSessions) finishFor(id string) *kickstartFinish {
	st.mu.Lock()
	defer st.mu.Unlock()
	if finish, ok := st.finished[id]; ok {
		return finish
	}
	return st.rememberFinishLocked(&kickstartFinish{st: st, sessionID: id})
}

// closeCommitted ends a session with reason committed and publishes the closed
// event that carries the finish body.
func (st *kickstartSessions) closeCommitted(id string, body schema.LocalKickstartCommitted) (*kickstartFinish, error) {
	var finish *kickstartFinish
	var failure error
	st.mutate(func() []schema.LocalKickstartEvent {
		sess, err := st.liveLocked(id)
		if err != nil {
			failure = err
			return nil
		}
		sess.committing = false
		event := st.closeLocked(sess, schema.LocalKickstartClosedCommitted, &body)
		finish = st.rememberFinishLocked(&kickstartFinish{st: st, sessionID: id, revision: event.Revision})
		return []schema.LocalKickstartEvent{event}
	})
	return finish, failure
}

// kickstartEnv wires the session store to the running server: its serve context,
// its hub, its store, its configuration file, and the sign-in this computer holds.
func (s *Server) kickstartEnv(ctx context.Context) kickstartEnv {
	git := &ingest.ExecGitResolver{}
	repositories := s.cfg.RepositoryIdentityResolver
	if repositories == nil {
		repositories = ingest.NewGitRepositoryIdentityResolver()
	}
	env := kickstartEnv{
		ctx:          ctx,
		configPath:   s.cfg.ConfigPath,
		repositories: repositories,
		spawn:        func(fn func()) { s.spawnBackground(ctx, func(context.Context) { fn() }) },
		loadConfig: func(ctx context.Context) (*config.Config, error) {
			data, _, cfg, err := config.ReadDocumentFile(ctx, s.cfg.ConfigPath)
			if err != nil {
				return nil, err
			}
			if data == nil {
				// No file yet: start from the defaults every command applies, such
				// as the email git supplies, as the terminal's first run does.
				return config.LoadDefaults(ctx, git), nil
			}
			return cfg, nil
		},
		village: func() kickstartFacts {
			creds, err := auth.LoadCredentialsFrom(s.cfg.ConfigHome)
			if err != nil || creds == nil || !creds.IsValid() {
				return kickstartFacts{}
			}
			return kickstartFacts{villageConnected: true, villageURL: creds.VillageURL, villageUserID: creds.UserID}
		},
		retentionSeed: func() int {
			// The retention stop starts on the value Claude Code already has in
			// force, or the recommended keep-forever when it has none.
			if current, ok := claudesettings.ReadCleanupDays(); ok && current > 0 {
				return current
			}
			return onboarding.RecommendedRetentionDays
		},
	}
	if s.hub != nil {
		env.emit = hubEmitter(ctx, s.hub)
	}
	if db := s.cfg.Store; db != nil {
		env.stored = db.AllIngestedSessions
		env.scan = func(ctx context.Context, cfg *config.Config, known ingest.KnownSessionIndex, progress discovery.ScanProgress) (discovery.ProviderInventory, []onboarding.SessionListing, discovery.SubagentRelation) {
			return discovery.Scan(ctx, cfg, &ingest.OSFileSystem{}, git, known, db, progress)
		}
	}
	return env
}
