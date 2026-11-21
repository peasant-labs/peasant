package store

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/mapping_set_reclaim.yaml
var mappingSetReclaimYAML []byte

// The actions a mapping-set reclaim case runs.
const (
	mappingSetActionSweep             = "sweep"
	mappingSetActionSweepInterrupted  = "sweep-interrupted"
	mappingSetActionDryRunEqualsApply = "dry-run-equals-apply"
	mappingSetActionVerify            = "verify"
	mappingSetActionRepresentation    = "representation-delete"
	mappingSetActionPrune             = "prune"
	mappingSetActionConcurrent        = "concurrent"
	mappingSetActionRepair            = "repair"
)

const mappingSetSession = "abc00000-0000-4000-8000-0000000000f7"

type mappingSetGeneration struct {
	ID         string `yaml:"id"`
	Text       string `yaml:"text"`
	References string `yaml:"references"`
	Activate   bool   `yaml:"activate"`
	Forged     bool   `yaml:"forged"`
}

type mappingSetReclaimCase struct {
	Name                    string                 `yaml:"name"`
	Given                   string                 `yaml:"given"`
	When                    string                 `yaml:"when"`
	Then                    string                 `yaml:"then"`
	Action                  string                 `yaml:"action"`
	SweepRows               int                    `yaml:"sweepRows"`
	Generations             []mappingSetGeneration `yaml:"generations"`
	Latest                  string                 `yaml:"latest"`
	CollectBeforeAction     []string               `yaml:"collectBeforeAction"`
	ConcurrentReference     *mappingSetGeneration  `yaml:"concurrentReference"`
	Cases                   []string               `yaml:"cases"`
	WantActive              string                 `yaml:"wantActive"`
	WantKept                []string               `yaml:"wantKept"`
	WantCollected           []string               `yaml:"wantCollected"`
	WantResolves            map[string]string      `yaml:"wantResolves"`
	WantCollectedBodiesGone bool                   `yaml:"wantCollectedBodiesGone"`
	WantDangling            *struct {
		Generation  string `yaml:"generation"`
		MappingSet  string `yaml:"mappingSet"`
		Disposition string `yaml:"disposition"`
	} `yaml:"wantDangling"`
	Pending string `yaml:"pending"`
}

func loadMappingSetReclaimCases(t *testing.T) map[string]mappingSetReclaimCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string                `yaml:"requiredNames"`
		Cases         []mappingSetReclaimCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(mappingSetReclaimYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode the mapping-set reclaim fixture: %v", err)
	}
	cases := map[string]mappingSetReclaimCase{}
	for _, c := range fixture.Cases {
		_, duplicate := cases[c.Name]
		stated := c.Given != "" && c.When != "" && c.Then != ""
		if c.Name == "" || duplicate || !stated {
			t.Fatalf("mapping-set reclaim case %q is unnamed, duplicated, or misses its given, when or then", c.Name)
		}
		cases[c.Name] = c
	}
	if got, want := slices.Sorted(maps.Keys(cases)), slices.Sorted(slices.Values(fixture.RequiredNames)); !slices.Equal(got, want) {
		t.Fatalf("mapping-set reclaim cases %v differ from the required names %v", got, want)
	}
	return cases
}

// TestParseTablesAreGenerationScoped pins parseTables as an exact subset of
// the generation-scoped inventory, so a parse table is always one the sweep,
// the reclaim and the representation delete already clear.
func TestParseTablesAreGenerationScoped(t *testing.T) {
	seen := map[string]bool{}
	for _, table := range parseTables {
		if seen[table] {
			t.Fatalf("parse table %s is listed twice", table)
		}
		seen[table] = true
		if !slices.Contains(reclaimTableNames, table) {
			t.Fatalf("parse table %s is not in reclaimTableNames", table)
		}
	}
	if slices.Contains(parseTables, "session_generations") {
		t.Fatal("the catalog table is never read from an owner")
	}
}

func TestMappingSetReclaim(t *testing.T) {
	cases := loadMappingSetReclaimCases(t)
	for _, name := range slices.Sorted(maps.Keys(cases)) {
		c := cases[name]
		t.Run(c.Name, func(t *testing.T) {
			switch c.Action {
			case mappingSetActionSweep, mappingSetActionSweepInterrupted, mappingSetActionConcurrent:
				runMappingSetSweep(t, c)
			case mappingSetActionDryRunEqualsApply:
				for _, sub := range c.Cases {
					built, ok := cases[sub]
					if !ok || built.Action != mappingSetActionSweep {
						t.Fatalf("dry-run case names %q, which is not a sweep case", sub)
					}
					t.Run(sub, func(t *testing.T) { runMappingSetDryRunEqualsApply(t, built) })
				}
			case mappingSetActionVerify:
				runMappingSetVerify(t, c)
			case mappingSetActionRepresentation, mappingSetActionPrune:
				runMappingSetRemoval(t, c)
			case mappingSetActionRepair:
				runMappingSetRepair(t, c)
			default:
				t.Fatalf("mapping-set reclaim case %s has unknown action %q", c.Name, c.Action)
			}
		})
	}
}

// mappingSetWorld is one built case: the store, the session and the parse
// row counts each owner held right after it was installed.
type mappingSetWorld struct {
	s          *Store
	sid        schema.SessionID
	ownerRows  map[string]int64
	ownerTexts map[string]string
}

func buildMappingSetWorld(t *testing.T, c mappingSetReclaimCase) mappingSetWorld {
	t.Helper()
	s := openGenerationStoreAt(t, t.TempDir())
	t.Cleanup(func() { _ = s.Close() })
	rows := c.SweepRows
	if rows == 0 {
		rows = 10_000
	}
	s.writeConfig.SweepRows = rows
	seedGenerationSession(t, s, mappingSetSession)
	w := mappingSetWorld{s: s, sid: schema.SessionID(mappingSetSession), ownerRows: map[string]int64{}, ownerTexts: map[string]string{}}
	lastOwner := ""
	for _, g := range c.Generations {
		if g.Text != "" {
			v2, blobs := buildTestGeneration(t, w.sid, g.ID, g.Text, g.Text+" input", g.Text+" output")
			if err := activateTestGeneration(t, s, v2, blobs); err != nil {
				t.Fatalf("install owner %s: %v", g.ID, err)
			}
			w.ownerRows[g.ID] = w.parseRows(t, g.ID)
			w.ownerTexts[g.ID] = g.Text
			lastOwner = g.ID
			continue
		}
		source := g.References
		if g.Forged {
			source = lastOwner
		}
		w.insertReference(t, g.ID, source, g.References, g.Activate)
	}
	if c.Latest != "" {
		execGenerationSQL(t, s, `UPDATE sessions SET latest_attempt_generation_id = '`+c.Latest+`', latest_attempt_artifact_hash = '`+strings.Repeat("b", 64)+`' WHERE session_id = '`+mappingSetSession+`';`)
	}
	execGenerationSQL(t, s, `UPDATE sessions SET content_sweep_pending = 1 WHERE session_id = '`+mappingSetSession+`';`)
	return w
}

// insertReference copies the catalog row of source as a new generation id
// whose mapping_set_id is mappingSet, in one write transaction, and makes it
// active when asked. It writes no parsed row.
func (w mappingSetWorld) insertReference(t *testing.T, id, source, mappingSet string, activate bool) {
	t.Helper()
	conn, err := w.s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer w.s.pool.Put(conn)
	if err := insertMappingSetReferenceOnConn(conn, w.sid, id, source, mappingSet, activate); err != nil {
		t.Fatal(err)
	}
}

func insertMappingSetReferenceOnConn(conn *sqlite.Conn, sid schema.SessionID, id, source, mappingSet string, activate bool) (err error) {
	var columns []string
	if err := sqlitex.Execute(conn, `SELECT name FROM pragma_table_info('session_generations') WHERE name NOT IN ('generation_id','mapping_set_id') ORDER BY cid`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		columns = append(columns, stmt.ColumnText(0))
		return nil
	}}); err != nil {
		return err
	}
	endFn, err := beginWrite(conn, "insert a test reference generation")
	if err != nil {
		return err
	}
	defer endFn(&err)
	list := strings.Join(columns, ", ")
	if err := sqlitex.Execute(conn, `INSERT INTO session_generations(generation_id, mapping_set_id, `+list+`) SELECT ?, ?, `+list+` FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{id, mappingSet, string(sid), source},
	}); err != nil {
		return fmt.Errorf("insert reference %s to %s: %w", id, mappingSet, err)
	}
	if conn.Changes() != 1 {
		return fmt.Errorf("insert reference %s: the source generation %s has no catalog row", id, source)
	}
	if activate {
		if err := sqlitex.Execute(conn, `UPDATE sessions SET active_generation_id = ?, content_sweep_pending = 1 WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{id, string(sid)}}); err != nil {
			return err
		}
	}
	return nil
}

// parseRows counts the parsed rows stored under one generation id.
func (w mappingSetWorld) parseRows(t *testing.T, generationID string) int64 {
	t.Helper()
	var total int64
	for _, table := range parseTables {
		total += countGenerationRows(t, w.s, table, w.sid, generationID)
	}
	return total
}

// allRows counts every generation-scoped row of one generation.
func (w mappingSetWorld) allRows(t *testing.T, generationID string) int64 {
	t.Helper()
	return generationRowCounts(t, w.s, w.sid, generationID).Total()
}

// rowSnapshot records every generation-scoped row count of every generation
// the case names.
func (w mappingSetWorld) rowSnapshot(t *testing.T, c mappingSetReclaimCase) map[string]ReclaimTableCounts {
	t.Helper()
	snapshot := map[string]ReclaimTableCounts{}
	for _, g := range c.Generations {
		snapshot[g.ID] = generationRowCounts(t, w.s, w.sid, g.ID)
	}
	if c.ConcurrentReference != nil {
		snapshot[c.ConcurrentReference.ID] = generationRowCounts(t, w.s, w.sid, c.ConcurrentReference.ID)
	}
	return snapshot
}

// assertMappingSetOutcome checks the active generation, that every kept
// generation's rows are unchanged since before, that every collected
// generation has no row left, and that each resolving generation reads its
// owner's complete parsed rows.
func (w mappingSetWorld) assertMappingSetOutcome(t *testing.T, c mappingSetReclaimCase, before map[string]ReclaimTableCounts) {
	t.Helper()
	if c.WantActive != "" {
		if active := visibleGeneration(t, w.s, w.sid); active != c.WantActive {
			t.Fatalf("active generation = %q, want %q", active, c.WantActive)
		}
	}
	for _, id := range c.WantKept {
		if got := generationRowCounts(t, w.s, w.sid, id); got != before[id] || got.SessionGenerations != 1 {
			t.Errorf("kept generation %s has rows %+v, want its %+v with its catalog row", id, got, before[id])
		}
	}
	for _, id := range c.WantCollected {
		if got := w.allRows(t, id); got != 0 {
			t.Errorf("collected generation %s still has %d rows", id, got)
		}
	}
	conn, err := w.s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer w.s.pool.Put(conn)
	for id, owner := range c.WantResolves {
		resolved, err := rowsGenerationIDOnConn(conn, w.sid, id)
		if err != nil {
			t.Fatal(err)
		}
		if resolved != owner {
			t.Errorf("generation %s resolves to %q, want %q", id, resolved, owner)
		}
		want, ok := w.ownerRows[owner]
		if !ok || want == 0 {
			t.Fatalf("owner %s recorded no parsed rows", owner)
		}
		if got := w.parseRows(t, resolved); got != want {
			t.Errorf("generation %s reads %d parsed rows through %s, want all %d the owner held", id, got, resolved, want)
		}
	}
}

func runMappingSetSweep(t *testing.T, c mappingSetReclaimCase) {
	w := buildMappingSetWorld(t, c)
	before := w.rowSnapshot(t, c)
	ownedBodies := map[string][]string{}
	for _, id := range c.WantCollected {
		ownedBodies[id] = w.mappedBodies(t, id)
	}
	ctx := context.Background()
	switch c.Action {
	case mappingSetActionSweepInterrupted:
		injected := errors.New("injected crash between sweep batches")
		contentSweepSeam = func(stage string) error {
			if stage == contentSweepSeamMidBatch {
				return injected
			}
			return nil
		}
		t.Cleanup(func() { contentSweepSeam = nil })
		if _, err := w.s.SweepSession(ctx, w.sid); !errors.Is(err, injected) {
			t.Fatalf("the interrupted sweep returned %v, want the injected crash", err)
		}
		for _, id := range c.WantKept {
			if got := generationRowCounts(t, w.s, w.sid, id); got != before[id] {
				t.Fatalf("the interrupted sweep deleted rows of kept generation %s: %+v, want %+v", id, got, before[id])
			}
		}
		contentSweepSeam = nil
	case mappingSetActionConcurrent:
		runMappingSetConcurrentReference(t, w, c)
		before = w.rowSnapshot(t, c)
		w.assertMappingSetOutcome(t, c, before)
		return
	}
	if _, err := w.s.SweepSession(ctx, w.sid); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	w.assertMappingSetOutcome(t, c, before)
	if c.WantCollectedBodiesGone {
		keptBodies := map[string]bool{}
		for _, id := range c.WantKept {
			for _, digest := range w.mappedBodies(t, id) {
				keptBodies[digest] = true
			}
		}
		for id, digests := range ownedBodies {
			for _, digest := range digests {
				if !keptBodies[digest] && w.bodyExists(t, digest) {
					t.Errorf("entry body %s, mapped only by collected generation %s, survived the sweep", digest, id)
				}
			}
		}
	}
	assertReclaimIntegrity(t, w.s)
}

// runMappingSetConcurrentReference gates a writer that holds the exclusive
// session lock before it installs the reference, starts a sweep of the same
// session, then releases the gate. The sweep waits on the lock, so it reads
// the keep set after the reference is active.
func runMappingSetConcurrentReference(t *testing.T, w mappingSetWorld, c mappingSetReclaimCase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	held := make(chan struct{})
	gate := make(chan struct{})
	writerDone := make(chan error, 1)
	reference := c.ConcurrentReference
	go func() {
		release, err := w.s.sessionLocker.LockExclusive(ctx, w.sid)
		if err != nil {
			close(held)
			writerDone <- err
			return
		}
		close(held)
		select {
		case <-gate:
		case <-ctx.Done():
			_ = release()
			writerDone <- ctx.Err()
			return
		}
		conn, err := w.s.pool.Take(ctx)
		if err != nil {
			_ = release()
			writerDone <- err
			return
		}
		err = insertMappingSetReferenceOnConn(conn, w.sid, reference.ID, reference.References, reference.References, true)
		w.s.pool.Put(conn)
		_ = release()
		writerDone <- err
	}()
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal("the writer never took the session lock")
	}
	sweepDone := make(chan error, 1)
	go func() {
		_, err := w.s.SweepSession(ctx, w.sid)
		sweepDone <- err
	}()
	close(gate)
	for _, done := range []chan error{writerDone, sweepDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("concurrent reference and sweep: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("the reference writer and the sweep did not both finish before the deadline")
		}
	}
}

// mappedBodies lists the body digests one generation's entry mapping names.
func (w mappingSetWorld) mappedBodies(t *testing.T, generationID string) []string {
	t.Helper()
	conn, err := w.s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer w.s.pool.Put(conn)
	var digests []string
	if err := sqlitex.Execute(conn, `SELECT body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, &sqlitex.ExecOptions{
		Args: []any{string(w.sid), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			digests = append(digests, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return digests
}

func (w mappingSetWorld) bodyExists(t *testing.T, digest string) bool {
	t.Helper()
	conn, err := w.s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer w.s.pool.Put(conn)
	found := false
	if err := sqlitex.Execute(conn, `SELECT 1 FROM session_entry_bodies WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{
		Args:       []any{string(w.sid), digest},
		ResultFunc: func(*sqlite.Stmt) error { found = true; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

func runMappingSetDryRunEqualsApply(t *testing.T, c mappingSetReclaimCase) {
	w := buildMappingSetWorld(t, c)
	before := w.rowSnapshot(t, c)
	ctx := context.Background()
	plan, err := w.s.PlanSupersededGenerationReclaim(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var forecast []string
	var forecastRows ReclaimTableCounts
	for _, session := range plan.Sessions {
		for _, g := range session.Generations {
			forecast = append(forecast, g.GenerationID)
			forecastRows.Add(g.Rows)
			if g.Rows != before[g.GenerationID] {
				t.Errorf("forecast rows of %s = %+v, want the %+v it holds", g.GenerationID, g.Rows, before[g.GenerationID])
			}
		}
	}
	if want := slices.Sorted(slices.Values(c.WantCollected)); !slices.Equal(slices.Sorted(slices.Values(forecast)), want) {
		t.Fatalf("forecast generations %v, want exactly the collected %v", forecast, want)
	}
	result, err := w.s.ReclaimSupersededGenerations(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows != forecastRows {
		t.Errorf("the apply deleted %+v, the forecast said %+v", result.Rows, forecastRows)
	}
	w.assertMappingSetOutcome(t, c, before)
	assertReclaimIntegrity(t, w.s)
}

// collectByForgery deletes every generation-scoped row of the named
// generations, as an interrupted sweep that reached them first leaves them.
func (w mappingSetWorld) collectByForgery(t *testing.T, ids []string) {
	t.Helper()
	for _, id := range ids {
		var script strings.Builder
		for _, table := range reclaimTableNames {
			fmt.Fprintf(&script, "DELETE FROM %s WHERE session_id = '%s' AND generation_id = '%s';\n", table, mappingSetSession, id)
		}
		execGenerationSQL(t, w.s, script.String())
	}
}

func runMappingSetVerify(t *testing.T, c mappingSetReclaimCase) {
	w := buildMappingSetWorld(t, c)
	w.collectByForgery(t, c.CollectBeforeAction)
	report, err := w.s.VerifyContent(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	want := DanglingMappingSet{SessionID: w.sid, GenerationID: c.WantDangling.Generation, MappingSetID: c.WantDangling.MappingSet, Disposition: DanglingMappingSetDisposition(c.WantDangling.Disposition)}
	if !slices.Contains(AllDanglingMappingSetDispositions(), want.Disposition) {
		t.Fatalf("the case names an unknown disposition %q", want.Disposition)
	}
	if !slices.Equal(report.DanglingMappingSets, []DanglingMappingSet{want}) {
		t.Fatalf("dangling mapping sets = %+v, want exactly %+v", report.DanglingMappingSets, want)
	}
}

func runMappingSetRemoval(t *testing.T, c mappingSetReclaimCase) {
	w := buildMappingSetWorld(t, c)
	ctx := context.Background()
	switch c.Action {
	case mappingSetActionRepresentation:
		conn, err := w.s.pool.Take(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = func() (err error) {
			endFn, err := beginWrite(conn, "replace the test session's representation")
			if err != nil {
				return err
			}
			defer endFn(&err)
			return generationIndexFormat{}.Delete(ctx, conn, w.sid)
		}()
		w.s.pool.Put(conn)
		if err != nil {
			t.Fatal(err)
		}
	case mappingSetActionPrune:
		if _, err := w.s.PruneSessions(ctx, []ingest.SessionID{w.sid}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range reclaimTableNames {
		conn, err := w.s.pool.Take(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var count int64
		err = sqlitex.Execute(conn, `SELECT COUNT(*) FROM `+table+` WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args:       []any{string(w.sid)},
			ResultFunc: func(stmt *sqlite.Stmt) error { count = stmt.ColumnInt64(0); return nil },
		})
		w.s.pool.Put(conn)
		if err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s keeps %d rows of the session after %s", table, count, c.Action)
		}
	}
}

func runMappingSetRepair(t *testing.T, c mappingSetReclaimCase) {
	w := buildMappingSetWorld(t, c)
	before := w.rowSnapshot(t, c)
	damageBodyPreview(t, w.s, w.sid, 0)
	if err := verifyAllBodies(t, w.s, w.sid); err == nil {
		t.Fatal("verifyAllBodies succeeded after body damage")
	}
	execGenerationSQL(t, w.s, fmt.Sprintf(`UPDATE sessions SET artifact_hash = '%s', indexed_input_hash = NULL WHERE session_id = '%s';`, strings.Repeat("a", 64), mappingSetSession))

	v2, blobs := buildTestGeneration(t, w.sid, "g3", w.ownerTexts["g1"], w.ownerTexts["g1"]+" input", w.ownerTexts["g1"]+" output")
	inputHash := strings.Repeat("c", 64)
	if _, err := w.s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:       filledCandidateForValidation(t, v2, blobs),
		Blobs:            blobs,
		IndexerVersion:   1,
		IndexedAtMs:      2,
		IndexedInputHash: &inputHash,
	}); err != nil {
		t.Fatalf("activate repair candidate: %v", err)
	}
	if got := generationRowCounts(t, w.s, w.sid, "g3").Total(); got != 0 {
		t.Fatalf("repair inserted %d rows for g3 instead of repairing g1 in place", got)
	}
	w.assertMappingSetOutcome(t, c, before)
	if err := verifyAllBodies(t, w.s, w.sid); err != nil {
		t.Fatalf("verifyAllBodies after repair: %v", err)
	}
	entries, err := w.s.ListEntries(context.Background(), w.sid)
	if err != nil {
		t.Fatalf("read entries after repair: %v", err)
	}
	for _, entry := range entries {
		if entry.ContentPreview != nil && *entry.ContentPreview == "corrupt bytes" {
			t.Fatalf("entry %d still carries corrupted content after repair", entry.EntryIndex)
		}
	}
	assertReclaimIntegrity(t, w.s)
}
