package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/filelock"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// upgradeDataFill is a data fill that commits with its schema step. It runs
// on the step's connection, inside the step's transaction, after the step's
// script. report sends the fill's count to the upgrade's progress events.
type upgradeDataFill func(conn *sqlite.Conn, report func(done, of int64)) error

// upgradeDataFills maps a schema version to the data fill its step needs. A
// fill is registered here, never run after the chain: a process that dies
// between a schema step and a later fill would leave the fill undone with no
// record that it is owed.
var upgradeDataFills = map[int]upgradeDataFill{
	conversationTurnCountBackfillSchemaVersion: func(conn *sqlite.Conn, report func(done, of int64)) error {
		filled, err := backfillConversationTurnCountsOnConn(conn, report)
		if err != nil {
			return err
		}
		logConversationTurnCountBackfill(filled)
		return nil
	},
}

// isStoreFilePath reports whether dbPath names a database file on disk, as
// opposed to an in-memory database.
func isStoreFilePath(dbPath string) bool {
	return dbPath != "" && dbPath != ":memory:"
}

// sqliteHeaderMagic is the first 16 bytes of every SQLite database file.
const sqliteHeaderMagic = "SQLite format 3\x00"

// peekSchemaVersion reads the schema version of the database file at dbPath
// without opening a connection that could write. exists is false when there
// is no file. When no write-ahead log holds frames, the version is read from
// the file header (bytes 60 to 63); otherwise it is read through a read-only
// connection so committed frames in the log are seen. A file too short to
// hold a header is a new, empty database: version 0.
func peekSchemaVersion(dbPath string) (version int, exists bool, err error) {
	info, err := os.Stat(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("store: stat %s to read its schema version: %w; nothing was changed; check the path and its permissions", dbPath, err)
	}
	if info.Size() < 100 {
		return 0, true, nil
	}
	if wal, walErr := os.Stat(dbPath + "-wal"); walErr == nil && wal.Size() > 0 {
		version, err := SchemaVersionAt(dbPath)
		return version, true, err
	}
	file, err := os.Open(dbPath)
	if err != nil {
		return 0, true, fmt.Errorf("store: open %s to read its schema version: %w; nothing was changed; check the file's read permission", dbPath, err)
	}
	defer file.Close()
	header := make([]byte, 100)
	if _, err := io.ReadFull(file, header); err != nil {
		return 0, true, fmt.Errorf("store: read the header of %s for its schema version: %w; nothing was changed", dbPath, err)
	}
	if string(header[:16]) != sqliteHeaderMagic {
		// Not a plain SQLite header: let a real connection report what the
		// file is.
		version, err := SchemaVersionAt(dbPath)
		return version, true, err
	}
	return int(int32(binary.BigEndian.Uint32(header[60:64]))), true, nil
}

// newerSchemaError is the refusal for a store a newer build upgraded.
func newerSchemaError(dbPath string, version int) error {
	return fmt.Errorf("store: open %s: database schema is version %d, but this build understands only up to version %d (the database was upgraded by a newer Peasant); nothing was changed; install that version or newer to open it",
		dbPath, version, CurrentSchemaVersion())
}

// PlanUpgrade reads the store at dbPath and reports what an upgrade would
// do. It changes no file: the database is opened read-only, with no sidecar
// created. From == To gives no steps; a missing file and a new, empty file
// are created at the head schema by Open and have no steps either. A store
// upgraded by a newer build is refused.
func PlanUpgrade(dbPath string) (UpgradePlan, error) {
	plan, _, err := planUpgrade(dbPath)
	return plan, err
}

// planUpgrade is PlanUpgrade plus the notices the plan raised.
func planUpgrade(dbPath string) (UpgradePlan, []string, error) {
	head := CurrentSchemaVersion()
	current := UpgradePlan{DBPath: dbPath, From: head, To: head}
	if !isStoreFilePath(dbPath) {
		return current, nil, nil
	}
	version, exists, err := peekSchemaVersion(dbPath)
	if err != nil {
		return UpgradePlan{}, nil, err
	}
	if !exists || version == head {
		return current, nil, nil
	}
	if version > head {
		return UpgradePlan{}, nil, newerSchemaError(dbPath, version)
	}
	conn, err := openReadOnlyNoSidecars(dbPath)
	if err != nil {
		return UpgradePlan{}, nil, fmt.Errorf("store: open %s read-only to plan its upgrade: %w; no files were changed; check the path and read permissions", dbPath, err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA query_only = ON`, nil); err != nil {
		return UpgradePlan{}, nil, fmt.Errorf("store: pin %s read-only to plan its upgrade: %w; no files were changed", dbPath, err)
	}
	return planUpgradeOnConn(conn, dbPath)
}

// planUpgradeOnConn builds the plan from an open connection. It only reads.
func planUpgradeOnConn(conn *sqlite.Conn, dbPath string) (UpgradePlan, []string, error) {
	head := CurrentSchemaVersion()
	plan := UpgradePlan{DBPath: dbPath, From: head, To: head}
	fresh, err := databaseIsFresh(conn)
	if err != nil {
		return UpgradePlan{}, nil, fmt.Errorf("store: probe whether %s is a new database while planning its upgrade: %w; nothing was changed", dbPath, err)
	}
	if fresh {
		return plan, nil, nil
	}
	version, err := readPragmaInt(conn, "PRAGMA user_version")
	if err != nil {
		return UpgradePlan{}, nil, fmt.Errorf("store: read the schema version of %s while planning its upgrade: %w; nothing was changed", dbPath, err)
	}
	if int(version) > head {
		return UpgradePlan{}, nil, newerSchemaError(dbPath, int(version))
	}
	plan.From = int(version)
	if plan.From == head {
		return plan, nil, nil
	}

	if isStoreFilePath(dbPath) {
		if info, statErr := os.Stat(dbPath); statErr == nil {
			plan.StoreBytes = info.Size()
		}
		if free, ok := migrateFreeBytes(filepath.Dir(dbPath)); ok {
			plan.FreeBytesKnown = true
			if free > math.MaxInt64 {
				free = math.MaxInt64
			}
			plan.FreeBytes = int64(free)
		}
	}

	var notices []string
	// basisBytes holds each basis the plan has measured, so a basis shared
	// by several steps is estimated once.
	basisBytes := map[upgradeDiskBasis]int64{}
	for produced := plan.From + 1; produced <= head; produced++ {
		entry, ok := upgradeStepCatalog[produced]
		if !ok {
			return UpgradePlan{}, nil, fmt.Errorf("store: plan the upgrade of %s: schema version %d has no entry in the upgrade step catalog; nothing was changed; this is a defect in this build: %s", dbPath, produced, missingCatalogEntryFix(produced))
		}
		step := UpgradeStep{Version: produced, Kind: UpgradeStepSchema, Title: entry.title, Cost: entry.cost, Counts: entry.counts}
		if produced == searchRepairSchemaVersion && plan.From == searchRepairSchemaVersion-1 {
			repair, notice, err := searchRepairStep(conn, step)
			if err != nil {
				return UpgradePlan{}, nil, err
			}
			step = repair
			if notice != "" {
				notices = append(notices, notice)
			}
		}
		need := entry.diskNeedFor(step.Cost)
		size, measured := basisBytes[need.basis]
		if !measured {
			var notice string
			size, notice = diskBasisBytes(conn, need.basis, plan.StoreBytes)
			basisBytes[need.basis] = size
			if notice != "" {
				notices = append(notices, notice)
			}
		}
		// Each step commits before the next starts, so the log space of one
		// step is reused by the next: the plan needs room for its hungriest
		// step, not for the sum.
		if asked := int64(math.Ceil(float64(size) * need.factor)); asked > plan.RequiredFreeBytes {
			plan.RequiredFreeBytes = asked
		}
		plan.Steps = append(plan.Steps, step)
	}
	return plan, notices, nil
}

// missingCatalogEntryFix says exactly what to add for a migration that has
// no catalog entry.
func missingCatalogEntryFix(version int) string {
	return fmt.Sprintf("add `%d: {title: \"<lowercase phrase>\", cost: <UpgradeCost...>}` to upgradeStepCatalog in internal/store/upgrade_catalog.go, in version order (with `counts:` when the step reports a count, and `disk:` only when the step copies a table), and add `- \"%d\"` to requiredNames in internal/store/testdata/upgrade_step_catalog.manifest.yaml", version, version)
}

// searchRepairStep resolves the cost of the search trigger repair for a
// store that is exactly one version below it. The repair is long only when
// the store carries stale search results: then it rebuilds the whole index
// over the stored text. A store that crosses the previous migration and the
// repair in one run has no window between them and keeps the quick cost.
//
// A store missing the live search index cannot be repaired and gets an
// actionable error. A store missing only the retired index gets a warning
// and the long cost: the repair creates the retired index empty before it
// compares, so it will find every indexed entry unmatched and rebuild.
func searchRepairStep(conn *sqlite.Conn, step UpgradeStep) (UpgradeStep, string, error) {
	consolidated, err := searchConsolidatedOnConn(conn)
	if err != nil {
		return UpgradeStep{}, "", err
	}
	if consolidated {
		return step, "", nil
	}
	if !tableExistsOnConn(conn, "session_search_fts") {
		return UpgradeStep{}, "", fmt.Errorf("store: plan the search index repair: the search index table session_search_fts is missing, so the repair cannot run. " +
			"why: a store at this schema always carries that table; the database was changed outside peasant or is damaged. " +
			"where: upgrade plan, before any step ran. " +
			"when: while reading the store; nothing was changed. " +
			"meaning: the store cannot be upgraded as it is, and commands that need the new schema will not run. " +
			"fix: restore the database from a backup, or move it aside and run `peasant harvest` to rebuild it from your transcripts")
	}
	long := UpgradeStep{Version: step.Version, Kind: step.Kind, Title: searchRepairLongTitle, Cost: UpgradeCostScalesWithText, Counts: step.Counts}
	if !tableExistsOnConn(conn, "session_entries_fts") {
		return long, searchIndexPrecheckWarning + ": the retired index table session_entries_fts is missing; continuing: the repair rebuilds the search index", nil
	}
	residue, err := searchIndexResiduePresent(conn)
	if err != nil {
		return UpgradeStep{}, "", fmt.Errorf("%w; nothing was changed; restore the database from a backup if the error persists", err)
	}
	if residue {
		return long, "", nil
	}
	return step, "", nil
}

// searchIndexRebuildNotice is the one line an upgrade prints before the
// search repair rebuilds the index: it names the cause, the cost, and the
// scope.
const searchIndexRebuildNotice = "rebuilding the search index once to remove stale results left by 0.16.1; this reads all stored text and can take several minutes"

// searchIndexPrecheckWarning prefixes the one line an upgrade prints when
// the store's search tables are not the pair the repair expects.
const searchIndexPrecheckWarning = "warning: could not check the search index for stale results left by 0.16.1"

// upgradeLockWaitNotice is the one line an upgrade prints while another run
// holds the data directory's lock.
const upgradeLockWaitNotice = "waiting: another peasant run holds the lock on this data directory; the store upgrade starts when it finishes; ctrl+c cancels"

// lockUpgrade takes the data directory's run lock, the one a harvest holds,
// so an upgrade and a harvest never write the store together. It waits for
// a holder and honors cancellation.
func lockUpgrade(ctx context.Context, dbPath string, notices io.Writer) (func(), error) {
	path := filepath.Join(filepath.Dir(dbPath), ingest.RunLockFileName)
	waiting := false
	for {
		release, err := filelock.Acquire(path, time.Now().Add(defaults.StoreUpgradeLockPoll))
		if err == nil {
			return func() { _ = release() }, nil
		}
		if !filelock.IsDeadline(err) {
			return nil, fmt.Errorf("store: upgrade %s: take the data directory lock %s: %w; nothing was changed; check that the data directory is writable", dbPath, path, err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("store: upgrade %s: cancelled while waiting for another peasant run that holds %s: %w; nothing was changed; run `peasant migrate --schema-only` again when that run finishes", dbPath, path, ctxErr)
		}
		if !waiting {
			waiting = true
			writeUpgradeNotice(notices, upgradeLockWaitNotice)
		}
	}
}

// writeUpgradeNotice writes one advisory line. A nil writer discards it and
// a failed write is ignored.
func writeUpgradeNotice(w io.Writer, line string) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintln(w, line)
}

// walFileBytes is the current size of the store's write-ahead log, or 0
// when there is none.
func walFileBytes(dbPath string) int64 {
	info, err := os.Stat(dbPath + "-wal")
	if err != nil {
		return 0
	}
	return info.Size()
}

// upgradeStepProgress serializes the events of one running step and carries
// its latest count between the periodic reports.
type upgradeStepProgress struct {
	mu       sync.Mutex
	emit     func(UpgradeEvent)
	dbPath   string
	step     UpgradeStep
	index    int
	total    int
	started  time.Time
	walStart int64
	done, of int64
}

func (p *upgradeStepProgress) send(kind UpgradeEventKind) (time.Duration, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := time.Since(p.started)
	grown := walFileBytes(p.dbPath) - p.walStart
	if grown < 0 {
		grown = 0
	}
	if p.emit != nil {
		event := UpgradeEvent{Kind: kind, Step: p.step, Index: p.index, Total: p.total, Unit: p.step.Counts, WALBytes: grown, Elapsed: elapsed}
		if p.step.Counts != UpgradeUnitNone {
			event.Done, event.Of = p.done, p.of
		}
		p.emit(event)
	}
	return elapsed, grown
}

func (p *upgradeStepProgress) count(done, of int64) {
	p.mu.Lock()
	p.done, p.of = done, of
	p.mu.Unlock()
	p.send(UpgradeEventStepProgress)
}

// Upgrade applies every pending migration to the store at dbPath. It is the
// only function that applies migrations to a store that holds data; Open
// refuses a behind store with SchemaBehindError and changes nothing.
//
// The upgrade holds the data directory's run lock for its whole run, so a
// harvest or a second upgrade waits. It refuses before the first step when
// the filesystem reports less free space than the plan asks for (the
// largest need among the planned steps, each its disk factor times its
// basis);
// unknown free space warns before a long step and continues. Each step is one transaction:
// the migration's script, any data fill the step needs, and the version
// stamp commit together. Cancelling ctx interrupts the running step, which
// rolls back; the steps before it stay applied, and the error is an
// *UpgradeInterruptedError. After the last step the write-ahead log is
// truncated, best effort.
//
// The report lists the steps that finished, also when Upgrade returns an
// error. A store that is already current, a missing file and a new, empty
// file give a report with no steps and no error.
func Upgrade(ctx context.Context, dbPath string, opts UpgradeOptions) (report UpgradeReport, err error) {
	head := CurrentSchemaVersion()
	report = UpgradeReport{From: head, To: head}
	if !isStoreFilePath(dbPath) {
		return report, nil
	}
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return report, nil
		}
		return report, fmt.Errorf("store: upgrade %s: stat the database: %w; nothing was changed; check the path and its permissions", dbPath, statErr)
	}
	// The cheap header read keeps a current store from taking the lock or
	// opening a writable connection at all.
	if version, exists, peekErr := peekSchemaVersion(dbPath); peekErr != nil {
		return report, peekErr
	} else if exists && version == head {
		return report, nil
	} else if version > head {
		return report, newerSchemaError(dbPath, version)
	}

	// A first, read-only plan refuses what can be refused before anything
	// writable is opened: a store that cannot be planned, and one whose
	// filesystem has too little free space. The plan is read again under
	// the lock, where it is authoritative.
	early, _, err := planUpgrade(dbPath)
	if err != nil {
		return report, err
	}
	if len(early.Steps) == 0 {
		return UpgradeReport{From: early.From, To: early.To}, nil
	}
	if early.RequiredFreeBytes > 0 && early.FreeBytesKnown && early.FreeBytes < early.RequiredFreeBytes {
		return report, &UpgradeDiskError{DBPath: dbPath, FreeBytes: early.FreeBytes, RequiredBytes: early.RequiredFreeBytes}
	}

	release, err := lockUpgrade(ctx, dbPath, opts.Notices)
	if err != nil {
		return report, err
	}
	defer release()

	// The harness identifier rename is the one breaking historical step:
	// ask first when a callback is set, and copy the database before it.
	if err := maybeRunV33Consent(dbPath, opts.Consent); err != nil {
		return report, err
	}

	pool, err := newSQLitePool(dbPath, sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
	if err != nil {
		return report, fmt.Errorf("store: upgrade %s: open the database: %w; nothing was changed", dbPath, err)
	}
	defer func() {
		if closeErr := pool.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("store: upgrade %s: close the database after the upgrade: %w; every step committed; run the command again", dbPath, closeErr)
		}
	}()
	conn, err := pool.Take(context.Background())
	if err != nil {
		return report, fmt.Errorf("store: upgrade %s: take a connection: %w; nothing was changed", dbPath, err)
	}
	connReturned := false
	putConn := func() {
		if connReturned {
			return
		}
		connReturned = true
		// A cancelled step can leave the interrupt set and its transaction
		// open; clear both before the connection goes back to the pool.
		conn.SetInterrupt(nil)
		if !conn.AutocommitEnabled() {
			_ = sqlitex.ExecuteTransient(conn, "ROLLBACK;", nil)
		}
		pool.Put(conn)
	}
	defer putConn()

	plan, notices, err := planUpgradeOnConn(conn, dbPath)
	if err != nil {
		return report, err
	}
	report.From, report.To = plan.From, plan.To
	if len(plan.Steps) == 0 {
		return report, nil
	}
	searchWarned := false
	for _, notice := range notices {
		writeUpgradeNotice(opts.Notices, notice)
		searchWarned = searchWarned || strings.HasPrefix(notice, searchIndexPrecheckWarning)
	}
	if plan.RequiredFreeBytes > 0 {
		switch {
		case !plan.FreeBytesKnown && !plan.HasLongStep():
			// A quick plan needs next to nothing; no warning for it.
		case !plan.FreeBytesKnown:
			writeUpgradeNotice(opts.Notices, fmt.Sprintf("warning: could not read the free disk space for %s; the upgrade asks for about %d bytes free and continues without the check", filepath.Dir(dbPath), plan.RequiredFreeBytes))
		case plan.FreeBytes < plan.RequiredFreeBytes:
			return report, &UpgradeDiskError{DBPath: dbPath, FreeBytes: plan.FreeBytes, RequiredBytes: plan.RequiredFreeBytes}
		}
	}
	if err := refuseUnmappableCaptureFormats(conn); err != nil {
		return report, err
	}

	for i, step := range plan.Steps {
		interrupted := func(cause error) error {
			return &UpgradeInterruptedError{DBPath: dbPath, Step: step, Finished: i, Total: len(plan.Steps), At: plan.From + i, Cause: cause}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return report, interrupted(ctxErr)
		}
		// A plan that already warned about the search tables said that the
		// repair rebuilds; the rebuild notice would repeat it.
		if step.Version == searchRepairSchemaVersion && step.Cost.Long() && !searchWarned {
			writeUpgradeNotice(opts.Notices, searchIndexRebuildNotice)
		}
		progress := &upgradeStepProgress{emit: opts.Progress, dbPath: dbPath, step: step, index: i + 1, total: len(plan.Steps), started: time.Now(), walStart: walFileBytes(dbPath)}
		progress.send(UpgradeEventStepStarted)

		stopTicks := make(chan struct{})
		ticksDone := make(chan struct{})
		go func() {
			defer close(ticksDone)
			ticker := time.NewTicker(defaults.StoreUpgradeProgressInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stopTicks:
					return
				case <-ticker.C:
					progress.send(UpgradeEventStepProgress)
				}
			}
		}()
		stepErr := applyUpgradeStep(ctx, conn, step.Version, progress.count, opts.afterDataFill)
		close(stopTicks)
		<-ticksDone
		if stepErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return report, interrupted(ctxErr)
			}
			return report, fmt.Errorf("store: upgrade %s: step %d of %d (%s) failed and rolled back: %w; the %d step(s) before it stay applied and the store is at schema %d; fix the cause, then run `peasant migrate --schema-only` again",
				dbPath, i+1, len(plan.Steps), step.Title, stepErr, i, plan.From+i)
		}
		elapsed, grown := progress.send(UpgradeEventStepFinished)
		report.Steps = append(report.Steps, UpgradeStepResult{Step: step, Duration: elapsed, WALBytes: grown})
	}
	putConn()

	// The opaque identifier rewrite is a Go phase of an old migration. It
	// checks the tables before it writes, so it is a no-op on every store
	// that already carries opaque identifiers.
	if err := applyV23DataMigration(pool); err != nil {
		return report, fmt.Errorf("store: upgrade %s: apply the opaque identifier data migration: %w; every schema step committed; run `peasant migrate --schema-only` again", dbPath, err)
	}
	truncateWAL(pool)
	return report, nil
}

// applyUpgradeStep applies the one migration that produces version. This is
// the only call into the migration engine: the schema is cut at the step, so
// the engine applies exactly one migration and the caller can report it,
// stop between steps, and attach the step's data fill to its transaction.
func applyUpgradeStep(ctx context.Context, conn *sqlite.Conn, version int, report func(done, of int64), afterDataFill func(version int) error) error {
	cut := sqlitemigration.Schema{
		AppID:      dbSchema.AppID,
		Migrations: dbSchema.Migrations[:version],
	}
	if len(dbSchema.MigrationOptions) > version {
		cut.MigrationOptions = dbSchema.MigrationOptions[:version]
	} else {
		cut.MigrationOptions = dbSchema.MigrationOptions
	}
	if version == len(dbSchema.Migrations) {
		cut.RepeatableMigration = dbSchema.RepeatableMigration
	}
	return sqlitemigration.MigrateWithAfterScript(ctx, conn, cut, func(conn *sqlite.Conn, produced int32) error {
		if int(produced) != version {
			return fmt.Errorf("store: the migration engine applied schema version %d while the upgrade asked for %d; another process changed the store during the upgrade", produced, version)
		}
		if fill := upgradeDataFills[version]; fill != nil {
			if err := fill(conn, report); err != nil {
				return err
			}
		}
		if afterDataFill != nil {
			if err := afterDataFill(version); err != nil {
				return err
			}
		}
		// A cancellation that arrived while the step ran must not commit
		// it: the caller reports the step as rolled back.
		return ctx.Err()
	})
}

// truncateWAL checkpoints the write-ahead log and truncates it to zero
// bytes. It is best effort: a reader holding the log open keeps the
// checkpoint from finishing, and the next checkpoint does the work then.
func truncateWAL(pool *sqlitex.Pool) {
	conn, err := pool.Take(context.Background())
	if err != nil {
		return
	}
	defer pool.Put(conn)
	_ = sqlitex.ExecuteTransient(conn, "PRAGMA wal_checkpoint(TRUNCATE);", nil)
}

// IsSchemaBehind reports whether err is, or wraps, a SchemaBehindError.
func IsSchemaBehind(err error) bool {
	var behind *SchemaBehindError
	return errors.As(err, &behind)
}
