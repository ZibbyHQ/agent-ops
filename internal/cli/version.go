// Copyright 2026 Zibby Lab. Apache-2.0.

package cli

import "github.com/spf13/cobra"

func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print agent-ops version.",
		Run: func(cmd *cobra.Command, _ []string) {
			printf(cmd, "%s\n", version)
		},
	}
}
