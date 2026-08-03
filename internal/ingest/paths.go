package ingest

import (
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// SessionDir returns the on-disk directory holding a session's redacted
// transcript and metadata within the peasant-sync tree rooted at base.
//
// Layout (must match the ingest write path):
//   - root session  (parentID == ""):  {base}/{hostSlug}/{sessionID}
//   - subagent       (parentID != ""):  {base}/{hostSlug}/{parentID}/{subagents}/{sessionID}
//
// Centralizing this here keeps the ingest write path and the push read path
// (pushSession, runPushWizard, buildMetadataReport) in agreement, so subagent
// metadata is never looked up at the wrong (top-level) location.
func SessionDir(base, hostSlug, sessionID, parentID string) string {
	if parentID != "" {
		return filepath.Join(base, hostSlug, parentID, defaults.DirSubagents.String(), sessionID)
	}
	return filepath.Join(base, hostSlug, sessionID)
}

// SessionMetadataPath returns the absolute path to a session's
// {sessionID}--metadata.json file, resolving the root-vs-subagent layout via
// SessionDir.
func SessionMetadataPath(base, hostSlug, sessionID, parentID string) string {
	return filepath.Join(SessionDir(base, hostSlug, sessionID, parentID), sessionID+defaults.MetadataSuffix)
}
