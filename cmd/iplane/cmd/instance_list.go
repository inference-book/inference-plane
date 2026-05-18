package cmd

import (
	"github.com/spf13/cobra"
)

var (
	listProvider string
	listRemote   bool
)

var instanceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List iplane instances",
	Long: `List instances tracked by iplane.

Two sources, intentionally not unified -- the design doc treats them
as different questions worth asking separately:

  default              read from the local state file (~/.iplane/state.json).
                       Fast; the operator's own audit log.

  --remote             query the provider directly. Surfaces instances
                       under the operator's tag that the local state
                       file doesn't know about (the wiped-state-file
                       recovery path), and instances created outside
                       iplane that match the operator tag.

--remote requires --provider (we don't enumerate all configured
providers silently; see design doc line 99 -- v0.1 punts that to v0.3).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help() // wired in commit 3
	},
}

func init() {
	instanceCmd.AddCommand(instanceListCmd)

	f := instanceListCmd.Flags()
	f.StringVar(&listProvider, "provider", "", `restrict to one provider (required with --remote)`)
	f.BoolVar(&listRemote, "remote", false, `query the provider directly rather than the local state file`)
}
