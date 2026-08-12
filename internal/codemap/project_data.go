package codemap

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/schema"
)

// projectData is the per-request snapshot of a project's recorded activity.
// It is loaded once per Service call and threaded through the aggregation
// helpers — the only caching in codemap (in-memory per-request
// caching only).
type projectData struct {
	hash schema.ProjectHash
	cwd  string // projects.canonical_cwd ("" when unknown)

	sessions    []sessionRow // newest first
	sessionByID map[string]sessionRow
	metrics     map[string]metricRow // sessionID -> metrics subset
	commits     []commitRow          // all session-linked commits
	commitsByID map[string][]commitRow
	ledger      []commitRow         // append-only observed relationships, including no-longer-current commits
	recorded    map[string]bool     // commit hash -> a recorded session links it
	labels      map[string][]string // sessionID -> effective annotation values

	tasks      []taskData          // all tasks, sessions' (newest-first) order
	editedByID map[string][]string // sessionID -> sorted distinct edited files
	tasksByID  map[string][]taskData
}

// loadProjectData snapshots everything codemap needs from the store.
// Unknown projectHash returns ErrProjectNotFound.
func (s *Service) loadProjectData(ctx context.Context, projectHash schema.ProjectHash) (*projectData, error) {
	cwd, _, found, err := s.queryProjectCwd(ctx, projectHash)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrProjectNotFound, projectHash)
	}

	sessions, err := s.querySessions(ctx, projectHash)
	if err != nil {
		return nil, err
	}
	visibleSessions := make([]sessionRow, 0, len(sessions))
	visibleIDs := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		visible, visibilityErr := s.visibility.Visible(sessionvisibility.Candidate{
			SessionID:   ingest.SessionID(session.id),
			Harness:     defaults.Harness(session.harness),
			GitRemote:   session.gitRemote,
			ProjectName: session.projectName,
			ClonePath:   s.resolveSessionClonePath(session.gitWorktree, session.projectName),
			GitBranch:   session.gitBranch,
		})
		if visibilityErr != nil {
			return nil, visibilityErr
		}
		if visible {
			visibleSessions = append(visibleSessions, session)
			visibleIDs[session.id] = true
		}
	}
	sessions = visibleSessions
	metrics, err := s.queryMetrics(ctx, projectHash)
	if err != nil {
		return nil, err
	}
	metrics = filterMetricRows(metrics, visibleIDs)
	commits, err := s.queryCommits(ctx, projectHash)
	if err != nil {
		return nil, err
	}
	commits = filterCommitRows(commits, visibleIDs)
	ledger, err := s.queryAssociationLedger(ctx, projectHash)
	if err != nil {
		return nil, err
	}
	ledger = filterCommitRows(ledger, visibleIDs)
	labels, err := s.queryEffectiveLabels(ctx, projectHash)
	if err != nil {
		return nil, err
	}
	for sessionID := range labels {
		if !visibleIDs[sessionID] {
			delete(labels, sessionID)
		}
	}
	tasks, err := s.loadTasks(ctx, cwd, sessions)
	if err != nil {
		return nil, err
	}

	pd := &projectData{
		hash:        projectHash,
		cwd:         cwd,
		sessions:    sessions,
		sessionByID: make(map[string]sessionRow, len(sessions)),
		metrics:     metrics,
		commits:     commits,
		commitsByID: make(map[string][]commitRow),
		ledger:      ledger,
		recorded:    make(map[string]bool),
		labels:      labels,
		tasks:       tasks,
		editedByID:  make(map[string][]string),
		tasksByID:   make(map[string][]taskData),
	}
	for _, sess := range sessions {
		pd.sessionByID[sess.id] = sess
	}
	for _, c := range commits {
		pd.commitsByID[c.sessionID] = append(pd.commitsByID[c.sessionID], c)
		pd.recorded[c.hash] = true
	}
	for _, t := range tasks {
		pd.tasksByID[t.sessionID] = append(pd.tasksByID[t.sessionID], t)
		merged := append(pd.editedByID[t.sessionID], t.editedFiles...)
		pd.editedByID[t.sessionID] = merged
	}
	for id, files := range pd.editedByID {
		pd.editedByID[id] = sortedDistinct(files)
	}
	return pd, nil
}

func filterMetricRows(rows map[string]metricRow, visibleIDs map[string]bool) map[string]metricRow {
	filtered := make(map[string]metricRow, len(visibleIDs))
	for sessionID, row := range rows {
		if visibleIDs[sessionID] {
			filtered[sessionID] = row
		}
	}
	return filtered
}

func filterCommitRows(rows []commitRow, visibleIDs map[string]bool) []commitRow {
	filtered := make([]commitRow, 0, len(rows))
	for _, row := range rows {
		if visibleIDs[row.sessionID] {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

// taskSummary maps a taskData to its wire form, attaching the session-level
// outcome and effective annotation labels.
func (pd *projectData) taskSummary(t taskData) schema.TaskSummary {
	summary := schema.NewTaskSummary(t.sessionID, t.entryIndex)
	summary.Title = pd.taskTitle(t)
	summary.StartMs = t.startMs
	summary.ReadCount = t.readCount
	summary.RetryLoop = t.retryLoop
	summary.EditedFiles = append(summary.EditedFiles, t.editedFiles...)
	if m, ok := pd.metrics[t.sessionID]; ok {
		summary.Outcome = m.outcome
	}
	summary.Labels = append(summary.Labels, informativeLabels(pd.labels[t.sessionID], summary.Outcome)...)
	return summary
}

// negativeSignalLabels are machine-annotator values that mean "the detector
// found nothing" — they carry no information worth a chip.
var negativeSignalLabels = map[string]bool{
	"not_detected": true,
	"unknown":      true,
	"none":         true,
}

// labelIsInformative reports whether an effective annotation value belongs on
// a task chip. Filtered out server-side (so every client stays clean):
//
//   - filesystem-path-looking values (start with '/' or '~', or contain
//     "/Users/") — cwd-style annotations are location metadata, not labels;
//   - the session outcome (already rendered as its own chip);
//   - negative-signal values ("not_detected", "unknown", "none").
//
// Genuinely informative values pass through: human annotations, scope values
// ("feature", "bug"), positive detector signals ("detected").
func labelIsInformative(value, outcome string) bool {
	v := strings.TrimSpace(value)
	if v == "" {
		return false
	}
	if strings.HasPrefix(v, "/") || strings.HasPrefix(v, "~") || strings.Contains(v, "/Users/") {
		return false
	}
	if strings.EqualFold(v, outcome) {
		return false
	}
	return !negativeSignalLabels[strings.ToLower(v)]
}

// informativeLabels filters a session's effective annotation values down to
// the chips worth showing, preserving input order.
func informativeLabels(values []string, outcome string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if labelIsInformative(v, outcome) {
			out = append(out, v)
		}
	}
	return out
}

// taskTitle hardens the task's primary label: a user-turn preview below the
// signal floor (fragments like "The file") falls back to the session's
// computed title metric, then the first edited filename, then
// "task @ <entryIndex>". Every arm keeps the <=80-char word-boundary
// truncation.
func (pd *projectData) taskTitle(t taskData) string {
	if titleHasSignal(t.title) {
		return t.title // already truncated by titleFromPreview
	}
	if m, ok := pd.metrics[t.sessionID]; ok && m.title != "" {
		return truncateTitle(m.title)
	}
	if len(t.editedFiles) > 0 {
		return truncateTitle(path.Base(t.editedFiles[0]))
	}
	return fmt.Sprintf("task @ %d", t.entryIndex)
}

// sessionTitle returns the session's display title: the computed title
// metric, falling back to the first task's title.
func (pd *projectData) sessionTitle(sessionID string) string {
	if m, ok := pd.metrics[sessionID]; ok && m.title != "" {
		return m.title
	}
	if tasks := pd.tasksByID[sessionID]; len(tasks) > 0 {
		return tasks[0].title // tasks are in entry order per session
	}
	return ""
}
