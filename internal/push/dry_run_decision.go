//go:build !dry_run_capability_negative

package push

func stopBeforeRemoteNegotiation(dryRun bool) bool { return dryRun }
