// Package upgrade joins the store's producer registries into one declaration
// table, so an upgrade can be planned from what this build declares.
//
// Dependency direction: this package imports store and ingest. Only entry
// points import this package; store and ingest never do.
package upgrade

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// ProducerKind is the closed set of producers a stored session or the store
// itself depends on.
type ProducerKind string

const (
	// ProducerKindAdapter derives a session's metadata from its transcript.
	ProducerKindAdapter ProducerKind = "metadata-adapter"
	// ProducerKindIndexer parses a stored transcript into index rows.
	ProducerKindIndexer ProducerKind = "transcript-indexer"
	// ProducerKindFormat is the representation the indexer's rows are written in.
	ProducerKindFormat ProducerKind = "index-format"
	// ProducerKindMetadataSchema is the shape of the stored metadata sidecar.
	ProducerKindMetadataSchema ProducerKind = "metadata-schema"
	// ProducerKindContentModel is one content capability the store preserves.
	ProducerKindContentModel ProducerKind = "content-model"
	// ProducerKindRedactionRules is the compiled redaction rule set.
	ProducerKindRedactionRules ProducerKind = "redaction-rules"
	// ProducerKindSchema is the store schema the migration chain produces.
	ProducerKindSchema ProducerKind = "store-schema"
)

// AllProducerKinds lists every producer kind in declaration order.
var AllProducerKinds = []ProducerKind{
	ProducerKindAdapter,
	ProducerKindIndexer,
	ProducerKindFormat,
	ProducerKindMetadataSchema,
	ProducerKindContentModel,
	ProducerKindRedactionRules,
	ProducerKindSchema,
}

// NewProducerKind validates a raw producer kind at an input boundary.
func NewProducerKind(raw string) (ProducerKind, error) {
	if kind := ProducerKind(raw); slices.Contains(AllProducerKinds, kind) {
		return kind, nil
	}
	return "", fmt.Errorf("upgrade: read producer kind %q: this build declares no such producer kind; nothing was planned; use one of %q", raw, AllProducerKinds)
}

// PerHarness reports whether the kind has one producer per registered harness.
func (k ProducerKind) PerHarness() bool {
	switch k {
	case ProducerKindAdapter, ProducerKindIndexer, ProducerKindFormat:
		return true
	default:
		return false
	}
}

// PerContentCapability reports whether the kind has one producer per content
// capability in the contract's inventory.
func (k ProducerKind) PerContentCapability() bool { return k == ProducerKindContentModel }

// ChangeScope is the closed set of what a producer's version bump can change.
type ChangeScope string

const (
	// ChangeScopeMetadataOnly changes the metadata derived from an unchanged transcript.
	ChangeScopeMetadataOnly ChangeScope = "metadata-only"
	// ChangeScopeIndexOnly changes the index rows parsed from an unchanged transcript.
	ChangeScopeIndexOnly ChangeScope = "index-only"
	// ChangeScopeIndexRepresentation changes the representation index rows are written in.
	ChangeScopeIndexRepresentation ChangeScope = "index-representation"
	// ChangeScopeMetadataShape changes the shape of the stored metadata sidecar.
	ChangeScopeMetadataShape ChangeScope = "metadata-shape"
	// ChangeScopeContentShape changes which content a stored session can carry.
	ChangeScopeContentShape ChangeScope = "content-shape"
	// ChangeScopePublicationOnly changes only what a publication sends; the
	// stored copy is redacted again on the way out.
	ChangeScopePublicationOnly ChangeScope = "publication-only"
	// ChangeScopeStoreShape changes the store's table shapes.
	ChangeScopeStoreShape ChangeScope = "store-shape"
)

// AllChangeScopes lists every change scope.
var AllChangeScopes = []ChangeScope{
	ChangeScopeMetadataOnly,
	ChangeScopeIndexOnly,
	ChangeScopeIndexRepresentation,
	ChangeScopeMetadataShape,
	ChangeScopeContentShape,
	ChangeScopePublicationOnly,
	ChangeScopeStoreShape,
}

// NewChangeScope validates a raw change scope at an input boundary.
func NewChangeScope(raw string) (ChangeScope, error) {
	if scope := ChangeScope(raw); slices.Contains(AllChangeScopes, scope) {
		return scope, nil
	}
	return "", fmt.Errorf("upgrade: read change scope %q: this build declares no such change scope; nothing was planned; use one of %q", raw, AllChangeScopes)
}

// ProducerInput is the closed set of what a producer reads.
type ProducerInput string

const (
	// ProducerInputNativeSource is the harness's own session files or database.
	ProducerInputNativeSource ProducerInput = "native-source"
	// ProducerInputStoredTranscript is the transcript copy the store already holds.
	ProducerInputStoredTranscript ProducerInput = "stored-transcript"
	// ProducerInputStoreRows is the store's existing rows.
	ProducerInputStoreRows ProducerInput = "store-rows"
)

// AllProducerInputs lists every producer input.
var AllProducerInputs = []ProducerInput{ProducerInputNativeSource, ProducerInputStoredTranscript, ProducerInputStoreRows}

// NewProducerInput validates a raw producer input at an input boundary.
func NewProducerInput(raw string) (ProducerInput, error) {
	if input := ProducerInput(raw); slices.Contains(AllProducerInputs, input) {
		return input, nil
	}
	return "", fmt.Errorf("upgrade: read producer input %q: this build declares no such producer input; nothing was planned; use one of %q", raw, AllProducerInputs)
}

// ProofName is the closed set of stored values that can prove a producer's
// output unchanged without redoing the work.
type ProofName string

const (
	// ProofContentHash is the stored transcript's content hash.
	ProofContentHash ProofName = "content-hash"
	// ProofMetadataContextDigest is the digest of the read-visible metadata.
	ProofMetadataContextDigest ProofName = "metadata-context-digest"
)

// AllProofNames lists every proof.
var AllProofNames = []ProofName{ProofContentHash, ProofMetadataContextDigest}

// NewProofName validates a raw proof name at an input boundary.
func NewProofName(raw string) (ProofName, error) {
	if proof := ProofName(raw); slices.Contains(AllProofNames, proof) {
		return proof, nil
	}
	return "", fmt.Errorf("upgrade: read output proof %q: this build has no reader for such a proof; nothing was planned; use one of %q", raw, AllProofNames)
}

// PrecheckName is the closed set of cheap checks run before a producer's step.
type PrecheckName string

const (
	// PrecheckNone runs no precheck.
	PrecheckNone PrecheckName = "none"
	// PrecheckAdapterContextEquivalence compares the refreshed metadata with
	// the stored generation before the adapter's output is installed.
	PrecheckAdapterContextEquivalence PrecheckName = "adapter-context-equivalence"
	// PrecheckRefreshFreeMetadataVersions reads a recorded metadata schema
	// version as current when its difference adds only optional fields.
	PrecheckRefreshFreeMetadataVersions PrecheckName = "refresh-free-metadata-versions"
)

// AllPrecheckNames lists every precheck.
var AllPrecheckNames = []PrecheckName{PrecheckNone, PrecheckAdapterContextEquivalence, PrecheckRefreshFreeMetadataVersions}

// NewPrecheckName validates a raw precheck name at an input boundary.
func NewPrecheckName(raw string) (PrecheckName, error) {
	if precheck := PrecheckName(raw); slices.Contains(AllPrecheckNames, precheck) {
		return precheck, nil
	}
	return "", fmt.Errorf("upgrade: read precheck %q: this build declares no such precheck; nothing was planned; use one of %q", raw, AllPrecheckNames)
}

// StepAction is the closed set of work a producer's step can cost.
type StepAction string

const (
	// StepActionNone does no work.
	StepActionNone StepAction = "none"
	// StepActionReindex re-indexes the stored transcript.
	StepActionReindex StepAction = "reindex-stored"
	// StepActionParse captures the native source and parses it.
	StepActionParse StepAction = "parse"
	// StepActionMigrate runs the store's migration chain.
	StepActionMigrate StepAction = "run-migrations"
)

// AllStepActions lists every step action.
var AllStepActions = []StepAction{StepActionNone, StepActionReindex, StepActionParse, StepActionMigrate}

// NewStepAction validates a raw step action at an input boundary.
func NewStepAction(raw string) (StepAction, error) {
	if action := StepAction(raw); slices.Contains(AllStepActions, action) {
		return action, nil
	}
	return "", fmt.Errorf("upgrade: read step action %q: this build declares no such step action; nothing was planned; use one of %q", raw, AllStepActions)
}

// ProducerVersion is one version a registry declares, as text: a revision
// number, a rule-set semantic version, or an immutable capability token.
type ProducerVersion string

func producerVersionOf(revision int) ProducerVersion {
	return ProducerVersion(strconv.Itoa(revision))
}

// ProducerDeclaration is what this build declares about one producer. The
// versions are read from the registry that owns them; the rest is the
// producer kind's declaration.
type ProducerDeclaration struct {
	Kind ProducerKind
	// Name is the harness for a per-harness kind and the capability token for
	// a content model. It is empty for a kind with a single producer.
	Name string
	// Versions is every version a registry can hand this build, oldest
	// first; the last is the newest target. A harness with a managed-generation
	// target carries both its baseline and that target.
	Versions    []ProducerVersion
	ChangeScope ChangeScope
	Input       ProducerInput
	OutputProof []ProofName
	Precheck    PrecheckName
	Fallback    StepAction
}

// String names the producer as kind or kind/name.
func (d ProducerDeclaration) String() string {
	if d.Name == "" {
		return string(d.Kind)
	}
	return string(d.Kind) + "/" + d.Name
}

// Current is the newest version the registries declare.
func (d ProducerDeclaration) Current() ProducerVersion { return d.Versions[len(d.Versions)-1] }

// producerRegistries is every registry the declaration table reads. The
// production table reads the live registries; a test substitutes a changed
// registry to prove a disagreement fails.
type producerRegistries struct {
	baseline            map[ingest.Harness]ingest.HarvesterVersions
	managed             map[ingest.Harness]ingest.HarvesterVersions
	adapters            map[ingest.Harness]ingest.AdapterFactory
	metadataSchema      int
	contentCapabilities []schema.ContentCapability
	redactionRules      string
	storeSchema         int
}

func currentProducerRegistries() producerRegistries {
	return producerRegistries{
		baseline:            ingest.HarvesterVersionRegistry,
		managed:             ingest.GenerationRepairTargets,
		adapters:            ingest.DefaultAdapterRegistry,
		metadataSchema:      int(ingest.CurrentSchemaVersion),
		contentCapabilities: schema.AllContentCapabilities,
		redactionRules:      redact.RuleSetVersion,
		storeSchema:         store.CurrentSchemaVersion(),
	}
}

// ProducerDeclarations returns one declaration per producer this build
// registers, ordered by kind and then name. It fails when two registries
// disagree about which producers exist.
func ProducerDeclarations() ([]ProducerDeclaration, error) {
	return declareProducers(currentProducerRegistries())
}

func declareProducers(registries producerRegistries) ([]ProducerDeclaration, error) {
	harnesses, err := registeredHarnesses(registries)
	if err != nil {
		return nil, err
	}
	var declarations []ProducerDeclaration
	for _, harness := range harnesses {
		baseline := registries.baseline[harness]
		managed, hasManaged := registries.managed[harness]
		versions := func(pick func(ingest.HarvesterVersions) int) []ProducerVersion {
			revisions := []int{pick(baseline)}
			if hasManaged && pick(managed) != pick(baseline) {
				revisions = append(revisions, pick(managed))
			}
			slices.Sort(revisions)
			out := make([]ProducerVersion, len(revisions))
			for index, revision := range revisions {
				out[index] = producerVersionOf(revision)
			}
			return out
		}
		declarations = append(declarations,
			adapterDeclaration(harness, registries.adapters[harness], versions(func(v ingest.HarvesterVersions) int { return v.AdapterVersion })),
			ProducerDeclaration{
				Kind: ProducerKindIndexer, Name: string(harness),
				Versions:    versions(func(v ingest.HarvesterVersions) int { return v.IndexerVersion }),
				ChangeScope: ChangeScopeIndexOnly, Input: ProducerInputStoredTranscript,
				Precheck: PrecheckNone, Fallback: StepActionReindex,
			},
			ProducerDeclaration{
				Kind: ProducerKindFormat, Name: string(harness),
				Versions:    versions(func(v ingest.HarvesterVersions) int { return v.IndexVersion }),
				ChangeScope: ChangeScopeIndexRepresentation, Input: ProducerInputStoredTranscript,
				Precheck: PrecheckNone, Fallback: StepActionReindex,
			},
		)
	}
	declarations = append(declarations, ProducerDeclaration{
		Kind:        ProducerKindMetadataSchema,
		Versions:    []ProducerVersion{producerVersionOf(registries.metadataSchema)},
		ChangeScope: ChangeScopeMetadataShape, Input: ProducerInputNativeSource,
		Precheck: PrecheckRefreshFreeMetadataVersions, Fallback: StepActionParse,
	})
	capabilities := slices.Clone(registries.contentCapabilities)
	slices.Sort(capabilities)
	for index, capability := range capabilities {
		if index > 0 && capabilities[index-1] == capability {
			return nil, fmt.Errorf("upgrade: declare producers: content capability %q is listed twice in the contract inventory; nothing was planned; list each capability once", capability)
		}
		declarations = append(declarations, ProducerDeclaration{
			Kind: ProducerKindContentModel, Name: string(capability),
			// A capability token is immutable, so the token is its own version.
			Versions:    []ProducerVersion{ProducerVersion(capability)},
			ChangeScope: ChangeScopeContentShape, Input: ProducerInputStoredTranscript,
			Precheck: PrecheckNone, Fallback: StepActionNone,
		})
	}
	declarations = append(declarations,
		ProducerDeclaration{
			Kind:        ProducerKindRedactionRules,
			Versions:    []ProducerVersion{ProducerVersion(registries.redactionRules)},
			ChangeScope: ChangeScopePublicationOnly, Input: ProducerInputStoredTranscript,
			Precheck: PrecheckNone, Fallback: StepActionNone,
		},
		ProducerDeclaration{
			Kind:        ProducerKindSchema,
			Versions:    []ProducerVersion{producerVersionOf(registries.storeSchema)},
			ChangeScope: ChangeScopeStoreShape, Input: ProducerInputStoreRows,
			Precheck: PrecheckNone, Fallback: StepActionMigrate,
		},
	)
	for _, declaration := range declarations {
		for _, version := range declaration.Versions {
			if version == "" || version == "0" {
				return nil, fmt.Errorf("upgrade: declare producers: %s has an empty or zero version %q in its registry; nothing was planned; declare a positive version in the owning registry", declaration, version)
			}
		}
	}
	kindRank := func(kind ProducerKind) int { return slices.Index(AllProducerKinds, kind) }
	slices.SortStableFunc(declarations, func(a, b ProducerDeclaration) int {
		if order := cmp.Compare(kindRank(a.Kind), kindRank(b.Kind)); order != 0 {
			return order
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return declarations, nil
}

// registeredHarnesses returns the harnesses the version registry and the
// adapter registry both declare. A harness only one of them declares, or a
// managed-generation target for a harness without a baseline, is a registry
// disagreement.
func registeredHarnesses(registries producerRegistries) ([]ingest.Harness, error) {
	for harness := range registries.baseline {
		if _, ok := registries.adapters[harness]; !ok {
			return nil, fmt.Errorf("upgrade: declare producers: harness %q has harvester versions but no registered adapter; nothing was planned; register its adapter or remove its versions", harness)
		}
	}
	for harness := range registries.adapters {
		if _, ok := registries.baseline[harness]; !ok {
			return nil, fmt.Errorf("upgrade: declare producers: adapter %q has no harvester versions; nothing was planned; declare its adapter, indexer and format versions", harness)
		}
	}
	for harness := range registries.managed {
		if _, ok := registries.baseline[harness]; !ok {
			return nil, fmt.Errorf("upgrade: declare producers: harness %q has a managed-generation target but no baseline versions; nothing was planned; declare its baseline versions or remove the target", harness)
		}
	}
	harnesses := slices.Collect(maps.Keys(registries.baseline))
	slices.Sort(harnesses)
	return harnesses, nil
}

// adapterDeclaration declares one harness's adapter. Its input is read from
// the adapter itself: an adapter that re-derives metadata from the stored
// transcript reads that copy, and any other reads the native source.
func adapterDeclaration(harness ingest.Harness, factory ingest.AdapterFactory, versions []ProducerVersion) ProducerDeclaration {
	input := ProducerInputNativeSource
	// The capability is a type assertion; the adapter's dependencies are never
	// used, so none are supplied.
	if _, ok := factory(nil, nil, salt.Salt{}).(ingest.TranscriptMetadataExtractor); ok {
		input = ProducerInputStoredTranscript
	}
	// The scope is read through the same function the refresh decision's
	// evidence uses, so the plan and a run agree on an undeclared adapter.
	scope := ChangeScopeMetadataOnly
	if !ingest.AdapterDeclaresChangeScope(factory) {
		scope = ""
	}
	return ProducerDeclaration{
		Kind: ProducerKindAdapter, Name: string(harness),
		Versions:    versions,
		ChangeScope: scope, Input: input,
		OutputProof: []ProofName{ProofContentHash, ProofMetadataContextDigest},
		Precheck:    PrecheckAdapterContextEquivalence, Fallback: StepActionParse,
	}
}
