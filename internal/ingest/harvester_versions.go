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
	HarnessClaudeCode: {AdapterVersion: 1, IndexerVersion: 16, IndexVersion: 1},
	HarnessOpenCode:   {AdapterVersion: 1, IndexerVersion: 15, IndexVersion: 1},
	HarnessCodex:      {AdapterVersion: 1, IndexerVersion: 16, IndexVersion: 1},
	HarnessCursor:     {AdapterVersion: 1, IndexerVersion: 16, IndexVersion: 1},
	HarnessStrike:     {AdapterVersion: 1, IndexerVersion: 16, IndexVersion: 1},
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
		if _, versioned := indexer.(VersionedTranscriptIndexer); versions.IndexVersion != 1 && !versioned {
			return fmt.Errorf("pipeline version registry: harness %q has a slice-returning indexer, which only produces format 1, but declares format %d; use a concrete versioned result before indexing", harness, versions.IndexVersion)
		}
		support, ok := p.metricsStore.(IndexFormatSupport)
		if !ok || !support.SupportsIndexFormat(versions.IndexVersion) {
			return fmt.Errorf("pipeline version registry: harness %q declares unsupported output format %d for its writer; register a compatible format writer before harvesting", harness, versions.IndexVersion)
		}
	}
	return nil
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

func (p *Pipeline) versionTargets() map[Harness]HarvesterVersions {
	if p.harvesterVersions != nil {
		return p.harvesterVersions
	}
	// Internal stage tests and callers can construct Pipeline directly. Reading
	// the defaults here does not mutate the registry or initialize shared state.
	return HarvesterVersionRegistry
}
