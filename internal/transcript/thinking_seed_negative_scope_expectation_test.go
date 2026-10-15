//go:build projection_negative_scope

package transcript

func thinkingSeedNegativeExpectedFailures(fixture thinkingSeedFixture) []string {
	return fixture.NegativeScopeFailures
}
