package ingest

import (
	"context"
	"log/slog"
	"path/filepath"
)

// indexProgressLogsPaired refuses a byte estimate when streamed outcomes and
// their logs no longer describe the same set of sessions.
func indexProgressLogsPaired(logger *slog.Logger, logs, done int) bool {
	if logs == done {
		return true
	}
	logger.Debug("index progress uses session counts: streamed outcome and log counts differ",
		"streamed_outcomes", done, "index_logs", logs)
	return false
}

type inputPayloadSizer interface {
	payloadSize(context.Context, DiscoveredSession) (int64, error)
}

var _ inputPayloadSizer = (*OpenCodeAdapter)(nil)

// extractInputBytes measures the input the extract worker expects to read.
// Zero means unknown; a failed optional measurement never changes the harvest.
func (p *Pipeline) extractInputBytes(ctx context.Context, entry DiffEntry) int64 {
	session := entry.Session
	if entry.Decision.Lane == RefreshLaneStoredIndex {
		// This lane does no extraction: its input is consumed later by INDEX.
		return 0
	}
	if entry.Decision.Lane == RefreshLaneStoredMetadata {
		metadata, err := p.metadataForRewrite(ctx, session)
		if err != nil || metadata == nil || p.config.Force || entry.pairRepair ||
			metadataNeedsNativeRefresh(metadata.SchemaVersion) || p.nativeInputChanged(session, metadata) {
			// The worker may switch to a native capture. Do not use the size
			// of a different input as a denominator.
			return 0
		}
		path, err := p.findMetadataPath(ctx, session)
		if err != nil || path == "" {
			return 0
		}
		path = filepath.Join(filepath.Dir(path), string(session.SessionID)+"--transcript."+string(metadata.Source.Format))
		return p.regularFileBytes(path)
	}
	if session.TranscriptOrigin != TranscriptOriginFile {
		factory, ok := p.adapters[session.Harness]
		if !ok {
			return 0
		}
		adapter, ok := factory(p.fs, p.git, p.salt).(inputPayloadSizer)
		if !ok {
			return 0
		}
		size, err := adapter.payloadSize(ctx, session)
		if err != nil {
			return 0
		}
		return size
	}
	if session.Harness == HarnessOpenCode && session.OriginalRoot != "" {
		// The JSON source is a directory of message and part files, not
		// just SourcePath. Discovery does not carry their aggregate size.
		return 0
	}
	return p.regularFileBytes(session.SourcePath.String())
}

func (p *Pipeline) regularFileBytes(path string) int64 {
	if path == "" {
		return 0
	}
	info, err := p.fs.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}
