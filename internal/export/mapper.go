package export

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/store"
)

// MetricMapper maps a single AnnotationTypeRow to a target metric config T.
type MetricMapper[T any] interface {
	MapType(at store.AnnotationTypeRow) (T, error)
	FormatName() string
}

// ExportResult is the aggregate output of a generic Export operation.
type ExportResult[T any] struct {
	Configs    []T `json:"configs"`
	TotalTypes int `json:"total_types"`
}

// Export maps all annotation types through the given mapper, accumulating results.
func Export[T any](mapper MetricMapper[T], types []store.AnnotationTypeRow) (*ExportResult[T], error) {
	result := &ExportResult[T]{
		Configs: make([]T, 0, len(types)),
	}
	for _, at := range types {
		config, err := mapper.MapType(at)
		if err != nil {
			return nil, fmt.Errorf("export[%s]: type %q: %w", mapper.FormatName(), at.TypeID, err)
		}
		result.Configs = append(result.Configs, config)
		result.TotalTypes++
	}
	return result, nil
}
