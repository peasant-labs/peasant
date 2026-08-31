package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ---------------------------------------------------------------------------
// Params / result types
// ---------------------------------------------------------------------------

// CreateAnnotationParams holds the inputs for CreateAnnotation.
type CreateAnnotationParams struct {
	// Target: exactly one of SessionID, EntryTarget, AnnotationID, ProjectHash,
	// or AssociationID must be set.
	SessionID     *string               // ARM 1: session-level annotation
	EntryTarget   *EntryTarget          // ARM 2: entry-level annotation
	AnnotationID  *string               // ARM 3: meta-annotation (annotation on annotation)
	ProjectHash   *string               // ARM 4: project-level annotation
	AssociationID *schema.AssociationID // ARM 5: durable session-to-commit association

	// AnnotatorID is the UUID FK to annotators.id (required).
	AnnotatorID string

	// AnnotationTypeID is the UUID FK to annotation_types.id (required).
	AnnotationTypeID string

	// Value is the annotation value string (required). Must be pre-validated.
	Value string

	// IsPrimary marks this as the primary annotation when multiple exist for the
	// same target + type combination. Stored as 0/1 in the DB.
	IsPrimary bool

	// Optional metadata.
	Confidence *float64
	Reason     *string
	Provenance *schema.Provenance
}

// EntryTarget is the compound target for entry-level annotations.
type EntryTarget struct {
	SessionID  string
	EntryIndex int
	EndIndex   int // V16: half-open [start, end); 0 means single-entry (entry_index + 1)
}

// CreateAnnotatorParams holds the inputs for CreateAnnotator.
type CreateAnnotatorParams struct {
	Kind        schema.AnnotatorKind
	Name        string
	DisplayName string
	Description string

	// ModelID and ProviderKey are required for agent annotators, forbidden for others.
	ModelID     *string
	ProviderKey *string
}

// AnnotatorRow is a store-native annotator representation.
type AnnotatorRow struct {
	ID          string
	Kind        schema.AnnotatorKind
	Name        string
	DisplayName string
	Description string
	ModelID     *string
	ProviderKey *string
	Status      string
	CreatedAt   int64
}

// AnnotationRow is a store-native annotation representation (from annotations_with_target view).
type AnnotationRow struct {
	ID string

	// Target (exactly one arm set, derived from TPT child tables via VIEW)
	TargetSessionID     *string
	TargetEntryIndex    *int
	TargetEntryEndIndex *int    // V16: half-open [start, end)
	TargetAnnotID       *string // V16: UUID
	TargetProjectHash   *schema.ProjectHash
	TargetAssociationID *schema.AssociationID
	TargetKind          schema.TargetKind

	// IsPrimary indicates this annotation is the primary one for its target+type.
	IsPrimary bool

	// Annotator
	AnnotatorID   string
	AnnotatorKind schema.AnnotatorKind
	AnnotatorName string

	// Type
	AnnotationTypeID string
	TypeID           string
	TypeName         string
	Family           string
	Class            string

	// Content
	Value       string
	Confidence  *float64
	Reason      *string
	Provenance  *schema.Provenance
	ContentHash *string // V16: push dedup

	// Timestamps
	CreatedAt    int64
	UpdatedAt    *int64
	SupersededBy *string // V16: UUID
}

// AnnotationTypeRow is a store-native annotation type representation.
// V16: ID and FamilyID are UUID TEXT; enum columns resolved via INT lookup JOINs.
// V17: ScaleKind from scale_kinds LEFT JOIN (empty string when NULL).
type AnnotationTypeRow struct {
	ID              string
	TypeID          string
	Version         int
	DisplayName     string
	Description     string
	FamilyID        string
	ValueDomainKind schema.ValueDomainKind
	Datatype        schema.AnnotationDatatype
	ValueConstraint string // JSON-encoded permissible values or constraint spec
	LowerIsBetter   *bool
	Status          schema.AnnotationStatus
	Origin          schema.TypeOrigin
	SupersededBy    *string // V16: UUID
	CreatedAt       int64
	// PriorityOverride overrides the kind-based priority for effective annotation
	// resolution. NULL means use the default kind priority (human=3, agent=2, rule=1).
	PriorityOverride *int64
	// ScaleKind is the Stevens measurement level (V17: scale_kinds lookup table).
	// Empty string when scale_kind_id IS NULL (unclassified user-defined types).
	ScaleKind schema.ScaleKind
}

// AnnotationTypeWithFamilyRow extends AnnotationTypeRow with resolved family and class strings.
// Returned by ListAnnotationTypesWithFamily for API response building.
type AnnotationTypeWithFamilyRow struct {
	AnnotationTypeRow
	Family string
	Class  string
	// AllowedTargetKinds lists the target kinds this type may annotate (V16),
	// resolved via the annotation_type_target_kinds junction. Empty slice means
	// no rows in the junction (all kinds allowed by registry convention).
	AllowedTargetKinds []schema.TargetKind
}

// ---------------------------------------------------------------------------
// SQL constants (V16: TPT, UUID PKs, INT lookup JOINs)
// ---------------------------------------------------------------------------

const (
	// V16 annotation INSERT: parent table only (TPT child insert done separately).
	sqlInsertAnnotation = `INSERT INTO annotations (
    id, target_kind_id, annotation_type_id, annotator_id, value,
    confidence, reason, provenance, is_primary, created_at
) VALUES (?, (SELECT id FROM target_kinds WHERE name = ?), ?, ?, ?, ?, ?, ?, ?, ?)`

	// V16 TPT child table INSERTs (one per target kind).
	sqlInsertTargetSession     = `INSERT INTO annotation_target_sessions (annotation_id, session_id) VALUES (?, ?)`
	sqlInsertTargetEntry       = `INSERT INTO annotation_target_entries (annotation_id, session_id, entry_index, end_index) VALUES (?, ?, ?, ?)`
	sqlInsertTargetAnnotation  = `INSERT INTO annotation_target_annotations (annotation_id, target_annotation_id) VALUES (?, ?)`
	sqlInsertTargetProject     = `INSERT INTO annotation_target_projects (annotation_id, project_hash) VALUES (?, ?)`
	sqlInsertTargetAssociation = `INSERT INTO annotation_target_associations (annotation_id, association_id) VALUES (?, ?)`

	// V16 Annotation SELECT column layout (from annotations_with_target view):
	//  0: v.id                       (TEXT UUID)
	//  1: v.target_kind              (TEXT: 'session'/'entry'/'annotation'/'project')
	//  2: v.target_session_id        (TEXT, nullable — from TPT child)
	//  3: v.target_entry_session_id  (TEXT, nullable)
	//  4: v.target_entry_index       (INT, nullable)
	//  5: v.target_entry_end_index   (INT, nullable)
	//  6: v.target_annotation_id     (TEXT UUID, nullable)
	//  7: v.target_project_hash      (TEXT, nullable)
	//  8: v.target_association_id    (TEXT, nullable)
	//  9: v.annotator_id             (TEXT UUID)
	// 10: v.annotator_kind           (TEXT)
	// 11: v.annotator_name           (TEXT)
	// 12: v.annotation_type_id       (TEXT UUID)
	// 13: v.type_id                  (TEXT)
	// 14: v.type_name                (TEXT)
	// 15: v.family                   (TEXT)
	// 16: v.class                    (TEXT)
	// 17: v.value                    (TEXT)
	// 18: v.confidence               (REAL, nullable)
	// 19: v.reason                   (TEXT, nullable)
	// 20: v.provenance               (TEXT, nullable)
	// 21: v.is_primary               (INT)
	// 22: v.content_hash             (TEXT, nullable)
	// 23: v.created_at               (INT)
	// 24: v.updated_at               (INT, nullable)
	// 25: v.superseded_by            (TEXT UUID, nullable)

	sqlAnnotationViewCols = `v.id, v.target_kind,
    v.target_session_id, v.target_entry_session_id,
	    v.target_entry_index, v.target_entry_end_index,
	    v.target_annotation_id, v.target_project_hash, v.target_association_id,
    v.annotator_id, v.annotator_kind, v.annotator_name,
    v.annotation_type_id, v.type_id, v.type_name, v.family, v.class,
    v.value, v.confidence, v.reason, v.provenance,
    v.is_primary, v.content_hash, v.created_at, v.updated_at, v.superseded_by`

	// annotationPushRowColumns is the SELECT column list for an annotation push
	// row, shared by sqlListSystemAnnotations and sqlListSupersededAnnotations. It
	// matches the column layout scanAnnotationPushRow expects (cols 0-17):
	//  0: v.id                      (TEXT UUID)
	//  1: v.target_kind             (TEXT)
	//  2: v.target_session_id       (TEXT, nullable)
	//  3: v.target_entry_session_id (TEXT, nullable)
	//  4: v.target_entry_index      (INT, nullable)
	//  5: v.target_entry_end_index  (INT, nullable)
	//  6: v.target_annotation_id    (TEXT UUID, nullable)
	//  7: v.target_project_hash     (TEXT, nullable)
	//  8: v.target_association_id   (TEXT, nullable)
	//  9: sca.session_id            (TEXT, nullable; selection context only)
	// 10: v.type_id                 (TEXT)
	// 11: v.value                   (TEXT)
	// 12: v.is_primary              (INT)
	// 13: v.confidence              (REAL, nullable)
	// 14: v.reason                  (TEXT, nullable)
	// 15: v.annotator_name          (TEXT)
	// 16: v.provenance              (TEXT JSON, nullable)
	// 17: v.content_hash            (TEXT, nullable)
	annotationPushRowColumns = `
    v.id, v.target_kind,
    v.target_session_id, v.target_entry_session_id,
	    v.target_entry_index, v.target_entry_end_index,
	    v.target_annotation_id, v.target_project_hash, v.target_association_id,
	    sca.session_id AS target_association_session_id,
    v.type_id, v.value, v.is_primary,
    v.confidence, v.reason, v.annotator_name,
    v.provenance, v.content_hash`

	// annotationPushRowQueryHead and annotationPushRowQueryTail bracket the ONLY
	// difference between the two annotation-push queries — the supersession
	// predicate (IS NULL vs IS NOT NULL), spliced in below. Everything else (the
	// column list, joins, system-origin filter, ordering) is shared.
	annotationPushRowQueryHead = `SELECT` + annotationPushRowColumns + `
FROM annotations_with_target v
LEFT JOIN session_commit_associations sca ON sca.association_id = v.target_association_id
JOIN annotation_types t ON t.type_id = v.type_id
JOIN type_origins o ON o.id = t.origin_id
WHERE o.name = 'system'
  AND v.superseded_by IS `
	annotationPushRowQueryTail = `
ORDER BY v.created_at DESC`

	// sqlListSystemAnnotations returns all NON-superseded system-origin annotations.
	// Used by the push pipeline to build the annotation push payload; the village
	// rejects unknown or user-defined type_ids.
	sqlListSystemAnnotations = annotationPushRowQueryHead + `NULL` + annotationPushRowQueryTail

	// sqlListSupersededAnnotations is sqlListSystemAnnotations with the supersession
	// predicate INVERTED: it returns SUPERSEDED system-origin annotations (the rows
	// ListSystemAnnotations excludes). It is the retraction source:
	// each superseded row still carries its original content-bearing fields, so the
	// caller recomputes the SAME content hash it was pushed with.
	sqlListSupersededAnnotations = annotationPushRowQueryHead + `NOT NULL` + annotationPushRowQueryTail

	// sqlFindExistingSessionAnnotation finds the most recent non-superseded annotation
	// for a given (annotation_type_id, annotator_id, session_id) triple.
	// Returns (id, content_hash) — used for dedup before creating a new annotation.
	sqlFindExistingSessionAnnotation = `SELECT a.id, COALESCE(a.content_hash, '')
FROM annotations a
JOIN annotation_target_sessions ts ON ts.annotation_id = a.id
WHERE a.annotation_type_id = ?
  AND a.annotator_id = ?
  AND ts.session_id = ?
  AND a.superseded_by IS NULL
ORDER BY a.created_at DESC
LIMIT 1`

	// sqlFindExistingEntryAnnotation finds the most recent non-superseded annotation
	// for a given (annotation_type_id, annotator_id, session_id, entry_index) tuple.
	// Returns (id, content_hash) — used for dedup before creating a new entry annotation.
	sqlFindExistingEntryAnnotation = `SELECT a.id, COALESCE(a.content_hash, '')
FROM annotations a
JOIN annotation_target_entries te ON te.annotation_id = a.id
WHERE a.annotation_type_id = ?
  AND a.annotator_id = ?
  AND te.session_id = ?
  AND te.entry_index = ?
  AND a.superseded_by IS NULL
ORDER BY a.created_at DESC
LIMIT 1`

	// sqlSupersedeAnnotation marks an annotation as superseded by another.
	sqlSupersedeAnnotation = `UPDATE annotations
SET superseded_by = ?, updated_at = ?
WHERE id = ?`

	// sqlCollectAnnotationIDsByAnnotatorUnscoped collects all annotation IDs belonging
	// to an annotator (by name), with no session scope restriction.
	sqlCollectAnnotationIDsByAnnotatorUnscoped = `SELECT a.id FROM annotations a
WHERE a.annotator_id = (SELECT id FROM annotators WHERE name = ?)`

	// sqlCollectAnnotationIDsByAnnotatorScoped collects annotation IDs belonging to an
	// annotator that target specific sessions (via direct session, entry, or
	// association targets). Association targets resolve through the durable ledger.
	// The IN clause placeholder (%s) is expanded at runtime.
	// Note: %s appears three times — once for each arm of the UNION — so callers must
	// repeat the session ID args for every target kind (matching the PruneSessions
	// pattern in prune.go).
	sqlCollectAnnotationIDsByAnnotatorScopedFmt = `SELECT a.id FROM annotations a
WHERE a.annotator_id = (SELECT id FROM annotators WHERE name = ?)
  AND a.id IN (
    SELECT annotation_id FROM annotation_target_sessions WHERE session_id IN (%s)
    UNION
    SELECT annotation_id FROM annotation_target_entries WHERE session_id IN (%s)
    UNION
    SELECT ata.annotation_id
    FROM annotation_target_associations ata
    JOIN session_commit_associations sca ON sca.association_id = ata.association_id
    WHERE sca.session_id IN (%s)
  )`

	// sqlUpdateContentHash sets the content_hash on an annotation.
	sqlUpdateContentHash = `UPDATE annotations
SET content_hash = ?
WHERE id = ?`

	sqlGetAnnotationsForSession = `SELECT ` + sqlAnnotationViewCols + `
FROM annotations_with_target v
WHERE v.target_session_id = ?
  AND v.superseded_by IS NULL
ORDER BY v.created_at DESC`

	// sqlGetAssociationAnnotationsForSession resolves a normalized association
	// target through the durable ledger so a session-detail annotations request
	// can display it without pretending it is a session- or turn-level label.
	// The quality aggregate intentionally does not use this query.
	sqlGetAssociationAnnotationsForSession = `SELECT ` + sqlAnnotationViewCols + `
FROM annotations_with_target v
JOIN annotation_target_associations ata ON ata.annotation_id = v.id
JOIN session_commit_associations sca ON sca.association_id = ata.association_id
WHERE sca.session_id = ?
  AND v.superseded_by IS NULL
ORDER BY v.created_at DESC`

	// sqlGetAllSessionAnnotations returns every non-superseded session-level
	// annotation across ALL sessions in one statement, for callers that need
	// the whole corpus (the quality snapshot). Global created_at DESC keeps
	// each session's group in the same order GetAnnotationsForSession returns.
	sqlGetAllSessionAnnotations = `SELECT ` + sqlAnnotationViewCols + `
FROM annotations_with_target v
WHERE v.target_session_id IS NOT NULL
  AND v.superseded_by IS NULL
ORDER BY v.created_at DESC`

	// sqlGetEntryAnnotationsForSession returns every non-superseded entry-level
	// annotation targeting any turn of the session. Session-targeted lookups key
	// on target_session_id, which is NULL for entry annotations — so without this
	// the per-turn labels (incl. ingest-generated rule labels) are never served.
	sqlGetEntryAnnotationsForSession = `SELECT ` + sqlAnnotationViewCols + `
FROM annotations_with_target v
WHERE v.target_entry_session_id = ?
  AND v.superseded_by IS NULL
ORDER BY v.created_at DESC`

	sqlGetAnnotationsForEntry = `SELECT ` + sqlAnnotationViewCols + `
FROM annotations_with_target v
WHERE v.target_entry_session_id = ?
  AND v.target_entry_index = ?
  AND v.superseded_by IS NULL
ORDER BY v.created_at DESC`

	// sqlGetAnnotationsForProject returns all non-superseded annotations targeting
	// the given project_hash, ordered by created_at DESC.
	sqlGetAnnotationsForProject = `SELECT ` + sqlAnnotationViewCols + `
FROM annotations_with_target v
WHERE v.target_project_hash = ?
  AND v.superseded_by IS NULL
ORDER BY v.created_at DESC`

	// sqlGetEffectiveAnnotation uses priority_override ordering:
	// COALESCE(t.priority_override, kind_priority) DESC, then most recent within tier.
	sqlGetEffectiveAnnotation = `SELECT ` + sqlAnnotationViewCols + `
FROM annotations_with_target v
JOIN annotation_types t ON t.id = v.annotation_type_id
WHERE v.target_session_id = ?
  AND v.type_id = ?
  AND v.superseded_by IS NULL
ORDER BY
  COALESCE(t.priority_override,
    CASE v.annotator_kind
      WHEN 'human' THEN 3
      WHEN 'agent' THEN 2
      WHEN 'rule'  THEN 1
      ELSE 0
    END) DESC,
  v.created_at DESC
LIMIT 1`

	// V16 annotator INSERT: kind resolved via INT lookup subquery, UUID PK.
	sqlInsertAnnotator = `INSERT INTO annotators (
    id, kind_id, name, display_name, description,
    model_id, provider_key, status, created_at
) VALUES (?, (SELECT id FROM annotator_kinds WHERE name = ?), ?, ?, ?, ?, ?, 'active', ?)`

	// V16 annotator SELECT: JOIN annotator_kinds for kind TEXT resolution.
	sqlGetAnnotatorByName = `SELECT
    a.id, ak.name, a.name, a.display_name, a.description,
    a.model_id, a.provider_key, a.status, a.created_at
FROM annotators a
JOIN annotator_kinds ak ON a.kind_id = ak.id
WHERE a.name = ? LIMIT 1`

	sqlListAnnotators = `SELECT
    a.id, ak.name, a.name, a.display_name, a.description,
    a.model_id, a.provider_key, a.status, a.created_at
FROM annotators a
JOIN annotator_kinds ak ON a.kind_id = ak.id
ORDER BY a.name`

	// V16/V17 AnnotationType SELECT column layout (cols 0-15):
	// Same column positions as pre-V16 but types differ; col 15 added by V17:
	//  0: t.id                  (TEXT UUID, was INTEGER)
	//  1: t.type_id             (TEXT)
	//  2: t.version             (INT)
	//  3: t.display_name        (TEXT)
	//  4: t.description         (TEXT)
	//  5: t.family_id           (TEXT UUID, was INTEGER)
	//  6: vdk.name              (TEXT, was t.value_domain_type)
	//  7: t.datatype            (TEXT)
	//  8: t.value_constraint    (TEXT)
	//  9: t.lower_is_better     (INT, nullable)
	// 10: s.name                (TEXT, was t.status)
	// 11: o.name                (TEXT, was t.origin)
	// 12: t.superseded_by       (TEXT UUID, was INTEGER, nullable)
	// 13: t.created_at          (INT)
	// 14: t.priority_override   (INT, nullable)
	// 15: COALESCE(sk.name,'')  (TEXT, V17: scale_kind name, empty string if NULL)

	sqlAnnotationTypeBase = `SELECT
    t.id, t.type_id, t.version, t.display_name, t.description, t.family_id,
    vdk.name, t.datatype, t.value_constraint,
    t.lower_is_better, s.name, o.name, t.superseded_by, t.created_at,
    t.priority_override, COALESCE(sk.name, '')
FROM annotation_types t
    JOIN value_domain_kinds vdk ON t.value_domain_kind_id = vdk.id
    JOIN annotation_statuses s ON t.status_id = s.id
    JOIN type_origins o ON t.origin_id = o.id
    LEFT JOIN scale_kinds sk ON sk.id = t.scale_kind_id`

	sqlGetAnnotationTypeByTypeID = sqlAnnotationTypeBase + `
WHERE t.type_id = ? LIMIT 1`

	sqlGetAnnotationTypeByDBID = sqlAnnotationTypeBase + `
WHERE t.id = ? LIMIT 1`

	sqlListAnnotationTypes = sqlAnnotationTypeBase

	// V16/V17 annotation types with family/class: same base + family/class JOINs.
	// Columns 15=scale_kind (V17), 16=family, 17=class.
	sqlListAnnotationTypesWithFamily = `SELECT
    t.id, t.type_id, t.version, t.display_name, t.description, t.family_id,
    vdk.name, t.datatype, t.value_constraint,
    t.lower_is_better, s.name, o.name, t.superseded_by, t.created_at,
    t.priority_override, COALESCE(sk.name, ''), f.family, c.class,
    COALESCE((
        SELECT group_concat(tk.name)
        FROM annotation_type_target_kinds attk
            JOIN target_kinds tk ON tk.id = attk.target_kind_id
        WHERE attk.annotation_type_id = t.id
    ), '')
FROM annotation_types t
    JOIN value_domain_kinds vdk ON t.value_domain_kind_id = vdk.id
    JOIN annotation_statuses s ON t.status_id = s.id
    JOIN type_origins o ON t.origin_id = o.id
    LEFT JOIN scale_kinds sk ON sk.id = t.scale_kind_id
    JOIN annotation_families f ON f.id = t.family_id
    JOIN annotation_classes c ON c.id = f.class_id`

	sqlGetAnnotationTypeWithFamilyByTypeID = sqlListAnnotationTypesWithFamily + `
WHERE t.type_id = ? LIMIT 1`

	sqlGetAnnotationTypeWithFamilyByDBID = sqlListAnnotationTypesWithFamily + `
WHERE t.id = ? LIMIT 1`

	// V16 annotation type INSERT: UUID PK, INT FK enums via subqueries.
	sqlInsertAnnotationType = `INSERT INTO annotation_types (
    id, type_id, version, display_name, description, family_id,
    value_domain_kind_id, datatype, value_constraint,
    lower_is_better, status_id, origin_id, created_at
) VALUES (?, ?, 1, ?, ?, ?,
    (SELECT id FROM value_domain_kinds WHERE name = ?),
    ?, ?,
    ?, (SELECT id FROM annotation_statuses WHERE name = 'proposed'),
    (SELECT id FROM type_origins WHERE name = ?), ?)`

	// V16: status transitions use INT lookup subqueries.
	sqlActivateAnnotationType = `UPDATE annotation_types
SET status_id = (SELECT id FROM annotation_statuses WHERE name = 'active')
WHERE type_id = ? AND status_id = (SELECT id FROM annotation_statuses WHERE name = 'proposed')`

	sqlDeprecateAnnotationType = `UPDATE annotation_types
SET status_id = (SELECT id FROM annotation_statuses WHERE name = 'deprecated'),
    superseded_by = (SELECT id FROM annotation_types WHERE type_id = ?)
WHERE type_id = ? AND status_id = (SELECT id FROM annotation_statuses WHERE name = 'active')`

	sqlInsertAnnotationTypeDep = `INSERT INTO annotation_type_deps
    (annotation_type_id, depends_on_id, required, rationale)
SELECT
    (SELECT id FROM annotation_types WHERE type_id = ?),
    (SELECT id FROM annotation_types WHERE type_id = ?),
    ?, ?`

	sqlGetAnnotationTypeDeps = `SELECT
    t1.type_id AS type_id,
    t2.type_id AS depends_on,
    d.required,
    d.rationale
FROM annotation_type_deps d
JOIN annotation_types t1 ON t1.id = d.annotation_type_id
JOIN annotation_types t2 ON t2.id = d.depends_on_id
WHERE t1.type_id = ?`

	// cycleDetectionQuery uses a recursive CTE (proposal section 3.2, depth ≤ 20).
	// V16: annotation_type_deps uses TEXT UUID PKs; the CTE walks TEXT IDs.
	// Parameters: depends_on_id (UUID of the type the new edge points FROM),
	// type_id (UUID that would receive the incoming edge).
	// A non-empty result indicates a cycle.
	cycleDetectionQuery = `
WITH RECURSIVE dep_chain(id, depth, path) AS (
    SELECT d.depends_on_id, 1,
           d.annotation_type_id || ' -> ' || d.depends_on_id
    FROM annotation_type_deps d
    WHERE d.annotation_type_id = ?
    UNION ALL
    SELECT d.depends_on_id, dc.depth + 1,
           dc.path || ' -> ' || d.depends_on_id
    FROM dep_chain dc
    JOIN annotation_type_deps d ON d.annotation_type_id = dc.id
    WHERE dc.depth < 20
)
SELECT id, path FROM dep_chain WHERE id = ?`
)

// ---------------------------------------------------------------------------
// Annotation CRUD
// ---------------------------------------------------------------------------

// CreateAnnotation inserts a new annotation row (parent + TPT child) and returns its UUID.
// The value must be pre-validated by the caller (use AnnotationTypeReader.ValidateValue).
// Exactly one target arm in params must be set.
//
// TODO(annotation/S3): populate updated_at on the superseded annotation when the
// supersede-on-create flow is implemented.
func (s *Store) CreateAnnotation(ctx context.Context, params CreateAnnotationParams) (retID string, err error) {
	if err := validateCreateAnnotationTarget(params); err != nil {
		return "", err
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return "", fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	newID := uuid.New().String()

	// Determine target kind and build TPT child insert.
	var (
		targetKindName string
		childSQL       string
		childArgs      []any
		provenanceJSON any
	)

	switch {
	case params.SessionID != nil:
		targetKindName = "session"
		childSQL = sqlInsertTargetSession
		childArgs = []any{newID, *params.SessionID}
	case params.EntryTarget != nil:
		targetKindName = "entry"
		endIdx := params.EntryTarget.EndIndex
		if endIdx == 0 {
			endIdx = params.EntryTarget.EntryIndex + 1 // default: single-entry span
		}
		childSQL = sqlInsertTargetEntry
		childArgs = []any{newID, params.EntryTarget.SessionID, params.EntryTarget.EntryIndex, endIdx}
	case params.AnnotationID != nil:
		targetKindName = "annotation"
		childSQL = sqlInsertTargetAnnotation
		childArgs = []any{newID, *params.AnnotationID}
	case params.ProjectHash != nil:
		targetKindName = "project"
		childSQL = sqlInsertTargetProject
		childArgs = []any{newID, *params.ProjectHash}
	case params.AssociationID != nil:
		targetKindName = schema.TargetAssociation.String()
		childSQL = sqlInsertTargetAssociation
		childArgs = []any{newID, params.AssociationID.String()}
	default:
		return "", fmt.Errorf("store: CreateAnnotation: exactly one target must be set (session, entry, annotation, project, or association)")
	}

	if params.Provenance != nil {
		b, err := json.Marshal(params.Provenance)
		if err != nil {
			return "", fmt.Errorf("store: CreateAnnotation: marshal provenance: %w", err)
		}
		provenanceJSON = string(b)
	}

	var isPrimaryInt int
	if params.IsPrimary {
		isPrimaryInt = 1
	}

	nowMs := time.Now().UnixMilli()

	// Transaction: parent + TPT child must succeed or fail together.
	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	// Insert parent annotation row.
	if err = sqlitex.ExecuteTransient(conn, sqlInsertAnnotation, &sqlitex.ExecOptions{
		Args: []any{
			newID, targetKindName,
			params.AnnotationTypeID, params.AnnotatorID, params.Value,
			ptrFloat64ToAny(params.Confidence), ptrStringToAny(params.Reason), provenanceJSON,
			isPrimaryInt, nowMs,
		},
	}); err != nil {
		return "", fmt.Errorf("store: CreateAnnotation: %w", err)
	}

	// Insert TPT child row.
	if err = sqlitex.ExecuteTransient(conn, childSQL, &sqlitex.ExecOptions{
		Args: childArgs,
	}); err != nil {
		return "", fmt.Errorf("store: CreateAnnotation: insert target: %w", err)
	}

	return newID, nil
}

func validateCreateAnnotationTarget(params CreateAnnotationParams) error {
	count := 0
	if params.SessionID != nil {
		count++
	}
	if params.EntryTarget != nil {
		count++
	}
	if params.AnnotationID != nil {
		count++
	}
	if params.ProjectHash != nil {
		count++
	}
	if params.AssociationID != nil {
		count++
		if err := params.AssociationID.Validate(); err != nil {
			return fmt.Errorf("store: CreateAnnotation: association target validation: %w", err)
		}
	}
	if count != 1 {
		return fmt.Errorf("store: CreateAnnotation: exactly one target must be set; received %d target arms, so the annotation cannot be persisted until one exclusive session, entry, annotation, project, or association target is supplied", count)
	}
	return nil
}

// GetAnnotationsForSession returns all non-superseded annotations targeting sessionID,
// ordered by created_at DESC.
func (s *Store) GetAnnotationsForSession(ctx context.Context, sessionID string) ([]AnnotationRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []AnnotationRow
	if err := sqlitex.ExecuteTransient(conn, sqlGetAnnotationsForSession, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, err := scanAnnotationRow(stmt)
			if err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetAnnotationsForSession %s: %w", sessionID, err)
	}
	return rows, nil
}

// GetAssociationAnnotationsForSession returns non-superseded annotations whose
// normalized association target belongs to sessionID. It is intentionally a
// separate read from GetSessionAnnotationsBulk so association annotations stay
// out of session metric and median calculations.
func (s *Store) GetAssociationAnnotationsForSession(ctx context.Context, sessionID string) ([]AnnotationRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []AnnotationRow
	if err := sqlitex.ExecuteTransient(conn, sqlGetAssociationAnnotationsForSession, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, scanErr := scanAnnotationRow(stmt)
			if scanErr != nil {
				return scanErr
			}
			rows = append(rows, row)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetAssociationAnnotationsForSession %s: %w", sessionID, err)
	}
	return rows, nil
}

// GetSessionAnnotationsBulk returns every non-superseded session-level
// annotation, grouped by target session, in one statement. The quality
// snapshot annotates ALL sessions at once — issuing GetAnnotationsForSession
// per session meant one serial round-trip per recorded session (minutes on a
// live store with thousands), re-paid on every annotation mutation. Each
// group preserves GetAnnotationsForSession's created_at DESC order.
func (s *Store) GetSessionAnnotationsBulk(ctx context.Context) (map[string][]AnnotationRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	grouped := make(map[string][]AnnotationRow)
	if err := sqlitex.ExecuteTransient(conn, sqlGetAllSessionAnnotations, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, err := scanAnnotationRow(stmt)
			if err != nil {
				return err
			}
			if row.TargetSessionID != nil {
				grouped[*row.TargetSessionID] = append(grouped[*row.TargetSessionID], row)
			}
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetSessionAnnotationsBulk: %w", err)
	}
	return grouped, nil
}

// GetEntryAnnotationsForSession returns all non-superseded entry-level
// annotations targeting any turn of sessionID, newest first. This complements
// GetAnnotationsForSession (which returns only session-level annotations) so the
// REST endpoint can surface per-turn labels — including ingest-generated rule
// labels — to the viewer.
func (s *Store) GetEntryAnnotationsForSession(ctx context.Context, sessionID string) ([]AnnotationRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []AnnotationRow
	if err := sqlitex.ExecuteTransient(conn, sqlGetEntryAnnotationsForSession, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, err := scanAnnotationRow(stmt)
			if err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetEntryAnnotationsForSession %s: %w", sessionID, err)
	}
	return rows, nil
}

// GetAnnotationsForProject returns all non-superseded annotations targeting the
// given projectHash, ordered by created_at DESC.
func (s *Store) GetAnnotationsForProject(ctx context.Context, projectHash string) ([]AnnotationRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []AnnotationRow
	if err := sqlitex.ExecuteTransient(conn, sqlGetAnnotationsForProject, &sqlitex.ExecOptions{
		Args: []any{projectHash},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, err := scanAnnotationRow(stmt)
			if err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetAnnotationsForProject %s: %w", projectHash, err)
	}
	return rows, nil
}

// GetAnnotationsForEntry returns all non-superseded annotations targeting the given
// entry (sessionID + entryIndex), ordered by created_at DESC.
func (s *Store) GetAnnotationsForEntry(ctx context.Context, sessionID string, entryIndex int) ([]AnnotationRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []AnnotationRow
	if err := sqlitex.ExecuteTransient(conn, sqlGetAnnotationsForEntry, &sqlitex.ExecOptions{
		Args: []any{sessionID, entryIndex},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, err := scanAnnotationRow(stmt)
			if err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetAnnotationsForEntry %s[%d]: %w", sessionID, entryIndex, err)
	}
	return rows, nil
}

// GetEffectiveAnnotation returns the single highest-priority non-superseded annotation
// for a session + type combination.
// Priority: human(3) > agent(2) > rule(1); ties broken by most recent created_at.
// Returns nil, nil if no annotation exists for this combination.
func (s *Store) GetEffectiveAnnotation(ctx context.Context, sessionID string, typeID string) (*AnnotationRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var result *AnnotationRow
	if err := sqlitex.ExecuteTransient(conn, sqlGetEffectiveAnnotation, &sqlitex.ExecOptions{
		Args: []any{sessionID, typeID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, err := scanAnnotationRow(stmt)
			if err != nil {
				return err
			}
			result = &row
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetEffectiveAnnotation %s/%s: %w", sessionID, typeID, err)
	}
	return result, nil
}

// scanAnnotationPushRow scans one annotations_with_target row into an
// ingest.AnnotationPushRow per the column layout documented on
// annotationPushRowColumns (cols 0-17). It is the single shared scanner for the
// system-origin and superseded variants, which differ only in their WHERE
// predicate — so the two list queries stay byte-for-byte consistent in how a row
// becomes an AnnotationPushRow (and recompute the SAME content hash).
func scanAnnotationPushRow(stmt *sqlite.Stmt) (ingest.AnnotationPushRow, error) {
	row := ingest.AnnotationPushRow{
		ID:            stmt.ColumnText(0),
		TargetKind:    schema.TargetKind(stmt.ColumnText(1)),
		TypeID:        stmt.ColumnText(10),
		Value:         stmt.ColumnText(11),
		IsPrimary:     stmt.ColumnInt(12) != 0,
		AnnotatorName: stmt.ColumnText(15),
	}

	// target_session_id (col 2)
	if stmt.ColumnType(2) != sqlite.TypeNull {
		v := stmt.ColumnText(2)
		row.SessionID = &v
	}
	// target_entry_session_id + entry_index + end_index (cols 3-5)
	if stmt.ColumnType(3) != sqlite.TypeNull {
		entrySessionID := stmt.ColumnText(3)
		row.SessionID = &entrySessionID
		idx := stmt.ColumnInt(4)
		row.EntryIndex = &idx
		endIdx := stmt.ColumnInt(5)
		row.EntryEndIndex = &endIdx
	}
	// target_annotation_id (col 6)
	if stmt.ColumnType(6) != sqlite.TypeNull {
		v := stmt.ColumnText(6)
		row.AnnotationID = &v
	}
	// target_project_hash (col 7)
	if stmt.ColumnType(7) != sqlite.TypeNull {
		v := stmt.ColumnText(7)
		row.ProjectHash = &v
	}
	// target_association_id (col 8)
	if stmt.ColumnType(8) != sqlite.TypeNull {
		id, err := schema.NewAssociationID(stmt.ColumnText(8))
		if err != nil {
			return ingest.AnnotationPushRow{}, fmt.Errorf("store: scan annotation association target: %w", err)
		}
		row.TargetAssociationID = &id
	}
	// target_association_session_id (col 9) is local selection context only.
	if stmt.ColumnType(9) != sqlite.TypeNull {
		sessionID := ingest.SessionID(stmt.ColumnText(9))
		row.AssociationSessionID = &sessionID
	}
	// confidence (col 13)
	if stmt.ColumnType(13) != sqlite.TypeNull {
		v := stmt.ColumnFloat(13)
		row.Confidence = &v
	}
	// reason (col 14)
	if stmt.ColumnType(14) != sqlite.TypeNull {
		v := stmt.ColumnText(14)
		row.Reason = &v
	}
	// provenance (col 16)
	if stmt.ColumnType(16) != sqlite.TypeNull {
		raw := stmt.ColumnText(16)
		var prov schema.Provenance
		if err := json.Unmarshal([]byte(raw), &prov); err != nil {
			return ingest.AnnotationPushRow{}, fmt.Errorf("store: scan annotation provenance: %w", err)
		}
		row.Provenance = &prov
	}
	// content_hash (col 17)
	if stmt.ColumnType(17) != sqlite.TypeNull {
		v := stmt.ColumnText(17)
		row.ContentHash = &v
	}

	return row, nil
}

// listAnnotationPushRows runs query (which MUST SELECT annotationPushRowColumns)
// and scans every row via scanAnnotationPushRow. label names the caller for error
// context. Shared by ListSystemAnnotations and ListSupersededAnnotations.
func (s *Store) listAnnotationPushRows(ctx context.Context, query, label string) ([]ingest.AnnotationPushRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []ingest.AnnotationPushRow
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, err := scanAnnotationPushRow(stmt)
			if err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: %s: %w", label, err)
	}
	return rows, nil
}

// ListSystemAnnotations returns all non-superseded annotations whose annotation type
// has system origin. Used by the annotation push pipeline to build the push payload.
// Filters are enforced server-side via sqlListSystemAnnotations JOIN on type_origins.
func (s *Store) ListSystemAnnotations(ctx context.Context) ([]ingest.AnnotationPushRow, error) {
	return s.listAnnotationPushRows(ctx, sqlListSystemAnnotations, "ListSystemAnnotations")
}

// ListSupersededAnnotations returns all SUPERSEDED system-origin annotations —
// the retraction source for the annotation push. It is the exact
// complement of ListSystemAnnotations (which filters superseded_by IS NULL): each
// returned row still carries the content-bearing fields it was pushed with, so the
// caller can recompute the SAME content hash the village stored and retract only
// what this machine locally retired.
func (s *Store) ListSupersededAnnotations(ctx context.Context) ([]ingest.AnnotationPushRow, error) {
	return s.listAnnotationPushRows(ctx, sqlListSupersededAnnotations, "ListSupersededAnnotations")
}

// Compile-time guard: *Store must satisfy ingest.AnnotationQueryStore.
var _ ingest.AnnotationQueryStore = (*Store)(nil)

// Compile-time guard: *Store must satisfy the optional classifier annotation
// batch persistence capability.
var _ ingest.ClassifierAnnotationBatchStore = (*Store)(nil)

// Compile-time guard: *Store can report profile detail for classifier batch writes.
var _ ingest.ProfiledClassifierAnnotationBatchStore = (*Store)(nil)

// Compile-time guards: *Store supports flushing prepared annotations for many
// sessions in one serial writer call.
var _ ingest.ClassifierAnnotationSessionBatchStore = (*Store)(nil)
var _ ingest.ProfiledClassifierAnnotationSessionBatchStore = (*Store)(nil)

// ---------------------------------------------------------------------------
// Dedup / Supersession (R9)
// ---------------------------------------------------------------------------

// FindExistingAnnotation looks up the most recent non-superseded annotation
// matching the given (annotation_type_id, annotator_id, target) triple.
// For session-level annotations, SessionID must be set.
// For entry-level annotations, SessionID and EntryIndex must both be set.
// Returns nil, nil if no matching annotation exists.
func (s *Store) FindExistingAnnotation(ctx context.Context, p ingest.FindAnnotationParams) (*ingest.ExistingAnnotation, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var query string
	var args []any

	if p.EntryIndex != nil && p.SessionID != nil {
		// Entry-level annotation lookup.
		query = sqlFindExistingEntryAnnotation
		args = []any{p.AnnotationTypeID, p.AnnotatorID, *p.SessionID, *p.EntryIndex}
	} else if p.SessionID != nil {
		// Session-level annotation lookup.
		query = sqlFindExistingSessionAnnotation
		args = []any{p.AnnotationTypeID, p.AnnotatorID, *p.SessionID}
	} else {
		return nil, fmt.Errorf("store: FindExistingAnnotation: SessionID is required")
	}

	result, err := findExistingAnnotationOnConn(conn, query, args)
	if err != nil {
		return nil, fmt.Errorf("store: FindExistingAnnotation: %w", err)
	}
	return result, nil
}

func findExistingAnnotationOnConn(conn *sqlite.Conn, query string, args []any) (*ingest.ExistingAnnotation, error) {
	var result *ingest.ExistingAnnotation
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			result = &ingest.ExistingAnnotation{
				ID:          stmt.ColumnText(0),
				ContentHash: stmt.ColumnText(1),
			}
			return nil
		},
	}); err != nil {
		return nil, err
	}
	return result, nil
}

// SupersedeAnnotation marks the annotation with oldID as superseded by newID.
// Sets superseded_by = newID and updated_at = current time on the old annotation.
func (s *Store) SupersedeAnnotation(ctx context.Context, oldID, newID string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	nowMs := time.Now().UnixMilli()
	if err := sqlitex.ExecuteTransient(conn, sqlSupersedeAnnotation, &sqlitex.ExecOptions{
		Args: []any{newID, nowMs, oldID},
	}); err != nil {
		return fmt.Errorf("store: SupersedeAnnotation %s → %s: %w", oldID, newID, err)
	}
	return nil
}

// collectAnnotationIDsByAnnotator is a shared helper that collects annotation IDs for
// the given annotator, optionally scoped to a set of session IDs. It queries direct
// session and entry targets plus association targets resolved through the durable
// session-to-commit ledger (via a UNION) when sessionIDs is non-empty, matching the
// PruneSessions pattern.
//
// Returns an empty slice (not an error) when the annotator is not found or has no matches.
func (s *Store) collectAnnotationIDsByAnnotator(conn *sqlite.Conn, annotatorName string, sessionIDs []string) ([]string, error) {
	var ids []string
	if len(sessionIDs) == 0 {
		// Unscoped: collect all annotations for this annotator.
		if err := sqlitex.ExecuteTransient(conn, sqlCollectAnnotationIDsByAnnotatorUnscoped, &sqlitex.ExecOptions{
			Args: []any{annotatorName},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				ids = append(ids, stmt.ColumnText(0))
				return nil
			},
		}); err != nil {
			return nil, fmt.Errorf("collect annotation IDs (unscoped) for annotator %s: %w", annotatorName, err)
		}
	} else {
		// Scoped: annotations targeting specific sessions through direct session, entry,
		// or association target tables.
		placeholders := make([]string, len(sessionIDs))
		for i := range sessionIDs {
			placeholders[i] = "?"
		}
		inClause := strings.Join(placeholders, ",")
		// Repeat the session ID args for each UNION arm: direct session targets,
		// entry targets, and association targets resolved through the durable ledger.
		args := make([]any, 1+len(sessionIDs)*3)
		args[0] = annotatorName
		for i, sid := range sessionIDs {
			args[1+i] = sid
			args[1+len(sessionIDs)+i] = sid
			args[1+len(sessionIDs)*2+i] = sid
		}
		query := fmt.Sprintf(sqlCollectAnnotationIDsByAnnotatorScopedFmt, inClause, inClause, inClause)
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			Args: args,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				ids = append(ids, stmt.ColumnText(0))
				return nil
			},
		}); err != nil {
			return nil, fmt.Errorf("collect annotation IDs (scoped) for annotator %s: %w", annotatorName, err)
		}
	}
	return ids, nil
}

// deleteAnnotationClosure removes direct annotation roots and every recursively
// nested meta-annotation that targets them. The caller must already hold the
// transaction so reference clearing and deletion remain atomic.
func deleteAnnotationClosure(conn *sqlite.Conn, rootIDs []string) error {
	if len(rootIDs) == 0 {
		return nil
	}
	placeholders := make([]string, len(rootIDs))
	args := make([]any, len(rootIDs))
	for i, id := range rootIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	seed := strings.Join(placeholders, ",")
	closure := `WITH RECURSIVE annotation_closure(id) AS (
    SELECT id FROM annotations WHERE id IN (` + seed + `)
    UNION
    SELECT targets.annotation_id
    FROM annotation_target_annotations targets
    JOIN annotation_closure parents ON parents.id = targets.target_annotation_id
) `
	if err := sqlitex.ExecuteTransient(conn,
		closure+`UPDATE annotations SET superseded_by = NULL WHERE superseded_by IN (SELECT id FROM annotation_closure)`,
		&sqlitex.ExecOptions{Args: args},
	); err != nil {
		return fmt.Errorf("clear superseded_by references into annotation closure: %w", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		closure+`DELETE FROM annotations WHERE id IN (SELECT id FROM annotation_closure)`,
		&sqlitex.ExecOptions{Args: args},
	); err != nil {
		return fmt.Errorf("delete annotation closure: %w", err)
	}
	return nil
}

// CreateAnnotationAndSupersede creates a new annotation, supersedes oldID, and sets the
// content_hash — all within a single SQLite transaction. This prevents the create/supersede
// split from leaving a window where a re-import could insert a duplicate before supersession.
//
// p uses ingest.CreateAnnotationParams; exactly one of SessionID or EntryTarget must be set.
// oldID is the UUID of the annotation to mark as superseded.
// contentHash is the SHA3-256 hash to store on the new annotation.
// Returns the new annotation's UUID.
func (s *Store) CreateAnnotationAndSupersede(ctx context.Context, p ingest.CreateAnnotationParams, oldID, contentHash string) (retID string, err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return "", fmt.Errorf("store: CreateAnnotationAndSupersede: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	newID := uuid.New().String()
	nowMs := time.Now().UnixMilli()

	// Translate ingest.CreateAnnotationParams → target kind name and TPT child args.
	var (
		targetKindName string
		childSQL       string
		childArgs      []any
		provenanceJSON any
	)

	switch {
	case p.SessionID != nil:
		targetKindName = "session"
		childSQL = sqlInsertTargetSession
		childArgs = []any{newID, *p.SessionID}
	case p.EntryTarget != nil:
		targetKindName = "entry"
		endIdx := p.EntryTarget.EndIndex
		if endIdx == 0 {
			endIdx = p.EntryTarget.EntryIndex + 1
		}
		childSQL = sqlInsertTargetEntry
		childArgs = []any{newID, p.EntryTarget.SessionID, p.EntryTarget.EntryIndex, endIdx}
	default:
		return "", fmt.Errorf("store: CreateAnnotationAndSupersede: exactly one target must be set (session or entry)")
	}

	if p.Provenance != nil {
		b, merr := json.Marshal(p.Provenance)
		if merr != nil {
			return "", fmt.Errorf("store: CreateAnnotationAndSupersede: marshal provenance: %w", merr)
		}
		provenanceJSON = string(b)
	}

	// Defers run LIFO: endFn (transaction commit/rollback) defers before pool.Put,
	// so the transaction closes before the connection returns to the pool.
	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	// 1. Insert new annotation parent row (is_primary=0 for import-originated annotations).
	if err = sqlitex.ExecuteTransient(conn, sqlInsertAnnotation, &sqlitex.ExecOptions{
		Args: []any{
			newID, targetKindName,
			p.AnnotationTypeID, p.AnnotatorID, p.Value,
			ptrFloat64ToAny(p.Confidence), ptrStringToAny(p.Reason), provenanceJSON,
			0, nowMs,
		},
	}); err != nil {
		return "", fmt.Errorf("store: CreateAnnotationAndSupersede: insert annotation: %w", err)
	}

	// 2. Insert TPT child row.
	if err = sqlitex.ExecuteTransient(conn, childSQL, &sqlitex.ExecOptions{
		Args: childArgs,
	}); err != nil {
		return "", fmt.Errorf("store: CreateAnnotationAndSupersede: insert target: %w", err)
	}

	// 3. Supersede old annotation: set superseded_by = newID and updated_at = now.
	if err = sqlitex.ExecuteTransient(conn, sqlSupersedeAnnotation, &sqlitex.ExecOptions{
		Args: []any{newID, nowMs, oldID},
	}); err != nil {
		return "", fmt.Errorf("store: CreateAnnotationAndSupersede: supersede %s → %s: %w", oldID, newID, err)
	}

	// 4. Set content_hash on the new annotation.
	if err = sqlitex.ExecuteTransient(conn, sqlUpdateContentHash, &sqlitex.ExecOptions{
		Args: []any{contentHash, newID},
	}); err != nil {
		return "", fmt.Errorf("store: CreateAnnotationAndSupersede: set content_hash on %s: %w", newID, err)
	}

	return newID, nil
}

// ApplyClassifierAnnotations persists one session's classifier results in one
// SQLite transaction. Each classifier result is protected by a savepoint so a
// validation or insert failure for one result is reported on that result and does
// not roll back other results from the same Annotate call.
func (s *Store) ApplyClassifierAnnotations(ctx context.Context, writes []ingest.ClassifierAnnotationWrite) []ingest.ClassifierAnnotationWriteResult {
	return s.applyClassifierAnnotations(ctx, writes, nil)
}

func (s *Store) ApplyClassifierAnnotationsWithProfile(ctx context.Context, writes []ingest.ClassifierAnnotationWrite, stats *ingest.AnnotationProfileStats) []ingest.ClassifierAnnotationWriteResult {
	return s.applyClassifierAnnotations(ctx, writes, stats)
}

func (s *Store) ApplyClassifierAnnotationBatches(ctx context.Context, batches []ingest.SessionAnnotationBatch) []ingest.SessionAnnotationBatchResult {
	return s.applyClassifierAnnotationBatches(ctx, batches, nil)
}

func (s *Store) ApplyClassifierAnnotationBatchesWithProfile(ctx context.Context, batches []ingest.SessionAnnotationBatch, stats *ingest.AnnotationProfileStats) []ingest.SessionAnnotationBatchResult {
	return s.applyClassifierAnnotationBatches(ctx, batches, stats)
}

func (s *Store) applyClassifierAnnotations(ctx context.Context, writes []ingest.ClassifierAnnotationWrite, stats *ingest.AnnotationProfileStats) []ingest.ClassifierAnnotationWriteResult {
	results := make([]ingest.ClassifierAnnotationWriteResult, len(writes))
	if len(writes) == 0 {
		return results
	}

	mutexStarted := annotationBatchProfileStart(stats)
	s.annotationWriteMu.Lock()
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchMutexWaitCount++
		s.BatchMutexWaitTime += time.Since(mutexStarted)
	})
	defer s.annotationWriteMu.Unlock()

	connStarted := annotationBatchProfileStart(stats)
	conn, err := s.pool.Take(ctx)
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchConnectionCount++
		s.BatchConnectionTime += time.Since(connStarted)
	})
	if err != nil {
		setupErr := fmt.Errorf("store: take connection for classifier annotation batch: %w", err)
		for i := range results {
			results[i].Err = setupErr
		}
		return results
	}
	defer s.pool.Put(conn)

	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	txnOpen := true
	defer func() {
		if txnOpen {
			endFn(&txnErr)
		}
	}()

	var fatalErr error
	results, fatalErr = applyClassifierAnnotationWritesOnConn(conn, writes, stats)
	if fatalErr != nil {
		txnErr = fatalErr
	}

	commitStarted := annotationBatchProfileStart(stats)
	endFn(&txnErr)
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchCommitCount++
		s.BatchCommitTime += time.Since(commitStarted)
	})
	txnOpen = false
	if txnErr != nil {
		commitErr := fmt.Errorf("store: commit classifier annotation batch: %w", txnErr)
		for i := range results {
			if results[i].Err == nil {
				results[i].Err = commitErr
			}
			results[i].AnnotationID = ""
		}
	}
	return results
}

func (s *Store) applyClassifierAnnotationBatches(ctx context.Context, batches []ingest.SessionAnnotationBatch, stats *ingest.AnnotationProfileStats) []ingest.SessionAnnotationBatchResult {
	results := make([]ingest.SessionAnnotationBatchResult, len(batches))
	for i, batch := range batches {
		results[i].SessionID = batch.SessionID
	}
	if len(batches) == 0 {
		return results
	}

	mutexStarted := annotationBatchProfileStart(stats)
	s.annotationWriteMu.Lock()
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchMutexWaitCount++
		s.BatchMutexWaitTime += time.Since(mutexStarted)
	})
	defer s.annotationWriteMu.Unlock()

	connStarted := annotationBatchProfileStart(stats)
	conn, err := s.pool.Take(ctx)
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchConnectionCount++
		s.BatchConnectionTime += time.Since(connStarted)
	})
	if err != nil {
		setupErr := fmt.Errorf("store: take connection for classifier annotation batch: %w", err)
		for i := range results {
			results[i].Err = setupErr
			results[i].Results = classifierAnnotationFailedResults(len(batches[i].Writes), setupErr)
		}
		return results
	}
	defer s.pool.Put(conn)

	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	txnOpen := true
	defer func() {
		if txnOpen {
			endFn(&txnErr)
		}
	}()

	for i, batch := range batches {
		storeWrites := sessionAnnotationBatchStoreWrites(batch.Writes)
		writeResults, fatalErr := applyClassifierAnnotationWritesOnConn(conn, storeWrites, stats)
		results[i].Results = writeResults
		if fatalErr != nil {
			results[i].Err = fatalErr
			txnErr = fatalErr
			break
		}
		if classifierAnnotationResultsHaveError(writeResults) || batch.RunState == nil {
			continue
		}
		if err := saveAnnotationRunStateOnConn(conn, *batch.RunState); err != nil {
			results[i].Err = fmt.Errorf("store: save annotation state in classifier annotation batch: %w", err)
		}
	}

	commitStarted := annotationBatchProfileStart(stats)
	endFn(&txnErr)
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchCommitCount++
		s.BatchCommitTime += time.Since(commitStarted)
	})
	txnOpen = false
	if txnErr != nil {
		commitErr := fmt.Errorf("store: commit classifier annotation batch: %w", txnErr)
		for i := range results {
			if results[i].Err == nil {
				results[i].Err = commitErr
			}
			if len(results[i].Results) == 0 && len(batches[i].Writes) != 0 {
				results[i].Results = classifierAnnotationFailedResults(len(batches[i].Writes), commitErr)
			}
			for j := range results[i].Results {
				if results[i].Results[j].Err == nil {
					results[i].Results[j].Err = commitErr
				}
				results[i].Results[j].AnnotationID = ""
			}
		}
	}
	return results
}

func applyClassifierAnnotationWritesOnConn(conn *sqlite.Conn, writes []ingest.ClassifierAnnotationWrite, stats *ingest.AnnotationProfileStats) ([]ingest.ClassifierAnnotationWriteResult, error) {
	results := make([]ingest.ClassifierAnnotationWriteResult, len(writes))
	for i := range writes {
		outcome, err, fatal := applyClassifierAnnotationSavepoint(conn, writes[i], stats)
		results[i] = outcome
		if err != nil {
			results[i].Err = err
			if fatal {
				return results, err
			}
		}
	}
	return results, nil
}

func sessionAnnotationBatchStoreWrites(writes []ingest.SessionAnnotationWrite) []ingest.ClassifierAnnotationWrite {
	storeWrites := make([]ingest.ClassifierAnnotationWrite, len(writes))
	for i, write := range writes {
		storeWrites[i] = write.Write
	}
	return storeWrites
}

func classifierAnnotationResultsHaveError(results []ingest.ClassifierAnnotationWriteResult) bool {
	for _, result := range results {
		if result.Err != nil {
			return true
		}
	}
	return false
}

func classifierAnnotationFailedResults(count int, err error) []ingest.ClassifierAnnotationWriteResult {
	results := make([]ingest.ClassifierAnnotationWriteResult, count)
	for i := range results {
		results[i].Err = err
	}
	return results
}

func applyClassifierAnnotationSavepoint(conn *sqlite.Conn, write ingest.ClassifierAnnotationWrite, stats *ingest.AnnotationProfileStats) (ingest.ClassifierAnnotationWriteResult, error, bool) {
	const savepointName = "classifier_annotation_batch_item"
	result := ingest.ClassifierAnnotationWriteResult{}
	savepointStarted := annotationBatchProfileStart(stats)
	if err := sqlitex.ExecuteTransient(conn, "SAVEPOINT "+savepointName, nil); err != nil {
		recordBatchSavepointProfile(stats, &result.Profile, time.Since(savepointStarted))
		return result, fmt.Errorf("store: start classifier annotation savepoint: %w", err), true
	}
	recordBatchSavepointProfile(stats, &result.Profile, time.Since(savepointStarted))

	applied, err := applyClassifierAnnotationOnConn(conn, write, stats)
	applied.Profile.Add(result.Profile)
	result = applied
	if err != nil {
		rollbackErr, fatal := rollbackClassifierAnnotationSavepoint(conn, savepointName, err, stats, &result.Profile)
		return result, rollbackErr, fatal
	}
	releaseStarted := annotationBatchProfileStart(stats)
	if err := sqlitex.ExecuteTransient(conn, "RELEASE SAVEPOINT "+savepointName, nil); err != nil {
		recordBatchSavepointProfile(stats, &result.Profile, time.Since(releaseStarted))
		return result, fmt.Errorf("store: release classifier annotation savepoint: %w", err), true
	}
	recordBatchSavepointProfile(stats, &result.Profile, time.Since(releaseStarted))
	return result, nil, false
}

func rollbackClassifierAnnotationSavepoint(conn *sqlite.Conn, savepointName string, cause error, stats *ingest.AnnotationProfileStats, profile *ingest.ClassifierAnnotationWriteProfile) (error, bool) {
	rollbackStarted := annotationBatchProfileStart(stats)
	rollbackErr := sqlitex.ExecuteTransient(conn, "ROLLBACK TO SAVEPOINT "+savepointName, nil)
	recordBatchSavepointProfile(stats, profile, time.Since(rollbackStarted))
	releaseStarted := annotationBatchProfileStart(stats)
	releaseErr := sqlitex.ExecuteTransient(conn, "RELEASE SAVEPOINT "+savepointName, nil)
	recordBatchSavepointProfile(stats, profile, time.Since(releaseStarted))
	if rollbackErr != nil || releaseErr != nil {
		errs := []error{fmt.Errorf("store: classifier annotation savepoint failed: %w", cause)}
		if rollbackErr != nil {
			errs = append(errs, fmt.Errorf("store: rollback classifier annotation savepoint: %w", rollbackErr))
		}
		if releaseErr != nil {
			errs = append(errs, fmt.Errorf("store: release classifier annotation savepoint: %w", releaseErr))
		}
		return errors.Join(errs...), true
	}
	return cause, false
}

func applyClassifierAnnotationOnConn(conn *sqlite.Conn, write ingest.ClassifierAnnotationWrite, stats *ingest.AnnotationProfileStats) (ingest.ClassifierAnnotationWriteResult, error) {
	result := ingest.ClassifierAnnotationWriteResult{Dedup: ingest.DedupCreate}
	if write.ContentHash == "" {
		return result, fmt.Errorf("store: classifier annotation write requires non-empty content hash")
	}

	query, args, err := classifierAnnotationFindQuery(write.Find)
	if err != nil {
		return result, err
	}
	dedupStarted := annotationBatchProfileStart(stats)
	existing, err := findExistingAnnotationOnConn(conn, query, args)
	recordBatchDedupLookupProfile(stats, &result.Profile, time.Since(dedupStarted))
	if err != nil {
		return result, fmt.Errorf("store: find existing classifier annotation: %w", err)
	}
	if existing != nil {
		result.ExistingAnnotationID = existing.ID
		if existing.ContentHash == write.ContentHash {
			result.Dedup = ingest.DedupSkip
			result.AnnotationID = existing.ID
			return result, nil
		}
		result.Dedup = ingest.DedupSupersede
	}

	newID, err := createClassifierAnnotationOnConn(conn, write.Create, write.ContentHash, stats, &result.Profile)
	if err != nil {
		return result, err
	}
	result.AnnotationID = newID
	if existing != nil {
		nowMs := time.Now().UnixMilli()
		supersedeStarted := annotationBatchProfileStart(stats)
		if err := sqlitex.ExecuteTransient(conn, sqlSupersedeAnnotation, &sqlitex.ExecOptions{
			Args: []any{newID, nowMs, existing.ID},
		}); err != nil {
			recordBatchSupersedeProfile(stats, &result.Profile, time.Since(supersedeStarted))
			return result, fmt.Errorf("store: supersede classifier annotation %s with %s: %w", existing.ID, newID, err)
		}
		recordBatchSupersedeProfile(stats, &result.Profile, time.Since(supersedeStarted))
	}
	return result, nil
}

func classifierAnnotationFindQuery(p ingest.FindAnnotationParams) (string, []any, error) {
	switch {
	case p.EntryIndex != nil && p.SessionID != nil:
		return sqlFindExistingEntryAnnotation, []any{p.AnnotationTypeID, p.AnnotatorID, *p.SessionID, *p.EntryIndex}, nil
	case p.SessionID != nil:
		return sqlFindExistingSessionAnnotation, []any{p.AnnotationTypeID, p.AnnotatorID, *p.SessionID}, nil
	default:
		return "", nil, fmt.Errorf("store: classifier annotation find requires a session target")
	}
}

func createClassifierAnnotationOnConn(conn *sqlite.Conn, p ingest.CreateAnnotationParams, contentHash string, stats *ingest.AnnotationProfileStats, profile *ingest.ClassifierAnnotationWriteProfile) (string, error) {
	newID := uuid.New().String()
	targetKindName, childSQL, childArgs, provenanceJSON, err := classifierAnnotationInsertParts(newID, p)
	if err != nil {
		return "", err
	}
	nowMs := time.Now().UnixMilli()
	parentStarted := annotationBatchProfileStart(stats)
	if err := sqlitex.ExecuteTransient(conn, sqlInsertAnnotation, &sqlitex.ExecOptions{
		Args: []any{
			newID, targetKindName,
			p.AnnotationTypeID, p.AnnotatorID, p.Value,
			ptrFloat64ToAny(p.Confidence), ptrStringToAny(p.Reason), provenanceJSON,
			0, nowMs,
		},
	}); err != nil {
		recordBatchInsertParentProfile(stats, profile, time.Since(parentStarted))
		return "", fmt.Errorf("store: insert classifier annotation: %w", err)
	}
	recordBatchInsertParentProfile(stats, profile, time.Since(parentStarted))
	targetStarted := annotationBatchProfileStart(stats)
	if err := sqlitex.ExecuteTransient(conn, childSQL, &sqlitex.ExecOptions{Args: childArgs}); err != nil {
		recordBatchInsertTargetProfile(stats, profile, time.Since(targetStarted))
		return "", fmt.Errorf("store: insert classifier annotation target: %w", err)
	}
	recordBatchInsertTargetProfile(stats, profile, time.Since(targetStarted))
	hashStarted := annotationBatchProfileStart(stats)
	if err := sqlitex.ExecuteTransient(conn, sqlUpdateContentHash, &sqlitex.ExecOptions{Args: []any{contentHash, newID}}); err != nil {
		recordBatchUpdateHashProfile(stats, profile, time.Since(hashStarted))
		return "", fmt.Errorf("store: set classifier annotation content hash on %s: %w", newID, err)
	}
	recordBatchUpdateHashProfile(stats, profile, time.Since(hashStarted))
	return newID, nil
}

func annotationBatchProfileStart(stats *ingest.AnnotationProfileStats) time.Time {
	if stats == nil {
		return time.Time{}
	}
	return time.Now()
}

func addAnnotationBatchProfile(stats *ingest.AnnotationProfileStats, add func(*ingest.AnnotationProfileStats)) {
	if stats != nil {
		add(stats)
	}
}

func recordBatchSavepointProfile(stats *ingest.AnnotationProfileStats, profile *ingest.ClassifierAnnotationWriteProfile, elapsed time.Duration) {
	if stats == nil {
		return
	}
	if profile != nil {
		profile.SavepointCount++
		profile.SavepointTime += elapsed
	}
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchSavepointCount++
		s.BatchSavepointTime += elapsed
	})
}

func recordBatchDedupLookupProfile(stats *ingest.AnnotationProfileStats, profile *ingest.ClassifierAnnotationWriteProfile, elapsed time.Duration) {
	if stats == nil {
		return
	}
	if profile != nil {
		profile.DedupLookupCount++
		profile.DedupLookupTime += elapsed
	}
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchDedupLookupCount++
		s.BatchDedupLookupTime += elapsed
	})
}

func recordBatchInsertParentProfile(stats *ingest.AnnotationProfileStats, profile *ingest.ClassifierAnnotationWriteProfile, elapsed time.Duration) {
	if stats == nil {
		return
	}
	if profile != nil {
		profile.InsertParentCount++
		profile.InsertParentTime += elapsed
	}
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchInsertParentCount++
		s.BatchInsertParentTime += elapsed
	})
}

func recordBatchInsertTargetProfile(stats *ingest.AnnotationProfileStats, profile *ingest.ClassifierAnnotationWriteProfile, elapsed time.Duration) {
	if stats == nil {
		return
	}
	if profile != nil {
		profile.InsertTargetCount++
		profile.InsertTargetTime += elapsed
	}
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchInsertTargetCount++
		s.BatchInsertTargetTime += elapsed
	})
}

func recordBatchUpdateHashProfile(stats *ingest.AnnotationProfileStats, profile *ingest.ClassifierAnnotationWriteProfile, elapsed time.Duration) {
	if stats == nil {
		return
	}
	if profile != nil {
		profile.UpdateHashCount++
		profile.UpdateHashTime += elapsed
	}
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchUpdateHashCount++
		s.BatchUpdateHashTime += elapsed
	})
}

func recordBatchSupersedeProfile(stats *ingest.AnnotationProfileStats, profile *ingest.ClassifierAnnotationWriteProfile, elapsed time.Duration) {
	if stats == nil {
		return
	}
	if profile != nil {
		profile.SupersedeCount++
		profile.SupersedeTime += elapsed
	}
	addAnnotationBatchProfile(stats, func(s *ingest.AnnotationProfileStats) {
		s.BatchSupersedeCount++
		s.BatchSupersedeTime += elapsed
	})
}

func classifierAnnotationInsertParts(newID string, p ingest.CreateAnnotationParams) (string, string, []any, any, error) {
	var provenanceJSON any
	if p.Provenance != nil {
		b, err := json.Marshal(p.Provenance)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("store: marshal classifier annotation provenance: %w", err)
		}
		provenanceJSON = string(b)
	}
	switch {
	case p.SessionID != nil && p.EntryTarget == nil:
		return "session", sqlInsertTargetSession, []any{newID, *p.SessionID}, provenanceJSON, nil
	case p.EntryTarget != nil && p.SessionID == nil:
		endIdx := p.EntryTarget.EndIndex
		if endIdx == 0 {
			endIdx = p.EntryTarget.EntryIndex + 1
		}
		return "entry", sqlInsertTargetEntry, []any{newID, p.EntryTarget.SessionID, p.EntryTarget.EntryIndex, endIdx}, provenanceJSON, nil
	default:
		return "", "", nil, nil, fmt.Errorf("store: classifier annotation requires exactly one session or entry target")
	}
}

// CountAnnotationsByAnnotator returns the number of annotations that would be deleted by
// DeleteAnnotationsByAnnotator with the same arguments, without modifying the database.
// Used by the --dry-run path of `peasant annotate prune`.
func (s *Store) CountAnnotationsByAnnotator(ctx context.Context, annotatorName string, sessionIDs []string) (int64, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: CountAnnotationsByAnnotator %s: take connection: %w", annotatorName, err)
	}
	defer s.pool.Put(conn)

	ids, err := s.collectAnnotationIDsByAnnotator(conn, annotatorName, sessionIDs)
	if err != nil {
		return 0, fmt.Errorf("store: CountAnnotationsByAnnotator %s: %w", annotatorName, err)
	}
	return int64(len(ids)), nil
}

// DeleteAnnotationsByAnnotator transactionally deletes all annotations by the named annotator,
// optionally scoped to a set of session IDs. Returns the count of annotations deleted.
//
// If annotatorName is not found in the DB, returns (0, nil) — not an error.
// If sessionIDs is nil or empty, all annotations by the annotator are deleted.
// If sessionIDs is non-empty, only annotations targeting those sessions are deleted.
//
// Cleanup collects direct roots, then atomically deletes their full recursive
// meta-annotation closure. The returned count includes direct roots only.
func (s *Store) DeleteAnnotationsByAnnotator(ctx context.Context, annotatorName string, sessionIDs []string) (int64, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: DeleteAnnotationsByAnnotator %s: take connection: %w", annotatorName, err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	// Step 1: Collect annotation IDs matching the filter.
	annIDs, err := s.collectAnnotationIDsByAnnotator(conn, annotatorName, sessionIDs)
	if err != nil {
		return 0, fmt.Errorf("store: DeleteAnnotationsByAnnotator %s: %w", annotatorName, err)
	}

	if len(annIDs) == 0 {
		// Annotator not found or no matching annotations — nothing to do.
		return 0, nil
	}

	if err = deleteAnnotationClosure(conn, annIDs); err != nil {
		return 0, fmt.Errorf("store: DeleteAnnotationsByAnnotator %s: %w", annotatorName, err)
	}

	return int64(len(annIDs)), nil
}

// UpdateContentHash sets the content_hash on an annotation.
func (s *Store) UpdateContentHash(ctx context.Context, annotationID, contentHash string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err := sqlitex.ExecuteTransient(conn, sqlUpdateContentHash, &sqlitex.ExecOptions{
		Args: []any{contentHash, annotationID},
	}); err != nil {
		return fmt.Errorf("store: UpdateContentHash %s: %w", annotationID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Annotator CRUD
// ---------------------------------------------------------------------------

// CreateAnnotator inserts a new annotator and returns its UUID.
func (s *Store) CreateAnnotator(ctx context.Context, params CreateAnnotatorParams) (string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return "", fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	newID := uuid.New().String()
	nowMs := time.Now().UnixMilli()
	if err := sqlitex.ExecuteTransient(conn, sqlInsertAnnotator, &sqlitex.ExecOptions{
		Args: []any{
			newID, params.Kind.String(), params.Name, params.DisplayName, params.Description,
			ptrStringToAny(params.ModelID), ptrStringToAny(params.ProviderKey), nowMs,
		},
	}); err != nil {
		return "", fmt.Errorf("store: CreateAnnotator %q: %w", params.Name, err)
	}
	return newID, nil
}

// GetAnnotator returns the annotator with the given name.
// Returns nil, nil if not found.
func (s *Store) GetAnnotator(ctx context.Context, name string) (*AnnotatorRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var result *AnnotatorRow
	if err := sqlitex.ExecuteTransient(conn, sqlGetAnnotatorByName, &sqlitex.ExecOptions{
		Args: []any{name},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row := scanAnnotatorRow(stmt)
			result = &row
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetAnnotator %q: %w", name, err)
	}
	return result, nil
}

// ListAnnotators returns all annotators ordered by name.
func (s *Store) ListAnnotators(ctx context.Context) ([]AnnotatorRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []AnnotatorRow
	if err := sqlitex.ExecuteTransient(conn, sqlListAnnotators, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, scanAnnotatorRow(stmt))
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: ListAnnotators: %w", err)
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// Annotation type CRUD (used by sqliteRegistry)
// ---------------------------------------------------------------------------

// GetAnnotationTypeByTypeID returns the annotation type row for the given type_id string.
// Returns nil, nil if not found.
func (s *Store) GetAnnotationTypeByTypeID(ctx context.Context, typeID string) (*AnnotationTypeRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var result *AnnotationTypeRow
	if err := sqlitex.ExecuteTransient(conn, sqlGetAnnotationTypeByTypeID, &sqlitex.ExecOptions{
		Args: []any{typeID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, err := scanAnnotationTypeRow(stmt)
			if err != nil {
				return err
			}
			result = &row
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetAnnotationTypeByTypeID %q: %w", typeID, err)
	}
	return result, nil
}

// GetAnnotationTypeByDBID returns the annotation type row for the given UUID PK.
// Returns nil, nil if not found.
func (s *Store) GetAnnotationTypeByDBID(ctx context.Context, dbID string) (*AnnotationTypeRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var result *AnnotationTypeRow
	if err := sqlitex.ExecuteTransient(conn, sqlGetAnnotationTypeByDBID, &sqlitex.ExecOptions{
		Args: []any{dbID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, err := scanAnnotationTypeRow(stmt)
			if err != nil {
				return err
			}
			result = &row
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetAnnotationTypeByDBID %q: %w", dbID, err)
	}
	return result, nil
}

// ListAnnotationTypes returns annotation type rows matching the given filter.
// Applies status and origin filters if set; otherwise returns all rows.
// V16: filters use INT lookup table names via JOIN-resolved columns.
func (s *Store) ListAnnotationTypes(ctx context.Context, statusFilter, originFilter string) ([]AnnotationTypeRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	// Build dynamic WHERE clause. The base query JOINs the lookup tables
	// so s.name and o.name are the resolved TEXT values.
	var args []any
	var conditions []string
	if statusFilter != "" {
		conditions = append(conditions, "s.name = ?")
		args = append(args, statusFilter)
	}
	if originFilter != "" {
		conditions = append(conditions, "o.name = ?")
		args = append(args, originFilter)
	}

	var q string
	if len(conditions) == 0 {
		q = sqlListAnnotationTypes + " ORDER BY t.type_id"
	} else {
		q = sqlListAnnotationTypes + " WHERE " + strings.Join(conditions, " AND ") + " ORDER BY t.type_id"
	}

	var rows []AnnotationTypeRow
	if err := sqlitex.ExecuteTransient(conn, q, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row, err := scanAnnotationTypeRow(stmt)
			if err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: ListAnnotationTypes: %w", err)
	}
	return rows, nil
}

// ListAnnotationTypesWithFamily returns annotation type rows with resolved family and class
// name strings (via JOIN with annotation_families and annotation_classes).
// Applies status and origin filters if set; otherwise returns all rows, ordered by type_id.
func (s *Store) ListAnnotationTypesWithFamily(ctx context.Context, statusFilter, originFilter string) ([]AnnotationTypeWithFamilyRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var args []any
	var conditions []string
	if statusFilter != "" {
		conditions = append(conditions, "s.name = ?")
		args = append(args, statusFilter)
	}
	if originFilter != "" {
		conditions = append(conditions, "o.name = ?")
		args = append(args, originFilter)
	}

	var q string
	if len(conditions) == 0 {
		q = sqlListAnnotationTypesWithFamily + " ORDER BY t.type_id"
	} else {
		q = sqlListAnnotationTypesWithFamily + " WHERE " + strings.Join(conditions, " AND ") + " ORDER BY t.type_id"
	}

	var rows []AnnotationTypeWithFamilyRow
	if err := sqlitex.ExecuteTransient(conn, q, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			base, err := scanAnnotationTypeRow(stmt)
			if err != nil {
				return err
			}
			rows = append(rows, AnnotationTypeWithFamilyRow{
				AnnotationTypeRow:  base,
				Family:             stmt.ColumnText(16), // V17: col shifted by 1 (scale_kind at col 15)
				Class:              stmt.ColumnText(17), // V17: col shifted by 1
				AllowedTargetKinds: parseAllowedTargetKinds(stmt.ColumnText(18)),
			})
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: ListAnnotationTypesWithFamily: %w", err)
	}
	return rows, nil
}

// GetAnnotationTypeWithFamilyByTypeID returns the annotation type with resolved family and class
// strings for the given type_id string. Returns nil, nil if no type matches.
func (s *Store) GetAnnotationTypeWithFamilyByTypeID(ctx context.Context, typeID string) (*AnnotationTypeWithFamilyRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var result *AnnotationTypeWithFamilyRow
	if err := sqlitex.ExecuteTransient(conn, sqlGetAnnotationTypeWithFamilyByTypeID, &sqlitex.ExecOptions{
		Args: []any{typeID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			base, err := scanAnnotationTypeRow(stmt)
			if err != nil {
				return err
			}
			r := AnnotationTypeWithFamilyRow{
				AnnotationTypeRow:  base,
				Family:             stmt.ColumnText(16), // V17: col shifted by 1 (scale_kind at col 15)
				Class:              stmt.ColumnText(17), // V17: col shifted by 1
				AllowedTargetKinds: parseAllowedTargetKinds(stmt.ColumnText(18)),
			}
			result = &r
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetAnnotationTypeWithFamilyByTypeID %q: %w", typeID, err)
	}
	return result, nil
}

// GetAnnotationTypeWithFamilyByDBID returns the annotation type with resolved family and class
// strings for the given UUID primary key. Returns nil, nil if no type matches.
func (s *Store) GetAnnotationTypeWithFamilyByDBID(ctx context.Context, dbID string) (*AnnotationTypeWithFamilyRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var result *AnnotationTypeWithFamilyRow
	if err := sqlitex.ExecuteTransient(conn, sqlGetAnnotationTypeWithFamilyByDBID, &sqlitex.ExecOptions{
		Args: []any{dbID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			base, err := scanAnnotationTypeRow(stmt)
			if err != nil {
				return err
			}
			r := AnnotationTypeWithFamilyRow{
				AnnotationTypeRow:  base,
				Family:             stmt.ColumnText(16), // V17: col shifted by 1 (scale_kind at col 15)
				Class:              stmt.ColumnText(17), // V17: col shifted by 1
				AllowedTargetKinds: parseAllowedTargetKinds(stmt.ColumnText(18)),
			}
			result = &r
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetAnnotationTypeWithFamilyByDBID %q: %w", dbID, err)
	}
	return result, nil
}

// CreateAnnotationType inserts a new annotation type (status=proposed) and returns its UUID.
func (s *Store) CreateAnnotationType(ctx context.Context, def CreateAnnotationTypeParams) (string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return "", fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	newID := uuid.New().String()
	nowMs := time.Now().UnixMilli()
	if err := sqlitex.ExecuteTransient(conn, sqlInsertAnnotationType, &sqlitex.ExecOptions{
		Args: []any{
			newID, def.TypeID, def.DisplayName, def.Description, def.FamilyID,
			def.ValueDomainKind.String(),
			def.Datatype.String(), def.ValueConstraint,
			boolPtrToInt(def.LowerIsBetter), def.Origin.String(), nowMs,
		},
	}); err != nil {
		return "", fmt.Errorf("store: CreateAnnotationType %q: %w", def.TypeID, err)
	}
	return newID, nil
}

// boolPtrToInt converts a *bool to an integer value for STRICT INTEGER columns.
// nil → nil (SQL NULL), true → 1, false → 0.
func boolPtrToInt(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}

// ptrStringToAny converts a *string to any for SQLite parameter binding.
// nil pointer → nil (SQL NULL); non-nil → dereferenced string value.
// Required because (*string)(nil) stored in any interface is a non-nil interface value,
// which SQLite STRICT mode rejects for TEXT or non-nullable columns.
func ptrStringToAny(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// ptrFloat64ToAny converts a *float64 to any for SQLite parameter binding.
// nil pointer → nil (SQL NULL); non-nil → dereferenced float64 value.
func ptrFloat64ToAny(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// CreateAnnotationTypeParams holds inputs for CreateAnnotationType.
type CreateAnnotationTypeParams struct {
	TypeID          string
	DisplayName     string
	Description     string
	FamilyID        string // V16: UUID FK to annotation_families.id
	ValueDomainKind schema.ValueDomainKind
	Datatype        schema.AnnotationDatatype
	ValueConstraint string // JSON-encoded permissible values or constraint spec
	LowerIsBetter   *bool
	Origin          schema.TypeOrigin
}

// ActivateAnnotationType transitions type from proposed → active.
// Returns nil if the type was already active (idempotent).
//
// TODO(annotation/S3): populate updated_at when lifecycle management ships.
func (s *Store) ActivateAnnotationType(ctx context.Context, typeID string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err := sqlitex.ExecuteTransient(conn, sqlActivateAnnotationType, &sqlitex.ExecOptions{
		Args: []any{typeID},
	}); err != nil {
		return fmt.Errorf("store: ActivateAnnotationType %q: %w", typeID, err)
	}
	return nil
}

// DeprecateAnnotationType transitions type from active → deprecated.
// supersededByTypeID is the type_id of the replacement.
//
// TODO(annotation/S3): populate deprecated_at and updated_at when lifecycle management ships.
func (s *Store) DeprecateAnnotationType(ctx context.Context, typeID, supersededByTypeID string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err := sqlitex.ExecuteTransient(conn, sqlDeprecateAnnotationType, &sqlitex.ExecOptions{
		Args: []any{supersededByTypeID, typeID},
	}); err != nil {
		return fmt.Errorf("store: DeprecateAnnotationType %q (superseded by %q): %w", typeID, supersededByTypeID, err)
	}
	return nil
}

// AddAnnotationTypeDependency inserts a dependency edge after running cycle detection.
// Returns ErrAnnotationCycle if adding the edge would create a cycle (V14).
// V16: uses TEXT UUID PKs for cycle detection (same recursive CTE, different ID types).
func (s *Store) AddAnnotationTypeDependency(ctx context.Context, typeID, dependsOnTypeID string, required bool, rationale string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	// Look up UUID PKs for cycle detection.
	var typeUUID, dependsOnUUID string
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT id FROM annotation_types WHERE type_id = ?`,
		&sqlitex.ExecOptions{
			Args: []any{typeID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				typeUUID = stmt.ColumnText(0)
				return nil
			},
		}); err != nil || typeUUID == "" {
		return fmt.Errorf("store: AddAnnotationTypeDependency: type %q not found", typeID)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT id FROM annotation_types WHERE type_id = ?`,
		&sqlitex.ExecOptions{
			Args: []any{dependsOnTypeID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				dependsOnUUID = stmt.ColumnText(0)
				return nil
			},
		}); err != nil || dependsOnUUID == "" {
		return fmt.Errorf("store: AddAnnotationTypeDependency: depends-on type %q not found", dependsOnTypeID)
	}

	// Run cycle detection: would adding typeID → dependsOnTypeID create a cycle?
	// We check if dependsOnTypeID's existing dep chain reaches back to typeID.
	var cycleFound bool
	var cyclePath string
	if err := sqlitex.ExecuteTransient(conn, cycleDetectionQuery, &sqlitex.ExecOptions{
		Args: []any{dependsOnUUID, typeUUID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			cycleFound = true
			cyclePath = stmt.ColumnText(1)
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: AddAnnotationTypeDependency: cycle detection: %w", err)
	}
	if cycleFound {
		return fmt.Errorf("store: AddAnnotationTypeDependency: %w: %s → %s would create cycle (path: %s)",
			ErrAnnotationCycle, typeID, dependsOnTypeID, cyclePath)
	}

	var requiredInt int
	if required {
		requiredInt = 1
	}
	if err := sqlitex.ExecuteTransient(conn, sqlInsertAnnotationTypeDep, &sqlitex.ExecOptions{
		Args: []any{typeID, dependsOnTypeID, requiredInt, rationale},
	}); err != nil {
		return fmt.Errorf("store: AddAnnotationTypeDependency %q → %q: %w", typeID, dependsOnTypeID, err)
	}
	return nil
}

// ErrAnnotationCycle is returned when adding a dependency would create a cycle.
var ErrAnnotationCycle = fmt.Errorf("annotation type dependency cycle detected")

// GetAnnotationTypeDependencies returns the dependency edges for the given type_id.
func (s *Store) GetAnnotationTypeDependencies(ctx context.Context, typeID string) ([]AnnotationTypeDep, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var deps []AnnotationTypeDep
	if err := sqlitex.ExecuteTransient(conn, sqlGetAnnotationTypeDeps, &sqlitex.ExecOptions{
		Args: []any{typeID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			deps = append(deps, AnnotationTypeDep{
				TypeID:    stmt.ColumnText(0),
				DependsOn: stmt.ColumnText(1),
				Required:  stmt.ColumnInt(2) != 0,
				Rationale: stmt.ColumnText(3),
			})
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: GetAnnotationTypeDependencies %q: %w", typeID, err)
	}
	return deps, nil
}

// AnnotationTypeDep is a store-native dependency row.
type AnnotationTypeDep struct {
	TypeID    string
	DependsOn string
	Required  bool
	Rationale string
}

// ---------------------------------------------------------------------------
// Scan helpers (V16 column layouts)
// ---------------------------------------------------------------------------

// scanAnnotationRow scans an annotations_with_target VIEW result row.
// Column order must match sqlAnnotationViewCols (cols 0-25).
func scanAnnotationRow(stmt *sqlite.Stmt) (AnnotationRow, error) {
	row := AnnotationRow{
		ID:               stmt.ColumnText(0),
		TargetKind:       schema.TargetKind(stmt.ColumnText(1)),
		AnnotatorID:      stmt.ColumnText(9),
		AnnotatorKind:    schema.AnnotatorKind(stmt.ColumnText(10)),
		AnnotatorName:    stmt.ColumnText(11),
		AnnotationTypeID: stmt.ColumnText(12),
		TypeID:           stmt.ColumnText(13),
		TypeName:         stmt.ColumnText(14),
		Family:           stmt.ColumnText(15),
		Class:            stmt.ColumnText(16),
		Value:            stmt.ColumnText(17),
		IsPrimary:        stmt.ColumnInt(21) != 0,
		CreatedAt:        stmt.ColumnInt64(23),
	}

	// target_session_id (col 2)
	if s := stmt.ColumnText(2); s != "" {
		row.TargetSessionID = &s
	}
	// target_entry_session_id + target_entry_index (cols 3-5)
	if entrySID := stmt.ColumnText(3); entrySID != "" {
		idx := stmt.ColumnInt(4)
		row.TargetEntryIndex = &idx
		endIdx := stmt.ColumnInt(5)
		row.TargetEntryEndIndex = &endIdx
	}
	// target_annotation_id (col 6)
	if stmt.ColumnType(6) != sqlite.TypeNull {
		v := stmt.ColumnText(6)
		row.TargetAnnotID = &v
	}
	// target_project_hash (col 7)
	if stmt.ColumnType(7) != sqlite.TypeNull {
		v, err := schema.NewProjectHash(stmt.ColumnText(7))
		if err != nil {
			return AnnotationRow{}, fmt.Errorf("store: scan annotation %q project target from annotations_with_target: %w; the annotation cannot be served or published until its stored project identity is repaired", row.ID, err)
		}
		row.TargetProjectHash = &v
	}
	// target_association_id (col 8)
	if stmt.ColumnType(8) != sqlite.TypeNull {
		associationID, err := schema.NewAssociationID(stmt.ColumnText(8))
		if err != nil {
			return AnnotationRow{}, fmt.Errorf("store: scan annotation %q association target from annotations_with_target: %w; the annotation cannot be served or published until its durable association is repaired", row.ID, err)
		}
		row.TargetAssociationID = &associationID
	}

	// confidence (col 18)
	if stmt.ColumnType(18) != sqlite.TypeNull {
		v := stmt.ColumnFloat(18)
		row.Confidence = &v
	}
	// reason (col 19)
	if stmt.ColumnType(19) != sqlite.TypeNull {
		v := stmt.ColumnText(19)
		row.Reason = &v
	}
	// provenance (col 20)
	if stmt.ColumnType(20) != sqlite.TypeNull {
		raw := stmt.ColumnText(20)
		var prov schema.Provenance
		if err := json.Unmarshal([]byte(raw), &prov); err != nil {
			return AnnotationRow{}, fmt.Errorf("store: scan annotation provenance: %w", err)
		}
		row.Provenance = &prov
	}
	// content_hash (col 22)
	if stmt.ColumnType(22) != sqlite.TypeNull {
		v := stmt.ColumnText(22)
		row.ContentHash = &v
	}
	// updated_at (col 24)
	if stmt.ColumnType(24) != sqlite.TypeNull {
		v := stmt.ColumnInt64(24)
		row.UpdatedAt = &v
	}
	// superseded_by (col 25)
	if stmt.ColumnType(25) != sqlite.TypeNull {
		v := stmt.ColumnText(25)
		row.SupersededBy = &v
	}

	return row, nil
}

// scanAnnotatorRow scans an annotators result row.
// Column order must match sqlGetAnnotatorByName / sqlListAnnotators.
// V16: col 0 is TEXT UUID, col 1 is kind TEXT from JOIN.
func scanAnnotatorRow(stmt *sqlite.Stmt) AnnotatorRow {
	row := AnnotatorRow{
		ID:          stmt.ColumnText(0),
		Kind:        schema.AnnotatorKind(stmt.ColumnText(1)),
		Name:        stmt.ColumnText(2),
		DisplayName: stmt.ColumnText(3),
		Description: stmt.ColumnText(4),
		Status:      stmt.ColumnText(7),
		CreatedAt:   stmt.ColumnInt64(8),
	}
	if stmt.ColumnType(5) != sqlite.TypeNull {
		v := stmt.ColumnText(5)
		row.ModelID = &v
	}
	if stmt.ColumnType(6) != sqlite.TypeNull {
		v := stmt.ColumnText(6)
		row.ProviderKey = &v
	}
	return row
}

// scanAnnotationTypeRow scans an annotation_types result row.
// Column order must match sqlAnnotationTypeBase (cols 0-15).
// V16: col 0 and 5 are TEXT UUIDs; cols 6,10,11 are TEXT from lookup JOINs.
// V17: col 15 is COALESCE(sk.name,”) from scale_kinds LEFT JOIN.
func scanAnnotationTypeRow(stmt *sqlite.Stmt) (AnnotationTypeRow, error) {
	row := AnnotationTypeRow{
		ID:              stmt.ColumnText(0),
		TypeID:          stmt.ColumnText(1),
		Version:         stmt.ColumnInt(2),
		DisplayName:     stmt.ColumnText(3),
		Description:     stmt.ColumnText(4),
		FamilyID:        stmt.ColumnText(5),
		ValueDomainKind: schema.ValueDomainKind(stmt.ColumnText(6)),
		Datatype:        schema.AnnotationDatatype(stmt.ColumnText(7)),
		ValueConstraint: stmt.ColumnText(8),
		Status:          schema.AnnotationStatus(stmt.ColumnText(10)),
		Origin:          schema.TypeOrigin(stmt.ColumnText(11)),
		CreatedAt:       stmt.ColumnInt64(13),
		// col 15: COALESCE(sk.name, '') — scale_kind name from scale_kinds JOIN (V17)
		ScaleKind: schema.ScaleKind(stmt.ColumnText(15)),
	}

	// lower_is_better (col 9): nullable INTEGER stored as 0/1
	if stmt.ColumnType(9) != sqlite.TypeNull {
		v := stmt.ColumnInt(9) != 0
		row.LowerIsBetter = &v
	}
	// superseded_by (col 12): nullable TEXT UUID
	if stmt.ColumnType(12) != sqlite.TypeNull {
		v := stmt.ColumnText(12)
		row.SupersededBy = &v
	}
	// priority_override (col 14): nullable INTEGER added in V14
	if stmt.ColumnType(14) != sqlite.TypeNull {
		v := stmt.ColumnInt64(14)
		row.PriorityOverride = &v
	}

	return row, nil
}

// parseAllowedTargetKinds parses the group_concat(target_kinds.name) column
// (col 18 of sqlListAnnotationTypesWithFamily) into a typed slice. An empty
// string yields a nil slice (no junction rows → registry default of all kinds).
func parseAllowedTargetKinds(raw string) []schema.TargetKind {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	kinds := make([]schema.TargetKind, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		kinds = append(kinds, schema.TargetKind(p))
	}
	return kinds
}

// ---------------------------------------------------------------------------
// ingest.AnnotationStore adapter methods
// ---------------------------------------------------------------------------

// Compile-time guard: *Store must satisfy ingest.AnnotationStore.
var _ ingest.AnnotationStore = (*Store)(nil)

// CreateSessionAnnotation persists a session-level annotation via the ingest.AnnotationStore interface.
// Delegates to CreateAnnotation with SessionID arm set and IsPrimary=false.
func (s *Store) CreateSessionAnnotation(ctx context.Context, p ingest.SessionAnnotationParams) (string, error) {
	sid := p.SessionID
	return s.CreateAnnotation(ctx, CreateAnnotationParams{
		SessionID:        &sid,
		AnnotatorID:      p.AnnotatorID,
		AnnotationTypeID: p.AnnotationTypeID,
		Value:            p.Value,
		Confidence:       p.Confidence,
		Reason:           p.Reason,
		Provenance:       p.Provenance,
		IsPrimary:        false,
	})
}

// CreateEntryAnnotation persists an entry-level annotation via the ingest.AnnotationStore interface.
// Delegates to CreateAnnotation with EntryTarget arm set and IsPrimary=false.
// EndIndex=0 defaults to EntryIndex+1 (single-entry span), handled by CreateAnnotation.
func (s *Store) CreateEntryAnnotation(ctx context.Context, p ingest.EntryAnnotationParams) (string, error) {
	return s.CreateAnnotation(ctx, CreateAnnotationParams{
		EntryTarget: &EntryTarget{
			SessionID:  p.SessionID,
			EntryIndex: p.EntryIndex,
			EndIndex:   p.EndIndex,
		},
		AnnotatorID:      p.AnnotatorID,
		AnnotationTypeID: p.AnnotationTypeID,
		Value:            p.Value,
		Confidence:       p.Confidence,
		Reason:           p.Reason,
		Provenance:       p.Provenance,
		IsPrimary:        false,
	})
}

// GetAnnotatorIDByName returns the UUID for the annotator with the given name.
// Returns "", nil if no annotator with that name exists.
func (s *Store) GetAnnotatorIDByName(ctx context.Context, name string) (string, error) {
	row, err := s.GetAnnotator(ctx, name)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", nil
	}
	return row.ID, nil
}

// GetAnnotationTypeID returns the UUID for the annotation type with the given type_id string.
// Returns "", nil if no annotation type with that type_id exists.
func (s *Store) GetAnnotationTypeID(ctx context.Context, typeID string) (string, error) {
	row, err := s.GetAnnotationTypeByTypeID(ctx, typeID)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", nil
	}
	return row.ID, nil
}

// BatchCreateAnnotations inserts all params in a single all-or-nothing SQLite transaction.
// All parent + TPT child rows are inserted on a single pooled connection.
// Returns the UUIDs in the same order as params, or an error if any insert fails.
// On error, no rows are committed (savepoint rollback).
func (s *Store) BatchCreateAnnotations(ctx context.Context, params []CreateAnnotationParams) ([]string, error) {
	if len(params) == 0 {
		return nil, fmt.Errorf("store: BatchCreateAnnotations: params slice is empty")
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: BatchCreateAnnotations: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	ids := make([]string, len(params))
	for i, p := range params {
		if validationErr := validateCreateAnnotationTarget(p); validationErr != nil {
			err = fmt.Errorf("store: BatchCreateAnnotations[%d]: %w", i, validationErr)
			return nil, err
		}
		newID := uuid.New().String()
		ids[i] = newID

		var (
			targetKindName string
			childSQL       string
			childArgs      []any
			provenanceJSON any
		)

		switch {
		case p.EntryTarget != nil:
			targetKindName = "entry"
			endIdx := p.EntryTarget.EndIndex
			if endIdx == 0 {
				endIdx = p.EntryTarget.EntryIndex + 1
			}
			childSQL = sqlInsertTargetEntry
			childArgs = []any{newID, p.EntryTarget.SessionID, p.EntryTarget.EntryIndex, endIdx}
		case p.AnnotationID != nil:
			targetKindName = "annotation"
			childSQL = sqlInsertTargetAnnotation
			childArgs = []any{newID, *p.AnnotationID}
		case p.ProjectHash != nil:
			targetKindName = "project"
			childSQL = sqlInsertTargetProject
			childArgs = []any{newID, *p.ProjectHash}
		case p.AssociationID != nil:
			targetKindName = schema.TargetAssociation.String()
			childSQL = sqlInsertTargetAssociation
			childArgs = []any{newID, p.AssociationID.String()}
		case p.SessionID != nil:
			targetKindName = "session"
			childSQL = sqlInsertTargetSession
			childArgs = []any{newID, *p.SessionID}
		default:
			err = fmt.Errorf("store: BatchCreateAnnotations[%d]: exactly one target must be set", i)
			return nil, err
		}

		if p.Provenance != nil {
			b, merr := json.Marshal(p.Provenance)
			if merr != nil {
				err = fmt.Errorf("store: BatchCreateAnnotations[%d]: marshal provenance: %w", i, merr)
				return nil, err
			}
			provenanceJSON = string(b)
		}

		var isPrimaryInt int
		if p.IsPrimary {
			isPrimaryInt = 1
		}
		nowMs := time.Now().UnixMilli()

		if err = sqlitex.ExecuteTransient(conn, sqlInsertAnnotation, &sqlitex.ExecOptions{
			Args: []any{
				newID, targetKindName,
				p.AnnotationTypeID, p.AnnotatorID, p.Value,
				ptrFloat64ToAny(p.Confidence), ptrStringToAny(p.Reason), provenanceJSON,
				isPrimaryInt, nowMs,
			},
		}); err != nil {
			err = fmt.Errorf("store: BatchCreateAnnotations[%d]: insert annotation: %w", i, err)
			return nil, err
		}

		if err = sqlitex.ExecuteTransient(conn, childSQL, &sqlitex.ExecOptions{
			Args: childArgs,
		}); err != nil {
			err = fmt.Errorf("store: BatchCreateAnnotations[%d]: insert target: %w", i, err)
			return nil, err
		}
	}

	return ids, nil
}
