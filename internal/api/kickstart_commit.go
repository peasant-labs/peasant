package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

// POST /api/v1/kickstart/sessions/{id}/commit saves a session. The order is
// fixed, and each step is owned by one function:
//
//	freeze the session            kickstartSessions.beginCommit
//	lock, read, compare, swap,
//	retention, live publish       onboarding.Draft.Commit (releases the lock)
//	start the import              syncHandler.StartIngest
//	end the session               kickstartSessions.closeCommitted
//	push the import's progress    kickstartImportFeed, after the closed event
//
// A refused save (409) writes nothing and publishes the rows. Nothing after the
// swap can retract saved: true; a failed retention write, a busy import or an
// import that cannot start is reported in the body and on the topic. The commit
// installs no hook, publishes no transcript and calls no village endpoint.

// kickstartCommitter answers the commit route over the session store and the
// server's live configuration and ingest.
type kickstartCommitter struct {
	sessions *kickstartSessions
	ingest   *syncHandler
	live     *liveConfig
	catalog  []settingSpec
	feed     *kickstartImportFeed
}

// ownsSetting reports the read-only settings keys a commit may change in the live
// configuration: the saved selection. The settings routes cannot change them; a
// setup session can.
func ownsSetting(key string) bool {
	return key == "selection" || strings.HasPrefix(key, "selection.")
}

// handleCommit answers POST /api/v1/kickstart/sessions/{id}/commit.
func (h *kickstartHandler) handleCommit(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	if h.commit == nil {
		writeAPIError(w, http.StatusServiceUnavailable,
			"Setup could not be saved because this server has no import to start in internal/api.kickstartHandler.handleCommit. Nothing was changed. Start the dashboard with `peasant web start`, then retry.",
			"kickstart_unavailable")
		return
	}
	h.commit.serve(w, r, r.PathValue("id"))
}

func (c *kickstartCommitter) serve(w http.ResponseWriter, r *http.Request, id string) {
	draft, err := c.sessions.beginCommit(id)
	if err != nil {
		writeKickstartError(w, err)
		return
	}
	env := c.sessions.env
	result, err := draft.Draft.Commit(r.Context(), onboarding.CommitInput{
		Model:        draft.Model,
		Answers:      draft.ConfigAnswers,
		Resolutions:  draft.Resolutions,
		Context:      draft.Write,
		LockDeadline: env.now().Add(env.lockWait),
		Hooks: onboarding.CommitHooks{
			Retention: env.writeRetention,
			Publish: func(saved *config.Config) {
				c.live.applyOwning(c.catalog, saved, ownsSetting)
			},
		},
	})
	if err != nil {
		c.sessions.abortCommit(id)
		writeKickstartError(w, commitFailureResponse(err))
		return
	}
	if !result.Saved {
		c.refuse(w, id, result.Conflicts)
		return
	}
	c.finish(w, id, draft, result)
}

// refuse answers 409 with the rows and publishes them to every tab, then lets the
// session take answers again. Nothing was written.
func (c *kickstartCommitter) refuse(w http.ResponseWriter, id string, rows []schema.LocalKickstartConflict) {
	_, publishErr := c.sessions.publishConflicts(id, rows)
	c.sessions.abortCommit(id)
	if publishErr != nil {
		slog.Warn("kickstart: the conflict rows of a refused save were not published", "session", id, "error", publishErr)
	}
	refusal := schema.LocalKickstartCommitRefusal{
		Error:     fmt.Sprintf("The setup was not saved because %d answered setting(s) changed in the configuration file since this session opened, in internal/api.kickstartHandler.handleCommit. Nothing was written. Choose mine or theirs for each row, then save again.", len(rows)),
		Conflicts: append([]schema.LocalKickstartConflict{}, rows...),
	}
	writeContractStatus(w, http.StatusConflict, &refusal)
}

// finish runs the steps that follow a swap. None of them can undo the save.
func (c *kickstartCommitter) finish(w http.ResponseWriter, id string, draft kickstartDraft, result onboarding.CommitResult) {
	// The import is claimed after Commit returned, so after the lock was released
	// and after the live configuration was published. It does not run until the
	// session's closed event is out, so a tab never hears an import state for a
	// session it has not been told is saved.
	start, launch, startErr := c.ingest.reserveIngest(true)

	body := schema.LocalKickstartCommitted{
		Saved:      true,
		ConfigPath: result.ConfigPath,
		Ingest:     ingestResponse(start, startErr),
		NextSteps:  webNextSteps(),
	}
	if result.Retention.Status == onboarding.RetentionFailed {
		body.Diagnostics = []string{result.Retention.Note}
	}
	if err := body.Validate(); err != nil {
		slog.Error("kickstart: a saved commit's body breaks the Local API contract", "session", id, "error", err)
	}

	finish, closeErr := c.sessions.closeCommitted(id, body)
	if closeErr != nil {
		// The save stands whether or not the session was still there to close.
		slog.Warn("kickstart: the saved session could not be closed", "session", id, "error", closeErr)
		finish = c.sessions.finishFor(id)
	}
	switch body.Ingest.State {
	case schema.LocalKickstartIngestBusy, schema.LocalKickstartIngestFailed:
		c.feed.wait(finish)
	case schema.LocalKickstartIngestRunning:
		launch(c.feed.observer(finish))
		c.feed.announce(schema.LocalKickstartIngest{State: schema.LocalKickstartIngestRunning})
	}

	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	_ = json.NewEncoder(w).Encode(&body)
}

// webNextSteps is the finish menu of the web: the terminal's four and the link
// to the settings section where an automatic-publishing rule is chosen. The
// neutral model owns the set.
func webNextSteps() []schema.LocalKickstartNextStep {
	steps := onboarding.NextStepsWeb()
	out := make([]schema.LocalKickstartNextStep, len(steps))
	for i, step := range steps {
		out[i] = schema.LocalKickstartNextStep(step)
	}
	return out
}

// ingestResponse is the import's state in the commit body.
func ingestResponse(start IngestStart, err error) schema.LocalKickstartIngest {
	switch {
	case err != nil:
		return schema.LocalKickstartIngest{State: schema.LocalKickstartIngestFailed, Error: err.Error()}
	case start.AlreadyRunning:
		return schema.LocalKickstartIngest{
			State: schema.LocalKickstartIngestBusy,
			Note:  "an import started earlier is still running; its selection may predate this save",
		}
	default:
		return schema.LocalKickstartIngest{State: schema.LocalKickstartIngestRunning}
	}
}

// commitFailureResponse maps a failed save to the refusal it is answered with.
// The contract declares 404 and 409 for this route; the rest follow the status
// the other kickstart routes use for the same kind of failure.
func commitFailureResponse(err error) error {
	var failed *onboarding.CommitError
	if !errors.As(err, &failed) {
		return err
	}
	switch failed.Kind {
	case onboarding.CommitFailureInvalid:
		return newKickstartError(http.StatusBadRequest, "kickstart_commit_refused",
			"The setup was not saved in internal/api.kickstartHandler.handleCommit: %v. Nothing was changed. Correct the answer the message names, then save again.", failed.Err)
	case onboarding.CommitFailureRead:
		return newKickstartError(http.StatusInternalServerError, "kickstart_config_unreadable",
			"The setup was not saved because the configuration file could not be read in internal/api.kickstartHandler.handleCommit: %v. Nothing was changed. Fix or restore the file, then save again.", failed.Err)
	case onboarding.CommitFailureLock:
		return newKickstartError(http.StatusServiceUnavailable, "kickstart_config_busy",
			"The setup was not saved because another change holds the configuration file in internal/api.kickstartHandler.handleCommit: %v. Nothing was changed. Wait for it to finish, then save again.", failed.Err)
	case onboarding.CommitFailureChanged:
		return newKickstartError(http.StatusServiceUnavailable, "kickstart_config_changing",
			"The setup was not saved because the configuration file kept changing in internal/api.kickstartHandler.handleCommit: %v. Nothing was written. Stop the other program that edits it, then save again.", failed.Err)
	default:
		return newKickstartError(http.StatusInternalServerError, "kickstart_config_unwritable",
			"The setup was not saved because the configuration file could not be written in internal/api.kickstartHandler.handleCommit: %v. The previous file is unchanged. Fix the directory's permissions or free space, then save again.", failed.Err)
	}
}

// writeContractStatus answers status with a contract value, after checking it
// the way a client does.
func writeContractStatus(w http.ResponseWriter, status int, value contractValue) {
	if err := value.Validate(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "The answer breaks the Local API contract, so it is not sent: "+err.Error()+". Retry; if it repeats, report it.", publishingContractBreachCode)
		return
	}
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// kickstartImportFeed carries an import's state to the saved sessions that wait
// for one. A session waits when its commit found an import already running, or
// could not start one: its finish panel offers "import now", and the import that
// answers it (or any import that starts meanwhile) imports the selection this
// session saved. The import's events are published under the session id, so they
// need no open session.
type kickstartImportFeed struct {
	mu      sync.Mutex
	waiting map[string]*kickstartFinish
	order   []string
}

func newKickstartImportFeed() *kickstartImportFeed {
	return &kickstartImportFeed{waiting: map[string]*kickstartFinish{}}
}

// wait records a saved session whose import has not started for it.
func (f *kickstartImportFeed) wait(finish *kickstartFinish) {
	if f == nil || finish == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.waiting[finish.SessionID()]; !ok {
		f.order = append(f.order, finish.SessionID())
		for len(f.order) > kickstartFinishedKept {
			delete(f.waiting, f.order[0])
			f.order = f.order[1:]
		}
	}
	f.waiting[finish.SessionID()] = finish
}

// snapshot is the sessions waiting now.
func (f *kickstartImportFeed) snapshot() []*kickstartFinish {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	targets := make([]*kickstartFinish, 0, len(f.waiting))
	for _, id := range f.order {
		if finish, ok := f.waiting[id]; ok {
			targets = append(targets, finish)
		}
	}
	return targets
}

// publish sends one import state to the given sessions. A finished import (idle)
// ends their wait; a failed one leaves it, so the panel can try again.
func (f *kickstartImportFeed) publish(targets []*kickstartFinish, ingest schema.LocalKickstartIngest) {
	if f != nil && ingest.State == schema.LocalKickstartIngestIdle {
		f.mu.Lock()
		for _, finish := range targets {
			delete(f.waiting, finish.SessionID())
		}
		f.mu.Unlock()
	}
	for _, finish := range targets {
		finish.publishIngest(ingest)
	}
}

// announce publishes one import state to every session waiting now.
func (f *kickstartImportFeed) announce(ingest schema.LocalKickstartIngest) {
	f.publish(f.snapshot(), ingest)
}

// observer returns what a run reports its final state to. The run reports to the
// sessions that wait when it starts, and no others: an import that began before a
// session saved says nothing about the selection that session saved. own, when it
// is not nil, is the session whose commit started the run. A nil result means
// nobody watches.
func (f *kickstartImportFeed) observer(own *kickstartFinish) func(schema.LocalKickstartIngest) {
	targets := f.snapshot()
	if own != nil {
		targets = append([]*kickstartFinish{own}, targets...)
	}
	if len(targets) == 0 {
		return nil
	}
	return func(ingest schema.LocalKickstartIngest) {
		f.publish(targets, ingest)
	}
}

// reviewLocked previews a session's save with the commit's own chain: the
// moved fields and the structured diff of what the file would become. A review
// that cannot be built is logged and leaves the view without a diff; the save
// itself re-reads the file under its lock.
func (st *kickstartSessions) reviewLocked(sess *kickstartSession, facts kickstartFacts) (onboarding.ReviewResult, bool) {
	working, draft, err := sess.freshWorking(st.env.configPath)
	if err != nil {
		slog.Warn("kickstart: a review could not open its draft", "session", sess.id, "error", err)
		return onboarding.ReviewResult{}, false
	}
	answers := sess.configAnswers()
	review, err := draft.Review(context.Background(), onboarding.ReviewInput{
		Model:       sess.model,
		Answers:     answers,
		Resolutions: sess.resolutions,
		Context:     sess.writeContext(working, answers, facts),
		Limits:      onboarding.DefaultDiffLimits(),
	})
	if err != nil {
		slog.Warn("kickstart: the review diff could not be built", "session", sess.id, "error", err)
		return onboarding.ReviewResult{}, false
	}
	return review, true
}

// reviewRows are the rows that still need a choice: the rows a refused
// save published, and the answered fields the file has moved since, which the
// review finds on its own. The other scope's save shows up here.
func reviewRows(published []schema.LocalKickstartConflict, review onboarding.ReviewResult) []schema.LocalKickstartConflict {
	rows := append([]schema.LocalKickstartConflict{}, published...)
	for _, key := range review.Unresolved {
		if hasConflict(rows, key) {
			continue
		}
		for _, row := range review.Conflicts {
			if row.Field == key.String() {
				rows = append(rows, row)
			}
		}
	}
	return rows
}

// servedConflictSide is the file value a conflict row showed for key: from the
// rows a refused save published, or from the review the view carried.
func (st *kickstartSessions) servedConflictSide(sess *kickstartSession, key onboarding.FieldKey) schema.LocalSettingValue {
	if side := seenConflictSide(sess.conflicts, key); side != nil {
		return side
	}
	if review, ok := st.reviewLocked(sess, st.env.village()); ok {
		return seenConflictSide(review.Conflicts, key)
	}
	return nil
}
