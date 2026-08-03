// Package annotations provides the runtime annotation type registry and validation logic.
//
// The registry supports two interface levels:
//   - AnnotationTypeReader: read-only access, consumed by classifiers and the ingest pipeline.
//   - AnnotationRegistry: full CRUD including Register, Activate, Deprecate, and dependency management.
//
// The production implementation (sqliteRegistry) wraps *store.Store and implements both interfaces.
// Classifiers and the pipeline depend on AnnotationTypeReader only (V23).
//
// Value validation follows ISO 11179: enumerated domains check set membership in
// permissibleValues; described domains accept any non-empty value (future: JSON schema).
//
// Cycle detection for annotation_type_deps uses a recursive CTE with depth limit 20 (V14).
package annotations
