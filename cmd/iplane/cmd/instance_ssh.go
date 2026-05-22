package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var instanceSSHCmd = &cobra.Command{
	Use:   "ssh <id> [-- <ssh args>]",
	Short: "Open an interactive SSH session to the instance",
	Args:  cobra.MinimumNArgs(1),
	Long: `Open an interactive SSH session to the named instance using the
operator's iplane-managed keypair. Saves the operator from having to
locate the private key on disk or set up an SSH config block.

In in-process mode (default) the verb reads the keystore directly.
With --service-url it fetches the key bytes via the GetInstanceSSHKey
RPC from the running iplane serve, materializes them to a 0600 temp
file, runs ssh, and removes the temp file. Either way the operator
sees the same flow.

Security note: --service-url mode causes private key bytes to traverse
the gRPC connection. The v0.1 control plane has no per-operator auth;
rely on network isolation (localhost / private network) for safety.

Pass extra ssh flags after a literal '--':

  # interactive shell
  iplane instance ssh my-pod

  # port-forward 8000 from the pod to localhost
  iplane instance ssh my-pod -- -L 8000:localhost:8000

  # one-shot remote command
  iplane instance ssh my-pod -- cat /etc/os-release`,
	RunE: runInstanceSSH,
}

func runInstanceSSH(cmd *cobra.Command, args []string) error {
	id := args[0]
	extraArgs := args[1:]

	client, err := buildClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	// DescribeInstance gives us ssh.host / ssh.port. Source=LOCAL is
	// fine -- if state is stale (instance terminated since last sync),
	// ssh will fail loudly on connect, not silently succeed against
	// the wrong host.
	descResp, err := client.DescribeInstance(ctx, &provisionerv1.DescribeInstanceRequest{
		Id:     id,
		Source: provisionerv1.Source_SOURCE_LOCAL,
	})
	if err != nil {
		return fmt.Errorf("describe %q: %w", id, err)
	}
	inst := descResp.GetInstance()
	sshTarget := inst.GetSsh()
	if sshTarget == nil || sshTarget.GetHost() == "" {
		return fmt.Errorf("instance %q has no SSH endpoint in state (try 'iplane instance wait %s' first)", id, id)
	}

	keyResp, err := client.GetInstanceSSHKey(ctx, &provisionerv1.GetInstanceSSHKeyRequest{Id: id})
	if err != nil {
		return fmt.Errorf("fetch ssh key for %q: %w", id, err)
	}

	tmpDir, err := os.MkdirTemp("", "iplane-ssh-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	keyPath := filepath.Join(tmpDir, "id_ed25519")
	if err := os.WriteFile(keyPath, keyResp.GetPrivateKeyPem(), 0600); err != nil {
		return fmt.Errorf("write temp key: %w", err)
	}

	host := sshTarget.GetHost()
	port := int(sshTarget.GetPort())
	if port == 0 {
		port = 22
	}
	user := keyResp.GetUser()
	if user == "" {
		user = sshTarget.GetUser()
	}
	if user == "" {
		user = "root"
	}

	sshArgs := []string{
		"-i", keyPath,
		"-p", fmt.Sprintf("%d", port),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
	sshArgs = append(sshArgs, extraArgs...)
	sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", user, host))

	sshCmd := exec.Command("ssh", sshArgs...)
	sshCmd.Stdin = os.Stdin
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	if err := sshCmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitWithCode(exitErr.ExitCode())
		}
		return fmt.Errorf("invoke ssh: %w", err)
	}
	return nil
}

func init() {
	instanceCmd.AddCommand(instanceSSHCmd)
}
