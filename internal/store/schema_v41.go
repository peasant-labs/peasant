package store

// migrationV41 adds the association target arm to the local annotation TPT
// model. It is deliberately normalized through session_commit_associations:
// annotations carry only an opaque association ID, while the producer ledger
// owns the session and observed-hash relationship. Recreating the view changes
// no existing rows or CHECK constraints.
const migrationV41 = `
DROP VIEW annotations_with_target;

INSERT INTO target_kinds (id, name) VALUES (5, 'association');

CREATE TABLE annotation_target_associations (
    annotation_id  TEXT PRIMARY KEY REFERENCES annotations(id) ON DELETE CASCADE,
    association_id TEXT NOT NULL REFERENCES session_commit_associations(association_id)
) STRICT;

CREATE INDEX idx_ann_target_association
    ON annotation_target_associations(association_id);

CREATE VIEW annotations_with_target AS
SELECT
    a.id, a.target_kind_id, tk.name AS target_kind,
    ts.session_id AS target_session_id,
    te.session_id AS target_entry_session_id,
    te.entry_index AS target_entry_index,
    te.end_index AS target_entry_end_index,
    ta.target_annotation_id,
    tp.project_hash AS target_project_hash,
    tsc.association_id AS target_association_id,
    a.annotator_id, ak.name AS annotator_kind,
    ann.name AS annotator_name, ann.display_name AS annotator_display_name,
    a.annotation_type_id, t.type_id, t.display_name AS type_name,
    f.family, c.class,
    a.value, a.confidence, a.reason, a.provenance,
    a.is_primary, a.content_hash, a.created_at, a.updated_at, a.superseded_by
FROM annotations a
    JOIN target_kinds tk ON a.target_kind_id = tk.id
    JOIN annotators ann ON a.annotator_id = ann.id
    JOIN annotator_kinds ak ON ann.kind_id = ak.id
    JOIN annotation_types t ON a.annotation_type_id = t.id
    JOIN annotation_families f ON t.family_id = f.id
    JOIN annotation_classes c ON f.class_id = c.id
    LEFT JOIN annotation_target_sessions ts ON a.id = ts.annotation_id
    LEFT JOIN annotation_target_entries te ON a.id = te.annotation_id
    LEFT JOIN annotation_target_annotations ta ON a.id = ta.annotation_id
    LEFT JOIN annotation_target_projects tp ON a.id = tp.annotation_id
    LEFT JOIN annotation_target_associations tsc ON a.id = tsc.annotation_id;
`
