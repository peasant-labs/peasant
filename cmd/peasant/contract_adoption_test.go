package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/cli/contract_adoption.yaml
var contractAdoptionYAML []byte

//go:embed testdata/cli/contract_adoption.manifest.yaml
var contractAdoptionManifestYAML []byte

type contractAdoptionWarning struct {
	Code      string `yaml:"code"`
	SessionID string `yaml:"sessionID"`
}

type contractAdoptionHelpCase struct {
	Command   []string `yaml:"command"`
	HelpLists []string `yaml:"helpLists"`
	HelpOmits []string `yaml:"helpOmits"`
}

type contractAdoptionCase struct {
	Name               string                     `yaml:"name"`
	Kind               string                     `yaml:"kind"`
	Seed               string                     `yaml:"seed"`
	SeedExport         []string                   `yaml:"seedExport"`
	Credentials        bool                       `yaml:"credentials"`
	IDsFile            []string                   `yaml:"idsFile"`
	Args               []string                   `yaml:"args"`
	Stdin              string                     `yaml:"stdin"`
	WantExit           int                        `yaml:"wantExit"`
	WantStdoutJSON     bool                       `yaml:"wantStdoutJSON"`
	WantStderrNonEmpty bool                       `yaml:"wantStderrNonEmpty"`
	WantExported       []string                   `yaml:"wantExported"`
	WantDocSessions    []string                   `yaml:"wantDocSessions"`
	WantWarnings       []contractAdoptionWarning  `yaml:"wantWarnings"`
	WantStderrContains []string                   `yaml:"wantStderrContains"`
	WantStdoutOmits    []string                   `yaml:"wantStdoutOmits"`
	WantSyncUnchanged  bool                       `yaml:"wantSyncUnchanged"`
	WantGolden         string                     `yaml:"wantGolden"`
	WantDocKeys        []string                   `yaml:"wantDocKeys"`
	HelpCases          []contractAdoptionHelpCase `yaml:"helpCases"`
	YesCommands        [][]string                 `yaml:"yesCommands"`
}

func loadContractAdoptionCases(t *testing.T) []contractAdoptionCase {
	t.Helper()
	var fixture struct {
		Cases []contractAdoptionCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(contractAdoptionYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode contract_adoption.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(contractAdoptionManifestYAML, "contract adoption")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "contract adoption"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// TestCLIContractAdoption drives every adopting command through its production
// builder and asserts the shared contract: --sessions-from reads a file or
// stdin, stdout under --json is one document carrying warnings, a dry run never
// prompts or writes, and every --yes has -y.
func TestCLIContractAdoption(t *testing.T) {
	t.Parallel()
	for _, tc := range loadContractAdoptionCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			switch tc.Kind {
			case "run":
				runContractAdoptionCase(t, tc)
			case "help":
				assertContractHelp(t, tc)
			case "yes_shorthand":
				assertYesShorthand(t, tc)
			default:
				t.Fatalf("unknown kind %q", tc.Kind)
			}
		})
	}
}

func contractAdoptionBuilder(t *testing.T, name string) *cobra.Command {
	t.Helper()
	switch name {
	case "export":
		return BuildExportCommand()
	case "redact":
		return BuildRedactCommand()
	case "push":
		return BuildPushCommand()
	default:
		t.Fatalf("no builder for command %q", name)
		return nil
	}
}

func runContractAdoptionCase(t *testing.T, tc contractAdoptionCase) {
	t.Helper()
	dir := t.TempDir()
	outDir := filepath.Join(dir, "exported")
	idsPath := filepath.Join(dir, "ids.txt")
	switch tc.Seed {
	case "":
	case "export":
		for _, id := range tc.SeedExport {
			seedUploadableSession(t, dir, id)
		}
	case "redact":
		seedRedactTestSession(t, dir)
	default:
		t.Fatalf("unknown seed %q", tc.Seed)
	}
	if tc.Credentials {
		writeTestCredentials(t, dir)
	}
	if len(tc.IDsFile) > 0 {
		if err := os.WriteFile(idsPath, []byte(strings.Join(tc.IDsFile, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	syncDir := resolveOutputSyncDirFor(dir)
	before := treeDigest(t, syncDir)

	args := make([]string, len(tc.Args))
	for i, arg := range tc.Args {
		arg = strings.ReplaceAll(arg, "{out}", outDir)
		args[i] = strings.ReplaceAll(arg, "{ids}", idsPath)
	}
	root := newTestRoot()
	root.AddCommand(contractAdoptionBuilder(t, args[0]))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(tc.Stdin))
	root.SetArgs(append([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir}, args...))
	err := root.Execute()
	exit := defaults.ExitOK
	if err != nil {
		exit = exitCodeFor(err)
	}
	if exit.Int() != tc.WantExit {
		t.Fatalf("run %v: exit %d, want %d; err: %v\nstdout: %s\nstderr: %s", args, exit, tc.WantExit, err, stdout.String(), stderr.String())
	}

	var doc map[string]any
	if tc.WantStdoutJSON {
		decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
		if err := decoder.Decode(&doc); err != nil {
			t.Fatalf("stdout is not a JSON document: %v\nstdout: %s", err, stdout.String())
		}
		if decoder.More() {
			t.Fatalf("stdout holds more than one JSON document: %s", stdout.String())
		}
	}
	if tc.WantWarnings != nil {
		warnings, ok := doc["warnings"].([]any)
		if !ok {
			t.Fatalf("document has no warnings array (null or missing): %s", stdout.String())
		}
		if len(warnings) != len(tc.WantWarnings) {
			t.Fatalf("warnings = %v, want %d", warnings, len(tc.WantWarnings))
		}
		for i, want := range tc.WantWarnings {
			got, _ := warnings[i].(map[string]any)
			if _, err := clicontract.NewWarningCode(want.Code); err != nil {
				t.Fatalf("fixture warning code: %v", err)
			}
			if got["code"] != want.Code || got["session_id"] != want.SessionID {
				t.Errorf("warnings[%d] = %v, want code %s session %s", i, got, want.Code, want.SessionID)
			}
		}
	}
	if tc.WantStderrNonEmpty && stderr.Len() == 0 {
		t.Error("the case needs stderr to carry text beside the document, and stderr is empty")
	}
	for _, want := range tc.WantStderrContains {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr misses %q: %s", want, stderr.String())
		}
	}
	for _, forbidden := range tc.WantStdoutOmits {
		if strings.Contains(stdout.String()+stderr.String(), forbidden) {
			t.Errorf("output carries %q: %s%s", forbidden, stdout.String(), stderr.String())
		}
	}
	if tc.WantExported != nil {
		var got []string
		matches, err := filepath.Glob(filepath.Join(outDir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range matches {
			got = append(got, strings.TrimSuffix(filepath.Base(match), ".json"))
		}
		slices.Sort(got)
		want := slices.Sorted(slices.Values(tc.WantExported))
		if !slices.Equal(got, want) {
			t.Errorf("exported files = %v, want exactly %v", got, want)
		}
	}
	if tc.WantDocSessions != nil {
		sessions, _ := doc["sessions"].([]any)
		var got []string
		for _, s := range sessions {
			object, _ := s.(map[string]any)
			id, _ := object["session_id"].(string)
			got = append(got, id)
		}
		if !slices.Equal(got, tc.WantDocSessions) {
			t.Errorf("document sessions = %v, want %v", got, tc.WantDocSessions)
		}
	}
	for _, key := range tc.WantDocKeys {
		if _, ok := doc[key]; !ok {
			t.Errorf("document misses %q: %s", key, stdout.String())
		}
	}
	if tc.WantGolden != "" && stdout.String() != tc.WantGolden {
		t.Errorf("stdout differs from the golden document\ngot:\n%s\nwant:\n%s", stdout.String(), tc.WantGolden)
	}
	if tc.WantSyncUnchanged {
		if after := treeDigest(t, syncDir); after != before {
			t.Error("a dry run changed the stored transcripts")
		}
	}
}

// resolveOutputSyncDirFor is the peasant-sync directory the commands resolve
// for --data-dir dir.
func resolveOutputSyncDirFor(dir string) string {
	cmd := newTestRoot()
	if err := cmd.PersistentFlags().Set("data-dir", dir); err != nil {
		panic(err)
	}
	return resolveOutputSyncDir(cmd)
}

// treeDigest hashes every file path and its bytes under root, so a test can
// prove a run wrote nothing there.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		h.Write([]byte(path))
		h.Write(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(h.Sum(nil))
}

func assertContractHelp(t *testing.T, tc contractAdoptionCase) {
	t.Helper()
	if len(tc.HelpCases) == 0 {
		t.Fatal("help case names no command")
	}
	root := buildRootCommand()
	for _, hc := range tc.HelpCases {
		cmd, _, err := root.Find(hc.Command)
		if err != nil || cmd.Name() != hc.Command[len(hc.Command)-1] {
			t.Fatalf("resolve %v: %v", hc.Command, err)
		}
		usage := cmd.UsageString()
		for _, flag := range hc.HelpLists {
			if !strings.Contains(usage, "--"+flag+" ") {
				t.Errorf("%v --help does not list --%s:\n%s", hc.Command, flag, usage)
			}
		}
		for _, flag := range hc.HelpOmits {
			if strings.Contains(usage, "--"+flag) {
				t.Errorf("%v --help lists the hidden --%s:\n%s", hc.Command, flag, usage)
			}
		}
	}
}

func assertYesShorthand(t *testing.T, tc contractAdoptionCase) {
	t.Helper()
	root := buildRootCommand()
	found := map[string]bool{}
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if flag := cmd.Flags().Lookup(clicontract.YesFlag); flag != nil {
			found[cmd.CommandPath()] = true
			if flag.Shorthand != clicontract.YesShorthand {
				t.Errorf("%s defines --yes without the -y shorthand", cmd.CommandPath())
			}
			if resolved := cmd.Flags().ShorthandLookup(clicontract.YesShorthand); resolved != flag {
				t.Errorf("%s: -y does not resolve to --yes", cmd.CommandPath())
			}
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(root)
	for _, path := range tc.YesCommands {
		want := root.Name() + " " + strings.Join(path, " ")
		if !found[want] {
			t.Errorf("%s was expected to define --yes; found %v", want, found)
		}
	}
}
