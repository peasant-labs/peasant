package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/schema"
)

// Resolution is the choice a user made for one conflict row: keep the draft's
// value (mine) or keep the file's (theirs). The set is the contract's.
//
// Seen is the file value the user was shown when they chose. When it is set,
// the choice applies only while the file still holds that value: a file that
// moved again after the choice raises the row again with the new value, so a
// choice is never applied to a value its owner did not see. A session records
// Seen from the rows it served.
type Resolution struct {
	Choice schema.LocalKickstartResolution
	Seen   schema.LocalSettingValue
}

// Resolutions maps a field to the choice made for its conflict row.
type Resolutions map[FieldKey]Resolution

// applies reports whether the choice still stands against the file's value now.
func (r Resolution) applies(theirs schema.LocalSettingValue) bool {
	return r.Choice.IsValid() && (len(r.Seen) == 0 || sameSettingValue(r.Seen, theirs))
}

// RetentionWriter writes Claude Code's cleanupPeriodDays. The Claude settings
// writer is injected so the model stays free of the filesystem layout.
type RetentionWriter func(days int) error

// RetentionStatus is what happened to the Claude retention preference during a
// commit.
type RetentionStatus uint8

const (
	RetentionUnknown RetentionStatus = iota
	// RetentionNotRequested means no retention choice was answered and visible.
	RetentionNotRequested
	// RetentionApplied means the writer stored the chosen period.
	RetentionApplied
	// RetentionFailed means the configuration was saved and the writer refused.
	RetentionFailed
)

// AllRetentionStatuses is the closed set, excluding the unknown zero value.
var AllRetentionStatuses = []RetentionStatus{RetentionNotRequested, RetentionApplied, RetentionFailed}

// IsValid reports whether s is a real status.
func (s RetentionStatus) IsValid() bool { return slices.Contains(AllRetentionStatuses, s) }

// String returns a stable name, or "unknown".
func (s RetentionStatus) String() string {
	switch s {
	case RetentionNotRequested:
		return "not-requested"
	case RetentionApplied:
		return "applied"
	case RetentionFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// RetentionOutcome reports the retention write. Note is set only on failure and
// is the sentence a presentation shows beside the saved configuration.
type RetentionOutcome struct {
	Status RetentionStatus
	Days   int
	Note   string
}

// CommitHooks are the effects that follow the configuration swap. The commit
// runs them in the order they are listed, while it still holds the lock, so a
// caller cannot reorder them or run one before the file is saved.
type CommitHooks struct {
	// Retention writes the Claude retention period after the configuration is
	// saved. It is called only when a visible retention answer chose a period.
	// A failure never retracts the save; it is reported in the result.
	Retention RetentionWriter
	// Publish hands the saved configuration to the running server. It is called
	// after retention, with the configuration the saved bytes parse to.
	Publish func(*config.Config)
}

// CommitInput is everything a commit or a review needs besides the draft.
type CommitInput struct {
	// Model is the setup model the answers were collected against.
	Model Model
	// Answers are the user's answers. Keys not answered are never written.
	Answers Answers
	// Resolutions are the choices made for conflict rows.
	Resolutions Resolutions
	// Context carries the session facts a write, a validator and a condition
	// read: forest, residual, scan state, connection and Claude presence. The
	// chain sets Config and Answers itself.
	Context WriteContext
	// LockDeadline bounds the wait for the configuration lock. It must be set.
	LockDeadline time.Time
	// Hooks are the effects that follow the swap.
	Hooks CommitHooks
}

// CommitResult is the outcome of a commit that did not fail.
//
// Saved is true when the configuration was written. When it is false,
// Conflicts holds the rows that still need a choice and nothing was written.
type CommitResult struct {
	Saved      bool
	ConfigPath string
	// Conflicts are the unresolved mine/theirs rows of a refused commit.
	Conflicts []schema.LocalKickstartConflict
	// Config is the saved configuration as the saved bytes parse, which is what
	// Publish received.
	Config    *config.Config
	Retention RetentionOutcome
	// ReleaseErr reports a failure to release the lock after the save. The save
	// stands; the lock is released when the process exits.
	ReleaseErr error
}

// CommitFailure is why a commit failed and wrote nothing.
type CommitFailure uint8

const (
	CommitFailureUnknown CommitFailure = iota
	// CommitFailureLock means the configuration lock was not taken in time.
	CommitFailureLock
	// CommitFailureRead means the file on disk could not be read, holds more than
	// one document, or does not parse.
	CommitFailureRead
	// CommitFailureInvalid means the answers or the wiring were refused before
	// any write.
	CommitFailureInvalid
	// CommitFailureChanged means the file changed again after the commit
	// recomputed against it once.
	CommitFailureChanged
	// CommitFailureWrite means the atomic replacement failed.
	CommitFailureWrite
)

// AllCommitFailures is the closed set, excluding the unknown zero value.
var AllCommitFailures = []CommitFailure{
	CommitFailureLock,
	CommitFailureRead,
	CommitFailureInvalid,
	CommitFailureChanged,
	CommitFailureWrite,
}

// IsValid reports whether f is a real failure kind.
func (f CommitFailure) IsValid() bool { return slices.Contains(AllCommitFailures, f) }

// String returns a stable name, or "unknown".
func (f CommitFailure) String() string {
	switch f {
	case CommitFailureLock:
		return "lock"
	case CommitFailureRead:
		return "read"
	case CommitFailureInvalid:
		return "invalid"
	case CommitFailureChanged:
		return "changed"
	case CommitFailureWrite:
		return "write"
	default:
		return "unknown"
	}
}

// CommitError is a failed commit. Kind says which refusal it was, so a caller
// maps it to a response without reading the message.
type CommitError struct {
	Kind CommitFailure
	Err  error
}

func (e *CommitError) Error() string { return e.Err.Error() }
func (e *CommitError) Unwrap() error { return e.Err }

func commitFailure(kind CommitFailure, err error) *CommitError {
	return &CommitError{Kind: kind, Err: err}
}

// Commit saves the answers into the configuration file in one chain under one
// lock, and returns once the lock is released:
//
//	lock -> read -> compare the answered fields against the baseline
//	     -> refuse with rows, or apply the answers -> validate -> swap
//	     -> retention -> publish -> release
//
// The comparison looks only at answered fields. A field the user did not answer
// is never written, never shown, and a change to it on disk survives untouched.
// An answered field whose file value moved since the draft opened, and that no
// resolution settles, is returned as a conflict row; nothing is written. When
// the swap finds the file changed it recomputes once against the fresh bytes
// under the same lock, and either returns rows or saves; a second change fails
// with CommitFailureChanged.
//
// The lock is released on every exit: a refusal with rows, a parse error, a
// validation error, a swap refusal and a success. A caller starts anything that
// reads the saved file after Commit returns.
//
// The draft's baseline must be the configuration the file parsed to when the
// draft opened, not a converted copy: a converted selection is a working edit,
// and a converted baseline reads as a change the user never made.
func (d *Draft) Commit(ctx context.Context, in CommitInput) (result CommitResult, err error) {
	if err := d.checkCommit(in); err != nil {
		return CommitResult{}, err
	}
	handle, err := config.OpenConfigFile(d.path, in.LockDeadline)
	if err != nil {
		return CommitResult{}, commitFailure(CommitFailureLock, err)
	}
	defer func() {
		if closeErr := handle.Close(); closeErr != nil {
			if result.Saved {
				result.ReleaseErr = closeErr
				return
			}
			err = errors.Join(err, closeErr)
		}
	}()

	data, doc, cfg, err := handle.Read(ctx)
	if err != nil {
		return CommitResult{}, commitFailure(CommitFailureRead, err)
	}
	snap := snapshot{data: data, doc: doc, cfg: cfg}
	for attempt := 0; ; attempt++ {
		p, err := d.plan(in.Model, in.Answers, in.Resolutions, in.Context, snap, true)
		if err != nil {
			return CommitResult{}, err
		}
		if unresolved := p.unresolvedRows(); len(unresolved) > 0 {
			return CommitResult{ConfigPath: d.path, Conflicts: unresolved}, nil
		}
		if p.retentionDays > 0 && in.Hooks.Retention == nil {
			return CommitResult{}, commitFailure(CommitFailureInvalid, actionable(
				"save the setup answers",
				fmt.Sprintf("a retention period of %d days was chosen and no retention writer was wired", p.retentionDays),
				"onboarding.Draft.Commit",
				"before the configuration was written",
				"nothing was written, so the saved file and the retention preference stay in step",
				"set CommitHooks.Retention to the Claude settings writer"))
		}
		swapErr := handle.Swap(snap.data, p.out)
		if swapErr == nil {
			return d.finish(in, p), nil
		}
		var changed *config.ConfigChangedError
		if !errors.As(swapErr, &changed) {
			return CommitResult{}, commitFailure(CommitFailureWrite, swapErr)
		}
		if attempt > 0 {
			return CommitResult{}, commitFailure(CommitFailureChanged, actionable(
				"save the setup answers",
				"the configuration file changed again while the save was recomputed against it",
				"onboarding.Draft.Commit",
				"at the second atomic replacement",
				"nothing was written",
				"save again; if another program keeps rewriting the file, stop it first"))
		}
		doc, cfg, err := config.ParseDocument(changed.Fresh)
		if err != nil {
			return CommitResult{}, commitFailure(CommitFailureRead, err)
		}
		snap = snapshot{data: changed.Fresh, doc: doc, cfg: cfg}
	}
}

// finish runs the effects that follow a successful swap, in their fixed order.
func (d *Draft) finish(in CommitInput, p *commitPlan) CommitResult {
	result := CommitResult{Saved: true, ConfigPath: d.path, Config: p.saved, Retention: RetentionOutcome{Status: RetentionNotRequested}}
	if p.retentionDays > 0 {
		result.Retention = RetentionOutcome{Status: RetentionApplied, Days: p.retentionDays}
		if err := in.Hooks.Retention(p.retentionDays); err != nil {
			result.Retention.Status = RetentionFailed
			result.Retention.Note = fmt.Sprintf("config saved, retention not applied: %v", err)
		}
	}
	in.Hooks.Publish(p.saved)
	return result
}

func (d *Draft) checkCommit(in CommitInput) error {
	refuse := func(why, fix string) error {
		return commitFailure(CommitFailureInvalid, actionable(
			"save the setup answers",
			why,
			"onboarding.Draft.Commit",
			"before the configuration lock was taken",
			"nothing was written",
			fix))
	}
	switch {
	case d == nil || d.path == "" || d.path == memoryDraftPath:
		return refuse("the draft has no file destination", "open the draft with onboarding.OpenDraft and the resolved config path")
	case len(in.Model.Stops) == 0:
		return refuse("the setup model has no stops", "pass the model from onboarding.BuildModel")
	case in.Hooks.Publish == nil:
		return refuse("no publish hook was wired", "set CommitHooks.Publish so the running server sees the saved configuration")
	case in.LockDeadline.IsZero():
		return refuse("no lock deadline was set", "set CommitInput.LockDeadline to a short bound; a zero deadline would wait minutes")
	}
	return checkInputs(in.Model, in.Answers, in.Resolutions, refuse)
}

func checkInputs(model Model, answers Answers, resolutions Resolutions, refuse func(why, fix string) error) error {
	for key := range answers {
		if _, ok := model.Field(key); !ok {
			return refuse(
				fmt.Sprintf("the answer %q is not a field of the %s setup", key, model.Scope),
				"send answers only for fields the session's model declares")
		}
	}
	for key, resolution := range resolutions {
		if _, ok := model.Field(key); !ok || !resolution.Choice.IsValid() {
			return refuse(
				fmt.Sprintf("the resolution for %q names an unknown field or a choice outside mine and theirs", key),
				"resolve a conflict row with mine or theirs")
		}
	}
	return nil
}

// ReviewResult is the preview of a save: the conflict rows and the structured
// diff of what the file would become, computed with the same chain a commit runs.
type ReviewResult struct {
	// Conflicts are every answered field whose file value moved, resolved or not.
	Conflicts []schema.LocalKickstartConflict
	// Unresolved are the fields among Conflicts that still need a choice.
	Unresolved []FieldKey
	// Diff is the file as it is now against the file as it would be saved, with
	// every unresolved row kept as the user's value.
	Diff schema.LocalKickstartDiff
}

// ReviewInput is a review's inputs. Hooks and the lock deadline are not part of
// a review: it writes nothing and takes no lock. Zero Limits mean
// DefaultDiffLimits.
type ReviewInput struct {
	Model       Model
	Answers     Answers
	Resolutions Resolutions
	Context     WriteContext
	Limits      DiffLimits
}

// Review reads the configuration file without locking it and runs the commit
// chain up to, not including, the swap. It does not validate the visible fields:
// a review is shown before the answers are complete. The file is read again,
// under the lock, when the commit runs.
func (d *Draft) Review(ctx context.Context, in ReviewInput) (ReviewResult, error) {
	refuse := func(why, fix string) error {
		return commitFailure(CommitFailureInvalid, actionable(
			"review the setup answers",
			why,
			"onboarding.Draft.Review",
			"before the configuration file was read",
			"nothing was read or written",
			fix))
	}
	switch {
	case d == nil || d.path == "" || d.path == memoryDraftPath:
		return ReviewResult{}, refuse("the draft has no file destination", "open the draft with onboarding.OpenDraft and the resolved config path")
	case len(in.Model.Stops) == 0:
		return ReviewResult{}, refuse("the setup model has no stops", "pass the model from onboarding.BuildModel")
	}
	if err := checkInputs(in.Model, in.Answers, in.Resolutions, refuse); err != nil {
		return ReviewResult{}, err
	}
	data, doc, cfg, err := config.ReadDocumentFile(ctx, d.path)
	if err != nil {
		return ReviewResult{}, commitFailure(CommitFailureRead, err)
	}
	p, err := d.plan(in.Model, in.Answers, in.Resolutions, in.Context, snapshot{data: data, doc: doc, cfg: cfg}, false)
	if err != nil {
		return ReviewResult{}, err
	}
	limits := in.Limits
	if limits == (DiffLimits{}) {
		limits = DefaultDiffLimits()
	}
	hunks, truncated, err := DiffDocuments(data, p.out, limits)
	if err != nil {
		return ReviewResult{}, commitFailure(CommitFailureInvalid, err)
	}
	review := ReviewResult{
		Conflicts: []schema.LocalKickstartConflict{},
		Diff:      schema.LocalKickstartDiff{File: filepath.Base(d.path), Hunks: hunks, Truncated: truncated},
	}
	for _, row := range p.rows {
		review.Conflicts = append(review.Conflicts, row.wire)
		if !row.settled {
			review.Unresolved = append(review.Unresolved, row.key)
		}
	}
	return review, nil
}

// snapshot is the file as one chain pass reads it. doc is consumed by the pass:
// the answers are projected into it.
type snapshot struct {
	data []byte
	doc  *yaml.Node
	cfg  *config.Config
}

type conflictRow struct {
	key  FieldKey
	wire schema.LocalKickstartConflict
	// settled is true when a resolution that still stands covers the row.
	settled bool
	choice  schema.LocalKickstartResolution
}

// commitPlan is one pass of the chain: the answers that will be written, the
// rows found, and the document they produce.
type commitPlan struct {
	rows          []conflictRow
	out           []byte
	saved         *config.Config
	retentionDays int
}

func (p *commitPlan) unresolvedRows() []schema.LocalKickstartConflict {
	var rows []schema.LocalKickstartConflict
	for _, row := range p.rows {
		if !row.settled {
			rows = append(rows, row.wire)
		}
	}
	return rows
}

// plan runs the chain once over snap: hide what is hidden, find the moved
// fields, drop the answers a "theirs" choice gives up, apply the rest through
// ApplyAnswers, project them into the document and parse the result. validate
// adds the visible fields' validators, which a commit needs and a review does
// not.
func (d *Draft) plan(model Model, input Answers, resolutions Resolutions, w WriteContext, snap snapshot, validate bool) (*commitPlan, error) {
	base := snap.cfg
	if snap.data == nil {
		// No file now: the session's starting configuration stands in for it.
		// That keeps the defaults the session opened with, such as the email
		// git supplied, exactly as the terminal's first save does.
		copied, err := cloneConfig(&d.baseline)
		if err != nil {
			return nil, commitFailure(CommitFailureInvalid, err)
		}
		base = &copied
	}
	answers := copyAnswers(input)
	var rows []conflictRow
	for pass := 0; ; pass++ {
		if pass > len(model.Fields()) {
			return nil, commitFailure(CommitFailureInvalid, actionable(
				"find the moved fields",
				"giving up one answer kept revealing another past the field count",
				"onboarding.Draft.plan",
				"while applying the resolutions",
				"nothing was written",
				"report this defect"))
		}
		w.Answers = answers
		kept, err := model.DropHidden(base, answers, w)
		if err != nil {
			return nil, commitFailure(CommitFailureInvalid, err)
		}
		answers = kept
		w.Answers = answers
		mine, err := model.DiffValues(base, answers, w)
		if err != nil {
			return nil, commitFailure(CommitFailureInvalid, err)
		}
		if rows, err = d.conflictRows(model, base, answers, mine, resolutions); err != nil {
			return nil, commitFailure(CommitFailureInvalid, err)
		}
		gaveUp := false
		for _, row := range rows {
			if row.settled && row.choice == schema.LocalKickstartResolutionTheirs {
				delete(answers, row.key)
				gaveUp = true
			}
		}
		if !gaveUp {
			break
		}
	}

	working, err := cloneConfig(base)
	if err != nil {
		return nil, commitFailure(CommitFailureInvalid, err)
	}
	w.Config, w.Answers = &working, answers
	if err := model.ApplyAnswers(&working, answers, w); err != nil {
		return nil, commitFailure(CommitFailureInvalid, err)
	}
	if validate {
		if err := model.Validate(w); err != nil {
			return nil, commitFailure(CommitFailureInvalid, err)
		}
	}
	out, err := d.document(model, snap, &working, answers)
	if err != nil {
		return nil, commitFailure(CommitFailureInvalid, err)
	}
	saved, err := config.Parse(out)
	if err != nil {
		return nil, commitFailure(CommitFailureInvalid, actionable(
			"save the setup answers",
			err.Error(),
			"onboarding.Draft.plan",
			"after the answers were applied and before the swap",
			"nothing was written",
			"correct the answer the message names and save again"))
	}
	plan := &commitPlan{rows: rows, out: out, saved: saved}
	if _, answered := answers[FieldRetention]; answered && working.ClaudeRetentionDays > 0 {
		plan.retentionDays = working.ClaudeRetentionDays
	}
	return plan, nil
}

// conflictRows compares every answered configuration field against the file and
// the baseline:
//
//	theirs == baseline  -> no row (the file did not move)
//	theirs == mine      -> no row (someone landed the same choice)
//	otherwise           -> a row
//
// Fields stored outside config.yaml, and fields that store nothing, never
// conflict with the file. Rows follow declaration order.
func (d *Draft) conflictRows(model Model, base *config.Config, answers Answers, mine map[FieldKey]schema.LocalSettingValue, resolutions Resolutions) ([]conflictRow, error) {
	var rows []conflictRow
	for _, field := range model.Fields() {
		key := field.Key()
		if _, answered := answers[key]; !answered || field.Destination() != DestConfig {
			continue
		}
		theirs, err := field.Read(base)
		if err != nil {
			return nil, err
		}
		baseline, err := field.Read(&d.baseline)
		if err != nil {
			return nil, err
		}
		if sameSettingValue(theirs, baseline) || sameSettingValue(theirs, mine[key]) {
			continue
		}
		stop, label := fieldPlace(model, field)
		row := conflictRow{key: key, wire: schema.LocalKickstartConflict{
			Field:  key.String(),
			Stop:   stop.String(),
			Label:  label,
			Mine:   mine[key],
			Theirs: theirs,
		}}
		if resolution, ok := resolutions[key]; ok && resolution.applies(theirs) {
			row.settled, row.choice = true, resolution.Choice
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// fieldPlace names the stop a field is on and the label its row carries. A field
// with no label of its own (the selection tree) takes its stop's title.
func fieldPlace(model Model, field Field) (FlowKey, string) {
	for _, stop := range model.Stops {
		for _, candidate := range stop.Fields {
			if candidate == nil || candidate.Key() != field.Key() {
				continue
			}
			label := strings.TrimSpace(field.Label())
			if label == "" {
				label = stop.Title
			}
			if label == "" {
				label = field.Key().String()
			}
			return stop.Key, label
		}
	}
	return "", field.Key().String()
}

// document builds the bytes the save would write. A file that is absent now is
// written whole, from the working configuration, as the terminal's first save
// writes it. A file that exists is edited in place: each answered path is set in
// its own document, so the keys the file does not name stay unnamed and its
// comments and key order survive.
func (d *Draft) document(model Model, snap snapshot, working *config.Config, answers Answers) ([]byte, error) {
	if snap.data == nil {
		out, err := yaml.Marshal(working)
		if err != nil {
			return nil, fmt.Errorf("marshal the first-run configuration: %w", err)
		}
		return out, nil
	}
	keys := make([]FieldKey, 0, len(answers))
	for key := range answers {
		keys = append(keys, key)
	}
	if err := model.ProjectAnswered(snap.doc, working, keys); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(documentIndent(snap.data))
	if err := encoder.Encode(snap.doc); err != nil {
		return nil, fmt.Errorf("encode the edited configuration document: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encode the edited configuration document: %w", err)
	}
	return buf.Bytes(), nil
}

// documentIndent is the indentation the file already uses, so an edit does not
// re-indent lines it did not touch. A file with no nested block gets the
// default four spaces the terminal's first save writes.
func documentIndent(data []byte) int {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if width := len(line) - len(trimmed); width >= 2 && width <= 8 {
			return width
		}
	}
	return 4
}

// sameSettingValue compares two setting values as JSON, ignoring whitespace.
func sameSettingValue(a, b schema.LocalSettingValue) bool {
	var left, right bytes.Buffer
	if json.Compact(&left, a) != nil || json.Compact(&right, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(left.Bytes(), right.Bytes())
}
