package main

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/spf13/cobra"
)

func BuildLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Log out from the Peasant village",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := auth.Logout(cmd.Context()); err != nil {
				return err
			}
			fmt.Println("Logged out successfully")
			return nil
		},
	}
}
