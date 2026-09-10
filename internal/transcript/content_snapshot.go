package transcript

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// PRIOR-VERSION, DEPRECATION CANDIDATE. Database content is authoritative.
//
// ContentSnapshot and the ReadSessionContent below overlay full text parsed
// from a retained managed FILE onto the stored SQL projection. They predate
// durable full-content capture in SQLite and have no production caller left:
// the viewer, export and publication all read verified content from the
// database, which never depends on a file still being present, unchanged, or
// parseable by today's harness parser. The code is retained, not deleted,
// because no replacement has been ratified for a caller that genuinely has
// only a file; nothing in this build may start using it as a content source.
//
// ContentSnapshot retains one SQL view and, when proven compatible, full text
// from its captured input. A viewer may use Entries despite FullContentError;
// export/publish callers must refuse full-content success when it is non-nil.
type ContentSnapshot struct {
	*store.SessionContentSnapshot
	Artifact         *ingest.ManagedArtifact
	FullContentError error
}

// ContentStore supplies the existing coherent SQL read and its locator hint.
// Callers may wrap Store without introducing another content snapshot boundary.
type ContentStore interface {
	LookupSessionLocation(context.Context, ingest.SessionID) (string, string, error)
	ReadSessionContent(context.Context, string) (*store.SessionContentSnapshot, error)
}

var _ ContentStore = (*store.Store)(nil)

// PRIOR-VERSION, DEPRECATION CANDIDATE. Database content is authoritative;
// this file overlay has no production caller. See the ContentSnapshot note.
//
// ReadSessionContent takes file ownership before the content SQL snapshot, then
// releases both before parsing. A locator lookup is only a hint: the captured
// artifact and SQL identity must agree. This never creates or repairs artifacts.
func ReadSessionContent(ctx context.Context, db ContentStore, fs ingest.FileSystem, managedRoot, sessionID string) (*ContentSnapshot, error) {
	sid := ingest.SessionID(sessionID)
	host, parent, err := db.LookupSessionLocation(ctx, sid)
	if err != nil {
		return nil, err
	}
	result := &ContentSnapshot{}
	var input *ingest.CapturedIndexInput
	var indexer ingest.TranscriptIndexer
	var readErr error
	readStarted := false
	resolvedRoot, fullErr := ingest.NewResolvedPath(managedRoot)
	var publisher *ingest.ArtifactPublisher
	if fullErr == nil {
		managedRoot = string(resolvedRoot)
		publisher, fullErr = ingest.NewArtifactPublisher(fs, managedRoot, ingest.ArtifactPublisherOptions{})
	}
	if fullErr == nil {
		path := ingest.SessionMetadataPath(managedRoot, host, sessionID, parent)
		fullErr = publisher.WithCapture(ctx, sid, path, func(artifact *ingest.ManagedArtifact) error {
			readStarted = true
			result.SessionContentSnapshot, readErr = db.ReadSessionContent(ctx, sessionID)
			if readErr != nil {
				return readErr
			}
			if result.SessionContentSnapshot == nil {
				return fmt.Errorf("session %s is no longer stored", sid)
			}
			state := result.IndexState
			meta := artifact.Metadata
			storedParent := ""
			if result.Detail.ParentID != nil {
				storedParent = *result.Detail.ParentID
			}
			artifactParent := ""
			if meta.ParentUUID != nil {
				artifactParent = string(*meta.ParentUUID)
			}
			if state == nil || state.SessionID != meta.SessionID || state.Harness != meta.ModelHarness || result.Detail.HostSlug != string(meta.HostSlug) || storedParent != artifactParent || state.ArtifactHash == nil || *state.ArtifactHash != artifact.ArtifactHash {
				return fmt.Errorf("retained artifact and stored session do not identify the same committed input")
			}
			target, known := ingest.HarvesterVersionRegistry[state.Harness]
			if !known || state.IndexerVersion != target.IndexerVersion || state.IndexVersion == nil || *state.IndexVersion != target.IndexVersion || state.IndexedAt == nil || state.IndexedInputHash == nil {
				return fmt.Errorf("stored index has no matching completed input/producer/format proof for the full-content parser")
			}
			indexer = ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{FullContent: true})[state.Harness]
			if _, strict := indexer.(ingest.VersionedTranscriptIndexer); !strict {
				return fmt.Errorf("full-content parser cannot verify completion")
			}
			session := ingest.DiscoveredSession{SessionID: sid, Harness: state.Harness, SourcePath: ingest.ResolvedPath(filepath.Join(filepath.Dir(path), sessionID+"--transcript."+string(meta.Source.Format)))}
			input, err = ingest.CaptureIndexInput(ctx, indexer, session, artifact)
			if err != nil {
				return err
			}
			if input.Hash() != *state.IndexedInputHash {
				return fmt.Errorf("captured parser input differs from the last successful index input")
			}
			result.Artifact = artifact
			return nil
		})
	}
	if !readStarted {
		result.SessionContentSnapshot, readErr = db.ReadSessionContent(ctx, sessionID)
	}
	if readErr != nil {
		return nil, readErr
	}
	if result.SessionContentSnapshot == nil {
		return nil, nil
	}
	if fullErr == nil {
		var parsed indexformat.Result
		parsed, fullErr = input.Parse(ctx, indexer)
		if fullErr == nil {
			var ok bool
			var full indexformat.V1
			full, ok = parsed.(indexformat.V1)
			if !ok || parsed.IndexVersion() != *result.IndexState.IndexVersion {
				fullErr = fmt.Errorf("full parser returned an incompatible concrete representation")
			} else if result.IndexState.Harness == ingest.HarnessCursor && AnyContentTruncated(full.Entries) {
				fullErr = fmt.Errorf("Cursor full-text recovery is unavailable for bounded content")
			} else {
				result.Entries, fullErr = overlayEntryContent(result.Entries, full.Entries)
			}
		}
	}
	if fullErr != nil {
		result.FullContentError = fmt.Errorf("read full content for session %s: %w; stored previews remain available; run harvest with a compatible parser to reconcile retained inputs and retry", sid, fullErr)
	}
	return result, nil
}

// Full text never establishes coordinates. Compare the actual parser projection
// before replacing any text, keeping stored structural and annotation identity.
func overlayEntryContent(stored, full []schema.SessionEntry) ([]schema.SessionEntry, error) {
	if len(stored) != len(full) {
		return stored, fmt.Errorf("full parser entry coordinates do not match the stored index")
	}
	for i := range stored {
		a, b := stored[i], full[i]
		if a.SessionID != b.SessionID || a.EntryIndex != b.EntryIndex || a.Harness != b.Harness || a.Depth != b.Depth || a.EntryType != b.EntryType || a.Role != b.Role || !reflect.DeepEqual(a.ParentIndex, b.ParentIndex) || !reflect.DeepEqual(a.EntryID, b.EntryID) || !reflect.DeepEqual(a.ParentEntryID, b.ParentEntryID) || !reflect.DeepEqual(a.ToolCallID, b.ToolCallID) || !reflect.DeepEqual(a.PartType, b.PartType) {
			return stored, fmt.Errorf("full parser coordinate differs at entry %d", a.EntryIndex)
		}
	}
	entries := slices.Clone(stored)
	for i := range entries {
		entries[i].ContentPreview = full[i].ContentPreview
		entries[i].ToolInput = full[i].ToolInput
		entries[i].ToolOutput = full[i].ToolOutput
	}
	return entries, nil
}
