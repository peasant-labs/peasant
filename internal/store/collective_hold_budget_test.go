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

// The budget is the fixture's input under test, not a wait. The interrupt is
// each case's wake source; the guard below only stops a hang, so a stalled
// machine fails loudly instead of passing silently.
const (
	holdBudgetHold  = 100 * time.Millisecond
	holdBudgetGuard = 10 * time.Second
)

// holdBudgetCodes is the closed set of SQLite result codes the fixtures may
// expect. "error" is SQLite's answer for an error that carries no SQLite code.
var holdBudgetCodes = map[string]sqlite.ResultCode{
	"interrupt": sqlite.ResultInterrupt,
	"error":     sqlite.ResultError,
}

// errOwnCause stands in for an observation write whose action fails on its own
// while the hold budget is also running out. SQLite never saw this one, so the
// budget explanation must not claim it.
var errOwnCause = errors.New("staged association rows failed validation: no evidence installed; re-read the source")

// holdBudgetOutcomes is the closed set of ways the fixture action ends once its
// first statement has opened the connection and its transaction.
var holdBudgetOutcomes = map[string]func(conn *sqlite.Conn, cancelCaller func()) error{
	// The budget expires while the transaction is still open.
	"interrupt-after-budget": func(conn *sqlite.Conn, _ func()) error {
		return waitHoldBudgetInterrupt(conn)
	},
	// The caller gives up first; the write reports the caller's interrupt.
	"interrupt-after-caller-cancel": func(conn *sqlite.Conn, cancelCaller func()) error {
		cancelCaller()
		return waitHoldBudgetInterrupt(conn)
	},
	// The budget expires first and the caller then gives up: the caller's
	// cancellation still owns the error, not the budget.
	"interrupt-after-budget-and-caller-cancel": func(conn *sqlite.Conn, cancelCaller func()) error {
		time.Sleep(holdBudgetHold + 50*time.Millisecond)
		cancelCaller()
		return waitHoldBudgetInterrupt(conn)
	},
	// The action fails for its own reason while the budget is expiring.
	"own-cause-after-budget": func(_ *sqlite.Conn, _ func()) error {
		time.Sleep(holdBudgetHold + 50*time.Millisecond)
		return errOwnCause
	},
	// Something else interrupts the connection while the budget is still
	// running: the failure keeps SQLite's own text, with no budget note.
	"interrupt-from-elsewhere": func(conn *sqlite.Conn, _ func()) error {
		interrupt := make(chan struct{})
		close(interrupt)
		conn.SetInterrupt(interrupt)
		return waitHoldBudgetInterrupt(conn)
	},
	// Something else interrupts the connection and the step is only reported
	// after the deadline. The note appears anyway, because the wrapper reports
	// what it observes at the end, not which interrupt fired; that is why its
	// text cannot name a cause.
	"interrupt-past-budget-from-elsewhere": func(conn *sqlite.Conn, _ func()) error {
		interrupt := make(chan struct{})
		close(interrupt)
		conn.SetInterrupt(interrupt)
		err := waitHoldBudgetInterrupt(conn)
		time.Sleep(holdBudgetHold + 50*time.Millisecond)
		return err
	},
}

// waitHoldBudgetInterrupt polls until the open transaction is interrupted. It
// never waits on a wall-clock guess: the interrupt closes the connection's
// statement steps, and the guard reports a fixture that was never interrupted.
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
			return fmt.Errorf("the observation write was never interrupted within %s; the interrupt is this fixture's wake source", holdBudgetGuard)
		}
	}
}

type collectiveHoldBudgetCase struct {
	Name         string `yaml:"name"`
	Outcome      string `yaml:"outcome"`
	WantCode     string `yaml:"wantCode"`
	WantOwnCause bool   `yaml:"wantOwnCause"`
	WantHoldNote bool   `yaml:"wantHoldNote"`
}

// TestObservationWriteHoldBudgetNamesTheExpiredBudget drives the one
// transaction wrapper every observation write uses. The action stays inside its
// transaction until the writer hold budget, the caller, or its own failure
// decides the outcome. The note under test reports what the wrapper observes at
// the end of the step; it must appear exactly when an interrupt error coincides
// with a budget that had run out and a caller that was still waiting, and it
// must not appear for any other ending.
func TestObservationWriteHoldBudgetNamesTheExpiredBudget(t *testing.T) {
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
		if _, ok := holdBudgetCodes[c.WantCode]; !ok {
			t.Fatalf("hold-budget fixture %s wants unknown SQLite code %q; use one of interrupt, error", c.Name, c.WantCode)
		}
		if _, ok := holdBudgetOutcomes[c.Outcome]; !ok {
			t.Fatalf("hold-budget fixture %s wants unknown outcome %q; use one of the declared outcomes", c.Name, c.Outcome)
		}
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required hold-budget fixture %s missing", name)
		}
	}
	const village = "https://village.invalid/hold-budget"
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
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
			caller, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()
			err = s.withObservationTransaction(caller, snapshot, true, func(conn *sqlite.Conn) error {
				if err := sqlitex.Execute(conn, `INSERT INTO collective_observation_scopes(village_url,user_id,incarnation) VALUES(?,?,?)`, &sqlitex.ExecOptions{Args: []any{village, creds.UserID, "hold-budget"}}); err != nil {
					return err
				}
				return holdBudgetOutcomes[c.Outcome](conn, cancelCaller)
			})
			if err == nil {
				t.Fatal("an observation write held open past its budget committed")
			}
			if code, want := sqlite.ErrCode(err), holdBudgetCodes[c.WantCode]; code != want {
				t.Fatalf("error code=%v want=%v err=%v", code, want, err)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the hold note changed how callers classify the failure: %v", err)
			}
			if kept := errors.Is(err, errOwnCause); kept != c.WantOwnCause {
				t.Fatalf("own-cause error kept=%t want=%t: %v", kept, c.WantOwnCause, err)
			}
			named := strings.Contains(err.Error(), holdBudgetHold.String()+" writer hold budget")
			if named != c.WantHoldNote {
				t.Fatalf("expired hold budget named=%t want=%t: %v", named, c.WantHoldNote, err)
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
		})
	}
}
