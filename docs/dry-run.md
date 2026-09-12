# Dry-run planning

`peasant harvest --dry-run`, `peasant harvest index --dry-run` and
`peasant village push --dry-run` leave existing configuration, managed artifacts
and database files unchanged. Timing output stays on stderr without creating a log.

Database-backed planning requires an existing database at this build's schema
version, with an existing installation salt. Close Peasant's database users first:
an existing WAL, shared-memory file or rollback journal prevents this inspection.
Dry-run reports that prerequisite instead of checkpointing or ignoring a journal.
It reads a stable checkpointed database into a private in-memory view, so memory
usage includes the database image. It never creates or migrates the source database.

Legacy configuration migration must be completed by a normal command before
planning can determine the affected work. Harvest planning
uses recorded metadata and index evidence; it does not parse full transcripts or
certify new successful input. Push forecasts additionally require coherent full
content through the same validation used before upload.
