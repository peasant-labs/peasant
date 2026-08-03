package main

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/spf13/cobra"
)

func BuildVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("peasant %s\n", defaults.Version)
		},
	}
}
