package onboarding

import (
	"bytes"
	"context"
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
	return r.Choice.IsValid() && (len(r.Seen) == 0 || SameSettingValue(r.Seen, theirs))
}

// RetentionWriter writes Claude Code's cleanupPeriodDays. The Claude settings
// writer is injected so the model stays free of the filesystem layout.
type RetentionWriter func(days int) error

// RetentionStatus is what happened to the Claude retention preference during a
// commit.
type RetentionStatus uint8

const (
	RetentionUnknown RetentionStatus = iota
	// RetentionNotRequested means no retention period was shown, so none was
	// written.
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
	// saved. It is called when the retention field is shown and holds a period,
	// the period the user chose or, when they left it alone, the period they were
	// shown (Model.RetentionDays). A failure never retracts the save; it is
	// reported in the result.
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
	Retention RetentionOutcome
	// ReleaseErr reports a failure to release the lock after the save. The save
	// stands; the lock is released when the process exits. A caller reports it,
	// because the next writer waits for the lock until then.
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
// The comparison looks at the fields the save writes: the answered ones and the
// ones an answer moves as a side effect (publishing automatically sets the share
// preference). A field the save does not write is never shown, and a change to
// it on disk survives untouched. A written field whose file value moved since
// the draft opened, and that no resolution settles, is returned as a conflict
// row; nothing is written. When
// the swap finds the file changed it recomputes once against the fresh bytes
// under the same lock, and either returns rows or saves; a second change fails
// with CommitFailureChanged.
//
// The lock is released on every exit: a refusal with rows, a parse error, a
// validation error, a swap refusal and a success. A caller starts anything that
// reads the saved file after Commit returns.
//
// A save with nothing to change leaves the file byte for byte as it is and still
// runs the effects. A Draft is not safe for concurrent use: the session that
// owns it serialises its calls.
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
		if in.Model.Scope == ScopeSelection && snap.data == nil {
			// The selection page edits a configuration that setup wrote. With no
			// file, saving would write the whole default document, which ends
			// first-run setup without the user having answered it.
			return CommitResult{}, commitFailure(CommitFailureInvalid, actionable(
				"save the selection",
				"there is no configuration file yet, and the selection page only edits one that setup wrote",
				"onboarding.Draft.Commit",
				"after the configuration lock was taken and the file was read",
				"nothing was written, and first-run setup is still ahead of you",
				"run setup first at /kickstart, then choose the sessions to import"))
		}
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
		if snap.data != nil && bytes.Equal(p.out, snap.data) {
			// Nothing to change: leave the file byte for byte as it is.
			return d.finish(in, p), nil
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
	result := CommitResult{Saved: true, ConfigPath: d.path, Retention: RetentionOutcome{Status: RetentionNotRequested}}
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
	// Saved is the configuration the save would produce: what the bytes it would
	// write parse to, plus the Claude retention period it would apply, which
	// config.yaml does not hold. It starts from the file as it is now and carries
	// the choices that stand ("theirs" gives an answer up), so a presentation
	// explains the save from this value and never from the draft's snapshot of
	// the file at open time.
	Saved *config.Config
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
		Saved:     p.shown,
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
	// coupled is true when the user did not answer the field: another answer
	// moves it as a side effect, so "theirs" keeps the file's value in place
	// rather than giving an answer up.
	coupled bool
}

// commitPlan is one pass of the chain: the answers that will be written, the
// rows found, and the document they produce.
type commitPlan struct {
	rows []conflictRow
	out  []byte
	// saved is the configuration the bytes in out parse to, which is what the
	// running server receives.
	saved *config.Config
	// shown is saved plus the retention period the save applies, for the review.
	shown         *config.Config
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
	givenUp := map[FieldKey]conflictRow{}
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
		applied, err := model.AppliedCopy(base, answers, w)
		if err != nil {
			return nil, commitFailure(CommitFailureInvalid, err)
		}
		if rows, err = d.conflictRows(model, base, applied, answers, resolutions); err != nil {
			return nil, commitFailure(CommitFailureInvalid, err)
		}
		gaveUp := false
		for _, row := range rows {
			if row.settled && row.choice == schema.LocalKickstartResolutionTheirs && !row.coupled {
				givenUp[row.key] = row
				delete(answers, row.key)
				gaveUp = true
			}
		}
		if !gaveUp {
			break
		}
	}
	// A row whose answer was given up is no longer an answered field, yet it stays
	// listed so the user can still see it and change the choice.
	for _, row := range givenUp {
		if !hasRow(rows, row.key) {
			rows = append(rows, row)
		}
	}
	order := map[FieldKey]int{}
	for i, field := range model.Fields() {
		order[field.Key()] = i
	}
	slices.SortFunc(rows, func(a, b conflictRow) int { return order[a.key] - order[b.key] })

	working, err := cloneConfig(base)
	if err != nil {
		return nil, commitFailure(CommitFailureInvalid, err)
	}
	// A transient value, such as the retention period the session showed, is not
	// in the file: it comes from the draft, as it does when the terminal's draft
	// is rebuilt.
	for _, seed := range d.seeds {
		seed(&working)
	}
	w.Config, w.Answers = &working, answers
	if err := model.ApplyAnswers(&working, answers, w); err != nil {
		return nil, commitFailure(CommitFailureInvalid, err)
	}
	// "Theirs" on a field the user did not answer keeps the file's value where an
	// answer's side effect would have replaced it.
	for _, row := range rows {
		if !row.coupled || !row.settled || row.choice != schema.LocalKickstartResolutionTheirs {
			continue
		}
		field, _ := model.Field(row.key)
		if err := restoreConfigValueAtPath(&working, base, field.YAMLPath()); err != nil {
			return nil, commitFailure(CommitFailureInvalid, err)
		}
	}
	if validate {
		if err := model.Validate(w); err != nil {
			return nil, commitFailure(CommitFailureInvalid, err)
		}
	}
	out, err := d.document(model, snap, base, &working, answers)
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
	shown := *saved
	shown.ClaudeRetentionDays = working.ClaudeRetentionDays
	return &commitPlan{rows: rows, out: out, saved: saved, shown: &shown, retentionDays: model.RetentionDays(w)}, nil
}

func hasRow(rows []conflictRow, key FieldKey) bool {
	return slices.ContainsFunc(rows, func(row conflictRow) bool { return row.key == key })
}

// conflictRows compares every configuration field the save writes against the
// file and the baseline. applied is the file with the answers applied, so it
// holds the value the save would write for each field, an answered one or one an
// answer moves as a side effect:
//
//	theirs == baseline  -> no row (the file did not move)
//	theirs == mine      -> no row (someone landed the same choice, or the
//	                       save does not write the field at all)
//	otherwise           -> a row
//
// A field the user did not answer is a row only when an answer moves it, so a
// change to a field the save leaves alone never raises one. Fields stored
// outside config.yaml, and fields that store nothing, never conflict with the
// file. Rows follow declaration order.
func (d *Draft) conflictRows(model Model, base, applied *config.Config, answers Answers, resolutions Resolutions) ([]conflictRow, error) {
	var rows []conflictRow
	for _, field := range model.Fields() {
		key := field.Key()
		if field.Destination() != DestConfig {
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
		mine, err := field.Read(applied)
		if err != nil {
			return nil, err
		}
		if SameSettingValue(theirs, baseline) || SameSettingValue(theirs, mine) {
			continue
		}
		stop, label := fieldPlace(model, field)
		_, answered := answers[key]
		row := conflictRow{key: key, coupled: !answered, wire: schema.LocalKickstartConflict{
			Field:  key.String(),
			Stop:   stop.String(),
			Label:  label,
			Mine:   mine,
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
// writes it. A file that exists is edited in place: each answered path, and each
// path the answers moved, is set in its own document, so the keys the file does
// not name stay unnamed and its comments, key order, blank lines and comment
// spacing survive.
func (d *Draft) document(model Model, snap snapshot, base, working *config.Config, answers Answers) ([]byte, error) {
	if snap.data == nil {
		out, err := yaml.Marshal(working)
		if err != nil {
			return nil, fmt.Errorf("marshal the first-run configuration: %w", err)
		}
		return out, nil
	}
	if len(answers) == 0 {
		// No answer to write: the file stays exactly as it is, including the
		// spacing and blank lines a re-encoding would normalise.
		return snap.data, nil
	}
	keys := make([]FieldKey, 0, len(answers))
	for key := range answers {
		keys = append(keys, key)
	}
	if err := model.ProjectEdits(snap.doc, base, working, keys); err != nil {
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
	return restoreDocumentFormat(snap.data, buf.Bytes()), nil
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
