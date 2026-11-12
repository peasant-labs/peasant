package onboarding

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// FieldKey is one authored setting. It is stable across presentations.
type FieldKey string

const (
	FieldSelection   FieldKey = "transcripts"
	FieldAutoIngest  FieldKey = "auto-ingest-new-branches"
	FieldPublication FieldKey = "publication-preference"
	FieldAutoPublish FieldKey = "publish-automatically"
	FieldPrivacy     FieldKey = "redaction-level"
	FieldLicense     FieldKey = "content-license"
	FieldVisibility  FieldKey = "default-visibility"
	FieldRetention   FieldKey = "claude-retention-days"
	FieldVillage     FieldKey = "village-connect"
	FieldReview      FieldKey = "review-notice"
	FieldIngest      FieldKey = "ingest-notice"
)

// AllFieldKeys is the closed set in flow order.
var AllFieldKeys = []FieldKey{
	FieldVillage,
	FieldSelection,
	FieldAutoIngest,
	FieldPublication,
	FieldAutoPublish,
	FieldPrivacy,
	FieldLicense,
	FieldVisibility,
	FieldRetention,
	FieldReview,
	FieldIngest,
}

// NewFieldKey refuses an unknown key.
func NewFieldKey(s string) (FieldKey, error) {
	key := FieldKey(s)
	if !key.IsValid() {
		return "", fmt.Errorf(
			"read setup field %q.\n"+
				"what: %q is not a setup field.\n"+
				"why: the closed set is the fields the setup model declares.\n"+
				"where: onboarding.NewFieldKey.\n"+
				"when: a caller named a field.\n"+
				"means: the field was not accepted.\n"+
				"fix: use one of %s.",
			s, s, joinKeys(AllFieldKeys))
	}
	return key, nil
}

// IsValid reports whether k is a declared field.
func (k FieldKey) IsValid() bool {
	for _, key := range AllFieldKeys {
		if key == k {
			return true
		}
	}
	return false
}

// String returns the stable token, or "unknown".
func (k FieldKey) String() string {
	if k.IsValid() {
		return string(k)
	}
	return "unknown"
}

// FieldKind is the closed set of field shapes. A presentation switches on it.
type FieldKind uint8

const (
	FieldKindUnknown FieldKind = iota
	FieldKindToggle
	FieldKindChoice
	FieldKindAction
	FieldKindTree
	FieldKindNotice
)

// AllFieldKinds is the closed set.
var AllFieldKinds = []FieldKind{
	FieldKindToggle,
	FieldKindChoice,
	FieldKindAction,
	FieldKindTree,
	FieldKindNotice,
}

// NewFieldKind accepts toggle, choice, action, tree, or notice.
func NewFieldKind(s string) (FieldKind, error) {
	for _, kind := range AllFieldKinds {
		if kind.String() == s {
			return kind, nil
		}
	}
	return FieldKindUnknown, fmt.Errorf(
		"read field kind %q.\n"+
			"what: %q is not a field kind.\n"+
			"why: the closed set is toggle, choice, action, tree, and notice.\n"+
			"where: onboarding.NewFieldKind.\n"+
			"when: a caller named a field shape.\n"+
			"means: the kind was not accepted.\n"+
			"fix: use toggle, choice, action, tree, or notice.",
		s, s)
}

// IsValid reports whether k is a real field kind.
func (k FieldKind) IsValid() bool {
	for _, kind := range AllFieldKinds {
		if kind == k {
			return true
		}
	}
	return false
}

// String returns the wire token.
func (k FieldKind) String() string {
	switch k {
	case FieldKindToggle:
		return "toggle"
	case FieldKindChoice:
		return "choice"
	case FieldKindAction:
		return "action"
	case FieldKindTree:
		return "tree"
	case FieldKindNotice:
		return "notice"
	default:
		return "unknown"
	}
}

// Destination is where a field's value is stored.
type Destination uint8

const (
	DestUnknown Destination = iota
	DestConfig
	DestNone
	DestClaudeSettings
)

// AllDestinations is the closed set.
var AllDestinations = []Destination{DestConfig, DestNone, DestClaudeSettings}

// IsValid reports whether d is a real destination.
func (d Destination) IsValid() bool {
	for _, destination := range AllDestinations {
		if destination == d {
			return true
		}
	}
	return false
}

// String returns the wire token.
func (d Destination) String() string {
	switch d {
	case DestConfig:
		return "config"
	case DestNone:
		return "none"
	case DestClaudeSettings:
		return "claude-settings"
	default:
		return "unknown"
	}
}

// NewDestination accepts config, none, or claude-settings.
func NewDestination(s string) (Destination, error) {
	for _, destination := range AllDestinations {
		if destination.String() == s {
			return destination, nil
		}
	}
	return DestUnknown, fmt.Errorf(
		"read field destination %q.\n"+
			"what: %q is not a field destination.\n"+
			"why: the closed set is config, none, and claude-settings.\n"+
			"where: onboarding.NewDestination.\n"+
			"when: a caller named where a field is stored.\n"+
			"means: the destination was not accepted.\n"+
			"fix: use config, none, or claude-settings.",
		s, s)
}

// Accessor reads and writes one typed configuration value.
type Accessor[T any] struct {
	Get func(*config.Config) T
	Set func(*config.Config, T)
}

// Lens binds a typed accessor to the document path it writes. The path is nil
// for a field that does not write config.yaml.
type Lens[T any] struct {
	Accessor[T]
	Path []string
}

// EnumOption is one typed choice before it is projected to a wire token.
type EnumOption[T any] struct {
	Value       T
	Label       string
	Description string
}

// Option is one choice. Value is the stable wire token.
type Option struct {
	Value       string
	Label       string
	Description string
}

// ActionOption is one labeled action on an action field. Token is the wire value.
type ActionOption struct {
	Token string
	Label string
}

// ExampleLineKind is the semantic role of one privacy-example row.
type ExampleLineKind uint8

const (
	ExampleLineUnknown ExampleLineKind = iota
	ExampleLineText
	ExampleLineLabel
	ExampleLineBefore
	ExampleLineAfter
	ExampleLineSpacer
)

// AllExampleLineKinds is the closed set.
var AllExampleLineKinds = []ExampleLineKind{
	ExampleLineText,
	ExampleLineLabel,
	ExampleLineBefore,
	ExampleLineAfter,
	ExampleLineSpacer,
}

// IsValid reports whether k is a real example role.
func (k ExampleLineKind) IsValid() bool {
	for _, kind := range AllExampleLineKinds {
		if kind == k {
			return true
		}
	}
	return false
}

// String returns the stable name.
func (k ExampleLineKind) String() string {
	switch k {
	case ExampleLineText:
		return "text"
	case ExampleLineLabel:
		return "label"
	case ExampleLineBefore:
		return "before"
	case ExampleLineAfter:
		return "after"
	case ExampleLineSpacer:
		return "spacer"
	default:
		return "unknown"
	}
}

// ExampleLine is one plain-text example row. The presentation owns glyphs and color.
type ExampleLine struct {
	Kind ExampleLineKind
	Text string
}

// WriteContext is everything a write, a validator, or a condition may need
// beyond the value itself. Config is the same pointer as the cfg argument of
// Write and ApplyAnswers in every production call.
type WriteContext struct {
	Config           *config.Config
	Answers          map[FieldKey]schema.LocalSettingValue
	Forest           Forest
	Residual         Residual
	VillageConnected bool
	LoginAvailable   bool
	ConnectAnswer    ConnectAnswer
	ClaudeSessions   bool
	ScanState        ScanState
	ScanErr          error
	LockedNodes      int
	DiscoveredRoots  int
	SelectedRoots    int
}

// Field is one authored setting. Visibility is data on Conditions, not a method
// that a presentation can replace.
type Field interface {
	Key() FieldKey
	Kind() FieldKind
	Label() string
	Description() string
	Destination() Destination
	YAMLPath() []string
	Options() []Option
	Conditions() []Condition
	Read(*config.Config) (schema.LocalSettingValue, error)
	Write(*config.Config, WriteContext, schema.LocalSettingValue) error
	Restore(*config.Config, *config.Config) error
	Validate(WriteContext) error
	Example(WriteContext) ([]ExampleLine, error)
}

type field struct {
	key         FieldKey
	kind        FieldKind
	label       string
	description string
	destination Destination
	path        []string
	options     []Option
	conditions  []Condition
	restore     func(*config.Config, *config.Config) error
	read        func(*config.Config) (schema.LocalSettingValue, error)
	write       func(*config.Config, WriteContext, schema.LocalSettingValue) error
	validate    func(WriteContext) error
	example     func(WriteContext) ([]ExampleLine, error)
}

func (f *field) Key() FieldKey            { return f.key }
func (f *field) Kind() FieldKind          { return f.kind }
func (f *field) Label() string            { return f.label }
func (f *field) Description() string      { return f.description }
func (f *field) Destination() Destination { return f.destination }
func (f *field) YAMLPath() []string       { return append([]string(nil), f.path...) }
func (f *field) Options() []Option        { return append([]Option(nil), f.options...) }
func (f *field) Conditions() []Condition  { return append([]Condition(nil), f.conditions...) }

func (f *field) Read(cfg *config.Config) (schema.LocalSettingValue, error) {
	if f.read == nil {
		return schema.LocalSettingNull(), nil
	}
	return f.read(cfg)
}

func (f *field) Write(cfg *config.Config, w WriteContext, value schema.LocalSettingValue) error {
	if f.write == nil {
		return writeRefused(f, "this field writes no configuration value")
	}
	w.Config = cfg
	return f.write(cfg, w, value)
}

// Restore copies an already stored choice without treating it as a new answer.
// Stored values can be unset or no longer offered; dropping an edit must keep them.
func (f *field) Restore(dst, src *config.Config) error {
	if dst == nil || src == nil || f.restore == nil {
		return writeRefused(f, "the field cannot restore its stored value")
	}
	return f.restore(dst, src)
}

func (f *field) Validate(w WriteContext) error {
	if f.validate == nil {
		return nil
	}
	return f.validate(w)
}

func (f *field) Example(w WriteContext) ([]ExampleLine, error) {
	if f.example == nil {
		return nil, nil
	}
	return f.example(w)
}

func (f *field) chainWrite(hook func(*config.Config, WriteContext, schema.LocalSettingValue) error) *field {
	previous := f.write
	f.write = func(cfg *config.Config, w WriteContext, value schema.LocalSettingValue) error {
		if previous != nil {
			if err := previous(cfg, w, value); err != nil {
				return err
			}
		}
		if hook == nil {
			return nil
		}
		return hook(cfg, w, value)
	}
	return f
}

// Toggle builds a boolean field bound to l.
func Toggle(key FieldKey, label, desc string, l Lens[bool]) Field {
	f := &field{
		key:         key,
		kind:        FieldKindToggle,
		label:       label,
		description: desc,
		path:        append([]string(nil), l.Path...),
		destination: destinationFor(l.Path),
		read: func(cfg *config.Config) (schema.LocalSettingValue, error) {
			if l.Get == nil || cfg == nil {
				return BoolValue(false), nil
			}
			return BoolValue(l.Get(cfg)), nil
		},
		write: func(cfg *config.Config, _ WriteContext, value schema.LocalSettingValue) error {
			on, err := BoolToken(value)
			if err != nil {
				return writeValueError(key, err)
			}
			if l.Set == nil {
				return writeRefused(keyField(key), "the toggle has no setter")
			}
			l.Set(cfg, on)
			return nil
		},
	}
	return f
}

// Choice builds a single-choice field whose wire token is the string form of T.
func Choice[T ~string](key FieldKey, label, desc string, l Lens[T], opts []EnumOption[T]) Field {
	return newChoice(key, label, desc, l, opts)
}

func newChoice[T ~string](key FieldKey, label, desc string, l Lens[T], opts []EnumOption[T]) *field {
	options := make([]Option, len(opts))
	allowed := make(map[string]T, len(opts))
	for i, opt := range opts {
		token := string(opt.Value)
		options[i] = Option{Value: token, Label: opt.Label, Description: opt.Description}
		allowed[token] = opt.Value
	}
	return &field{
		key:         key,
		kind:        FieldKindChoice,
		label:       label,
		description: desc,
		path:        append([]string(nil), l.Path...),
		destination: destinationFor(l.Path),
		options:     options,
		restore: func(dst, src *config.Config) error {
			if l.Get == nil || l.Set == nil {
				return writeRefused(keyField(key), "the choice has no accessor")
			}
			l.Set(dst, l.Get(src))
			return nil
		},
		read: func(cfg *config.Config) (schema.LocalSettingValue, error) {
			if l.Get == nil || cfg == nil {
				return StringValue(""), nil
			}
			return StringValue(string(l.Get(cfg))), nil
		},
		write: func(cfg *config.Config, _ WriteContext, value schema.LocalSettingValue) error {
			token, err := StringToken(value)
			if err != nil {
				return writeValueError(key, err)
			}
			typed, ok := allowed[token]
			if !ok && len(allowed) > 0 {
				return writeRefused(keyField(key), fmt.Sprintf("token %q is not one of the field's options", token))
			}
			if l.Set == nil {
				return writeRefused(keyField(key), "the choice has no setter")
			}
			if !ok {
				typed = T(token)
			}
			l.Set(cfg, typed)
			return nil
		},
	}
}

// IntChoice builds a single-choice field whose wire token is the decimal integer.
func IntChoice(key FieldKey, label, desc string, l Lens[int], opts []EnumOption[int]) Field {
	options := make([]Option, len(opts))
	allowed := map[string]int{}
	for i, opt := range opts {
		token := strconv.Itoa(opt.Value)
		options[i] = Option{Value: token, Label: opt.Label, Description: opt.Description}
		allowed[token] = opt.Value
	}
	return &field{
		key:         key,
		kind:        FieldKindChoice,
		label:       label,
		description: desc,
		path:        append([]string(nil), l.Path...),
		destination: destinationFor(l.Path),
		options:     options,
		restore: func(dst, src *config.Config) error {
			if l.Get == nil || l.Set == nil {
				return writeRefused(keyField(key), "the choice has no accessor")
			}
			l.Set(dst, l.Get(src))
			return nil
		},
		read: func(cfg *config.Config) (schema.LocalSettingValue, error) {
			if l.Get == nil || cfg == nil {
				return IntValue(0), nil
			}
			return IntValue(l.Get(cfg)), nil
		},
		write: func(cfg *config.Config, _ WriteContext, value schema.LocalSettingValue) error {
			number, err := IntToken(value)
			if err != nil {
				token, tokenErr := StringToken(value)
				if tokenErr != nil {
					return writeValueError(key, err)
				}
				parsed, parseErr := strconv.Atoi(token)
				if parseErr != nil {
					return writeValueError(key, err)
				}
				number = parsed
			}
			if _, ok := allowed[strconv.Itoa(number)]; !ok && len(allowed) > 0 {
				return writeRefused(keyField(key), fmt.Sprintf("%d is not one of the field's options", number))
			}
			if l.Set == nil {
				return writeRefused(keyField(key), "the choice has no setter")
			}
			l.Set(cfg, number)
			return nil
		},
	}
}

// Action builds a field that writes no configuration. opts are the labeled actions.
func Action(key FieldKey, label, desc string, opts ...ActionOption) Field {
	options := make([]Option, len(opts))
	for i, opt := range opts {
		options[i] = Option{Value: opt.Token, Label: opt.Label}
	}
	return &field{
		key:         key,
		kind:        FieldKindAction,
		label:       label,
		description: desc,
		destination: DestNone,
		options:     options,
	}
}

// Notice builds a read-only field. body is the notice text, returned as its description.
func Notice(key FieldKey, label, body string) Field {
	return &field{
		key:         key,
		kind:        FieldKindNotice,
		label:       label,
		description: body,
		destination: DestNone,
	}
}

// SelectionTree builds the selection field. A mapping answer writes mode and
// harnesses, and sets auto-ingest when mode is all. An id answer calls
// the shared selection derivation directly.
func SelectionTree(key FieldKey, label string) Field {
	return &field{
		key:         key,
		kind:        FieldKindTree,
		label:       label,
		destination: DestConfig,
		path:        []string{"selection"},
		read:        readSelection,
		write:       writeSelection,
		validate:    validateSelection,
	}
}

// SelectionAnswer is the tree field's JSON value. A mapping answer carries mode
// and harnesses. An id answer carries node ids and is derived.
type SelectionAnswer struct {
	Mode      string                                   `json:"mode,omitempty"`
	Harnesses map[string]config.SelectionHarnessConfig `json:"harnesses,omitempty"`
	IDs       []string                                 `json:"ids,omitempty"`
}

func readSelection(cfg *config.Config) (schema.LocalSettingValue, error) {
	if cfg == nil {
		return SelectionMappingValue("", nil)
	}
	return SelectionMappingValue(cfg.Selection.Mode, cfg.Selection.Harnesses)
}

func writeSelection(cfg *config.Config, w WriteContext, value schema.LocalSettingValue) error {
	if cfg == nil {
		return writeRefused(keyField(FieldSelection), "the configuration pointer is nil")
	}
	var answer SelectionAnswer
	if err := json.Unmarshal(value, &answer); err != nil {
		return writeValueError(FieldSelection, err)
	}
	if ids, isIDAnswer := SelectionIDs(value); isIDAnswer {
		derived, err := deriveSelection(w.Forest, SelectedForest{IDs: ids}, cfg.Selection.AutoIngestNewBranches, w.Residual)
		if err != nil {
			return err
		}
		if derived.Mode == config.SelectionModeAll {
			derived.AutoIngestNewBranches = true
		}
		cfg.Selection = derived
		return nil
	}
	mode := config.SelectionMode(answer.Mode)
	if mode != "" && !mode.IsValid() {
		return writeRefused(keyField(FieldSelection), fmt.Sprintf("mode %q is not all or selected", answer.Mode))
	}
	cfg.Selection.Mode = mode
	cfg.Selection.Harnesses = answer.Harnesses
	if mode == config.SelectionModeAll {
		cfg.Selection.AutoIngestNewBranches = true
	}
	return nil
}

func validateSelection(w WriteContext) error {
	if w.ScanState != ScanDone {
		return actionable(
			"validate the selection",
			"the discovery scan is not done",
			"onboarding selection field",
			"before the selection can be saved",
			"the selection was not accepted",
			"wait until the scan finishes, then save again")
	}
	if w.LockedNodes != 0 {
		return actionable(
			"validate the selection",
			fmt.Sprintf("%d locked rows are still in the tree", w.LockedNodes),
			"onboarding selection field",
			"before the selection can be saved",
			"a locked row was not treated as unselected",
			"resolve the ambiguous or unroutable rows, then save again")
	}
	sel := config.SelectionConfig{}
	if w.Config != nil {
		sel = w.Config.Selection
	}
	if ids := answeredIDs(w); len(ids) > 0 {
		auto := sel.AutoIngestNewBranches
		derived, err := deriveSelection(w.Forest, SelectedForest{IDs: ids}, auto, w.Residual)
		if err != nil {
			return err
		}
		sel = derived
	}
	if err := config.ValidateSelection(sel); err != nil {
		return actionable(
			"validate the selection",
			err.Error(),
			"onboarding selection field",
			"before the selection can be saved",
			"the derived selection was not written",
			"correct the selection so it passes configuration validation")
	}
	return nil
}

func answeredIDs(w WriteContext) []string {
	if w.Answers == nil {
		return nil
	}
	ids, _ := SelectionIDs(w.Answers[FieldSelection])
	return ids
}

func destinationFor(path []string) Destination {
	if len(path) == 0 {
		return DestNone
	}
	return DestConfig
}

func keyField(key FieldKey) Field {
	return &field{key: key}
}

func writeRefused(field Field, why string) error {
	name := "field"
	if field != nil && field.Key() != "" {
		name = field.Key().String()
	}
	return actionable(
		"write "+name,
		why,
		"onboarding field "+name,
		"while applying an answer",
		"the field was not written",
		"apply a value this field accepts")
}

func writeValueError(key FieldKey, err error) error {
	return actionable(
		"write "+key.String(),
		err.Error(),
		"onboarding field "+key.String(),
		"while decoding an answer",
		"the field was not written",
		"send the JSON type this field reads")
}

// NeverExpireDays is the Claude Code cleanup period that keeps transcripts.
const NeverExpireDays = 99999

// neverExpireFloor is the smallest stored cleanup period that reads as "keeps
// transcripts forever". Claude Code accepts any large number, so a value that a
// person or another tool wrote below NeverExpireDays still means never expire.
const neverExpireFloor = 9999

// NeverExpires reports whether a stored cleanup period keeps transcripts
// forever. A presentation uses it to read an existing value. It writes
// NeverExpireDays.
func NeverExpires(days int) bool { return days >= neverExpireFloor }

// RecommendedRetentionDays is the retention choice a first run starts on when
// Claude Code has no cleanup period set.
const RecommendedRetentionDays = NeverExpireDays

func privacyExample(WriteContext) ([]ExampleLine, error) {
	return renderPrivacyExample(redact.Standard)
}

func renderPrivacyExample(level redact.RedactionLevel) ([]ExampleLine, error) {
	samples := []struct {
		category redact.Category
		before   string
	}{
		{redact.CategorySecrets, "sk-ant-api03-EXAMPLEKEY0000000000000"},
		{redact.CategoryPII, "user@example.com"},
		{redact.CategoryPaths, "/Users/example/projects/sample/"},
		{redact.CategoryProject, `"github.com/example-org/example-repo"`},
	}
	redactor, err := redact.NewRedactor(level, nil, redact.XDGPaths{})
	if err != nil {
		return nil, privacyExampleError(
			fmt.Sprintf("the %s privacy example redactor could not be constructed", level),
			err.Error(),
			"repair the redactor configuration or constructor before showing privacy claims")
	}
	claimed := redact.AllCategories()
	seen := map[redact.Category]int{}
	for _, sample := range samples {
		if err := sample.category.Validate(); err != nil {
			return nil, privacyExampleError(
				fmt.Sprintf("synthetic privacy sample declares unknown category %q", sample.category),
				err.Error(),
				"assign the sample to secrets, pii, paths, or project before rendering the guide")
		}
		seen[sample.category]++
	}
	for _, category := range claimed {
		if seen[category] != 1 {
			return nil, privacyExampleError(
				fmt.Sprintf("privacy guidance has %d samples for claimed category %s", seen[category], category),
				"every claimed category must have exactly one independent demonstration",
				"restore one safe synthetic sample for each category returned by redact.AllCategories")
		}
	}
	lines := make([]ExampleLine, 0, len(samples)*3)
	for _, sample := range samples {
		label := sample.category.String()
		if label.String() == "" {
			return nil, privacyExampleError(
				fmt.Sprintf("privacy guidance has no canonical label for category %q", sample.category),
				"the validated category resolved to an empty public label",
				"restore the canonical category mapping")
		}
		matches := redactor.Detect(sample.before)
		found := false
		for _, match := range matches {
			found = found || match.Category == sample.category
		}
		if !found {
			return nil, privacyExampleError(
				fmt.Sprintf("the synthetic %s sample did not trigger its claimed category", sample.category),
				"the real redactor detection result contains no matching category",
				"update the safe sample if its rule shape changed, or repair the missing redaction rule")
		}
		after := redactor.RedactText(sample.before)
		if after == sample.before {
			return nil, privacyExampleError(
				fmt.Sprintf("the synthetic %s sample remained unchanged at the %s level", sample.category, level),
				"detection claimed the category but the real redaction output did not change",
				"repair the redactor replacement behavior instead of hard-coding example output")
		}
		heading := strings.ToLower(label.String())
		if sample.category == redact.CategoryPII {
			heading = "personally identifiable information (PII)"
		}
		lines = append(lines,
			ExampleLine{Kind: ExampleLineLabel, Text: heading},
			ExampleLine{Kind: ExampleLineBefore, Text: sample.before},
			ExampleLine{Kind: ExampleLineAfter, Text: after},
		)
	}
	return lines, nil
}

func privacyExampleError(what, why, fix string) error {
	return actionable(
		what,
		why,
		"onboarding privacy example",
		"while preparing synthetic before and after examples",
		"no example output is shown, so setup does not claim an unverified privacy result",
		fix)
}

func actionable(what, why, where, when, means, fix string) error {
	return fmt.Errorf(
		"%s.\nwhat: %s.\nwhy: %s.\nwhere: %s.\nwhen: %s.\nmeans: %s.\nfix: %s.",
		what, what, why, where, when, means, fix)
}
