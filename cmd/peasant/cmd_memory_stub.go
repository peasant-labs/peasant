//go:build !experimental

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// BuildMemoryCommand returns a hidden stub for the experimental `peasant memory`
// command group.
//
// The full implementation lives in cmd_memory.go behind the `experimental`
// build tag (along with the internal/memory package it depends on). In the
// default build, that code is excluded, so this stub satisfies the static
// commands registry in main.go: the command is hidden from `peasant --help`
// and any invocation returns an actionable error directing the user to rebuild
// with -tags=experimental.
//
// This is the paired-file seam: cmd_memory.go (//go:build experimental) and
// cmd_memory_stub.go (//go:build !experimental) define mutually exclusive
// versions of BuildMemoryCommand, so exactly one is compiled per build.
func BuildMemoryCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "memory",
		Short:  "Agent memory construction and retrieval (experimental)",
		Hidden: true,
		// Silence the usage dump so the actionable error is the only output.
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("peasant memory is experimental; rebuild with -tags=experimental")
		},
	}
}
