// Package onboarding is the one definition of kickstart and settings setup:
// stop order, visibility, option menus, copy, consent, and the answers that
// write a configuration. Presentations render it. They do not restate it.
package onboarding

import (
	"fmt"
	"slices"
)

// FlowKey is one stop in the setup flow. The eight configuration sections and
// the three phases share this set so a stop has one identity.
type FlowKey string

const (
	FlowSelection   FlowKey = "selection"
	FlowAutoIngest  FlowKey = "auto-ingest"
	FlowPublication FlowKey = "publication"
	FlowAutoPublish FlowKey = "auto-publish"
	FlowPrivacy     FlowKey = "privacy"
	FlowLicense     FlowKey = "license"
	FlowDestination FlowKey = "destination"
	FlowRetention   FlowKey = "retention"
	FlowVillage     FlowKey = "village"
	FlowReview      FlowKey = "review"
	FlowIngest      FlowKey = "ingest"
)

// AllFlowKeys is the closed set, sections first in flow order, then phases
// in the order a full wizard meets them is not this slice: flow order is
// Model.Stops. This slice is the accepted set.
var AllFlowKeys = []FlowKey{
	FlowSelection,
	FlowAutoIngest,
	FlowPublication,
	FlowAutoPublish,
	FlowPrivacy,
	FlowLicense,
	FlowDestination,
	FlowRetention,
	FlowVillage,
	FlowReview,
	FlowIngest,
}

// NewFlowKey accepts exactly the eight sections and three phases.
func NewFlowKey(s string) (FlowKey, error) {
	key := FlowKey(s)
	if !key.IsValid() {
		return "", fmt.Errorf(
			"read setup stop %q.\n"+
				"what: %q is not a setup stop.\n"+
				"why: the closed set is the eight configuration sections plus village, review, and ingest.\n"+
				"where: onboarding.NewFlowKey.\n"+
				"when: a caller named a stop before the model was built.\n"+
				"means: no stop was selected.\n"+
				"fix: use one of %s.",
			s, s, joinKeys(AllFlowKeys))
	}
	return key, nil
}

// IsValid reports whether k is one of the eleven stops.
func (k FlowKey) IsValid() bool { return slices.Contains(AllFlowKeys, k) }

// String returns the stable token, or "unknown".
func (k FlowKey) String() string {
	if k.IsValid() {
		return string(k)
	}
	return "unknown"
}

// Section reports the configuration-section identity of k.
func (k FlowKey) Section() (SectionKey, bool) {
	section := SectionKey(k)
	if section.IsValid() {
		return section, true
	}
	return "", false
}

// Phase reports the phase identity of k.
func (k FlowKey) Phase() (PhaseKey, bool) {
	phase := PhaseKey(k)
	if phase.IsValid() {
		return phase, true
	}
	return "", false
}

// Kind is derived from the key. A stop does not store a second kind that can disagree.
func (k FlowKey) Kind() StopKind {
	switch k {
	case FlowVillage:
		return StopConnection
	case FlowReview:
		return StopReview
	case FlowIngest:
		return StopIngest
	default:
		if _, ok := k.Section(); ok {
			return StopConfig
		}
		return StopUnknown
	}
}

// SectionKey is one of the eight configuration sections a settings presentation mounts.
type SectionKey string

const (
	SectionSelection   SectionKey = SectionKey(FlowSelection)
	SectionAutoIngest  SectionKey = SectionKey(FlowAutoIngest)
	SectionPublication SectionKey = SectionKey(FlowPublication)
	SectionAutoPublish SectionKey = SectionKey(FlowAutoPublish)
	SectionPrivacy     SectionKey = SectionKey(FlowPrivacy)
	SectionLicense     SectionKey = SectionKey(FlowLicense)
	SectionDestination SectionKey = SectionKey(FlowDestination)
	SectionRetention   SectionKey = SectionKey(FlowRetention)
)

// AllSectionKeys is the eight sections in flow order.
var AllSectionKeys = []SectionKey{
	SectionSelection,
	SectionAutoIngest,
	SectionPublication,
	SectionAutoPublish,
	SectionPrivacy,
	SectionLicense,
	SectionDestination,
	SectionRetention,
}

// IsValid reports whether k is one of the eight sections.
func (k SectionKey) IsValid() bool { return slices.Contains(AllSectionKeys, k) }

// String returns the stable token, or "unknown".
func (k SectionKey) String() string {
	if k.IsValid() {
		return string(k)
	}
	return "unknown"
}

// PhaseKey is a non-configuration stop: village, review, or ingest.
type PhaseKey string

const (
	PhaseVillage PhaseKey = PhaseKey(FlowVillage)
	PhaseReview  PhaseKey = PhaseKey(FlowReview)
	PhaseIngest  PhaseKey = PhaseKey(FlowIngest)
)

// AllPhaseKeys is the closed phase set.
var AllPhaseKeys = []PhaseKey{PhaseVillage, PhaseReview, PhaseIngest}

// IsValid reports whether k is a phase.
func (k PhaseKey) IsValid() bool { return slices.Contains(AllPhaseKeys, k) }

// String returns the stable token, or "unknown".
func (k PhaseKey) String() string {
	if k.IsValid() {
		return string(k)
	}
	return "unknown"
}

// StopKind is the role of a stop, derived from its FlowKey.
type StopKind uint8

const (
	StopUnknown StopKind = iota
	StopConnection
	StopConfig
	StopReview
	StopIngest
)

// AllStopKinds is the closed set, excluding the unknown zero value.
var AllStopKinds = []StopKind{StopConnection, StopConfig, StopReview, StopIngest}

// IsValid reports whether k names a real stop role.
func (k StopKind) IsValid() bool { return slices.Contains(AllStopKinds, k) }

// String returns the wire token.
func (k StopKind) String() string {
	switch k {
	case StopConnection:
		return "connection"
	case StopConfig:
		return "config"
	case StopReview:
		return "review"
	case StopIngest:
		return "ingest"
	default:
		return "unknown"
	}
}

// NewStopKind accepts a wire token.
func NewStopKind(s string) (StopKind, error) {
	for _, kind := range AllStopKinds {
		if kind.String() == s {
			return kind, nil
		}
	}
	return StopUnknown, fmt.Errorf(
		"read stop kind %q.\n"+
			"what: %q is not a stop kind.\n"+
			"why: the closed set is connection, config, review, and ingest.\n"+
			"where: onboarding.NewStopKind.\n"+
			"when: a caller named a stop role.\n"+
			"means: no kind was selected.\n"+
			"fix: use one of connection, config, review, ingest.",
		s, s)
}

// ModelScope is the closed set of answer sets a session collects.
//
//	ScopeFull      - the whole wizard: 8 sections plus village, review, and ingest
//	ScopeSelection - the standalone selection page: selection, review, and ingest only
//
// The scope decides which stops exist, which fields can be answered, and
// therefore what a commit writes. Nothing else changes between them.
type ModelScope uint8

const (
	ScopeUnknown ModelScope = iota
	ScopeFull
	ScopeSelection
)

// AllModelScopes is the closed set, excluding the unknown zero value.
var AllModelScopes = []ModelScope{ScopeFull, ScopeSelection}

// NewModelScope refuses an unknown token. The accepted tokens are "full" and "selection".
func NewModelScope(s string) (ModelScope, error) {
	switch s {
	case ScopeFull.String():
		return ScopeFull, nil
	case ScopeSelection.String():
		return ScopeSelection, nil
	default:
		return ScopeUnknown, fmt.Errorf(
			"read setup scope %q.\n"+
				"what: %q is not a setup scope.\n"+
				"why: the closed set is full and selection.\n"+
				"where: onboarding.NewModelScope.\n"+
				"when: a caller opened a setup session.\n"+
				"means: no model was built.\n"+
				"fix: use full or selection.",
			s, s)
	}
}

// IsValid reports whether s is full or selection.
func (s ModelScope) IsValid() bool { return slices.Contains(AllModelScopes, s) }

// String returns "full", "selection", or "unknown".
func (s ModelScope) String() string {
	switch s {
	case ScopeFull:
		return "full"
	case ScopeSelection:
		return "selection"
	default:
		return "unknown"
	}
}

// Guide is the presentation-neutral framing of a stop. A guided presentation
// renders it. A dense presentation may ignore it. It never changes field
// identity, visibility, or persistence.
type Guide struct {
	Intro              string
	Hints              []string
	ExampleInSplitPane bool
}

// Section is the payload of a configuration stop. It holds no key: the stop's
// FlowKey is the key, so the two cannot disagree.
type Section struct{}

// Phase is the payload of a village, review, or ingest stop. It holds no key.
type Phase struct{}

// Stop is one step. Key is its only identity. Section is set when Key is a
// configuration section; Phase is set otherwise. Fields on a phase are action
// or notice fields. Visible is the stop's show/hide data.
type Stop struct {
	Key     FlowKey
	Title   string
	Guide   Guide
	Section *Section
	Phase   *Phase
	Fields  []Field
	Visible []Condition
}

// Shown reports whether every visibility condition holds. An empty list means
// the stop is always shown.
func (s Stop) Shown(w WriteContext) bool {
	return ConditionsHold(s.Visible, w)
}

// Options carries what BuildModel needs. Runtime facts (connection, scan,
// answers) belong on WriteContext, not here.
type Options struct {
	Scope ModelScope
}

// Model is the one definition every presentation consumes. Stops are in flow
// order. The scope decides which of them exist.
type Model struct {
	Scope  ModelScope
	Stops  []Stop
	offers []ConnectOffer
}

// BuildModel returns the setup model for opts.Scope. A duplicate YAML path, or
// an unknown scope, is a programming error and panics with an actionable
// message. Callers that assemble stops themselves use ModelFromStops, which
// returns that refusal as an error.
func BuildModel(opts Options) Model {
	if !opts.Scope.IsValid() {
		panic(fmt.Errorf(
			"build the setup model.\n"+
				"what: scope %q is not full or selection.\n"+
				"why: BuildModel only builds a closed scope.\n"+
				"where: onboarding.BuildModel.\n"+
				"when: the model was requested.\n"+
				"means: no model was returned.\n"+
				"fix: pass ScopeFull or ScopeSelection.",
			opts.Scope))
	}
	model, err := ModelFromStops(opts.Scope, stopsFor(opts.Scope), offersFor(opts.Scope))
	if err != nil {
		panic(err)
	}
	return model
}

// ModelFromStops checks the stop list and returns a model. BuildModel uses it
// and panics on the same refusal: two fields must not share a YAML path.
func ModelFromStops(scope ModelScope, stops []Stop, offers []ConnectOffer) (Model, error) {
	if !scope.IsValid() {
		return Model{}, fmt.Errorf(
			"build the setup model.\n"+
				"what: scope %q is not full or selection.\n"+
				"why: a model must name a closed scope.\n"+
				"where: onboarding.ModelFromStops.\n"+
				"when: stops were assembled.\n"+
				"means: no model was returned.\n"+
				"fix: pass ScopeFull or ScopeSelection.",
			scope)
	}
	if err := refuseDuplicateYAMLPaths(stops); err != nil {
		return Model{}, err
	}
	return Model{Scope: scope, Stops: append([]Stop(nil), stops...), offers: append([]ConnectOffer(nil), offers...)}, nil
}

func refuseDuplicateYAMLPaths(stops []Stop) error {
	seen := map[string]FieldKey{}
	for _, stop := range stops {
		for _, field := range stop.Fields {
			if field == nil {
				continue
			}
			path := field.YAMLPath()
			if len(path) == 0 {
				continue
			}
			key := joinPath(path)
			if previous, ok := seen[key]; ok {
				return fmt.Errorf(
					"build the setup model.\n"+
						"what: fields %s and %s share YAML path %s.\n"+
						"why: each configuration path has one writer, so a later answer cannot silently replace an earlier one.\n"+
						"where: onboarding.BuildModel.\n"+
						"when: the stop list was checked before the model was returned.\n"+
						"means: the model was refused and no answers were applied.\n"+
						"fix: give each field its own YAML path, or drop the duplicate field.",
					previous, field.Key(), key)
			}
			seen[key] = field.Key()
		}
	}
	return nil
}

// Fields returns every field in stop order, then field order.
func (m Model) Fields() []Field {
	var out []Field
	for _, stop := range m.Stops {
		for _, field := range stop.Fields {
			if field != nil {
				out = append(out, field)
			}
		}
	}
	return out
}

// Field returns the field with key.
func (m Model) Field(key FieldKey) (Field, bool) {
	for _, field := range m.Fields() {
		if field.Key() == key {
			return field, true
		}
	}
	return nil, false
}

// StopByKey returns the stop with key.
func (m Model) StopByKey(key FlowKey) (Stop, bool) {
	for _, stop := range m.Stops {
		if stop.Key == key {
			return stop, true
		}
	}
	return Stop{}, false
}

// SectionStops returns the configuration sections in flow order.
func (m Model) SectionStops() []Stop {
	var out []Stop
	for _, stop := range m.Stops {
		if _, ok := stop.Key.Section(); ok {
			out = append(out, stop)
		}
	}
	return out
}

func joinKeys[T interface{ String() string }](keys []T) string {
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = key.String()
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, part := range parts[1:] {
		out += ", " + part
	}
	return out
}

func joinPath(path []string) string {
	out := ""
	for i, part := range path {
		if i > 0 {
			out += "."
		}
		out += part
	}
	return out
}
