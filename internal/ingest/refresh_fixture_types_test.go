package ingest_test

import (
	"fmt"
	"slices"

	"github.com/peasant-labs/peasant/internal/ingest"
)

type refreshLaneSource string

const (
	refreshSourceCurrent         refreshLaneSource = "current"
	refreshSourceChanged         refreshLaneSource = "changed"
	refreshSourceChangedOldClock refreshLaneSource = "changed_old_clock"
)

func (v refreshLaneSource) Validate() error {
	return validateFixtureChoice("source", v, refreshSourceCurrent, refreshSourceChanged, refreshSourceChangedOldClock)
}

type refreshLaneAdapter string

const (
	refreshAdapterCurrent refreshLaneAdapter = "current"
	refreshAdapterBehind  refreshLaneAdapter = "behind"
	refreshAdapterAbsent  refreshLaneAdapter = "absent"
)

func (v refreshLaneAdapter) Validate() error {
	return validateFixtureChoice("stored_adapter", v, refreshAdapterCurrent, refreshAdapterBehind, refreshAdapterAbsent)
}

type refreshLaneFileAdapter string

const (
	refreshFileAdapterUnspecified refreshLaneFileAdapter = ""
	refreshFileAdapterCurrent     refreshLaneFileAdapter = "current"
)

func (v refreshLaneFileAdapter) Validate() error {
	return validateFixtureChoice("file_adapter", v, refreshFileAdapterUnspecified, refreshFileAdapterCurrent)
}

type refreshLaneMetadataSchema string

const (
	refreshSchemaCurrent       refreshLaneMetadataSchema = "current"
	refreshSchemaNativeRefresh refreshLaneMetadataSchema = "native_refresh"
)

func (v refreshLaneMetadataSchema) Validate() error {
	return validateFixtureChoice("metadata_schema", v, refreshSchemaCurrent, refreshSchemaNativeRefresh)
}

type refreshLanePair string

const (
	refreshPairPresent refreshLanePair = "present"
	refreshPairMissing refreshLanePair = "missing"
)

func (v refreshLanePair) Validate() error {
	return validateFixtureChoice("pair", v, refreshPairPresent, refreshPairMissing)
}

type refreshLaneIndexer string

const (
	refreshIndexerCurrent refreshLaneIndexer = "current"
	refreshIndexerBehind  refreshLaneIndexer = "behind"
)

func (v refreshLaneIndexer) Validate() error {
	return validateFixtureChoice("indexer", v, refreshIndexerCurrent, refreshIndexerBehind)
}

type refreshLaneRefusalIndexer string

const (
	refreshRefusalIndexerUnspecified refreshLaneRefusalIndexer = ""
	refreshRefusalIndexerCurrent     refreshLaneRefusalIndexer = "current"
	refreshRefusalIndexerBehind      refreshLaneRefusalIndexer = "behind"
)

func (v refreshLaneRefusalIndexer) Validate() error {
	return validateFixtureChoice("refusal_indexer", v, refreshRefusalIndexerUnspecified, refreshRefusalIndexerCurrent, refreshRefusalIndexerBehind)
}

type refreshLanePublication string

const (
	refreshPublicationBound   refreshLanePublication = "bound"
	refreshPublicationUnbound refreshLanePublication = "unbound"
	refreshPublicationMissing refreshLanePublication = "missing"
)

func (v refreshLanePublication) Validate() error {
	return validateFixtureChoice("publication", v, refreshPublicationBound, refreshPublicationUnbound, refreshPublicationMissing)
}

type fixtureNativeSource string

const (
	fixtureNativeSourceUnspecified fixtureNativeSource = ""
	fixtureNativeSourceMalformed   fixtureNativeSource = "malformed"
)

func (v fixtureNativeSource) Validate() error {
	return validateFixtureChoice("native source", v, fixtureNativeSourceUnspecified, fixtureNativeSourceMalformed)
}

type refreshLanePairMember string

const (
	refreshPairMetadata   refreshLanePairMember = "metadata"
	refreshPairTranscript refreshLanePairMember = "transcript"
)

func (v refreshLanePairMember) Validate() error {
	return validateFixtureChoice("want_pair_writes", v, refreshPairMetadata, refreshPairTranscript)
}

func validateFixtureChoice[T ~string](field string, value T, allowed ...T) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("%s %q is unsupported; use one of %q", field, value, allowed)
}

func (c refreshLaneCase) Validate() error {
	if c.Harness != ingest.HarnessClaudeCode && c.Harness != ingest.HarnessStrike {
		return fmt.Errorf("harness %q is unsupported; use %q or %q in refresh_lane.yaml", c.Harness, ingest.HarnessClaudeCode, ingest.HarnessStrike)
	}
	for _, axis := range []interface{ Validate() error }{
		c.Source, c.StoredAdapter, c.FileAdapter, c.MetadataSchema, c.Pair,
		c.Indexer, c.RefusalIndexer, c.Publication, c.NativeSource,
	} {
		if err := axis.Validate(); err != nil {
			return err
		}
	}
	for _, member := range c.WantPairWrites {
		if err := member.Validate(); err != nil {
			return err
		}
	}
	if _, err := ingest.NewContentCaptureFailureCode(string(c.Refusal)); err != nil {
		return fmt.Errorf("refusal: %w", err)
	}
	if c.Refusal != ingest.ContentCaptureNoFailure &&
		c.Refusal != ingest.ContentCaptureStrictRefused &&
		c.Refusal != ingest.ContentCaptureSourceRecordsOmitted &&
		c.Refusal != ingest.ContentCaptureUnknownDataRetained {
		return fmt.Errorf("refusal %q is not a permanent refusal; use an empty code or a permanent refusal in refresh_lane.yaml", c.Refusal)
	}
	if _, err := ingest.NewRefreshReason(string(c.WantReason)); err != nil {
		return fmt.Errorf("want_reason: %w", err)
	}
	if _, err := ingest.NewRefreshLane(string(c.WantLane)); err != nil {
		return fmt.Errorf("want_lane: %w", err)
	}
	if c.NativeSource == fixtureNativeSourceMalformed && c.MaxNativeReads <= 0 {
		return fmt.Errorf("max_native_reads must be positive for %q; set a bound in refresh_lane.yaml", c.NativeSource)
	}
	return nil
}
