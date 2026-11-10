package kickstart

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/schema"
)

// Options carries the runtime seams the kickstart presentation composes over.
// Copy, option menus, and visibility live on the shared onboarding model.
type Options struct {
	// Source feeds the selection tree its provider->remote->worktree->session
	// forest. In production it is the real scanner adapter over discovery; in the
	// dev loop and tests it is scannerfix.FixtureTreeSource.
	Source kit.TreeSource
	// VillageConnected gates the destination field when no live function is set.
	VillageConnected bool
	// VillageConnectedFunc reports live connection state for the guided Program.
	// When present it takes precedence over VillageConnected.
	VillageConnectedFunc func() bool
	// ClaudeSessionsPresent gates the Claude transcript-retention field.
	ClaudeSessionsPresent bool
	// Preview loads the transcript shown beside the selection tree. When nil
	// the step renders the tree alone.
	Preview kit.BodySource
}

// Section keys are the shared model's section tokens.
const (
	SectionSelection   = string(onboarding.FlowSelection)
	SectionAutoIngest  = string(onboarding.FlowAutoIngest)
	SectionPublication = string(onboarding.FlowPublication)
	SectionAutoPublish = string(onboarding.FlowAutoPublish)
	SectionPrivacy     = string(onboarding.FlowPrivacy)
	SectionLicense     = string(onboarding.FlowLicense)
	SectionDestination = string(onboarding.FlowDestination)
	SectionRetention   = string(onboarding.FlowRetention)
)

// Field keys are the shared model's field tokens.
const (
	FieldSelection   = string(onboarding.FieldSelection)
	FieldAutoIngest  = string(onboarding.FieldAutoIngest)
	FieldPublication = string(onboarding.FieldPublication)
	FieldAutoPublish = string(onboarding.FieldAutoPublish)
	FieldPrivacy     = string(onboarding.FieldPrivacy)
	FieldLicense     = string(onboarding.FieldLicense)
	FieldVisibility  = string(onboarding.FieldVisibility)
	FieldRetention   = string(onboarding.FieldRetention)
)

// RecommendedRetentionDays is the retention choice a first-run flow starts on
// when Claude Code has no cleanup period set.
const RecommendedRetentionDays = onboarding.RecommendedRetentionDays

// BuildRegistry adapts the shared model into a settings.Registry. It switches
// on field kind and carries no copy, no option set, and no visibility rule.
func BuildRegistry(opts Options) settings.Registry {
	model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeFull})
	sections := make([]settings.Section, 0, len(model.SectionStops()))
	for _, stop := range model.SectionStops() {
		if len(stop.Fields) != 1 || stop.Fields[0] == nil {
			panic(fmt.Errorf("adapt setup stop %s: a settings section has one field", stop.Key))
		}
		field := stop.Fields[0]
		example := adaptExample(field)
		adapted := adaptField(field, opts)
		if stop.Guide.ExampleInSplitPane {
			adapted = settings.WithExamplePane(adapted, example)
		}
		sections = append(sections, settings.Section{
			Key:    stop.Key.String(),
			Title:  stop.Title,
			Fields: []settings.Field{adapted},
			When:   adaptWhen(stop, opts),
			Guide:  adaptGuide(stop, example),
		})
	}
	return settings.Registry{Sections: sections}
}

func (o Options) villageConnected() bool {
	if o.VillageConnectedFunc != nil {
		return o.VillageConnectedFunc()
	}
	return o.VillageConnected
}

func adaptWhen(stop onboarding.Stop, opts Options) func(*settings.Draft) bool {
	if len(stop.Visible) == 0 {
		return nil
	}
	conditions := append([]onboarding.Condition(nil), stop.Visible...)
	return func(d *settings.Draft) bool {
		return onboarding.ConditionsHold(conditions, presentationContext(d, opts))
	}
}

func presentationContext(d *settings.Draft, opts Options) onboarding.WriteContext {
	var cfg *config.Config
	if d != nil {
		cfg = d.Working()
	}
	return onboarding.WriteContext{
		Config:           cfg,
		VillageConnected: opts.villageConnected(),
		ClaudeSessions:   opts.ClaudeSessionsPresent,
	}
}

func adaptGuide(stop onboarding.Stop, example settings.GuideExampleFunc) *settings.Guide {
	if stop.Guide.Intro == "" && len(stop.Guide.Hints) == 0 && !stop.Guide.ExampleInSplitPane {
		return nil
	}
	guide := &settings.Guide{
		Intro:              stop.Guide.Intro,
		Hints:              append([]string(nil), stop.Guide.Hints...),
		ExampleInSplitPane: stop.Guide.ExampleInSplitPane,
	}
	if stop.Guide.ExampleInSplitPane {
		guide.Example = example
	}
	return guide
}

func adaptField(field onboarding.Field, opts Options) settings.Field {
	key := field.Key().String()
	label := field.Label()
	var adapted settings.Field
	switch field.Kind() {
	case onboarding.FieldKindToggle:
		adapted = settings.Toggle(key, label, boolAccessor(field))
	case onboarding.FieldKindChoice:
		adapted = settings.Radio(key, label, stringAccessor(field), radioOptions(field)...)
	case onboarding.FieldKindTree:
		adapted = settings.Tree(key, label, treeAccessor(field), opts.Source, selectionTreeOptions(opts)...)
	default:
		panic(fmt.Errorf(
			"adapt setup field %s.\nwhat: kind %s has no settings control.\nwhy: the terminal registry only mounts toggle, choice, and tree fields.\nwhere: kickstart.BuildRegistry.\nwhen: the shared model was adapted.\nmeans: the registry was not built.\nfix: keep phase action and notice fields off the settings sections.",
			key, field.Kind()))
	}
	if field.Description() != "" {
		adapted = settings.WithDescription(adapted, field.Description())
	}
	return adapted
}

func radioOptions(field onboarding.Field) []settings.Option[string] {
	options := field.Options()
	out := make([]settings.Option[string], len(options))
	for i, option := range options {
		out[i] = settings.Option[string]{
			Label:       option.Label,
			Value:       option.Value,
			Description: option.Description,
		}
	}
	return out
}

func boolAccessor(field onboarding.Field) settings.Accessor[bool] {
	return settings.Accessor[bool]{
		Get: func(cfg *config.Config) bool {
			value, err := field.Read(cfg)
			if err != nil {
				return false
			}
			on, err := onboarding.BoolToken(value)
			if err != nil {
				return false
			}
			return on
		},
		Set: func(cfg *config.Config, on bool) {
			_ = field.Write(cfg, onboarding.WriteContext{Config: cfg}, onboarding.BoolValue(on))
		},
	}
}

func stringAccessor(field onboarding.Field) settings.Accessor[string] {
	return settings.Accessor[string]{
		Restore: func(dst, src *config.Config) { _ = field.Restore(dst, src) },
		Get: func(cfg *config.Config) string {
			value, err := field.Read(cfg)
			if err != nil {
				return ""
			}
			if token, err := onboarding.StringToken(value); err == nil {
				return token
			}
			// An integer choice is stored as a JSON number and shown as its decimal token.
			if number, err := onboarding.IntToken(value); err == nil {
				return strconv.Itoa(number)
			}
			return ""
		},
		Set: func(cfg *config.Config, token string) {
			_ = field.Write(cfg, onboarding.WriteContext{Config: cfg}, onboarding.StringValue(token))
		},
	}
}

func treeAccessor(field onboarding.Field) settings.Accessor[settings.TreeSelection] {
	return settings.Accessor[settings.TreeSelection]{
		Get: func(cfg *config.Config) settings.TreeSelection {
			if cfg == nil {
				return settings.TreeSelection{}
			}
			return settings.TreeSelection{Mode: cfg.Selection.Mode, Harnesses: cfg.Selection.Harnesses}
		},
		Set: func(cfg *config.Config, selection settings.TreeSelection) {
			value, err := onboarding.SelectionMappingValue(selection.Mode, selection.Harnesses)
			if err != nil {
				return
			}
			_ = field.Write(cfg, onboarding.WriteContext{Config: cfg}, value)
		},
	}
}

func retentionAccessor() settings.Accessor[int] {
	model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeFull})
	field, ok := model.Field(onboarding.FieldRetention)
	if !ok {
		panic("adapt retention: the shared model has no retention field")
	}
	return settings.Accessor[int]{
		Get: func(cfg *config.Config) int {
			value, err := field.Read(cfg)
			if err != nil {
				return 0
			}
			days, err := onboarding.IntToken(value)
			if err != nil {
				return 0
			}
			return days
		},
		Set: func(cfg *config.Config, days int) {
			_ = field.Write(cfg, onboarding.WriteContext{Config: cfg}, onboarding.IntValue(days))
		},
	}
}

func adaptExample(field onboarding.Field) settings.GuideExampleFunc {
	return func(_ *settings.Draft) ([]settings.GuideExampleLine, error) {
		lines, err := field.Example(onboarding.WriteContext{})
		if err != nil {
			return nil, err
		}
		out := make([]settings.GuideExampleLine, len(lines))
		for i, line := range lines {
			kind, err := exampleKind(line.Kind)
			if err != nil {
				return nil, err
			}
			out[i] = settings.GuideExampleLine{Kind: kind, Text: line.Text}
		}
		return out, nil
	}
}

func exampleKind(kind onboarding.ExampleLineKind) (settings.GuideExampleLineKind, error) {
	switch kind {
	case onboarding.ExampleLineText:
		return settings.GuideExampleLineText, nil
	case onboarding.ExampleLineLabel:
		return settings.GuideExampleLineLabel, nil
	case onboarding.ExampleLineBefore:
		return settings.GuideExampleLineBefore, nil
	case onboarding.ExampleLineAfter:
		return settings.GuideExampleLineAfter, nil
	case onboarding.ExampleLineSpacer:
		return settings.GuideExampleLineSpacer, nil
	default:
		return settings.GuideExampleLineUnknown, fmt.Errorf(
			"adapt privacy example line.\nwhat: kind %s has no guide role.\nwhy: the example renderer returned a kind the settings guide does not draw.\nwhere: kickstart example adapter.\nwhen: a privacy example was prepared.\nmeans: the example was withheld.\nfix: return text, label, before, after, or spacer.",
			kind)
	}
}

func selectionTreeOptions(opts Options) []settings.TreeOption {
	out := []settings.TreeOption{
		settings.WithFacet(settings.MetaHarness, "harness"),
		settings.WithFacetDisplay(harnessFacetLabel),
		settings.WithSelectAllHelp("toggle all"),
		settings.WithDraftSelectionState(),
		settings.WithCompactFooter(),
		settings.WithCollapseSessionlessRoots(),
	}
	if opts.Preview != nil {
		out = append(out,
			settings.WithPreviewBodySource(opts.Preview),
			settings.WithPreviewRatio(0.5),
		)
	}
	return out
}

func harnessDisplayName(harness string) string {
	return strings.ToLower(schema.HarnessDisplayName(defaults.Harness(harness)))
}

func harnessFacetLabel(harness string) string { return harnessDisplayName(harness) }

func adaptNextSteps(steps []onboarding.NextStep) []NextStepKind {
	out := make([]NextStepKind, 0, len(steps))
	for _, step := range steps {
		kind, ok := kickstartNextStep(step)
		if !ok {
			return []NextStepKind{NextStepUnknown}
		}
		out = append(out, kind)
	}
	return out
}

func kickstartNextStep(step onboarding.NextStep) (NextStepKind, bool) {
	switch step {
	case onboarding.NextStepConfig:
		return NextStepConfig, true
	case onboarding.NextStepWebStart:
		return NextStepWebStart, true
	case onboarding.NextStepVillageLogin:
		return NextStepVillageLogin, true
	case onboarding.NextStepVillagePush:
		return NextStepVillagePush, true
	default:
		return NextStepUnknown, false
	}
}
