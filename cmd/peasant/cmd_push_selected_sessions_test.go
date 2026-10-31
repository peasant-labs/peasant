package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/push_selected_sessions.yaml
var pushSelectedSessionsYAML []byte

//go:embed testdata/push_selected_sessions_manifest.yaml
var pushSelectedSessionsManifestYAML []byte

//go:embed testdata/push_plan_closed_sets.yaml
var pushPlanClosedSetsYAML []byte

// The collectives the Village double knows. Fixture args name them by
// placeholder, so a case reads as names and a test can check ids.
var selectedPushCollectives = []testutil.VillageCollective{
	{ID: "aaaaaaaa-0000-4000-8000-000000000001", Name: "research", Acceptance: schema.VillageGroupAcceptanceOpen, Member: true},
	{ID: "aaaaaaaa-0000-4000-8000-000000000002", Name: "review", Acceptance: schema.VillageGroupAcceptanceCurated, Member: true},
	{ID: "aaaaaaaa-0000-4000-8000-000000000003", Name: "twin", Acceptance: schema.VillageGroupAcceptanceOpen, Member: true},
	{ID: "aaaaaaaa-0000-4000-8000-000000000004", Name: "twin", Acceptance: schema.VillageGroupAcceptanceOpen, Member: true},
}

type selectedSeed struct {
	ID string `yaml:"id"`
	// Kind is ready (a verified capture), needs_ingest (a stored row with no
	// verified capture), latest_attempt (a last-good capture with a newer
	// incomplete attempt kept beside it), no_model_reply (a verified capture
	// with no model and no assistant reply), or model_gap (a verified capture
	// with an assistant reply but no model).
	Kind string `yaml:"kind"`
	// Where is repo or other: the git repository the session was recorded in.
	Where string `yaml:"where"`
	// Parent makes the session a subagent session of that session.
	Parent string `yaml:"parent"`
	// Published publishes the session before the runs, through a plain push.
	Published bool `yaml:"published"`
	// Public makes its published transcript public on the Village double.
	Public bool `yaml:"public"`
	// Branch is the git branch the session recorded; main when empty.
	Branch string `yaml:"branch"`
}

type selectedPlanWant struct {
	ID               string `yaml:"id"`
	Verdict          string `yaml:"verdict"`
	Reason           string `yaml:"reason"`
	SelectedBy       string `yaml:"selectedBy"`
	SubagentsOmitted int    `yaml:"subagentsOmitted"`
	NextContains     string `yaml:"nextContains"`
	DetailContains   string `yaml:"detailContains"`
	// SelectionInheritedFrom is the exact selectionInheritedFrom the entry
	// must carry; empty asserts the key is absent.
	SelectionInheritedFrom string             `yaml:"selectionInheritedFrom"`
	DetailOmits            string             `yaml:"detailOmits"`
	ShareOutcomes          []string           `yaml:"shareOutcomes"`
	Subagents              []selectedPlanWant `yaml:"subagents"`
}

type selectedRun struct {
	Args  []string `yaml:"args"`
	Stdin string   `yaml:"stdin"`
	TTY   bool     `yaml:"tty"`

	WantExit              int                `yaml:"wantExit"`
	WantStdoutJSON        bool               `yaml:"wantStdoutJSON"`
	WantPlan              []selectedPlanWant `yaml:"wantPlan"`
	SamePlanAs            *int               `yaml:"samePlanAs"`
	WantNeedsConfirmation *bool              `yaml:"wantNeedsConfirmation"`
	WantPrompt            *bool              `yaml:"wantPrompt"`
	WantPublished         *[]string          `yaml:"wantPublished"`
	WantAudience          map[string]string  `yaml:"wantAudience"`
	WantRequests          *[]string          `yaml:"wantRequests"`
	WantRequestsOmit      []string           `yaml:"wantRequestsOmit"`
	WantNoWrites          bool               `yaml:"wantNoWrites"`
	WantRefusal           bool               `yaml:"wantRefusal"`
	WantErrContains       []string           `yaml:"wantErrContains"`
	WantOutputContains    []string           `yaml:"wantOutputContains"`
	WantOutputOmits       []string           `yaml:"wantOutputOmits"`
	// WantFileContains maps a file under the world directory to text it must
	// hold after the run; WantFileOmits to text it must not.
	WantFileContains map[string][]string `yaml:"wantFileContains"`
	WantFileOmits    map[string][]string `yaml:"wantFileOmits"`
	// WantProfileOutcome is the outcome of the push run span in the profile
	// the run wrote to profile.json under the world directory.
	WantProfileOutcome string   `yaml:"wantProfileOutcome"`
	WantKeys           []string `yaml:"wantKeys"`
	Golden             string   `yaml:"golden"`
}

type selectedCase struct {
	Name string `yaml:"name"`
	// Kind is run (the default) or wizard (asserts pushWizardEnabled directly).
	Kind         string         `yaml:"kind"`
	Seeds        []selectedSeed `yaml:"seeds"`
	SelectedOnly []string       `yaml:"selectedOnly"`
	// SelectedBranches makes the saved selection admit only these branches of
	// the repo clone.
	SelectedBranches []string `yaml:"selectedBranches"`
	PausedRule       bool     `yaml:"pausedRule"`
	FailShare        string   `yaml:"failShare"`
	// RefusePublish makes peasant online refuse the upload of these sessions
	// with 422.
	RefusePublish    []string      `yaml:"refusePublish"`
	ConfigVisibility string        `yaml:"configVisibility"`
	Runs             []selectedRun `yaml:"runs"`
}

func loadSelectedPushCases(t *testing.T) []selectedCase {
	t.Helper()
	var fixture struct {
		Cases []selectedCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(pushSelectedSessionsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode push_selected_sessions.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pushSelectedSessionsManifestYAML, "push selected sessions")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "push selected sessions"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// selectedWorld is one computer with a store, two git repositories, a config,
// and a Village double it is signed in to.
type selectedWorld struct {
	dir     string
	repo    string
	other   string
	cfgPath string
	village *testutil.CollectiveVillage
}

func newSelectedWorld(t *testing.T, tc selectedCase) *selectedWorld {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &selectedWorld{dir: t.TempDir(), repo: filepath.Join(root, "repo"), other: filepath.Join(root, "other")}
	for repo, remote := range map[string]string{w.repo: "https://github.com/acme/tools.git", w.other: "https://github.com/acme/notes.git"} {
		if err := os.MkdirAll(repo, 0o700); err != nil {
			t.Fatal(err)
		}
		hooksGit(t, repo, "", "init", "--quiet", "--initial-branch=main")
		hooksGit(t, repo, "", "remote", "add", "origin", remote)
	}
	w.village = testutil.NewCollectiveVillage(t, selectedPushCollectives...)
	for _, id := range tc.RefusePublish {
		w.village.RefusePublish(schema.SessionID(id))
	}
	if tc.FailShare != "" {
		w.village.FailShare(selectedCollectiveID(t, tc.FailShare))
	}
	writeTestCredentialsFor(t, w.dir, w.village.URL())

	visibility := tc.ConfigVisibility
	if visibility == "" {
		visibility = "private"
	}
	body := "version: 1\noutput:\n  basePath: " + filepath.Join(w.dir, "peasant-sync") + "\npush:\n  method: all\n  visibility: " + visibility + "\n"
	if len(tc.SelectedOnly) > 0 {
		body += "selection:\n  mode: selected\n  harnesses:\n    claude-code:\n      sessions: [" + strings.Join(tc.SelectedOnly, ", ") + "]\n"
	}
	if len(tc.SelectedBranches) > 0 {
		body += "selection:\n  mode: selected\n  harnesses:\n    claude-code:\n      projects:\n        - name: " + filepath.Base(w.repo) +
			"\n          clonePaths: [" + w.repo + "]\n          branches: [" + strings.Join(tc.SelectedBranches, ", ") + "]\n"
	}
	w.cfgPath = writeCfg(t, w.dir, "selected.yaml", body)

	for i, seed := range tc.Seeds {
		w.seed(t, seed, int64(i))
	}
	if tc.PausedRule {
		raw, err := yaml.Marshal(map[string]any{"version": 1, "autoPublish": []map[string]any{{
			"id": "paused", "kind": "folder", "match": autopublish.FolderMatch(w.repo), "events": []string{},
			"collectives": []string{string(selectedPushCollectives[0].ID)},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		path := autopublish.Path(defaults.ResolveConfigDirPathWith(w.dir))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var published []string
	for _, seed := range tc.Seeds {
		if seed.Published {
			published = append(published, "--session", seed.ID)
		}
	}
	if len(published) > 0 {
		args := append([]string{"--non-interactive", "--json", "--without-subagents"}, published...)
		if context, out := w.push(t, args, "", false); out.err != nil {
			t.Fatalf("publish the seeded sessions: %v\n%s", out.err, context)
		}
		for _, seed := range tc.Seeds {
			if seed.Public {
				w.village.SetPublic(schema.TranscriptID(seed.ID), true)
			}
		}
	}
	return w
}

func selectedCollectiveID(t *testing.T, name string) schema.VillageUUID {
	t.Helper()
	for _, c := range selectedPushCollectives {
		if c.Name == name {
			return c.ID
		}
	}
	t.Fatalf("no collective named %q", name)
	return ""
}

func (w *selectedWorld) seed(t *testing.T, seed selectedSeed, order int64) {
	t.Helper()
	dbPath := string(defaults.ResolveDBFilePathWith(w.dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := openPreparedStore(t, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := w.repo
	if seed.Where == "other" {
		dir = w.other
	}
	hash, _, err := ingest.DeriveProjectIdentifiersWithGit(t.Context(), db.InstallationSalt(), &ingest.ExecGitResolver{}, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	remote, _ := (&ingest.ExecGitResolver{}).RemoteURL(t.Context(), dir)
	branch := seed.Branch
	if branch == "" {
		branch = "main"
	}
	entry := makeCmdStoreEntry(t, seed.ID, "github.com-acme-tools", remote, branch, 1700000000000+order*60000, dir)
	entry.Metadata.Project.Hash = hash
	if seed.Parent != "" {
		parent := schema.SessionID(seed.Parent)
		entry.Metadata.ParentUUID = &parent
	}
	switch seed.Kind {
	case "ready", "latest_attempt":
		testutil.SeedReadyPublication(t, db, entry.Metadata, nil)
	case "no_model_reply", "model_gap":
		// A verified capture whose source names no model: with only a user
		// entry the session never reached a model; with an assistant reply
		// the model is an ingest gap.
		entry.Metadata.Model = ""
		role := schema.RoleUser
		if seed.Kind == "model_gap" {
			role = schema.RoleAssistant
		}
		text := "recorded text"
		testutil.SeedReadyPublication(t, db, entry.Metadata, []schema.SessionEntry{{
			SessionID: schema.SessionID(seed.ID), Harness: schema.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: role, ContentPreview: &text,
		}})
	case "needs_ingest":
		if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown seed kind %q", seed.Kind)
	}
	db.Close()
	if seed.Kind == "latest_attempt" {
		seedEntryCarrying(t, w.dir, seed.ID, "the last-good authority body")
		seedLatestAttemptPreview(t, w.dir, seed.ID, "the newer incomplete attempt body")
	}
}

// runOutcome is what one push run printed and returned.
type runOutcome struct {
	stdout, stderr string
	err            error
}

func (w *selectedWorld) push(t *testing.T, args []string, stdin string, tty bool) (string, runOutcome) {
	t.Helper()
	root := newTestRoot()
	root.AddCommand(buildPushCommand(pushCommandDependencies{isTTY: func() bool { return tty }}))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(stdin))
	expanded := make([]string, 0, len(args))
	for _, arg := range args {
		arg = strings.ReplaceAll(arg, "{dir}", w.dir)
		expanded = append(expanded, strings.ReplaceAll(arg, "{repo}", w.repo))
	}
	root.SetArgs(append([]string{"--data-dir", w.dir, "--config-dir", w.dir, "--state-dir", w.dir, "push", "--config", w.cfgPath}, expanded...))
	err := root.Execute()
	out := runOutcome{stdout: stdout.String(), stderr: stderr.String(), err: err}
	return "stdout:\n" + out.stdout + "\nstderr:\n" + out.stderr, out
}

// TestPushCmd_SelectedSessions drives village push with --session,
// --sessions-from and --collective through the production command, against a
// real store and a Village double, and checks the per-session plan, the
// requests made, and what was published and shared.
func TestPushCmd_SelectedSessions(t *testing.T) {
	t.Parallel()
	for _, tc := range loadSelectedPushCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			if tc.Kind == "wizard" {
				if !pushWizardEnabled(false, false, true, false, false, false) {
					t.Fatal("control: a TTY push that names no session opens the wizard")
				}
				if pushWizardEnabled(false, false, true, false, false, true) {
					t.Fatal("a TTY push that names its sessions must never open the wizard")
				}
			}
			if len(tc.Runs) == 0 {
				return
			}
			w := newSelectedWorld(t, tc)
			var plans [][]any
			for i, run := range tc.Runs {
				before := len(w.village.Publishes())
				requestsBefore := len(w.village.Requests())
				context, out := w.push(t, run.Args, run.Stdin, run.TTY)
				context = "run " + string(rune('0'+i)) + " " + strings.Join(run.Args, " ") + "\n" + context
				exit := defaults.ExitOK
				if out.err != nil {
					exit = exitCodeFor(out.err)
				}
				if exit.Int() != run.WantExit {
					t.Fatalf("exit = %d, want %d; err: %v\n%s", exit, run.WantExit, out.err, context)
				}
				for _, want := range run.WantErrContains {
					if out.err == nil || !strings.Contains(out.err.Error(), want) {
						t.Errorf("error must contain %q; got %v\n%s", want, out.err, context)
					}
				}
				output := out.stdout + out.stderr
				for _, want := range run.WantOutputContains {
					if !strings.Contains(output, want) {
						t.Errorf("output misses %q\n%s", want, context)
					}
				}
				for _, forbidden := range run.WantOutputOmits {
					if strings.Contains(output, forbidden) {
						t.Errorf("output carries %q\n%s", forbidden, context)
					}
				}
				for name, wants := range run.WantFileContains {
					raw, err := os.ReadFile(filepath.Join(w.dir, name))
					if err != nil {
						t.Fatalf("read %s: %v\n%s", name, err, context)
					}
					for _, want := range wants {
						if !strings.Contains(string(raw), want) {
							t.Errorf("%s misses %q\n%s", name, want, raw)
						}
					}
				}
				for name, forbidden := range run.WantFileOmits {
					raw, err := os.ReadFile(filepath.Join(w.dir, name))
					if err != nil {
						t.Fatalf("read %s: %v\n%s", name, err, context)
					}
					for _, text := range forbidden {
						if strings.Contains(string(raw), text) {
							t.Errorf("%s carries %q\n%s", name, text, raw)
						}
					}
				}
				if run.WantProfileOutcome != "" {
					raw, err := os.ReadFile(filepath.Join(w.dir, "profile.json"))
					if err != nil {
						t.Fatalf("read profile: %v\n%s", err, context)
					}
					var profile perf.ProfileDocument
					if err := json.Unmarshal(raw, &profile); err != nil {
						t.Fatalf("decode profile: %v", err)
					}
					outcome := ""
					for _, span := range profile.Spans {
						if span.Stage == perf.StagePushRun {
							outcome = string(span.Outcome)
						}
					}
					if outcome != run.WantProfileOutcome {
						t.Errorf("profile run outcome = %q, want %q\n%s", outcome, run.WantProfileOutcome, context)
					}
				}
				if run.WantPrompt != nil {
					shown := strings.Contains(out.stderr, "This skips your auto-publish rules for these sessions. [y/N]")
					if shown != *run.WantPrompt {
						t.Errorf("confirmation prompt shown = %v, want %v\n%s", shown, *run.WantPrompt, context)
					}
				}

				var doc map[string]any
				if run.WantStdoutJSON {
					decoder := json.NewDecoder(strings.NewReader(out.stdout))
					if err := decoder.Decode(&doc); err != nil {
						t.Fatalf("stdout is not one JSON document: %v\n%s", err, context)
					}
					if decoder.More() {
						t.Fatalf("stdout holds more than one JSON document\n%s", context)
					}
				}
				refusal, _ := doc["refusal"].(string)
				if run.WantRefusal != (refusal != "") {
					t.Errorf("refusal = %q, want present %v\n%s", refusal, run.WantRefusal, context)
				}
				for _, key := range run.WantKeys {
					if _, ok := doc[key]; !ok {
						t.Errorf("document misses key %q\n%s", key, context)
					}
				}
				plan, _ := doc["plan"].([]any)
				plans = append(plans, plan)
				if run.WantPlan != nil {
					assertSelectedPlan(t, "plan", plan, run.WantPlan, context)
				}
				if run.SamePlanAs != nil && !reflect.DeepEqual(plans[*run.SamePlanAs], plan) {
					t.Errorf("plan differs from run %d's\n%s", *run.SamePlanAs, context)
				}
				if run.WantNeedsConfirmation != nil {
					got, ok := doc["needsConfirmation"].(bool)
					if !ok || got != *run.WantNeedsConfirmation {
						t.Errorf("needsConfirmation = %v (present %v), want %v\n%s", got, ok, *run.WantNeedsConfirmation, context)
					}
				}
				if run.Golden != "" && out.stdout != run.Golden {
					t.Errorf("stdout differs from the golden document\ngot:\n%s\nwant:\n%s", out.stdout, run.Golden)
				}

				if run.WantPublished != nil {
					var got []string
					for _, request := range w.village.Publishes()[before:] {
						got = append(got, string(request.Identity.SessionID))
					}
					slices.Sort(got)
					want := slices.Sorted(slices.Values(*run.WantPublished))
					if !slices.Equal(got, want) {
						t.Errorf("published = %v, want %v\n%s", got, want, context)
					}
				}
				for id, want := range run.WantAudience {
					got := []string{}
					for collective, status := range w.village.Audience(schema.TranscriptID(id)) {
						for _, c := range selectedPushCollectives {
							if c.ID == collective {
								got = append(got, c.Name+":"+string(status))
							}
						}
					}
					slices.Sort(got)
					if strings.Join(got, ",") != want {
						t.Errorf("audience of %s = %q, want %q\n%s", id, strings.Join(got, ","), want, context)
					}
				}
				requests := w.village.Requests()[requestsBefore:]
				if run.WantRequests != nil && !slices.Equal(requests, *run.WantRequests) {
					t.Errorf("requests = %v, want exactly %v\n%s", requests, *run.WantRequests, context)
				}
				for _, request := range requests {
					if slices.Contains(run.WantRequestsOmit, request) {
						t.Errorf("request %q must not be made\n%s", request, context)
					}
					if run.WantNoWrites && !strings.HasPrefix(request, "GET ") {
						t.Errorf("write request %q made\n%s", request, context)
					}
				}
			}
		})
	}
}

func assertSelectedPlan(t *testing.T, where string, got []any, want []selectedPlanWant, context string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s has %d entries, want %d: %v\n%s", where, len(got), len(want), got, context)
		return
	}
	for i, w := range want {
		entry, _ := got[i].(map[string]any)
		label := where + "[" + w.ID + "]"
		if entry["session_id"] != w.ID {
			t.Errorf("%s session_id = %v, want %s (request order)\n%s", label, entry["session_id"], w.ID, context)
			continue
		}
		if _, err := NewPlanVerdict(w.Verdict); err != nil {
			t.Fatalf("fixture verdict: %v", err)
		}
		if entry["verdict"] != w.Verdict {
			t.Errorf("%s verdict = %v, want %s (detail %v)\n%s", label, entry["verdict"], w.Verdict, entry["detail"], context)
		}
		reason, _ := entry["reason"].(string)
		if w.Reason != "" {
			if _, err := NewPlanReason(w.Reason); err != nil {
				t.Fatalf("fixture reason: %v", err)
			}
		}
		if reason != w.Reason {
			t.Errorf("%s reason = %q, want %q\n%s", label, reason, w.Reason, context)
		}
		selectedBy := w.SelectedBy
		if selectedBy == "" {
			selectedBy = planSelectedByRequest
		}
		if entry["selectedBy"] != selectedBy {
			t.Errorf("%s selectedBy = %v, want %s\n%s", label, entry["selectedBy"], selectedBy, context)
		}
		omitted, _ := entry["subagentsOmitted"].(float64)
		if int(omitted) != w.SubagentsOmitted {
			t.Errorf("%s subagentsOmitted = %v, want %d\n%s", label, omitted, w.SubagentsOmitted, context)
		}
		inheritedFrom, _ := entry["selectionInheritedFrom"].(string)
		if inheritedFrom != w.SelectionInheritedFrom {
			t.Errorf("%s selectionInheritedFrom = %q, want %q\n%s", label, inheritedFrom, w.SelectionInheritedFrom, context)
		}
		detail, _ := entry["detail"].(string)
		if w.DetailContains != "" && !strings.Contains(detail, w.DetailContains) {
			t.Errorf("%s detail = %q, want it to contain %q\n%s", label, detail, w.DetailContains, context)
		}
		if w.DetailOmits != "" && strings.Contains(detail, w.DetailOmits) {
			t.Errorf("%s detail = %q, want it not to contain %q\n%s", label, detail, w.DetailOmits, context)
		}
		next, _ := entry["next"].(string)
		if w.NextContains != "" && !strings.Contains(next, w.NextContains) {
			t.Errorf("%s next = %q, want it to contain %q\n%s", label, next, w.NextContains, context)
		}
		if w.ShareOutcomes != nil {
			shares, _ := entry["shares"].([]any)
			var outcomes []string
			for _, s := range shares {
				share, _ := s.(map[string]any)
				outcome, _ := share["outcome"].(string)
				if outcome == "" {
					outcome, _ = share["would"].(string)
				}
				outcomes = append(outcomes, outcome)
			}
			if !slices.Equal(outcomes, w.ShareOutcomes) {
				t.Errorf("%s share outcomes = %v, want %v\n%s", label, outcomes, w.ShareOutcomes, context)
			}
		}
		subagents, _ := entry["subagents"].([]any)
		assertSelectedPlan(t, label+".subagents", subagents, w.Subagents, context)
	}
}

// TestPushPlanClosedSets pins the exact membership of the plan's verdicts and
// reasons, and that each constructor refuses a value outside its set.
func TestPushPlanClosedSets(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Verdicts []string `yaml:"verdicts"`
		Reasons  []string `yaml:"reasons"`
		Rejected []string `yaml:"rejected"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(pushPlanClosedSetsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode push_plan_closed_sets.yaml: %v", err)
	}
	var verdicts []string
	for _, v := range AllPlanVerdicts {
		verdicts = append(verdicts, string(v))
		if _, err := NewPlanVerdict(string(v)); err != nil {
			t.Errorf("NewPlanVerdict(%q): %v", v, err)
		}
	}
	if !slices.Equal(slices.Sorted(slices.Values(verdicts)), slices.Sorted(slices.Values(fixture.Verdicts))) {
		t.Errorf("AllPlanVerdicts = %v, want exactly %v", verdicts, fixture.Verdicts)
	}
	var reasons []string
	for _, r := range AllPlanReasons {
		reasons = append(reasons, string(r))
		if _, err := NewPlanReason(string(r)); err != nil {
			t.Errorf("NewPlanReason(%q): %v", r, err)
		}
	}
	if !slices.Equal(slices.Sorted(slices.Values(reasons)), slices.Sorted(slices.Values(fixture.Reasons))) {
		t.Errorf("AllPlanReasons = %v, want exactly %v", reasons, fixture.Reasons)
	}
	for _, raw := range fixture.Rejected {
		if _, err := NewPlanVerdict(raw); err == nil {
			t.Errorf("NewPlanVerdict(%q) must fail", raw)
		}
		if _, err := NewPlanReason(raw); err == nil {
			t.Errorf("NewPlanReason(%q) must fail", raw)
		}
	}
}

// The store is the child-session reader the push expands subagents through.
var _ childSessionReader = (*store.Store)(nil)
