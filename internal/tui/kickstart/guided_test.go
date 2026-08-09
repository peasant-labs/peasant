package kickstart_test

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	expectedGuidedFramingRows       = 12
	expectedGuidedLifecycleRows     = 3
	expectedForbiddenAuthorityNames = 5
)

type guidedSurface string

const (
	guidedSurfaceConnect         guidedSurface = "connect"
	guidedSurfaceRegistrySection guidedSurface = "registry-section"
	guidedSurfaceDerivedExample  guidedSurface = "derived-example"
	guidedSurfaceSummary         guidedSurface = "guided-summary"
)

func (s guidedSurface) valid() bool {
	switch s {
	case guidedSurfaceConnect, guidedSurfaceRegistrySection, guidedSurfaceDerivedExample, guidedSurfaceSummary:
		return true
	default:
		return false
	}
}

type guidedOperation string

const (
	guidedOperationExit     guidedOperation = "exit"
	guidedOperationComplete guidedOperation = "complete"
)

func (o guidedOperation) valid() bool {
	switch o {
	case guidedOperationExit, guidedOperationComplete:
		return true
	default:
		return false
	}
}

type guideFixture struct {
	Intro string   `yaml:"intro"`
	Hints []string `yaml:"hints"`
}

type guidedFramingRow struct {
	Name                  string               `yaml:"name"`
	Surface               guidedSurface        `yaml:"surface"`
	SectionKey            string               `yaml:"sectionKey"`
	SectionTitle          string               `yaml:"sectionTitle"`
	SelectionMode         config.SelectionMode `yaml:"selectionMode"`
	VillageConnected      bool                 `yaml:"villageConnected"`
	ClaudeSessionsPresent bool                 `yaml:"claudeSessionsPresent"`
	WantVisible           bool                 `yaml:"wantVisible"`
	FieldKey              string               `yaml:"fieldKey"`
	FieldKind             string               `yaml:"fieldKind"`
	FieldText             string               `yaml:"fieldText"`
	ExampleText           string               `yaml:"exampleText"`
	Guide                 guideFixture         `yaml:"guide"`
	WantContains          []string             `yaml:"wantContains"`
	WantBeforeChoice      []string             `yaml:"wantBeforeChoice"`
	WantMissing           []string             `yaml:"wantMissing"`
}

type guidedFramingDoc struct {
	ExpectedRowCount int                `yaml:"expectedRowCount"`
	Rows             []guidedFramingRow `yaml:"rows"`
}

type authorityGuardFixture struct {
	ExpectedNameCount int      `yaml:"expectedNameCount"`
	Names             []string `yaml:"names"`
}

type guidedLifecycleRow struct {
	Name                   string          `yaml:"name"`
	Operation              guidedOperation `yaml:"operation"`
	EditLicense            config.License  `yaml:"editLicense"`
	RetentionDays          int             `yaml:"retentionDays"`
	ClaudeSessionsPresent  bool            `yaml:"claudeSessionsPresent"`
	WantCommitted          bool            `yaml:"wantCommitted"`
	WantExited             bool            `yaml:"wantExited"`
	WantConfigChanged      bool            `yaml:"wantConfigChanged"`
	WantEffects            []string        `yaml:"wantEffects"`
	WantIngestResult       bool            `yaml:"wantIngestResult"`
	WantRetentionSawCommit bool            `yaml:"wantRetentionSawCommit"`
}

type guidedLifecycleDoc struct {
	ExpectedRowCount int                   `yaml:"expectedRowCount"`
	AuthorityGuard   authorityGuardFixture `yaml:"authorityGuard"`
	Rows             []guidedLifecycleRow  `yaml:"rows"`
}

//go:embed testdata/guided/framing.yaml
var guidedFramingData []byte

//go:embed testdata/guided/lifecycle.yaml
var guidedLifecycleData []byte

func decodeSingleKnownFieldsDocument(t *testing.T, name string, data []byte, dst any) {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("%s must hold exactly one YAML document", name)
	}
}

func loadGuidedFramingDoc(t *testing.T) guidedFramingDoc {
	t.Helper()
	var doc guidedFramingDoc
	decodeSingleKnownFieldsDocument(t, "testdata/guided/framing.yaml", guidedFramingData, &doc)
	if doc.ExpectedRowCount != expectedGuidedFramingRows || len(doc.Rows) != expectedGuidedFramingRows {
		t.Fatalf("guided framing fixture count: declared=%d actual=%d required=%d",
			doc.ExpectedRowCount, len(doc.Rows), expectedGuidedFramingRows)
	}
	names := map[string]bool{}
	for _, row := range doc.Rows {
		if strings.TrimSpace(row.Name) == "" || names[row.Name] {
			t.Fatalf("guided framing row name %q is missing or duplicated", row.Name)
		}
		names[row.Name] = true
		if !row.Surface.valid() {
			t.Fatalf("guided framing row %q has unknown surface %q", row.Name, row.Surface)
		}
		switch row.Surface {
		case guidedSurfaceConnect:
			if len(row.WantContains) == 0 || len(row.WantBeforeChoice) == 0 {
				t.Fatalf("guided framing row %q asserts no connect copy", row.Name)
			}
		case guidedSurfaceSummary:
			if len(row.WantContains) == 0 {
				t.Fatalf("guided framing row %q asserts no review summary", row.Name)
			}
		case guidedSurfaceRegistrySection, guidedSurfaceDerivedExample:
			if row.SectionKey == "" || row.SectionTitle == "" || row.FieldKey == "" ||
				row.FieldKind == "" || row.FieldText == "" || row.Guide.Intro == "" || len(row.Guide.Hints) == 0 {
				t.Fatalf("guided framing row %q leaves its section, field, or guide contract unspecified", row.Name)
			}
			if row.Surface == guidedSurfaceRegistrySection &&
				row.SelectionMode != config.SelectionModeAll && row.SelectionMode != config.SelectionModeSelected {
				t.Fatalf("guided framing row %q has unknown selection mode %q", row.Name, row.SelectionMode)
			}
		}
	}
	return doc
}

func loadGuidedLifecycleDoc(t *testing.T) guidedLifecycleDoc {
	t.Helper()
	var doc guidedLifecycleDoc
	decodeSingleKnownFieldsDocument(t, "testdata/guided/lifecycle.yaml", guidedLifecycleData, &doc)
	if doc.ExpectedRowCount != expectedGuidedLifecycleRows || len(doc.Rows) != expectedGuidedLifecycleRows {
		t.Fatalf("guided lifecycle fixture count: declared=%d actual=%d required=%d",
			doc.ExpectedRowCount, len(doc.Rows), expectedGuidedLifecycleRows)
	}
	if doc.AuthorityGuard.ExpectedNameCount != expectedForbiddenAuthorityNames ||
		len(doc.AuthorityGuard.Names) != expectedForbiddenAuthorityNames {
		t.Fatalf("authority guard count: declared=%d actual=%d required=%d",
			doc.AuthorityGuard.ExpectedNameCount, len(doc.AuthorityGuard.Names), expectedForbiddenAuthorityNames)
	}
	names := map[string]bool{}
	for _, row := range doc.Rows {
		if strings.TrimSpace(row.Name) == "" || names[row.Name] {
			t.Fatalf("guided lifecycle row name %q is missing or duplicated", row.Name)
		}
		names[row.Name] = true
		if !row.Operation.valid() {
			t.Fatalf("guided lifecycle row %q has unknown operation %q", row.Name, row.Operation)
		}
		if row.EditLicense == "" || row.RetentionDays <= 0 {
			t.Fatalf("guided lifecycle row %q does not exercise a real pending edit and retention effect", row.Name)
		}
	}
	return doc
}

func newGuidedDraft(t *testing.T) (*settings.Draft, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatalf("seed guided config: %v", err)
	}
	loaded, err := config.Parse(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("parse guided config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open guided draft: %v", err)
	}
	return draft, path
}

func findSection(t *testing.T, reg settings.Registry, key string) settings.Section {
	t.Helper()
	for _, section := range reg.Sections {
		if section.Key == key {
			return section
		}
	}
	t.Fatalf("canonical registry has no section %q", key)
	return settings.Section{}
}

func sectionVisible(section settings.Section, draft *settings.Draft) bool {
	return section.When == nil || section.When(draft)
}

func assertGuideMetadata(t *testing.T, row guidedFramingRow, section settings.Section) {
	t.Helper()
	if section.Title != row.SectionTitle {
		t.Errorf("section %q title = %q, want %q", row.SectionKey, section.Title, row.SectionTitle)
	}
	if len(section.Fields) != 1 {
		t.Fatalf("section %q has %d fields, want its one canonical field", row.SectionKey, len(section.Fields))
	}
	field := section.Fields[0]
	if field.Key() != row.FieldKey || field.Kind().String() != row.FieldKind {
		t.Errorf("section %q field identity = %q/%s, want %q/%s",
			row.SectionKey, field.Key(), field.Kind(), row.FieldKey, row.FieldKind)
	}
	if section.Guide == nil {
		t.Fatalf("section %q has no guided framing", row.SectionKey)
	}
	if section.Guide.Intro != row.Guide.Intro || !reflect.DeepEqual(section.Guide.Hints, row.Guide.Hints) {
		t.Errorf("section %q guide = %#v, want intro %q and hints %v",
			row.SectionKey, section.Guide, row.Guide.Intro, row.Guide.Hints)
	}
}

func advanceFlowToSection(t *testing.T, flow settings.Flow, key string, sectionCount int) settings.Flow {
	t.Helper()
	for step := 0; step <= sectionCount && !flow.OnReceipt(); step++ {
		if flow.CurrentSectionKey() == key {
			return flow
		}
		flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	t.Fatalf("visible section %q was never reached", key)
	return flow
}

func allVisibleFlowViews(t *testing.T, flow settings.Flow, sectionCount int) string {
	t.Helper()
	var views []string
	for step := 0; step <= sectionCount && !flow.OnReceipt(); step++ {
		views = append(views, stripRender(flow.View()))
		flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	return strings.Join(views, "\n")
}

func assertFramingBeforeField(t *testing.T, row guidedFramingRow, view string) {
	t.Helper()
	plain := stripRender(view)
	fieldAt := strings.Index(plain, row.FieldText)
	if fieldAt < 0 {
		t.Fatalf("section %q does not render unchanged field text %q:\n%s", row.SectionKey, row.FieldText, plain)
	}
	wantBefore := append([]string{row.Guide.Intro}, row.Guide.Hints...)
	if row.ExampleText != "" {
		wantBefore = append(wantBefore, row.ExampleText)
	}
	for _, want := range wantBefore {
		at := strings.Index(plain, want)
		if at < 0 {
			t.Errorf("section %q does not render guide text %q:\n%s", row.SectionKey, want, plain)
			continue
		}
		if at >= fieldAt {
			t.Errorf("section %q renders guide text %q after its field, want guide before unchanged fields", row.SectionKey, want)
		}
	}
	for _, want := range row.WantContains {
		if !strings.Contains(plain, want) {
			t.Errorf("section %q does not render required guidance %q:\n%s", row.SectionKey, want, plain)
		}
	}
	for _, forbidden := range row.WantMissing {
		if strings.Contains(plain, forbidden) {
			t.Errorf("section %q renders forbidden guidance %q:\n%s", row.SectionKey, forbidden, plain)
		}
	}
}

func TestGuidedFramingFixture(t *testing.T) {
	doc := loadGuidedFramingDoc(t)
	for _, row := range doc.Rows {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			draft, _ := newGuidedDraft(t)
			th := theme.New(theme.ModeDark)
			switch row.Surface {
			case guidedSurfaceConnect:
				program := kickstart.NewProgram(kickstart.ProgramDeps{
					Theme:  th,
					Draft:  draft,
					Source: scannerfix.NewFixtureTreeSource("standard"),
				})
				program.SetSize(120, 40)
				view := stripRender(program.View())
				for _, want := range row.WantContains {
					if !strings.Contains(view, want) {
						t.Errorf("connect framing does not contain %q:\n%s", want, view)
					}
				}
				choiceAt := strings.Index(view, "connect to a village now?")
				if choiceAt < 0 {
					t.Fatalf("connect framing does not render its choice after the safety explanation:\n%s", view)
				}
				for _, want := range row.WantBeforeChoice {
					copyAt := strings.Index(view, want)
					if copyAt < 0 || copyAt >= choiceAt {
						t.Errorf("connect safety copy %q must precede the choice; copy=%d choice=%d:\n%s", want, copyAt, choiceAt, view)
					}
				}
			case guidedSurfaceRegistrySection:
				draft.Working().Selection.Mode = row.SelectionMode
				reg := kickstart.BuildRegistry(kickstart.Options{
					Source:                scannerfix.NewFixtureTreeSource("standard"),
					VillageConnected:      row.VillageConnected,
					ClaudeSessionsPresent: row.ClaudeSessionsPresent,
				})
				section := findSection(t, reg, row.SectionKey)
				assertGuideMetadata(t, row, section)
				if got := sectionVisible(section, draft); got != row.WantVisible {
					t.Fatalf("section %q visibility = %t, want %t", row.SectionKey, got, row.WantVisible)
				}
				flow := settings.NewFlow(th, reg, draft)
				flow.SetSize(120, 40)
				if row.WantVisible {
					flow = advanceFlowToSection(t, flow, row.SectionKey, len(reg.Sections))
					assertFramingBeforeField(t, row, flow.View())
					return
				}
				visibleViews := allVisibleFlowViews(t, flow, len(reg.Sections))
				if strings.Contains(visibleViews, row.Guide.Intro) {
					t.Errorf("hidden section %q leaked its guide into the visible flow:\n%s", row.SectionKey, visibleViews)
				}
			case guidedSurfaceDerivedExample:
				called := false
				reg := settings.Registry{Sections: []settings.Section{{
					Key:   row.SectionKey,
					Title: row.SectionTitle,
					Guide: &settings.Guide{
						Intro: row.Guide.Intro,
						Hints: row.Guide.Hints,
						Example: func(gotTheme theme.Theme, gotDraft *settings.Draft) (string, error) {
							called = true
							if gotTheme.Mode != th.Mode || gotDraft != draft {
								t.Errorf("guide example received theme/draft %v/%p, want %v/%p", gotTheme.Mode, gotDraft, th.Mode, draft)
							}
							return row.ExampleText, nil
						},
					},
					Fields: []settings.Field{settings.Info(row.FieldKey, func(*settings.Draft) string {
						return row.FieldText
					})},
				}}}
				flow := settings.NewFlow(th, reg, draft)
				flow.SetSize(120, 40)
				assertFramingBeforeField(t, row, flow.View())
				if !called {
					t.Fatal("guided Flow did not invoke the section's optional Example")
				}
			case guidedSurfaceSummary:
				draft.Working().Push.License = config.LicenseCC0
				reg := kickstart.BuildRegistry(kickstart.Options{Source: scannerfix.NewFixtureTreeSource("standard")})
				flow := settings.NewFlow(th, reg, draft)
				flow.SetSize(120, 40)
				for step := 0; step <= len(reg.Sections) && !flow.OnReceipt(); step++ {
					flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyTab})
				}
				if !flow.OnReceipt() {
					t.Fatal("guided Flow did not reach its presentation-owned review summary")
				}
				view := stripRender(flow.View())
				for _, want := range row.WantContains {
					if !strings.Contains(view, want) {
						t.Errorf("guided review summary does not contain %q:\n%s", want, view)
					}
				}
			}
		})
	}
}

func TestGuidedLifecycleFixture(t *testing.T) {
	doc := loadGuidedLifecycleDoc(t)
	for _, row := range doc.Rows {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			draft, path := newGuidedDraft(t)
			before := mustReadFile(t, path)
			draft.Working().Push.License = row.EditLicense
			if err := kickstart.SeedRetentionInitial(draft, row.RetentionDays); err != nil {
				t.Fatalf("seed paired retention state: %v", err)
			}
			if draft.Baseline().ClaudeRetentionDays != row.RetentionDays ||
				draft.Working().ClaudeRetentionDays != row.RetentionDays {
				t.Fatalf("paired retention seed = %d/%d, want %d/%d",
					draft.Baseline().ClaudeRetentionDays, draft.Working().ClaudeRetentionDays,
					row.RetentionDays, row.RetentionDays)
			}
			effects := []string{}
			retentionSawCommit := false
			program := kickstart.NewProgram(kickstart.ProgramDeps{
				Theme:                 theme.New(theme.ModeDark),
				Draft:                 draft,
				Source:                scannerfix.NewFixtureTreeSource("standard"),
				ClaudeSessionsPresent: row.ClaudeSessionsPresent,
				RetentionDays:         0,
				Retention: kickstart.RetentionWriterFunc(func(int) error {
					effects = append(effects, "retention")
					persisted, err := config.Parse(mustReadFile(t, path))
					if err == nil {
						retentionSawCommit = persisted.Push.License == row.EditLicense
					}
					return err
				}),
				Ingest: func(context.Context) (*ftue.IngestResult, error) {
					effects = append(effects, "ingest")
					return &ftue.IngestResult{New: 1}, nil
				},
			})
			program.SetSize(120, 40)
			program = declineOAuth(t, program)

			switch row.Operation {
			case guidedOperationExit:
				program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
				if !program.Confirming() {
					t.Fatal("escape did not open the no-save confirmation")
				}
				program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
				program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			case guidedOperationComplete:
				var cmd tea.Cmd
				program, cmd = advanceToCommit(program)
				program = drainCmds(t, program, cmd)
			}

			after := mustReadFile(t, path)
			changed := !bytes.Equal(before, after)
			if program.Committed() != row.WantCommitted || program.Exited() != row.WantExited || changed != row.WantConfigChanged {
				t.Errorf("lifecycle result committed/exited/changed = %t/%t/%t, want %t/%t/%t",
					program.Committed(), program.Exited(), changed,
					row.WantCommitted, row.WantExited, row.WantConfigChanged)
			}
			if !reflect.DeepEqual(effects, row.WantEffects) {
				t.Errorf("effect order = %v, want %v", effects, row.WantEffects)
			}
			if retentionSawCommit != row.WantRetentionSawCommit {
				t.Errorf("retention writer observed committed config = %t, want %t",
					retentionSawCommit, row.WantRetentionSawCommit)
			}
			if got := program.IngestResult() != nil; got != row.WantIngestResult {
				t.Errorf("ingest result present = %t, want %t", got, row.WantIngestResult)
			}
		})
	}
}

func TestProgramDependenciesExposeNoPublicationAuthority(t *testing.T) {
	doc := loadGuidedLifecycleDoc(t)
	typ := reflect.TypeOf(kickstart.ProgramDeps{})
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		descriptor := strings.ToLower(field.Name + " " + field.Type.String())
		for _, forbidden := range doc.AuthorityGuard.Names {
			if strings.Contains(descriptor, strings.ToLower(forbidden)) {
				t.Errorf("kickstart ProgramDeps field %s exposes forbidden publication authority %q", field.Name, forbidden)
			}
		}
	}
}
