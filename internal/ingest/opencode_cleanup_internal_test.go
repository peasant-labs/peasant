package ingest

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_cleanup.yaml
var openCodeCleanupYAML []byte

type openCodeCleanupCase struct {
	Name         string `yaml:"name"`
	Fixture      string `yaml:"fixture"`
	SessionID    string `yaml:"session_id"`
	Snapshot     bool   `yaml:"snapshot"`
	PreviewSlice bool   `yaml:"preview_slice"`
	Budget       int64  `yaml:"budget"`
	Cancel       bool   `yaml:"cancel"`
}

func loadOpenCodeCleanupCases(t *testing.T) []openCodeCleanupCase {
	t.Helper()
	var doc struct {
		RequiredCases []string              `yaml:"required_cases"`
		Cases         []openCodeCleanupCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(openCodeCleanupYAML, &doc); err != nil {
		t.Fatalf("decode cleanup cases: %v", err)
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("cleanup fixture has no required case names")
	}
	present := make(map[string]bool, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" || c.Fixture == "" || c.SessionID == "" {
			t.Fatalf("incomplete cleanup case: %+v", c)
		}
		if present[c.Name] {
			t.Fatalf("duplicate cleanup case %q", c.Name)
		}
		if c.PreviewSlice && c.Budget <= 0 {
			t.Fatalf("preview cleanup case %q has no positive budget", c.Name)
		}
		present[c.Name] = true
	}
	for _, name := range doc.RequiredCases {
		if !present[name] {
			t.Fatalf("cleanup fixture missing required case %q", name)
		}
	}
	return doc.Cases
}

// cancelPendingCleanupRead cancels only after a real row was decoded under the
// source lease, so both the read transaction and SQLite interrupt are active.
type cancelPendingCleanupRead struct{ cancel context.CancelFunc }

var _ openCodeSQLiteCancellationCheckpoint = cancelPendingCleanupRead{}

func (c cancelPendingCleanupRead) AfterPendingRow(ctx context.Context, _ openCodeCurrentPendingPageState) error {
	c.cancel()
	return ctx.Err()
}

// canceledCloseGate removes the idle permit only if Close is given a cancelled
// parent. This makes the old bug deterministic: it cannot randomly win a
// select on the idle permit. A detached cleanup parent leaves the source alone.
type canceledCloseGate struct {
	t      *testing.T
	source *zombiezenOpenCodeSQLiteSource
	held   bool
}

var _ OpenCodeSQLiteDeadlineClock = (*canceledCloseGate)(nil)

func (c *canceledCloseGate) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if parent.Err() != nil && c.source != nil && !c.held {
		testwait.Receive(c.t, c.source.permit, "idle source permit for cancelled-close regression")
		c.held = true
	}
	return context.WithTimeout(parent, d)
}

func (c *canceledCloseGate) release() {
	if c.held {
		c.held = false
		c.source.permit <- struct{}{}
	}
}

func assertSyntheticWriterCanLock(t *testing.T, path string) {
	t.Helper()
	writer, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic writer: %v", err)
	}
	defer func() {
		if err := writer.Close(); err != nil {
			t.Errorf("close synthetic writer: %v", err)
		}
	}()
	writer.SetBusyTimeout(0)
	if err := sqlitex.ExecuteTransient(writer, "BEGIN EXCLUSIVE", nil); err != nil {
		t.Fatalf("synthetic database remains read-locked after cleanup: %v", err)
	}
	if err := sqlitex.ExecuteTransient(writer, "ROLLBACK", nil); err != nil {
		t.Fatalf("end synthetic writer transaction: %v", err)
	}
}

func TestOpenCodeMaterializationReleasesCanceledSQLiteSource(t *testing.T) {
	for _, c := range loadOpenCodeCleanupCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			fixture := testfixture.MaterializeByName(t, c.Fixture)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			clock := &canceledCloseGate{t: t}
			options := DefaultOpenCodeSQLiteSourceOptions()
			options.clock = clock
			if c.Cancel {
				options.cancellationCheckpoint = cancelPendingCleanupRead{cancel: cancel}
			}
			adapter := &OpenCodeAdapter{fs: &OSFileSystem{}, git: snapshotGitResolver{}, candidateOptions: options}
			adapter.candidateOpener = func(ctx context.Context, path OpenCodeSQLiteSourcePath, options OpenCodeSQLiteSourceOptions) (OpenCodeSQLiteSource, error) {
				opened, err := OpenOpenCodeSQLiteSource(ctx, path, options)
				if err == nil {
					clock.source = opened.(*zombiezenOpenCodeSQLiteSource)
				}
				return opened, err
			}
			t.Cleanup(func() {
				clock.release()
				if clock.source != nil && !clock.source.connClosed {
					if err := clock.source.Close(context.Background()); err != nil {
						t.Errorf("cleanup synthetic source: %v", err)
					}
				}
			})
			sessionID, err := NewSessionID(c.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			session := DiscoveredSession{SessionID: sessionID, Harness: HarnessOpenCode, SourcePath: ResolvedPath(fixture.Path), TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite}
			if c.PreviewSlice {
				result, readErr := adapter.MaterializeTranscriptSlice(ctx, session, c.Budget, TranscriptSliceCursor{})
				err = readErr
				if !c.Cancel && err == nil && len(result.Data) == 0 {
					t.Error("successful preview slice returned no transcript")
				}
			} else if c.Snapshot {
				var result CursorMaterializedTranscript
				result, err = adapter.MaterializeTranscriptWithCursor(ctx, session)
				if !c.Cancel && err == nil && len(result.Transcript) == 0 {
					t.Error("successful snapshot materialization returned no transcript")
				}
			} else {
				var result MaterializedTranscript
				result, err = adapter.MaterializeTranscript(ctx, session)
				if !c.Cancel && err == nil && len(result.Data) == 0 {
					t.Error("successful materialization returned no transcript")
				}
			}
			if c.Cancel {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("materialization error = %v; want original cancellation", err)
				}
			} else if err != nil {
				t.Fatalf("successful materialization: %v", err)
			}
			if clock.source == nil || !clock.source.connClosed {
				t.Fatal("materialization returned without closing the real SQLite source")
			}
			assertSyntheticWriterCanLock(t, fixture.Path)
		})
	}
}

// cleanupTransactionClock signals the actual detached cleanup attempt. Its
// timeout mode gives that attempt an already-expired deadline, with no sleep.
type cleanupTransactionClock struct {
	armed   bool
	expire  bool
	started chan struct{}
}

var _ OpenCodeSQLiteDeadlineClock = (*cleanupTransactionClock)(nil)

func (c *cleanupTransactionClock) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if c.armed && parent.Err() == nil {
		c.armed = false
		close(c.started)
		if c.expire {
			return context.WithDeadline(parent, time.Now().Add(-time.Second))
		}
	}
	return context.WithTimeout(parent, d)
}

func TestOpenCodeReadTransactionRollbackAfterCancellation(t *testing.T) {
	fixture := testfixture.MaterializeByName(t, "native-current-rows")
	path, err := NewOpenCodeSQLiteSourcePath(fixture.Path)
	if err != nil {
		t.Fatal(err)
	}
	clock := &cleanupTransactionClock{started: make(chan struct{})}
	options := DefaultOpenCodeSQLiteSourceOptions()
	options.clock = clock
	opened, err := OpenOpenCodeSQLiteSource(t.Context(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	source := opened.(*zombiezenOpenCodeSQLiteSource)
	defer func() {
		if err := source.Close(context.Background()); err != nil {
			t.Errorf("close synthetic source: %v", err)
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	held := make(chan struct{})
	go func() {
		result <- source.withReadTransaction(ctx, func() error {
			if _, err := source.Catalog(ctx); err != nil {
				return err
			}
			testwait.Receive(t, source.permit, "source permit held for rollback synchronization")
			clock.armed = true
			cancel()
			close(held)
			return ctx.Err()
		})
	}()
	testwait.Receive(t, held, "transaction acquired the source permit and cancelled")
	select {
	case <-clock.started:
		source.permit <- struct{}{}
	case err := <-result:
		source.permit <- struct{}{}
		t.Fatalf("transaction ended before attempting independent rollback: %v", err)
	case <-testwait.Context(t).Done():
		source.permit <- struct{}{}
		t.Fatal("transaction cleanup did not start")
	}
	err = testwait.Receive(t, result, "transaction rollback completed")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("transaction error = %v, want original cancellation", err)
	}
	if !source.conn.AutocommitEnabled() {
		t.Fatal("cancelled transaction remains active before the source closes")
	}
	assertSyntheticWriterCanLock(t, fixture.Path)
}

func TestOpenCodeReadTransactionClearsInterruptForRollback(t *testing.T) {
	fixture := testfixture.MaterializeByName(t, "native-current-rows")
	source := openConcreteSyntheticSource(t, fixture)
	t.Cleanup(func() {
		if err := source.Close(context.Background()); err != nil {
			t.Errorf("close synthetic source: %v", err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := source.withReadTransaction(ctx, func() error {
		if _, err := source.Catalog(ctx); err != nil {
			return err
		}
		// Model a connection retaining the interrupted read's channel. Zombiezen
		// refuses every statement until SetInterrupt changes that channel.
		testwait.Receive(t, source.permit, "source permit before installing interrupted channel")
		source.conn.SetInterrupt(ctx.Done())
		source.permit <- struct{}{}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "end OpenCode SQLite read transaction") {
		t.Errorf("interrupted transaction error = %v, want cancellation and a successful rollback", err)
	}
	if !source.conn.AutocommitEnabled() {
		t.Error("interrupted transaction remains active before close")
	}
	// A fresh production read must be usable after the cancelled operation.
	if _, err := source.Catalog(t.Context()); err != nil {
		t.Errorf("reuse source after interrupted rollback: %v", err)
	}
	assertSyntheticWriterCanLock(t, fixture.Path)
	if err := source.Close(context.Background()); err != nil {
		t.Fatalf("close synthetic source: %v", err)
	}
}

func TestOpenCodeReadTransactionCommitsBeforeClosing(t *testing.T) {
	fixture := testfixture.MaterializeByName(t, "native-current-rows")
	source := openConcreteSyntheticSource(t, fixture)
	defer func() {
		if err := source.Close(context.Background()); err != nil {
			t.Errorf("close synthetic source: %v", err)
		}
	}()
	err := source.withReadTransaction(t.Context(), func() error {
		_, err := source.Catalog(t.Context())
		return err
	})
	if err != nil {
		t.Fatalf("complete ordinary read transaction: %v", err)
	}
	if !source.conn.AutocommitEnabled() {
		t.Fatal("successful read transaction remains active before close")
	}
	assertSyntheticWriterCanLock(t, fixture.Path)
}

type interruptCleanupClock struct {
	armed  bool
	cancel context.CancelCauseFunc
}

var _ OpenCodeSQLiteDeadlineClock = (*interruptCleanupClock)(nil)

func (c *interruptCleanupClock) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if c.armed {
		c.armed = false
		ctx, cancel := context.WithCancelCause(parent)
		c.cancel = cancel
		return ctx, func() { cancel(context.Canceled) }
	}
	return context.WithTimeout(parent, d)
}

func TestOpenCodeReadTransactionInterruptsCleanupStatement(t *testing.T) {
	fixture := testfixture.MaterializeByName(t, "native-current-rows")
	path, err := NewOpenCodeSQLiteSourcePath(fixture.Path)
	if err != nil {
		t.Fatal(err)
	}
	clock := &interruptCleanupClock{}
	options := DefaultOpenCodeSQLiteSourceOptions()
	options.clock = clock
	opened, err := OpenOpenCodeSQLiteSource(t.Context(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	source := opened.(*zombiezenOpenCodeSQLiteSource)
	defer func() {
		if err := source.Close(context.Background()); err != nil {
			t.Errorf("close synthetic source: %v", err)
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = source.withReadTransaction(ctx, func() error {
		if _, err := source.Catalog(ctx); err != nil {
			return err
		}
		testwait.Receive(t, source.permit, "source permit before installing cleanup interrupt")
		authorizerErr := source.conn.SetAuthorizer(sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
			if action.Type() == sqlite.OpTransaction {
				// End the injected deadline only after SQLite starts preparing the
				// rollback, not while cleanup is waiting for the permit.
				clock.cancel(context.DeadlineExceeded)
			}
			return source.authorizeRead(action)
		}))
		source.permit <- struct{}{}
		if authorizerErr != nil {
			return authorizerErr
		}
		clock.armed = true
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("interrupted cleanup error = %v, want the original cancellation and cleanup deadline", err)
	}
	if err := source.conn.SetAuthorizer(sqlite.AuthorizeFunc(source.authorizeRead)); err != nil {
		t.Fatalf("restore synthetic source authorizer: %v", err)
	}
	cleanupErr := errors.New("retry rollback")
	source.endReadTransaction(&cleanupErr)
	if !source.conn.AutocommitEnabled() {
		t.Errorf("retry left the read transaction active: %v", cleanupErr)
	}
	assertSyntheticWriterCanLock(t, fixture.Path)
}

func TestOpenCodeReadTransactionReportsRollbackTimeout(t *testing.T) {
	fixture := testfixture.MaterializeByName(t, "native-current-rows")
	path, err := NewOpenCodeSQLiteSourcePath(fixture.Path)
	if err != nil {
		t.Fatal(err)
	}
	clock := &cleanupTransactionClock{expire: true, started: make(chan struct{})}
	options := DefaultOpenCodeSQLiteSourceOptions()
	options.clock = clock
	opened, err := OpenOpenCodeSQLiteSource(t.Context(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	source := opened.(*zombiezenOpenCodeSQLiteSource)
	t.Cleanup(func() {
		if err := source.Close(context.Background()); err != nil {
			t.Errorf("close synthetic source: %v", err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	held := false
	err = source.withReadTransaction(ctx, func() error {
		if _, err := source.Catalog(ctx); err != nil {
			return err
		}
		testwait.Receive(t, source.permit, "source permit held until rollback deadline")
		held = true
		clock.armed = true
		cancel()
		return ctx.Err()
	})
	if !held {
		t.Fatalf("rollback timeout test never acquired the source permit: %v", err)
	}
	source.permit <- struct{}{}
	clock.armed = false
	if !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("transaction error = %v, want both cancellation and bounded rollback timeout", err)
	}
	if err := source.Close(context.Background()); err != nil {
		t.Fatalf("close source after rollback timeout: %v", err)
	}
	assertSyntheticWriterCanLock(t, fixture.Path)
}

func TestOpenCodeReadTransactionReportsRollbackFailure(t *testing.T) {
	fixture := testfixture.MaterializeByName(t, "native-current-rows")
	source := openConcreteSyntheticSource(t, fixture)
	t.Cleanup(func() {
		if err := source.Close(context.Background()); err != nil {
			t.Errorf("close synthetic source: %v", err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := source.withReadTransaction(ctx, func() error {
		if _, err := source.Catalog(ctx); err != nil {
			return err
		}
		testwait.Receive(t, source.permit, "source permit before installing rollback refusal")
		authorizerErr := source.conn.SetAuthorizer(sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
			if action.Type() == sqlite.OpTransaction {
				return sqlite.AuthResultDeny
			}
			return source.authorizeRead(action)
		}))
		source.permit <- struct{}{}
		if authorizerErr != nil {
			return authorizerErr
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "end OpenCode SQLite read transaction") {
		t.Errorf("transaction error = %v, want cancellation joined with rollback failure", err)
	}
	if source.conn.AutocommitEnabled() {
		t.Error("denied rollback unexpectedly ended the transaction")
	}
	if err := source.conn.SetAuthorizer(sqlite.AuthorizeFunc(source.authorizeRead)); err != nil {
		t.Fatalf("restore synthetic source authorizer: %v", err)
	}
	cleanupErr := errors.New("retry rollback")
	source.endReadTransaction(&cleanupErr)
	if !source.conn.AutocommitEnabled() {
		t.Errorf("retry did not roll back the synthetic transaction: %v", cleanupErr)
	}
	if err := source.Close(context.Background()); err != nil {
		t.Fatalf("close synthetic source: %v", err)
	}
	assertSyntheticWriterCanLock(t, fixture.Path)
}

var errSyntheticClose = errors.New("synthetic close failure")

type failingCleanupSource struct{ OpenCodeSQLiteSource }

var _ OpenCodeSQLiteSource = failingCleanupSource{}

func (s failingCleanupSource) Close(ctx context.Context) error {
	return errors.Join(s.OpenCodeSQLiteSource.Close(ctx), errSyntheticClose)
}

func TestOpenCodeMaterializationJoinsCancellationAndCloseError(t *testing.T) {
	fixture := testfixture.MaterializeByName(t, "native-current-rows")
	options := DefaultOpenCodeSQLiteSourceOptions()
	adapter := &OpenCodeAdapter{candidateOptions: options}
	var concrete *zombiezenOpenCodeSQLiteSource
	adapter.candidateOpener = func(ctx context.Context, path OpenCodeSQLiteSourcePath, options OpenCodeSQLiteSourceOptions) (OpenCodeSQLiteSource, error) {
		opened, err := OpenOpenCodeSQLiteSource(ctx, path, options)
		if err != nil {
			return nil, err
		}
		concrete = opened.(*zombiezenOpenCodeSQLiteSource)
		return failingCleanupSource{opened}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := adapter.withOpenCodeMaterializationSource(ctx, fixture.Path, true, func(source OpenCodeSQLiteSource) error {
		if _, err := source.Catalog(ctx); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errSyntheticClose) {
		t.Errorf("materialization error = %v, want cancellation joined with close failure", err)
	}
	if concrete == nil || !concrete.connClosed {
		t.Fatal("real source remained open after injected close failure")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("unexpected cleanup timeout: %v", err)
	}
	assertSyntheticWriterCanLock(t, fixture.Path)
}

func TestOpenCodeMaterializationJoinsCancellationAndCloseTimeout(t *testing.T) {
	fixture := testfixture.MaterializeByName(t, "native-current-rows")
	clock := &cleanupTransactionClock{expire: true, started: make(chan struct{})}
	options := DefaultOpenCodeSQLiteSourceOptions()
	options.clock = clock
	adapter := &OpenCodeAdapter{candidateOptions: options}
	var concrete *zombiezenOpenCodeSQLiteSource
	adapter.candidateOpener = func(ctx context.Context, path OpenCodeSQLiteSourcePath, options OpenCodeSQLiteSourceOptions) (OpenCodeSQLiteSource, error) {
		opened, err := OpenOpenCodeSQLiteSource(ctx, path, options)
		if err == nil {
			concrete = opened.(*zombiezenOpenCodeSQLiteSource)
		}
		return opened, err
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	held := false
	t.Cleanup(func() {
		if concrete != nil && !concrete.connClosed {
			if err := concrete.Close(context.Background()); err != nil {
				t.Errorf("close synthetic source: %v", err)
			}
		}
	})
	err := adapter.withOpenCodeMaterializationSource(ctx, fixture.Path, true, func(source OpenCodeSQLiteSource) error {
		if _, err := source.Catalog(ctx); err != nil {
			return err
		}
		testwait.Receive(t, concrete.permit, "source permit held until close deadline")
		held = true
		clock.armed = true
		cancel()
		return ctx.Err()
	})
	if !held {
		t.Fatalf("close timeout test never acquired the source permit: %v", err)
	}
	concrete.permit <- struct{}{}
	clock.armed = false
	if !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("materialization error = %v, want original cancellation and bounded close timeout", err)
	}
	if err := concrete.Close(context.Background()); err != nil {
		t.Fatalf("retry bounded source close: %v", err)
	}
	assertSyntheticWriterCanLock(t, fixture.Path)
}

type expiredCleanupClock struct{ armed bool }

var _ OpenCodeSQLiteDeadlineClock = (*expiredCleanupClock)(nil)

func (c *expiredCleanupClock) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if c.armed {
		return context.WithDeadline(parent, time.Now().Add(-time.Second))
	}
	return context.WithTimeout(parent, d)
}

func TestOpenCodeMaterializationJoinsRollbackAndCloseTimeouts(t *testing.T) {
	fixture := testfixture.MaterializeByName(t, "native-current-rows")
	clock := &expiredCleanupClock{}
	options := DefaultOpenCodeSQLiteSourceOptions()
	options.clock = clock
	adapter := &OpenCodeAdapter{candidateOptions: options}
	var concrete *zombiezenOpenCodeSQLiteSource
	adapter.candidateOpener = func(ctx context.Context, path OpenCodeSQLiteSourcePath, options OpenCodeSQLiteSourceOptions) (OpenCodeSQLiteSource, error) {
		opened, err := OpenOpenCodeSQLiteSource(ctx, path, options)
		if err == nil {
			concrete = opened.(*zombiezenOpenCodeSQLiteSource)
		}
		return opened, err
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	held := false
	t.Cleanup(func() {
		if concrete != nil && !concrete.connClosed {
			if err := concrete.Close(context.Background()); err != nil {
				t.Errorf("close synthetic source: %v", err)
			}
		}
	})
	err := adapter.withOpenCodeSQLiteSource(ctx, fixture.Path, func(source OpenCodeSQLiteSource) error {
		if _, err := source.Catalog(ctx); err != nil {
			return err
		}
		testwait.Receive(t, concrete.permit, "source permit held through both cleanup deadlines")
		held = true
		clock.armed = true
		cancel()
		return ctx.Err()
	})
	if !held {
		t.Fatalf("cleanup timeout test never acquired the source permit: %v", err)
	}
	concrete.permit <- struct{}{}
	clock.armed = false
	if !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("materialization error = %v, want original cancellation and cleanup deadlines", err)
	}
	if err == nil || !strings.Contains(err.Error(), "end OpenCode SQLite read transaction") || !strings.Contains(err.Error(), "close OpenCode SQLite source") {
		t.Errorf("materialization error = %v, want both rollback and close failures", err)
	}
	if err := concrete.Close(context.Background()); err != nil {
		t.Fatalf("retry bounded source close: %v", err)
	}
	assertSyntheticWriterCanLock(t, fixture.Path)
}

func TestOpenCodeProvenanceSnapshotReleasesCanceledSource(t *testing.T) {
	fixture := testfixture.MaterializeByName(t, "semantic-parity-current")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock := &canceledCloseGate{t: t}
	options := DefaultOpenCodeSQLiteSourceOptions()
	options.clock = clock
	options.cancellationCheckpoint = cancelPendingCleanupRead{cancel: cancel}
	adapter := &OpenCodeAdapter{candidateOptions: options}
	adapter.candidateOpener = func(ctx context.Context, path OpenCodeSQLiteSourcePath, options OpenCodeSQLiteSourceOptions) (OpenCodeSQLiteSource, error) {
		opened, err := OpenOpenCodeSQLiteSource(ctx, path, options)
		if err == nil {
			clock.source = opened.(*zombiezenOpenCodeSQLiteSource)
		}
		return opened, err
	}
	t.Cleanup(func() {
		clock.release()
		if clock.source != nil && !clock.source.connClosed {
			if err := clock.source.Close(context.Background()); err != nil {
				t.Errorf("close synthetic snapshot source: %v", err)
			}
		}
	})
	_, err := adapter.SnapshotOpenCodeProvenance(ctx, fixture.Path, "ses_3cd91f52effeXd3QAJ54jOyzv5", OpenCodeSnapshotOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("snapshot error = %v, want original cancellation", err)
	}
	if err != nil && strings.Contains(err.Error(), fixture.Path) {
		t.Errorf("snapshot error exposed its synthetic source path: %v", err)
	}
	if clock.source == nil || !clock.source.connClosed {
		t.Fatal("snapshot returned without closing the real SQLite source")
	}
	assertSyntheticWriterCanLock(t, fixture.Path)
}

func TestOpenCodeProvenanceSnapshotReportsSanitizedCloseFailure(t *testing.T) {
	fixture := testfixture.MaterializeByName(t, "semantic-parity-current")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	options := DefaultOpenCodeSQLiteSourceOptions()
	options.cancellationCheckpoint = cancelPendingCleanupRead{cancel: cancel}
	adapter := &OpenCodeAdapter{candidateOptions: options}
	var concrete *zombiezenOpenCodeSQLiteSource
	adapter.candidateOpener = func(ctx context.Context, path OpenCodeSQLiteSourcePath, options OpenCodeSQLiteSourceOptions) (OpenCodeSQLiteSource, error) {
		opened, err := OpenOpenCodeSQLiteSource(ctx, path, options)
		if err != nil {
			return nil, err
		}
		concrete = opened.(*zombiezenOpenCodeSQLiteSource)
		return failingCleanupSource{opened}, nil
	}
	_, err := adapter.SnapshotOpenCodeProvenance(ctx, fixture.Path, "ses_3cd91f52effeXd3QAJ54jOyzv5", OpenCodeSnapshotOptions{})
	if err == nil {
		t.Fatal("snapshot succeeded despite cancellation and an injected close failure")
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "close snapshot source") || !strings.Contains(err.Error(), "read message rows") {
		t.Errorf("snapshot error = %v, want cancellation, read refusal, and sanitized close failure", err)
	}
	if strings.Contains(err.Error(), fixture.Path) || strings.Contains(err.Error(), errSyntheticClose.Error()) {
		t.Errorf("snapshot exposed a raw dependency failure: %v", err)
	}
	if concrete == nil || !concrete.connClosed {
		t.Fatal("snapshot failed to close the real source")
	}
	assertSyntheticWriterCanLock(t, fixture.Path)
}

func TestOpenCodeMaterializationRetainsCancellationWhenCallbackReturnsNil(t *testing.T) {
	for _, c := range loadOpenCodeCleanupCases(t) {
		if !c.Cancel || c.PreviewSlice {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			fixture := testfixture.MaterializeByName(t, c.Fixture)
			adapter := &OpenCodeAdapter{candidateOptions: DefaultOpenCodeSQLiteSourceOptions(), candidateOpener: OpenOpenCodeSQLiteSource}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := adapter.withOpenCodeMaterializationSource(ctx, fixture.Path, c.Snapshot, func(source OpenCodeSQLiteSource) error {
				if _, err := source.Catalog(ctx); err != nil {
					return err
				}
				cancel()
				return nil
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("materialization error = %v, want cancellation despite the callback result", err)
			}
			assertSyntheticWriterCanLock(t, fixture.Path)
		})
	}
}
