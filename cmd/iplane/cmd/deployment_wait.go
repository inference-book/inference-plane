package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var (
	waitForState string
	waitQuiet    bool
	waitTimeout  time.Duration
)

var deploymentWaitCmd = &cobra.Command{
	Use:   "wait <id>",
	Short: "Block until the deployment reaches a target state",
	Args:  cobra.ExactArgs(1),
	Long: `Block until the deployment reaches the target state (--for) or the
timeout expires. Designed for scripts that orchestrate multi-step
flows ("deploy, wait for RUNNING, run smoke test, destroy").

Exit codes:

  0   target state reached
  2   deployment entered FAILED before the target (unrecoverable;
      a wait --for running cannot succeed from FAILED)
  3   timeout expired before a terminal transition

The valid --for values are:

  running     engine reached RUNNING (typical use)
  terminated  destroy completed

Other states (PENDING, STARTING, etc.) are transient by design and
not useful wait targets. To observe transient states pass through,
use 'iplane deployment watch'.`,
	RunE: runDeploymentWait,
}

func runDeploymentWait(cmd *cobra.Command, args []string) error {
	id := args[0]
	target, err := parseWaitTarget(waitForState)
	if err != nil {
		return err
	}

	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), waitTimeout)
	defer cancel()

	out := cmd.OutOrStdout()
	var failed bool
	err = client.WatchDeployment(ctx, &provisionerv1.WatchDeploymentRequest{Id: id},
		func(evt *provisionerv1.DeploymentStateChangedEvent) error {
			if !waitQuiet {
				fmt.Fprintf(out, "%s  %s -> %s  phase=%s  %s\n",
					evt.GetAt().AsTime().Format(time.RFC3339),
					deploymentStateLabel(evt.GetFrom()),
					deploymentStateLabel(evt.GetTo()),
					orDash(evt.GetPhase()),
					evt.GetProgressMessage(),
				)
			}
			if evt.GetTo() == target {
				return errStopIteration
			}
			if evt.GetTo() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED && target != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
				failed = true
				return errStopIteration
			}
			return nil
		})
	switch {
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		return waitErr(3, fmt.Sprintf("timed out after %s waiting for %q to reach %s", waitTimeout, id, deploymentStateLabel(target)))
	case err != nil:
		return err
	case failed:
		return waitErr(2, fmt.Sprintf("%q entered FAILED before reaching %s", id, deploymentStateLabel(target)))
	}
	return nil
}

// parseWaitTarget restricts --for to the two states that make sense
// as wait targets in v0.1.
func parseWaitTarget(in string) (provisionerv1.DeploymentState, error) {
	switch strings.ToLower(in) {
	case "running":
		return provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, nil
	case "terminated":
		return provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED, nil
	case "":
		return 0, fmt.Errorf("--for is required (valid: running | terminated)")
	default:
		return 0, fmt.Errorf("invalid --for %q (valid: running | terminated)", in)
	}
}

// waitErr carries a target exit code so the root cobra exit-status
// path (rootCmd.SilenceErrors=false) prints the message AND we exit
// with the correct code. Implemented as a thin error type so the
// cobra wrapper in main.go can recognize it.
type waitExitError struct {
	code int
	msg  string
}

func (e *waitExitError) Error() string { return e.msg }
func (e *waitExitError) ExitCode() int { return e.code }

func waitErr(code int, msg string) error {
	return &waitExitError{code: code, msg: msg}
}

func init() {
	deploymentCmd.AddCommand(deploymentWaitCmd)

	f := deploymentWaitCmd.Flags()
	f.StringVar(&waitForState, "for", "", `target state to wait for: running | terminated (required)`)
	f.BoolVar(&waitQuiet, "quiet", false, `suppress per-transition output`)
	f.DurationVar(&waitTimeout, "timeout", 10*time.Minute, `maximum time to wait`)
}
