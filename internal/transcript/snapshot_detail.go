package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// ErrLegacySnapshot marks a V1 read snapshot that carries no managed
// generation. Callers handle it with the preserved legacy detail path; it is
// never a reason to reparse a mutable native source for a V2 read.
var ErrLegacySnapshot = fmt.Errorf("transcript snapshot is a legacy V1 read without a managed generation")

// SnapshotToDetailValidated hydrates one captured V2 generation snapshot and
// folds it through the canonical validated detail path shared with export and
// publication. Full tool arguments and results are hydrated from immutable
// managed blobs BEFORE folding; the mutable native source is never reparsed.
// Main and each earlier partition fold independently; equal text or index
// values across partitions are never merged. A corrupt or missing artifact
// fails the read; nothing partial is emitted.
func SnapshotToDetailValidated(ctx context.Context, snapshot indexformat.ReadSnapshot, resolver indexformat.ContentResolver) (*schema.SessionDetailPayload, error) {
	fail := func(err error) (*schema.SessionDetailPayload, error) {
		return nil, fmt.Errorf("transcript.SnapshotToDetailValidated: hydrate captured generation %q for session %q before detail/export/publication: %w; no transcript was emitted; run managed recovery to repair the generation, then retry",
			snapshot.GenerationID, snapshot.Session.ID, err)
	}
	if snapshot.IndexVersion == 1 {
		return fail(ErrLegacySnapshot)
	}
	if resolver == nil {
		return fail(fmt.Errorf("no content resolver; a generation read cannot hydrate managed blobs without one"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	mainEntries, err := hydratePartitionEntries(ctx, snapshot, snapshot.Main.Entries, resolver)
	if err != nil {
		return fail(err)
	}
	mainTurns, err := EntriesToTurnsValidated(mainEntries)
	if err != nil {
		return fail(err)
	}
	session := snapshotToSession(snapshot)
	session.Turns = mainTurns
	session.NativeMetadata = append([]schema.NativeMetadataRecord(nil), snapshot.Main.NativeMetadata...)
	for i := range snapshot.Earlier {
		section := snapshot.Earlier[i]
		entries, err := hydratePartitionEntries(ctx, snapshot, section.Content.Entries, resolver)
		if err != nil {
			return fail(err)
		}
		turns, err := EntriesToTurnsValidated(entries)
		if err != nil {
			return fail(err)
		}
		session.EarlierHistory = append(session.EarlierHistory, ingest.EarlierHistorySection{
			State:          section.State,
			Turns:          turns,
			NativeMetadata: append([]schema.NativeMetadataRecord(nil), section.Content.NativeMetadata...),
		})
	}
	detail, err := SessionToDetailValidated(session)
	if err != nil {
		return fail(err)
	}
	return detail, nil
}

// hydratePartitionEntries resolves one partition's entries against the
// snapshot's captured content map. Every entry that names a source ref reads
// its full bytes from the immutable managed blob addressed by the captured
// generation identity; ref-less legacy rows pass through unchanged. The blob
// selects its target field by entry type: tool_use hydrates arguments,
// tool_result hydrates the result, every other block hydrates display text in
// the transient conversion input. Integrity failures refuse the partition.
func hydratePartitionEntries(ctx context.Context, snapshot indexformat.ReadSnapshot, entries []schema.SessionEntry, resolver indexformat.ContentResolver) ([]schema.SessionEntry, error) {
	byRef := make(map[schema.SourceEntryRef]indexformat.ContentRecord, len(snapshot.Content))
	for _, record := range snapshot.Content {
		byRef[record.Ref] = record
	}
	out := make([]schema.SessionEntry, len(entries))
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := entries[i]
		if entry.SourceEntryRef == "" {
			out[i] = entry
			continue
		}
		record, ok := byRef[entry.SourceEntryRef]
		if !ok {
			return nil, fmt.Errorf("entry %d names source ref %q with no captured content record in generation %q; the snapshot is not self-contained; no partial transcript was emitted",
				entry.EntryIndex, entry.SourceEntryRef, snapshot.GenerationID)
		}
		full, err := resolver.ReadFullContent(ctx, snapshot.Metadata.SessionID, snapshot.GenerationID, record)
		if err != nil {
			return nil, fmt.Errorf("entry %d: resolve managed content for source ref %q in generation %q: %w",
				entry.EntryIndex, entry.SourceEntryRef, snapshot.GenerationID, err)
		}
		if !utf8.Valid(full) {
			return nil, fmt.Errorf("entry %d: managed content for source ref %q in generation %q is not valid UTF-8; the artifact is corrupt; no partial transcript was emitted",
				entry.EntryIndex, entry.SourceEntryRef, snapshot.GenerationID)
		}
		text := string(full)
		switch entry.EntryType {
		case schema.EntryTypeToolUse:
			entry.ToolInput = &text
		case schema.EntryTypeToolResult:
			entry.ToolOutput = &text
		default:
			entry.ContentPreview = &text
		}
		out[i] = entry
	}
	return out, nil
}

// snapshotToSession projects a captured snapshot's durable metadata onto the
// detail builder input. Counts, graph identity and relationships come from the
// committed snapshot only; the unknown/measured distinction on the input count
// is preserved exactly, never backfilled from turn totals.
func snapshotToSession(snapshot indexformat.ReadSnapshot) *ingest.Session {
	metadata := snapshot.Metadata
	session := &ingest.Session{
		ID:        ingest.SessionID(snapshot.Session.ID),
		Harness:   metadata.ModelHarness,
		Model:     string(metadata.Model),
		StartTime: unixMilliToTime(metadata.Timestamp.Start),
		EndTime:   unixMilliToTime(metadata.Timestamp.End),
		Metadata: ingest.SessionMetadata{
			TurnCount:     snapshot.Session.TurnCount,
			ToolCallCount: snapshot.Session.ToolCallCount,
		},
		Relationships:        append([]schema.SessionRelationship(nil), snapshot.Session.Relationships...),
		Purpose:              snapshot.Session.Purpose,
		RootSessionID:        snapshot.Session.RootSessionID,
		ParentSessionID:      snapshot.Session.ParentSessionID,
		InputSubmissionCount: snapshot.Session.InputSubmissionCount,
	}
	if metadata.CWD != "" {
		session.ProjectPath = metadata.CWD
	}
	if metadata.Git.Branch != nil {
		session.GitBranch = *metadata.Git.Branch
	}
	if metadata.Git.Remote != nil {
		session.GitRemote = *metadata.Git.Remote
	}
	return session
}

// unixMilliToTime converts durable millisecond timestamps to builder time.
// A non-positive stored bound means the source recorded no time; the builder
// keeps the zero time rather than inventing an epoch date.
func unixMilliToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// BuildSnapshotDetailBytes is the single durable payload-construction boundary
// shared by detail reads, export and publication packaging (IP-P8). It holds
// the snapshot's shared lock through hydration, folding, validation AND final
// serialization, then releases the lock before returning owned bytes. Callers
// perform no store access and no remote network while holding the lock; they
// send or write the returned bytes after the lock is released.
func BuildSnapshotDetailBytes(ctx context.Context, reader indexformat.SnapshotReader, resolver indexformat.ContentResolver, sessionID schema.SessionID) ([]byte, *schema.SessionDetailPayload, error) {
	if reader == nil {
		return nil, nil, fmt.Errorf("transcript.BuildSnapshotDetailBytes: no snapshot reader for session %q; a durable payload cannot be built; open the store with generation support", sessionID)
	}
	var owned []byte
	var payload *schema.SessionDetailPayload
	err := reader.WithSessionSnapshot(ctx, sessionID, func(snapshot indexformat.ReadSnapshot) error {
		detail, err := SnapshotToDetailValidated(ctx, snapshot, resolver)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("transcript.BuildSnapshotDetailBytes: serialize validated detail for session %q: %w; nothing was emitted", sessionID, err)
		}
		owned = encoded
		payload = detail
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return owned, payload, nil
}
