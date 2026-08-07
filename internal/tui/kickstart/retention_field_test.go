package kickstart_test

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/retention_gating.yaml
var retentionGatingData []byte

type retentionGatingCase struct {
	Name                  string `yaml:"name"`
	ClaudeSessionsPresent bool   `yaml:"claudeSessionsPresent"`
	RetentionVisible      bool   `yaml:"retentionVisible"`
}

type retentionGatingDoc struct {
	ExpectedCaseCount int                   `yaml:"expectedCaseCount"`
	Cases             []retentionGatingCase `yaml:"cases"`
}

func loadRetentionGatingDoc(t *testing.T) retentionGatingDoc {
	t.Helper()
	var doc retentionGatingDoc
	dec := yaml.NewDecoder(bytes.NewReader(retentionGatingData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/retention_gating.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("retention_gating.yaml must hold exactly one document")
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases", doc.ExpectedCaseCount, len(doc.Cases))
	}
	return doc
}

// retentionSectionWhen finds the retention section in the registry and evaluates
// its visibility predicate over d. It fails the test when the section is absent,
// so a rename or drop of the section is caught rather than silently reported as
// "not visible".
func retentionSectionWhen(t *testing.T, reg settings.Registry, d *settings.Draft) bool {
	t.Helper()
	for _, s := range reg.Sections {
		if s.Key == kickstart.SectionRetention {
			if s.When == nil {
				return true
			}
			return s.When(d)
		}
	}
	t.Fatalf("registry has no %q section", kickstart.SectionRetention)
	return false
}

// TestRegistry_RetentionGating proves the Claude retention section is offered
// only when Claude Code sessions were discovered.
func TestRegistry_RetentionGating(t *testing.T) {
	doc := loadRetentionGatingDoc(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			reg := kickstart.BuildRegistry(kickstart.Options{
				Source:                scannerfix.NewFixtureTreeSource("standard"),
				ClaudeSessionsPresent: c.ClaudeSessionsPresent,
			})
			d := newGatingDraft(t)
			if got := retentionSectionWhen(t, reg, d); got != c.RetentionVisible {
				t.Errorf("retention section visible = %t, want %t", got, c.RetentionVisible)
			}
		})
	}
}

func newGatingDraft(t *testing.T) *settings.Draft {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatalf("seed base config: %v", err)
	}
	loaded, err := config.Parse(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	d, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	return d
}

// TestProgram_RetentionFieldChoiceReachesWriter proves the draft->writer path: a
// retention value carried on the committed draft is what the retention writer
// receives, even when no fallback RetentionDays is injected. It seeds the draft
// directly; the field->draft accessor path is covered by
// TestRetentionRadio_SelectionWritesDraft.
func TestProgram_RetentionFieldChoiceReachesWriter(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatalf("seed base config: %v", err)
	}
	loaded, err := config.Parse(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	// The mount seeds the retention choice on the draft; the flow carries it to
	// commit. 90 is a value NEITHER the fallback (0) nor a default would produce.
	const chosenDays = 90
	draft.Working().ClaudeRetentionDays = chosenDays

	var wroteDays int
	deps := kickstart.ProgramDeps{
		Theme:                 theme.New(theme.ModeDark),
		Draft:                 draft,
		Source:                scannerfix.NewFixtureTreeSource("standard"),
		ClaudeSessionsPresent: true,
		RetentionDays:         0, // no fallback: the write can only come from the field
		Retention: kickstart.RetentionWriterFunc(func(days int) error {
			wroteDays = days
			return nil
		}),
		Ingest: func(_ context.Context) (*ftue.IngestResult, error) {
			return &ftue.IngestResult{}, nil
		},
	}
	p := kickstart.NewProgram(deps)
	p.SetSize(80, 24)

	p = declineOAuth(t, p)
	p, commitCmd := advanceToCommit(p)
	if !p.Committed() {
		t.Fatal("flow did not commit")
	}
	p = drainCmds(t, p, commitCmd)

	if p.RetentionErr() != nil {
		t.Fatalf("retention write errored: %v", p.RetentionErr())
	}
	if wroteDays != chosenDays {
		t.Fatalf("retention writer received %d days, want %d (the committed field choice)", wroteDays, chosenDays)
	}
}
