// Package cmd contains the cobra commands for the iplane CLI.
package cmd

import (
	"errors"
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

// exitCoder is implemented by errors that want to drive a specific
// process exit code. `iplane deployment wait` returns one of these to
// distinguish timeout (3) from FAILED (2) from generic failure (1);
// `iplane deployment status` returns an exitCoder with an empty
// message so RUNNING / FAILED / other render only the stdout line
// without a stderr error.
type exitCoder interface {
	ExitCode() int
}

// Execute runs the root command. main.go calls this; its only job is
// to print the error and exit non-zero if the command fails so the
// shell sees the correct status code. Errors implementing exitCoder
// drive a custom exit code; an empty message suppresses the stderr
// line so scripting-friendly commands (`status`) can carry their
// signal entirely via exit code + stdout.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		var ec exitCoder
		if errors.As(err, &ec) {
			os.Exit(ec.ExitCode())
		}
		os.Exit(1)
	}
}

// exitWithCode is an exitCoder carrying no message -- the caller has
// already printed everything to stdout and just wants to choose the
// exit code. Used by `iplane deployment status`.
type exitWithCode int

func (e exitWithCode) Error() string { return "" }
func (e exitWithCode) ExitCode() int { return int(e) }

func init() {
	cobra.OnInitialize(initConfig)

	// --config is persistent so any subcommand can pick up a YAML file.
	// Subcommands that don't use config (load, gen-names) just ignore it.
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "",
		"config file (default deploy/config.yaml; YAML)")

	// --skip-model-validation bypasses the HF model-info pre-flight on
	// deploy / up. Useful when offline, when the HF API is degraded,
	// or for self-hosted models that aren't on the public Hub.
	// Default off (validation catches typos before pod spin).
	rootCmd.PersistentFlags().BoolVar(&skipModelValidation, "skip-model-validation", false,
		"skip the pre-flight model-spec validation against huggingface.co (use when offline or for non-HF models)")
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
