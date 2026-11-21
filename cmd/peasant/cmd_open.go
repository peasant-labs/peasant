package main

import (
	"context"
	"encoding/json"
	"errors"
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
// and the store: the web server it may start and stop, the HTTP client that
// probes it, the browser it opens, and the process loggers it mutes.
type openDependencies struct {
	// startServer forks the server the spawn describes and returns its PID and
	// PID file. It does not wait for the server to answer.
	startServer func(spawn webServerSpawn) (pid int, pidFile string, err error)
	// stopServer stops a server startServer forked that never answered.
	stopServer  func(pid int) error
	openBrowser func(url string) error
	// muteLoggers silences the process-wide loggers while open runs and
	// returns the function that puts them back.
	muteLoggers   func() (restore func())
	client        *http.Client
	readyWait     time.Duration
	readyInterval time.Duration
}

// BuildOpenCommand constructs `peasant open`, which records one session and
// opens its transcript in the local web dashboard.
func BuildOpenCommand() *cobra.Command {
	return buildOpenCommand(openDependencies{
		startServer:   spawnWebServer,
		stopServer:    stopForkedServer,
		openBrowser:   browser.Open,
		muteLoggers:   muteDefaultLoggers,
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
		noRelated bool
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
			"with the same lines in stopReason, writes nothing to stderr, and exits 0 on every outcome of the run. " +
			"A flag or argument error, such as an extra argument, stops the command before it runs: " +
			"it exits 1 with the usage on stderr, as for every command.",
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
			restoreLoggers := deps.muteLoggers()
			lines, warning, failure := runOpen(cmd, deps, sessionID, port, noRelated)
			restoreLoggers()
			cmd.SetOut(nil)
			cmd.SetErr(nil)

			if hook {
				if warning != "" {
					lines = append(lines, warning)
				}
				reason := strings.Join(lines, "\n")
				if failure != nil {
					reason = failure.line()
				}
				return writeHookStop(stdout, reason)
			}
			if warning != "" {
				fmt.Fprintln(stderr, warning)
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
	portFlagVar(cmd.Flags(), &port, defaults.DefaultPort, "Port of the web dashboard (1-65535)")
	cmd.Flags().BoolVar(&hook, "hook", false, "Print the Claude Code hook response JSON instead of plain lines, and exit 0 on every outcome of the run (a flag error still exits 1)")
	cmd.Flags().BoolVar(&noRelated, "no-related", false, "Process only the named sessions. By default a session-scoped run also processes each named session's child sessions and its ancestors that are not recorded yet.")
	return cmd
}

// stopForkedServer stops a server spawnWebServer forked, by the mechanism the
// platform offers (see terminateProcess).
func stopForkedServer(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return terminateProcess(proc)
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
func runOpen(cmd *cobra.Command, deps openDependencies, rawSessionID string, port int, noRelated bool) ([]string, string, *openFailure) {
	id, err := ingest.NewSessionID(rawSessionID)
	if err != nil {
		return nil, "", &openFailure{
			step:   openStepSession,
			reason: sessionCheckReason(rawSessionID, err),
			fix:    fmt.Sprintf("pass the session to open as `%s`", peasantCommand(cmd, "open", "--session", "<id>")),
		}
	}
	warning, err := recordOpenSession(cmd, id, noRelated)
	if err != nil {
		// A scope miss already renders as a full, actionable sentence pair: the
		// reason names the searched sources and the fix names the command, so
		// open passes both through instead of quoting only the first line.
		var scopeErr *ingest.SessionScopeError
		if errors.As(err, &scopeErr) {
			return nil, warning, &openFailure{step: openStepHarvest, reason: scopeErr.Reason(), fix: scopeErr.FixLine()}
		}
		return nil, warning, &openFailure{
			step:   openStepHarvest,
			reason: firstErrorLine(err),
			fix:    fmt.Sprintf("run `%s` to see the reason for each session", openHarvestCommand(cmd, id, noRelated, "--json")),
		}
	}
	session, failure := lookupOpenSession(cmd, id, noRelated)
	if failure != nil {
		return nil, warning, failure
	}
	if failure := ensureDashboard(cmd, deps, port, id); failure != nil {
		return nil, warning, failure
	}
	address := transcriptURL(port, session.projectHash, id)
	// A browser that cannot open, as on a headless host, is not a failure: the
	// printed address is the deliverable.
	_ = deps.openBrowser(address)
	return []string{openedLine(session.title, session.state), address}, warning, nil
}

// openHarvestArgs is the harvest `open` runs, as `peasant harvest` arguments.
// The flags the harvest runs with and the command a failure line suggests are
// both built from it, so the two cannot differ.
func openHarvestArgs(id ingest.SessionID, noRelated bool) []string {
	args := []string{"--session", string(id), "--detect-commits", "--refresh-commits"}
	if noRelated {
		args = append(args, "--no-related")
	}
	return args
}

// openHarvestCommand is the harvest `open` runs, as a command the user can
// paste: the same arguments, on the configuration and directories of this run,
// followed by any extra report flags.
func openHarvestCommand(cmd *cobra.Command, id ingest.SessionID, noRelated bool, extra ...string) string {
	args := append([]string{"harvest"}, openHarvestArgs(id, noRelated)...)
	return peasantCommand(cmd, append(args, extra...)...)
}

// sessionCheckReason says why rawSessionID is not a session id. The
// validation error opens with "invalid session ID" and the quoted value, which
// the line already says, so only its explanation is kept.
func sessionCheckReason(rawSessionID string, err error) string {
	if rawSessionID == "" {
		return "no session id was given"
	}
	detail := firstErrorLine(err)
	if _, explanation, ok := strings.Cut(detail, ": "); ok {
		detail = explanation
	}
	return fmt.Sprintf("%q is not a session id: %s", rawSessionID, detail)
}

// peasantCommand renders a peasant command line with the configuration and
// directory flags this run was given, so the command reads the same store.
func peasantCommand(cmd *cobra.Command, args ...string) string {
	return githooks.CommandPrefix(hookBinding(cmd)) + " " + strings.Join(args, " ")
}

// recordOpenSession harvests exactly this session, with commit detection,
// through the production harvest wiring. It prints nothing.
func recordOpenSession(cmd *cobra.Command, id ingest.SessionID, noRelated bool) (string, error) {
	var flags harvestFlags
	parser := &cobra.Command{Use: "harvest"}
	registerHarvestFlags(parser, &flags, harvestAll)
	if err := parser.ParseFlags(openHarvestArgs(id, noRelated)); err != nil {
		return "", fmt.Errorf("parse the harvest arguments: %w", err)
	}
	var warning string
	err := runHarvestWith(cmd, harvestAll, &flags, &ingest.OSFileSystem{}, func(_ *cobra.Command, execution harvestExecution, _ harvestOutputOptions) error {
		warning = openUnreadableWarning(execution, id)
		return openHarvestOutcome(execution, id)
	})
	return warning, err
}

// openUnreadableWarning is open's one stderr line when the selected session is
// recorded but its source could not be read again. The run still succeeds: the
// stored copy is what open shows.
func openUnreadableWarning(execution harvestExecution, id ingest.SessionID) string {
	if execution.result == nil || execution.result.ScopeReport == nil {
		return ""
	}
	member, ok := execution.result.ScopeReport.SelectedMember(id)
	if !ok {
		return ""
	}
	return member.UnreadableOpenLine()
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

func lookupOpenSession(cmd *cobra.Command, id ingest.SessionID, noRelated bool) (openedSession, *openFailure) {
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
			fix:    fmt.Sprintf("record it again with `%s`", openHarvestCommand(cmd, id, noRelated)),
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
		pid, pidFile, err := deps.startServer(webServerSpawnFor(cmd, port))
		if err != nil {
			return failed(firstErrorLine(err))
		}
		// keep `peasant open` budget-only even if its command context becomes
		// cancellable; making it cancellable must also keep the PID file of a
		// server that is still starting (tracked as a follow-up)
		if !waitForWebServer(context.WithoutCancel(cmd.Context()), deps.client, base, deps.readyWait, deps.readyInterval) {
			reason := fmt.Sprintf("no Peasant server answered %s%s within %s of the start, and another program may hold port %d", base, defaults.RouteHealth, deps.readyWait, port)
			if stopErr := stopUnreadyServer(deps, pid, pidFile); stopErr != nil {
				reason += fmt.Sprintf("; the started server (PID %d) could not be stopped (%s), so its PID file was kept", pid, firstErrorLine(stopErr))
			}
			return failed(reason)
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

// stopUnreadyServer stops a forked server that never answered, then removes its
// PID file. A slow start can still be running, so the PID file goes only once
// the process is stopped: removed first, it would leave a server that
// `peasant web stop` can no longer find. A process that already exited counts
// as stopped. When the stop fails, the PID file stays and the error is
// returned.
func stopUnreadyServer(deps openDependencies, pid int, pidFile string) error {
	if pid > 0 {
		if err := deps.stopServer(pid); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	if pidFile != "" {
		_ = os.Remove(pidFile)
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
	var payload schema.SessionsPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, fmt.Errorf("decode the %s answer: %w", defaults.RouteSessionSummaries, err)
	}
	for _, summary := range payload.Sessions {
		if summary.ID == string(id) {
			return true, nil
		}
	}
	return false, nil
}

// transcriptURL is the dashboard address of the session's transcript. It always
// names the session: open never falls back to the dashboard root.
func transcriptURL(port int, projectHash schema.ProjectHash, id ingest.SessionID) string {
	return dashboardBaseURL(port) + defaults.SPARouteProjects.String() + url.PathEscape(projectHash.String()) + "/" + url.PathEscape(string(id))
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
