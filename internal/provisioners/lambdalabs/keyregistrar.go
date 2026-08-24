package lambdalabs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
//   - otherwise the key is POSTed under a name that identifies the key.
//
// **Nothing is ever deleted.** An earlier version replaced a key stored under
// iplane's derived name, so the name stayed one-to-one and Spawn could find
// it by prefix. Lambda refuses to delete a key any running instance
// references ("Key is currently in use, cannot delete"), which turned a
// second keystore into a total inability to rent for as long as the first
// machine was up (#442). Two people on one account, or CI and a laptop, is
// all it takes.
//
// So the name carries a digest of the key rather than identifying the
// operator alone, several iplane keys may coexist, and the Provider remembers
// which one it registered. See sshKeyName.
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

	name := sshKeyName(comment, publicKey)
	for _, k := range keys {
		if parsed, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(k.PublicKey)); perr == nil {
			if string(ssh.MarshalAuthorizedKey(parsed)) == string(wantLine) {
				p.rememberKeyName(k.Name)
				return nil
			}
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
	p.rememberKeyName(name)
	return nil
}

// rememberKeyName records which stored key holds the public half of the pair
// this operator will SSH with, so Spawn attaches that one.
//
// The memo is what pays for dropping the delete. With one key per keypair
// rather than one per operator, an account can hold several `iplane-` keys
// and a prefix scan can no longer tell which private half the caller holds.
//
// It is reliable because the Service calls EnsurePublicKey immediately before
// Spawn on every path that rents (registerKeyAndSpawn, and the fan-out's
// pre-flight). A Spawn that somehow arrives without it falls back to the
// prefix scan and then to the account's first key, which is what an operator
// driving the adapter by hand has always got.
func (p *Provider) rememberKeyName(name string) {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	p.registeredKeyName = name
}

// registeredKey returns the name EnsurePublicKey last registered, if any.
func (p *Provider) registeredKey() string {
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	return p.registeredKeyName
}

// rfc3339Suffix matches the timestamp sshkeys.FormatComment appends, so the
// derived name drops it.
var rfc3339Suffix = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(Z|[+-]\d{2}:\d{2})$`)

// unsafeForKeyName is everything outside what a Lambda key name is safe to
// carry. Lambda's schema states a length bound and no charset, so the set
// kept here is the conservative one.
var unsafeForKeyName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sshKeyName derives the name a key is stored under: iplane's key comment
// with the RFC3339 timestamp dropped, plus a short digest of the public key.
//
// Two forces pull on this name and the digest satisfies both.
//
// The timestamp goes, because keeping it would give every re-registration of
// the *same* key a new name and the account would accumulate duplicates of
// one key. The digest arrives, because without it two *different* keypairs
// collide on one name, and resolving that collision means deleting, which
// Lambda refuses while a running instance references the key (#442). With the
// digest, re-registering an unchanged key is a no-op and a regenerated
// keypair is simply a second entry.
//
// The tradeoff is that the name no longer identifies the operator on its own,
// so Spawn cannot pick by prefix and the Provider remembers what it
// registered. See rememberKeyName.
//
// Lambda bounds the field at 1..64 characters. The readable half is truncated
// to make room rather than the digest being shortened: two operators whose
// ids share a long prefix still get distinct names, where a truncated digest
// would put them back in collision.
func sshKeyName(comment string, publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	digest := hex.EncodeToString(sum[:4])

	base := rfc3339Suffix.ReplaceAllString(strings.TrimSpace(comment), "")
	base = strings.Trim(unsafeForKeyName.ReplaceAllString(base, "-"), "-")
	if base == "" {
		base = "iplane-key"
	}
	if limit := 64 - len(digest) - 1; len(base) > limit {
		base = strings.TrimRight(base[:limit], "-")
	}
	return base + "-" + digest
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
