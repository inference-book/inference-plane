package vast

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	skhttp "github.com/panyam/servicekit/http"
	"golang.org/x/crypto/ssh"

	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// pathSSHKeys is Vast's account-level SSH key collection. Keys live on the
// account rather than on an instance: Vast injects every registered key into
// each machine it rents you, so registration happens once and applies to
// everything created afterwards.
const pathSSHKeys = "/api/v0/ssh/"

// EnsurePublicKey satisfies provisioners.KeyRegistrar.
//
// Without this, iplane could rent a Vast machine and then never log into it.
// The rent call asks for `runtype: "ssh"` and Vast dutifully starts sshd,
// but the only keys in the machine's authorized_keys are the ones registered
// on the account, and iplane generates its keypair locally. So every deploy
// died at the SSH dial, before `docker run` was ever reached, on a machine
// that was already billing. Creating instances worked, which is what made
// the gap easy to miss: the failure was one capability further along than
// anything being tested.
//
// Ordering matters and is the Service's, not ours: EnsurePublicKey runs
// once per CreateInstance *before* Spawn, so the key is on the account
// before the machine exists. Registering afterwards would be a race against
// the machine's own boot.
//
// Idempotent, and it has to be, because it runs on every instance create.
// Vast returns the account's keys as a list rather than RunPod's single
// concatenated blob, so the check is a membership test instead of a
// read-modify-write: parse each registered key, compare the wire bytes, and
// return early on a match. Comparing parsed bytes rather than strings means
// trailing whitespace or a differing comment does not cause a duplicate
// upload of a key that is already there.
func (p *Provider) EnsurePublicKey(ctx context.Context, publicKey []byte, comment string) error {
	want, _, _, _, err := ssh.ParseAuthorizedKey(publicKey)
	if err != nil {
		return fmt.Errorf("vast: parse iplane public key: %w", err)
	}

	existing, err := p.listSSHKeys(ctx)
	if err != nil {
		return err
	}
	for _, k := range existing {
		got, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(k.SSHKey))
		if perr != nil {
			// A key we cannot parse is someone else's problem, not a
			// reason to fail the deploy. Skip it and keep looking.
			continue
		}
		if string(got.Marshal()) == string(want.Marshal()) {
			return nil
		}
	}

	// Not present: upload it. The comment travels inside the key line, the
	// way authorized_keys carries it, so the account listing stays
	// attributable to iplane rather than being an anonymous blob.
	line := strings.TrimSpace(string(publicKey))
	if comment != "" && !strings.HasSuffix(line, comment) {
		line = line + " " + comment
	}
	req, err := p.client.newReq(http.MethodPost, pathSSHKeys, nil, map[string]any{
		"ssh_key": line,
	})
	if err != nil {
		return err
	}
	if _, err := skhttp.Call[sshKeyCreateResponse](ctx, req, p.client.callOpts()...); err != nil {
		return fmt.Errorf("vast: register ssh key: %w", err)
	}
	return nil
}

// listSSHKeys returns the SSH keys registered on the account.
func (p *Provider) listSSHKeys(ctx context.Context) ([]sshKeyRecord, error) {
	req, err := p.client.newReq(http.MethodGet, pathSSHKeys, nil, nil)
	if err != nil {
		return nil, err
	}
	keys, err := skhttp.Call[[]sshKeyRecord](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, fmt.Errorf("vast: list ssh keys: %w", err)
	}
	return keys, nil
}

// sshKeyRecord is one entry in Vast's account SSH key list.
type sshKeyRecord struct {
	ID     int    `json:"id"`
	SSHKey string `json:"ssh_key"`
}

// sshKeyCreateResponse is Vast's reply to a key upload. Only the success
// flag is load-bearing; the rest of the body varies and is not relied on.
type sshKeyCreateResponse struct {
	Success bool   `json:"success"`
	Msg     string `json:"msg"`
	ID      int    `json:"id"`
}

// IplaneKeyComments returns the comments of every iplane-registered key on
// the account. Exported for diagnostics: an operator debugging "why can I
// not ssh in" wants to know what iplane thinks it registered, and the Vast
// console shows keys without making their provenance obvious.
func (p *Provider) IplaneKeyComments(ctx context.Context) ([]string, error) {
	keys, err := p.listSSHKeys(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, k := range keys {
		_, comment, _, _, perr := ssh.ParseAuthorizedKey([]byte(k.SSHKey))
		if perr == nil && sshkeys.IsIplaneComment(comment) {
			out = append(out, comment)
		}
	}
	return out, nil
}
