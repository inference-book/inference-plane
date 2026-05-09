// Package cmd contains the cobra commands for the iplane CLI.
package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Persistent flags / values shared across subcommands. Bound to viper
// keys via viper.BindPFlag in init().
var (
	cfgFile string
)

// rootCmd is the parent of every subcommand. cobra dispatches to the
// matching child based on the first positional argument; with no
// argument it prints --help.
var rootCmd = &cobra.Command{
	Use:           "iplane",
	Short:         "Inference Plane control plane and tooling",
	SilenceUsage:  true,
	SilenceErrors: false,
	Long: `iplane runs and operates the v0.1 inference control plane.

Subcommands:
  serve       run the control plane server
  load        fire synthetic OpenAI requests at a running stack
  gen-names   regenerate the OTel name vocabulary

The same binary is the docker image entrypoint (iplane serve) and the
local tooling for load testing and code generation. Run any subcommand
with --help for its specific flags.`,
}

// Execute runs the root command. main.go calls this; its only job is
// to print the error and exit non-zero if the command fails so the
// shell sees the correct status code.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	// --config is persistent so any subcommand can pick up a YAML file.
	// Subcommands that don't use config (load, gen-names) just ignore it.
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "",
		"config file (default deploy/config.yaml; YAML)")
}

// initConfig sets up viper's config sources before any subcommand
// runs: the explicit --config path if given, otherwise walks
// conventional locations. Env vars are bound automatically with the
// IPLANE_ prefix; nested keys map by replacing '.' with '_' so
// 'server.addr' binds to IPLANE_SERVER_ADDR.
//
// Read errors that aren't 'file not found' are fatal -- a malformed
// config file is more likely a bug than an intentional 'no config'
// state, and silently ignoring it would surface as a confusing
// 'why aren't my settings taking effect' problem at runtime.
func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath("/etc/iplane")
		viper.AddConfigPath("./deploy")
		viper.AddConfigPath(".")
	}

	viper.SetEnvPrefix("IPLANE")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			fmt.Fprintln(os.Stderr, "iplane: read config:", err)
			os.Exit(1)
		}
	}
}
