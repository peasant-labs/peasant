package annotations

import (
	"fmt"
	"time"

	"github.com/peasant-labs/schema"
)

// AnnotationType is the runtime representation of a registered annotation type.
// Fields are unexported; use accessor methods to read values (V21: no standalone trait interfaces).
// The struct is constructed by sqliteRegistry from DB rows.
type AnnotationType struct {
	dbID             string // V16: UUID PK
	id               string // type_id (e.g., "quality.session_approval")
	version          int
	displayName      string
	description      string
	familyID         string // V16: UUID FK to annotation_families.id; class derived via store join
	valueDomain      schema.ValueDomain
	scaleKind        schema.ScaleKind // V17: Stevens measurement level; empty when unclassified
	lowerIsBetter    *bool
	status           schema.AnnotationStatus
	origin           schema.TypeOrigin
	priorityOverride *int
	supersededBy     *string // V16: UUID FK
	createdAt        time.Time
}

// TypeID returns the string type identifier (e.g., "quality.session_approval").
func (t *AnnotationType) TypeID() string { return t.id }

// DBID returns the UUID primary key in annotation_types.
func (t *AnnotationType) DBID() string { return t.dbID }

// Version returns the version number of this annotation type.
func (t *AnnotationType) Version() int { return t.version }

// DisplayName returns the human-readable display name.
func (t *AnnotationType) DisplayName() string { return t.displayName }

// Description returns the optional description.
func (t *AnnotationType) Description() string { return t.description }

// FamilyID returns the UUID FK to annotation_families.id.
func (t *AnnotationType) FamilyID() string { return t.familyID }

// ValueDomain returns the permissible value domain definition.
func (t *AnnotationType) ValueDomain() schema.ValueDomain { return t.valueDomain }

// ScaleKind returns the Stevens measurement level (V17). Empty string when unclassified.
func (t *AnnotationType) ScaleKind() schema.ScaleKind { return t.scaleKind }

// LowerIsBetter returns nil (non-ordinal), false (higher is better), or true (lower is better).
func (t *AnnotationType) LowerIsBetter() *bool { return t.lowerIsBetter }

// Status returns the ISO 11179 lifecycle status.
func (t *AnnotationType) Status() schema.AnnotationStatus { return t.status }

// Origin returns who created this annotation type.
func (t *AnnotationType) Origin() schema.TypeOrigin { return t.origin }

// PriorityOverride returns the explicit priority override, or nil to use the
// default kind-based priority (human=3, agent=2, rule=1).
func (t *AnnotationType) PriorityOverride() *int { return t.priorityOverride }

// IsCurrent returns true when this type has not been superseded.
func (t *AnnotationType) IsCurrent() bool { return t.supersededBy == nil }

// SupersededBy returns the UUID of the superseding type, or nil if current.
func (t *AnnotationType) SupersededBy() *string { return t.supersededBy }

// CreatedAt returns the creation time.
func (t *AnnotationType) CreatedAt() time.Time { return t.createdAt }

// ValidateValue checks whether value is permissible for this annotation type.
// Delegates to schema.ValidateAnnotationValue.
// Returns nil if valid, a descriptive error otherwise.
func (t *AnnotationType) ValidateValue(value string) error {
	if err := schema.ValidateAnnotationValue(t.valueDomain, value); err != nil {
		return fmt.Errorf("annotation type %q: %w", t.id, err)
	}
	return nil
}

// ToSummary converts the AnnotationType to its wire format.
// family and class must be provided by the caller (derived via store join).
func (t *AnnotationType) ToSummary(family, class string) schema.AnnotationTypeSummary {
	return schema.AnnotationTypeSummary{
		ID:               t.dbID,
		TypeID:           t.id,
		Version:          t.version,
		DisplayName:      t.displayName,
		Description:      t.description,
		Family:           family,
		Class:            class,
		ValueDomain:      t.valueDomain,
		ScaleKind:        t.scaleKind,
		LowerIsBetter:    t.lowerIsBetter,
		Status:           t.status,
		Origin:           t.origin,
		PriorityOverride: t.priorityOverride,
	}
}
