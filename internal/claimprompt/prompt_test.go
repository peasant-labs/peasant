package claimprompt_test

import (
	"bytes"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/claimprompt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/parse_choice_cases.yaml
var parseChoiceCasesYAML []byte

type parseChoiceFixture struct {
	RequiredNames []string          `yaml:"requiredNames"`
	Cases         []parseChoiceCase `yaml:"cases"`
}

type parseChoiceCase struct {
	Name  string `yaml:"name"`
	Input string `yaml:"input"`
	Want  string `yaml:"want"`
	OK    bool   `yaml:"ok"`
}

func TestParseChoice(t *testing.T) {
	t.Parallel()
	var fixture parseChoiceFixture
	decoder := yaml.NewDecoder(bytes.NewReader(parseChoiceCasesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode parse_choice_cases.yaml: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, test := range fixture.Cases {
		names = append(names, test.Name)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "parse_choice_cases.yaml"); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		test := test
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			want := claimprompt.Choice(test.Want)
			got, ok := claimprompt.ParseChoice(test.Input)
			if ok != test.OK || got != want {
				t.Fatalf("ParseChoice(%q) = %q, %v; want %q, %v", test.Input, got, ok, want, test.OK)
			}
		})
	}
}

func TestPromptDefaultsAndRetries(t *testing.T) {
	t.Parallel()
	t.Run("empty-answer-is-unclaimed-default", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		choice, err := claimprompt.Prompt(strings.NewReader("\n"), &out, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if choice != claimprompt.Unclaimed {
			t.Fatalf("choice = %q, want %q", choice, claimprompt.Unclaimed)
		}
		if !strings.Contains(out.String(), "alice") || !strings.Contains(out.String(), "Not now") {
			t.Fatalf("prompt = %q, want the three-way question for alice", out.String())
		}
	})
	t.Run("invalid-answer-repeats-then-accepts", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		choice, err := claimprompt.Prompt(strings.NewReader("yes\n2\n"), &out, "")
		if err != nil {
			t.Fatal(err)
		}
		if choice != claimprompt.All {
			t.Fatalf("choice = %q, want %q", choice, claimprompt.All)
		}
		if !strings.Contains(out.String(), "Enter 1, 2, or 3.") {
			t.Fatalf("prompt = %q, want an invalid-answer retry", out.String())
		}
	})
	t.Run("eof-dismisses", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		choice, err := claimprompt.Prompt(strings.NewReader(""), &out, "")
		if err != nil {
			t.Fatal(err)
		}
		if choice != claimprompt.NotNow {
			t.Fatalf("choice = %q, want %q", choice, claimprompt.NotNow)
		}
	})
	t.Run("read-error-is-reported", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		_, err := claimprompt.Prompt(errorReader{}, &out, "")
		if err == nil || !strings.Contains(err.Error(), "nothing was claimed") {
			t.Fatalf("error = %v, want a read failure naming that nothing was claimed", err)
		}
	})
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestStateRoundTripAndScopeKeys(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", claimprompt.FileName)
	const base = "https://village.example/team"
	const user = "11111111-1111-4111-8111-111111111111"
	const owner = "lo_fixture"

	state, err := claimprompt.Load(path)
	if err != nil {
		t.Fatalf("load missing state: %v", err)
	}
	if state.Decided(owner, base, user) {
		t.Fatal("a missing state file decided the prompt")
	}
	state.Record(owner, base, user, claimprompt.NotNow, 1234)
	if !state.Decided(owner, base, user) {
		t.Fatal("recorded decline is not decided")
	}
	if state.Decided(owner, base, "22222222-2222-4222-8222-222222222222") {
		t.Fatal("a decision leaked to another Village user")
	}
	if state.Decided(owner, "https://village.example/other", user) {
		t.Fatal("a decision leaked to another Village base URL")
	}
	if err := claimprompt.Save(path, state); err != nil {
		t.Fatalf("save state: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := claimprompt.Load(path)
	if err != nil {
		t.Fatalf("load saved state: %v", err)
	}
	decision, ok := loaded.Decisions[claimprompt.ScopeKey(owner, base, user)]
	if !ok || decision.Choice != claimprompt.NotNow || decision.DecidedAt != 1234 {
		t.Fatalf("decision = %+v, %v; want the saved decline", decision, ok)
	}
}

func TestStateKeysIncludeLocalOwner(t *testing.T) {
	t.Parallel()
	var state claimprompt.State
	state.Record("lo_one", "https://village.example/team", "11111111-1111-4111-8111-111111111111", claimprompt.Unclaimed, 1)
	if state.Decided("lo_two", "https://village.example/team", "11111111-1111-4111-8111-111111111111") {
		t.Fatal("a decision in one data directory suppressed another data directory's prompt")
	}
	if !state.Decided("lo_one", "https://village.example/team", "11111111-1111-4111-8111-111111111111") {
		t.Fatal("the recorded local owner and scope did not decide the prompt")
	}
}

func TestNoticeIsSeparateFromDecision(t *testing.T) {
	t.Parallel()
	var state claimprompt.State
	state.RecordNotice("lo_one", "https://village.example/team", "11111111-1111-4111-8111-111111111111", 1)
	if !state.Noticed("lo_one", "https://village.example/team", "11111111-1111-4111-8111-111111111111") {
		t.Fatal("the recorded notice was not remembered")
	}
	if state.Decided("lo_one", "https://village.example/team", "11111111-1111-4111-8111-111111111111") {
		t.Fatal("showing the notice settled the prompt; the real prompt must remain due")
	}
	if state.Noticed("lo_two", "https://village.example/team", "11111111-1111-4111-8111-111111111111") {
		t.Fatal("a notice leaked to another local owner")
	}
}

func TestLoadRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), claimprompt.FileName)
	if err := os.WriteFile(path, []byte(`{"version":99,"decisions":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := claimprompt.Load(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("error = %v, want an unsupported-version refusal", err)
	}
}
