package claimprompt_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/claimprompt"
)

func TestParseChoice(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  claimprompt.Choice
		ok    bool
	}{
		{name: "empty-defaults-unclaimed", input: "", want: claimprompt.Unclaimed, ok: true},
		{name: "one", input: "1\n", want: claimprompt.Unclaimed, ok: true},
		{name: "unclaimed-word", input: " unclaimed ", want: claimprompt.Unclaimed, ok: true},
		{name: "two", input: "2\n", want: claimprompt.All, ok: true},
		{name: "all-word", input: "ALL", want: claimprompt.All, ok: true},
		{name: "three", input: "3\n", want: claimprompt.NotNow, ok: true},
		{name: "not-now-word", input: "not now", want: claimprompt.NotNow, ok: true},
		{name: "unknown", input: "yes", ok: false},
		{name: "unknown-number", input: "4", ok: false},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := claimprompt.ParseChoice(test.input)
			if ok != test.ok || got != test.want {
				t.Fatalf("ParseChoice(%q) = %q, %v; want %q, %v", test.input, got, ok, test.want, test.ok)
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

	state, err := claimprompt.Load(path)
	if err != nil {
		t.Fatalf("load missing state: %v", err)
	}
	if state.Decided(base, user) {
		t.Fatal("a missing state file decided the prompt")
	}
	state.Record(base, user, claimprompt.NotNow, 1234)
	if !state.Decided(base, user) {
		t.Fatal("recorded decline is not decided")
	}
	if state.Decided(base, "22222222-2222-4222-8222-222222222222") {
		t.Fatal("a decision leaked to another Village user")
	}
	if state.Decided("https://village.example/other", user) {
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
	decision, ok := loaded.Decisions[claimprompt.ScopeKey(base, user)]
	if !ok || decision.Choice != claimprompt.NotNow || decision.DecidedAt != 1234 {
		t.Fatalf("decision = %+v, %v; want the saved decline", decision, ok)
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
