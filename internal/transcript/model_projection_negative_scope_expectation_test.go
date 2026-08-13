//go:build projection_negative_scope

package transcript

func modelProjectionNegativeExpectedFailures() []string {
	return []string{"nested_observation_does_not_replace_root_seed"}
}
