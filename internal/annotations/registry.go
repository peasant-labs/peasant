package annotations

import (
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// Compile-time interface guards — sqliteRegistry must implement both schema interfaces.
var (
	_ schema.AnnotationTypeReader = (*sqliteRegistry)(nil)
	_ schema.AnnotationRegistry   = (*sqliteRegistry)(nil)
)

// NewRegistry creates a new sqliteRegistry wrapping the given store.
// The returned value implements both schema.AnnotationTypeReader and schema.AnnotationRegistry.
func NewRegistry(s *store.Store) schema.AnnotationRegistry {
	return &sqliteRegistry{s: s}
}

// NewTypeReader returns a schema.AnnotationTypeReader backed by the given store.
// Use this in contexts that should only read types (classifiers, pipeline, REST handlers).
func NewTypeReader(s *store.Store) schema.AnnotationTypeReader {
	return &sqliteRegistry{s: s}
}
