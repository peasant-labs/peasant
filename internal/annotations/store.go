package annotations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// sqliteRegistry implements schema.AnnotationTypeReader and schema.AnnotationRegistry
// by delegating to *store.Store. It is the production registry implementation.
type sqliteRegistry struct {
	s *store.Store
}

// ---------------------------------------------------------------------------
// schema.AnnotationTypeReader implementation
// ---------------------------------------------------------------------------

// GetType returns the AnnotationTypeSummary for the given type_id string.
// Returns an error wrapping schema.ErrTypeNotFound if no match exists.
func (r *sqliteRegistry) GetType(ctx context.Context, typeID string) (*schema.AnnotationTypeSummary, error) {
	row, err := r.s.GetAnnotationTypeWithFamilyByTypeID(ctx, typeID)
	if err != nil {
		return nil, fmt.Errorf("annotations: GetType %q: %w", typeID, err)
	}
	if row == nil {
		return nil, fmt.Errorf("annotations: GetType %q: %w", typeID, schema.ErrTypeNotFound)
	}
	return rowWithFamilyToSummary(row)
}

// GetTypeByDBID returns the AnnotationType for the given UUID PK.
// This method is available on the concrete *sqliteRegistry for internal use.
// It is NOT part of the schema.AnnotationTypeReader interface (UUID PKs are an internal detail).
// Returns an error wrapping schema.ErrTypeNotFound if no match exists.
func (r *sqliteRegistry) GetTypeByDBID(ctx context.Context, dbID string) (*AnnotationType, error) {
	row, err := r.s.GetAnnotationTypeByDBID(ctx, dbID)
	if err != nil {
		return nil, fmt.Errorf("annotations: GetTypeByDBID %q: %w", dbID, err)
	}
	if row == nil {
		return nil, fmt.Errorf("annotations: GetTypeByDBID %q: %w", dbID, schema.ErrTypeNotFound)
	}
	return rowToAnnotationType(row)
}

// ListTypes returns all annotation types matching the given filter.
func (r *sqliteRegistry) ListTypes(ctx context.Context, f schema.TypeFilter) ([]schema.AnnotationTypeSummary, error) {
	rows, err := r.s.ListAnnotationTypesWithFamily(ctx, string(f.Status), string(f.Origin))
	if err != nil {
		return nil, fmt.Errorf("annotations: ListTypes: %w", err)
	}

	var types []schema.AnnotationTypeSummary
	for i := range rows {
		row := &rows[i]

		// FamilyID filter (post-query, keeps SQL simple). f.FamilyID is a UUID FK.
		if f.FamilyID != "" && row.FamilyID != f.FamilyID {
			continue
		}
		// IncludeDeprecated filter: skip deprecated/retired unless opted in or status set.
		if !f.IncludeDeprecated && f.Status == "" {
			if row.Status == schema.StatusDeprecated || row.Status == schema.StatusRetired {
				continue
			}
		}

		s, err := rowWithFamilyToSummary(row)
		if err != nil {
			return nil, fmt.Errorf("annotations: ListTypes: %w", err)
		}
		types = append(types, *s)
	}
	return types, nil
}

// ValidateValue validates that value is permissible for the type identified by typeID.
// Returns nil if valid, or a wrapped schema.ErrTypeNotFound / schema.ErrInvalidValue.
func (r *sqliteRegistry) ValidateValue(ctx context.Context, typeID string, value string) error {
	at, err := r.GetType(ctx, typeID)
	if err != nil {
		return err
	}
	return schema.ValidateAnnotationValue(at.ValueDomain, value)
}

// ---------------------------------------------------------------------------
// schema.AnnotationRegistry (mutation) implementation
// ---------------------------------------------------------------------------

// Register inserts a new annotation type with status=proposed.
func (r *sqliteRegistry) Register(ctx context.Context, def schema.TypeDefinition) (*schema.AnnotationTypeSummary, error) {
	valueConstraint, err := marshalValueConstraint(def.ValueDomain)
	if err != nil {
		return nil, fmt.Errorf("annotations: Register %q: %w", def.TypeID, err)
	}

	params := store.CreateAnnotationTypeParams{
		TypeID:          def.TypeID,
		DisplayName:     def.DisplayName,
		Description:     def.Description,
		FamilyID:        def.FamilyID,
		ValueDomainKind: def.ValueDomain.Kind,
		Datatype:        def.ValueDomain.Datatype,
		ValueConstraint: valueConstraint,
		LowerIsBetter:   def.LowerIsBetter,
		Origin:          def.Origin,
	}
	newUUID, err := r.s.CreateAnnotationType(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("annotations: Register %q: %w", def.TypeID, err)
	}

	row, err := r.s.GetAnnotationTypeWithFamilyByDBID(ctx, newUUID)
	if err != nil {
		return nil, fmt.Errorf("annotations: Register %q: get created type: %w", def.TypeID, err)
	}
	if row == nil {
		return nil, fmt.Errorf("annotations: Register %q: created type not found", def.TypeID)
	}
	return rowWithFamilyToSummary(row)
}

// Activate transitions a type from proposed -> active.
func (r *sqliteRegistry) Activate(ctx context.Context, typeID string) error {
	// Verify type exists first.
	if _, err := r.GetType(ctx, typeID); err != nil {
		return fmt.Errorf("annotations: Activate: %w", err)
	}
	return r.s.ActivateAnnotationType(ctx, typeID)
}

// Deprecate transitions a type from active -> deprecated.
func (r *sqliteRegistry) Deprecate(ctx context.Context, typeID string, supersededBy string) error {
	if _, err := r.GetType(ctx, typeID); err != nil {
		return fmt.Errorf("annotations: Deprecate: %w", err)
	}
	if _, err := r.GetType(ctx, supersededBy); err != nil {
		return fmt.Errorf("annotations: Deprecate: supersededBy %q: %w", supersededBy, err)
	}
	return r.s.DeprecateAnnotationType(ctx, typeID, supersededBy)
}

// AddDependency records that typeID depends on dependsOn, with cycle detection (V14).
// Returns an error wrapping schema.ErrCycleDetected if the new edge would create a cycle.
func (r *sqliteRegistry) AddDependency(ctx context.Context, typeID, dependsOn string, required bool, rationale string) error {
	err := r.s.AddAnnotationTypeDependency(ctx, typeID, dependsOn, required, rationale)
	if err != nil && errors.Is(err, store.ErrAnnotationCycle) {
		return fmt.Errorf("annotations: AddDependency %q -> %q: %w", typeID, dependsOn, schema.ErrCycleDetected)
	}
	return err
}

// GetDependencies returns the dependency edges for typeID.
func (r *sqliteRegistry) GetDependencies(ctx context.Context, typeID string) ([]schema.TypeDependency, error) {
	storeDeps, err := r.s.GetAnnotationTypeDependencies(ctx, typeID)
	if err != nil {
		return nil, fmt.Errorf("annotations: GetDependencies %q: %w", typeID, err)
	}
	deps := make([]schema.TypeDependency, len(storeDeps))
	for i, d := range storeDeps {
		deps[i] = schema.TypeDependency{
			TypeID:    d.TypeID,
			DependsOn: d.DependsOn,
			Required:  d.Required,
			Rationale: d.Rationale,
		}
	}
	return deps, nil
}

// ---------------------------------------------------------------------------
// Conversion helpers
// ---------------------------------------------------------------------------

// rowWithFamilyToSummary converts a store AnnotationTypeWithFamilyRow to *schema.AnnotationTypeSummary.
func rowWithFamilyToSummary(row *store.AnnotationTypeWithFamilyRow) (*schema.AnnotationTypeSummary, error) {
	at, err := rowToAnnotationType(&row.AnnotationTypeRow)
	if err != nil {
		return nil, err
	}
	s := at.ToSummary(row.Family, row.Class)
	return &s, nil
}

// rowToAnnotationType converts a store AnnotationTypeRow to an *AnnotationType.
func rowToAnnotationType(row *store.AnnotationTypeRow) (*AnnotationType, error) {
	var permissibleValues []string
	if row.ValueConstraint != "" && row.ValueDomainKind == schema.DomainEnumerated {
		if err := json.Unmarshal([]byte(row.ValueConstraint), &permissibleValues); err != nil {
			return nil, fmt.Errorf("annotations: parse value_constraint for %q: %w", row.TypeID, err)
		}
	}

	at := &AnnotationType{
		dbID:        row.ID,
		id:          row.TypeID,
		version:     row.Version,
		displayName: row.DisplayName,
		description: row.Description,
		familyID:    row.FamilyID,
		valueDomain: schema.ValueDomain{
			Kind:              row.ValueDomainKind,
			Datatype:          row.Datatype,
			PermissibleValues: permissibleValues,
			ConstraintSpec:    row.ValueConstraint,
		},
		scaleKind:     row.ScaleKind,
		lowerIsBetter: row.LowerIsBetter,
		status:        row.Status,
		origin:        row.Origin,
		supersededBy:  row.SupersededBy,
		createdAt:     time.UnixMilli(row.CreatedAt),
	}
	// Convert *int64 to *int at the store boundary.
	if row.PriorityOverride != nil {
		v := int(*row.PriorityOverride)
		at.priorityOverride = &v
	}
	// For enumerated domains, ConstraintSpec is not needed; PermissibleValues is populated.
	if at.valueDomain.Kind == schema.DomainEnumerated {
		at.valueDomain.ConstraintSpec = ""
	}

	return at, nil
}

// marshalValueConstraint serialises the ValueDomain into the DB value_constraint column.
// For enumerated: JSON array of permissible values.
// For described: the ConstraintSpec string (already JSON).
func marshalValueConstraint(vd schema.ValueDomain) (string, error) {
	switch vd.Kind {
	case schema.DomainEnumerated:
		b, err := json.Marshal(vd.PermissibleValues)
		if err != nil {
			return "", fmt.Errorf("marshal enumerated permissible values: %w", err)
		}
		return string(b), nil
	case schema.DomainDescribed:
		return vd.ConstraintSpec, nil
	default:
		return "", fmt.Errorf("unknown value domain kind %q", vd.Kind)
	}
}
