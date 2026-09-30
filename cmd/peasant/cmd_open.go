package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/peasant-labs/peasant/internal/browser"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// openStep names the step of `peasant open` that failed, so the one failure
// line says where the command stopped.
type openStep string

const (
	openStepSession        openStep = "session check"
	openStepHarvest        openStep = "harvest"
	openStepLookup         openStep = "session lookup"
	openStepDashboardStart openStep = "dashboard start"
	openStepDashboardCheck openStep = "dashboard check"
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
// and the store: the web server it may start, the HTTP client that probes it,
// and the browser it opens.
type openDependencies struct {
	// startServer forks the server the spawn describes and returns its PID
	// file. It does not wait for the server to answer.
	startServer   func(spawn webServerSpawn) (pidFile string, err error)
	openBrowser   func(url string) error
	client        *http.Client
	readyWait     time.Duration
	readyInterval time.Duration
}

// BuildOpenCommand constructs `peasant open`, which records one session and
// opens its transcript in the local web dashboard.
func BuildOpenCommand() *cobra.Command {
	return buildOpenCommand(openDependencies{
		startServer: func(spawn webServerSpawn) (string, error) {
			_, pidFile, err := spawnWebServer(spawn)
			return pidFile, err
		},
		openBrowser:   browser.Open,
		client:        &http.Client{Timeout: defaults.ServerClientTimeout},
		readyWait:     webServerReadyWait,
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
			"checks that the dashboard serves the session, opens the transcript in the browser, and prints two lines on stdout:\n\n" +
			"  peasant: opened \"<title>\" · not published\n" +
			"  http://localhost:8690/projects/<project-hash>/<session-id>\n\n" +
			"The state reads \"published\" when the store holds a publication receipt for the session. " +
			"open opens the browser itself, so a caller must not open the address again.\n\n" +
			"A failed step prints one line on stderr and exits 1:\n\n" +
			"  peasant: <step> failed: <reason>; fix: <action>\n\n" +
			"The steps are \"session check\", \"harvest\", \"session lookup\", \"dashboard start\" and \"dashboard check\". " +
			"open never opens the dashboard root in place of the session.\n\n" +
			"With --hook, open prints the Claude Code hook response {\"continue\":false,\"stopReason\":\"...\"} as one line on stdout instead, " +
			"with the same lines in stopReason, writes nothing to stderr, and exits 0 on every outcome.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			// The harvest and the store report through the command's writers and
			// the default loggers. The lines below are the whole output of this
			// command, so everything else is muted while it runs.
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			restoreLoggers := muteDefaultLoggers()
			lines, failure := runOpen(cmd, deps, sessionID, port)
			restoreLoggers()
			cmd.SetOut(nil)
			cmd.SetErr(nil)

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

// muteDefaultLoggers sends the default slog logger and the standard log
// package to nowhere, and returns the function that puts both back. Setting a
// slog default also redirects the log package, and restoring the slog default
// alone does not undo that, so the log writer and flags are saved as well.
func muteDefaultLoggers() (restore func()) {
	previous, writer, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return func() {
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
	}
}

// runOpen records the session, reads what the output reports, makes sure the
// dashboard serves it, and opens the transcript. It returns the two output
// lines, or the failure of the first step that failed.
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
			fix:    fmt.Sprintf("run `%s` to see the full report", openHarvestCommand(cmd, id)),
		}
	}
	session, failure := lookupOpenSession(cmd, id)
	if failure != nil {
		return nil, failure
	}
	if failure := ensureDashboard(cmd, deps, port, id); failure != nil {
		return nil, failure
	}
	address := transcriptURL(port, session.projectHash, id)
	// A browser that cannot open, as on a headless host, is not a failure: the
	// printed address is the deliverable.
	_ = deps.openBrowser(address)
	return []string{openedLine(session.title, session.state), address}, nil
}

// openHarvestArgs is the harvest `open` runs, as `peasant harvest` arguments.
// The flags the harvest runs with and the command a failure line suggests are
// both built from it, so the two cannot differ.
func openHarvestArgs(id ingest.SessionID) []string {
	return []string{"--session", string(id), "--force", "--detect-commits"}
}

// openHarvestCommand is the harvest `open` runs, as a command the user can
// paste: the same arguments, on the configuration and directories of this run.
func openHarvestCommand(cmd *cobra.Command, id ingest.SessionID) string {
	return peasantCommand(cmd, append([]string{"harvest"}, openHarvestArgs(id)...)...)
}

// peasantCommand renders a peasant command line with the configuration and
// directory flags this run was given, so the command reads the same store.
func peasantCommand(cmd *cobra.Command, args ...string) string {
	return githooks.CommandPrefix(hookBinding(cmd)) + " " + strings.Join(args, " ")
}

// recordOpenSession harvests exactly this session, with commit detection,
// through the production harvest wiring. It prints nothing.
func recordOpenSession(cmd *cobra.Command, id ingest.SessionID) error {
	var flags harvestFlags
	parser := &cobra.Command{Use: "harvest"}
	registerHarvestFlags(parser, &flags, harvestAll)
	if err := parser.ParseFlags(openHarvestArgs(id)); err != nil {
		return fmt.Errorf("parse the harvest arguments: %w", err)
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
	case execution.kind != harvestCompletionSucceeded || execution.result == nil:
		return fmt.Errorf("the harvest ended without a result for session %s", id)
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
		return &openFailure{step: openStepLookup, reason: firstErrorLine(err), fix: fmt.Sprintf("run `%s` to check the database", peasantCommand(cmd, "harvest", "verify"))}
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
			fix: fmt.Sprintf("check the id with `%s`, or enable the harness that recorded it with `%s`",
				peasantCommand(cmd, "sessions", "list"), peasantCommand(cmd, "kickstart")),
		}
	}
	projectHash, err := schema.NewProjectHash(row.ProjectHash)
	if err != nil {
		return openedSession{}, &openFailure{
			step:   openStepLookup,
			reason: fmt.Sprintf("session %s is recorded without a valid project: %s", id, firstErrorLine(err)),
			fix:    fmt.Sprintf("record it again with `%s`", openHarvestCommand(cmd, id)),
		}
	}
	published, err := db.HasPublication(ctx, string(id))
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
// when none does, and that it serves the session: a server started on another
// data directory would answer the transcript address with a page that cannot
// show it.
func ensureDashboard(cmd *cobra.Command, deps openDependencies, port int, id ingest.SessionID) *openFailure {
	base := dashboardBaseURL(port)
	if !webServerHealthy(cmd.Context(), deps.client, base) {
		failed := func(reason string) *openFailure {
			return &openFailure{
				step:   openStepDashboardStart,
				reason: reason,
				fix: fmt.Sprintf("run `%s` to see why, or pass --port with a free port",
					peasantCommand(cmd, "web", "start", "--foreground", "--port", fmt.Sprint(port))),
			}
		}
		pidFile, err := deps.startServer(webServerSpawnFor(cmd, port))
		if err != nil {
			return failed(firstErrorLine(err))
		}
		// keep `peasant open` budget-only even if its command context becomes
		// cancellable; making it cancellable must also keep the PID file of a
		// server that is still starting (tracked as a follow-up)
		if !waitForWebServer(context.WithoutCancel(cmd.Context()), deps.client, base, deps.readyWait, deps.readyInterval) {
			// The forked server is gone or never answered; a PID file left behind
			// would point `peasant web stop` at a process that is not it.
			if pidFile != "" {
				_ = os.Remove(pidFile)
			}
			return failed(fmt.Sprintf("no Peasant server answered %s%s within %s of the start, and another program may hold port %d", base, defaults.RouteHealth, deps.readyWait, port))
		}
	}
	serves, err := dashboardServesSession(cmd.Context(), deps.client, base, id)
	if err != nil || !serves {
		reason := fmt.Sprintf("the dashboard on port %d does not serve session %s, so it reads another data directory or mock data", port, id)
		if err != nil {
			reason = fmt.Sprintf("the dashboard on port %d did not answer the session lookup: %s", port, firstErrorLine(err))
		}
		return &openFailure{
			step:   openStepDashboardCheck,
			reason: reason,
			fix: fmt.Sprintf("stop it with `%s` and run open again, or pass --port with a free port",
				peasantCommand(cmd, "web", "stop", "--port", fmt.Sprint(port))),
		}
	}
	return nil
}

// dashboardServesSession asks the running server whether it holds the session,
// through the link-resolving summaries read the dashboard itself uses. That
// read applies neither origin nor selection scope, so a recorded session is
// found whenever the server reads the store this command wrote.
func dashboardServesSession(ctx context.Context, client *http.Client, base string, id ingest.SessionID) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+defaults.RouteSessionSummaries.String()+"?ids="+url.QueryEscape(string(id)), nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(request)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%s answered %s", defaults.RouteSessionSummaries, resp.Status)
	}
	var envelope struct {
		Sessions []schema.SessionSummary `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return false, fmt.Errorf("decode the %s answer: %w", defaults.RouteSessionSummaries, err)
	}
	for _, summary := range envelope.Sessions {
		if summary.ID == string(id) {
			return true, nil
		}
	}
	return false, nil
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

// oneLine keeps text on one terminal line that reads in order: format
// characters, such as bidirectional overrides, are dropped, and line breaks,
// other whitespace, and control characters become single spaces.
func oneLine(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
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
