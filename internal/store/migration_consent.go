package store

import (
	"fmt"
	"io"
	"os"
	"strings"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// MigrationV33 is the schema version that renames harness identifiers in the
// sessions and daily_summary_harness tables (claude → claude-code,
// gemini → gemini-cli). Crossing this boundary on an existing database is a
// breaking change, so peasant requires explicit consent before running it.
const migrationV33TargetVersion = 33

// MigrationConsent is the callback Open invokes when it detects a pending V33
// migration on a database that already has user data. The callback should
// return true to proceed (Open will then create a backup and run the migration)
// or false to abort (Open will return ErrMigrationRefused).
//
// Implementations typically prompt the user on a TTY. Non-interactive callers
// can return true unconditionally to opt in, or false to gate the upgrade.
type MigrationConsent func(ctx MigrationConsentContext) (bool, error)

// MigrationConsentContext is passed to a MigrationConsent callback so it can
// describe the upcoming migration to the user.
type MigrationConsentContext struct {
	DBPath         string // path to the SQLite file about to be migrated
	CurrentVersion int    // user_version before migration
	TargetVersion  int    // user_version after migration completes
	BackupPath     string // path where Open will copy the DB before migrating
	SessionsCount  int    // number of rows in the sessions table (for context)
}

// ErrMigrationRefused is returned by Open when a MigrationConsent callback
// returns false.
var ErrMigrationRefused = fmt.Errorf("store: migration refused by consent callback")

// PromptMigrationConsentOnTTY returns a MigrationConsent that prompts on stderr
// and reads a y/N answer from stdin. If stdin is not a TTY, it returns true
// without prompting (so non-interactive contexts proceed silently — pair with
// stricter consent in scripts that need it).
func PromptMigrationConsentOnTTY() MigrationConsent {
	// Detect the TTY here (it needs the concrete *os.File), then delegate the
	// prompt/parse logic to newConsentPrompter so it stays unit-testable.
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		interactive = true
	}
	return newConsentPrompter(os.Stdin, os.Stderr, interactive)
}

// newConsentPrompter builds a MigrationConsent that writes the migration prompt
// to out and reads a y/N answer from in. When interactive is false (e.g. stdin
// is not a TTY) it proceeds without prompting. Extracted from
// PromptMigrationConsentOnTTY so the prompt/parse logic is testable with an
// injected reader and writer (no real TTY required).
func newConsentPrompter(in io.Reader, out io.Writer, interactive bool) MigrationConsent {
	return func(ctx MigrationConsentContext) (bool, error) {
		// Non-interactive: skip the prompt, proceed.
		if !interactive {
			return true, nil
		}

		fmt.Fprintf(out,
			"\npeasant will run a one-time database migration (v%d → v%d).\n"+
				"  What changes: harness identifiers in sessions are renamed\n"+
				"    'claude'  → 'claude-code'\n"+
				"    'gemini'  → 'gemini-cli'\n"+
				"  Database:    %s  (%d sessions)\n"+
				"  Backup:      %s  (created before migration)\n"+
				"\n"+
				"Proceed? [y/N] ",
			ctx.CurrentVersion, ctx.TargetVersion,
			ctx.DBPath, ctx.SessionsCount, ctx.BackupPath,
		)
		var line string
		if _, err := fmt.Fscanln(in, &line); err != nil {
			// Empty input (just Enter) → treat as N.
			return false, nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		}
		return false, nil
	}
}

// maybeRunV33Consent inspects the SQLite file at dbPath. If it is at a schema
// version below V33 AND already contains session data, the consent callback is
// invoked. On consent the DB file is copied to backupPath. If the callback is
// nil, the migration proceeds silently (consistent with prior behavior — used
// by tests and fresh-install paths).
//
// Caller invariant: dbPath must be a real on-disk file (not :memory:). Callers
// that open in-memory stores skip this check entirely.
func maybeRunV33Consent(dbPath string, consent MigrationConsent) error {
	if dbPath == "" || dbPath == ":memory:" {
		return nil
	}
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		// Fresh install — sqlitemigration will create the DB at the target
		// version directly. No consent needed.
		return nil
	} else if err != nil {
		return fmt.Errorf("store: stat DB before migration: %w", err)
	}

	// Open a read-only connection just to inspect user_version + row counts.
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadOnly)
	if err != nil {
		return fmt.Errorf("store: open DB for migration check: %w", err)
	}
	defer conn.Close()

	var currentVersion int
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA user_version", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			currentVersion = int(stmt.ColumnInt64(0))
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}

	// Not crossing the V33 boundary → nothing to gate.
	if currentVersion >= migrationV33TargetVersion {
		return nil
	}

	// Count sessions to determine whether this is a fresh DB (just had the
	// schema created but never used) or a real user DB with data to migrate.
	var sessionsCount int
	err = sqlitex.ExecuteTransient(conn, "SELECT COUNT(*) FROM sessions", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sessionsCount = int(stmt.ColumnInt64(0))
			return nil
		},
	})
	if err != nil {
		// Sessions table may not exist on very old schemas — treat as empty,
		// no consent needed.
		return nil
	}
	if sessionsCount == 0 {
		return nil
	}

	if consent == nil {
		// No callback configured — proceed (matches legacy behavior).
		return nil
	}

	backupPath := dbPath + ".pre-v33.bak"
	proceed, err := consent(MigrationConsentContext{
		DBPath:         dbPath,
		CurrentVersion: currentVersion,
		TargetVersion:  migrationV33TargetVersion,
		BackupPath:     backupPath,
		SessionsCount:  sessionsCount,
	})
	if err != nil {
		return fmt.Errorf("store: migration consent callback: %w", err)
	}
	if !proceed {
		return ErrMigrationRefused
	}

	// Close the read-only connection before copying so the file is in a
	// well-defined state. (SQLite WAL mode keeps changes in -wal/-shm files,
	// but at this point we haven't taken any writes.)
	_ = conn.Close()

	if err := copyFile(dbPath, backupPath); err != nil {
		return fmt.Errorf("store: backup DB to %s: %w", backupPath, err)
	}
	return nil
}

// copyFile copies src to dst, replacing dst if it exists. Mode 0o600 by default
// (peasant DB is private).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
