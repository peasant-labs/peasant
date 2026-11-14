package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/spf13/cobra"
)

// takeHarvestRunLock holds the data dir's harvest run lock for a maintenance
// command that writes the same data dir as a harvest. While a harvest holds
// it, the command says so once on stderr and waits; ctrl+c cancels the wait.
func takeHarvestRunLock(cmd *cobra.Command, command string) (func(), error) {
	path := filepath.Join(string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd))), ingest.RunLockFileName)
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := ingest.AcquireRunLock(ctx, path, func() {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", command, ingest.RunLockWaitNotice)
	})
	if err != nil {
		return nil, fmt.Errorf("%s: nothing was changed: %w", command, err)
	}
	return release, nil
}
