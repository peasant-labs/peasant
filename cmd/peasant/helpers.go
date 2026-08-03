package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/redact"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// dataDirOverride returns the value of the persistent --data-dir flag (an
// XDG_DATA_HOME override), or "" when the flag is unset or not registered on
// cmd. Using Lookup (not GetString) means a command executed WITHOUT the root's
// persistent flag — e.g. a unit test building a subcommand directly — simply
// falls back to the environment instead of erroring. This is the parallel-safe
// path injection: the value lives on the per-invocation command instance, never
// in process-global env.
func dataDirOverride(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	if f := cmd.Flags().Lookup("data-dir"); f != nil {
		return f.Value.String()
	}
	return ""
}

// configDirOverride returns the value of the persistent --config-dir flag (an
// XDG_CONFIG_HOME override), or "" when unset / not registered on cmd. See
// dataDirOverride for the parallel-safety rationale.
func configDirOverride(cmd *cobra.Command) string {
	if f := commandFlag(cmd, "config-dir"); f != nil {
		return f.Value.String()
	}
	return ""
}

// resolveConfigPath applies CLI precedence once for both config reads and
// writes: an explicit --config wins, otherwise --config-dir selects the XDG
// config home used to derive peasant/config.yaml.
func resolveConfigPath(cmd *cobra.Command) string {
	if f := commandFlag(cmd, "config"); f != nil && f.Changed {
		return f.Value.String()
	}
	return defaults.ResolveConfigFilePathWith(configDirOverride(cmd)).String()
}

func commandFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if cmd == nil {
		return nil
	}
	if flag := cmd.Flags().Lookup(name); flag != nil {
		return flag
	}
	if flag := cmd.InheritedFlags().Lookup(name); flag != nil {
		return flag
	}
	if root := cmd.Root(); root != nil {
		return root.PersistentFlags().Lookup(name)
	}
	return nil
}

// stateDirOverride returns the value of the persistent --state-dir flag (an
// XDG_STATE_HOME override), or "" when unset / not registered on cmd. See
// dataDirOverride for the parallel-safety rationale.
func stateDirOverride(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	if f := cmd.Flags().Lookup("state-dir"); f != nil {
		return f.Value.String()
	}
	return ""
}

// openDB opens the analytics store, creating the data directory if needed.
// Returns the store, a cleanup function, and any error. The data directory is
// resolved from the --data-dir flag on cmd (if set), else the environment.
// For best-effort DB opens (web, tui) where failure is non-fatal, use inline logic instead.
func openDB(cmd *cobra.Command) (*store.Store, func(), error) {
	override := dataDirOverride(cmd)
	dataDir := string(defaults.ResolveDataDirPathWith(override))
	dbPath := string(defaults.ResolveDBFilePathWith(override))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		return nil, func() {}, fmt.Errorf("create data directory: %w", err)
	}
	db, err := store.Open(dbPath, store.WithMigrationConsent(store.PromptMigrationConsentOnTTY()))
	if err != nil {
		return nil, func() {}, fmt.Errorf("open analytics store: %w", err)
	}
	return db, func() { db.Close() }, nil
}

// resolveXDGPaths returns the current XDG base directories for redaction rules,
// honoring the --data-dir flag override on cmd for the data home.
func resolveXDGPaths(cmd *cobra.Command) redact.XDGPaths {
	return redact.XDGPaths{
		DataHome:   string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd))),
		ConfigHome: string(defaults.ResolveConfigDirPathWith(configDirOverride(cmd))),
		StateHome:  string(defaults.ResolveStateDirPathWith(stateDirOverride(cmd))),
	}
}

// loadConfig loads the config file with standard filesystem and git resolver.
func loadConfig(cfgPath string) (*config.Config, error) {
	fs := &ingest.OSFileSystem{}
	git := &ingest.ExecGitResolver{}
	return config.Load(cfgPath, fs, git)
}

// resolveSessionIDs reads session IDs from --session or --session-from-file flags.
// Exactly one of the two flags must be provided; providing both or neither
// is an error. When --session-from-file is provided the file is read line by line;
// blank lines and lines that consist only of whitespace are skipped.
//
// Returns an actionable error when:
//   - neither flag is set (caller must supply exactly one)
//   - both flags are set (ambiguous input source)
//   - the file named by --session-from-file cannot be opened or read
func resolveSessionIDs(cmd *cobra.Command) ([]string, error) {
	sessionFlag, err := cmd.Flags().GetString("session")
	if err != nil {
		return nil, fmt.Errorf(
			"resolveSessionIDs: read --session flag: %w\n"+
				"What went wrong: the --session flag is not registered on command %q.\n"+
				"Fix: ensure buildExportSessionsCommand/buildExportAnnotationsCommand registers a --session string flag.",
			err, cmd.Name(),
		)
	}
	sessionFromFileFlag, err := cmd.Flags().GetString("session-from-file")
	if err != nil {
		return nil, fmt.Errorf(
			"resolveSessionIDs: read --session-from-file flag: %w\n"+
				"What went wrong: the --session-from-file flag is not registered on command %q.\n"+
				"Fix: ensure buildExportSessionsCommand/buildExportAnnotationsCommand registers a --session-from-file string flag.",
			err, cmd.Name(),
		)
	}

	sessionSet := cmd.Flags().Changed("session")
	fromFileSet := cmd.Flags().Changed("session-from-file")

	if sessionSet && fromFileSet {
		return nil, fmt.Errorf(
			"resolveSessionIDs: conflicting flags on command %q\n"+
				"What went wrong: both --session and --session-from-file were provided.\n"+
				"Why: exactly one source of session IDs is required.\n"+
				"Fix: provide --session <id> for a single session, or --session-from-file <path> for a file of IDs, not both.",
			cmd.Name(),
		)
	}
	if !sessionSet && !fromFileSet {
		return nil, fmt.Errorf(
			"resolveSessionIDs: missing required flag on command %q\n"+
				"What went wrong: neither --session nor --session-from-file was provided.\n"+
				"Why: a session ID source is required to identify what to export.\n"+
				"Fix: provide --session <session-id> for a single session, or --session-from-file <path> for a newline-delimited file of session IDs.",
			cmd.Name(),
		)
	}

	if sessionSet {
		return []string{sessionFlag}, nil
	}

	// Read from file: one session ID per line, skip blank/whitespace-only lines.
	f, err := os.Open(sessionFromFileFlag)
	if err != nil {
		return nil, fmt.Errorf(
			"resolveSessionIDs: open --session-from-file %q: %w\n"+
				"What went wrong: the file %q could not be opened.\n"+
				"Why: the file may not exist, or the process may lack read permission.\n"+
				"Fix: verify the file path exists and is readable, then retry.",
			sessionFromFileFlag, err, sessionFromFileFlag,
		)
	}
	defer f.Close()

	var ids []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		ids = append(ids, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf(
			"resolveSessionIDs: read --session-from-file %q: %w\n"+
				"What went wrong: an I/O error occurred while reading the session ID file.\n"+
				"Fix: check the file is not corrupt or being written to concurrently, then retry.",
			sessionFromFileFlag, err,
		)
	}
	return ids, nil
}

// resolveOptionalSessionIDs reads session IDs from --session or --session-from-file flags.
// Unlike resolveSessionIDs, it is NOT an error when neither flag is set — in that case
// (nil, nil) is returned to indicate "all sessions". Providing both flags is still an error.
//
// Returns an actionable error when:
//   - both flags are set (ambiguous input source)
//   - the file named by --session-from-file cannot be opened or read
func resolveOptionalSessionIDs(cmd *cobra.Command) ([]string, error) {
	sessionSet := cmd.Flags().Changed("session")
	fromFileSet := cmd.Flags().Changed("session-from-file")
	if !sessionSet && !fromFileSet {
		return nil, nil
	}
	ids, err := resolveSessionIDs(cmd)
	if err != nil {
		return nil, err
	}
	if fromFileSet && len(ids) == 0 {
		path, _ := cmd.Flags().GetString("session-from-file")
		return nil, fmt.Errorf(
			"resolveOptionalSessionIDs: --session-from-file %q resolved to an empty session scope\n"+
				"What went wrong: the explicitly requested scope contains zero nonblank session IDs.\n"+
				"Why: treating an empty explicit scope as an unscoped annotation prune could delete every annotation by the named annotator.\n"+
				"Where: resolveOptionalSessionIDs.\n"+
				"When: before annotate prune opens or queries the database.\n"+
				"Impact: annotation pruning was stopped without accessing or changing the database.\n"+
				"Fix: add at least one nonblank session ID to the file, or remove --session-from-file only if you intentionally want an unscoped prune.",
			path,
		)
	}
	return ids, nil
}

// ensureOutputDir creates the output directory and all required parent directories
// if they do not already exist. It is a no-op if the directory already exists.
func ensureOutputDir(path string) error {
	return os.MkdirAll(path, 0o755)
}
