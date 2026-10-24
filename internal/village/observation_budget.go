package village

import (
	"context"
	"fmt"
	"sync/atomic"
)

// ObservationResponseLimit is the existing complete-response byte cap. A
// metadata invocation may additionally bound the sum of its source bodies.
const ObservationResponseLimit int64 = collectiveResponseLimit

type observationBudgetKey struct{}
type observationReadBudget struct{ remaining atomic.Int64 }

// WithObservationReadBudget charges source bytes before decoding/staging.
// Nested operations retain the original budget instead of resetting it.
func WithObservationReadBudget(ctx context.Context, bytes int64) context.Context {
	if _, ok := ctx.Value(observationBudgetKey{}).(*observationReadBudget); ok {
		return ctx
	}
	budget := &observationReadBudget{}
	budget.remaining.Store(bytes)
	return context.WithValue(ctx, observationBudgetKey{}, budget)
}
func observationBudgetAvailable(ctx context.Context) error {
	if budget, ok := ctx.Value(observationBudgetKey{}).(*observationReadBudget); ok && budget.remaining.Load() <= 0 {
		return fmt.Errorf("read complete metadata source: invocation byte budget exhausted; no response prefix can be installed; narrow targets or increase the configured source budget before retrying")
	}
	return nil
}
func chargeObservationBytes(ctx context.Context, size int64) error {
	if budget, ok := ctx.Value(observationBudgetKey{}).(*observationReadBudget); ok && budget.remaining.Add(-size) < 0 {
		return fmt.Errorf("read complete metadata source: invocation byte budget exceeded; dependent complete dataset was not staged; narrow targets or increase the configured source budget before retrying")
	}
	return nil
}
