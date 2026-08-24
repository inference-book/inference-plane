package lambdalabs

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"

	skhttp "github.com/panyam/servicekit/http"
)

// EnsurePublicKey satisfies provisioners.KeyRegistrar. The Service calls it
// before Spawn so that the key iplane will later SSH in with is the key the
// VM boots holding.
//
// Until this existed the adapter had no registrar at all, so the Service's
// registration step was skipped for Lambda, Spawn attached whatever key the
// account happened to list first, and the sshdocker executor then connected
// with iplane's own generated private key. The two were never the same key
// and the deploy path could not have worked on hardware.
//
// Lambda's key model is named objects rather than RunPod's single
// authorized_keys blob, so the shape here is list, compare, replace:
//
//   - a stored key whose material matches ours is success, whatever it is
//     named. That covers an operator who added iplane's key by hand.
//   - a stored key under our derived name whose material differs is stale,
//     left behind by a keystore that was regenerated. It is deleted before
//     the new one is added, because two keys under iplane's name would make
//     the launch-time lookup a coin toss.
//   - otherwise the key is POSTed.
//
// Comparison is on the parsed key material, so a differing comment or
// trailing whitespace does not read as a different key.
func (p *Provider) EnsurePublicKey(ctx context.Context, publicKey []byte, comment string) error {
	want, _, _, _, err := ssh.ParseAuthorizedKey(publicKey)
	if err != nil {
		return fmt.Errorf("parse own public key: %w", err)
	}
	wantLine := ssh.MarshalAuthorizedKey(want)

	keys, err := p.listSSHKeys(ctx)
	if err != nil {
		return wrapErr("ensure-key:list", err)
	}

	name := sshKeyName(comment)
	var staleID string
	for _, k := range keys {
		if parsed, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(k.PublicKey)); perr == nil {
			if string(ssh.MarshalAuthorizedKey(parsed)) == string(wantLine) {
				return nil
			}
		}
		if k.Name == name {
			staleID = k.ID
		}
	}

	if staleID != "" {
		if err := p.deleteSSHKey(ctx, staleID); err != nil {
			return wrapErr("ensure-key:delete-stale", err)
		}
	}

	body := map[string]any{
		"name":       name,
		"public_key": strings.TrimRight(string(publicKey), "\n"),
	}
	req, err := p.client.newReq(http.MethodPost, pathSSHKeys, nil, body)
	if err != nil {
		return wrapErr("ensure-key:add", err)
	}
	if err := skhttp.CallVoid(ctx, req, p.client.callOpts()...); err != nil {
		return wrapErr("ensure-key:add", err)
	}
	return nil
}

// rfc3339Suffix matches the timestamp sshkeys.FormatComment appends, so the
// derived name drops it.
var rfc3339Suffix = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(Z|[+-]\d{2}:\d{2})$`)

// unsafeForKeyName is everything outside what a Lambda key name is safe to
// carry. Lambda's schema states a length bound and no charset, so the set
// kept here is the conservative one.
var unsafeForKeyName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sshKeyName derives the name a key is stored under from iplane's key
// comment (`iplane-<operator>-<provider>-<rfc3339>`).
//
// The timestamp is dropped deliberately. Keeping it would give every
// regenerated keypair a new name, so a wiped keystore would leave the
// account accumulating iplane-prefixed keys and Spawn would have no way to
// tell which one the current private half matches. Dropping it makes the
// name stable per operator and provider, which turns re-registration into a
// replace.
//
// Lambda bounds the field at 1..64 characters, so the result is sanitized
// and truncated, and an empty comment falls back to a fixed name rather
// than an empty one the API would reject.
func sshKeyName(comment string) string {
	base := rfc3339Suffix.ReplaceAllString(strings.TrimSpace(comment), "")
	base = strings.Trim(unsafeForKeyName.ReplaceAllString(base, "-"), "-")
	if base == "" {
		return "iplane-key"
	}
	if len(base) > 64 {
		base = strings.TrimRight(base[:64], "-")
	}
	return base
}

// listSSHKeys returns every key stored on the operator's Lambda account.
func (p *Provider) listSSHKeys(ctx context.Context) ([]apiSSHKey, error) {
	req, err := p.client.newReq(http.MethodGet, pathSSHKeys, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := skhttp.Call[sshKeysResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// deleteSSHKey removes one stored key by id.
func (p *Provider) deleteSSHKey(ctx context.Context, id string) error {
	req, err := p.client.newReq(http.MethodDelete, pathSSHKeys+"/"+id, nil, nil)
	if err != nil {
		return err
	}
	return skhttp.CallVoid(ctx, req, p.client.callOpts()...)
}
