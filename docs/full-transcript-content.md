# Full transcript reads and publication

Session detail, session export, the share redaction scan, and publication read verified full
content from the local database. Provider source files and retained transcript files are not
read by these consumers. Search and list previews remain bounded. Tool input and output keep
their existing full stored values.

An incomplete or corrupt capture cannot serve as a complete transcript or publication. The
error identifies the failed read and asks for `peasant harvest index --force` with the retained
artifacts available. Rebuild the capture before retrying. A short legacy preview is not proof
that a capture is complete.

Indexing first attempts recovery of database-listed incomplete captures. It parses retained
root and subagent artifacts first. `peasant harvest index --force` also reprocesses other
sessions found in the configured retained output directory; it is not a single-publication
repair selector. A matching retained
entry projection receives content-only backfill; with `--force`, a changed projection can be
replaced through normal reindexing and annotation-anchor remapping. Existing provider sources
may be used when no retained artifact exists, or to refresh a legacy OpenCode header that lacks
its message corpus. An unreadable retained artifact is not silently replaced with provider data.
The command cannot recover text when neither usable retained content nor the recorded provider
source remains. Restore that source or regenerate the retained harvest before retrying; it does
not reconstruct missing text from previews and does not publish recovered sessions.

Publication reads metadata and full entries from one database snapshot, then redacts hydrated
entries before building either multipart part. Generated metadata sidecars are not required.
The metadata, indexed entries, and full capture must refer to the same recorded capture revision.
The readiness lists check this metadata without loading transcript bodies. A ready session is
eligible for a full read; chunk and content integrity checks can still refuse publication.

## Read budgets

A complete database read verifies the capture once and hydrates entries in batches within the
same SQLite snapshot. The first batch has a soft budget of 256 KiB; later batches use 8 MiB.
Each batch also has a 100-entry limit. A single oversized entry is returned whole so the cursor
can advance. Explicit byte-budget overrides apply to every batch. Standalone page requests
independently verify the capture, including requests that start at a later entry.

Kickstart's source preview uses the same 256 KiB initial and 8 MiB body/continuation defaults.
Source previews measure source or materialized projection bytes; stored reads measure normalized
entry field bytes. The separate 64 MiB format/materialization safety ceiling is unchanged.
These soft budgets do not truncate stored text or publication payloads. Complete consumers still
allocate the whole session, and the stored push preview still loads the whole body before display.
Database connections are released before rendering, review, consent, or upload.

## Replace one existing publication

Content backfill does not publish anything automatically. Use the existing explicit push flow
to replace a publication that its owner has already approved:

1. Verify the existing local publication receipt against the current Village owner metadata.
   Confirm the account, Village origin, project/session identity, transcript ID and URL, current
   visibility, license, and collective shares. A local receipt alone can be stale.
2. Rebuild and review the full capture locally. Verify that the share scan can read it.
3. Run `peasant village push --force --visibility <current-visibility> --license <current-license>`.
   In the chooser, select **only** the confirmed session. Do not use an unscoped non-interactive
   force push. Keep the same account, Village, project identity, and session identity.
4. Verify the returned receipt and current owner metadata. Content must change while the
   transcript ID, URL, license, visibility, and shares stay the same. If current remote metadata
   changed during the operation, resolve that change before retrying.

For internal callers of the existing pipeline, the same scoped invocation is
`PipelineConfig{Force: true, FilterSessionIDs: []string{sessionID}, Visibility: currentVisibility,
License: currentLicense}`. Supply the verified current values explicitly. The authoritative
publish operation uses the unchanged project/session identity; it does not mutate collective
shares. Visibility convergence is a separate owner update only when the requested value differs
from the returned authoritative visibility.

This is not a new repair API, CLI command, or automatic republish policy. Ordinary pushes still
honor a user's configured or explicitly supplied license and visibility changes. If the current
remote license is absent, keep the configuration's license absent too; an empty runtime override
means “use configuration,” not “remove the remote license.” Do not use this procedure to remove
an irrevocable license.
