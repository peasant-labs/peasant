package main

import (
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/spf13/cobra"
)

// requireVillageCredentials loads and validates credentials for a
// village-CONTACTING command (pull, remote list, annotations sync). The gate
// MIRRORS cmd_push.go:77-86 EXACTLY: it fails fast logged-out with an actionable
// error naming `peasant village login`, and separately rejects a missing village
// URL. Purely-local commands (`list --local`, `context`) MUST NOT call this.
func requireVillageCredentials(cmd *cobra.Command) (*auth.Credentials, error) {
	creds, err := auth.LoadCredentialsFrom(configDirOverride(cmd))
	if err != nil {
		return nil, fmt.Errorf("load credentials: %w", err)
	}
	if creds == nil || !creds.IsValid() {
		return nil, fmt.Errorf("not logged in — run 'peasant village login' first")
	}
	if creds.VillageURL == "" {
		return nil, fmt.Errorf("village URL is not set — run 'peasant village login' to re-link your account")
	}
	return creds, nil
}

// newVillageClientFromCreds constructs the production *village.VillageClient from
// loaded credentials. Centralizing the one-line construction means a future
// client-signature change touches a single site (used by villagePullPipeline and
// runVillageTranscriptsListRemote).
func newVillageClientFromCreds(creds *auth.Credentials) *village.VillageClient {
	return village.NewVillageClient(creds.VillageURL, creds.APIKey, nil)
}

// villagePullPipeline constructs a pull.Pipeline wired with real dependencies:
// a *village.VillageClient (the VillageReader, NegotiatePull called once per
// command inside the pipeline op), the opened *store.Store (PullStore), the
// system clock, the requester credentials mapped to pull.Credentials, and the
// resolved village-pulls root honoring --data-dir. The caller owns db's lifetime.
func villagePullPipeline(cmd *cobra.Command, creds *auth.Credentials, db *store.Store) *pull.Pipeline {
	client := newVillageClientFromCreds(creds)
	pullsRoot := string(defaults.ResolveVillagePullsDirPathWith(dataDirOverride(cmd)))
	return pull.NewPipeline(
		client,
		&ingest.OSFileSystem{},
		db,
		pull.SystemClock{},
		pull.Credentials{UserID: creds.UserID, VillageURL: creds.VillageURL},
		pullsRoot,
	)
}

// buildVillageTranscriptsPullCommand constructs `village transcripts pull
// <uuid|url>` (L2). Login is REQUIRED (it contacts the village). It resolves the
// ref, negotiates the pull contract once (inside the pipeline), fetches the
// transcript blob + annotations, and lands them atomically. Output conventions
// MIRROR push: the human summary goes to stdout, notices/dry-run banners to
// stderr, and --json emits a parseable result. Exit code keys off the typed
// PullStatus (0 for pulled/up-to-date; non-zero otherwise) — never a string.
func buildVillageTranscriptsPullCommand() *cobra.Command {
	var (
		force      bool
		dryRun     bool
		jsonOutput bool
		verbose    bool
		quiet      bool
	)

	cmd := &cobra.Command{
		Use:   "pull <uuid|url>",
		Short: "Pull a transcript and its annotations from the village (login required)",
		Long: `Pull one transcript (and the annotations authored on it) from the Peasant
village into the local village-pulls/ namespace and the pulled-transcripts
index. Pulled data is foreign and one-way: it never enters ingest, analytics, or
your annotate-push candidate set.

The reference is the transcript UUID or a pasted village web URL. --dry-run
reports what WOULD be pulled (and why / why-not) without writing anything.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Past argument validation, every failure here is a RUNTIME error
			// (auth gate, unreachable village, pipeline) — not a usage error. Cobra
			// dumps the Usage/Flags block on any RunE error unless silenced, which
			// drowns the actionable one-liner (e.g. "not logged in — run 'peasant
			// village login' first"). Silence usage so runtime errors stand alone;
			// cobra still prints usage for genuine arg/flag misuse (handled before RunE).
			cmd.SilenceUsage = true

			level, levelErr := resolveOutputLevel(quiet, verbose)
			if levelErr != nil {
				return levelErr
			}

			ref, err := pull.ParseTranscriptRef(args[0])
			if err != nil {
				return err
			}

			creds, err := requireVillageCredentials(cmd)
			if err != nil {
				return err
			}

			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			pipeline := villagePullPipeline(cmd, creds, db)

			if dryRun && !jsonOutput && level != outputQuiet {
				fmt.Fprintf(cmd.ErrOrStderr(), "Dry run — no transcript will be written\n\n")
			}

			result, pullErr := pipeline.PullTranscript(cmd.Context(), ref, pull.PullOptions{
				Force:  force,
				DryRun: dryRun,
			})

			if jsonOutput {
				if jErr := printPullJSON(cmd, ref, result, pullErr); jErr != nil {
					return jErr
				}
				return pullErr
			}

			if result != nil {
				printPullSummary(cmd, ref, result, dryRun, level)
			}
			// The pipeline error is already actionable (what/why/where/fix),
			// including the not-logged-in case naming `peasant village login` and
			// the compensation-failure case naming the orphan dir + --force.
			return pullErr
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Re-download and rewrite even when the local copy matches the served blob")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be pulled without writing anything")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output as JSON instead of human-readable")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Show extra pull detail (village host, pull directory, served-blob hash)")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "Suppress the summary; print only errors and a final result line")

	return cmd
}

// jsonPullResult is the JSON-safe output for `village transcripts pull --json`.
type jsonPullResult struct {
	TranscriptID    string `json:"transcriptId"`
	FromURL         bool   `json:"fromUrl"`
	Status          string `json:"status"`
	DryRun          bool   `json:"dryRun,omitempty"`
	VillageHost     string `json:"villageHost,omitempty"`
	PullDir         string `json:"pullDir,omitempty"`
	ServedBlobHash  string `json:"servedBlobHash,omitempty"`
	License         string `json:"license,omitempty"`
	AnnotationCount int    `json:"annotationCount"`
	Error           string `json:"error,omitempty"`
}

// printPullJSON writes the pull result as JSON. The status is the typed
// PullStatus string (the single serialization boundary).
func printPullJSON(cmd *cobra.Command, ref pull.TranscriptRef, result *pull.PullResult, pullErr error) error {
	out := jsonPullResult{
		TranscriptID: ref.ID.String(),
		FromURL:      ref.FromURL,
	}
	if result != nil {
		out.Status = result.Status.String()
		// Surface PullResult.DryRun so a machine consumer can distinguish a
		// would-pull (zero mutation) from an actual pull — the dry-run path returns
		// Status=pulled with identical PullDir/ServedBlobHash, so dryRun is the only
		// marker (the human banner is suppressed in --json mode).
		out.DryRun = result.DryRun
		out.VillageHost = result.VillageHost
		out.PullDir = result.PullDir
		out.ServedBlobHash = result.ServedBlobHash
		out.License = string(result.License)
		out.AnnotationCount = result.AnnotationCount
	} else {
		out.Status = pull.PullStatusError.String()
	}
	if pullErr != nil {
		out.Error = pullErr.Error()
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// printPullSummary writes the human-readable pull summary to stdout, routed by
// the resolved verbosity. The final result line is kept even under --quiet.
func printPullSummary(cmd *cobra.Command, ref pull.TranscriptRef, result *pull.PullResult, dryRun bool, level outputLevel) {
	w := cmd.OutOrStdout()

	verb := "pulled"
	if dryRun {
		verb = "would pull"
	}

	switch result.Status {
	case pull.PullStatusPulled:
		fmt.Fprintf(w, "%s transcript %s (%d annotation(s))\n", verb, ref.ID, result.AnnotationCount)
	case pull.PullStatusUpToDate:
		fmt.Fprintf(w, "transcript %s already up to date — nothing to pull\n", ref.ID)
	default:
		// Failure statuses surface via the returned error; print a terse line so
		// the typed outcome is still visible on stdout.
		fmt.Fprintf(w, "transcript %s: %s\n", ref.ID, result.Status)
	}

	if level == outputVerbose && result.Status == pull.PullStatusPulled {
		if result.VillageHost != "" {
			fmt.Fprintf(w, "  village: %s\n", result.VillageHost)
		}
		if result.PullDir != "" {
			fmt.Fprintf(w, "  dir:     %s\n", result.PullDir)
		}
		if result.ServedBlobHash != "" {
			fmt.Fprintf(w, "  hash:    %s\n", result.ServedBlobHash)
		}
		if result.License != "" {
			fmt.Fprintf(w, "  license: %s\n", result.License)
		}
	}
}
