package kickstart_test

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// TestPublicationSectionIsAlwaysVisibleFirstClassChoice proves the keep-local
// choice is a first-class, always-offered publication preference - not the side
// effect of declining the connect prompt - and that its framing states plainly
// that the screen only records a preference, that kickstart publishes nothing now
// or when the user finishes, and that a later publish is `peasant village push`.
func TestPublicationSectionIsAlwaysVisibleFirstClassChoice(t *testing.T) {
	t.Parallel()
	for _, connected := range []bool{false, true} {
		connected := connected
		t.Run(map[bool]string{false: "disconnected", true: "connected"}[connected], func(t *testing.T) {
			t.Parallel()
			reg := kickstart.BuildRegistry(kickstart.Options{
				Source:           scannerfix.NewFixtureTreeSource("standard"),
				VillageConnected: connected,
			})
			section := findSection(t, reg, kickstart.SectionPublication)

			// Always visible: no When predicate gates it on a village connection.
			if section.When != nil {
				t.Fatal("publication section is conditional; keep-local must be a first-class always-visible choice")
			}
			if len(section.Fields) != 1 {
				t.Fatalf("publication section has %d fields, want its one canonical radio", len(section.Fields))
			}
			field := section.Fields[0]
			if field.Key() != kickstart.FieldPublication || field.Kind().String() != "radio" {
				t.Fatalf("publication field = %q/%s, want %q/radio",
					field.Key(), field.Kind(), kickstart.FieldPublication)
			}

			// Render the section and assert the explicit keep-local option and the
			// preference-only, publishes-nothing framing are all present.
			draft, _ := settings.NewDraft(filepath.Join(t.TempDir(), "config.yaml"), config.BaseConfig())
			flow := settings.NewFlow(theme.New(theme.ModeDark),
				settings.Registry{Sections: []settings.Section{section}}, draft)
			flow.SetSize(120, 40)
			view := stripRender(flow.View())
			for _, want := range []string{
				"keep local, do not publish",
				"plan to publish later",
				"this only records a preference; kickstart itself publishes nothing.",
				"nothing is published when kickstart finishes.",
				"peasant village push",
			} {
				if !strings.Contains(view, want) {
					t.Errorf("publication section does not render required framing %q:\n%s", want, view)
				}
			}
		})
	}
}

// TestKeepLocalChoicePersistsAndCommitPathPublishesNothing drives the mounted
// kickstart program through its atomic commit and proves two things: choosing
// keep-local persists that preference to the real config file, and the only
// side effects of the commit path are the config save and the local import -
// no publish or push. The seed starts on "plan to publish later" so the persisted
// keep-local value is a genuine change, keeping the assertion non-vacuous.
func TestKeepLocalChoicePersistsAndCommitPathPublishesNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	seed := config.BaseConfig()
	seed.Push.SharePreference = config.SharePreferenceShareLater
	if err := config.SaveAtomic(path, seed); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	before := mustReadFile(t, path)
	loaded, err := config.Parse(before)
	if err != nil {
		t.Fatalf("parse seed config: %v", err)
	}
	if loaded.Push.SharePreference != config.SharePreferenceShareLater {
		t.Fatalf("precondition: on-disk sharePreference = %q, want share-later", loaded.Push.SharePreference)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}

	// The program is given only a local-import seam. There is deliberately no
	// publish/push dependency to inject, so effects can name at most config save
	// and local import.
	var effects []string
	program := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:  theme.New(theme.ModeDark),
		Draft:  draft,
		Source: scannerfix.NewFixtureTreeSource("standard"),
		Ingest: func(context.Context) (*ftue.IngestResult, error) {
			effects = append(effects, "ingest")
			return &ftue.IngestResult{New: 1}, nil
		},
	})
	program.SetSize(120, 40)
	program = declineOAuth(t, program)
	program = chooseKeepLocal(t, program)

	var cmd tea.Cmd
	program, cmd = advanceToCommit(program)
	program = drainCmds(t, program, cmd)

	if !program.Committed() {
		t.Fatal("program did not commit the config")
	}
	after := mustReadFile(t, path)
	persisted, err := config.Parse(after)
	if err != nil {
		t.Fatalf("parse committed config: %v", err)
	}
	if persisted.Push.SharePreference != config.SharePreferenceKeepLocal {
		t.Fatalf("persisted sharePreference = %q, want keep-local", persisted.Push.SharePreference)
	}
	// The commit path saved config and ran the local import - and nothing else.
	if !reflect.DeepEqual(effects, []string{"ingest"}) {
		t.Fatalf("commit-path effects = %v, want only the local import (no publish/push)", effects)
	}
}

// chooseKeepLocal advances the mounted flow to the publication section and
// selects the keep-local option. Pressing up twice clamps the radio highlight to
// the first option (keep-local) regardless of which value was highlighted first.
func chooseKeepLocal(t *testing.T, p kickstart.Program) kickstart.Program {
	t.Helper()
	if p.Phase() != kickstart.PhaseFlow {
		t.Fatalf("chooseKeepLocal expects the flow phase, got %s", p.Phase())
	}
	// From the selection step, one tab reaches publication (auto-ingest is hidden
	// for the default all-mode selection).
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	p, _ = p.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	return p
}
