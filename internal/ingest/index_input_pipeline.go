package ingest

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
)

// CapturedIndexInput owns the exact bytes and consumed context for one parse.
// The native OpenCode tree remains private; Parse never reopens the source.
type CapturedIndexInput struct {
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

func (p *Pipeline) captureIndexInput(ctx context.Context, im indexedMeta, indexer TranscriptIndexer) (*CapturedIndexInput, error) {
	reader, ok := p.metricsStore.(SessionIndexStateReader)
	if !ok {
		return nil, fmt.Errorf("capture index input for session %s: configured index writer cannot read its complete SQL state; indexing was refused without replacing entries; use a store with SessionIndexStateReader and atomic entry writes", im.session.SessionID)
	}
	publisher, err := p.artifactPublisher(nil)
	if err != nil {
		return nil, err
	}
	metadataPath := filepath.Join(filepath.Dir(im.outputTranscriptPath), string(im.session.SessionID)+defaults.MetadataSuffix)
	var input *CapturedIndexInput
	err = publisher.WithCapture(ctx, im.session.SessionID, metadataPath, func(artifact *ManagedArtifact) error {
		state, err := reader.ReadIndexState(ctx, im.session.SessionID)
		if err != nil {
			return err
		}
		if state == nil || state.SessionID != artifact.Metadata.SessionID || state.ArtifactHash == nil || *state.ArtifactHash != artifact.ArtifactHash || state.Harness != artifact.Metadata.ModelHarness || artifact.Metadata.ModelHarness != im.session.Harness {
			return fmt.Errorf("capture index input for session %s: committed files and stored metadata do not identify the same artifact; no parser ran or entries changed; run harvest to reconcile the committed files before retrying indexing", im.session.SessionID)
		}
		if err := p.checkIndexProducer(state); err != nil {
			return err
		}
		session := im.session
		session.SourcePath = ResolvedPath(im.outputTranscriptPath)
		input, err = CaptureIndexInput(ctx, indexer, session, artifact)
		if err != nil {
			return err
		}
		input.expected, input.metadataPath = state, metadataPath
		return nil
	})
	if err != nil {
		return nil, err
	}
	return input, nil
}

func (p *Pipeline) checkIndexProducer(state *SessionIndexState) error {
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
	return nil
}

// CaptureIndexInput captures parser input while the caller owns the artifact.
// A validated artifact establishes transcript presence, including zero bytes.
func CaptureIndexInput(ctx context.Context, indexer TranscriptIndexer, session DiscoveredSession, artifact *ManagedArtifact) (*CapturedIndexInput, error) {
	if err := artifact.Validate(); err != nil {
		return nil, err
	}
	if indexer == nil || session.SessionID != artifact.Metadata.SessionID || session.Harness != artifact.Metadata.ModelHarness {
		return nil, fmt.Errorf("capture index input: parser/session does not match the managed artifact; retain stored previews and reconcile before retrying")
	}
	input := &CapturedIndexInput{session: session, artifactHash: artifact.ArtifactHash, transcript: bytes.Clone(artifact.Transcript)}
	input.session.SourceFormat = artifact.Metadata.Source.Format
	input.session.ParentUUID = artifact.Metadata.ParentUUID
	if session.Harness == HarnessOpenCode {
		origin, err := recognizeManagedOpenCodeProjection(artifact.Transcript, session.SessionID)
		if err != nil {
			return nil, err
		}
		input.session.TranscriptOrigin = origin
		if origin == TranscriptOriginFile && input.session.OriginalRoot == "" {
			native, err := NewResolvedPath(artifact.Metadata.Source.FilePath)
			if err != nil {
				return nil, fmt.Errorf("capture native OpenCode input for %s: valid original locator is unavailable: %w; restore the native message/part source before retrying", session.SessionID, err)
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
	case TranscriptSourceDirectory:
		native, ok := indexer.(openCodeInputIndexer)
		if !ok || session.Harness != HarnessOpenCode {
			return nil, fmt.Errorf("capture index input for %s: directory indexer has no supported native input capture", session.SessionID)
		}
		var err error
		input.tree, err = native.captureJSONInput(ctx, input.session)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("capture index input for %s: unsupported source kind; declare a file or supported native directory source", session.SessionID)
	}
	input.inputHash = indexInputDigest(input.session, input.transcript, input.tree)
	return input, nil
}

// Hash identifies actual parser input, independently of preview mode and version.
func (input *CapturedIndexInput) Hash() string { return input.inputHash }

// Parse runs against captured input only, preserving the existing strict Result
// and legacy nonempty-result completion rules.
func (input *CapturedIndexInput) Parse(ctx context.Context, indexer TranscriptIndexer) (indexformat.Result, error) {
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

func parseCapturedIndexInput(ctx context.Context, indexer TranscriptIndexer, input *CapturedIndexInput) (indexformat.Result, error) {
	return input.Parse(ctx, indexer)
}

func (p *Pipeline) withCurrentIndexInput(ctx context.Context, input *CapturedIndexInput, use func() error) error {
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
