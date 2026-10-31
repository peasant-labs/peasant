package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/village_share.yaml
var villageShareYAML []byte

//go:embed testdata/village_share.manifest.yaml
var villageShareManifestYAML []byte

// The fixture names ids by placeholder.
var villageSharePlaceholders = map[string]string{
	"{a}":           "11111111-0000-4000-8000-00000000000a",
	"{b}":           "11111111-0000-4000-8000-00000000000b",
	"{unpublished}": "11111111-0000-4000-8000-00000000000c",
	"{stranger}":    "11111111-0000-4000-8000-00000000000d",
	"{outsider}":    "aaaaaaaa-0000-4000-8000-000000000009",
}

type villageShareStep struct {
	Transcript     string                     `yaml:"transcript"`
	Collective     string                     `yaml:"collective"`
	Outcome        schema.SyncPushStepOutcome `yaml:"outcome"`
	Session        string                     `yaml:"session"`
	ReasonContains string                     `yaml:"reasonContains"`
}

type villageShareCase struct {
	Name   string `yaml:"name"`
	Shares []struct {
		Transcript string                    `yaml:"transcript"`
		Collective string                    `yaml:"collective"`
		Status     schema.VillageShareStatus `yaml:"status"`
	} `yaml:"shares"`
	RefuseShare   []string `yaml:"refuseShare"`
	ConflictShare string   `yaml:"conflictShare"`
	LoggedOut     bool     `yaml:"loggedOut"`
	BrokenRules   bool     `yaml:"brokenRules"`
	TTY           bool     `yaml:"tty"`
	Stdin         string   `yaml:"stdin"`
	Args          []string `yaml:"args"`
	AlsoArgs      []string `yaml:"alsoArgs"`
	// Text leaves out the --json every other case adds.
	Text bool `yaml:"text"`

	WantExit       int                `yaml:"wantExit"`
	WantPosts      int                `yaml:"wantPosts"`
	WantNoRequests bool               `yaml:"wantNoRequests"`
	WantSteps      []villageShareStep `yaml:"wantSteps"`
	WantErrors     []struct {
		Ref      string `yaml:"ref"`
		Contains string `yaml:"contains"`
	} `yaml:"wantErrors"`
	WantErr               string `yaml:"wantErr"`
	WantRefusal           bool   `yaml:"wantRefusal"`
	WantPrompt            string `yaml:"wantPrompt"`
	WantNoPrompt          bool   `yaml:"wantNoPrompt"`
	WantOutput            string `yaml:"wantOutput"`
	WantNeedsConfirmation *bool  `yaml:"wantNeedsConfirmation"`
	WantReceipts          int    `yaml:"wantReceipts"`
	WantStoreUnchanged    bool   `yaml:"wantStoreUnchanged"`
	Golden                string `yaml:"golden"`
}

func loadVillageShareCases(t *testing.T) []villageShareCase {
	t.Helper()
	var fixture struct {
		Cases []villageShareCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(villageShareYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(villageShareManifestYAML, "village share")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "village share"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

func expandVillageShare(value string) string {
	for placeholder, id := range villageSharePlaceholders {
		value = strings.ReplaceAll(value, placeholder, id)
	}
	return value
}

// shareRun runs the command once in the world.
func shareRun(t *testing.T, w *selectedWorld, tc villageShareCase, args []string, configDir string) runOutcome {
	t.Helper()
	root := newTestRoot()
	root.AddCommand(buildVillageShareCommand(pushCommandDependencies{isTTY: func() bool { return tc.TTY }}))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(expandVillageShare(tc.Stdin)))
	full := []string{"--data-dir", w.dir, "--config-dir", configDir, "--state-dir", w.dir, "share"}
	for _, arg := range args {
		full = append(full, expandVillageShare(arg))
	}
	if !tc.Text {
		full = append(full, "--json")
	}
	root.SetArgs(full)
	err := root.Execute()
	return runOutcome{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// TestVillageShare drives the production command and checks what it asked, what
// it sent, and what it reported. Every mutating request is counted, so a case
// that states no wantPosts proves the run changed nothing on Village.
func TestVillageShare(t *testing.T) {
	t.Parallel()
	for _, tc := range loadVillageShareCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			w := newSelectedWorld(t, selectedCase{Name: tc.Name, Seeds: []selectedSeed{
				{ID: villageSharePlaceholders["{a}"], Kind: "ready", Where: "repo", Published: true},
				{ID: villageSharePlaceholders["{b}"], Kind: "ready", Where: "repo", Published: true},
				{ID: villageSharePlaceholders["{unpublished}"], Kind: "ready", Where: "repo"},
			}})
			for _, share := range tc.Shares {
				w.village.Share(schema.TranscriptID(expandVillageShare(share.Transcript)), selectedCollectiveID(t, share.Collective), share.Status)
			}
			for _, transcript := range tc.RefuseShare {
				w.village.RefuseShare(schema.TranscriptID(expandVillageShare(transcript)))
			}
			if tc.ConflictShare != "" {
				w.village.ConflictShare(selectedCollectiveID(t, tc.ConflictShare))
			}
			configDir := w.dir
			if tc.LoggedOut {
				configDir = t.TempDir()
			}
			if tc.BrokenRules {
				path := autopublish.Path(defaults.ResolveConfigDirPathWith(w.dir))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{ not rules"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := len(w.village.Requests())
			storeBefore := storeBytes(t, w)
			if len(tc.AlsoArgs) > 0 {
				if out := shareRun(t, w, tc, tc.Args, configDir); out.err != nil {
					t.Fatalf("first run: %v\n%s%s", out.err, out.stdout, out.stderr)
				} else if strings.Contains(out.stderr, "[y/N]") {
					t.Fatalf("first run asked: %s", out.stderr)
				}
			}
			args := tc.Args
			if len(tc.AlsoArgs) > 0 {
				args = tc.AlsoArgs
			}
			out := shareRun(t, w, tc, args, configDir)
			context := "stdout:\n" + out.stdout + "\nstderr:\n" + out.stderr
			exit := defaults.ExitOK
			if out.err != nil {
				exit = exitCodeFor(out.err)
			}
			if exit.Int() != tc.WantExit {
				t.Fatalf("exit = %d, want %d; err: %v\n%s", exit, tc.WantExit, out.err, context)
			}
			if tc.WantErr != "" && (out.err == nil || !strings.Contains(out.err.Error(), tc.WantErr)) {
				t.Fatalf("error = %v, want it to contain %q", out.err, tc.WantErr)
			}
			requests := w.village.Requests()[before:]
			assertVillageShareRequests(t, tc, requests)
			if tc.WantPrompt != "" && !strings.Contains(out.stderr, tc.WantPrompt) {
				t.Fatalf("prompt missing %q\n%s", tc.WantPrompt, context)
			}
			if tc.WantNoPrompt && strings.Contains(out.stderr, "[y/N]") {
				t.Fatalf("the run asked\n%s", context)
			}
			if tc.WantOutput != "" && !strings.Contains(out.stdout+out.stderr, tc.WantOutput) {
				t.Fatalf("output misses %q\n%s", tc.WantOutput, context)
			}
			if tc.WantReceipts != 0 {
				assertShareReceipts(t, w, tc.WantReceipts)
			}
			if tc.WantStoreUnchanged {
				for name, raw := range storeBytes(t, w) {
					if !bytes.Equal(storeBefore[name], raw) {
						t.Fatalf("a dry run wrote to %s", name)
					}
				}
			}
			if !tc.Text {
				assertVillageShareDocument(t, tc, out.stdout, w.village.URL())
			}
		})
	}
}

func assertVillageShareRequests(t *testing.T, tc villageShareCase, requests []string) {
	t.Helper()
	if tc.WantNoRequests && len(requests) != 0 {
		t.Fatalf("a refused run made requests: %v", requests)
	}
	posts := 0
	for _, request := range requests {
		if strings.HasPrefix(request, "GET ") {
			continue
		}
		if !strings.HasPrefix(request, "POST /api/v1/transcripts/") || !strings.HasSuffix(request, "/share") {
			t.Fatalf("the only change this command may send is the share route, got %q", request)
		}
		posts++
	}
	if posts != tc.WantPosts {
		t.Fatalf("share requests = %d, want %d: %v", posts, tc.WantPosts, requests)
	}
}

// assertShareReceipts reads the receipts the collective observation store holds
// for the signed-in account.
func assertShareReceipts(t *testing.T, w *selectedWorld, want int) {
	t.Helper()
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(w.dir)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := collectiveobservations.New(collectiveobservations.Dependencies{Store: db, ConfigHome: w.dir})
	history, err := service.History(t.Context(), schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceReceipts, Page: 1, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	shares := 0
	for _, receipt := range history.Receipts {
		if receipt.Operation == schema.CollectiveOperationShare {
			shares++
		}
	}
	if shares != want {
		t.Fatalf("share receipts = %d, want %d", shares, want)
	}
}

// storeBytes reads the store's files, so a dry run can prove it wrote none.
func storeBytes(t *testing.T, w *selectedWorld) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	path := string(defaults.ResolveDBFilePathWith(w.dir))
	for _, name := range []string{path, path + "-wal"} {
		raw, err := os.ReadFile(name)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		files[name] = raw
	}
	return files
}

func assertVillageShareDocument(t *testing.T, tc villageShareCase, stdout, villageURL string) {
	t.Helper()
	var doc shareDocument
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if doc.Warnings == nil || doc.Transcripts == nil || doc.Errors == nil || doc.Collectives == nil {
		t.Fatalf("an array is null: %s", stdout)
	}
	if tc.WantRefusal && doc.Refusal == "" {
		t.Fatalf("the document names no refusal: %s", stdout)
	}
	if tc.WantNeedsConfirmation != nil {
		if doc.NeedsConfirmation == nil || *doc.NeedsConfirmation != *tc.WantNeedsConfirmation {
			t.Fatalf("needsConfirmation = %v, want %v", doc.NeedsConfirmation, *tc.WantNeedsConfirmation)
		}
	}
	got := map[string]schema.SyncPushStepResult{}
	sessions := map[string]string{}
	for _, entry := range doc.Transcripts {
		for _, step := range entry.Steps {
			got[entry.TranscriptID.String()+"/"+step.CollectiveID.String()] = step
			sessions[entry.TranscriptID.String()] = entry.SessionID
		}
	}
	for _, want := range tc.WantSteps {
		transcript := expandVillageShare(want.Transcript)
		step, ok := got[transcript+"/"+selectedCollectiveID(t, want.Collective).String()]
		if !ok {
			t.Fatalf("no step for %s/%s: %s", want.Transcript, want.Collective, stdout)
		}
		if step.Outcome != want.Outcome || !strings.Contains(step.Reason, want.ReasonContains) {
			t.Fatalf("step = %+v, want %+v", step, want)
		}
		if want.Session != "" && sessions[transcript] != expandVillageShare(want.Session) {
			t.Fatalf("session = %q, want %q", sessions[transcript], want.Session)
		}
	}
	for i, want := range tc.WantErrors {
		if i >= len(doc.Errors) || doc.Errors[i].Ref != expandVillageShare(want.Ref) || !strings.Contains(doc.Errors[i].Message, expandVillageShare(want.Contains)) {
			t.Fatalf("errors = %+v, want %+v", doc.Errors, want)
		}
	}
	if tc.Golden != "" {
		raw, err := os.ReadFile(tc.Golden)
		if err != nil {
			t.Fatal(err)
		}
		var want, have any
		if err := json.Unmarshal([]byte(strings.ReplaceAll(expandVillageShare(string(raw)), "{village}", villageURL)), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(stdout), &have); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(have, want) {
			t.Fatalf("document differs from %s:\n%s", tc.Golden, stdout)
		}
	}
}
