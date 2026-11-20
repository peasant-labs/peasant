//go:build skipbudget

package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

//go:embed testdata/arm_equivalence_allowlist.yaml
var armEquivalenceAllowlistYAML []byte

// sessionOutcome is what the index stage did with one session.
type sessionOutcome string

const (
	// outcomeSettled: the cheap proof held and the store settled.
	outcomeSettled sessionOutcome = "settled"
	// outcomeValidated: the cheap proof failed on a context term, the
	// validator proved the candidate, and the store settled.
	outcomeValidated sessionOutcome = "validated"
	// outcomeParsedUnsupported: a context mismatch with no candidate, or a
	// validator that could not answer.
	outcomeParsedUnsupported sessionOutcome = "parsed:validate-unsupported"
	// outcomeParsedNotProven: the validator answered not proven.
	outcomeParsedNotProven sessionOutcome = "parsed:validator-not-proven"
	// outcomeParsedSettleRefused: the validator proved it and the settle
	// refused under the lock.
	outcomeParsedSettleRefused sessionOutcome = "parsed:settle-refused"
	// outcomeParsedSkipsDisabled: no proof was attempted (the baseline arm).
	outcomeParsedSkipsDisabled sessionOutcome = "parsed:refresh-skips-disabled"
	// outcomeParsedTermPrefix prefixes a parse after a non-context term.
	outcomeParsedTermPrefix = "parsed:"
)

// sessionTrace is one session's path through the index stage of one run.
type sessionTrace struct {
	Session ingest.SessionID `json:"session"`
	Outcome sessionOutcome   `json:"outcome"`
	// ValidateMs is the metadata half plus the validator call.
	ValidateMs float64 `json:"validateMs"`
	ParseMs    float64 `json:"parseMs"`
	// SecondScanMs is set for a session that built a candidate and then
	// parsed: the measured metadata half, which BuildGeneration runs again.
	SecondScanMs float64 `json:"secondScanMs,omitempty"`
}

// outcomeRow is one outcome of the skip arm over every round.
type outcomeRow struct {
	Outcome            sessionOutcome `json:"outcome"`
	Sessions           int            `json:"sessions"`
	MedianValidateMs   float64        `json:"medianValidateMs"`
	MedianParseMs      float64        `json:"medianParseMs"`
	MedianSecondScanMs float64        `json:"medianSecondScanMs,omitempty"`
}

// runTrace collects, per session, what the index stage did in one run.
type runTrace struct {
	mu          sync.Mutex
	proof       map[ingest.SessionID]ingest.UnchangedProof
	proofErr    map[ingest.SessionID]bool
	half        map[ingest.SessionID]time.Duration
	halfOK      map[ingest.SessionID]bool
	validation  map[ingest.SessionID]ingest.ManagedGenerationValidationResult
	validatorMs map[ingest.SessionID]time.Duration
	validateErr map[ingest.SessionID]bool
	settle      map[ingest.SessionID]error
}

func newRunTrace() *runTrace {
	return &runTrace{
		proof: map[ingest.SessionID]ingest.UnchangedProof{}, proofErr: map[ingest.SessionID]bool{},
		half: map[ingest.SessionID]time.Duration{}, halfOK: map[ingest.SessionID]bool{},
		validation: map[ingest.SessionID]ingest.ManagedGenerationValidationResult{}, validatorMs: map[ingest.SessionID]time.Duration{},
		validateErr: map[ingest.SessionID]bool{}, settle: map[ingest.SessionID]error{},
	}
}

func (r *runTrace) proved(sid ingest.SessionID, proof ingest.UnchangedProof, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proof[sid], r.proofErr[sid] = proof, err != nil
}

func (r *runTrace) builtHalf(sid ingest.SessionID, took time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.half[sid], r.halfOK[sid] = took, err == nil
}

func (r *runTrace) validated(sid ingest.SessionID, result ingest.ManagedGenerationValidationResult, err error, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.validatorMs[sid] = took
	if err != nil {
		r.validateErr[sid] = true
		return
	}
	r.validation[sid] = result
}

func (r *runTrace) settled(sid ingest.SessionID, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settle[sid] = err
}

// count fills the executed counts the trace observed.
func (r *runTrace) count(e *executed, unsupported *int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ok := range r.halfOK {
		if ok {
			e.Candidates++
		}
	}
	e.Validations = len(r.validation)
	for _, result := range r.validation {
		switch result.Outcome {
		case ingest.ValidationProven:
			e.Validated++
		case ingest.ValidationUnsupported:
			*unsupported++
		}
	}
	for _, err := range r.settle {
		if err == nil {
			e.Settles++
		}
	}
	for sid, proof := range r.proof {
		if r.proofErr[sid] || proof.Proven || !proof.Mismatch.IsContext() {
			continue
		}
		if _, answered := r.validation[sid]; !answered {
			*unsupported++
		}
	}
}

// sessions classifies every session the run's index log names.
func (r *runTrace) sessions(result *ingest.PipelineResult) []sessionTrace {
	r.mu.Lock()
	defer r.mu.Unlock()
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	var traces []sessionTrace
	for _, entry := range result.IndexLog {
		sid := entry.SessionID
		trace := sessionTrace{Session: sid, ValidateMs: ms(r.half[sid] + r.validatorMs[sid])}
		parsed := entry.ParseDuration != nil && *entry.ParseDuration > 0
		if parsed {
			trace.ParseMs = ms(*entry.ParseDuration)
			if r.halfOK[sid] {
				trace.SecondScanMs = ms(r.half[sid])
			}
		}
		proof, proved := r.proof[sid]
		settleErr, settleRan := r.settle[sid]
		validation, answered := r.validation[sid]
		switch {
		case settleRan && settleErr == nil && proof.Proven:
			trace.Outcome = outcomeSettled
		case settleRan && settleErr == nil:
			trace.Outcome = outcomeValidated
		case !proved || r.proofErr[sid]:
			trace.Outcome = outcomeParsedSkipsDisabled
		case settleRan:
			trace.Outcome = outcomeParsedSettleRefused
		case !proof.Mismatch.IsContext():
			trace.Outcome = sessionOutcome(outcomeParsedTermPrefix + string(proof.Mismatch))
		case answered && validation.Outcome == ingest.ValidationNotProven:
			trace.Outcome = outcomeParsedNotProven
		default:
			trace.Outcome = outcomeParsedUnsupported
		}
		traces = append(traces, trace)
	}
	sort.Slice(traces, func(i, j int) bool { return traces[i].Session < traces[j].Session })
	return traces
}

// breakdown groups the skip arm's sessions by outcome over every sample.
func breakdown(samples []sample) []outcomeRow {
	type values struct{ validate, parse, second []float64 }
	grouped := map[sessionOutcome]*values{}
	for _, s := range samples {
		for _, trace := range s.Traces {
			v := grouped[trace.Outcome]
			if v == nil {
				v = &values{}
				grouped[trace.Outcome] = v
			}
			v.validate = append(v.validate, trace.ValidateMs)
			v.parse = append(v.parse, trace.ParseMs)
			if trace.SecondScanMs > 0 {
				v.second = append(v.second, trace.SecondScanMs)
			}
		}
	}
	median := func(values []float64) float64 {
		if len(values) == 0 {
			return 0
		}
		sorted := slices.Clone(values)
		slices.Sort(sorted)
		return sorted[(len(sorted)-1)/2]
	}
	var rows []outcomeRow
	for outcome, v := range grouped {
		rows = append(rows, outcomeRow{Outcome: outcome, Sessions: len(v.validate),
			MedianValidateMs: median(v.validate), MedianParseMs: median(v.parse), MedianSecondScanMs: median(v.second)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Outcome < rows[j].Outcome })
	return rows
}

// tracingClaudeIndexer is the registered Claude indexer, timing each
// metadata half the pipeline runs.
type tracingClaudeIndexer struct {
	*ingest.ClaudeIndexer
	trace *runTrace
}

var _ ingest.GenerationMetadataBuilder = tracingClaudeIndexer{}

func (i tracingClaudeIndexer) BuildGenerationMetadata(ctx context.Context, input ingest.GenerationBuildInput) (ingest.GenerationMetadataCandidate, error) {
	start := time.Now()
	candidate, err := i.ClaudeIndexer.BuildGenerationMetadata(ctx, input)
	i.trace.builtHalf(input.Session.SessionID, time.Since(start), err)
	return candidate, err
}

// forgeStoredMetadataHashes forges every active generation's stored
// metadata_hash to another non-zero value. The immutability trigger is lifted
// for the one update and restored from its own stored definition, and the
// restore is checked.
func forgeStoredMetadataHashes(ctx context.Context, db *store.Store) (int64, error) {
	conn, err := db.Pool().Take(ctx)
	if err != nil {
		return 0, err
	}
	defer db.Pool().Put(conn)
	readTrigger := func() (string, error) {
		var sql string
		err := sqlitex.Execute(conn, `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='session_generations_immutable'`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
			sql = st.ColumnText(0)
			return nil
		}})
		if err == nil && sql == "" {
			err = errors.New("the session_generations immutability trigger is missing")
		}
		return sql, err
	}
	trigger, err := readTrigger()
	if err != nil {
		return 0, fmt.Errorf("read the immutability trigger before forging the stored metadata hash: %w", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `DROP TRIGGER session_generations_immutable`, nil); err != nil {
		return 0, err
	}
	err = sqlitex.ExecuteTransient(conn, `UPDATE session_generations SET metadata_hash = ? WHERE generation_id IN (SELECT active_generation_id FROM sessions)`, &sqlitex.ExecOptions{Args: []any{strings.Repeat("f", 64)}})
	forged := int64(conn.Changes())
	if restoreErr := sqlitex.ExecuteTransient(conn, trigger, nil); restoreErr != nil {
		return 0, fmt.Errorf("restore the immutability trigger: %w", restoreErr)
	}
	if err != nil {
		return 0, fmt.Errorf("forge the stored metadata hash: %w", err)
	}
	if restored, err := readTrigger(); err != nil || restored != trigger {
		return 0, fmt.Errorf("the immutability trigger was not restored exactly: %v", err)
	}
	return forged, nil
}

// armEquivalenceAllowlist is the set of table.column names the
// arm-equivalence check leaves out, each with its reason.
func armEquivalenceAllowlist() (map[string]bool, error) {
	var fixture struct {
		Columns []struct {
			Column string `yaml:"column"`
			Reason string `yaml:"reason"`
		} `yaml:"columns"`
	}
	if err := testutil.DecodeFixtureYAML(armEquivalenceAllowlistYAML, &fixture); err != nil {
		return nil, fmt.Errorf("decode arm_equivalence_allowlist.yaml: %w", err)
	}
	allowed := map[string]bool{}
	for _, c := range fixture.Columns {
		if !strings.Contains(c.Column, ".") || len(strings.Fields(c.Reason)) < 3 {
			return nil, fmt.Errorf("arm_equivalence_allowlist.yaml: %q needs a table.column name and a reason", c.Column)
		}
		allowed[c.Column] = true
	}
	return allowed, nil
}

// armStateQueries reads each compared table for one session.
var armStateQueries = map[string]string{
	"sessions":               `SELECT * FROM sessions WHERE session_id = ?`,
	"session_metrics":        `SELECT * FROM session_metrics WHERE session_id = ?`,
	"session_captured_stats": `SELECT * FROM session_captured_stats WHERE session_id = ?`,
	"annotations":            `SELECT a.* FROM annotations a JOIN annotation_target_sessions t ON t.annotation_id = a.id WHERE t.session_id = ?`,
}

// digestArmState digests, per session, the entries the reader serves and
// each compared table's rows without the allowlisted columns.
func digestArmState(ctx context.Context, db *store.Store) (map[string]map[string]string, error) {
	allowed, err := armEquivalenceAllowlist()
	if err != nil {
		return nil, err
	}
	conn, err := db.Pool().Take(ctx)
	if err != nil {
		return nil, err
	}
	var sids []string
	err = sqlitex.ExecuteTransient(conn, `SELECT session_id FROM sessions ORDER BY session_id`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		sids = append(sids, st.ColumnText(0))
		return nil
	}})
	if err != nil {
		db.Pool().Put(conn)
		return nil, err
	}
	digests := map[string]map[string]string{}
	for _, sid := range sids {
		digests[sid] = map[string]string{}
		for table, query := range armStateQueries {
			var rows []string
			err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: []any{sid}, ResultFunc: func(st *sqlite.Stmt) error {
				var cells []string
				for i := range st.ColumnCount() {
					if allowed[table+"."+st.ColumnName(i)] {
						continue
					}
					cells = append(cells, fmt.Sprintf("%s=%d:%s", st.ColumnName(i), st.ColumnType(i), st.ColumnText(i)))
				}
				rows = append(rows, strings.Join(cells, "|"))
				return nil
			}})
			if err != nil {
				db.Pool().Put(conn)
				return nil, fmt.Errorf("read %s for session %s: %w", table, sid, err)
			}
			sort.Strings(rows)
			digests[sid][table] = digest(strings.Join(rows, "\n"))
		}
	}
	db.Pool().Put(conn)
	for _, sid := range sids {
		entries, err := db.ListEntries(ctx, ingest.SessionID(sid))
		if err != nil {
			return nil, fmt.Errorf("list entries of session %s: %w", sid, err)
		}
		encoded, err := json.Marshal(entries)
		if err != nil {
			return nil, err
		}
		digests[sid]["entries"] = fmt.Sprintf("%d:%s", len(entries), digest(string(encoded)))
	}
	return digests, nil
}

func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// armsEquivalent fails when the two arms of one round left any session's
// compared state different.
func armsEquivalent(skip, baseline sample) error {
	if len(skip.Equivalence) == 0 || len(baseline.Equivalence) == 0 {
		return fmt.Errorf("arm equivalence: a sample carries no state digest (skip %d sessions, baseline %d)", len(skip.Equivalence), len(baseline.Equivalence))
	}
	var differences []string
	for sid, tables := range skip.Equivalence {
		other, ok := baseline.Equivalence[sid]
		if !ok {
			differences = append(differences, sid+": only the skip arm holds it")
			continue
		}
		for table, value := range tables {
			if other[table] != value {
				differences = append(differences, sid+"."+table)
			}
		}
	}
	for sid := range baseline.Equivalence {
		if _, ok := skip.Equivalence[sid]; !ok {
			differences = append(differences, sid+": only the baseline arm holds it")
		}
	}
	if len(differences) != 0 {
		sort.Strings(differences)
		return fmt.Errorf("arm equivalence: the skip and baseline arms left different state in %v", differences)
	}
	return nil
}
