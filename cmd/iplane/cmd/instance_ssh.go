package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var instanceSSHCmd = &cobra.Command{
	Use:   "ssh <id> [-- <ssh args...>]",
	Short: "Drop into the instance's SSH session in one step",
	Args:  cobra.MinimumNArgs(1),
	Long: `Materialize the operator's iplane-managed SSH key for the named
instance, then exec-replace iplane with ssh -- the operator drops
straight into the remote shell. Closing the session returns to the
operator's original prompt; no iplane process sits in memory for the
duration.

Args after the instance id are passed through to ssh verbatim, so
flags like port-forwards and agent-forwarding work as on any normal
ssh invocation:

  iplane instance ssh my-pod                       # plain shell
  iplane instance ssh my-pod -- -L 8080:localhost:8000   # local port-forward
  iplane instance ssh my-pod -- -A                       # agent-forward
  iplane instance ssh my-pod -- ls /workspace            # remote command

Works in both transports:

  - In-process (default): reads the key directly from the local
    keystore.
  - Remote (--service-url): fetches the key via the GetInstanceSSHKey
    RPC and materializes it locally.

Key residency:

The private key is materialized to a tmpfs-backed per-user directory
when one is available (Linux: /run/user/$UID, RAM-backed and wiped at
session end), falling back to the OS temp directory otherwise (macOS
/tmp, swept by the 3-day periodic cleanup). Because syscall.Exec
replaces the iplane process, the key file is NOT removed at session
exit -- the OS sweep is the cleanup mechanism.

Security note: --service-url mode causes private key bytes to traverse
the gRPC connection. The v0.1 control plane has no per-operator auth;
rely on network isolation (localhost / private network) for safety.`,
	RunE: runInstanceSSH,
}

func runInstanceSSH(cmd *cobra.Command, args []string) error {
	id := args[0]
	extraArgs := args[1:] // pass-through to ssh

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
		return fmt.Errorf("instance %q has no SSH endpoint -- either the deployment was created with the cost-aware proxy-only default (re-deploy with --debug-shell to opt into a routable publicIp + sshd), or the wait hasn't completed yet (run 'iplane instance wait %s' for an explicit error)", id, id)
	}

	keyResp, err := client.GetInstanceSSHKey(ctx, &provisionerv1.GetInstanceSSHKeyRequest{Id: id})
	if err != nil {
		return fmt.Errorf("fetch ssh key for %q: %w", id, err)
	}

	keyPath, err := materializeKey(keyResp.GetPrivateKeyPem())
	if err != nil {
		return fmt.Errorf("materialize key: %w", err)
	}

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found on PATH: %w", err)
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

	argv := buildSSHArgv(filepath.Base(sshBin), keyPath, user, host, port, extraArgs)

	// Exec-replace the iplane process with ssh. On success this does
	// NOT return; the operator's shell takes over from ssh's exit.
	// The cleanup of the key file is left to the OS (see verb's
	// Long doc).
	return syscall.Exec(sshBin, argv, os.Environ())
}

// buildSSHArgv assembles the argv passed to syscall.Exec. The first
// element is argv[0] (program name as seen by ssh, conventionally the
// basename of the binary).
//
// Two orderings are required at once and they pull in opposite directions.
// ssh options must come BEFORE the destination, because ssh stops parsing
// options at the first non-option word; a remote command must come AFTER
// it, because that word is where the command starts. The pass-through list
// holds both, so it is split rather than placed wholesale at either end.
//
// Putting all of it before the destination is what this used to do, and it
// made the documented `iplane instance ssh my-pod -- ls /workspace` fail
// with "Could not resolve hostname ls": ssh read the command's first word
// as the machine to connect to. The comment here even explained the
// reasoning, which was sound for the forwarding flags it was written for
// and exactly backwards for a command.
//
// iplane's own options stay first so an operator can override them.
//
// Pure function so tests can assert the shape without invoking ssh.
func buildSSHArgv(progName, keyPath, user, host string, port int, extraArgs []string) []string {
	argv := []string{
		progName,
		"-i", keyPath,
		"-p", strconv.Itoa(port),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
	opts, command := splitSSHArgs(extraArgs)
	argv = append(argv, opts...)
	argv = append(argv, fmt.Sprintf("%s@%s", user, host))
	return append(argv, command...)
}

// sshValueFlags are the ssh short options whose value is a separate word, so
// "-L" must carry "8080:localhost:8000" across the destination with it.
// Taken from ssh(1); the combined spellings ("-p22", "-oFoo=bar") need no
// entry because they are already one word.
var sshValueFlags = map[string]bool{
	"-B": true, "-b": true, "-c": true, "-D": true, "-E": true, "-e": true,
	"-F": true, "-I": true, "-i": true, "-J": true, "-L": true, "-l": true,
	"-m": true, "-O": true, "-o": true, "-p": true, "-Q": true, "-R": true,
	"-S": true, "-W": true, "-w": true,
}

// splitSSHArgs divides pass-through args into ssh options and the remote
// command, at the first word that is not an option or an option's value.
//
// That split point is not a heuristic about what the words mean: it is
// where ssh itself stops reading options, so the two agree by construction.
// "-A -L 8080:x ls /tmp" gives options "-A -L 8080:x" and command "ls /tmp",
// and a bare "ls /tmp" gives no options at all.
//
// A lone "--" is dropped, so an operator who writes a second one to be
// explicit about where their command starts gets what they meant rather
// than ssh trying to run it.
func splitSSHArgs(args []string) (opts, command []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return opts, args[i+1:]
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			return opts, args[i:]
		}
		opts = append(opts, a)
		// A value-taking flag carries the next word only when that word is
		// not itself a flag; "-o -A" is a malformed invocation ssh should
		// complain about, not one to paper over by swallowing the -A.
		if sshValueFlags[a] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			opts = append(opts, args[i])
		}
	}
	return opts, nil
}

// materializeKey writes the supplied PEM bytes to a tmpfs-backed
// per-user directory (Linux /run/user/$UID) when one is available,
// or the OS temp directory otherwise. Returns the path to the key
// file. The directory + file are deliberately NOT cleaned up at exit
// -- syscall.Exec replaces the process so no defer would fire; the
// OS sweep is the cleanup mechanism (see verb's Long doc).
func materializeKey(pem []byte) (string, error) {
	dir, err := os.MkdirTemp(preferredKeyDir(), "iplane-ssh-*")
	if err != nil {
		return "", err
	}
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, pem, 0600); err != nil {
		return "", err
	}
	return keyPath, nil
}

// preferredKeyDir returns the directory iplane should write the
// short-lived private key into. On Linux with systemd, /run/user/$UID
// is RAM-backed (tmpfs) and per-user — both better for key residency
// than /tmp. Elsewhere (notably macOS, which has no equivalent
// per-user tmpfs), fall back to the OS temp directory.
func preferredKeyDir() string {
	if runtime.GOOS == "linux" {
		if uid := os.Getuid(); uid > 0 {
			xdg := fmt.Sprintf("/run/user/%d", uid)
			if info, err := os.Stat(xdg); err == nil && info.IsDir() {
				return xdg
			}
		}
	}
	return os.TempDir()
}

func init() {
	instanceCmd.AddCommand(instanceSSHCmd)
}
