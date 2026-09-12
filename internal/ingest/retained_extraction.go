package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// processRetainedSession prepares from one captured pair and uses the same
// metadata-last publisher and drain reconciliation as a native extraction.
func (p *Pipeline) processRetainedSession(ctx context.Context, session DiscoveredSession, metadataPath string) workerResult {
	result := SessionResult{SessionID: session.SessionID, Harness: session.Harness, ParentUUID: session.ParentUUID, Status: DiffUpdated}
	fail := func(err error) workerResult {
		result.Error = err
		return workerResult{result: result}
	}
	if err := p.checkStoredRewriteVersion(ctx, session.SessionID, session.Harness); err != nil {
		return fail(err)
	}
	// Native debug files are not metadata extraction inputs or replacement targets.
	session.DebugPaths = nil
	original, err := readArtifactPair(p.fs, string(p.config.OutputDir), metadataPath, session.SessionID)
	if err != nil {
		return fail(err)
	}
	result.ParentUUID = original.Metadata.ParentUUID
	factory, ok := p.adapters[session.Harness]
	if !ok {
		return fail(&InsufficientRetainedInputError{SessionID: session.SessionID, Harness: session.Harness, Reason: "no adapter is registered"})
	}
	extractor, ok := factory(p.fs, p.git, p.salt).(TranscriptMetadataExtractor)
	if !ok {
		return fail(&InsufficientRetainedInputError{SessionID: session.SessionID, Harness: session.Harness, Reason: "this adapter requires native records and context that its managed projection does not retain"})
	}
	metadata, err := extractor.ExtractMetadataFromTranscript(ctx, original.Transcript, &original.Metadata)
	if err != nil {
		return fail(err)
	}
	if metadata == nil || metadata.SessionID != original.Metadata.SessionID || metadata.ModelHarness != original.Metadata.ModelHarness || metadata.HostSlug != original.Metadata.HostSlug || metadata.Source != original.Metadata.Source {
		return fail(fmt.Errorf("extract metadata for retained session %s: adapter changed the original identity or source locator; no artifact was published; correct the adapter's retained-input implementation and retry", session.SessionID))
	}
	version := p.versionTargets()[session.Harness].AdapterVersion
	metadata.AdapterVersion = &version
	metadata.SchemaVersion = CurrentSchemaVersion
	metadata.Timestamp.Ingested = original.Metadata.Timestamp.Ingested
	metadata.DerivedAt = nil
	metadata.ContentHash = schema.ComputeTranscriptHash(original.Transcript)
	metadata.MetadataHash = schema.ComputeMetadataHash(metadata)
	before, err := json.Marshal(&original.Metadata)
	if err != nil {
		return fail(err)
	}
	after, err := json.Marshal(metadata)
	if err != nil {
		return fail(err)
	}
	metadataJSON, err := updateRetainedMetadataFields(original.MetadataJSON, before, after)
	if err != nil {
		return fail(err)
	}
	candidate, err := newIngestArtifact(metadata, metadataJSON, original.Transcript)
	if err != nil {
		return fail(err)
	}
	// Re-install the same transcript with the refreshed metadata by rename,
	// metadata last. Extracting from retained bytes acquires no native cursor
	// or origin evidence, so the mirror preserves the previously acquired
	// evidence; the drain records the pair.
	sessionDir := filepath.Dir(metadataPath)
	metaFilename := string(session.SessionID) + defaults.MetadataSuffix
	pair := installedPair{
		transcriptName: string(session.SessionID) + "--transcript." + string(candidate.Metadata.Source.Format),
		transcript:     candidate.Transcript,
		metadataName:   metaFilename,
		metadata:       candidate.MetadataJSON,
	}
	if err := p.installManagedPair(ctx, sessionDir, string(session.SessionID), pair, session); err != nil {
		return fail(err)
	}
	result.OutputPath = sessionDir
	return workerResult{
		result: result, meta: &candidate.Metadata, artifact: candidate,
		transcriptData:       candidate.Transcript,
		outputTranscriptPath: filepath.Join(result.OutputPath, string(session.SessionID)+"--transcript."+string(candidate.Metadata.Source.Format)),
		originalRoot:         session.OriginalRoot, transcriptOrigin: session.TranscriptOrigin,
		startMs:      candidate.Metadata.Timestamp.Start,
		metaFilename: metaFilename, sessionDir: result.OutputPath,
	}
}

// Update only changed declared fields. Supported metadata can contain additional
// fields, including nested context, which decoding through UnifiedMetadata alone
// would otherwise discard during metadata extraction.
func updateRetainedMetadataFields(original, before, after []byte) ([]byte, error) {
	var fields, oldFields, newFields map[string]json.RawMessage
	if err := json.Unmarshal(original, &fields); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(before, &oldFields); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(after, &newFields); err != nil {
		return nil, err
	}
	for name := range oldFields {
		if _, exists := newFields[name]; !exists {
			delete(fields, name)
		}
	}
	for name, value := range newFields {
		if bytes.Equal(oldFields[name], value) {
			continue
		}
		prior, old, next := bytes.TrimSpace(fields[name]), bytes.TrimSpace(oldFields[name]), bytes.TrimSpace(value)
		if len(prior) > 0 && prior[0] == '{' && len(old) > 0 && old[0] == '{' && len(next) > 0 && next[0] == '{' {
			merged, err := updateRetainedMetadataFields(prior, old, next)
			if err != nil {
				return nil, err
			}
			fields[name] = merged
		} else {
			fields[name] = value
		}
	}
	return json.Marshal(fields)
}
