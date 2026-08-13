//go:build projection_negative_seed

package transcript

func modelProjectionNegativeExpectedFailures() []string {
	return []string{"earliest_root_observation_beats_stored_metadata"}
}
