package clicontract_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/read_session_ids.yaml
var readSessionIDsYAML []byte

//go:embed testdata/read_session_ids.manifest.yaml
var readSessionIDsManifestYAML []byte

//go:embed testdata/prompt_policy.yaml
var promptPolicyYAML []byte

//go:embed testdata/prompt_policy.manifest.yaml
var promptPolicyManifestYAML []byte

//go:embed testdata/closed_sets.yaml
var closedSetsYAML []byte

type readSessionIDsCase struct {
	Name            string   `yaml:"name"`
	Input           string   `yaml:"input"`
	WantIDs         []string `yaml:"wantIDs"`
	WantErrContains string   `yaml:"wantErrContains"`
	WantNoIDs       bool     `yaml:"wantNoIDs"`
}

type promptPolicyCase struct {
	Name   string `yaml:"name"`
	Policy struct {
		DryRun    bool `yaml:"dryRun"`
		JSON      bool `yaml:"json"`
		AssumeYes bool `yaml:"assumeYes"`
		IsTTY     bool `yaml:"isTTY"`
	} `yaml:"policy"`
	Want string `yaml:"want"`
}

type closedSetsFixture struct {
	WarningCodes     []string `yaml:"warningCodes"`
	EmptyCodes       []string `yaml:"emptyCodes"`
	ConfirmDecisions []string `yaml:"confirmDecisions"`
	Rejected         []string `yaml:"rejected"`
}

// decodeStrict decodes one YAML document and refuses unknown fields.
func decodeStrict(t *testing.T, data []byte, out any) {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
}

func requireNames(t *testing.T, manifestYAML []byte, names []string, label string) {
	t.Helper()
	manifest, err := testutil.DecodeRequiredNamesManifest(manifestYAML, label)
	if err != nil {
		t.Fatal(err)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, label); err != nil {
		t.Fatal(err)
	}
}

func loadReadSessionIDsCases(t *testing.T) []readSessionIDsCase {
	t.Helper()
	var fixture struct {
		Cases []readSessionIDsCase `yaml:"cases"`
	}
	decodeStrict(t, readSessionIDsYAML, &fixture)
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	requireNames(t, readSessionIDsManifestYAML, names, "read session ids")
	return fixture.Cases
}

func loadPromptPolicyCases(t *testing.T) []promptPolicyCase {
	t.Helper()
	var fixture struct {
		Cases []promptPolicyCase `yaml:"cases"`
	}
	decodeStrict(t, promptPolicyYAML, &fixture)
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	requireNames(t, promptPolicyManifestYAML, names, "prompt policy")
	return fixture.Cases
}

func TestReadSessionIDs(t *testing.T) {
	t.Parallel()
	for _, tc := range loadReadSessionIDsCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			ids, err := clicontract.ReadSessionIDs(strings.NewReader(tc.Input))
			switch {
			case tc.WantNoIDs:
				if !errors.Is(err, clicontract.ErrNoSessionIDs) {
					t.Fatalf("want ErrNoSessionIDs, got ids=%v err=%v", ids, err)
				}
			case tc.WantErrContains != "":
				if err == nil {
					t.Fatalf("want an error naming %q, got ids %v", tc.WantErrContains, ids)
				}
				if !strings.Contains(err.Error(), tc.WantErrContains) {
					t.Fatalf("error %q does not name %q", err, tc.WantErrContains)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !slices.Equal(ids, tc.WantIDs) {
					t.Fatalf("ids = %v, want %v", ids, tc.WantIDs)
				}
			}
		})
	}
}

func TestPromptPolicyDecide(t *testing.T) {
	t.Parallel()
	for _, tc := range loadPromptPolicyCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			if !slices.Contains(clicontract.AllConfirmDecisions, clicontract.ConfirmDecision(tc.Want)) {
				t.Fatalf("fixture want %q is not a ConfirmDecision", tc.Want)
			}
			policy := clicontract.PromptPolicy{
				DryRun:    tc.Policy.DryRun,
				JSON:      tc.Policy.JSON,
				AssumeYes: tc.Policy.AssumeYes,
				IsTTY:     tc.Policy.IsTTY,
			}
			if got := policy.Decide(); string(got) != tc.Want {
				t.Fatalf("Decide(%+v) = %q, want %q", policy, got, tc.Want)
			}
		})
	}
}

func TestClosedSetsExactMembership(t *testing.T) {
	t.Parallel()
	var fixture closedSetsFixture
	decodeStrict(t, closedSetsYAML, &fixture)

	assertSameSet(t, "AllWarningCodes", stringsOf(clicontract.AllWarningCodes), fixture.WarningCodes)
	assertSameSet(t, "AllEmptyCodes", stringsOf(clicontract.AllEmptyCodes), fixture.EmptyCodes)
	assertSameSet(t, "AllConfirmDecisions", stringsOf(clicontract.AllConfirmDecisions), fixture.ConfirmDecisions)

	for _, raw := range fixture.WarningCodes {
		if _, err := clicontract.NewWarningCode(raw); err != nil {
			t.Errorf("NewWarningCode(%q): %v", raw, err)
		}
	}
	for _, raw := range fixture.EmptyCodes {
		if _, err := clicontract.NewEmptyCode(raw); err != nil {
			t.Errorf("NewEmptyCode(%q): %v", raw, err)
		}
	}
	for _, raw := range fixture.Rejected {
		if _, err := clicontract.NewWarningCode(raw); err == nil {
			t.Errorf("NewWarningCode(%q) accepted an unknown code", raw)
		}
		if _, err := clicontract.NewEmptyCode(raw); err == nil {
			t.Errorf("NewEmptyCode(%q) accepted an unknown code", raw)
		}
	}
}

func stringsOf[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

func assertSameSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	g := slices.Clone(got)
	w := slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	if !slices.Equal(g, w) {
		t.Errorf("%s = %v, want exactly %v", label, got, want)
	}
}

// TestMetaWarningsNeverNull proves a document with no warnings carries
// "warnings": [] and never null, through both constructors a command uses.
func TestMetaWarningsNeverNull(t *testing.T) {
	t.Parallel()
	for label, meta := range map[string]clicontract.Meta{
		"NewMeta":        clicontract.NewMeta(),
		"Collector.Meta": clicontract.NewCollector(nil, true).Meta("", ""),
	} {
		var buf bytes.Buffer
		if err := clicontract.WriteJSON(&buf, meta); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), `"warnings": []`) {
			t.Errorf("%s encodes warnings as %s, want []", label, buf.String())
		}
	}
}

// TestCollectorWritesBothPlaces proves a warning reaches stderr in both modes
// and the document only in --json mode, and that empty fields are omitted
// until set.
func TestCollectorWritesBothPlaces(t *testing.T) {
	t.Parallel()
	for _, jsonMode := range []bool{true, false} {
		var stderr bytes.Buffer
		c := clicontract.NewCollector(&stderr, jsonMode)
		c.Warn(clicontract.Warning{Code: clicontract.WarningSessionRefused, SessionID: "s-1", Message: "capture is incomplete"})
		c.Warn(clicontract.Warning{Code: clicontract.WarningFilterIgnored, Message: "--since ignored"})
		if want := "warning: session s-1: capture is incomplete\nwarning: --since ignored\n"; stderr.String() != want {
			t.Errorf("jsonMode=%v stderr = %q, want %q", jsonMode, stderr.String(), want)
		}
		if c.Count() != 2 {
			t.Errorf("jsonMode=%v Count = %d, want 2", jsonMode, c.Count())
		}
		meta := c.Meta(clicontract.EmptyNothingChanged, "nothing to do")
		wantWarnings := 0
		if jsonMode {
			wantWarnings = 2
		}
		if len(meta.Warnings) != wantWarnings {
			t.Errorf("jsonMode=%v Meta.Warnings = %v, want %d", jsonMode, meta.Warnings, wantWarnings)
		}
		var buf bytes.Buffer
		if err := clicontract.WriteJSON(&buf, meta); err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["empty_code"] != "nothing_changed" || decoded["empty_reason"] != "nothing to do" {
			t.Errorf("jsonMode=%v document = %s", jsonMode, buf.String())
		}
	}
	var buf bytes.Buffer
	if err := clicontract.WriteJSON(&buf, clicontract.NewMeta()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "empty_code") || strings.Contains(buf.String(), "empty_reason") {
		t.Errorf("a non-empty result must omit the empty fields: %s", buf.String())
	}
}

// newSelectorCommand builds a command that registers --sessions-from beside
// the selectors it must refuse to combine with, and records what it resolved.
func newSelectorCommand(got *[]string, gotErr *error) *cobra.Command {
	cmd := &cobra.Command{
		Use: "probe",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ids, set, err := clicontract.ResolveSessionsFrom(cmd)
			*got, *gotErr = ids, err
			if err == nil && !set {
				*got = []string{"<unset>"}
			}
			return nil
		},
	}
	cmd.Flags().String("session", "", "one session")
	cmd.Flags().Bool("all", false, "every session")
	clicontract.RegisterSessionsFrom(cmd)
	clicontract.AliasSessionsFrom(cmd, "session-from-file")
	return cmd
}

func runSelector(t *testing.T, stdin string, args ...string) ([]string, error) {
	t.Helper()
	var ids []string
	var err error
	cmd := newSelectorCommand(&ids, &err)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	if execErr := cmd.Execute(); execErr != nil {
		t.Fatalf("execute: %v", execErr)
	}
	return ids, err
}

func TestResolveSessionsFrom(t *testing.T) {
	t.Parallel()
	ids, err := runSelector(t, "")
	if err != nil || !slices.Equal(ids, []string{"<unset>"}) {
		t.Fatalf("unset flag: ids=%v err=%v", ids, err)
	}

	ids, err = runSelector(t, `[{"id":"a"},{"id":"b"}]`, "--sessions-from", "-")
	if err != nil || !slices.Equal(ids, []string{"a", "b"}) {
		t.Fatalf("stdin: ids=%v err=%v", ids, err)
	}

	path := t.TempDir() + "/ids.txt"
	if writeErr := os.WriteFile(path, []byte("c\nd\n"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	ids, err = runSelector(t, "", "--session-from-file", path)
	if err != nil || !slices.Equal(ids, []string{"c", "d"}) {
		t.Fatalf("hidden alias: ids=%v err=%v", ids, err)
	}

	var usage *clicontract.UsageError
	for _, args := range [][]string{
		{"--sessions-from", "-", "--session", "x"},
		{"--sessions-from", "-", "--all"},
	} {
		_, err = runSelector(t, "a\n", args...)
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), "conflicting flags") {
			t.Errorf("%v: want a conflicting-flags usage error, got %v", args, err)
		}
	}

	_, err = runSelector(t, "\n# nothing\n", "--sessions-from", "-")
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "must not widen") {
		t.Errorf("empty list: want a usage error refusing to widen, got %v", err)
	}

	_, err = runSelector(t, "", "--sessions-from", path+".missing")
	if !errors.As(err, &usage) {
		t.Errorf("missing file: want a usage error, got %v", err)
	}
}

func TestAddYesFlagRegistersShorthand(t *testing.T) {
	t.Parallel()
	var yes bool
	cmd := &cobra.Command{Use: "probe", RunE: func(*cobra.Command, []string) error {
		return nil
	}}
	clicontract.AddYesFlag(cmd, &yes, "confirm")
	cmd.SetArgs([]string{"-y"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !yes {
		t.Fatal("-y did not set --yes")
	}
}
