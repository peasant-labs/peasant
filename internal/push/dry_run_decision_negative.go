//go:build dry_run_capability_negative

package push

func stopBeforeRemoteNegotiation(bool) bool { return false }

const DryRunCapabilityMutation = true
