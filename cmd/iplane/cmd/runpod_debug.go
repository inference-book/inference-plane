package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// runpodCmd is a development / diagnostic group hosting provider-
// specific debug commands. Not part of the operator-facing surface
// the chapter teaches; here to make it easy to inspect what's in
// RunPod's account when the deployment surface misbehaves.
var runpodCmd = &cobra.Command{
	Use:   "runpod",
	Short: "RunPod-specific diagnostic commands",
	Long: `Provider-specific commands for inspecting RunPod state. Not part of
the operator-facing surface; useful when 'iplane deployment deploy'
fails with an SSH auth error and you need to compare what's in your
RunPod account vs. what iplane has locally.`,
}

var runpodDebugKeysCmd = &cobra.Command{
	Use:   "debug-keys",
	Short: "Print RunPod-side pubKey blob alongside the iplane-managed key",
	Long: `Fetches the user's pubKey blob from RunPod's GraphQL endpoint and
prints it next to iplane's local public key, so you can verify the
two are aligned.

Useful when 'iplane deployment deploy' fails with:

  ssh: handshake failed: ssh: unable to authenticate

A mismatch (or stale lines) is one of the common causes.

Requires RUNPOD_API_KEY in the environment (Full-scope key; the
GraphQL endpoint is not covered by scoped rpa_ keys).`,
	RunE: runRunpodDebugKeys,
}

func runRunpodDebugKeys(cmd *cobra.Command, args []string) error {
	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("RUNPOD_API_KEY is required (must be a Full-scope key; GraphQL is not in the rpa_ scope set)")
	}

	dir, err := resolveStateDir()
	if err != nil {
		return err
	}
	keyStore, err := sshkeys.New(sshkeys.WithDir(filepath.Join(dir, "keys")))
	if err != nil {
		return fmt.Errorf("open ssh key store: %w", err)
	}
	kp, err := keyStore.EnsureKeyPair(instanceOperatorID, provisioners.ProviderRunPod)
	if err != nil {
		return fmt.Errorf("load iplane keypair: %w", err)
	}
	localPub, err := kp.MarshalAuthorizedKey()
	if err != nil {
		return fmt.Errorf("marshal local public key: %w", err)
	}

	p := runpod.New(runpod.NewClient(apiKey))
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	blob, err := p.FetchUserPubKey(ctx)
	if err != nil {
		return fmt.Errorf("fetch runpod pubKey: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "iplane-managed key (the one the deployment executor uses):")
	fmt.Fprintln(out, "  comment:    "+kp.Comment)
	fmt.Fprintln(out, "  pub line:   "+strings.TrimSpace(string(localPub)))
	fmt.Fprintln(out)

	fmt.Fprintln(out, "RunPod user pubKey blob ("+countLines(blob)+" lines):")
	if blob == "" {
		fmt.Fprintln(out, "  (empty)")
	} else {
		matched := false
		for _, line := range strings.Split(strings.TrimRight(blob, "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			parsed, comment, _, _, perr := ssh.ParseAuthorizedKey([]byte(line))
			tag := "    "
			switch {
			case perr != nil:
				tag = "  ?!"
			case sshkeys.IsIplaneComment(comment):
				if equalKeyLines(line, strings.TrimSpace(string(localPub))) {
					tag = " >>>"
					matched = true
				} else {
					tag = "  !!" // stale iplane entry
				}
			}
			fmt.Fprintf(out, "%s %s\n", tag, line)
			_ = parsed
		}
		fmt.Fprintln(out)
		if matched {
			fmt.Fprintln(out, "Legend: >>> = local pub key match (good)")
		} else {
			fmt.Fprintln(out, "Legend: >>> = local pub key match (NOT FOUND -- this is the bug)")
		}
		fmt.Fprintln(out, "        !!  = iplane-tagged but bytes differ (stale; PR 31+ prunes these)")
		fmt.Fprintln(out, "        ?!  = malformed line (preserved verbatim)")
	}
	return nil
}

func countLines(s string) string {
	if s == "" {
		return "0"
	}
	n := 0
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return fmt.Sprintf("%d", n)
}

// equalKeyLines compares two authorized_keys lines by their parsed
// public-key bytes only, so trailing-whitespace / comment-string
// differences don't cause a false negative.
func equalKeyLines(a, b string) bool {
	pa, _, _, _, errA := ssh.ParseAuthorizedKey([]byte(a))
	pb, _, _, _, errB := ssh.ParseAuthorizedKey([]byte(b))
	if errA != nil || errB != nil {
		return false
	}
	return string(ssh.MarshalAuthorizedKey(pa)) == string(ssh.MarshalAuthorizedKey(pb))
}

func init() {
	rootCmd.AddCommand(runpodCmd)
	runpodCmd.AddCommand(runpodDebugKeysCmd)
}
