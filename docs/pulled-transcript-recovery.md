# Pulled transcript recovery

Pulls stage a unique complete file set before publishing it with filesystem
renames. A per-transcript operating-system lock serializes concurrent Peasant
processes. The previous directory remains in an owned backup until the new
files are validated and their SQLite index transaction succeeds.

Directory replacement uses a backup rename followed by a staging rename, not
a portable atomic exchange of two nonempty directories. An interruption between
those renames can leave the canonical directory absent until recovery; the
previous complete files remain in the backup throughout that interval.

If a write, publication, or index operation fails, the command returns an error.
It does not delete valid published content or claim a database rollback removed
files. Retry the normal pull to validate and finish an interrupted installation;
`peasant village transcripts pull <id> --force` also repairs the index and
downloads a fresh replacement. No manual SQL is required. An invalid journal or
checksum mismatch fails closed, retaining files rather than guessing which
unverified copy to delete.

An explicit `--force` also authorizes replacement of damaged old files when a
normal pull has already left a valid pending journal. It does not bypass the
journal identity, staged checksum, index projection, or exact stage ownership
checks. File and directory components must be real artifacts, not symlinks;
bounded filesystem handles prevent namespace swaps from redirecting artifact I/O
outside the configured pulls root. Existing configured ancestors remain trusted
configuration (including an explicitly configured linked XDG home).

An unchanged content hash or HTTP 304 does not freeze descriptive metadata.
Changed metadata is atomically replaced as a file, without moving the blob or
provenance, then indexed through the same recovery boundary. A metadata-only
failure retains the prior complete copy. Dry runs neither repair installations
nor write files or index rows.

Recovery cleans only its reserved per-transcript staging namespace and known
journal files, never legacy temporary directories or unrelated files. Pulled
manifests remain content provenance, not account-private collective observations;
downloaded foreign transcripts remain non-republishable.

File data and directory publication barriers are flushed on supported local
filesystems. Windows flushes file data but has no portable directory-flush API
through Go's `os.File`; process-interruption recovery still applies. Filesystem
or hardware durability guarantees remain platform-dependent.
First-pull creation flushes the entry chain through the existing Store data
directory, including directories created by lock setup, before committing the
derived index. That established data directory is opened before creating the
pull namespace. Unchanged configuration ancestors above it need traversal
permission only; a search-only ancestor does not prevent a valid pull. Every
required namespace barrier still fails the command if it cannot be flushed.
