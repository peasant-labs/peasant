package annotations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/annotations"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// openTestStore opens a Store backed by a copy of the golden (pre-migrated) DB.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Open(t)
}

// ---------------------------------------------------------------------------
// AnnotationTypeReader — GetType
// ---------------------------------------------------------------------------

// TestGetType_SeedTypes verifies that the 4 seed annotation types from V13 migration
// are retrievable via GetType.
func TestGetType_SeedTypes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewTypeReader(openTestStore(t))

	seedTypeIDs := []string{
		testutil.TestTypeIDSessionApproval,
		testutil.TestTypeIDSessionOutcome,
		testutil.TestTypeIDUserFrustration,
		testutil.TestTypeIDSessionScope,
	}
	for _, typeID := range seedTypeIDs {
		at, err := reg.GetType(ctx, typeID)
		if err != nil {
			t.Errorf("GetType(%q): unexpected error: %v", typeID, err)
			continue
		}
		if at.TypeID != typeID {
			t.Errorf("GetType(%q): TypeID = %q, want %q", typeID, at.TypeID, typeID)
		}
		if at.Status != schema.StatusDeprecated {
			t.Errorf("GetType(%q): Status = %q, want %q", typeID, at.Status, schema.StatusDeprecated)
		}
	}
}

// TestGetType_NotFound verifies ErrTypeNotFound for unknown type IDs.
func TestGetType_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewTypeReader(openTestStore(t))

	_, err := reg.GetType(ctx, "quality.does_not_exist")
	if err == nil {
		t.Fatal("GetType(unknown): expected error, got nil")
	}
	if !errors.Is(err, schema.ErrTypeNotFound) {
		t.Errorf("GetType(unknown): error = %v, want wrapping schema.ErrTypeNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// AnnotationTypeReader — ListTypes
// ---------------------------------------------------------------------------

// TestListTypes_DefaultFilter returns only active/proposed types (excludes deprecated/retired).
func TestListTypes_DefaultFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewTypeReader(openTestStore(t))

	types, err := reg.ListTypes(ctx, schema.TypeFilter{})
	if err != nil {
		t.Fatalf("ListTypes: %v", err)
	}
	// Active types: research.friction_episode (V25) + user.custom_label (V35)
	// + quality.turn_outcome + quality.turn_flag (V39); every earlier type is
	// deprecated.
	if len(types) != 4 {
		t.Errorf("ListTypes (default): expected 4 active types, got %d", len(types))
	}
	for _, at := range types {
		if at.Status == schema.StatusDeprecated || at.Status == schema.StatusRetired {
			t.Errorf("ListTypes (default): included deprecated/retired type %q", at.TypeID)
		}
	}
}

// TestListTypes_StatusFilter returns only types with the requested status.
func TestListTypes_StatusFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewTypeReader(openTestStore(t))

	types, err := reg.ListTypes(ctx, schema.TypeFilter{Status: schema.StatusActive})
	if err != nil {
		t.Fatalf("ListTypes: %v", err)
	}
	if len(types) != 4 {
		t.Errorf("ListTypes (active): expected 4 types (research.friction_episode, user.custom_label, quality.turn_outcome, quality.turn_flag), got %d", len(types))
	}
}

// ---------------------------------------------------------------------------
// AnnotationTypeReader — ValidateValue
// ---------------------------------------------------------------------------

// TestValidateValue_Enumerated checks that enumerated domain validation accepts
// valid values and rejects invalid ones.
func TestValidateValue_Enumerated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewTypeReader(openTestStore(t))

	t.Run("approve is valid for session_approval", func(t *testing.T) {
		if err := reg.ValidateValue(ctx, testutil.TestTypeIDSessionApproval, "approve"); err != nil {
			t.Errorf("ValidateValue(approve): unexpected error: %v", err)
		}
	})

	t.Run("deny is valid for session_approval", func(t *testing.T) {
		if err := reg.ValidateValue(ctx, testutil.TestTypeIDSessionApproval, "deny"); err != nil {
			t.Errorf("ValidateValue(deny): unexpected error: %v", err)
		}
	})

	t.Run("invalid value rejected for session_approval", func(t *testing.T) {
		err := reg.ValidateValue(ctx, testutil.TestTypeIDSessionApproval, "maybe")
		if err == nil {
			t.Fatal("ValidateValue(maybe): expected error, got nil")
		}
		if !errors.Is(err, schema.ErrInvalidValue) {
			t.Errorf("ValidateValue(maybe): error = %v, want wrapping schema.ErrInvalidValue", err)
		}
	})

	t.Run("empty value rejected", func(t *testing.T) {
		err := reg.ValidateValue(ctx, testutil.TestTypeIDSessionApproval, "")
		if err == nil {
			t.Fatal("ValidateValue(''): expected error, got nil")
		}
		if !errors.Is(err, schema.ErrInvalidValue) {
			t.Errorf("ValidateValue(''): error = %v, want wrapping schema.ErrInvalidValue", err)
		}
	})
}

// TestValidateValue_TypeNotFound returns ErrTypeNotFound for unknown type IDs.
func TestValidateValue_TypeNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewTypeReader(openTestStore(t))

	err := reg.ValidateValue(ctx, "quality.nonexistent", "approve")
	if err == nil {
		t.Fatal("ValidateValue(unknown type): expected error, got nil")
	}
	if !errors.Is(err, schema.ErrTypeNotFound) {
		t.Errorf("ValidateValue(unknown type): error = %v, want wrapping schema.ErrTypeNotFound", err)
	}
}

// TestValidateValue_SessionOutcomeDomain verifies all 4 session_outcome values.
func TestValidateValue_SessionOutcomeDomain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewTypeReader(openTestStore(t))

	validValues := []string{"resolved", "partial", "failed", "abandoned"}
	for _, v := range validValues {
		if err := reg.ValidateValue(ctx, testutil.TestTypeIDSessionOutcome, v); err != nil {
			t.Errorf("ValidateValue(session_outcome, %q): unexpected error: %v", v, err)
		}
	}

	// V15: invalid values are rejected.
	invalidValues := []string{"ok", "unknown", "pass"}
	for _, v := range invalidValues {
		err := reg.ValidateValue(ctx, testutil.TestTypeIDSessionOutcome, v)
		if err == nil {
			t.Errorf("ValidateValue(session_outcome, %q): expected error, got nil", v)
		}
	}
}

// ---------------------------------------------------------------------------
// AnnotationRegistry — Register + Activate + Deprecate
// ---------------------------------------------------------------------------

// TestRegister_NewType verifies that a new type can be registered with proposed status.
func TestRegister_NewType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewRegistry(openTestStore(t))

	// Get a valid family UUID from seed data (V16: old int64=1 → deterministic UUID).
	sessionQualityFamily := store.GenerateEntityUUID("annotation_families", 1)

	at, err := reg.Register(ctx, schema.TypeDefinition{
		TypeID:      "quality.test_signal",
		DisplayName: "Test Signal",
		Description: "A test annotation type",
		FamilyID:    sessionQualityFamily,
		ValueDomain: schema.ValueDomain{
			Kind:              schema.DomainEnumerated,
			Datatype:          schema.DatatypeText,
			PermissibleValues: []string{"pass", "fail"},
		},
		Origin: schema.OriginUser,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if at.TypeID != "quality.test_signal" {
		t.Errorf("Register: TypeID = %q, want %q", at.TypeID, "quality.test_signal")
	}
	if at.Status != schema.StatusProposed {
		t.Errorf("Register: Status = %q, want %q", at.Status, schema.StatusProposed)
	}
}

// TestActivate_ProposedToActive verifies activation lifecycle transition.
func TestActivate_ProposedToActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewRegistry(openTestStore(t))

	_, err := reg.Register(ctx, schema.TypeDefinition{
		TypeID:      "quality.activate_test",
		DisplayName: "Activate Test",
		FamilyID:    store.GenerateEntityUUID("annotation_families", 1),
		ValueDomain: schema.ValueDomain{
			Kind:              schema.DomainEnumerated,
			Datatype:          schema.DatatypeText,
			PermissibleValues: []string{"yes", "no"},
		},
		Origin: schema.OriginUser,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := reg.Activate(ctx, "quality.activate_test"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	at, err := reg.GetType(ctx, "quality.activate_test")
	if err != nil {
		t.Fatalf("GetType after activate: %v", err)
	}
	if at.Status != schema.StatusActive {
		t.Errorf("after Activate: Status = %q, want %q", at.Status, schema.StatusActive)
	}
}

// TestDeprecate_ActiveToDeprecated verifies deprecation lifecycle transition.
func TestDeprecate_ActiveToDeprecated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewRegistry(openTestStore(t))

	// Register and activate two types (original and replacement).
	for _, def := range []schema.TypeDefinition{
		{TypeID: "quality.original", DisplayName: "Original", FamilyID: store.GenerateEntityUUID("annotation_families", 1),
			ValueDomain: schema.ValueDomain{Kind: schema.DomainEnumerated, Datatype: schema.DatatypeText, PermissibleValues: []string{"yes"}},
			Origin:      schema.OriginUser},
		{TypeID: "quality.replacement", DisplayName: "Replacement", FamilyID: store.GenerateEntityUUID("annotation_families", 1),
			ValueDomain: schema.ValueDomain{Kind: schema.DomainEnumerated, Datatype: schema.DatatypeText, PermissibleValues: []string{"yes"}},
			Origin:      schema.OriginUser},
	} {
		if _, err := reg.Register(ctx, def); err != nil {
			t.Fatalf("Register(%q): %v", def.TypeID, err)
		}
		if err := reg.Activate(ctx, def.TypeID); err != nil {
			t.Fatalf("Activate(%q): %v", def.TypeID, err)
		}
	}

	if err := reg.Deprecate(ctx, "quality.original", "quality.replacement"); err != nil {
		t.Fatalf("Deprecate: %v", err)
	}

	at, err := reg.GetType(ctx, "quality.original")
	if err != nil {
		t.Fatalf("GetType after deprecate: %v", err)
	}
	if at.Status != schema.StatusDeprecated {
		t.Errorf("after Deprecate: Status = %q, want %q", at.Status, schema.StatusDeprecated)
	}
}

// ---------------------------------------------------------------------------
// AnnotationRegistry — AddDependency + GetDependencies + Cycle Detection
// ---------------------------------------------------------------------------

// TestAddDependency_Valid adds a valid dependency edge.
func TestAddDependency_Valid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewRegistry(openTestStore(t))

	// Seed types: A depends on B (no cycle).
	for _, def := range []schema.TypeDefinition{
		{TypeID: "quality.type_a", DisplayName: "A", FamilyID: store.GenerateEntityUUID("annotation_families", 1),
			ValueDomain: schema.ValueDomain{Kind: schema.DomainEnumerated, Datatype: schema.DatatypeText, PermissibleValues: []string{"v"}},
			Origin:      schema.OriginUser},
		{TypeID: "quality.type_b", DisplayName: "B", FamilyID: store.GenerateEntityUUID("annotation_families", 1),
			ValueDomain: schema.ValueDomain{Kind: schema.DomainEnumerated, Datatype: schema.DatatypeText, PermissibleValues: []string{"v"}},
			Origin:      schema.OriginUser},
	} {
		if _, err := reg.Register(ctx, def); err != nil {
			t.Fatalf("Register(%q): %v", def.TypeID, err)
		}
	}

	if err := reg.AddDependency(ctx, "quality.type_a", "quality.type_b", false, "test dependency"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	deps, err := reg.GetDependencies(ctx, "quality.type_a")
	if err != nil {
		t.Fatalf("GetDependencies: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("GetDependencies: expected 1 dep, got %d", len(deps))
	}
	if deps[0].DependsOn != "quality.type_b" {
		t.Errorf("GetDependencies: DependsOn = %q, want %q", deps[0].DependsOn, "quality.type_b")
	}
	if deps[0].Required {
		t.Error("GetDependencies: Required = true, want false")
	}
}

// TestAddDependency_CycleDetected verifies V14: cycle detection prevents circular deps.
func TestAddDependency_CycleDetected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewRegistry(openTestStore(t))

	// Register types A, B, C.
	for _, def := range []schema.TypeDefinition{
		{TypeID: "quality.cycle_a", DisplayName: "A", FamilyID: store.GenerateEntityUUID("annotation_families", 1),
			ValueDomain: schema.ValueDomain{Kind: schema.DomainEnumerated, Datatype: schema.DatatypeText, PermissibleValues: []string{"v"}},
			Origin:      schema.OriginUser},
		{TypeID: "quality.cycle_b", DisplayName: "B", FamilyID: store.GenerateEntityUUID("annotation_families", 1),
			ValueDomain: schema.ValueDomain{Kind: schema.DomainEnumerated, Datatype: schema.DatatypeText, PermissibleValues: []string{"v"}},
			Origin:      schema.OriginUser},
		{TypeID: "quality.cycle_c", DisplayName: "C", FamilyID: store.GenerateEntityUUID("annotation_families", 1),
			ValueDomain: schema.ValueDomain{Kind: schema.DomainEnumerated, Datatype: schema.DatatypeText, PermissibleValues: []string{"v"}},
			Origin:      schema.OriginUser},
	} {
		if _, err := reg.Register(ctx, def); err != nil {
			t.Fatalf("Register(%q): %v", def.TypeID, err)
		}
	}

	// Build chain: A -> B -> C.
	if err := reg.AddDependency(ctx, "quality.cycle_a", "quality.cycle_b", false, "a->b"); err != nil {
		t.Fatalf("AddDependency(a->b): %v", err)
	}
	if err := reg.AddDependency(ctx, "quality.cycle_b", "quality.cycle_c", false, "b->c"); err != nil {
		t.Fatalf("AddDependency(b->c): %v", err)
	}

	// C -> A would create a cycle. Must fail with schema.ErrCycleDetected.
	err := reg.AddDependency(ctx, "quality.cycle_c", "quality.cycle_a", false, "c->a cycle")
	if err == nil {
		t.Fatal("AddDependency(c->a cycle): expected error, got nil")
	}
	if !errors.Is(err, schema.ErrCycleDetected) {
		t.Errorf("AddDependency(c->a cycle): error = %v, want wrapping schema.ErrCycleDetected", err)
	}
}

// TestGetDependencies_SeedDependency verifies seed dependencies from V13 and V19 migrations.
func TestGetDependencies_SeedDependency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewRegistry(openTestStore(t))

	// session_outcome has 2 deps: scope (V13, optional) + resolution_evidence (V19, required).
	deps, err := reg.GetDependencies(ctx, testutil.TestTypeIDSessionOutcome)
	if err != nil {
		t.Fatalf("GetDependencies: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("GetDependencies(session_outcome): expected 2, got %d", len(deps))
	}

	// Build a lookup map for assertions.
	depByTypeID := make(map[string]struct {
		Required bool
	})
	for _, d := range deps {
		depByTypeID[d.DependsOn] = struct{ Required bool }{d.Required}
	}

	// V13: outcome → scope (optional).
	if d, ok := depByTypeID[testutil.TestTypeIDSessionScope]; !ok {
		t.Error("missing dependency: session_outcome → session_scope")
	} else if d.Required {
		t.Error("session_outcome → session_scope: Required = true, want false")
	}

	// V19: outcome → resolution_evidence (required).
	if d, ok := depByTypeID[testutil.TestTypeIDResolutionEvidence]; !ok {
		t.Error("missing dependency: session_outcome → resolution_evidence")
	} else if !d.Required {
		t.Error("session_outcome → resolution_evidence: Required = false, want true")
	}
}

// ---------------------------------------------------------------------------
// AnnotationTypeSummary — field checks (replaces old AnnotationType method tests)
// ---------------------------------------------------------------------------

// TestAnnotationType_IsCurrent verifies that active seed types are not superseded.
// After refactor: checks Status field (not IsCurrent() method — that's internal domain only).
func TestAnnotationType_IsCurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewTypeReader(openTestStore(t))

	at, err := reg.GetType(ctx, testutil.TestTypeIDSessionApproval)
	if err != nil {
		t.Fatalf("GetType: %v", err)
	}
	// V25 deprecated all pre-research types.
	if at.Status != schema.StatusDeprecated {
		t.Errorf("Status = %q for deprecated seed type, want %q", at.Status, schema.StatusDeprecated)
	}
}

// TestAnnotationType_Family verifies Family field on the wire type.
// After refactor: Family is the display name string, not the UUID.
func TestAnnotationType_Family(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewTypeReader(openTestStore(t))

	at, err := reg.GetType(ctx, testutil.TestTypeIDSessionApproval)
	if err != nil {
		t.Fatalf("GetType: %v", err)
	}
	// Seed type belongs to the "session_quality" family.
	if at.Family == "" {
		t.Error("Family = empty string, want non-empty family name")
	}
}

// TestAnnotationType_ValidateValue verifies value validation via the registry interface.
// After refactor: uses schema.ValidateAnnotationValue on the wire type's ValueDomain.
func TestAnnotationType_ValidateValue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := annotations.NewTypeReader(openTestStore(t))

	at, err := reg.GetType(ctx, testutil.TestTypeIDSessionApproval)
	if err != nil {
		t.Fatalf("GetType: %v", err)
	}

	if err := schema.ValidateAnnotationValue(at.ValueDomain, "approve"); err != nil {
		t.Errorf("ValidateAnnotationValue(approve): %v", err)
	}
	if err := schema.ValidateAnnotationValue(at.ValueDomain, "bad"); err == nil {
		t.Error("ValidateAnnotationValue(bad): expected error")
	}
}
