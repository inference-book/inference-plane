package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/inference-book/inference-plane/internal/provisioners/state"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

var instanceSSHCmd = &cobra.Command{
	Use:   "ssh <id> [-- <ssh args>]",
	Short: "Open an interactive SSH session to the instance",
	Args:  cobra.MinimumNArgs(1),
	Long: `Open an interactive SSH session to the named instance using the
operator's iplane-managed keypair. Saves the operator from having to
locate the private key on disk or set up an SSH config block.

The verb reads the instance's ssh.host / ssh.port / ssh.user from
the local state file (so 'iplane instance wait' must have populated
them for providers like RunPod), writes the private key to a temp
file with 0600 permissions, and exec's the system 'ssh' binary
against it.

Pass extra ssh flags after a literal '--':

  # interactive shell
  iplane instance ssh my-pod

  # port-forward 8000 from the pod to localhost
  iplane instance ssh my-pod -- -L 8000:localhost:8000

  # one-shot remote command
  iplane instance ssh my-pod -- cat /etc/os-release

In-process mode only. With --service-url the keystore lives on the
server and the CLI has no way to read the private key bytes; use a
plain ssh command from the host running 'iplane serve' instead.`,
	RunE: runInstanceSSH,
	// Allow flags after positional args via DisableFlagParsing -- we
	// parse args ourselves so anything after '--' gets passed through
	// to ssh literally.
	DisableFlagParsing: false,
}

func runInstanceSSH(cmd *cobra.Command, args []string) error {
	if instanceServiceURL != "" {
		return fmt.Errorf("iplane instance ssh requires in-process mode (the CLI must read the private key directly); --service-url is set. From the host running iplane serve, use a plain 'ssh -i <key>' instead.")
	}

	id := args[0]
	extraArgs := args[1:]

	dir, err := resolveStateDir()
	if err != nil {
		return err
	}
	store, err := state.Open(dir, instanceOperatorID)
	if err != nil {
		return fmt.Errorf("open state store: %w", err)
	}
	file, err := store.Read()
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	inst, ok := file.Instances[id]
	if !ok {
		return fmt.Errorf("no instance with id %q", id)
	}
	sshTarget := inst.GetSsh()
	if sshTarget == nil || sshTarget.GetHost() == "" {
		return fmt.Errorf("instance %q has no SSH endpoint in state (try 'iplane instance wait %s' first)", id, id)
	}

	keyStore, err := sshkeys.New(sshkeys.WithDir(filepath.Join(dir, "keys")))
	if err != nil {
		return fmt.Errorf("open ssh key store: %w", err)
	}
	kp, err := keyStore.EnsureKeyPair(instanceOperatorID, inst.GetProvider())
	if err != nil {
		return fmt.Errorf("load ssh keypair for (%s,%s): %w", instanceOperatorID, inst.GetProvider(), err)
	}
	pemBytes, err := kp.MarshalPrivatePEM()
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}

	// Write the private key to a temp file. ssh refuses key files with
	// permissions wider than 0600, so we create with 0600 directly.
	// CreateTemp + Chmod has a TOCTOU window; opening with explicit
	// mode via OpenFile closes it.
	tmpDir, err := os.MkdirTemp("", "iplane-ssh-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	keyPath := filepath.Join(tmpDir, "id_ed25519")
	if err := os.WriteFile(keyPath, pemBytes, 0600); err != nil {
		return fmt.Errorf("write temp key: %w", err)
	}

	host := sshTarget.GetHost()
	port := int(sshTarget.GetPort())
	if port == 0 {
		port = 22
	}
	user := sshTarget.GetUser()
	if user == "" {
		user = "root"
	}

	sshArgs := []string{
		"-i", keyPath,
		"-p", fmt.Sprintf("%d", port),
		// v0.1: trust the provider's pod identity. TOFU + a managed
		// known_hosts file is a v0.2+ defense layer. Suppressing the
		// host-key prompt matches what the deployment executor does.
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
		// Pass through ssh's own exit code so scripts can branch on it.
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
