package store

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// PeasantAnnotationNamespace is the UUIDv5 namespace for deterministic annotation entity UUIDs.
// Used in both SQLite V16 migration and PG 009 seed data for cross-database consistency.
var PeasantAnnotationNamespace = uuid.MustParse("a8e3f1b2-c4d5-4e6f-87a9-0b1c2d3e4f56")

// GenerateEntityUUID returns a deterministic UUID for an entity row.
// Same (tableName, oldID) always produces the same UUID.
func GenerateEntityUUID(tableName string, oldID int64) string {
	return uuid.NewSHA1(PeasantAnnotationNamespace, fmt.Appendf(nil, "%s:%d", tableName, oldID)).String()
}

// Pre-computed UUIDs for all seed data rows.
// These are deterministic: GenerateEntityUUID("table_name", old_integer_id).
var (
	// annotation_classes seed UUIDs (old IDs: 1, 2, 3)
	uuidClassQuality  = GenerateEntityUUID("annotation_classes", 1)
	uuidClassMetadata = GenerateEntityUUID("annotation_classes", 2)
	uuidClassBehavior = GenerateEntityUUID("annotation_classes", 3)

	// annotation_families seed UUIDs (old IDs: 1-5)
	uuidFamSessionQuality  = GenerateEntityUUID("annotation_families", 1)
	uuidFamTurnQuality     = GenerateEntityUUID("annotation_families", 2)
	uuidFamSessionMetadata = GenerateEntityUUID("annotation_families", 3)
	uuidFamTurnMetadata    = GenerateEntityUUID("annotation_families", 4)
	uuidFamSessionBehavior = GenerateEntityUUID("annotation_families", 5)

	// annotation_types seed UUIDs (old IDs: 1-4)
	uuidTypeSessionApproval = GenerateEntityUUID("annotation_types", 1)
	uuidTypeSessionOutcome  = GenerateEntityUUID("annotation_types", 2)
	uuidTypeUserFrustration = GenerateEntityUUID("annotation_types", 3)
	uuidTypeSessionScope    = GenerateEntityUUID("annotation_types", 4)

	// annotators seed UUIDs (old IDs: 1-4)
	uuidAnnotatorOutcome     = GenerateEntityUUID("annotators", 1)
	uuidAnnotatorFrustration = GenerateEntityUUID("annotators", 2)
	uuidAnnotatorScope       = GenerateEntityUUID("annotators", 3)
	uuidAnnotatorHumanWeb    = GenerateEntityUUID("annotators", 4)

	// V18: Entry-level annotation types (old IDs: 5-6)
	uuidTypeFrustrationSignal  = GenerateEntityUUID("annotation_types", 5)
	uuidTypeResolutionEvidence = GenerateEntityUUID("annotation_types", 6)

	// V18: Entry-level classifier annotators (old IDs: 5-6)
	uuidAnnotatorFrustrationSignal  = GenerateEntityUUID("annotators", 5)
	uuidAnnotatorResolutionEvidence = GenerateEntityUUID("annotators", 6)

	// V20: annotation_approval meta-annotation type (old ID: 7)
	uuidTypeAnnotationApproval = GenerateEntityUUID("annotation_types", 7)
)

// buildMigrationV16 constructs the V16 SQL migration string with embedded deterministic UUIDs.
//
// V16: Annotation schema refactor — INT lookup tables, UUID PKs, TPT targets.
//
// Steps:
//  1. Create 5 INT lookup tables + seed
//  2. Rebuild annotation_classes (UUID PK)
//  3. Rebuild annotation_families (UUID PK)
//  4. Rebuild annotation_types (UUID PK, INT FK enums)
//  5. Rebuild annotation_type_deps (UUID FKs)
//  6. Rebuild annotators (UUID PK, INT FK enum)
//  7. Rebuild annotations (UUID PK, TPT parent + INT FK enums)
//  8. Create 4 TPT child tables + migrate target data
//  9. Create AllowedTargetKinds junction table + seed
//
// 10. Recreate annotations_with_target VIEW
// 11. Drop old tables
//
// Foreign keys disabled via MigrationOptions.DisableForeignKeys (not inline PRAGMA).
// PRAGMA legacy_alter_table = ON prevents FK auto-rewrite during ALTER TABLE RENAME.
func buildMigrationV16() string {
	var b strings.Builder

	// --- PRAGMA setup ---
	// Foreign keys are disabled via MigrationOptions.DisableForeignKeys in migrations.go
	// (PRAGMA foreign_keys is a no-op inside transactions per SQLite docs).
	// legacy_alter_table = ON disables SQLite 3.26+ auto-rewrite of FK references
	// during ALTER TABLE RENAME — without this, renaming a parent table auto-updates
	// FK definitions in child tables, preventing DROP of the renamed table.
	// legacy_alter_table CAN be changed inside a transaction, unlike foreign_keys.
	b.WriteString("PRAGMA legacy_alter_table = ON;\n\n")

	// --- Step 1: INT lookup tables ---
	b.WriteString(v16CreateLookupTables)
	b.WriteString(";\n\n")
	b.WriteString(v16SeedLookupTables)
	b.WriteString(";\n\n")

	// --- Step 2: Rebuild annotation_classes (UUID PK) ---
	b.WriteString("DROP VIEW IF EXISTS annotations_with_target;\n\n")
	b.WriteString("ALTER TABLE annotation_classes RENAME TO _old_annotation_classes;\n\n")
	b.WriteString(v16CreateAnnotationClasses)
	b.WriteString(";\n\n")
	fmt.Fprintf(&b, v16InsertAnnotationClasses,
		uuidClassQuality, uuidClassMetadata, uuidClassBehavior)
	b.WriteString(";\n\n")
	b.WriteString("DROP TABLE _old_annotation_classes;\n\n")

	// --- Step 3: Rebuild annotation_families (UUID PK, UUID FK to classes) ---
	b.WriteString("ALTER TABLE annotation_families RENAME TO _old_annotation_families;\n\n")
	b.WriteString("DROP INDEX IF EXISTS idx_annfam_class_id;\n\n")
	b.WriteString(v16CreateAnnotationFamilies)
	b.WriteString(";\n\n")
	fmt.Fprintf(&b, v16InsertAnnotationFamilies,
		uuidFamSessionQuality, uuidClassQuality,
		uuidFamTurnQuality, uuidClassQuality,
		uuidFamSessionMetadata, uuidClassMetadata,
		uuidFamTurnMetadata, uuidClassMetadata,
		uuidFamSessionBehavior, uuidClassBehavior)
	b.WriteString(";\n\n")
	b.WriteString("DROP TABLE _old_annotation_families;\n\n")
	b.WriteString("CREATE INDEX idx_annfam_class_id ON annotation_families(class_id);\n\n")

	// --- Step 4: Rebuild annotation_types (UUID PK, UUID FK to families, INT enums) ---
	b.WriteString("ALTER TABLE annotation_types RENAME TO _old_annotation_types;\n\n")
	b.WriteString("DROP INDEX IF EXISTS idx_anntype_status;\n")
	b.WriteString("DROP INDEX IF EXISTS idx_anntype_family_id;\n\n")
	b.WriteString(v16CreateAnnotationTypes)
	b.WriteString(";\n\n")
	fmt.Fprintf(&b, v16InsertAnnotationTypes,
		uuidTypeSessionApproval, uuidFamSessionQuality,
		uuidTypeSessionOutcome, uuidFamSessionQuality,
		uuidTypeUserFrustration, uuidFamSessionQuality,
		uuidTypeSessionScope, uuidFamSessionMetadata)
	b.WriteString(";\n\n")
	// NOTE: _old_annotation_types is NOT dropped here — it is still needed by
	// Step 5 (v16InsertAnnotationTypeDeps references it to match old integer FKs).
	b.WriteString(v16CreateAnnotationTypesIndexes)
	b.WriteString(";\n\n")

	// --- Step 5: Rebuild annotation_type_deps (UUID FKs) ---
	b.WriteString("ALTER TABLE annotation_type_deps RENAME TO _old_annotation_type_deps;\n\n")
	b.WriteString("DROP INDEX IF EXISTS idx_anntype_deps_reverse;\n\n")
	b.WriteString(v16CreateAnnotationTypeDeps)
	b.WriteString(";\n\n")
	fmt.Fprintf(&b, v16InsertAnnotationTypeDeps,
		uuidTypeSessionOutcome, uuidTypeSessionScope)
	b.WriteString(";\n\n")
	// NOTE: _old_annotation_types is NOT dropped here — v16MigrateAnnotations (Step 7)
	// still needs it to map old integer annotation_type_id → new UUID.
	b.WriteString("DROP TABLE _old_annotation_type_deps;\n\n")
	b.WriteString("CREATE INDEX idx_anntype_deps_reverse ON annotation_type_deps(depends_on_id);\n\n")

	// --- Step 6: Rebuild annotators (UUID PK, INT kind FK) ---
	b.WriteString("ALTER TABLE annotators RENAME TO _old_annotators;\n\n")
	b.WriteString("DROP INDEX IF EXISTS idx_annotator_kind;\n")
	b.WriteString("DROP INDEX IF EXISTS idx_annotator_model;\n\n")
	b.WriteString(v16CreateAnnotators)
	b.WriteString(";\n\n")
	fmt.Fprintf(&b, v16InsertAnnotators,
		uuidAnnotatorOutcome, uuidAnnotatorFrustration, uuidAnnotatorScope, uuidAnnotatorHumanWeb)
	b.WriteString(";\n\n")
	// NOTE: _old_annotators is NOT dropped here — v16MigrateAnnotations (Step 7)
	// still needs it to map old integer annotator_id → new UUID.
	b.WriteString(v16CreateAnnotatorsIndexes)
	b.WriteString(";\n\n")

	// --- Step 7: Rebuild annotations (UUID PK, TPT parent, INT enums) ---
	b.WriteString("ALTER TABLE annotations RENAME TO _old_annotations;\n\n")
	b.WriteString("DROP INDEX IF EXISTS idx_ann_session;\n")
	b.WriteString("DROP INDEX IF EXISTS idx_ann_entry;\n")
	b.WriteString("DROP INDEX IF EXISTS idx_ann_meta;\n")
	b.WriteString("DROP INDEX IF EXISTS idx_ann_project;\n")
	b.WriteString("DROP INDEX IF EXISTS idx_ann_type_id;\n")
	b.WriteString("DROP INDEX IF EXISTS idx_ann_effective;\n\n")
	b.WriteString(v16CreateAnnotations)
	b.WriteString(";\n\n")
	// Migrate existing annotation rows — generate UUIDs via lower(hex(randomblob(16)))
	// Map old INTEGER FKs to new UUID FKs using the known seed UUID mappings.
	// Map old TEXT enum columns to INT FK IDs via lookup table JOINs.
	b.WriteString(v16MigrateAnnotations)
	b.WriteString(";\n\n")

	// --- Step 8: Create 4 TPT child tables + migrate target data ---
	b.WriteString(v16CreateTPTChildTables)
	b.WriteString(";\n\n")
	b.WriteString(v16MigrateTargetData)
	b.WriteString(";\n\n")
	// Drop old lookup tables now that annotation data migration is complete.
	b.WriteString("DROP TABLE _old_annotation_types;\n\n")
	b.WriteString("DROP TABLE _old_annotators;\n\n")
	b.WriteString("DROP TABLE _old_annotations;\n\n")
	b.WriteString(v16CreateAnnotationsIndexes)
	b.WriteString(";\n\n")

	// --- Step 9: AllowedTargetKinds junction table ---
	b.WriteString(v16CreateAllowedTargetKinds)
	b.WriteString(";\n\n")
	fmt.Fprintf(&b, v16SeedAllowedTargetKinds,
		uuidTypeSessionApproval,
		uuidTypeSessionOutcome,
		uuidTypeUserFrustration,
		uuidTypeSessionScope,
		uuidTypeSessionOutcome,
		uuidTypeSessionScope)
	b.WriteString(";\n\n")

	// --- Step 10: Recreate VIEW ---
	b.WriteString(v16CreateAnnotationsView)
	b.WriteString(";\n\n")

	// --- Step 11: Restore PRAGMAs ---
	// FK validation is handled by the migration library after the transaction commits.
	b.WriteString("PRAGMA legacy_alter_table = OFF")

	return b.String()
}

// --------------------------------------------------------------------------
// V16 DDL constants
// --------------------------------------------------------------------------

const v16CreateLookupTables = `CREATE TABLE target_kinds (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE annotator_kinds (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE annotation_statuses (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE value_domain_kinds (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE type_origins (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
) STRICT`

const v16SeedLookupTables = `INSERT INTO target_kinds (id, name) VALUES
    (1, 'session'), (2, 'entry'), (3, 'annotation'), (4, 'project');

INSERT INTO annotator_kinds (id, name) VALUES
    (1, 'human'), (2, 'agent'), (3, 'rule');

INSERT INTO annotation_statuses (id, name) VALUES
    (1, 'proposed'), (2, 'active'), (3, 'deprecated'), (4, 'retired');

INSERT INTO value_domain_kinds (id, name) VALUES
    (1, 'enumerated'), (2, 'described');

INSERT INTO type_origins (id, name) VALUES
    (1, 'system'), (2, 'user'), (3, 'group')`

const v16CreateAnnotationClasses = `CREATE TABLE annotation_classes (
    id           TEXT PRIMARY KEY,
    class        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    description  TEXT
) STRICT`

// v16InsertAnnotationClasses: 3 format args (quality UUID, metadata UUID, behavior UUID)
const v16InsertAnnotationClasses = `INSERT INTO annotation_classes (id, class, display_name, description)
SELECT '%s', o.class, o.display_name, o.description FROM _old_annotation_classes o WHERE o.id = 1
UNION ALL
SELECT '%s', o.class, o.display_name, o.description FROM _old_annotation_classes o WHERE o.id = 2
UNION ALL
SELECT '%s', o.class, o.display_name, o.description FROM _old_annotation_classes o WHERE o.id = 3`

const v16CreateAnnotationFamilies = `CREATE TABLE annotation_families (
    id           TEXT PRIMARY KEY,
    family       TEXT NOT NULL UNIQUE,
    class_id     TEXT NOT NULL REFERENCES annotation_classes(id),
    display_name TEXT NOT NULL,
    description  TEXT
) STRICT`

// v16InsertAnnotationFamilies: 10 format args (5 family UUIDs + 5 class UUIDs)
const v16InsertAnnotationFamilies = `INSERT INTO annotation_families (id, family, class_id, display_name, description)
SELECT '%s', o.family, '%s', o.display_name, o.description FROM _old_annotation_families o WHERE o.id = 1
UNION ALL
SELECT '%s', o.family, '%s', o.display_name, o.description FROM _old_annotation_families o WHERE o.id = 2
UNION ALL
SELECT '%s', o.family, '%s', o.display_name, o.description FROM _old_annotation_families o WHERE o.id = 3
UNION ALL
SELECT '%s', o.family, '%s', o.display_name, o.description FROM _old_annotation_families o WHERE o.id = 4
UNION ALL
SELECT '%s', o.family, '%s', o.display_name, o.description FROM _old_annotation_families o WHERE o.id = 5`

const v16CreateAnnotationTypes = `CREATE TABLE annotation_types (
    id                 TEXT PRIMARY KEY,
    type_id            TEXT NOT NULL UNIQUE,
    version            INTEGER NOT NULL DEFAULT 1,
    display_name       TEXT NOT NULL,
    description        TEXT,
    family_id          TEXT NOT NULL REFERENCES annotation_families(id),
    value_domain_kind_id INTEGER NOT NULL REFERENCES value_domain_kinds(id),
    datatype           TEXT NOT NULL
                       CHECK (datatype IN ('text', 'integer', 'real', 'boolean')),
    value_constraint   TEXT NOT NULL,
    lower_is_better    INTEGER CHECK (lower_is_better IN (0, 1)),
    status_id          INTEGER NOT NULL DEFAULT 1 REFERENCES annotation_statuses(id),
    origin_id          INTEGER NOT NULL REFERENCES type_origins(id),
    priority_override  INTEGER,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER,
    deprecated_at      INTEGER,
    superseded_by      TEXT REFERENCES annotation_types(id),
    CHECK (type_id LIKE '%.%')
) STRICT`

// v16InsertAnnotationTypes: 8 format args (4 type UUIDs + 4 family UUIDs)
const v16InsertAnnotationTypes = `INSERT INTO annotation_types
    (id, type_id, version, display_name, description, family_id,
     value_domain_kind_id, datatype, value_constraint, lower_is_better,
     status_id, origin_id, priority_override, created_at, updated_at, deprecated_at, superseded_by)
SELECT '%s', o.type_id, o.version, o.display_name, o.description, '%s',
    (SELECT vdk.id FROM value_domain_kinds vdk WHERE vdk.name = o.value_domain_type),
    o.datatype, o.value_constraint, o.lower_is_better,
    (SELECT s.id FROM annotation_statuses s WHERE s.name = o.status),
    (SELECT orig.id FROM type_origins orig WHERE orig.name = o.origin),
    o.priority_override, o.created_at, o.updated_at, o.deprecated_at, o.superseded_by
FROM _old_annotation_types o WHERE o.id = 1
UNION ALL
SELECT '%s', o.type_id, o.version, o.display_name, o.description, '%s',
    (SELECT vdk.id FROM value_domain_kinds vdk WHERE vdk.name = o.value_domain_type),
    o.datatype, o.value_constraint, o.lower_is_better,
    (SELECT s.id FROM annotation_statuses s WHERE s.name = o.status),
    (SELECT orig.id FROM type_origins orig WHERE orig.name = o.origin),
    o.priority_override, o.created_at, o.updated_at, o.deprecated_at, o.superseded_by
FROM _old_annotation_types o WHERE o.id = 2
UNION ALL
SELECT '%s', o.type_id, o.version, o.display_name, o.description, '%s',
    (SELECT vdk.id FROM value_domain_kinds vdk WHERE vdk.name = o.value_domain_type),
    o.datatype, o.value_constraint, o.lower_is_better,
    (SELECT s.id FROM annotation_statuses s WHERE s.name = o.status),
    (SELECT orig.id FROM type_origins orig WHERE orig.name = o.origin),
    o.priority_override, o.created_at, o.updated_at, o.deprecated_at, o.superseded_by
FROM _old_annotation_types o WHERE o.id = 3
UNION ALL
SELECT '%s', o.type_id, o.version, o.display_name, o.description, '%s',
    (SELECT vdk.id FROM value_domain_kinds vdk WHERE vdk.name = o.value_domain_type),
    o.datatype, o.value_constraint, o.lower_is_better,
    (SELECT s.id FROM annotation_statuses s WHERE s.name = o.status),
    (SELECT orig.id FROM type_origins orig WHERE orig.name = o.origin),
    o.priority_override, o.created_at, o.updated_at, o.deprecated_at, o.superseded_by
FROM _old_annotation_types o WHERE o.id = 4`

const v16CreateAnnotationTypesIndexes = `CREATE INDEX idx_anntype_status ON annotation_types(status_id);
CREATE INDEX idx_anntype_family_id ON annotation_types(family_id)`

const v16CreateAnnotationTypeDeps = `CREATE TABLE annotation_type_deps (
    annotation_type_id TEXT NOT NULL REFERENCES annotation_types(id),
    depends_on_id      TEXT NOT NULL REFERENCES annotation_types(id),
    required           INTEGER NOT NULL DEFAULT 1 CHECK (required IN (0, 1)),
    rationale          TEXT,
    PRIMARY KEY (annotation_type_id, depends_on_id),
    CHECK (annotation_type_id != depends_on_id)
) STRICT`

// v16InsertAnnotationTypeDeps: 2 format args (type UUID, depends_on UUID)
const v16InsertAnnotationTypeDeps = `INSERT INTO annotation_type_deps (annotation_type_id, depends_on_id, required, rationale)
SELECT '%s', '%s', o.required, o.rationale
FROM _old_annotation_type_deps o WHERE o.annotation_type_id = (
    SELECT id FROM _old_annotation_types WHERE type_id = 'quality.session_outcome'
) AND o.depends_on_id = (
    SELECT id FROM _old_annotation_types WHERE type_id = 'metadata.session_scope'
)`

const v16CreateAnnotators = `CREATE TABLE annotators (
    id            TEXT PRIMARY KEY,
    kind_id       INTEGER NOT NULL REFERENCES annotator_kinds(id),
    name          TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL,
    description   TEXT,
    model_id      TEXT,
    provider_key  TEXT,
    status        TEXT NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'inactive')),
    created_at    INTEGER NOT NULL,
    FOREIGN KEY (model_id, provider_key) REFERENCES models(model_id, provider_key),
    CHECK (kind_id != 2 OR (model_id IS NOT NULL AND provider_key IS NOT NULL)),
    CHECK (kind_id = 2 OR (model_id IS NULL AND provider_key IS NULL))
) STRICT`

// v16InsertAnnotators: 4 format args (4 annotator UUIDs)
const v16InsertAnnotators = `INSERT INTO annotators (id, kind_id, name, display_name, description, model_id, provider_key, status, created_at)
SELECT '%s',
    (SELECT ak.id FROM annotator_kinds ak WHERE ak.name = o.kind),
    o.name, o.display_name, o.description, o.model_id, o.provider_key, o.status, o.created_at
FROM _old_annotators o WHERE o.id = 1
UNION ALL
SELECT '%s',
    (SELECT ak.id FROM annotator_kinds ak WHERE ak.name = o.kind),
    o.name, o.display_name, o.description, o.model_id, o.provider_key, o.status, o.created_at
FROM _old_annotators o WHERE o.id = 2
UNION ALL
SELECT '%s',
    (SELECT ak.id FROM annotator_kinds ak WHERE ak.name = o.kind),
    o.name, o.display_name, o.description, o.model_id, o.provider_key, o.status, o.created_at
FROM _old_annotators o WHERE o.id = 3
UNION ALL
SELECT '%s',
    (SELECT ak.id FROM annotator_kinds ak WHERE ak.name = o.kind),
    o.name, o.display_name, o.description, o.model_id, o.provider_key, o.status, o.created_at
FROM _old_annotators o WHERE o.id = 4`

const v16CreateAnnotatorsIndexes = `CREATE INDEX idx_annotator_kind ON annotators(kind_id);
CREATE INDEX idx_annotator_model ON annotators(model_id, provider_key) WHERE model_id IS NOT NULL`

const v16CreateAnnotations = `CREATE TABLE annotations (
    id                 TEXT PRIMARY KEY,
    target_kind_id     INTEGER NOT NULL REFERENCES target_kinds(id),
    annotation_type_id TEXT NOT NULL REFERENCES annotation_types(id),
    annotator_id       TEXT NOT NULL REFERENCES annotators(id),
    value              TEXT NOT NULL,
    confidence         REAL,
    reason             TEXT,
    provenance         TEXT,
    is_primary         INTEGER NOT NULL DEFAULT 0 CHECK (is_primary IN (0, 1)),
    content_hash       TEXT,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER,
    superseded_by      TEXT REFERENCES annotations(id)
) STRICT`

// v16MigrateAnnotations uses a temp mapping table for robust old_id -> new_uuid correlation.
// All annotations are migrated (including superseded). Superseded_by is resolved via the
// temp mapping table after initial insert.
const v16MigrateAnnotations = `CREATE TEMP TABLE _ann_id_map (old_id INTEGER PRIMARY KEY, new_id TEXT NOT NULL);

INSERT INTO _ann_id_map (old_id, new_id)
SELECT id,
    lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' ||
          substr(hex(randomblob(2)),2) || '-' ||
          substr('89ab', abs(random()) % 4 + 1, 1) ||
          substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6)))
FROM _old_annotations;

INSERT INTO annotations
    (id, target_kind_id, annotation_type_id, annotator_id,
     value, confidence, reason, provenance, is_primary,
     created_at, updated_at, superseded_by)
SELECT
    m.new_id,
    CASE
        WHEN o.target_session_id IS NOT NULL THEN 1
        WHEN o.target_entry_session_id IS NOT NULL THEN 2
        WHEN o.target_annotation_id IS NOT NULL THEN 3
        WHEN o.target_project_hash IS NOT NULL THEN 4
    END,
    (SELECT t_new.id FROM annotation_types t_new
     JOIN _old_annotation_types t_old ON t_old.type_id = t_new.type_id
     WHERE t_old.id = o.annotation_type_id),
    (SELECT a_new.id FROM annotators a_new
     JOIN _old_annotators a_old ON a_old.name = a_new.name
     WHERE a_old.id = o.annotator_id),
    o.value, o.confidence, o.reason, o.provenance, o.is_primary,
    o.created_at, o.updated_at,
    (SELECT m2.new_id FROM _ann_id_map m2 WHERE m2.old_id = o.superseded_by)
FROM _old_annotations o
JOIN _ann_id_map m ON m.old_id = o.id`

const v16CreateTPTChildTables = `CREATE TABLE annotation_target_sessions (
    annotation_id TEXT PRIMARY KEY REFERENCES annotations(id) ON DELETE CASCADE,
    session_id    TEXT NOT NULL REFERENCES sessions(session_id)
) STRICT;

CREATE TABLE annotation_target_entries (
    annotation_id TEXT PRIMARY KEY REFERENCES annotations(id) ON DELETE CASCADE,
    session_id    TEXT NOT NULL,
    entry_index   INTEGER NOT NULL,
    end_index     INTEGER NOT NULL,
    FOREIGN KEY (session_id, entry_index) REFERENCES session_entries(session_id, entry_index),
    CHECK (end_index > entry_index)
) STRICT;

CREATE TABLE annotation_target_annotations (
    annotation_id        TEXT PRIMARY KEY REFERENCES annotations(id) ON DELETE CASCADE,
    target_annotation_id TEXT NOT NULL REFERENCES annotations(id)
) STRICT;

CREATE TABLE annotation_target_projects (
    annotation_id TEXT PRIMARY KEY REFERENCES annotations(id) ON DELETE CASCADE,
    project_hash  TEXT NOT NULL REFERENCES projects(project_hash)
) STRICT`

// v16MigrateTargetData populates TPT child tables from old annotations data.
// Uses _ann_id_map temp table for robust old_id -> new_uuid correlation.
const v16MigrateTargetData = `INSERT INTO annotation_target_sessions (annotation_id, session_id)
SELECT m.new_id, o.target_session_id
FROM _old_annotations o
JOIN _ann_id_map m ON m.old_id = o.id
WHERE o.target_session_id IS NOT NULL;

INSERT INTO annotation_target_entries (annotation_id, session_id, entry_index, end_index)
SELECT m.new_id, o.target_entry_session_id, o.target_entry_index, o.target_entry_index + 1
FROM _old_annotations o
JOIN _ann_id_map m ON m.old_id = o.id
WHERE o.target_entry_session_id IS NOT NULL;

INSERT INTO annotation_target_annotations (annotation_id, target_annotation_id)
SELECT m.new_id, m_target.new_id
FROM _old_annotations o
JOIN _ann_id_map m ON m.old_id = o.id
JOIN _ann_id_map m_target ON m_target.old_id = o.target_annotation_id
WHERE o.target_annotation_id IS NOT NULL;

INSERT INTO annotation_target_projects (annotation_id, project_hash)
SELECT m.new_id, o.target_project_hash
FROM _old_annotations o
JOIN _ann_id_map m ON m.old_id = o.id
WHERE o.target_project_hash IS NOT NULL;

DROP TABLE _ann_id_map`

const v16CreateAnnotationsIndexes = `CREATE INDEX idx_ann_type_id ON annotations(annotation_type_id);
CREATE INDEX idx_ann_annotator ON annotations(annotator_id);
CREATE INDEX idx_ann_target_kind ON annotations(target_kind_id);
CREATE INDEX idx_ann_content_hash ON annotations(content_hash) WHERE content_hash IS NOT NULL;
CREATE INDEX idx_ann_created_at ON annotations(created_at DESC);

CREATE INDEX idx_ann_target_session ON annotation_target_sessions(session_id);
CREATE INDEX idx_ann_target_entry ON annotation_target_entries(session_id, entry_index);
CREATE INDEX idx_ann_target_annot ON annotation_target_annotations(target_annotation_id);
CREATE INDEX idx_ann_target_project ON annotation_target_projects(project_hash)`

const v16CreateAllowedTargetKinds = `CREATE TABLE annotation_type_target_kinds (
    annotation_type_id TEXT NOT NULL REFERENCES annotation_types(id) ON DELETE CASCADE,
    target_kind_id     INTEGER NOT NULL REFERENCES target_kinds(id),
    PRIMARY KEY (annotation_type_id, target_kind_id)
) STRICT`

// v16SeedAllowedTargetKinds: 6 format args (4 session-level + 2 project-level UUIDs)
// All 4 system types allow session-level targeting (target_kind 1=session).
// session_outcome and session_scope also allow project-level (target_kind 4=project).
const v16SeedAllowedTargetKinds = `INSERT INTO annotation_type_target_kinds (annotation_type_id, target_kind_id) VALUES
    ('%s', 1),
    ('%s', 1),
    ('%s', 1),
    ('%s', 1),
    ('%s', 4),
    ('%s', 4)`

const v16CreateAnnotationsView = `CREATE VIEW annotations_with_target AS
SELECT
    a.id, a.target_kind_id, tk.name AS target_kind,
    ts.session_id AS target_session_id,
    te.session_id AS target_entry_session_id,
    te.entry_index AS target_entry_index,
    te.end_index AS target_entry_end_index,
    ta.target_annotation_id,
    tp.project_hash AS target_project_hash,
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
    LEFT JOIN annotation_target_projects tp ON a.id = tp.annotation_id`
