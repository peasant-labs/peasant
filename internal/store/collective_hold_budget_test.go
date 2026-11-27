package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/collective_hold_budget.yaml
var collectiveHoldBudgetFixtures []byte

// The note is composed at the end of an observation write. Most cases below
// call it directly with contexts they build, so whether the caller or the
// budget is live is an input, never a race with the clock. The one wrapper
// check waits for the connection's own interrupt; see its comment for the one
// real-time budget it still inherits.
const (
	// holdBudgetHold is the validated ceiling of ingest.WriteConfig.Validate,
	// so the wrapper check below runs a budget a configured app can actually
	// use. Its deadline starts before the connection is taken and the
	// transaction begins, which is why this check cannot use a short budget:
	// a pause longer than it in that window ends the step with the context's
	// error, before any SQLite interrupt exists.
	holdBudgetHold  = 5 * time.Second
	holdBudgetGuard = 30 * time.Second
)

// errOwnCause stands in for an observation write whose action fails on its own
// while the hold budget is also running out. SQLite never saw this one, so the
// budget note must not claim it.
var errOwnCause = errors.New("staged association rows failed validation: no evidence installed; re-read the source")

// holdBudgetErrors is the closed set of endings the note may be handed.
var holdBudgetErrors = map[string]func() error{
	"interrupt":         sqlite.ResultInterrupt.ToError,
	"wrapped-interrupt": func() error { return fmt.Errorf("observation write: %w", sqlite.ResultInterrupt.ToError()) },
	"busy":              sqlite.ResultBusy.ToError,
	"plain":             func() error { return errOwnCause },
	"none":              func() error { return nil },
}

// holdBudgetContexts is the closed set of caller and held states, built here
// rather than waited for.
var holdBudgetContexts = map[string]func(*testing.T) context.Context{
	"live": func(t *testing.T) context.Context { return t.Context() },
	"cancelled": func(t *testing.T) context.Context {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return ctx
	},
	"expired": func(t *testing.T) context.Context {
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		t.Cleanup(cancel)
		return ctx
	},
}

type collectiveHoldBudgetCase struct {
	Name     string `yaml:"name"`
	Error    string `yaml:"error"`
	Caller   string `yaml:"caller"`
	Held     string `yaml:"held"`
	WantNote bool   `yaml:"wantNote"`
}

// TestNameExpiredObservationHold pins the note's exact rule: it appears only
// for an interrupt error under a live caller and a budget that had already run
// out, and every other ending keeps the error it arrived with. Building the
// contexts directly is what lets these cases assert live-versus-expired states
// without depending on setup or rollback finishing inside a short window.
func TestNameExpiredObservationHold(t *testing.T) {
	var fixtures struct {
		RequiredNames []string                   `yaml:"requiredNames"`
		Cases         []collectiveHoldBudgetCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(collectiveHoldBudgetFixtures)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixtures.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("missing or duplicate hold-budget fixture %q", c.Name)
		}
		names[c.Name] = true
		if _, ok := holdBudgetErrors[c.Error]; !ok {
			t.Fatalf("hold-budget fixture %s names unknown ending %q", c.Name, c.Error)
		}
		if _, ok := holdBudgetContexts[c.Caller]; !ok {
			t.Fatalf("hold-budget fixture %s names unknown caller state %q", c.Name, c.Caller)
		}
		if _, ok := holdBudgetContexts[c.Held]; !ok {
			t.Fatalf("hold-budget fixture %s names unknown held state %q", c.Name, c.Held)
		}
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required hold-budget fixture %s missing", name)
		}
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			in := holdBudgetErrors[c.Error]()
			caller, held := holdBudgetContexts[c.Caller](t), holdBudgetContexts[c.Held](t)
			out := nameExpiredObservationHold(in, caller, held, holdBudgetHold)
			if in == nil {
				if out != nil {
					t.Fatalf("no error gained a note: %v", out)
				}
				return
			}
			if named := out != in; named != c.WantNote {
				t.Fatalf("expired hold budget named=%t want=%t: in=%v out=%v", named, c.WantNote, in, out)
			}
			if note := strings.Contains(out.Error(), holdBudgetHold.String()+" writer hold budget"); note != c.WantNote {
				t.Fatalf("note text present=%t want=%t: %v", note, c.WantNote, out)
			}
			if !errors.Is(out, in) {
				t.Fatalf("the result dropped the error it was handed: in=%v out=%v", in, out)
			}
			if code, want := sqlite.ErrCode(out), sqlite.ErrCode(in); code != want {
				t.Fatalf("the SQLite code changed: got=%v want=%v err=%v", code, want, out)
			}
			if errors.Is(out, context.DeadlineExceeded) {
				t.Fatalf("the note changed how callers classify the failure: %v", out)
			}
		})
	}
}

// TestObservationTransactionNamesExpiredHoldBudget proves the production
// wrapper reaches the note, keeps the interrupt classification and rolls the
// write back. The action waits for the connection's own interrupt rather than
// for a clock, so once the write is running a pause only delays the run: the
// deadline has already passed when the interrupt arrives, the caller is never
// cancelled and the transaction has installed nothing.
//
// The wrapper's hold budget is the one real-time bound here, and it starts
// before the connection is taken and the transaction begins. A pause longer
// than the validated five-second ceiling while pool.Take or BEGIN runs ends the
// step with the context's own error before any interrupt exists, and this check
// cannot observe the note; it reports that explicitly below instead of a wrong
// assertion. A configured app can use this budget.
func TestObservationTransactionNamesExpiredHoldBudget(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "observations.db"), WithWriteConfig(ingest.WriteConfig{HoldTarget: holdBudgetHold}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	home := t.TempDir()
	creds := auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-key-id", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture", VillageURL: "https://village.example/a"}
	if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
		t.Fatal(err)
	}
	snapshot, err := auth.CaptureCredentials(t.Context(), home)
	if err != nil {
		t.Fatal(err)
	}
	const village = "https://village.invalid/hold-budget"
	err = s.withObservationTransaction(t.Context(), snapshot, true, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn, `INSERT INTO collective_observation_scopes(village_url,user_id,incarnation) VALUES(?,?,?)`, &sqlitex.ExecOptions{Args: []any{village, creds.UserID, "hold-budget"}}); err != nil {
			return err
		}
		return waitHoldBudgetInterrupt(conn)
	})
	if err == nil {
		t.Fatal("an observation write held past its budget committed")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// The budget covers pool.Take and BEGIN, so a pause past it there ends
		// the step with the context's error and no interrupt exists to note.
		// That is a machine stall, not a behavior of the wrapper.
		t.Fatalf("the machine paused past the %s hold budget before the write reached an interrupt, so this check never observed the note: %v", holdBudgetHold, err)
	}
	if code := sqlite.ErrCode(err); code != sqlite.ResultInterrupt {
		t.Fatalf("the wrapper changed the SQLite code: code=%v err=%v", code, err)
	}
	if !strings.Contains(err.Error(), holdBudgetHold.String()+" writer hold budget") {
		t.Fatalf("the wrapper did not name the expired hold budget: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the note changed how callers classify the failure: %v", err)
	}
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	rows := -1
	if err := sqlitex.Execute(conn, `SELECT count(*) FROM collective_observation_scopes WHERE village_url=?`, &sqlitex.ExecOptions{Args: []any{village}, ResultFunc: func(stmt *sqlite.Stmt) error { rows = stmt.ColumnInt(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("unsettled observation write left %d row(s) installed", rows)
	}
}

// waitHoldBudgetInterrupt waits for the connection's own interrupt: the budget
// closing its context closes the connection's statement steps. The guard only
// reports a write that was never interrupted.
func waitHoldBudgetInterrupt(conn *sqlite.Conn) error {
	ticks := time.NewTicker(time.Millisecond)
	defer ticks.Stop()
	deadline := time.NewTimer(holdBudgetGuard)
	defer deadline.Stop()
	for {
		if err := sqlitex.Execute(conn, `SELECT 1`, nil); err != nil {
			return err
		}
		select {
		case <-ticks.C:
		case <-deadline.C:
			return fmt.Errorf("the observation write was never interrupted within %s; the interrupt is this check's wake source", holdBudgetGuard)
		}
	}
}
