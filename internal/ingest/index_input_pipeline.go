package ingest

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
)

// capturedIndexInput exists only for one parse/write attempt. Raw bytes are
// released after parsing; the writer keeps only identity and expected SQL state.
type capturedIndexInput struct {
	session      DiscoveredSession
	kind         TranscriptSourceKind
	metadataPath string
	artifactHash string
	inputHash    string
	expected     *SessionIndexState
	transcript   []byte
	tree         *openCodeJSONInput
}

// Only OpenCode's native JSON layout needs a provider-specific capture method.
type openCodeInputIndexer interface {
	captureJSONInput(context.Context, DiscoveredSession) (*openCodeJSONInput, error)
	indexJSONInput(context.Context, DiscoveredSession, *openCodeJSONInput) (indexformat.Result, error)
}

var _ openCodeInputIndexer = (*OpenCodeIndexer)(nil)

func (p *Pipeline) captureIndexInput(ctx context.Context, im indexedMeta, indexer TranscriptIndexer) (*capturedIndexInput, error) {
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	if !ok {
		return nil, fmt.Errorf("capture index input for session %s: configured index writer cannot read its complete SQL state; indexing was refused without replacing entries; use a store with SessionIndexStateReader and atomic entry writes", im.session.SessionID)
	}
	publisher, err := p.artifactPublisher(nil)
	if err != nil {
		return nil, err
	}
	input := &capturedIndexInput{session: im.session, metadataPath: filepath.Join(filepath.Dir(im.outputTranscriptPath), string(im.session.SessionID)+defaults.MetadataSuffix)}
	err = publisher.WithCapture(ctx, im.session.SessionID, input.metadataPath, func(artifact *ManagedArtifact) error {
		state, err := reader.ReadIndexState(ctx, im.session.SessionID)
		if err != nil {
			return err
		}
		if state == nil || state.SessionID != artifact.Metadata.SessionID || state.ArtifactHash == nil || *state.ArtifactHash != artifact.ArtifactHash || state.Harness != artifact.Metadata.ModelHarness || artifact.Metadata.ModelHarness != im.session.Harness {
			return fmt.Errorf("capture index input for session %s: committed files and stored metadata do not identify the same artifact; no parser ran or entries changed; run harvest to reconcile the committed files before retrying indexing", im.session.SessionID)
		}
		target := p.versionTargets()[state.Harness]
		if state.IndexerVersion > target.IndexerVersion {
			return fmt.Errorf("capture index input for session %s: stored producer revision %d is newer than this indexer's revision %d; no parser ran or entries changed; use a compatible newer indexer", state.SessionID, state.IndexerVersion, target.IndexerVersion)
		}
		if state.IndexVersion != nil {
			if support, ok := p.metricsStore.(IndexFormatSupport); ok && !support.SupportsIndexFormat(*state.IndexVersion) {
				return fmt.Errorf("capture index input for session %s: unsupported index format %d; no parser ran or entries changed; use a build supporting the stored representation", state.SessionID, *state.IndexVersion)
			}
			if *state.IndexVersion > target.IndexVersion {
				return fmt.Errorf("capture index input for session %s: stored index format %d is newer than output format %d; no parser ran or entries changed; use an indexer that can preserve the stored format", state.SessionID, *state.IndexVersion, target.IndexVersion)
			}
		}
		input.expected, input.artifactHash, input.transcript = state, artifact.ArtifactHash, artifact.Transcript
		input.session.SourcePath = ResolvedPath(im.outputTranscriptPath)
		input.session.SourceFormat = artifact.Metadata.Source.Format
		input.session.ParentUUID = artifact.Metadata.ParentUUID
		if input.session.Harness == HarnessOpenCode {
			origin, err := recognizeManagedOpenCodeProjection(artifact.Transcript, input.session.SessionID)
			if err != nil {
				return err
			}
			input.session.TranscriptOrigin = origin
			if origin == TranscriptOriginFile && input.session.OriginalRoot == "" {
				// A retained file-origin session may have no enabled source config.
				// Its original native locator, never the managed copy, supplies root.
				native, err := NewResolvedPath(artifact.Metadata.Source.FilePath)
				if err != nil {
					return fmt.Errorf("capture native OpenCode input for %s: original root and valid native locator are unavailable: %w; stored entries were preserved; restore the native message/part source before retrying", input.session.SessionID, err)
				}
				input.session.SourcePath = native
			}
		}
		input.kind = indexer.SourceKind()
		if resolver, ok := indexer.(SessionTranscriptSourceResolver); ok {
			input.kind = resolver.TranscriptSourceKindFor(input.session)
		}
		switch input.kind {
		case TranscriptSourceFile:
			// Both parser APIs consume these captured bytes. The orchestration
			// boundary wraps legacy format1 output and refuses unverified empty.
		case TranscriptSourceDirectory:
			native, ok := indexer.(openCodeInputIndexer)
			if !ok || input.session.Harness != HarnessOpenCode {
				return fmt.Errorf("capture index input for session %s: directory indexer has no supported native input capture; no entries changed; provide the OpenCode captured-tree indexer", im.session.SessionID)
			}
			input.tree, err = native.captureJSONInput(ctx, input.session)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("capture index input for session %s: indexer did not declare a supported source kind; no parser ran; declare a file or supported native directory source", im.session.SessionID)
		}
		input.inputHash = indexInputDigest(input.session, input.transcript, input.tree)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return input, nil
}

func parseCapturedIndexInput(ctx context.Context, indexer TranscriptIndexer, input *capturedIndexInput) (indexformat.Result, error) {
	if input.kind == TranscriptSourceDirectory {
		return indexer.(openCodeInputIndexer).indexJSONInput(ctx, input.session, input.tree)
	}
	// Artifact capture, including zero bytes, establishes presence. Never reopen
	// the path because an empty captured transcript happens to have a nil slice.
	data := input.transcript
	if data == nil {
		data = []byte{}
	}
	return indexWithSourceKind(ctx, indexer, input.session, data)
}

func (p *Pipeline) withCurrentIndexInput(ctx context.Context, input *capturedIndexInput, use func() error) error {
	publisher, err := p.artifactPublisher(nil)
	if err != nil {
		return err
	}
	return publisher.WithCapture(ctx, input.session.SessionID, input.metadataPath, func(current *ManagedArtifact) error {
		if current.ArtifactHash != input.artifactHash {
			return &StaleIndexWorkError{SessionID: input.session.SessionID}
		}
		if input.kind == TranscriptSourceDirectory {
			native, ok := p.indexers[input.session.Harness].(openCodeInputIndexer)
			if !ok {
				return fmt.Errorf("revalidate native index input for %s: captured-tree indexer is unavailable; entries were preserved; restore the registered indexer and retry", input.session.SessionID)
			}
			tree, err := native.captureJSONInput(ctx, input.session)
			if err != nil {
				return err
			}
			if indexInputDigest(input.session, nil, tree) != input.inputHash {
				return &StaleIndexWorkError{SessionID: input.session.SessionID}
			}
		}
		return use()
	})
}
