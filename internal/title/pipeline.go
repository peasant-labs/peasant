// Package title owns Peasant's process-scoped connection to the canonical title policy.
package title

import (
	"sync"

	"github.com/peasant-labs/redact"
)

// Pipeline is the complete title surface Peasant consumes.
type Pipeline interface {
	Generate(string, redact.TitleContext) (redact.TitleResult, error)
	Sanitize(string, redact.TitleContext) (redact.TitleResult, error)
}

var (
	defaultOnce sync.Once
	defaultPipe Pipeline
	defaultErr  error
)

// Default returns the immutable title pipeline shared by this process.
func Default() (Pipeline, error) {
	defaultOnce.Do(func() {
		defaultPipe, defaultErr = redact.NewTitlePipeline()
	})
	return defaultPipe, defaultErr
}
