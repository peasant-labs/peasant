package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// BuildPushCommand constructs the fully wired push cobra command.
func BuildPushCommand() *cobra.Command {
	var (
		dryRun             bool
		force              bool
		sourceProvider     string
		visibility         string
		license            string
		jsonOutput         bool
		verbose            bool
		quiet              bool
		nonInteractiveFlag bool
		yesFlag            bool
		annotationIDs      []string
		annotationHash     []string
		timing             bool
		concurrency        int
		repository         string
		timeout            time.Duration
	)

	cmd := &cobra.Command{
		Use:   "push",
		Short: "Push ingested transcripts to the Peasant village",
		Long: "Upload locally ingested session transcripts to the Peasant village for sharing and analytics.\n\n" +
			"Performance — the --concurrency default is max(1, NumCPU/2), tuned for the common case: a\n" +
			"steady-state / no-change re-push, where the server manifest skips already-pushed annotations\n" +
			"and unchanged transcripts so very little goes over the wire. A one-time LARGE COLD push (e.g.\n" +
			"hundreds of new transcripts + tens of thousands of new annotations) benefits from a higher\n" +
			"--concurrency. Throughput is bounded by the per-transcript S3 round-trip and the village's\n" +
			"CPU — NOT by the DB connection pool: a pooled connection is held only for the brief row\n" +
			"insert (the S3 upload itself holds none), so the village pool (default ~2x its vCPUs) has\n" +
			"headroom. A genuinely cold run of N new transcripts costs roughly\n" +
			"ceil(N / effective-concurrency) x the per-upload round-trip. For such a push set\n" +
			"--concurrency to about 2x NumCPU to scale throughput up to the village's capacity; confirm\n" +
			"with --timing. The default stays at max(1, NumCPU/2) — sufficient for the common\n" +
			"steady-state re-push, where the manifest skip means little goes over the wire.)\n\n" +
			"Exit status — a caller that branches on it, including a generated Git hook, needs the\n" +
			"one distinction it cannot read out of prose: whether anything was published at all.\n\n" +
			"  0  the run succeeded.\n" +
			"  1  the run failed after it had started talking to the village, so part of the work\n" +
			"     may be published and recorded as published.\n" +
			"  3  the run failed BEFORE a single village request was made — a missing or expired\n" +
			"     login, an unreadable config, a store that would not open, a repository scope that\n" +
			"     would not resolve. Nothing was published and nothing was recorded as published.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Past argument validation, every failure here is a RUNTIME error
			// (auth gate, unreachable village, pipeline) — not a usage error. Cobra
			// dumps the Usage/Flags block on any RunE error unless silenced, which
			// drowns the actionable one-liner. That matters most from a git hook,
			// where an expired login would otherwise print the whole flag list into
			// every commit. Cobra still prints usage for genuine arg/flag misuse,
			// which it handles before RunE.
			cmd.SilenceUsage = true

			ctx := cmd.Context()
			// Honours --config-dir when --config was not given explicitly, so a
			// hook that bound a config directory runs on that directory's
			// configuration rather than silently on the default one.
			cfgPath := resolveConfigPath(cmd)

			level, levelErr := resolveOutputLevel(quiet, verbose)
			if levelErr != nil {
				return levelErr
			}
			nonInteractive := nonInteractiveFlag || yesFlag

			// An overall time budget for the whole command. The village client's
			// own timeout is PER REQUEST and one push issues several in sequence,
			// so a village that accepts a connection and never answers stalls for
			// minutes with no bound. Unset (the default) keeps a manual push
			// unbounded exactly as before; a git hook always passes one.
			if timeout < 0 {
				return fmt.Errorf(
					"village push: --timeout must not be negative (got %s); it is the overall budget for the whole upload, and a negative budget has no meaning; nothing was uploaded; pass a positive duration such as --timeout 5s, or omit the flag to run without a budget",
					timeout)
			}
			if timeout > 0 {
				var cancelBudget context.CancelFunc
				ctx, cancelBudget = context.WithTimeout(ctx, timeout)
				defer cancelBudget()
			}

			// Everything below runs inside that budget, so it is written as ONE
			// function with a single error return and every failure leaves
			// through the budget check at the bottom. Wrapping only the tail
			// meant an early return under an expired cap escaped raw: a store
			// query came back "sqlite: prepare: interrupted", and repository
			// resolution reported that a perfectly valid repository was not one —
			// which, inside a hook, is the entire diagnosis the user gets.
			var run pushRun
			run.villageRequested = &atomic.Bool{}
			run.binding = hookBinding(cmd)
			// Preserve the requested containment before any budgeted local work.
			// Credentials, config, or opening the store can consume the budget
			// before scope resolution starts; a recovery printed there must still
			// carry the --repository the caller supplied.
			if cmd.Flags().Changed("repository") && strings.TrimSpace(repository) != "" {
				run.requestedRepository = repository
				if absolute, absErr := filepath.Abs(repository); absErr == nil {
					run.requestedRepository = filepath.Clean(absolute)
				}
			}
			runErr := func(ctx context.Context) error {

				// --quiet promises errors and one final result line. Structured
				// diagnostics below error level are fail-safe notes about degraded
				// service - a manifest that could not be fetched, quality metrics that
				// could not be read - not failures, and a hook fires on every commit.
				// Route them out for the duration of this command; errors still pass.
				if level == outputQuiet {
					previousLogger := slog.Default()
					slog.SetDefault(slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelError})))
					defer slog.SetDefault(previousLogger)
				}

				creds, err := auth.LoadCredentialsFrom(configDirOverride(cmd))
				if err != nil {
					return fmt.Errorf("load credentials: %w", err)
				}
				if creds == nil || !creds.IsValid() {
					return fmt.Errorf("not logged in — run 'peasant village login' first")
				}
				if creds.VillageURL == "" {
					return fmt.Errorf("village URL is not set — run 'peasant village login' to re-link your account")
				}

				cfg, err := loadConfig(cfgPath)
				if err != nil {
					return fmt.Errorf("load config: %w", err)
				}

				// A configured level this version cannot apply refuses the push
				// outright. Publishing under a weaker level than the user chose
				// would send content they believe is protected, and no weaker level
				// is a substitute, so there is nothing safe to fall back to.
				//
				// Checked here, before the store is opened or any candidate is
				// queried: an unrunnable configuration should be reported at once
				// rather than after work that can exhaust the command's --timeout
				// budget and report a deadline instead of the real cause.
				if !config.RedactionLevelSupported(cfg.Redaction.Level) {
					return &config.UnsupportedRedactionLevelError{
						Level:     cfg.Redaction.Level,
						Source:    configSourceDescription(cfgPath),
						Operation: "village push",
						Step:      "before the session store was opened and before any session was uploaded",
						Impact:    "Nothing was published and nothing was recorded as published.",
					}
				}

				// Resolve the effective upload concurrency: --concurrency (if set)
				// overrides push.concurrency config, which overrides the CPU-derived
				// default max(1, NumCPU/2). An explicit --concurrency <= 0 is rejected
				// with an actionable error. This value sizes BOTH the upload
				// parallelism and the HTTP connection pool.
				resolvedConcurrency, err := push.ResolveConcurrency(
					cmd.Flags().Changed("concurrency"), concurrency, cfg.Push.Concurrency, runtime.NumCPU())
				if err != nil {
					return err
				}
				fs := &ingest.OSFileSystem{}

				// Not having configured Peasant is the DEFAULT state, not an
				// error and not a result: under --quiet — which is what a git
				// hook runs — this printed on every commit and every push, and
				// named a remedy that needs a terminal a hook does not have.
				if cfgPath != "" && level != outputQuiet {
					if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) {
						fmt.Fprintf(cmd.ErrOrStderr(), "notice: no config found at %s — using defaults. Run 'peasant kickstart' to configure.\n", cfgPath)
					}
				}

				// Deprecation note for the retired `push.fields.projectHash` key. The
				// project hash is now always sent (salted, non-correlatable); the key
				// is parsed but ignored. Stderr so --json stdout stays clean.
				if cfg.Push.Fields.HasDeprecatedProjectHashKey() {
					noteCfgPath := cfgPath
					fmt.Fprintf(cmd.ErrOrStderr(),
						"note: 'push.fields.projectHash' in %s is deprecated and ignored. "+
							"project.hash is a per-install SALTED digest (not the raw remote URL), so sending it leaks nothing, "+
							"and the village requires it to group your sessions by project — it is now always sent. "+
							"To silence this note, remove the 'push.fields.projectHash' line from %s.\n",
						noteCfgPath, noteCfgPath)
				}

				dataDir := string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
				dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
				if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
					return fmt.Errorf("create data directory: %w", err)
				}
				db, err := store.Open(dbPath)
				if err != nil {
					return fmt.Errorf("open analytics store: %w", err)
				}
				defer db.Close()

				resolvedOutput, resolveErr := ingest.NewResolvedPath(cfg.Output.BasePath)
				if resolveErr != nil {
					return fmt.Errorf("resolve output path: %w", resolveErr)
				}
				cfg.Output.BasePath = string(resolvedOutput)

				// Fail fast on a bad --license flag: an invalid value would otherwise be
				// rejected per-session at the client-side schema pre-flight, failing every
				// upload with the same error. One upfront message is clearer.
				if license != "" && !schema.License(license).IsValid() {
					return fmt.Errorf("invalid --license %q (valid: %s)", license, schema.LicenseMenu())
				}

				// Same treatment for --visibility, against the same kind of closed
				// contract set. It used to accept any string, so a typo was taken as
				// a visibility, silently resolved to the default, and reported as
				// applied: a consent boundary answering a question nobody asked.
				if visibility != "" && !schema.Visibility(visibility).IsValid() {
					return fmt.Errorf("invalid --visibility %q (valid: %s)", visibility, config.VisibilityMenu())
				}

				runCfg := push.PipelineConfig{
					DryRun:         dryRun,
					Force:          force,
					SourceProvider: sourceProvider,
					Visibility:     schema.Visibility(visibility),
					License:        schema.License(license),
					JSONOutput:     jsonOutput,
					Verbose:        verbose,
					Quiet:          level == outputQuiet,
					Concurrency:    resolvedConcurrency,
					CommandBinding: run.binding,
				}
				if cmd.Flags().Changed("repository") {
					// An empty --repository is rejected rather than ignored. The
					// flag's only purpose is to confine a push to one repository, so
					// silently degrading it to no confinement at all turns a
					// containment request into the opposite.
					if strings.TrimSpace(repository) == "" {
						return fmt.Errorf(
							"village push: --repository was given an empty value; the flag exists to confine this push to one repository's sessions, and an empty value would confine nothing and upload every configured session instead; nothing was uploaded; pass a path inside the repository you meant (--repository . for the working directory), or drop the flag if you really do want every configured session")
					}
					scope, scopeErr := run.resolveScope(ctx, db, repository)
					if scopeErr != nil {
						return scopeErr
					}
					runCfg.Repository = scope
					// Report the resolved scope, so a push that uploads nothing is
					// self-diagnosing rather than silent. Suppressed under --quiet,
					// which is what a hook runs with.
					if level != outputQuiet {
						fmt.Fprintf(cmd.ErrOrStderr(), "scope: %s\n", scope.Describe())
					}
				}
				// Branch-aware selection filter. When selection.mode=selected,
				// push honors the configured projects/branches (composing with
				// --source-provider and the wizard). mode != selected => nil (no
				// filter; push everything otherwise eligible).
				if cfg.Selection.Mode == config.SelectionModeSelected {
					matcher := cfg.SelectionMatcher()
					runCfg.Selection = &matcher
				}

				// Run the push wizard for interactive confirmation.
				// Skipped for --dry-run, --json, non-TTY, or --non-interactive/--yes.
				isTTY := term.IsTerminal(int(os.Stdin.Fd()))
				if !dryRun && !jsonOutput && isTTY && !nonInteractive {
					wizQuery := push.PushCandidateQuery{
						Force:          force,
						SourceProvider: sourceProvider,
						Method:         cfg.Push.Method,
						Sources:        cfg.Push.Sources,
					}
					wizardIDs, wizErr := runPushWizard(ctx, db, fs, cfg.Output.BasePath, wizQuery, runCfg.Selection)
					if wizErr != nil {
						return wizErr
					}
					if wizardIDs == nil {
						// User cancelled.
						return nil
					}
					runCfg.FilterSessionIDs = wizardIDs
				}

				// Home paths and PII are stripped from metadata on the way out even
				// for a session imported before redaction existed, or under a level
				// this version no longer offers. The resolution and its wording are
				// shared with every other surface rather than phrased here, which is
				// how the same configuration used to produce a different level
				// depending on which command read it.
				redactionPolicy := config.ResolveRedactionPolicy(cfg.Redaction.Level)
				effectiveLevel := redactionPolicy.Effective
				if redactionPolicy.Raised() {
					fmt.Fprintln(cmd.ErrOrStderr(),
						quietAware(level, redactionPolicy.Disclosure(), redactionPolicy.BriefDisclosure()))
				}
				userPatterns, patErr := config.CustomPatternsToUserPatterns(cfg.Redaction.CustomPatterns)
				if patErr != nil {
					return fmt.Errorf("build push redactor: %w", patErr)
				}
				pushRedactor, err := redact.NewRedactor(effectiveLevel, userPatterns, resolveXDGPaths(cmd))
				if err != nil {
					return fmt.Errorf("create push redactor: %w", err)
				}

				visibilityPolicy := config.EffectiveVisibility(schema.Visibility(visibility), cfg)
				effectiveVisibility := visibilityPolicy.Effective
				if visibilityPolicy.Downgraded() {
					fmt.Fprintln(cmd.ErrOrStderr(),
						quietAware(level, visibilityPolicy.Disclosure(), visibilityPolicy.BriefDisclosure()))
				}

				// The redaction report is the published-content record CI logs
				// retain, so it is printed for the pushes that actually happen. It
				// used to sit inside the public-visibility branch below, which no
				// input reaches any more, so no push printed it at all. --quiet
				// suppresses it (errors + final result line only), and a dry run
				// publishes nothing to keep a record of.
				if !dryRun && !jsonOutput && level != outputQuiet {
					reportSessions, queryErr := pushCandidates(ctx, db, force, sourceProvider)
					if queryErr != nil {
						// The record is informational: losing it must not fail a push
						// that would otherwise publish. Saying so is the honest form.
						fmt.Fprintf(cmd.ErrOrStderr(),
							"warning: the redaction record for this push could not be produced (%v); the upload itself is unaffected\n",
							queryErr)
					} else {
						// Narrowed exactly the way the pipeline narrows, through the
						// same shared helpers, so the record describes what will be
						// published rather than everything that might have been.
						reportSessions = filterToSelectedSessions(reportSessions, runCfg.FilterSessionIDs)
						reportSessions, _ = push.ApplySelection(reportSessions, runCfg.Selection)
						reportSessions = push.ApplyRepositoryScope(reportSessions, runCfg.Repository)
						// Nothing to publish is not a publication to keep a record
						// of, and the empty-state line below already says what
						// happened. A hook reaches that state on most commits.
						if len(reportSessions) > 0 {
							// effectiveLevel is the level the redactor injected into
							// the pipeline below is built at, so the record names the
							// protection this push actually applies rather than a
							// flag recorded by an earlier import.
							printRedactionReport(cmd.ErrOrStderr(),
								buildRedactionRecord(reportSessions, cfg.Output.BasePath, fs, effectiveLevel))
						}
					}
				}

				// Public visibility is consent-gated here. Publication itself starts
				// private, then the authoritative owner update converges it to the
				// requested public state. Do NOT move the redaction record inside this
				// condition: the record belongs to every push.
				if !dryRun && !jsonOutput && effectiveVisibility == config.VisibilityPublic {
					consented, promptErr := promptPublicConsent(cmd.InOrStdin(), cmd.ErrOrStderr(), nonInteractive, isTTY)
					if promptErr != nil {
						return fmt.Errorf("consent prompt: %w", promptErr)
					}
					if !consented {
						// User declined or non-TTY without --yes: abort with exit 0.
						return nil
					}
				}

				client := village.NewVillageClientWithConcurrency(creds.VillageURL, creds.APIKey, resolvedConcurrency)
				client.SetRequestObserver(run.markVillageRequest)
				pipeline := push.NewPipeline(db, client, creds, cfg, fs, runCfg, pushRedactor, cmd.ErrOrStderr())

				if dryRun {
					if level != outputQuiet {
						fmt.Fprintf(cmd.OutOrStdout(), "Dry run — no uploads will be made\n\n")
					}
				} else if !jsonOutput && level != outputQuiet {
					fmt.Fprintf(cmd.OutOrStdout(), "Pushing to %s as @%s...\n\n",
						creds.VillageURL, creds.Username)
				}

				// Animation for non-dry-run, non-JSON, non-quiet modes.
				var stopAnim func()
				if !dryRun && !jsonOutput && level != outputQuiet {
					animCtx, animCancel := context.WithCancel(ctx)
					anim := animation.NewRenderer(os.Stderr, animation.PushAnimation())
					go anim.Run(animCtx)
					stopAnim = func() {
						animCancel()
						anim.Wait()
						anim.Clear()
					}
				}

				// Build the annotation selection from --annotation-id / --annotation-hash.
				// An empty selection means "push every system annotation" (the
				// historical all-or-nothing behaviour). When the user names specific
				// annotations, only those are published — this is the CLI counterpart
				// of the share wizard's label-selection step.
				annSelection := buildAnnotationSelection(annotationIDs, annotationHash)

				// When a branch-aware session selection is active, narrow the
				// annotation push to annotations whose target session is selected
				// (reusing the SAME ApplySelection used for the session push, applied
				// to all push-eligible sessions). Annotations not tied to a session
				// are unaffected. mode != selected => runCfg.Selection is nil => no
				// session gate, so annotation behavior is unchanged.
				if runCfg.Selection != nil || runCfg.Repository != nil {
					// AllPushableSessions (NOT the narrower unpushed session-push set):
					// annotations push on their own cadence, so the gate must admit
					// annotations for ALL selected sessions, including already-pushed
					// ones. Do NOT "dedupe" this with the session pipeline's query —
					// that would drop annotations for selected-but-already-pushed
					// sessions. Sessions without metrics are absent here (held from
					// push), so their annotations are held too — consistent with the
					// SessionsWithoutMetrics held-back design.
					allRows, selErr := db.AllPushableSessions(ctx)
					if selErr != nil {
						return fmt.Errorf("query sessions for annotation selection: %w", selErr)
					}
					keptForAnn, _ := push.ApplySelection(allRows, runCfg.Selection)
					keptForAnn = push.ApplyRepositoryScope(keptForAnn, runCfg.Repository)
					sessIDs := make(map[string]bool, len(keptForAnn))
					for _, s := range keptForAnn {
						sessIDs[s.SessionID] = true
					}
					annSelection.SessionIDs = sessIDs
					// Under a repository scope, an annotation with no target session
					// is published only when it targets one of that repository's own
					// project identities. Anything Peasant cannot attribute to this
					// repository is withheld rather than swept along.
					if runCfg.Repository != nil {
						hashes := make(map[string]bool)
						for _, hash := range runCfg.Repository.Hashes() {
							hashes[hash] = true
						}
						annSelection.RepositoryProjectHashes = hashes
					}
				}

				// --timing: thread a perf.Collector through the run context so the
				// pipeline (per-session redact + per-upload httptrace split) and the
				// annotation path (per-batch timing) record into it. Off by default —
				// runCtx == ctx and every recorder resolves to Nop (no overhead).
				runCtx := ctx
				var timingCollector *perf.Collector
				if timing {
					timingCollector = perf.NewCollector()
					runCtx = perf.ContextWithRecorder(ctx, timingCollector)
				}

				// Complete transcript publishing before starting annotation publishing.
				// Each stage retains its existing internal concurrency; the barrier makes
				// transcript-backed annotation targets visible before validation runs.
				result, transcErr, annSummary, annErr := runPushStages(
					runCtx,
					func(ctx context.Context) (*push.PushResult, error) {
						return pipeline.Run(ctx)
					},
					func(ctx context.Context) (*push.AnnotationPushSummary, error) {
						return push.PushAnnotationsSelected(ctx, client, db, annSelection, dryRun, resolvedConcurrency)
					},
				)
				if annSummary != nil {
					prefix := githooks.CommandPrefix(run.binding)
					for index := range annSummary.Unpublishable {
						annSummary.Unpublishable[index] = annSummary.Unpublishable[index].WithCommandPrefix(prefix)
					}
				}

				// Recorded before any early return below, so a budget error can say
				// what did and did not reach the village.
				run.result = result
				run.annotationSummary = annSummary

				// Stop animation before printing results.
				if stopAnim != nil {
					stopAnim()
				}

				// Emit the timing rollup (stderr) and per-upload JSONL log. Done before
				// the result-nil/empty branches below so timing is reported on every
				// real run regardless of push outcome. JSONL path is announced on
				// stderr so --json stdout stays clean.
				if timingCollector != nil {
					if rollupErr := perf.WriteRollup(cmd.ErrOrStderr(), timingCollector.Rollup()); rollupErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: write timing rollup: %v\n", rollupErr)
					}
					if logPath, logErr := writeTimingLog(timingCollector, stateDirOverride(cmd)); logErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: write timing log: %v\n", logErr)
					} else if logPath != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "timing log written to %s\n", logPath)
					}
				}

				// Handle nil transcript result (pipeline returned nothing — fatal).
				if result == nil {
					if transcErr != nil {
						return transcErr
					}
					return fmt.Errorf("push pipeline returned nil result")
				}

				// Handle empty transcript state. EmptyReason already names which
				// narrowing removed every candidate, states that --force cannot widen
				// one, and gives the next command; a separate note here restated the
				// same fact a third time.
				if result.EmptyReason != "" {
					if jsonOutput {
						return printPushJSON(cmd.OutOrStdout(), result, annSummary, annErr)
					}
					// EmptyReason is the final result line — kept even under --quiet.
					fmt.Fprintln(cmd.OutOrStdout(), result.EmptyReason)
					if level != outputQuiet && (annSummary != nil || annErr != nil) {
						printAnnotationSummary(cmd.OutOrStdout(), annSummary, annErr, dryRun)
					} else if level == outputQuiet {
						printQuietUnpublishableNotice(cmd.ErrOrStderr(), annSummary, run.repositoryCommand())
					}
					return annErr
				}

				// Print results, routed by the resolved verbosity (--json unaffected).
				if jsonOutput {
					if err := printPushJSON(cmd.OutOrStdout(), result, annSummary, annErr); err != nil {
						return err
					}
				} else if level == outputQuiet {
					printPushResultLine(cmd.OutOrStdout(), result, dryRun)
					// --quiet suppresses the annotation summary, but content
					// that will NEVER be published is not a summary detail: a
					// hook would otherwise report a clean push on every commit
					// while silently leaving those annotations behind forever.
					// One line, only when there is something to say, naming the
					// command that shows which and how to clear them.
					printQuietUnpublishableNotice(cmd.ErrOrStderr(), annSummary, run.repositoryCommand())
				} else {
					printPushSummary(cmd.OutOrStdout(), result, dryRun, level == outputVerbose)
					// Errors-by-type table: typed-category breakdown that complements
					// the per-session writePushErrorLog. Default + verbose only;
					// suppressed under --quiet (handled by the branch above). --json
					// stdout is never touched (separate branch).
					printErrorSummaryTable(cmd.OutOrStdout(), result)
					// Surface the annotation summary whenever there is anything to
					// report: candidates considered, retractions propagated (a
					// pure-retraction run has Total 0), a whole-push skip reason, or an
					// error. Retraction propagation must be observable.
					if annSummary != nil && (annSummary.Total > 0 || annSummary.Retracted > 0 || annSummary.SkipReason != "" || len(annSummary.Unpublishable) > 0 || annErr != nil) {
						printAnnotationSummary(cmd.OutOrStdout(), annSummary, annErr, dryRun)
					}
				}

				// On any upload failure, dump per-session error detail to a log file
				// (the summary only shows a count) and point the user at it. The path
				// is announced on STDERR so it never pollutes --json/stdout.
				if !dryRun && (result.Errors > 0 || annErr != nil) {
					if logPath, logErr := writePushErrorLog(result, annSummary, annErr, creds.VillageURL, stateDirOverride(cmd)); logErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not write push error log: %v\n", logErr)
					} else {
						fmt.Fprintf(cmd.ErrOrStderr(), "error details written to %s\n", logPath)
					}
				}

				// Return the first fatal error (transcript before annotation). The
				// budget check below turns it into the budget's own explanation when
				// the cap is what ended the run.
				return firstPushStageError(transcErr, annErr)
			}(ctx)

			// A budget that ran out is reported as itself: the raw "context
			// deadline exceeded" — or whichever local call happened to notice the
			// cancellation first — names neither the budget nor how to finish the
			// upload.
			runErr = applyUploadBudgetError(timeout, ctx.Err(), runErr, run)
			// A failure that never reached the village exits with a status of
			// its own, so a generated hook can say so instead of describing an
			// upload that never started.
			if runErr != nil && !run.reachedVillage() {
				return &pushNothingAttemptedError{cause: runErr}
			}
			return runErr
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be pushed without uploading")
	cmd.Flags().BoolVar(&force, "force", false, "Re-push all sessions (including already-pushed ones)")
	cmd.Flags().StringVar(&sourceProvider, "source-provider", "", sourceProviderHelp())
	cmd.Flags().StringVar(&visibility, "visibility", "", "Override visibility for this run (public, private, group)")
	cmd.Flags().StringVar(&license, "license", "", fmt.Sprintf("Override the content license for this run (%s)", schema.LicenseMenu()))
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output as JSON instead of human-readable")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Show per-session detail")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "Suppress the summary and redaction report; print only errors and a final result line")
	cmd.Flags().BoolVar(&nonInteractiveFlag, "non-interactive", false, "Run without the interactive wizard or public-consent prompt (for CI/scripts)")
	cmd.Flags().BoolVar(&yesFlag, "yes", false, "(alias for --non-interactive)")
	cmd.Flags().StringArrayVar(&annotationIDs, "annotation-id", nil, "Only push these annotation IDs (repeatable; default: all). Counterpart to the share wizard's label selection.")
	cmd.Flags().StringArrayVar(&annotationHash, "annotation-hash", nil, "Only push annotations with these content hashes (repeatable; default: all).")
	cmd.Flags().BoolVar(&timing, "timing", false, "Measure and report per-phase push timing (connection setup/server split, redaction, annotation batches) to stderr, plus a per-upload JSONL log under the state dir. Off by default.")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "Number of parallel uploads and HTTP connection-pool size. Must be >= 1. Overrides push.concurrency in config. Default: max(1, NumCPU/2) (tuned for steady-state re-push). For a one-time large COLD push, use ~22 to saturate the village pool toward the <5s target.")
	cmd.Flags().StringVar(&repository, "repository", "", "Only push sessions carrying this Git repository's canonical project identity (a path). Identity comes from the normalized origin remote when there is one Peasant can normalize, so separate clones of that origin share it; with no origin remote — or an origin that is not a network remote, such as a local path or a file:// URL — it is instead the worktree paths the sessions were recorded in, which belong to that directory alone. A repository nested inside another keeps its own identity and never inherits the outer one's. Which of the two was used is printed when the push runs. Default: every configured session")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Overall time budget for the whole upload (e.g. 5s). The per-request client timeout does not bound a push, which issues several requests in sequence, so a village that accepts a connection and never answers can stall for minutes. On expiry the push gives up and reports what did and did not reach the village. Default: no budget. Git hooks always pass one.")

	return cmd
}

// applyUploadBudgetError replaces the operation error when the configured cap
// is what ended the run. A deadline observed after a successful run must not
// retroactively turn that success into a timeout failure.
func applyUploadBudgetError(timeout time.Duration, contextErr, runErr error, run pushRun) error {
	if timeout > 0 && runErr != nil && errors.Is(contextErr, context.DeadlineExceeded) {
		return uploadBudgetExceededError(timeout, run)
	}
	return runErr
}

// resolveRepositoryScope derives the same canonical project identity ingestion
// stores on sessions, and the identities of any directories inside the same
// worktree that carry their own. The resulting scope is applied as an additional
// AND filter and therefore cannot widen configured selection.
func resolveRepositoryScope(ctx context.Context, db *store.Store, root string) (*push.RepositoryScope, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf(
			"scope village push to repository: path %q could not be resolved during --repository processing: %w; nothing was uploaded; pass the path to an existing Git repository",
			root, err)
	}
	resolver := &ingest.ExecGitResolver{}
	canonicalRoot, err := resolver.ResolveRepositoryRoot(ctx, abs)
	if err != nil {
		return nil, fmt.Errorf(
			"scope village push to repository: Git could not resolve a repository root from %q during --repository processing: %w; nothing was uploaded; pass a path inside an existing Git repository",
			abs, err)
	}
	// Ask for the remote the same way ingestion does, so the identity below is
	// byte-identical to the one stamped on sessions. The walk stops at this
	// repository's own boundary, so a repository nested inside another one is
	// never given its parent's remote — and therefore never its parent's
	// sessions.
	remote, _, _ := resolver.WalkUpRemoteURL(ctx, canonicalRoot)
	installationSalt := db.InstallationSalt()
	identity, _, err := ingest.DeriveProjectIdentifiers(installationSalt, remote, canonicalRoot)
	if err != nil {
		return nil, fmt.Errorf(
			"scope village push to repository: canonical project identity derivation failed for %q during --repository processing: %w; nothing was uploaded; verify the repository path and retry",
			abs, err)
	}
	basis := identityBasis(installationSalt, identity, canonicalRoot)
	recorded, unadmitted, err := recordedUnderRoot(ctx, db, resolver, installationSalt, canonicalRoot, identity)
	if err != nil {
		return nil, err
	}
	return push.NewRepositoryScope(canonicalRoot, identity, basis, recorded, unadmitted), nil
}

// identityBasis reports how identity was actually derived, by proof rather than
// by prediction.
//
// Having a remote is not the same as having an identity derived from one:
// DeriveProjectIdentifiers hashes the normalized remote only when normalization
// succeeded, and silently falls back to hashing the worktree path otherwise —
// which is what happens for a local-path or file:// origin. Predicting the basis
// from "a remote string was returned" therefore tells a user that every clone of
// their remote is in scope when in fact the identity belongs to this directory
// alone. Comparing the derived identity against the hash of the path is the same
// proof recordedUnderRoot already applies to every extra directory it admits,
// and the two values cannot collide: a normalized remote is "host/path" and
// never starts with a separator, while a worktree root is always absolute.
func identityBasis(installationSalt salt.Salt, identity ingest.ProjectHash, canonicalRoot string) push.IdentityBasis {
	pathHash, err := installationSalt.Hash(canonicalRoot)
	if err != nil || pathHash == identity {
		// Unprovable is reported as path-derived: that is the narrower claim,
		// promising only that this directory's own sessions are in scope rather
		// than promising a user that every clone of a remote is.
		return push.IdentityFromPath
	}
	return push.IdentityFromRemote
}

// recordedUnderRoot finds the recorded projects that are this repository's own
// work done from a subdirectory, and — separately — the ones recorded inside it
// that this repository's identity does not reach.
//
// A repository with no origin remote is identified by the directory a session
// was recorded in. A session recorded in a subdirectory therefore carries an
// identity the worktree root never derives, and scoping only by the root's
// identity would upload nothing, forever, with no error.
//
// Admission is proven twice, never guessed. A row qualifies only when its own
// identity is exactly the hash of its recorded directory — which is what makes
// it path-derived rather than remote-derived — and when that directory really
// belongs to this worktree. A submodule, a nested repository, or a clone of
// some other remote fails one of those two checks and stays out.
//
// The second return value is the evidence a scoped push needs before it may
// recommend re-ingesting: a directory that still EXISTS in this worktree whose
// sessions carry some other identity. A directory that is gone is deliberately
// not evidence — re-ingesting there re-derives the identity from a dead path
// and loses the sessions for good.
func recordedUnderRoot(
	ctx context.Context,
	db *store.Store,
	resolver *ingest.ExecGitResolver,
	installationSalt salt.Salt,
	canonicalRoot string,
	rootIdentity ingest.ProjectHash,
) (recorded, unadmitted []push.RecordedUnderRoot, err error) {
	projects, err := db.RecordedProjects(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"scope village push to repository: the recorded projects could not be read while resolving --repository %q: %w; nothing was uploaded; re-run 'peasant ingest' or check the analytics store",
			canonicalRoot, err)
	}
	membership := newWorktreeMembership(canonicalRoot)
	for _, project := range projects {
		if project.CanonicalCwd == "" || !filepath.IsAbs(project.CanonicalCwd) {
			continue
		}
		if !pathInsideRoot(canonicalRoot, project.CanonicalCwd) {
			continue
		}
		if !membership.belongs(ctx, resolver, project.CanonicalCwd) {
			// The directory is gone, or it belongs to a different worktree
			// (a submodule or a nested repository). Fail closed.
			continue
		}
		pathHash, hashErr := installationSalt.Hash(project.CanonicalCwd)
		if hashErr == nil && string(pathHash) == project.Hash {
			// pathHash is the validated identity: it was just proven equal to
			// the stored one, so there is nothing left to re-parse.
			recorded = append(recorded, push.RecordedUnderRoot{Hash: pathHash, Directory: project.CanonicalCwd})
			continue
		}
		if project.Hash == "" || project.Hash == string(rootIdentity) {
			// Identified by this repository's own remote: already in scope.
			continue
		}
		unadmitted = append(unadmitted, push.RecordedUnderRoot{
			Hash: ingest.ProjectHash(project.Hash), Directory: project.CanonicalCwd,
		})
	}
	return recorded, unadmitted, nil
}

// worktreeMembership answers "does this directory belong to the worktree rooted
// at root", spending a git subprocess only when the filesystem cannot prove it.
//
// The question was answered with one `git rev-parse --show-toplevel` per
// recorded directory. That is ~1.24ms each, paid on EVERY commit before any
// upload: 0.28s at 150 recorded directories and 0.84s at 600 — a monorepo pays
// it in full, and at that scale it can consume a hook's whole upload budget on
// local work while the failure message blames the village.
//
// Git decides membership by walking up from the directory until it finds a
// repository boundary — a `.git` directory or file. That walk is pure `lstat`,
// so it is done here directly, memoized across the shared ancestors that a set
// of sibling subdirectories has in common. Only what cannot be settled that way
// — a symlinked component, an unreadable directory — falls back to asking git,
// so the answer is never a guess.
type worktreeMembership struct {
	root string
	// clean records, per directory, whether the walk from it up to root found
	// no repository boundary. Sibling directories share ancestors, so a
	// monorepo resolves in roughly one lstat per distinct path component.
	clean map[string]bool
}

// repositoryRootResolver is the git question this falls back to. It is an
// interface so a test can prove how often the subprocess is actually reached —
// which is the whole point of the memoized walk above it.
type repositoryRootResolver interface {
	ResolveRepositoryRoot(ctx context.Context, dir string) (string, error)
}

func newWorktreeMembership(root string) *worktreeMembership {
	return &worktreeMembership{root: filepath.Clean(root), clean: map[string]bool{}}
}

// belongs reports whether dir is part of the worktree. dir must already be
// known to lie inside root lexically.
func (m *worktreeMembership) belongs(ctx context.Context, resolver repositoryRootResolver, dir string) bool {
	switch m.walk(filepath.Clean(dir)) {
	case boundaryNone:
		return true
	case boundaryFound:
		return false
	default:
		root, err := resolver.ResolveRepositoryRoot(ctx, dir)
		return err == nil && root == m.root
	}
}

// boundary is what a walk from a directory up to the worktree root found.
type boundary int

const (
	// boundaryNone means nothing between the directory and the root claims it,
	// so git resolves it to the root.
	boundaryNone boundary = iota
	// boundaryFound means a nested repository claims it, or it is not there.
	boundaryFound
	// boundaryUnknown means the filesystem could not settle it and git must be
	// asked: a symlinked component, or a directory that cannot be read.
	boundaryUnknown
)

func (m *worktreeMembership) walk(dir string) boundary {
	if dir == m.root {
		return boundaryNone
	}
	if known, seen := m.clean[dir]; seen {
		if known {
			return boundaryNone
		}
		return boundaryFound
	}
	info, err := os.Lstat(dir)
	if err != nil {
		// A recorded directory that is no longer there is not part of the
		// worktree. Anything else unreadable is not decidable here.
		if os.IsNotExist(err) {
			return boundaryFound
		}
		return boundaryUnknown
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		// git resolves symlinks before deciding; the lexical walk cannot.
		return boundaryUnknown
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		// A nested repository, a submodule, or a linked worktree starts here,
		// so git stops here and this directory is not the outer worktree's.
		m.clean[dir] = false
		return boundaryFound
	} else if !os.IsNotExist(err) {
		return boundaryUnknown
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		// The walk left the worktree without meeting the root, which can only
		// happen if the caller passed a directory outside it. Decide nothing.
		return boundaryUnknown
	}
	result := m.walk(parent)
	switch result {
	case boundaryNone:
		m.clean[dir] = true
	case boundaryFound:
		m.clean[dir] = false
	}
	return result
}

// pathInsideRoot reports whether path is root or lies beneath it.
func pathInsideRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// pushPhase identifies which kind of local work a run was doing when its budget
// expired. Whether an HTTP request was attempted is tracked independently by the
// client observer; entering a pipeline is not evidence that the village was
// contacted.
type pushPhase int

const (
	// pushPhaseLocal is everything before the village is contacted: loading
	// credentials and config, opening the analytics store, building the
	// redactor, the wizard, and the consent report.
	pushPhaseLocal pushPhase = iota
	// pushPhaseScope is resolving --repository into a project identity. It is
	// separated from the rest of the local work because it is the one local
	// step whose cost grows with the number of recorded projects, so it is the
	// one that plausibly consumes a short budget on its own.
	pushPhaseScope
)

// pushRun records what a push established before it returned. A failure that
// happened under the upload budget has to be reported in terms of that budget,
// including what had already reached the village and which repository was left
// partly unpublished — and both are only known part-way through the run.
type pushRun struct {
	phase pushPhase
	// requestedRepository is the --repository value recorded before resolution,
	// normalized to an absolute path when possible.
	//
	// It exists because the resolved scope is not available for the entire
	// window in which a containment-preserving remedy has to be printed: a
	// budget that expires while the scope is still being resolved leaves scope
	// nil, and a recovery rendered from nil drops --repository and tells the
	// user to push EVERY project on the machine. The flag value is known the
	// moment the flag is read, so it is kept from then on.
	requestedRepository string
	scope               *push.RepositoryScope
	result              *push.PushResult
	annotationSummary   *push.AnnotationPushSummary
	// binding is the explicitly-overridden config/data/state context this push
	// runs under. Every recovery command must retain it or it diagnoses a
	// different store from the one that failed.
	binding githooks.Binding
	// villageRequested is separate from the local/scope phase: constructing the
	// client or entering the pipelines does not mean an HTTP request happened.
	// The client flips this atomically immediately before its first attempt.
	villageRequested *atomic.Bool
}

// reachedVillage reports whether this run got as far as talking to the village.
func (r pushRun) reachedVillage() bool {
	return r.villageRequested != nil && r.villageRequested.Load()
}

func (r *pushRun) markVillageRequest() {
	if r.villageRequested != nil {
		r.villageRequested.Store(true)
	}
}

// resolveScope resolves --repository into a project identity and records, on the
// run, everything a later failure has to be able to say about it.
//
// The phase transitions are here rather than at the call site because their
// ORDER is the whole point and it was wrong: the phase was reset to local before
// the error was checked, so a run that genuinely exhausted its budget inside
// scope resolution — the one local step whose cost grows with the number of
// recorded projects — reported that the budget expired while loading credentials
// and config. The branch written for that case could never fire. On failure the
// phase stays at pushPhaseScope; it advances only when the resolution succeeded.
func (r *pushRun) resolveScope(ctx context.Context, db *store.Store, repository string) (*push.RepositoryScope, error) {
	// Recorded before resolution, so a failure DURING it can still name the
	// repository the caller asked to be confined to.
	if r.requestedRepository == "" {
		r.requestedRepository = repository
	}
	r.phase = pushPhaseScope
	scope, err := resolveRepositoryScope(ctx, db, repository)
	if err != nil {
		return nil, err
	}
	r.phase = pushPhaseLocal
	// Recorded as soon as it is known, so a budget error can name the repository
	// that was left partly unpublished.
	r.scope = scope
	return scope, nil
}

// repository names the repository this run was confined to: the resolved root
// once it is known, and the path the caller asked for before that. Empty only
// when the run was genuinely unscoped.
func (r pushRun) repository() string {
	if r.scope != nil {
		return r.scope.Root
	}
	return r.requestedRepository
}

// repositoryCommand renders the by-hand push for whatever this run was confined
// to, quoted so it survives a path with a space.
func (r pushRun) repositoryCommand() string {
	return githooks.ManualCommand(r.repository(), r.binding)
}

// scopeSuffix names the repository a scoped push was confined to, so a message
// above it says which repository was left partly unpublished — and so every
// command it prints keeps the containment the caller asked for.
func (r pushRun) scopeSuffix() string {
	repository := r.repository()
	if repository == "" {
		return ""
	}
	return " --repository " + githooks.ShellQuote(repository)
}

// pushNothingAttemptedError marks a push failure that happened before a single
// village request was made.
//
// It exists for one reader that cannot read prose: a generated Git hook, which
// sees only an exit status. Without it the hook's warning says "whatever the
// upload finished before it stopped is on the village and is recorded as
// published" after an expired login, where nothing was ever sent. The message
// is passed through unchanged; only the process status differs.
type pushNothingAttemptedError struct{ cause error }

func (e *pushNothingAttemptedError) Error() string { return e.cause.Error() }
func (e *pushNothingAttemptedError) Unwrap() error { return e.cause }

// uploadBudgetExceededError explains a push that ran out of its overall time
// budget, and says exactly what did and did not reach the village.
//
// Being cut off is not the same failure as being rejected, and the underlying
// "context deadline exceeded" says neither how long the budget was nor how to
// finish the job.
//
// The underlying error is deliberately NOT quoted. Under the cap the cancelled
// context propagates into SQLite, so the message used to announce the budget
// expiring, append "sqlite: prepare: interrupted", and then assert that the
// village had not answered — three lines that together read as local database
// corruption. Per-session detail is still written to the push error log.
func uploadBudgetExceededError(budget time.Duration, run pushRun) error {
	uploaded, failed := 0, 0
	if run.result != nil {
		uploaded = run.result.New + run.result.Updated
		failed = run.result.Errors
	}
	annotationChanges, annotationErrors := 0, 0
	if run.annotationSummary != nil {
		annotationChanges = run.annotationSummary.Created + run.annotationSummary.Updated + run.annotationSummary.Retracted
		annotationErrors = run.annotationSummary.Errors
	}
	why, when, impact := budgetPhaseNarrative(run, uploaded, failed, annotationChanges, annotationErrors)
	return fmt.Errorf(
		"village push: the upload ran out of its %s budget\n"+
			"What went wrong: the whole push was capped at %s with --timeout, and the cap expired before it finished.\n"+
			"Why: %s\n"+
			"Where: peasant village push%s.\n"+
			"When: %s.\n"+
			"Impact: %s\n"+
			"Fix: %s",
		budget, budget, why, run.scopeSuffix(), when, impact,
		budgetRecovery(budget, run, uploaded, annotationChanges))
}

// budgetPhaseNarrative says what the budget was actually spent on.
//
// The message asserted "the village did not answer in time" in every case. With
// the village not running and a short budget, the whole budget was spent on
// LOCAL work — resolving the repository scope — and zero HTTP requests were
// made, so the sentence named the wrong culprit and sent the user to check a
// network that was never used.
func budgetPhaseNarrative(run pushRun, uploaded, failed, annotationChanges, annotationErrors int) (why, when, impact string) {
	if run.reachedVillage() {
		return "the village did not answer in time. A village that accepts a connection and then stalls is the usual cause; a refused connection fails immediately instead",
			fmt.Sprintf("after %d transcript(s) had been uploaded and %d annotation change(s) completed, with %d transcript and %d annotation error(s) recorded", uploaded, annotationChanges, failed, annotationErrors),
			"whatever was uploaded before the cap is on the village and is recorded locally, so it is skipped next time; the rest was not sent, and nothing local was changed or lost."
	}
	switch run.phase {
	case pushPhaseScope:
		return "the budget expired during local work, before any village request was made: resolving which recorded sessions belong to this repository. The village was never contacted, so it is not the cause",
			"while resolving the repository scope",
			"nothing was sent and nothing was published; nothing local was changed or lost."
	default:
		return "the budget expired during local work, before any village request was made: loading credentials and config, opening the analytics store, and preparing the run. The village was never contacted, so it is not the cause",
			"before the upload started",
			"nothing was sent and nothing was published; nothing local was changed or lost."
	}
}

// budgetRecovery separates a run that was cut off but made progress from one
// that a fixed budget cannot finish at all.
//
// "If this ran from a git hook, no action is needed — the next commit or push
// retries" is true only while each run converts its budget into recorded
// uploads. A session too large to complete inside the cap does not: it is
// retried on every commit forever, costs the whole budget every time, and prints
// this error every time, while the message says nothing needs doing. The
// evidence is in this run — a budget that bought zero uploads will buy zero
// again — so the wording follows it, and names both remedies the earlier text
// never mentioned: an unbounded manual push, and reinstalling the hook with a
// larger budget.
func budgetRecovery(budget time.Duration, run pushRun, uploaded, annotationChanges int) string {
	manual := run.repositoryCommand()
	if !run.reachedVillage() {
		// The cap was spent before the first request. Nothing is half-published,
		// and nothing about the village is known — so neither "let it finish over
		// several runs" nor "the sessions do not fit" applies.
		return fmt.Sprintf(
			"raise the cap, because the run never got as far as sending anything: %s of local work was not enough on this machine. "+
				"Run '%s' by hand to see it finish — run by hand it has no budget.%s",
			budget, manual, hookReinstallAdvice(budget, run))
	}
	if uploaded+annotationChanges > 0 {
		return fmt.Sprintf(
			"nothing, if you are content to let this finish over several runs: %d transcript(s) and %d annotation change(s) got through and are recorded, so the next commit or push starts from the work that did not. "+
				"To finish it now, run '%s' by hand — run by hand it has no budget.",
			uploaded, annotationChanges, manual)
	}
	return fmt.Sprintf(
		"this run recorded no upload progress inside the cap, so retrying alone may never finish it. "+
			"If the village is simply unreachable the next attempt can succeed; if it is reachable, the remaining transcript or annotation work does not fit in %s and every commit will spend the whole budget and stop in the same place. "+
			"Run '%s' by hand once to settle which it is — run by hand it has no budget.%s",
		budget, manual, hookReinstallAdvice(budget, run))
}

// hookReinstallAdvice is the "if a git hook set this cap" sentence, and it is
// printed only when it can be made runnable.
//
// A generated hook ALWAYS scopes its push to one repository, so a run with no
// repository at all did not come from a hook and the advice is noise. When there
// is one, the command is built by the hook lifecycle's own builder, which cannot
// omit --dir: printed without it, the reinstall acts on whatever repository the
// user happens to be standing in — an error from outside a repository, and a
// silent upload hook installed into an unrelated one from inside a different
// repository, while the repository that actually timed out keeps its old cap.
func hookReinstallAdvice(budget time.Duration, run pushRun) string {
	repository := run.repository()
	if repository == "" {
		return ""
	}
	raised := raisedBudget(budget)
	return fmt.Sprintf(
		" If a git hook set this cap, reinstall it with a larger one: '%s' (and --event %s for the other one).",
		githooks.InstallCommandWithBudget(githooks.EventPostCommit, repository, raised, run.binding),
		githooks.EventPrePush)
}

// raisedBudget picks a cap that is actually larger than the one that just
// expired. A fixed suggestion is wrong the moment the expired budget is already
// at or above it: telling a user whose 60s cap ran out to reinstall with 30s is
// advice that cannot work from the state that printed it.
func raisedBudget(budget time.Duration) time.Duration {
	const floor = 30 * time.Second
	if doubled := budget * 2; doubled > floor {
		return doubled.Round(time.Second)
	}
	return floor
}

// runPushStages preserves both stage outcomes while enforcing the ordering
// required by transcript-backed annotation targets. The same context is passed
// to both stages, including when the transcript stage returns an error.
func runPushStages(
	ctx context.Context,
	transcriptStage func(context.Context) (*push.PushResult, error),
	annotationStage func(context.Context) (*push.AnnotationPushSummary, error),
) (result *push.PushResult, transcriptErr error, annotationSummary *push.AnnotationPushSummary, annotationErr error) {
	result, transcriptErr = transcriptStage(ctx)
	annotationSummary, annotationErr = annotationStage(ctx)
	return
}

// firstPushStageError retains the existing transcript-before-annotation error
// precedence for the command's final exit status.
func firstPushStageError(transcriptErr, annotationErr error) error {
	if transcriptErr != nil {
		return transcriptErr
	}
	return annotationErr
}

// sourceProviderHelp builds the --source-provider flag help text by deriving
// the provider list from schema.AllHarnesses (the canonical ingestion-supported
// harness set). Deriving — rather than hardcoding — means adding a new harness
// to AllHarnesses updates this help string automatically, so it can never drift
// out of sync with the providers peasant actually supports.
func sourceProviderHelp() string {
	names := make([]string, len(schema.AllHarnesses))
	for i, h := range schema.AllHarnesses {
		names[i] = h.String()
	}
	return fmt.Sprintf("Filter to a specific provider (%s)", strings.Join(names, ", "))
}

// buildAnnotationSelection assembles a push.AnnotationSelection from the
// repeatable --annotation-id / --annotation-hash flags. An empty result
// (no flags given) means "push every system annotation".
func buildAnnotationSelection(ids, hashes []string) push.AnnotationSelection {
	sel := push.AnnotationSelection{}
	if len(ids) > 0 {
		sel.IDs = make(map[string]bool, len(ids))
		for _, id := range ids {
			if id != "" {
				sel.IDs[id] = true
			}
		}
	}
	if len(hashes) > 0 {
		sel.ContentHashes = make(map[string]bool, len(hashes))
		for _, h := range hashes {
			if h != "" {
				sel.ContentHashes[h] = true
			}
		}
	}
	return sel
}

// jsonPushResult is the JSON-safe output for peasant push --json.
type jsonPushResult struct {
	New         int               `json:"new"`
	Updated     int               `json:"updated"`
	Skipped     int               `json:"skipped"`
	Errors      int               `json:"errors"`
	Held        int               `json:"held"`
	EmptyReason string            `json:"empty_reason,omitempty"`
	Sessions    []jsonPushSession `json:"sessions"`
	Annotations *jsonAnnotResult  `json:"annotations,omitempty"`
}

// jsonAnnotResult is the JSON-safe annotation push summary.
type jsonAnnotResult struct {
	Total         int                 `json:"total"`
	Created       int                 `json:"created"`
	Updated       int                 `json:"updated"`
	Skipped       int                 `json:"skipped"`
	Retracted     int                 `json:"retracted"`
	Errors        int                 `json:"errors"`
	Error         string              `json:"error,omitempty"`
	SkipReason    string              `json:"skip_reason,omitempty"`
	Unpublishable []jsonUnpublishable `json:"unpublishable,omitempty"`
}

// jsonUnpublishable is one stored annotation that cannot be put on the wire, in
// the machine-readable form. It carries its own recovery so a scripted consumer
// is not left to reconstruct one.
type jsonUnpublishable struct {
	ID            string `json:"id"`
	AnnotatorName string `json:"annotator_name"`
	TypeID        string `json:"type_id"`
	TargetKind    string `json:"target_kind"`
	SessionID     string `json:"session_id,omitempty"`
	Reason        string `json:"reason"`
	Recovery      string `json:"recovery"`
}

// jsonPushSession is the JSON-safe per-session result.
type jsonPushSession struct {
	SessionID string `json:"session_id"`
	HostSlug  string `json:"host_slug"`
	Title     string `json:"title,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// printPushJSON outputs the push result as JSON including annotation summary.
func printPushJSON(w io.Writer, result *push.PushResult, annSummary *push.AnnotationPushSummary, annErr error) error {
	out := jsonPushResult{
		New:         result.New,
		Updated:     result.Updated,
		Skipped:     result.Skipped,
		Errors:      result.Errors,
		Held:        result.Held,
		EmptyReason: result.EmptyReason,
	}
	for _, sr := range result.Sessions {
		js := jsonPushSession{
			SessionID: sr.SessionID,
			HostSlug:  sr.HostSlug,
			Title:     sr.Title,
			Status:    sr.Status.String(),
		}
		if sr.Error != nil {
			js.Error = sr.Error.Error()
		}
		out.Sessions = append(out.Sessions, js)
	}
	if annSummary != nil {
		out.Annotations = &jsonAnnotResult{
			Total:      annSummary.Total,
			Created:    annSummary.Created,
			Updated:    annSummary.Updated,
			Skipped:    annSummary.Skipped,
			Retracted:  annSummary.Retracted,
			Errors:     annSummary.Errors,
			SkipReason: annSummary.SkipReason,
		}
		for _, ann := range annSummary.Unpublishable {
			out.Annotations.Unpublishable = append(out.Annotations.Unpublishable, jsonUnpublishable{
				ID:            ann.ID,
				AnnotatorName: ann.AnnotatorName,
				TypeID:        ann.TypeID,
				TargetKind:    ann.TargetKind.String(),
				SessionID:     ann.SessionID,
				Reason:        ann.Reason,
				Recovery:      ann.Recovery(),
			})
		}
		if annErr != nil {
			out.Annotations.Error = annErr.Error()
		}
	} else if annErr != nil {
		out.Annotations = &jsonAnnotResult{Error: annErr.Error()}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// printAnnotationSummary outputs the annotation push summary with user-facing context.
func printAnnotationSummary(w io.Writer, summary *push.AnnotationPushSummary, err error, dryRun bool) {
	if err != nil {
		fmt.Fprintf(w, "\nAnnotations: push failed — %v\n", err)
		fmt.Fprintf(w, "  Your transcript push was not affected. Publishable annotations will be retried on next push.\n")
		printUnpublishableAnnotations(w, summary)
		return
	}
	if summary == nil {
		return
	}
	if summary.SkipReason != "" {
		fmt.Fprintf(w, "\nAnnotations: skipped (%d annotations not uploaded)\n", summary.Total)
		fmt.Fprintf(w, "  Reason: %s\n", summary.SkipReason)
		fmt.Fprintf(w, "  Your transcript push was not affected. Upgrade the village to enable annotation syncing.\n")
		printUnpublishableAnnotations(w, summary)
		return
	}
	if dryRun {
		fmt.Fprintf(w, "Annotations (dry run): %d would push\n", summary.Total)
		printUnpublishableAnnotations(w, summary)
		return
	}
	fmt.Fprintf(w, "Annotations: %d created, %d updated, %d skipped, %d retracted, %d error(s)\n",
		summary.Created, summary.Updated, summary.Skipped, summary.Retracted, summary.Errors)
	printUnpublishableAnnotations(w, summary)
}

// printQuietUnpublishableNotice is the one line a quiet run — which is what a
// git hook runs — prints when annotations were left behind because they cannot
// be published at all.
//
// It names the count and the command that shows the detail, and nothing else:
// the full per-annotation report belongs to the run a user asked for, not to
// every commit.
func printQuietUnpublishableNotice(w io.Writer, summary *push.AnnotationPushSummary, manual string) {
	if summary == nil || len(summary.Unpublishable) == 0 {
		return
	}
	fmt.Fprintf(w,
		"notice: %d annotation(s) cannot be published in their stored state and were left behind. Run '%s' to see which ones and how to clear them.\n",
		len(summary.Unpublishable), manual)
}

// printUnpublishableAnnotations reports the stored annotations that were left
// behind because they cannot be put on the wire at all.
//
// They are named individually, with the recovery for each, because the state is
// permanent: the same rows are left behind on every push until they are repaired
// or deleted, and a bare count gives a user no way to find them.
func printUnpublishableAnnotations(w io.Writer, summary *push.AnnotationPushSummary) {
	if summary == nil || len(summary.Unpublishable) == 0 {
		return
	}
	fmt.Fprintf(w, "\nAnnotations not publishable (%d) — left in the local store:\n",
		len(summary.Unpublishable))
	for _, ann := range summary.Unpublishable {
		fmt.Fprintf(w, "  %s  type=%s  target=%s  annotator=%s\n", ann.ID, ann.TypeID, ann.TargetKind, ann.AnnotatorName)
		fmt.Fprintf(w, "    why: %s\n", ann.Reason)
		fmt.Fprintf(w, "    fix: %s\n", ann.Recovery())
	}
}

// outputLevel is the resolved verbosity for a push run. It mirrors the
// typed-enum pattern used elsewhere (push.PushAction, the wizard's wizardPage):
// a small int enum with a String() so the value never round-trips through bare
// strings.
type outputLevel int

const (
	// outputQuiet suppresses the summary and redaction report, leaving only
	// errors (stderr) and a single final result line (stdout).
	outputQuiet outputLevel = iota
	// outputNormal prints a concise summary (the default).
	outputNormal
	// outputVerbose prints per-session detail in addition to the summary.
	outputVerbose
)

// String implements fmt.Stringer.
func (l outputLevel) String() string {
	switch l {
	case outputQuiet:
		return "quiet"
	case outputNormal:
		return "normal"
	case outputVerbose:
		return "verbose"
	default:
		return "unknown"
	}
}

// writePushErrorLog dumps per-session upload failures (status codes + server
// bodies) and any annotation error to a timestamped log under the XDG state
// directory, returning the file path. The default summary only shows an error
// count, so this gives the user the full detail without re-running with --verbose.
func writePushErrorLog(result *push.PushResult, annSummary *push.AnnotationPushSummary, annErr error, villageURL string, stateDirOv string) (string, error) {
	logDir := filepath.Join(defaults.ResolveStateDirPathWith(stateDirOv).String(), "logs")
	if err := os.MkdirAll(logDir, defaults.PrivateDirPerm); err != nil {
		return "", fmt.Errorf("create log directory %s: %w", logDir, err)
	}
	now := time.Now()
	path := filepath.Join(logDir, fmt.Sprintf("push--error--%s.log", now.Format("2006-01-02T15-04-05")))

	var b strings.Builder
	fmt.Fprintf(&b, "peasant push error log — %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "village: %s\n", villageURL)
	if result != nil {
		fmt.Fprintf(&b, "transcripts: %d new, %d updated, %d error(s), %d skipped, %d held\n",
			result.New, result.Updated, result.Errors, result.Skipped, result.Held)
	}
	if annSummary != nil {
		fmt.Fprintf(&b, "annotations: %d created, %d updated, %d skipped, %d error(s)\n",
			annSummary.Created, annSummary.Updated, annSummary.Skipped, annSummary.Errors)
	}
	b.WriteString("\n")

	if result != nil {
		for _, sr := range result.Sessions {
			if sr.Status == push.PushStatusError && sr.Error != nil {
				fmt.Fprintf(&b, "%s/%s\t%s\n", sr.HostSlug, sr.SessionID, sr.Error.Error())
			}
		}
	}
	if annErr != nil {
		fmt.Fprintf(&b, "\nannotations error: %v\n", annErr)
	}

	if err := os.WriteFile(path, []byte(b.String()), defaults.PrivateFilePerm); err != nil {
		return "", fmt.Errorf("write log file %s: %w", path, err)
	}
	return path, nil
}

// writeTimingLog writes the per-upload JSONL timing log (one line per transcript
// upload: sessionId / setupMs / serverMs / reused) to a timestamped file under
// the XDG state directory, returning the path. It mirrors writePushErrorLog's
// location/permission convention. An empty collector produces an empty file
// (header-less JSONL), which is harmless and keeps the path stable for tooling.
func writeTimingLog(c *perf.Collector, stateDirOv string) (string, error) {
	logDir := filepath.Join(defaults.ResolveStateDirPathWith(stateDirOv).String(), "logs")
	if err := os.MkdirAll(logDir, defaults.PrivateDirPerm); err != nil {
		return "", fmt.Errorf("create log directory %s: %w", logDir, err)
	}
	path := filepath.Join(logDir, fmt.Sprintf("push--timing--%s.jsonl", time.Now().Format("2006-01-02T15-04-05")))

	var b bytes.Buffer
	if err := perf.WriteJSONL(&b, c); err != nil {
		return "", fmt.Errorf("encode timing log: %w", err)
	}
	if err := os.WriteFile(path, b.Bytes(), defaults.PrivateFilePerm); err != nil {
		return "", fmt.Errorf("write timing log %s: %w", path, err)
	}
	return path, nil
}

// resolveOutputLevel maps the --quiet/--verbose flags to a typed outputLevel.
// Passing both is an error: the flags request opposite things, so the
// command refuses rather than silently picking one.
func resolveOutputLevel(quiet, verbose bool) (outputLevel, error) {
	if quiet && verbose {
		return outputNormal, fmt.Errorf(
			"--quiet and --verbose are mutually exclusive: --quiet suppresses output while --verbose expands it — pass at most one")
	}
	switch {
	case quiet:
		return outputQuiet, nil
	case verbose:
		return outputVerbose, nil
	default:
		return outputNormal, nil
	}
}

// printPushResultLine prints the single terse result line used in --quiet mode.
// It omits the multi-line "Summary:" block and per-session detail, keeping only
// the final tally so scripts get a confirmation without noise.
func printPushResultLine(w io.Writer, result *push.PushResult, dryRun bool) {
	if dryRun {
		fmt.Fprintf(w, "pushed 0 (dry run): %d would push\n", result.New+result.Updated)
	} else {
		fmt.Fprintf(w, "pushed %d session(s), %d error(s)\n", result.New+result.Updated, result.Errors)
	}
}

// printPushSummary outputs the human-readable push summary.
// Per-session detail rows are only printed when verbose=true.
func printPushSummary(w io.Writer, result *push.PushResult, dryRun bool, verbose bool) {
	if verbose {
		for _, sr := range result.Sessions {
			var statusLabel string
			switch sr.Status {
			case push.PushStatusNew:
				statusLabel = "new "
			case push.PushStatusUpdated:
				statusLabel = "upd "
			case push.PushStatusError:
				statusLabel = "err "
			case push.PushStatusSkipped:
				statusLabel = "skip"
			case push.PushStatusHeld:
				statusLabel = "held"
			default:
				statusLabel = sr.Status.String()
			}

			if sr.Error != nil {
				fmt.Fprintf(w, "  %s %s/%s  %s\n", statusLabel, sr.HostSlug, sr.SessionID, sr.Error.Error())
			} else if sr.Title != "" {
				fmt.Fprintf(w, "  %s %s/%s  %s\n", statusLabel, sr.HostSlug, sr.SessionID, sr.Title)
			} else {
				fmt.Fprintf(w, "  %s %s/%s\n", statusLabel, sr.HostSlug, sr.SessionID)
			}
		}
		fmt.Fprintln(w)
	}

	if dryRun {
		wouldPush := result.New + result.Updated
		fmt.Fprintf(w, "Dry run summary: %d would push, %d unchanged (0 errors — no HTTP calls made)\n",
			wouldPush, result.Skipped)
	} else {
		fmt.Fprintf(w, "Summary: %d new, %d updated, %d error(s), %d skipped, %d held\n",
			result.New, result.Updated, result.Errors, result.Skipped, result.Held)
	}
}

// printErrorSummaryTable prints an "Errors by type:" breakdown grouping the
// run's failed sessions by typed push.PushErrorCategory (deterministic order).
// It is a no-op when there were no errors. This complements the per-session
// detail in writePushErrorLog (which carries full messages); the table gives an
// at-a-glance count per category. Callers must NOT invoke it under --quiet or on
// the --json path.
func printErrorSummaryTable(w io.Writer, result *push.PushResult) {
	rows := push.SummarizePushErrors(result)
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w, "Errors by type:")
	for _, r := range rows {
		fmt.Fprintf(w, "  %-18s %d  (e.g. %s)\n", r.Category.String(), r.Count, r.Example)
	}
}

// buildPushWizardSessions assembles the exact []push.PushWizardSession the TUI
// will display: it runs the SHARED base query (QueryPushCandidates), partitions
// the rows with the branch-aware selection matcher (WizardCandidates — kept
// first, then withheld/Locked), and attaches per-session metadata for the
// redaction-status column.
//
// It is PURE of TTY (no BubbleTea), so tests can call it directly to assert the
// wizard's VIEW equals the selected set, with branch-conflict sessions surfaced
// as Locked. The base query is the same one the pipeline's getTargetSessions
// uses, so the wizard view and the dry-run/real push set cannot diverge.
func buildPushWizardSessions(
	ctx context.Context,
	db push.CandidateStore,
	fs ingest.FileSystem,
	outputBasePath string,
	q push.PushCandidateQuery,
	selection *ingest.SelectionMatcher,
) ([]push.PushWizardSession, error) {
	sessions, err := push.QueryPushCandidates(ctx, db, q)
	if err != nil {
		return nil, fmt.Errorf("query sessions for wizard: %w", err)
	}

	wizSessions := push.WizardCandidates(sessions, selection)
	for i := range wizSessions {
		sess := wizSessions[i].Row
		// Load metadata for redaction status (best-effort). Resolved via the
		// shared ingest helper so subagent sessions read from the correct
		// {parentID}/subagents/{id} location.
		metaPath := ingest.SessionMetadataPath(
			outputBasePath, sess.HostSlug, sess.SessionID, sess.ParentID,
		)
		metaBytes, readErr := fs.ReadFile(metaPath)
		if readErr != nil {
			continue
		}
		var meta schema.UnifiedMetadata
		if jsonErr := json.Unmarshal(metaBytes, &meta); jsonErr != nil {
			continue
		}
		wizSessions[i].Meta = &meta
	}
	return wizSessions, nil
}

// runPushWizard is a thin TTY wrapper over buildPushWizardSessions: it builds the
// selection-aware session list, runs the interactive BubbleTea program, and
// returns the session IDs the user confirmed for push (nil if cancelled, empty
// slice if there was nothing to show).
func runPushWizard(
	ctx context.Context,
	db push.CandidateStore,
	fs ingest.FileSystem,
	outputBasePath string,
	q push.PushCandidateQuery,
	selection *ingest.SelectionMatcher,
) ([]string, error) {
	wizSessions, err := buildPushWizardSessions(ctx, db, fs, outputBasePath, q, selection)
	if err != nil {
		return nil, err
	}
	if len(wizSessions) == 0 {
		// No sessions — skip wizard, let the pipeline handle the empty-state message.
		return []string{}, nil
	}

	model := push.NewPushWizard(wizSessions)
	p := tea.NewProgram(model)
	finalModel, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("push wizard: %w", err)
	}

	result := finalModel.(push.PushWizardModel)
	if result.Quitting() {
		return nil, nil // user cancelled
	}

	return result.SelectedSessionIDs(), nil
}

// pushCandidates reads the sessions this run would publish, using the same query
// the pipeline uses for the same flags.
func pushCandidates(
	ctx context.Context, db *store.Store, force bool, sourceProvider string,
) ([]ingest.PushSessionRow, error) {
	switch {
	case force:
		return db.AllPushableSessions(ctx)
	case sourceProvider != "":
		return db.UnpushedSessionsByProvider(ctx, sourceProvider)
	default:
		return db.UnpushedSessions(ctx)
	}
}

// filterToSelectedSessions narrows rows to an explicit session selection, which
// is what the interactive wizard produces. An empty selection narrows nothing.
func filterToSelectedSessions(sessions []ingest.PushSessionRow, selected []string) []ingest.PushSessionRow {
	if len(selected) == 0 {
		return sessions
	}
	keep := make(map[string]bool, len(selected))
	for _, id := range selected {
		keep[id] = true
	}
	kept := make([]ingest.PushSessionRow, 0, len(sessions))
	for _, session := range sessions {
		if keep[session.SessionID] {
			kept = append(kept, session)
		}
	}
	return kept
}

// configSourceDescription names where a configured value came from, so a refusal
// tells the user which file to edit rather than leaving them to guess which of
// several possible configuration paths was actually loaded.
func configSourceDescription(cfgPath string) string {
	if strings.TrimSpace(cfgPath) == "" {
		return "the default configuration (no configuration file was loaded)"
	}
	return "your configuration file " + cfgPath
}

// quietAware picks the form of a disclosure that suits the output level. A Git
// hook runs with --quiet on every commit, so a multi-line statement whose own
// content is "nothing to do" would arrive there forever; the one-line form says
// the same thing once per line.
func quietAware(level outputLevel, full, brief string) string {
	if level == outputQuiet {
		return brief
	}
	return full
}

// redactionRecord is the published-content record: what protection this push
// applies to what it is about to send.
//
// It used to be derived from the RedactionInfo stamped into each session's
// metadata file on disk, described as "the ground truth". That stopped being
// true. Nothing on the import path stamps it any more - there is no supported
// level that redacts while ingest writes - so every session on disk carries
// applied=false, and the record printed "redacted: 0 of N" and "(mixed or
// unknown)" on every push. It was reporting a field nobody sets rather than the
// protection actually being applied.
//
// The direction of that error mattered: it under-claimed, so nothing was ever
// over-shared. It was still worse than no record, because a record that always
// says zero invites the conclusion that nothing is redacted.
//
// So the record now describes the OUTWARD path, which is where redaction
// actually happens: the push re-redacts each session's metadata immediately
// before upload, at one known level, with the compiled rule set. Those two
// values are exact, identical for every session in the push, and known before
// the first byte leaves - which is what a record has to be.
type redactionRecord struct {
	// SessionCount is the number of sessions about to be published.
	SessionCount int
	// Level is the level this push applies to metadata on the way out.
	Level redact.RedactionLevel
	// RuleSetVersion is the compiled rule set this push applies. There is no
	// "stale" case: the outward redaction always runs the current rules,
	// whatever an older import happened to record.
	RuleSetVersion string
	// MissingMetadataCount is the number of sessions whose metadata file could
	// not be read or parsed. These are reported because the push itself needs
	// that file, so it is an early warning rather than a redaction fact.
	MissingMetadataCount int
}

// buildRedactionRecord assembles the record for a push at the given level.
//
// The only thing it reads from disk is whether each session's metadata file can
// be read at all. It deliberately does NOT read RedactionInfo: that field
// describes what happened at import, and what happens at import is no longer
// what is published.
func buildRedactionRecord(
	sessions []ingest.PushSessionRow,
	outputBasePath string,
	fs ingest.FileSystem,
	level redact.RedactionLevel,
) redactionRecord {
	record := redactionRecord{
		SessionCount:   len(sessions),
		Level:          level,
		RuleSetVersion: redact.RuleSetVersion,
	}
	for _, sess := range sessions {
		metaPath := ingest.SessionMetadataPath(
			outputBasePath, sess.HostSlug, sess.SessionID, sess.ParentID,
		)
		data, readErr := fs.ReadFile(metaPath)
		if readErr != nil {
			record.MissingMetadataCount++
			continue
		}
		var meta schema.UnifiedMetadata
		if jsonErr := json.Unmarshal(data, &meta); jsonErr != nil {
			record.MissingMetadataCount++
		}
	}
	return record
}

// redactionRecordEntrySeparator joins the labelled entries of the
// published-content record.
//
// It is named because the boundary is load-bearing for anything reading the
// record back. Each entry has to be findable as ONE run of text rather than
// assembled from pieces of several: the metadata and transcript-content entries
// both carry the phrase "redacted at <level> on upload", so a whole-output
// search for it is satisfied by either, and the content entry's level went
// unpinned that way while reading as covered. Anything asserting about one entry
// splits on this same constant, so a sibling cannot supply what the entry under
// test is missing.
const redactionRecordEntrySeparator = "\n"

// redactionRecordEntry renders one labelled entry of the published-content
// record, label and body together in a single segment.
func redactionRecordEntry(label, body string) string {
	return fmt.Sprintf("  %-20s %s", label, body)
}

// printRedactionReport writes the published-content record to w (stderr).
//
// Every line has to be true of the push that is about to run. The content line is
// the one that is easiest to leave out and the most important to keep, and what
// it must SAY has now changed once: content used to be published as recorded and
// is now redacted at the same level as metadata, through the same function the
// redact command uses. A record that listed only metadata would let a reader
// infer content was untouched - which is what it said while that was true, and
// what would understate the product now.
func printRedactionReport(w io.Writer, record redactionRecord) {
	// The header states the count only. It used to say "publicly", which was the
	// one visibility this version never publishes at, and the report is printed
	// for every push rather than for one visibility.
	fmt.Fprintf(w, "You are about to publish %d session(s).\n", record.SessionCount)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Redaction report:")
	entries := []string{
		redactionRecordEntry("metadata:",
			fmt.Sprintf("redacted at %s on upload, all %d session(s)", record.Level, record.SessionCount)),
		redactionRecordEntry("rule set version:", "v"+record.RuleSetVersion),
		redactionRecordEntry("transcript content:",
			fmt.Sprintf("redacted at %s on upload, matched patterns replaced", record.Level)),
	}
	if record.MissingMetadataCount > 0 {
		entries = append(entries, redactionRecordEntry("note:",
			fmt.Sprintf("%d session(s) missing metadata - the upload will fail for those until 'peasant ingest' re-creates them",
				record.MissingMetadataCount)))
	}
	fmt.Fprintln(w, strings.Join(entries, redactionRecordEntrySeparator))
	fmt.Fprintln(w)
	// The scope sentence is the same one the refusals and the wizard use. It says
	// what redaction finds and admits what it cannot promise; a completeness claim
	// here would be a defect.
	fmt.Fprintf(w, "%s\n", config.RedactionScopeSentence())
	fmt.Fprintln(w)
}

// promptPublicConsent prints the "Continue? [y/N]" prompt and reads the user's
// response from r.
//
// Returns (true, nil) when the user consents (response is "y" or "Y", or
// autoConfirm is set).
// Returns (false, nil) when the user declines or the environment is non-TTY
// without autoConfirm.
// Returns (false, err) only on unexpected I/O errors.
func promptPublicConsent(r io.Reader, w io.Writer, autoConfirm bool, isTTY bool) (bool, error) {
	if autoConfirm {
		return true, nil
	}
	if !isTTY {
		// Non-TTY without --yes: print a clear message and abort (exit 0).
		fmt.Fprintln(w, "note: public push requires confirmation; re-run with --yes to skip the prompt in non-interactive environments")
		return false, nil
	}

	fmt.Fprint(w, "Continue? [y/N] ")
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("read consent response: %w", err)
		}
		// EOF without input — treat as "no".
		return false, nil
	}
	response := strings.TrimSpace(scanner.Text())
	return response == "y" || response == "Y", nil
}
