package cmd

import (
	"github.com/spf13/cobra"
)

var (
	destroyForce  bool
	destroyDryRun bool
)

var instanceDestroyCmd = &cobra.Command{
	Use:   "destroy <id>",
	Short: "Terminate an instance and record it in the audit log",
	Args:  cobra.ExactArgs(1),
	Long: `Terminate the named instance via the provider and patch the local
state record to TERMINATED. Idempotent: a destroy against an already-
terminated record is a no-op, and an "instance not found" response
from the provider is treated as success (the desired end state is the
same).

--force skips the provider Terminate call. Use only when the provider
has confirmed the instance is gone and the local record is stuck in
TERMINATING.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help() // wired in commit 3
	},
}

func init() {
	instanceCmd.AddCommand(instanceDestroyCmd)

	f := instanceDestroyCmd.Flags()
	f.BoolVar(&destroyForce, "force", false,
		`skip the provider call; mark TERMINATED locally only (recovery)`)
	f.BoolVar(&destroyDryRun, "dry-run", false,
		`print the planned action and exit without provider calls`)
}
