package ingest

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// CapturedIndexInput owns the exact bytes and consumed context for one parse.
// The native OpenCode tree remains private; Parse never reopens the source.
type CapturedIndexInput struct {
	session DiscoveredSession
	kind    TranscriptSourceKind
	// published reports that this run committed the artifact the input was
	// captured from, so a directory tree read for it is the tree the
	// publication capture saw.
	published    bool
	metadataPath string
	// metadataData carries the committed metadata bytes when this run wrote them,
	// so the write-time identity check reads no file. Nil means read from disk.
	metadataData []byte
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
	metadataPath := filepath.Join(filepath.Dir(im.outputTranscriptPath), string(im.session.SessionID)+defaults.MetadataSuffix)
	// Bytes the caller already read are the bytes it verified; the transcript
	// file is not read again behind them. A retained target reads its pair
	// from disk, validated by the hash the metadata carries, so a torn or
	// stale pair is refused here.
	artifact, err := p.captureArtifactForIndex(metadataPath, im)
	if err != nil {
		return nil, err
	}
	state, err := reader.ReadIndexState(ctx, im.session.SessionID)
	if err != nil {
		return nil, err
	}
	// The stored artifact identity is a CONTRADICTION check, not a presence
	// check. A row that predates the artifact-hash column has no identity to
	// check, and its retained pair is the only input it has ever had; the
	// ordinary index write establishes the identity from the pair it parsed
	// (SessionEntryWrite.ArtifactIdentity), so the next run can check it (see
	// content_backfill.go's "until an ordinary index run establishes it").
	// A stored identity that disagrees with the pair stays a refusal: that is
	// the torn, mixed or stale pair this guard exists for.
	if state == nil || state.SessionID != artifact.Metadata.SessionID || state.Harness != artifact.Metadata.ModelHarness || artifact.Metadata.ModelHarness != im.session.Harness {
		return nil, fmt.Errorf("capture index input for session %s: saved files and stored metadata do not identify the same artifact; no parser ran or entries changed; run harvest to save the session again before retrying indexing", im.session.SessionID)
	}
	if state.ArtifactHash != nil && *state.ArtifactHash != artifact.ArtifactHash {
		return nil, fmt.Errorf("capture index input for session %s: the saved pair does not match the stored artifact identity; no parser ran or entries changed; run harvest to save the session again before retrying indexing", im.session.SessionID)
	}
	if err := p.checkIndexProducer(state); err != nil {
		return nil, err
	}
	session := im.session
	session.SourcePath = ResolvedPath(im.outputTranscriptPath)
	input, err := CaptureIndexInput(ctx, indexer, session, artifact)
	if err != nil {
		return nil, err
	}
	input.expected, input.metadataPath, input.published = state, metadataPath, im.published
	input.metadataData = im.metadataData
	return input, nil
}

// captureArtifactForIndex pairs the session's committed metadata with the
// bytes the index step will parse. When the worker handed over the transcript
// bytes it just wrote, those are used directly; otherwise the pair is read
// from disk and validated by the metadata's own content hash.
func (p *Pipeline) captureArtifactForIndex(metadataPath string, im indexedMeta) (*ManagedArtifact, error) {
	if len(im.transcriptData) == 0 {
		return readArtifactPair(p.fs, string(p.config.OutputDir), metadataPath, im.session.SessionID)
	}
	// The worker handed over the bytes it just wrote for both halves of the
	// pair. Use the in-memory metadata directly rather than reading the file
	// back: a newly ingested session's index costs no read of its own pair.
	data := im.metadataData
	if len(data) == 0 {
		var err error
		if data, err = p.fs.ReadFile(metadataPath); err != nil {
			return nil, err
		}
	}
	meta, err := decodeManagedMetadata(data, metadataPath)
	if err != nil {
		return nil, err
	}
	if meta.SessionID != im.session.SessionID {
		return nil, fmt.Errorf("capture index input for session %s: metadata names different session %s; no parser ran; run harvest to save the session again", im.session.SessionID, meta.SessionID)
	}
	return NewManagedArtifact(data, im.transcriptData)
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
	input := &CapturedIndexInput{session: session, artifactHash: artifact.ArtifactHash, transcript: artifact.Transcript}
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

// strictIndexFormat is the only stored format the strict capture parsers can
// produce; complete-content certification is possible only on this path.
const strictIndexFormat = 1

// Parse runs against captured input only, preserving the existing strict Result
// and legacy nonempty-result completion rules.
func (input *CapturedIndexInput) Parse(ctx context.Context, indexer TranscriptIndexer) (indexformat.Result, error) {
	return input.ParseForFormat(ctx, indexer, strictIndexFormat)
}

// ParseForFormat selects the parser by the harness's declared output format.
// Format 1 is the strict capture path. Any other declared format needs the
// indexer's own versioned result, because the strict capture interface only
// produces format 1 and using it would contradict the declaration; such a
// result is never certified as complete content.
func (input *CapturedIndexInput) ParseForFormat(ctx context.Context, indexer TranscriptIndexer, declared int) (indexformat.Result, error) {
	if declared != strictIndexFormat {
		versioned, ok := indexer.(VersionedTranscriptIndexer)
		if !ok {
			return nil, fmt.Errorf("index session %s: harness %s declares output format %d but its indexer only produces format %d; no entries were replaced; correct the indexer declaration or supply a versioned indexer", input.session.SessionID, input.session.Harness, declared, strictIndexFormat)
		}
		if input.kind == TranscriptSourceDirectory {
			return versioned.IndexTranscriptResult(ctx, input.session)
		}
		data := input.transcript
		if data == nil {
			data = []byte{}
		}
		return versioned.IndexTranscriptBytesResult(ctx, input.session, data)
	}
	if input.kind == TranscriptSourceDirectory {
		if native, ok := indexer.(*OpenCodeIndexer); ok {
			messages, err := parseOpenCodeJSONInput(input.tree, &indexCompletion{ctx: ctx, session: input.session})
			if err != nil {
				return nil, err
			}
			capture, err := native.captureSemanticMessages(ctx, input.session, messages)
			return indexformat.V1{Entries: capture.Entries}, err
		}
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

// ParseTolerant parses the same captured bytes with the harness's tolerant
// projection parser, the one that represents what it recognizes and skips
// what it does not. Its result is a bounded projection only: it is stored as
// an incomplete capture and never certified as complete content.
func (input *CapturedIndexInput) ParseTolerant(ctx context.Context, indexer TranscriptIndexer) (indexformat.Result, error) {
	if input.kind == TranscriptSourceDirectory {
		native, ok := indexer.(openCodeInputIndexer)
		if !ok {
			return nil, fmt.Errorf("index session %s: directory indexer has no tolerant native-tree projection; no entries were stored", input.session.SessionID)
		}
		return native.indexJSONInput(ctx, input.session, input.tree)
	}
	data := input.transcript
	if data == nil {
		data = []byte{}
	}
	if versioned, ok := indexer.(VersionedTranscriptIndexer); ok {
		return versioned.IndexTranscriptBytesResult(ctx, input.session, data)
	}
	entries, err := indexer.IndexTranscriptBytes(ctx, input.session, data)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, &unverifiedEmptyIndexError{session: input.session}
	}
	return indexformat.V1{Entries: entries}, nil
}

func parseCapturedIndexInput(ctx context.Context, indexer TranscriptIndexer, input *CapturedIndexInput, declared int) (indexformat.Result, error) {
	return input.ParseForFormat(ctx, indexer, declared)
}

func (p *Pipeline) withCurrentIndexInput(ctx context.Context, input *CapturedIndexInput, use func() error) error {
	// The saved identity is what the write must still describe. It is read from
	// the committed metadata, not by hashing the transcript file again: a
	// replaced pair rewrites its metadata and is caught; a transcript that
	// moved on disk after the input was verified does not invalidate the
	// verified parse.
	current, err := p.committedArtifactIdentity(input.metadataData, input.metadataPath, input.session.SessionID)
	if err != nil {
		return err
	}
	if current != input.artifactHash {
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
}

// committedArtifactIdentity is the artifact hash the metadata at metadataPath
// declares. The metadata's own content checksum stands in for hashing the
// transcript file, so a transcript that moved on disk after a verified read
// leaves the identity as it was, while a replaced pair, whose metadata is
// rewritten, changes it. Metadata that carries no content checksum is
// identified by reading the pair.
func (p *Pipeline) committedArtifactIdentity(metadataData []byte, metadataPath string, sid SessionID) (string, error) {
	// Use the committed bytes this run wrote when they are in hand; otherwise
	// read the metadata file. A newly ingested session pays no pair read here.
	data := metadataData
	if len(data) == 0 {
		var err error
		if data, err = p.fs.ReadFile(metadataPath); err != nil {
			return "", err
		}
	}
	meta, err := decodeManagedMetadata(data, metadataPath)
	if err != nil {
		return "", err
	}
	if meta.SessionID != sid {
		return "", fmt.Errorf("identify session %s: metadata names different session %s; preserve the files and restore the correct locator", sid, meta.SessionID)
	}
	if meta.ContentHash == "" {
		artifact, err := readArtifactPair(p.fs, string(p.config.OutputDir), metadataPath, sid)
		if err != nil {
			return "", err
		}
		return artifact.ArtifactHash, nil
	}
	semantic, err := artifactSemanticJSON(data, meta.ContentHash)
	if err != nil {
		return "", err
	}
	return schema.ComputeTranscriptHash(semantic), nil
}
