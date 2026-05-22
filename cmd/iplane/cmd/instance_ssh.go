package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var instanceSSHCmd = &cobra.Command{
	Use:   "ssh <id>",
	Short: "Materialize the SSH key and print the connect command",
	Args:  cobra.ExactArgs(1),
	Long: `Materialize the operator's iplane-managed SSH key for the named
instance into a 0600 temp file and print the ssh command an operator
can copy-paste into their shell. Saves the operator from having to
locate the key on disk or assemble the right '-i / -p / -o ...' flags
by hand.

The verb does NOT exec ssh itself -- the operator runs the printed
command. That gives them their natural interactive shell (vim, htop,
tmux, port-forwarding, anything ssh supports) without iplane sitting
in the middle of the session.

Works in both transports:

  - In-process (default): reads the key directly from the local
    keystore.
  - Remote (--service-url): fetches the key via the GetInstanceSSHKey
    RPC and materializes it locally.

The temp key file is NOT cleaned up automatically. The verb prints the
cleanup command alongside the ssh command; rm when you're done.

Security note: --service-url mode causes private key bytes to traverse
the gRPC connection. The v0.1 control plane has no per-operator auth;
rely on network isolation (localhost / private network) for safety.`,
	RunE: runInstanceSSH,
}

func runInstanceSSH(cmd *cobra.Command, args []string) error {
	id := args[0]

	client, err := buildClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

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

	// Write the key under a per-run temp dir so concurrent invocations
	// don't collide. The dir + key file deliberately survive the
	// command -- the operator copy-pastes the ssh command and uses
	// the file at their leisure, then cleans up via the printed rm.
	tmpDir, err := os.MkdirTemp("", "iplane-ssh-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
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

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Wrote SSH key to:\n  %s\n\n", keyPath)
	fmt.Fprintln(out, "Run this to connect:")
	fmt.Fprintf(out, "  ssh -i %s -p %d -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR %s@%s\n\n",
		keyPath, port, user, host)
	fmt.Fprintln(out, "Clean up when done:")
	fmt.Fprintf(out, "  rm -rf %s\n", tmpDir)
	return nil
}

func init() {
	instanceCmd.AddCommand(instanceSSHCmd)
}
