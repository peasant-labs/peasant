package store

// migrationV34 creates the two derived-index tables backing `peasant village
// transcripts pull`. They are a derived index of the on-disk
// village-pulls/ manifests — any files↔DB divergence is repaired by re-pull
// (--force), never by manual DB surgery. Neither table feeds ingest/analytics
// or the annotate-push candidate set: pulled rows are foreign and one-way.
//
// pulled_transcripts: one row per pulled transcript. Composite PK
// (village_host, transcript_id) keeps the same village UUID distinct across
// multiple villages (C-F4). local_session_id correlates a pull back to a
// locally-published session when the round-trip applies (NULL otherwise);
// title/harness/project_name are display-only snapshots (NULL when the village
// did not supply them). content_hash is the SERVER-COMPUTED served-blob hash
// used for idempotency. pull_dir records the on-disk manifest location.
//
// pulled_annotations: foreign-marked authored annotations. Composite PK
// (village_host, content_hash) dedups by content across pulls/refreshes.
// transcript_id has NO foreign key — an annotation may target an OWN pushed
// transcript that has no pulled_transcripts row (the annotation-refresh path).
// author_user_id/author_username carry the village account identity used to
// foreign-mark and to exclude the requester's own authored rows. payload holds
// the full PullAnnotation JSON. retracted_at is SCHEMA-ONLY (designed for the
// deferred retraction-reconciliation feature; never written by the MVP).
//
// Indexes: idx_pulled_annotations_transcript supports listing a transcript's
// annotations; idx_pulled_annotations_local_session supports the round-trip
// correlation lookup.
const migrationV34 = `
CREATE TABLE pulled_transcripts (
    village_host      TEXT NOT NULL,
    transcript_id     TEXT NOT NULL,
    owner_user_id     TEXT NOT NULL,
    owner_username    TEXT NOT NULL,
    local_session_id  TEXT,
    title             TEXT,
    harness           TEXT,
    project_name      TEXT,
    content_hash      TEXT NOT NULL,
    visibility        TEXT NOT NULL,
    pull_dir          TEXT NOT NULL,
    first_pulled_at   INTEGER NOT NULL,
    last_pulled_at    INTEGER NOT NULL,
    annotation_count  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (village_host, transcript_id)
) STRICT;

CREATE TABLE pulled_annotations (
    village_host      TEXT NOT NULL,
    content_hash      TEXT NOT NULL,
    transcript_id     TEXT NOT NULL,
    local_session_id  TEXT,
    author_user_id    TEXT NOT NULL,
    author_username   TEXT NOT NULL,
    payload           TEXT NOT NULL,
    pulled_at         INTEGER NOT NULL,
    retracted_at      INTEGER,
    PRIMARY KEY (village_host, content_hash)
) STRICT;

CREATE INDEX idx_pulled_annotations_transcript ON pulled_annotations(village_host, transcript_id);
CREATE INDEX idx_pulled_annotations_local_session ON pulled_annotations(local_session_id);
`
