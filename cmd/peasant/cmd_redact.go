package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// RedactStatusEnum represents the outcome status of redacting a single session.
type RedactStatusEnum string

// String implements fmt.Stringer.
func (s RedactStatusEnum) String() string { return string(s) }

// RedactStatus namespaces redact outcome identifiers for dot-notation usage.
// Example: RedactStatus.Redacted, RedactStatus.NotIngested
var RedactStatus = struct {
	// Redacted means the session transcript was successfully redacted.
	Redacted RedactStatusEnum
	// Skipped means the session was already redacted at the current level.
	Skipped RedactStatusEnum
	// NotIngested means the session exists in the database but has no
	// local files — the user needs to run 'peasant ingest' first.
	NotIngested RedactStatusEnum
	// Error means the redaction attempt failed.
	Error RedactStatusEnum
}{
	Redacted:    "redacted",
	Skipped:     "skipped",
	NotIngested: "not_ingested",
	Error:       "error",
}

// redactSessionResult records the outcome of redacting a single session.
type redactSessionResult struct {
	SessionID string           `json:"session_id"`
	Status    RedactStatusEnum `json:"status"`
	Reason    string           `json:"reason,omitempty"`
}

// BuildRedactCommand constructs the `peasant redact` command.
func BuildRedactCommand() *cobra.Command {
	var (
		sessionIDs []string
		all        bool
		level      string
		dryRun     bool
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "redact",
		Short: "Redact local transcripts",
		Long:  "Apply redaction to locally stored transcripts. Sessions already redacted at the current level are skipped unless content has changed (stale).",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Require at least one filter.
			if !all && len(sessionIDs) == 0 {
				return fmt.Errorf("at least one filter is required (--session or --all)")
			}

			// Load config for default redaction level.
			//
			// Through resolveConfigPath, NOT the raw --config flag. Reading the flag
			// directly returns its DEFAULT when it is unset, so --config-dir - a
			// supported root flag - was ignored here: a configuration carrying a
			// level this version refuses ran to completion and exited 0, while the
			// same file passed via --config refused correctly. The refusal this
			// command owns was bypassed by a supported flag.
			cfgPath := resolveConfigPath(cmd)
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				// Non-fatal: use default config if file not found.
				cfg = config.BaseConfig()
			}

			// Resolve redaction level: --level flag overrides config.
			requestedLevel := cfg.Redaction.Level
			if level != "" {
				requestedLevel = redact.RedactionLevel(level)
			}
			if !requestedLevel.IsValid() {
				return fmt.Errorf("invalid redaction level %q (the level this version offers is %s)",
					requestedLevel, config.RedactionLevelMenu())
			}
			if refusal := redactionLevelRefusal(requestedLevel, level != "", configSourceDescription(cfgPath)); refusal != nil {
				cmd.SilenceUsage = true
				return refusal
			}
			redactionPolicy := config.ResolveRedactionPolicy(requestedLevel)
			if redactionPolicy.Raised() {
				fmt.Fprintln(cmd.ErrOrStderr(), redactionPolicy.Disclosure())
			}
			redactLevel := redactionPolicy.Effective

			// Convert custom patterns from config.
			userPatterns, err := config.CustomPatternsToUserPatterns(cfg.Redaction.CustomPatterns)
			if err != nil {
				return fmt.Errorf("invalid custom patterns: %w", err)
			}

			// Create redactor.
			redactor, err := redact.NewRedactor(redactLevel, userPatterns, resolveXDGPaths(cmd))
			if err != nil {
				return fmt.Errorf("create redactor: %w", err)
			}

			// Open the store to look up session locations.
			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			ctx := cmd.Context()

			// Resolve session IDs.
			var targetIDs []ingest.SessionID
			if all {
				ids, dbErr := db.AllSessionIDs(ctx)
				if dbErr != nil {
					return fmt.Errorf("query all sessions: %w", dbErr)
				}
				for _, raw := range ids {
					sid, sidErr := ingest.NewSessionID(raw)
					if sidErr != nil {
						continue // skip invalid IDs from DB
					}
					targetIDs = append(targetIDs, sid)
				}
			} else {
				for _, raw := range sessionIDs {
					sid, sidErr := ingest.NewSessionID(raw)
					if sidErr != nil {
						return fmt.Errorf("invalid session ID %q: %w", raw, sidErr)
					}
					targetIDs = append(targetIDs, sid)
				}
			}

			if len(targetIDs) == 0 {
				if jsonOutput {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
						"redacted": 0,
						"skipped":  0,
						"errors":   0,
						"sessions": []any{},
					})
				}
				fmt.Fprintln(cmd.OutOrStdout(), "no sessions found")
				return nil
			}

			// Resolve output sync directory.
			syncDir := resolveOutputSyncDir(cmd)

			// Process each session.
			var results []redactSessionResult
			var redactedCount, skippedCount, noFilesCount, errorCount int

			for _, sid := range targetIDs {
				result := processRedactSession(ctx, db, redactor, syncDir, sid, redactLevel, dryRun)
				results = append(results, result)
				switch result.Status {
				case RedactStatus.Redacted:
					redactedCount++
				case RedactStatus.Skipped:
					skippedCount++
				case RedactStatus.NotIngested:
					noFilesCount++
				case RedactStatus.Error:
					errorCount++
				}
			}

			// Output results.
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
					"dry_run":      dryRun,
					"redacted":     redactedCount,
					"skipped":      skippedCount,
					"not_ingested": noFilesCount,
					"errors":       errorCount,
					"sessions":     results,
				})
			}

			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "dry run: %d would be redacted, %d already current, %d not ingested, %d errors\n",
					redactedCount, skippedCount, noFilesCount, errorCount)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "redacted %d session(s), skipped %d (current), %d not ingested, %d error(s)\n",
					redactedCount, skippedCount, noFilesCount, errorCount)
			}
			if noFilesCount > 0 {
				fmt.Fprintf(cmd.OutOrStderr(), "  %d session(s) not yet ingested — run 'peasant ingest' to create local copies first\n", noFilesCount)
			}
			for _, r := range results {
				if r.Status == RedactStatus.Error {
					fmt.Fprintf(cmd.OutOrStderr(), "  error: %s: %s\n", r.SessionID, r.Reason)
				}
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&sessionIDs, "session", nil, "Session IDs to redact (repeatable)")
	cmd.Flags().BoolVar(&all, "all", false, "Redact all sessions")
	// The help text is derived from the offered set rather than written out, so a
	// level removed from the menu cannot keep being advertised here. This flag was
	// the single loudest place minimal and maximum were offered to users.
	cmd.Flags().StringVar(&level, "level", "",
		fmt.Sprintf("Override redaction level (%s)", config.RedactionLevelMenu()))
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be redacted without modifying files")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output results as JSON")

	return cmd
}

// resolveOutputSyncDir returns the peasant-sync directory path under the data
// dir, honoring the --data-dir flag override on cmd (falling back to
// $XDG_DATA_HOME at call time), so it stays consistent with where openDB(cmd)
// opens the database.
func resolveOutputSyncDir(cmd *cobra.Command) string {
	return filepath.Join(string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd))), "peasant-sync")
}

// processRedactSession handles redaction for a single session.
func processRedactSession(
	ctx context.Context,
	db *store.Store,
	redactor redact.Redactor,
	syncDir string,
	sid ingest.SessionID,
	targetLevel redact.RedactionLevel,
	dryRun bool,
) redactSessionResult {
	// Look up session location (host_slug) from the store.
	hostSlug, _, err := db.LookupSessionLocation(ctx, sid)
	if err != nil {
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Error, Reason: fmt.Sprintf("lookup: %v", err)}
	}
	if hostSlug == "" {
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Error, Reason: "session not found in database"}
	}

	// Construct file paths.
	sessionDir := filepath.Join(syncDir, hostSlug, string(sid))
	metadataPath := filepath.Join(sessionDir, string(sid)+defaults.MetadataSuffix)

	// Read metadata. Distinguish between "file not found" (no local files) and actual errors.
	metaBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return redactSessionResult{SessionID: string(sid), Status: RedactStatus.NotIngested, Reason: "not yet ingested — run 'peasant ingest' first"}
		}
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Error, Reason: fmt.Sprintf("read metadata: %v", err)}
	}

	var meta schema.UnifiedMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Error, Reason: fmt.Sprintf("parse metadata: %v", err)}
	}

	// Find transcript file (JSONL or JSON).
	transcriptPath, sourceFormat, err := findTranscriptFile(sessionDir, string(sid))
	if err != nil {
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Error, Reason: err.Error()}
	}

	// Read transcript.
	transcriptBytes, err := os.ReadFile(transcriptPath)
	if err != nil {
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Error, Reason: fmt.Sprintf("read transcript: %v", err)}
	}

	// Compute current content hash.
	currentHash := schema.ComputeTranscriptHash(transcriptBytes)

	// Check redaction status.
	if meta.Redaction.IsCurrent(currentHash) && meta.Redaction.Level == targetLevel.String() {
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Skipped, Reason: "already current at same level"}
	}

	if dryRun {
		reason := "raw"
		if meta.Redaction.IsStale(currentHash) {
			reason = "stale"
		} else if meta.Redaction.Applied && meta.Redaction.Level != targetLevel.String() {
			reason = "different level"
		}
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Redacted, Reason: fmt.Sprintf("would redact (%s)", reason)}
	}

	// Apply redaction to transcript.
	var redactedTranscript []byte
	switch sourceFormat {
	case schema.SourceFormatJSONL:
		redactedTranscript, err = redactJSONLBytes(redactor, transcriptBytes)
		if err != nil {
			return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Error, Reason: fmt.Sprintf("redact JSONL: %v", err)}
		}
	case schema.SourceFormatJSON:
		redactedTranscript = redactJSONDocBytes(redactor, transcriptBytes)
	default:
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Error, Reason: fmt.Sprintf("unknown source format: %s", sourceFormat)}
	}

	// Redact metadata.
	redactedMeta := redactor.RedactMetadata(&meta)

	// Compute new content hash from redacted transcript.
	newContentHash := schema.ComputeTranscriptHash(redactedTranscript)

	// Update RedactionInfo.
	nowMs := time.Now().UnixMilli()
	redactedMeta.Redaction = schema.RedactionInfo{
		Applied:             true,
		Level:               targetLevel.String(),
		RuleSetVersion:      redact.Version(),
		RedactedAtMs:        &nowMs,
		ContentHashAtRedact: newContentHash,
	}
	redactedMeta.ContentHash = newContentHash
	redactedMeta.MetadataHash = schema.ComputeMetadataHash(redactedMeta)

	// Write transcript.
	if err := os.WriteFile(transcriptPath, redactedTranscript, defaults.PrivateFilePerm); err != nil {
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Error, Reason: fmt.Sprintf("write transcript: %v", err)}
	}

	// Write metadata.
	newMetaBytes, err := json.MarshalIndent(redactedMeta, "", "  ")
	if err != nil {
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Error, Reason: fmt.Sprintf("marshal metadata: %v", err)}
	}
	if err := os.WriteFile(metadataPath, newMetaBytes, defaults.PrivateFilePerm); err != nil {
		return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Error, Reason: fmt.Sprintf("write metadata: %v", err)}
	}

	return redactSessionResult{SessionID: string(sid), Status: RedactStatus.Redacted}
}

// findTranscriptFile locates the transcript file for a session and returns
// its path and format. Filename pattern: {sessionId}--transcript.{jsonl|json}
func findTranscriptFile(sessionDir, sessionID string) (string, schema.SourceFormat, error) {
	jsonlPath := filepath.Join(sessionDir, fmt.Sprintf("%s%s%s", sessionID, defaults.TranscriptPrefix, schema.SourceFormatJSONL))
	if _, err := os.Stat(jsonlPath); err == nil {
		return jsonlPath, schema.SourceFormatJSONL, nil
	}

	jsonPath := filepath.Join(sessionDir, fmt.Sprintf("%s%s%s", sessionID, defaults.TranscriptPrefix, schema.SourceFormatJSON))
	if _, err := os.Stat(jsonPath); err == nil {
		return jsonPath, schema.SourceFormatJSON, nil
	}

	return "", "", fmt.Errorf("transcript not found in %s", sessionDir)
}

// redactJSONLBytes delegates to redact.RedactJSONLBytes (shared utility).
func redactJSONLBytes(r redact.Redactor, data []byte) ([]byte, error) {
	return redact.RedactJSONLBytes(r, data, redact.WithRedactScannerBufSize(defaults.ScannerInitBuf, defaults.ScannerMaxLine))
}

// redactJSONDocBytes delegates to redact.RedactJSONDocBytes (shared utility).
func redactJSONDocBytes(r redact.Redactor, data []byte) []byte {
	return redact.RedactJSONDocBytes(r, data)
}

// redactionLevelRefusal is what `peasant redact` does with a level before it
// opens anything, or nil when the run may proceed.
//
// It is a function rather than a block inside RunE so its arms can be driven
// directly. One of them - an unknown disposition - is not reachable through the
// command today, because a level with no entry in the policy table would have to
// pass redact.RedactionLevel.IsValid first, and nothing does. That is precisely
// why it needs to be reachable from a test: an arm nobody can exercise is an arm
// that can be deleted without anything going red, and this one is the fail-closed
// one. Deleting it lets a level nobody classified run at full strength.
//
// askedByFlag is where the level came from, and it changes two things. It changes
// what the user is told to edit - a file path or the flag - and, for a level that
// is merely no longer OFFERED, it changes the outcome: a level typed by name is
// refused, because a caller who names one must not be handed a different one,
// while the same level sitting in a configuration is raised and disclosed, there
// being nobody at the keyboard to tell at the moment it is read.
func redactionLevelRefusal(level redact.RedactionLevel, askedByFlag bool, configSource string) error {
	source := configSource
	if askedByFlag {
		source = "the --level flag"
	}
	const (
		operation = "peasant redact"
		step      = "before any stored transcript was read or rewritten"
		impact    = "No transcript was changed."
	)
	switch config.RedactionLevelDispositionOf(level) {
	case config.RedactionLevelDispositionRefused, config.RedactionLevelDispositionUnknown:
		return &config.UnsupportedRedactionLevelError{
			Level: level, Source: source, Operation: operation, Step: step, Impact: impact,
		}
	case config.RedactionLevelDispositionRaised:
		if askedByFlag {
			return &config.UnofferedRedactionLevelError{
				Level: level, Source: source, Operation: operation, Step: step, Impact: impact,
			}
		}
	}
	return nil
}
