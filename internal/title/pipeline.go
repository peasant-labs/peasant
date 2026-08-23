// Package title owns Peasant's process-scoped connection to the canonical title policy.
package title

import (
	"sync"

	"github.com/peasant-labs/redact"
)

// Pipeline is the complete title surface Peasant consumes.
type Pipeline interface {
	Generate(string, redact.TitleContext) (redact.TitleResult, error)
	// GenerateFromTurns returns the first usable generated title in turn
	// order, the index of the turn that produced it, and one error per turn
	// that was unusable. It returns index -1 when no turn is usable.
	GenerateFromTurns([]string, redact.TitleContext) (redact.TitleResult, int, []error)
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
