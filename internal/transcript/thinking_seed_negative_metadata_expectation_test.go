//go:build projection_negative_seed

package transcript

func thinkingSeedNegativeExpectedFailures(fixture thinkingSeedFixture) []string {
	return fixture.NegativeSeedFailures
}
