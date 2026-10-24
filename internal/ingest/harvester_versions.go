package ingest

import (
	"fmt"
	"maps"
)

// HarvesterVersions declares current parser revisions and the indexer's output
// format. These targets are not evidence that a stored session was processed.
type HarvesterVersions struct {
	AdapterVersion int
	IndexerVersion int
	IndexVersion   int
}

// HarvesterVersionRegistry is initialized once and read-only by convention.
// Indexer revision 15 preserves the former global parser baseline. Index format
// 1 is the relational representation, not that parser revision.
var HarvesterVersionRegistry = map[Harness]HarvesterVersions{
	HarnessPi:         {AdapterVersion: 3, IndexerVersion: 18, IndexVersion: 1},
	HarnessClaudeCode: {AdapterVersion: 3, IndexerVersion: 20, IndexVersion: 1},
	HarnessOpenCode:   {AdapterVersion: 3, IndexerVersion: 18, IndexVersion: 1},
	HarnessCodex:      {AdapterVersion: 2, IndexerVersion: 18, IndexVersion: 1},
	HarnessCursor:     {AdapterVersion: 1, IndexerVersion: 18, IndexVersion: 1},
	HarnessStrike:     {AdapterVersion: 1, IndexerVersion: 17, IndexVersion: 1},
}

// GenerationRepairTargets declares the adapter, indexer and format
// targets that replace the stored baseline ONLY for a store that can persist
// and read a managed generation: a registered format-2 writer, a configured
// snapshot reader with its owned-artifact root and lock, a managed generation
// activator and a prior reader. A store that cannot is served by the stored
// baseline, so the managed-generation path is never advertised before its writer and reader
// exist. When a harness is absent from the baseline it is also absent here.
var GenerationRepairTargets = map[Harness]HarvesterVersions{
	HarnessClaudeCode: {AdapterVersion: 3, IndexerVersion: 21, IndexVersion: 2},
	HarnessCodex:      {AdapterVersion: 3, IndexerVersion: 20, IndexVersion: 2},
	HarnessOpenCode:   {AdapterVersion: 4, IndexerVersion: 19, IndexVersion: 2},
}

// generationTargetApplicable reports whether this build can read one
// stored session's native layout into a managed generation. The OpenCode
// candidate snapshot reads the current session_message projection; a session
// stored from the older message/part layout, or from a JSON tree, has no
// snapshot this build can read, so the stored adapter refresh serves it
// instead of failing the managed-generation lane. A harness without a declared native
// target is always served by its stored path.
func generationTargetApplicable(session DiscoveredSession) bool {
	if session.Harness != HarnessOpenCode {
		return true
	}
	return session.TranscriptOrigin == TranscriptOriginOpenCodeCurrentSQLite
}

// GenerationTargets overlays the managed-generation targets on a baseline
// registry for a store that supports them, leaving every other harness and any
// harness without a declared managed-generation target exactly as the baseline states.
func GenerationTargets(baseline map[Harness]HarvesterVersions) map[Harness]HarvesterVersions {
	targets := maps.Clone(baseline)
	for harness, target := range GenerationRepairTargets {
		if _, ok := targets[harness]; ok {
			targets[harness] = target
		}
	}
	return targets
}

// WithHarvesterVersions injects targets for a pipeline without changing global
// registrations. The pipeline owns a copy, including when an option is reused.
func WithHarvesterVersions(versions map[Harness]HarvesterVersions) PipelineOption {
	targets := maps.Clone(versions)
	return func(p *Pipeline) { p.harvesterVersions = maps.Clone(targets) }
}

func (p *Pipeline) validateHarvesterVersions() error {
	if p.harvesterVersions == nil {
		p.harvesterVersions = maps.Clone(HarvesterVersionRegistry)
	}
	if p.config.Harness != nil {
		if _, ok := p.harvesterVersions[*p.config.Harness]; !ok {
			return fmt.Errorf("pipeline version registry: selected harness %q has no version declaration; select a registered harvester before running maintenance", *p.config.Harness)
		}
	}
	for harness, versions := range p.harvesterVersions {
		if _, ok := DefaultAdapterRegistry[harness]; !ok {
			return fmt.Errorf("pipeline version registry: harness %q has no registered adapter; remove its target or register the supported harness before harvesting", harness)
		}
		if versions.AdapterVersion < 1 || versions.IndexerVersion < 1 || versions.IndexVersion < 1 {
			return fmt.Errorf("pipeline version registry: harness %q has nonpositive versions %+v; declare positive adapter, indexer and index format versions before harvesting", harness, versions)
		}
	}
	for harness := range p.adapters {
		if _, ok := p.harvesterVersions[harness]; !ok {
			return fmt.Errorf("pipeline version registry: adapter %q has no version declaration; add its harvester targets before harvesting", harness)
		}
	}
	for harness := range p.indexers {
		if _, ok := p.harvesterVersions[harness]; !ok {
			return fmt.Errorf("pipeline version registry: indexer %q has no version declaration; add its harvester targets before harvesting", harness)
		}
	}
	// Logs-only ingestion never uses a Store or an indexer. An unused index
	// declaration must not introduce a database dependency into that path.
	if p.metricsStore == nil || len(p.indexers) == 0 {
		return nil
	}
	for harness, versions := range p.indexerTargets() {
		indexer := p.indexers[harness]
		if versions.IndexVersion != 1 && !indexerDeclaresManagedOutput(indexer) {
			return fmt.Errorf("pipeline version registry: harness %q has a slice-returning indexer, which implements neither the versioned result exit nor the generation builder, but declares format %d; register an indexer that can produce the declared representation before indexing", harness, versions.IndexVersion)
		}
		support, ok := p.metricsStore.(IndexFormatSupport)
		if !ok || !support.SupportsIndexFormat(versions.IndexVersion) {
			return fmt.Errorf("pipeline version registry: harness %q declares unsupported output format %d for its writer; register a compatible format writer before harvesting", harness, versions.IndexVersion)
		}
	}
	return nil
}

// indexerDeclaresManagedOutput reports whether a registered indexer can produce
// the representation its harness declares. Two contracts qualify: the versioned
// result exit, and the generation builder the managed-generation dispatch
// asserts at runtime. Both are per-harness optional capabilities, asserted at
// construction and at dispatch, never members of the compatibility surface.
func indexerDeclaresManagedOutput(indexer TranscriptIndexer) bool {
	if _, ok := indexer.(VersionedTranscriptIndexer); ok {
		return true
	}
	_, ok := indexer.(GenerationBuilder)
	return ok
}

// indexerTargets scopes maintenance by the actual registered indexers and an
// explicit harness filter. Discovery source overrides do not narrow maintenance.
func (p *Pipeline) indexerTargets() map[Harness]HarvesterVersions {
	targets := make(map[Harness]HarvesterVersions, len(p.indexers))
	for harness := range p.indexers {
		if p.config.Harness != nil && *p.config.Harness != harness {
			continue
		}
		targets[harness] = p.versionTargets()[harness]
	}
	return targets
}

// resolveVersionTargets computes the effective harness targets once. An
// explicit injection wins; otherwise a store that can persist and read a
// managed generation activates the managed-generation repair targets, and every other store
// keeps the stored baseline.
func (p *Pipeline) resolveVersionTargets() map[Harness]HarvesterVersions {
	if p.harvesterVersions != nil {
		return p.harvesterVersions
	}
	if p.supportsManagedGenerations() {
		return GenerationTargets(HarvesterVersionRegistry)
	}
	return HarvesterVersionRegistry
}

func (p *Pipeline) versionTargets() map[Harness]HarvesterVersions {
	if p.resolvedVersions != nil {
		return p.resolvedVersions
	}
	// Internal stage tests and callers can construct Pipeline directly. Reading
	// the defaults here does not mutate the registry or initialize shared state.
	return p.resolveVersionTargets()
}

// sessionVersionTarget returns the effective harvester target for one stored
// session. When the resolved target is the managed-generation override but the
// session's layout has no native snapshot this build can read, the session
// keeps the stored baseline target so the ordinary adapter refresh serves it
// instead of the managed-generation lane. An explicitly injected non-managed-generation target is
// returned unchanged.
func (p *Pipeline) sessionVersionTarget(session DiscoveredSession) HarvesterVersions {
	target := p.versionTargets()[session.Harness]
	native, declared := GenerationRepairTargets[session.Harness]
	if declared && target == native && !generationTargetApplicable(session) {
		if baseline, ok := HarvesterVersionRegistry[session.Harness]; ok {
			return baseline
		}
	}
	return target
}

// managedGenerationSupport is the store capability probe the managed-generation
// gate needs: a registered format-2 writer and a configured snapshot reader.
type managedGenerationSupport interface {
	SupportsIndexFormat(int) bool
	GenerationSnapshotsSupported() bool
}

// supportsManagedGenerations reports whether the configured store can stage,
// activate and read a managed generation. All four capabilities are required:
// without the activator the candidate cannot be persisted, without the prior
// reader a refresh would rekey unchanged source, and without the writer and
// snapshot reader the format target would advertise a representation no caller
// can persist or read.
func (p *Pipeline) supportsManagedGenerations() bool {
	if p.metricsStore == nil {
		return false
	}
	if _, ok := p.metricsStore.(GenerationActivator); !ok {
		return false
	}
	if _, ok := p.metricsStore.(GenerationPriorReader); !ok {
		return false
	}
	support, ok := p.metricsStore.(managedGenerationSupport)
	if !ok {
		return false
	}
	return support.SupportsIndexFormat(2) && support.GenerationSnapshotsSupported()
}
