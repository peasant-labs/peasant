package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// CollectiveChanges is the change of audience one publish from the local web
// asks for: the collectives each transcript is shared with, and the ones it is
// taken back from. A collective named in neither keeps its access.
type CollectiveChanges struct {
	Add    []schema.VillageUUID
	Remove []schema.VillageUUID
}

func (c CollectiveChanges) empty() bool { return len(c.Add) == 0 && len(c.Remove) == 0 }

// CollectiveSharer is the Village surface the collective steps use.
type CollectiveSharer interface {
	ShareTranscript(context.Context, schema.TranscriptID, schema.VillageUUID) error
	UnshareTranscript(context.Context, schema.TranscriptID, schema.VillageUUID) error
	LatestShareStatus(context.Context, schema.TranscriptID, schema.VillageUUID) (schema.VillageShareStatus, error)
}

// SharePublishVillage is everything a publish from the local web asks Village
// besides the upload: the collective steps and the waiting pull requests.
type SharePublishVillage interface {
	CollectiveSharer
	GetPromptRequests(context.Context) (*schema.VillagePromptRequestsResponse, int, error)
}

// SharePublishStore is the local store surface a publish from the local web
// reads besides the pipeline's: whether each named session exists and why one
// is held, and the receipt that names its transcript on Village.
type SharePublishStore interface {
	SessionsByIDs(context.Context, []string) ([]store.SessionRow, error)
	SessionsWithoutMetrics(context.Context) ([]ingest.HeldSession, error)
	SessionPublications(ctx context.Context, origin, owner string, sessionIDs []string) (map[string]store.PublicationRecord, error)
}

var (
	_ SharePublishVillage = (*village.VillageClient)(nil)
	_ SharePublishStore   = (*store.Store)(nil)
)

// sharePromptRequestTimeout bounds the waiting pull request lookup. It is a
// convenience beside the publish, so a Village that does not answer costs the
// response this long and nothing else.
const sharePromptRequestTimeout = 3 * time.Second

// NewSharePipeline builds the pipeline a publish from the local web runs. It
// publishes exactly the named sessions. A first publication opens private and
// no license is sent, so neither a configured visibility nor a configured
// default license reaches Village: publishing from the local web is for
// collectives, and the collective steps that follow decide who can read the
// transcript. An update keeps the audience and the license the transcript
// already has.
func NewSharePipeline(pipelineStore PipelineStore, transport Transport, creds *auth.Credentials, cfg *config.Config, redactor ingest.TextRedactor, sessionIDs []string) (*Pipeline, error) {
	if cfg == nil {
		return nil, fmt.Errorf("build the publish pipeline for the local web: no configuration was loaded; nothing was published; start Peasant with its configuration and retry")
	}
	shareCfg, runCfg := CollectiveAudience(cfg, PipelineConfig{FilterSessionIDs: sessionIDs})
	return NewPipeline(pipelineStore, transport, creds, shareCfg, &ingest.OSFileSystem{},
		runCfg, redactor, io.Discard)
}

// CollectiveAudience narrows a publish to one whose audience is collectives:
// a first publication opens private and no license is sent, whatever the
// configuration says, so neither a configured visibility nor a configured
// default license reaches Village. The collective steps that follow decide who
// can read the transcript. An update asks for no change, so it keeps the
// audience and the license the transcript has. A publish from the local web
// and a hook push under an auto-publish rule both publish this way.
func CollectiveAudience(cfg *config.Config, runCfg PipelineConfig) (*config.Config, PipelineConfig) {
	narrowed := *cfg
	narrowed.Push.License = ""
	runCfg.Visibility = schema.VisibilityPrivate
	runCfg.License = ""
	runCfg.ChangeVisibility, runCfg.ChangeLicense = false, false
	return &narrowed, runCfg
}

// SharePublish is one publish from the local web: the content of every named
// session through the push pipeline, then each session's collective steps.
type SharePublish struct {
	Pipeline *Pipeline
	Store    SharePublishStore
	Village  SharePublishVillage
	Creds    *auth.Credentials
}

// SharePublishResult is what a publish from the local web did.
type SharePublishResult struct {
	// Response is the typed answer, one result per named session.
	Response schema.SyncPushResponse
	// Pushed is the pipeline's own result, which the annotation push is scoped
	// by. It is nil when the pipeline returned none.
	Pushed *PushResult
}

// Run publishes the named sessions and then changes who can read each one.
//
// Every named session gets one result. Its steps are listed in the order they
// ran: the content first, then each collective it is taken back from, then each
// collective it is shared with. Taking access away runs whenever Village holds
// the transcript, so it never waits on an unrelated failure; sharing runs only
// after every earlier step of the session succeeded or was skipped, so a
// failure never widens access. A step that did not run after a failure is
// not_attempted, and the session's error says what Village kept.
//
// The error is returned only when the pipeline failed before it considered any
// session; nothing was sent then.
func (s SharePublish) Run(ctx context.Context, sessionIDs []string, changes CollectiveChanges) (SharePublishResult, error) {
	pushed, runErr := s.Pipeline.Run(ctx)
	if runErr != nil && pushed == nil {
		return SharePublishResult{}, runErr
	}
	contents, rows, err := contentOutcomes(ctx, s.Store, s.Creds, sessionIDs, pushed, runErr)
	if err != nil {
		return SharePublishResult{Pushed: pushed}, err
	}
	requests := s.waitingPromptRequests(ctx, rows)

	response := schema.SyncPushResponse{Sessions: make([]schema.SyncPushSessionResult, 0, len(sessionIDs))}
	for _, id := range sessionIDs {
		result := runSessionSteps(ctx, s.Village, contents[id], changes, "Publish again to retry.")
		if row, ok := rows[id]; ok && row.CanonicalRemote != nil {
			result.WaitingPullRequests = village.WaitingPromptRequestsFor(requests, village.GitHubRepositoryFullName(*row.CanonicalRemote))
		}
		switch result.Status {
		case schema.SyncPushSessionNew:
			response.New++
		case schema.SyncPushSessionUpdated:
			response.Updated++
		case schema.SyncPushSessionSkipped:
			response.Skipped++
		case schema.SyncPushSessionError:
			response.Errors++
		}
		response.Sessions = append(response.Sessions, result)
	}
	if err := response.Validate(); err != nil {
		return SharePublishResult{Pushed: pushed}, fmt.Errorf("the publish ran, but its result breaks the Local API contract, so it is not reported as success: %w; open the transcripts on Village to see what they hold", err)
	}
	return SharePublishResult{Response: response, Pushed: pushed}, nil
}

// RuleShareStore is the local store surface sharing under an auto-publish rule
// reads and writes: the sessions and their receipts, and the attempt ledger a
// failed share is recorded in.
type RuleShareStore interface {
	SharePublishStore
	RecordPublicationAttempt(context.Context, store.PublicationAttemptDiagnostic) error
}

var _ RuleShareStore = (*store.Store)(nil)

// RuleShare shares the transcripts a hook push sent with the collectives an
// auto-publish rule binds their sessions to.
type RuleShare struct {
	Store   RuleShareStore
	Village CollectiveSharer
	Creds   *auth.Credentials
	// Retry is the command that publishes and shares again, named in the
	// reason recorded for a failed share.
	Retry string
}

// Run shares each transcript the finished run sent (published or updated)
// with the collectives its session is bound to. A session the run skipped as
// unchanged, held, or failed is not shared: sharing never follows a failure,
// and an unchanged transcript keeps the readers it has.
//
// An update does not undo a decision made on Village since the last push: a
// collective that already holds the transcript, or whose share was rejected,
// retracted, or revoked, is skipped with the reason, and only a collective
// that never had a share is asked. A share that fails is recorded as the
// session's latest failed attempt, so the local publication state shows it.
func (s RuleShare) Run(ctx context.Context, pushed *PushResult, collectives map[string][]schema.VillageUUID) ([]schema.SyncPushSessionResult, error) {
	if pushed == nil {
		return nil, nil
	}
	var sent []string
	updated := map[string]bool{}
	for _, result := range pushed.Sessions {
		if (result.Status == PushStatusNew || result.Status == PushStatusUpdated) && len(collectives[result.SessionID]) > 0 {
			sent = append(sent, result.SessionID)
			updated[result.SessionID] = result.Status == PushStatusUpdated
		}
	}
	if len(sent) == 0 {
		return nil, nil
	}
	// The local reads run on a context of their own: the upload budget may be
	// spent, and a sent transcript that cannot be read back is never shared
	// or recorded.
	readCtx, cancelRead := persistenceContext(ctx)
	defer cancelRead()
	contents, _, err := contentOutcomes(readCtx, s.Store, s.Creds, sent, pushed, nil)
	if err != nil {
		return nil, err
	}
	results := make([]schema.SyncPushSessionResult, 0, len(sent))
	for _, id := range sent {
		outcome := contents[id]
		add, decided := collectives[id], []schema.SyncPushStepResult(nil)
		if updated[id] && outcome.transcript != nil {
			add, decided, err = s.undecided(ctx, outcome.transcript.Receipt.TranscriptID, add)
			if err != nil {
				result := schema.SyncPushSessionResult{SessionID: id, Status: schema.SyncPushSessionError, Title: outcome.title, TranscriptURL: outcome.transcript.Receipt.TranscriptURL,
					Error: "the content was published, but reading who can read the transcript failed, so it was not shared again: " + err.Error()}
				result.Error += "; retry with: " + s.Retry
				result.Steps = []schema.SyncPushStepResult{*outcome.content}
				results = append(results, s.recorded(ctx, outcome, result))
				continue
			}
		}
		// The next push does not send an unchanged session again, so it does
		// not retry the share either: the reason names the command that does.
		result := runSessionSteps(ctx, s.Village, outcome, CollectiveChanges{Add: add}, "Retry with: "+s.Retry)
		result.Steps = append(result.Steps, decided...)
		results = append(results, s.recorded(ctx, outcome, result))
	}
	return results, nil
}

// undecided splits the collectives into the ones to ask and a skipped step for
// each one that already has a share of the transcript, live or decided.
func (s RuleShare) undecided(ctx context.Context, transcript schema.TranscriptID, collectives []schema.VillageUUID) ([]schema.VillageUUID, []schema.SyncPushStepResult, error) {
	var ask []schema.VillageUUID
	var skipped []schema.SyncPushStepResult
	for _, id := range collectives {
		collective := id
		// The latest event of the share history says what the collective, or
		// the developer, last did with this transcript there.
		latest, err := s.Village.LatestShareStatus(ctx, transcript, id)
		if err != nil {
			return nil, nil, err
		}
		reason := ""
		switch latest {
		case "":
			ask = append(ask, id)
			continue
		case schema.VillageShareStatusApproved, schema.VillageShareStatusPending:
			reason = "the collective already holds this transcript or its share waits for approval"
		case schema.VillageShareStatusRejected:
			reason = "the collective's owner rejected this transcript, and a rule does not submit it again; share it again on Village if you want to"
		default:
			reason = "this transcript was taken back from the collective, and a rule does not share it again; share it again on Village if you want to"
		}
		skipped = append(skipped, schema.SyncPushStepResult{Step: schema.SyncPushStepAddCollective, CollectiveID: &collective, Outcome: schema.SyncPushStepSkipped, Reason: reason})
	}
	return ask, skipped, nil
}

// recorded records a failed session as its latest failed attempt, under the
// access stage: the content is published and who can read it is not what the
// rule asks.
func (s RuleShare) recorded(ctx context.Context, outcome contentOutcome, result schema.SyncPushSessionResult) schema.SyncPushSessionResult {
	if result.Status != schema.SyncPushSessionError || outcome.transcript == nil {
		return result
	}
	diagnosticCtx, cancel := persistenceContext(ctx)
	defer cancel()
	_ = s.Store.RecordPublicationAttempt(diagnosticCtx, store.PublicationAttemptDiagnostic{
		VillageOrigin: s.Creds.VillageURL, OwnerUserID: s.Creds.UserID, SessionID: result.SessionID,
		ProjectHash: outcome.transcript.ProjectHash, Stage: store.PublicationAttemptStageVisibility, Message: result.Error,
	})
	return result
}

// contentOutcome is what happened to one session's content, and the transcript
// Village holds for it after the push.
type contentOutcome struct {
	sessionID string
	title     string
	status    schema.SyncPushSessionStatus
	// content is the content step, or nil when the session names nothing this
	// computer recorded, so no step applies to it.
	content *schema.SyncPushStepResult
	// transcript is this account's receipt for the session after the push, or
	// nil when Village holds no transcript of it for this account.
	transcript *store.PublicationRecord
	// runStopped says the run stopped before it sent this session, so no
	// collective step can run either.
	runStopped bool
}

func contentOutcomes(ctx context.Context, sessions SharePublishStore, creds *auth.Credentials, sessionIDs []string, pushed *PushResult, runErr error) (map[string]contentOutcome, map[string]store.SessionRow, error) {
	results := make(map[string]SessionPushResult, len(pushed.Sessions))
	for _, result := range pushed.Sessions {
		results[result.SessionID] = result
	}
	storedRows, err := sessions.SessionsByIDs(ctx, sessionIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("read the published sessions after the push: %w; the push ran, so open the transcripts on Village to see what they hold", err)
	}
	rows := make(map[string]store.SessionRow, len(storedRows))
	for _, row := range storedRows {
		rows[row.SessionID] = row
	}
	heldRows, err := sessions.SessionsWithoutMetrics(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read the held sessions after the push: %w; the push ran, so open the transcripts on Village to see what they hold", err)
	}
	held := make(map[string]bool, len(heldRows))
	for _, row := range heldRows {
		held[row.SessionID] = true
	}
	receipts, err := sessions.SessionPublications(ctx, creds.VillageURL, creds.UserID, sessionIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("read the publication receipts after the push: %w; the push ran, so open the transcripts on Village to see what they hold", err)
	}

	outcomes := make(map[string]contentOutcome, len(sessionIDs))
	for _, id := range sessionIDs {
		outcome := contentOutcome{sessionID: id}
		if receipt, ok := receipts[id]; ok {
			outcome.transcript = &receipt
		}
		content := schema.SyncPushStepResult{Step: schema.SyncPushStepContent}
		result, considered := results[id]
		switch {
		case considered:
			outcome.title = result.Title
			switch result.Status {
			case PushStatusNew, PushStatusUpdated:
				outcome.status = schema.SyncPushSessionNew
				if result.Status == PushStatusUpdated {
					outcome.status = schema.SyncPushSessionUpdated
				}
				content.Outcome = schema.SyncPushStepSucceeded
				if outcome.transcript == nil {
					outcome.status = schema.SyncPushSessionError
					content.Outcome = schema.SyncPushStepFailed
					content.Reason = "Village accepted the content, but no receipt on this computer names its transcript, so its link and its collectives cannot be reached; publish again to record the receipt"
				}
			case PushStatusSkipped:
				outcome.status = schema.SyncPushSessionSkipped
				content.Outcome = schema.SyncPushStepSkipped
				content.Reason = "Village already holds this content; nothing was sent"
			case PushStatusHeld:
				outcome.status = schema.SyncPushSessionHeld
				content.Outcome = schema.SyncPushStepSkipped
				content.Reason = "the session is held back; nothing was sent"
				if result.HeldReason != "" {
					content.Reason = "nothing was sent: " + result.HeldReason
				}
			default:
				outcome.status = schema.SyncPushSessionError
				content.Outcome = schema.SyncPushStepFailed
				content.Reason = "sending the content failed: " + errorText(result.Error)
			}
		case runErr != nil:
			outcome.status = schema.SyncPushSessionError
			outcome.runStopped = true
			content.Outcome = schema.SyncPushStepFailed
			content.Reason = "the push stopped before it sent this session: " + runErr.Error()
		case held[id]:
			// A session whose metrics are not computed yet is not listed with
			// the stored sessions either, so this comes first.
			outcome.status = schema.SyncPushSessionHeld
			content.Outcome = schema.SyncPushStepSkipped
			content.Reason = "the session is held because its metrics are not computed yet; nothing was sent; run 'peasant ingest' and publish again"
		case !hasRow(rows, id):
			outcome.status = schema.SyncPushSessionError
			outcomes[id] = outcome
			continue
		default:
			outcome.status = schema.SyncPushSessionError
			content.Outcome = schema.SyncPushStepFailed
			content.Reason = "the push did not reach this session, because it is not among the sessions this computer publishes; check push.method and push.sources in config.yaml, run 'peasant ingest', and publish again"
		}
		outcome.content = &content
		outcomes[id] = outcome
	}
	return outcomes, rows, nil
}

func hasRow(rows map[string]store.SessionRow, id string) bool {
	_, ok := rows[id]
	return ok
}

// waitingPromptRequests reads the caller's waiting pull requests once for the
// whole publish, only when a named session comes from a GitHub repository. It
// is a convenience: every failure is silent and reports none.
func (s SharePublish) waitingPromptRequests(ctx context.Context, rows map[string]store.SessionRow) []schema.VillagePromptRequest {
	fromGitHub := false
	for _, row := range rows {
		fromGitHub = fromGitHub || (row.CanonicalRemote != nil && village.GitHubRepositoryFullName(*row.CanonicalRemote) != "")
	}
	if !fromGitHub {
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, sharePromptRequestTimeout)
	defer cancel()
	response, _, err := s.Village.GetPromptRequests(lookupCtx)
	if err != nil || response == nil {
		return nil
	}
	return response.Requests
}

// runSessionSteps runs the collective steps of one session after its content
// step and assembles its result.
func runSessionSteps(ctx context.Context, sharer CollectiveSharer, outcome contentOutcome, changes CollectiveChanges, retry string) schema.SyncPushSessionResult {
	result := schema.SyncPushSessionResult{SessionID: outcome.sessionID, Status: outcome.status, Title: outcome.title}
	if outcome.content == nil {
		result.Error = "no session with this ID is recorded on this computer; nothing was published; open the session from the local list and publish it again"
		return result
	}
	content := *outcome.content
	if outcome.transcript != nil && (content.Outcome == schema.SyncPushStepSucceeded || content.Outcome == schema.SyncPushStepSkipped) {
		result.TranscriptURL = outcome.transcript.Receipt.TranscriptURL
	}
	steps := []schema.SyncPushStepResult{content}
	// stopped is the index of the first failed step, or -1 while none failed.
	stopped := -1
	if content.Outcome == schema.SyncPushStepFailed {
		stopped = 0
	}

	for _, id := range changes.Remove {
		collective := id
		step := schema.SyncPushStepResult{Step: schema.SyncPushStepRemoveCollective, CollectiveID: &collective}
		switch {
		case outcome.runStopped:
			step.Outcome = schema.SyncPushStepNotAttempted
		case outcome.transcript == nil:
			step.Outcome = schema.SyncPushStepSkipped
			step.Reason = "Village holds no transcript of this session, so no collective can read it"
		default:
			var refusal *village.StatusError
			if err := sharer.UnshareTranscript(ctx, outcome.transcript.Receipt.TranscriptID, collective); errors.As(err, &refusal) && refusal.StatusCode == http.StatusNotFound {
				step.Outcome = schema.SyncPushStepFailed
				step.Reason = missingTranscriptReason
			} else if err != nil {
				step.Outcome = schema.SyncPushStepFailed
				step.Reason = "taking the transcript back failed: " + err.Error()
			} else {
				step.Outcome = schema.SyncPushStepSucceeded
			}
		}
		steps = append(steps, step)
		if step.Outcome == schema.SyncPushStepFailed && stopped < 0 {
			stopped = len(steps) - 1
		}
	}

	for _, id := range changes.Add {
		collective := id
		step := schema.SyncPushStepResult{Step: schema.SyncPushStepAddCollective, CollectiveID: &collective}
		switch {
		case stopped >= 0 || outcome.runStopped:
			step.Outcome = schema.SyncPushStepNotAttempted
		case outcome.status == schema.SyncPushSessionHeld:
			step.Outcome = schema.SyncPushStepSkipped
			step.Reason = "the session is held, so it was not shared; publish it again after ingest completes"
		case outcome.transcript == nil:
			step.Outcome = schema.SyncPushStepSkipped
			step.Reason = "Village holds no transcript of this session, so there is nothing to share"
		default:
			step = shareWithCollective(ctx, sharer, outcome.transcript.Receipt.TranscriptID, step)
		}
		steps = append(steps, step)
		if step.Outcome == schema.SyncPushStepFailed && stopped < 0 {
			stopped = len(steps) - 1
		}
	}

	result.Steps = steps
	if stopped >= 0 {
		result.Status = schema.SyncPushSessionError
		result.Error = stoppedMessage(steps[stopped], outcome, steps, retry)
	}
	return result
}

// missingTranscriptReason explains a share change Village refused because the
// transcript this computer's receipt names is gone from Village, for example
// deleted there. An unchanged session is not uploaded again on its own.
const missingTranscriptReason = "Village no longer holds the transcript this computer's receipt names, for example because it was deleted on Village; run 'peasant village push --force' choosing only this session to publish it again, then change its collectives"

// shareWithCollective offers the transcript to the step's collective and
// reports what the collective did with it. Village's answer to a share does not
// say whether the collective accepted it, holds it for its owner's approval, or
// skipped it because the caller is not a member it takes contributions from,
// and a 409 does not say whether a live share already exists or another request
// won a race. The latest event of the share history says which, so every
// outcome is read back from it.
func shareWithCollective(ctx context.Context, sharer CollectiveSharer, transcript schema.TranscriptID, step schema.SyncPushStepResult) schema.SyncPushStepResult {
	collective := *step.CollectiveID
	err := sharer.ShareTranscript(ctx, transcript, collective)
	var refusal *village.StatusError
	refused := errors.As(err, &refusal)
	duplicate := refused && refusal.StatusCode == http.StatusConflict
	if refused && refusal.StatusCode == http.StatusNotFound {
		step.Outcome = schema.SyncPushStepFailed
		step.Reason = missingTranscriptReason
		return step
	}
	if err != nil && !duplicate {
		step.Outcome = schema.SyncPushStepFailed
		step.Reason = "sharing with this collective failed: " + err.Error()
		return step
	}
	status, err := sharer.LatestShareStatus(ctx, transcript, collective)
	if err != nil {
		step.Outcome = schema.SyncPushStepFailed
		step.Reason = "Village answered the share, but reading back whether the collective accepted it failed: " + err.Error() + "; open the transcript on Village to see who can read it, and publish again to retry"
		return step
	}
	switch {
	case duplicate && status == schema.VillageShareStatusApproved:
		step.Outcome = schema.SyncPushStepSkipped
		step.Reason = "the collective already shares this transcript"
	case duplicate && status == schema.VillageShareStatusPending:
		step.Outcome = schema.SyncPushStepSkipped
		step.Reason = "the collective already holds this transcript for its owner's approval"
	case duplicate:
		step.Outcome = schema.SyncPushStepFailed
		step.Reason = "Village refused the share, so this collective cannot read the transcript: " + refusal.Message
	case status == schema.VillageShareStatusApproved:
		step.Outcome = schema.SyncPushStepSucceeded
	case status == schema.VillageShareStatusPending:
		step.Outcome = schema.SyncPushStepPendingApproval
	default:
		step.Outcome = schema.SyncPushStepSkipped
		step.Reason = "Village did not share the transcript with this collective: it shares only with a collective you are a member of that accepts your contributions"
	}
	return step
}

// stoppedMessage says where a session's steps stopped and what Village kept.
func stoppedMessage(stopped schema.SyncPushStepResult, outcome contentOutcome, steps []schema.SyncPushStepResult, retry string) string {
	var b strings.Builder
	switch stopped.Step {
	case schema.SyncPushStepContent:
		b.WriteString("stopped at sending the content: ")
	case schema.SyncPushStepRemoveCollective:
		fmt.Fprintf(&b, "stopped at taking the transcript back from collective %s: ", *stopped.CollectiveID)
	default:
		fmt.Fprintf(&b, "stopped at sharing with collective %s: ", *stopped.CollectiveID)
	}
	b.WriteString(stopped.Reason)
	if stopped.Reason == missingTranscriptReason {
		// The receipt names a transcript Village no longer holds, so what the
		// receipt says Village kept is not true, and the reason names the
		// recovery.
		b.WriteString(".")
		return b.String()
	}
	var applied, skippedLater []string
	for _, step := range steps {
		if step.CollectiveID == nil {
			continue
		}
		switch step.Outcome {
		case schema.SyncPushStepSucceeded, schema.SyncPushStepPendingApproval:
			applied = append(applied, describeStep(step))
		case schema.SyncPushStepNotAttempted:
			skippedLater = append(skippedLater, describeStep(step))
		}
	}
	// What Village kept is said from this computer's receipt. Without one,
	// Village may still hold content it accepted just before the stop.
	if outcome.transcript == nil {
		b.WriteString(". Village kept no transcript of this session that this computer has a receipt for; if Village accepted the content before the stop, publishing again records it")
	} else {
		fmt.Fprintf(&b, ". Village kept the transcript at %s", outcome.transcript.Receipt.TranscriptURL)
		if stopped.Step == schema.SyncPushStepContent {
			b.WriteString(" with the content it had before")
			if len(applied) == 0 {
				b.WriteString(" and the same readers")
			}
		}
	}
	if len(applied) > 0 {
		b.WriteString("; applied: " + strings.Join(applied, ", "))
	}
	if len(skippedLater) > 0 {
		b.WriteString("; not attempted: " + strings.Join(skippedLater, ", "))
	}
	b.WriteString(". " + retry)
	return b.String()
}

func describeStep(step schema.SyncPushStepResult) string {
	verb := "share with"
	if step.Step == schema.SyncPushStepRemoveCollective {
		verb = "take back from"
	}
	suffix := ""
	if step.Outcome == schema.SyncPushStepPendingApproval {
		suffix = " (waits for approval)"
	}
	return fmt.Sprintf("%s %s%s", verb, *step.CollectiveID, suffix)
}

func errorText(err error) string {
	if err == nil {
		return "no reason was recorded"
	}
	return err.Error()
}
