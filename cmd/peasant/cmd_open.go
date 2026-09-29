package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/peasant-labs/peasant/internal/browser"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// openStep names the step of `peasant open` that failed, so the one failure
// line says where the command stopped.
type openStep string

const (
	openStepSession   openStep = "session check"
	openStepHarvest   openStep = "harvest"
	openStepLookup    openStep = "session lookup"
	openStepDashboard openStep = "dashboard start"
)

// publishState is the publication state the first output line reports. It is
// read from the stored publication receipt, never inferred.
type publishState string

const (
	publishStateNotPublished publishState = "not published"
	publishStatePublished    publishState = "published"
)

// transcriptRoutePrefix is the web dashboard's session viewer route:
// /projects/<project-hash>/<session-id>.
const transcriptRoutePrefix = "/projects/"

// openFailure is one failed step and the action that fixes it. It renders as
// the single line the command prints on failure.
type openFailure struct {
	step   openStep
	reason string
	fix    string
}

func (f *openFailure) line() string {
	return oneLine(fmt.Sprintf("peasant: %s failed: %s; fix: %s", f.step, strings.TrimRight(f.reason, ". "), f.fix))
}

// openFailedError carries a failure to main as a non-zero exit. The command has
// already printed the failure line, so Cobra prints nothing more.
type openFailedError struct{ failure *openFailure }

func (e *openFailedError) Error() string { return e.failure.line() }

// hookStop is the Claude Code hook response that ends the prompt without a
// model turn and shows stopReason to the user.
type hookStop struct {
	Continue   bool   `json:"continue"`
	StopReason string `json:"stopReason"`
}

// openDependencies are the side effects of `peasant open` outside the harvest
// and the store: the web server it may start, the health probe that decides
// whether one runs, and the browser it opens.
type openDependencies struct {
	// startServer forks the web server on port. It does not wait for it.
	startServer   func(cmd *cobra.Command, port int) error
	openBrowser   func(url string) error
	health        *http.Client
	readyAttempts int
	readyInterval time.Duration
}

// BuildOpenCommand constructs `peasant open`, which records one session and
// opens its transcript in the local web dashboard.
func BuildOpenCommand() *cobra.Command {
	return buildOpenCommand(openDependencies{
		startServer: func(cmd *cobra.Command, port int) error {
			_, _, err := spawnWebServer(webServerSpawnFor(cmd, port))
			return err
		},
		openBrowser:   browser.Open,
		health:        &http.Client{Timeout: defaults.ServerClientTimeout},
		readyAttempts: defaults.HealthCheckAttempts,
		readyInterval: defaults.HealthCheckInterval,
	})
}

func buildOpenCommand(deps openDependencies) *cobra.Command {
	var (
		sessionID string
		port      int
		hook      bool
	)
	cmd := &cobra.Command{
		Use:   "open",
		Short: "Record one session and open its transcript in the web dashboard",
		Long: "Record one session and open its transcript in the local web dashboard.\n\n" +
			"open harvests the session with commit detection, starts the web dashboard when it is not running, " +
			"opens the transcript in the browser, and prints two lines:\n\n" +
			"  peasant: opened \"<title>\" · not published\n" +
			"  http://localhost:8690/projects/<project-hash>/<session-id>\n\n" +
			"The state reads \"published\" when the store holds a publication receipt for the session. " +
			"A failed step prints one line that names the step and the fix, and the command exits non-zero. " +
			"open never opens the dashboard root in place of the session.\n\n" +
			"With --hook, open prints the Claude Code hook response {\"continue\":false,\"stopReason\":\"...\"} instead, " +
			"with the same lines in stopReason, and exits 0 on every outcome.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			// The harvest and the store report through the command's writers and
			// the default logger. The lines below are the whole output of this
			// command, so everything else is muted.
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
			lines, failure := runOpen(cmd, deps, sessionID, port)
			slog.SetDefault(previousLogger)

			if hook {
				reason := strings.Join(lines, "\n")
				if failure != nil {
					reason = failure.line()
				}
				return writeHookStop(stdout, reason)
			}
			if failure != nil {
				fmt.Fprintln(stderr, failure.line())
				return &openFailedError{failure: failure}
			}
			for _, line := range lines {
				fmt.Fprintln(stdout, line)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "", "ID of the session to record and open (required)")
	cmd.Flags().IntVar(&port, "port", defaults.DefaultPort, "Port of the web dashboard")
	cmd.Flags().BoolVar(&hook, "hook", false, "Print the Claude Code hook response JSON instead of plain lines, and exit 0 on every outcome")
	return cmd
}

// runOpen records the session, reads what the output reports, makes sure the
// dashboard answers, and opens the transcript. It returns the two output lines,
// or the failure of the first step that failed.
func runOpen(cmd *cobra.Command, deps openDependencies, rawSessionID string, port int) ([]string, *openFailure) {
	id, err := ingest.NewSessionID(rawSessionID)
	if err != nil {
		reason := fmt.Sprintf("%q is not a session id: %s", rawSessionID, firstErrorLine(err))
		if rawSessionID == "" {
			reason = "no session id was given"
		}
		return nil, &openFailure{step: openStepSession, reason: reason, fix: "pass the session to open as `peasant open --session <id>`"}
	}
	if err := recordOpenSession(cmd, id); err != nil {
		return nil, &openFailure{
			step:   openStepHarvest,
			reason: firstErrorLine(err),
			fix:    fmt.Sprintf("run `peasant harvest --session %s --detect-commits` to see the full report", id),
		}
	}
	session, failure := lookupOpenSession(cmd, id)
	if failure != nil {
		return nil, failure
	}
	if failure := ensureDashboard(cmd, deps, port); failure != nil {
		return nil, failure
	}
	address := transcriptURL(port, session.projectHash, id)
	// A browser that cannot open, as on a headless host, is not a failure: the
	// printed address is the deliverable.
	_ = deps.openBrowser(address)
	return []string{openedLine(session.title, session.state), address}, nil
}

// recordOpenSession harvests exactly this session, with commit detection,
// through the production harvest wiring. It prints nothing.
func recordOpenSession(cmd *cobra.Command, id ingest.SessionID) error {
	flags := harvestFlags{
		force:         true,
		includeActive: true,
		detectCommits: true,
		sessionIDs:    []string{string(id)},
	}
	return runHarvestWith(cmd, harvestAll, &flags, &ingest.OSFileSystem{}, func(_ *cobra.Command, execution harvestExecution, _ harvestOutputOptions) error {
		return openHarvestOutcome(execution, id)
	})
}

// openHarvestOutcome reduces a harvest execution to whether this session was
// recorded and indexed. A session the sources do not hold is not an error here:
// the lookup that follows reports it.
func openHarvestOutcome(execution harvestExecution, id ingest.SessionID) error {
	switch {
	case execution.uiErr != nil:
		return fmt.Errorf("harvest progress failed: %w", execution.uiErr)
	case execution.kind == harvestCompletionCanceled:
		return harvestCancellationError(execution.ctxErr)
	case execution.kind == harvestCompletionFailed:
		return fmt.Errorf("pipeline failed: %w", execution.runErr)
	}
	result := execution.result
	if result.Summary.StoreError != nil {
		return fmt.Errorf("store session %s: %w", id, result.Summary.StoreError)
	}
	for _, session := range result.Sessions {
		if session.SessionID == id && session.Error != nil {
			return fmt.Errorf("record session %s: %w", id, session.Error)
		}
	}
	if slices.Contains(ingest.FailedIndexSessions(result.IndexLog), id) {
		return fmt.Errorf("session %s was stored, but its transcript could not be indexed for the viewer", id)
	}
	return nil
}

// openedSession is what the output reports about the recorded session.
type openedSession struct {
	title       string
	projectHash schema.ProjectHash
	state       publishState
}

func lookupOpenSession(cmd *cobra.Command, id ingest.SessionID) (openedSession, *openFailure) {
	storeFailure := func(err error) *openFailure {
		return &openFailure{step: openStepLookup, reason: firstErrorLine(err), fix: "run `peasant harvest verify` to check the database"}
	}
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return openedSession{}, storeFailure(err)
	}
	defer cleanup()
	ctx := cmd.Context()
	row, err := db.SessionByID(ctx, string(id))
	if err != nil {
		return openedSession{}, storeFailure(err)
	}
	if row == nil {
		return openedSession{}, &openFailure{
			step:   openStepLookup,
			reason: fmt.Sprintf("session %s is not recorded, and no configured source holds a transcript with that id", id),
			fix:    "check the id with `peasant sessions list`, or enable the harness that recorded it with `peasant kickstart`",
		}
	}
	projectHash, err := schema.NewProjectHash(row.ProjectHash)
	if err != nil {
		return openedSession{}, &openFailure{
			step:   openStepLookup,
			reason: fmt.Sprintf("session %s is recorded without a valid project: %s", id, firstErrorLine(err)),
			fix:    fmt.Sprintf("record it again with `peasant harvest --session %s --force`", id),
		}
	}
	published, err := db.HasPublication(ctx, projectHash, string(id))
	if err != nil {
		return openedSession{}, storeFailure(err)
	}
	session := openedSession{projectHash: projectHash, state: publishStateNotPublished}
	if published {
		session.state = publishStatePublished
	}
	if row.Title != nil {
		session.title = *row.Title
	}
	return session, nil
}

// ensureDashboard makes sure a Peasant web server answers on port, starting one
// when none does.
func ensureDashboard(cmd *cobra.Command, deps openDependencies, port int) *openFailure {
	base := dashboardBaseURL(port)
	if webServerHealthy(deps.health, base) {
		return nil
	}
	failed := func(reason string) *openFailure {
		return &openFailure{
			step:   openStepDashboard,
			reason: reason,
			fix:    fmt.Sprintf("run `peasant web start --foreground --port %d` to see why, or pass --port with a free port", port),
		}
	}
	if err := deps.startServer(cmd, port); err != nil {
		return failed(firstErrorLine(err))
	}
	if !waitForWebServer(deps.health, base, deps.readyAttempts, deps.readyInterval) {
		wait := time.Duration(deps.readyAttempts) * deps.readyInterval
		return failed(fmt.Sprintf("the server was started, but %s%s did not answer within %s", base, defaults.RouteHealth, wait))
	}
	return nil
}

// transcriptURL is the dashboard address of the session's transcript. It always
// names the session: open never falls back to the dashboard root.
func transcriptURL(port int, projectHash schema.ProjectHash, id ingest.SessionID) string {
	return dashboardBaseURL(port) + transcriptRoutePrefix + url.PathEscape(projectHash.String()) + "/" + url.PathEscape(string(id))
}

func openedLine(title string, state publishState) string {
	if title = oneLine(title); title == "" {
		return fmt.Sprintf("peasant: opened an untitled session · %s", state)
	}
	return fmt.Sprintf("peasant: opened \"%s\" · %s", title, state)
}

func writeHookStop(w io.Writer, reason string) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(hookStop{Continue: false, StopReason: reason})
}

// oneLine keeps text on one terminal line: line breaks, other whitespace, and
// control characters become single spaces.
func oneLine(text string) string {
	return strings.Join(strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// firstErrorLine is the first line of an error. Peasant errors put what failed
// first and the full explanation after it; the full report stays one command
// away in each failure's fix.
func firstErrorLine(err error) string {
	text, _, _ := strings.Cut(err.Error(), "\n")
	return oneLine(text)
}
