// Package sshkeys handles iplane-managed SSH key pairs. v0.1's
// chapter narrative keeps key management invisible to operators: the
// CLI auto-generates a key on first `iplane instance create runpod ...`,
// uploads it to the provider, and uses it at deploy time. Operators
// never type ssh-keygen, never touch ~/.ssh/, never pass --ssh-key.
//
// Two concerns split across two files:
//
//   - keys.go  : pure key-pair generation + serialization (Ed25519,
//                OpenSSH authorized_keys public format, PEM private
//                format). No I/O.
//   - store.go : persistence + scoping. Wraps oneauth's FSKeyStore
//                to give an EnsureKeyPair(operator, provider) call
//                that returns an existing key or generates a new one.
//
// Encryption at rest is deferred to filesystem permissions in v0.1
// (the JSON envelope lands at 0600 via oneauth's FSKeyStore, same as
// ssh-keygen's id_rsa default). Encrypting the asymmetric private
// key is a tracked oneauth followup but is theoretical for v0.1's
// threat model (operator laptop, FileVault/LUKS handles disk-at-rest).
package sshkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Algorithm is the protojson-stored algorithm name we tag SSH keys
// with in oneauth's KeyRecord. Chosen to look like the OpenSSH key
// type string so anyone inspecting the on-disk JSON sees a familiar
// label, not "raw bytes".
const Algorithm = "ssh-ed25519"

// KeyPair carries an Ed25519 pair plus the operator/provider scope
// it was generated for. The Comment is the trailing string we stamp
// on the public-key authorized_keys line so the iplane key can be
// identified later (for skip-if-present checks on the provider side
// and for cleanup).
type KeyPair struct {
	Operator   string
	Provider   string
	CreatedAt  time.Time
	Public     ed25519.PublicKey
	Private    ed25519.PrivateKey
	Comment    string
}

// Generate returns a fresh Ed25519 key pair scoped to (operator,
// provider). The comment string is deterministic:
//
//	iplane-<operator>-<provider>-<created_at_rfc3339>
//
// so the runpod adapter (and any other KeyRegistrar) can recognize
// iplane's own entries in the provider's authorized_keys blob and
// skip re-uploading when one is already present.
func Generate(operator, provider string, now time.Time) (*KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ed25519 GenerateKey: %w", err)
	}
	return &KeyPair{
		Operator:  operator,
		Provider:  provider,
		CreatedAt: now,
		Public:    pub,
		Private:   priv,
		Comment:   FormatComment(operator, provider, now),
	}, nil
}

// FormatComment is the canonical iplane comment string for a key.
// Exposed so tests + KeyRegistrar implementations can construct the
// same string for skip-if-present comparisons.
func FormatComment(operator, provider string, now time.Time) string {
	return fmt.Sprintf("iplane-%s-%s-%s", operator, provider, now.UTC().Format(time.RFC3339))
}

// IsIplaneComment returns true if a parsed authorized_keys comment
// belongs to iplane (prefix-match on "iplane-"). The runpod
// KeyRegistrar uses this to find iplane's own entries in the blob
// without depending on an exact match to a specific timestamp.
func IsIplaneComment(comment string) bool {
	return strings.HasPrefix(comment, "iplane-")
}

// MarshalAuthorizedKey returns the public half in OpenSSH
// authorized_keys format: "ssh-ed25519 BASE64 <comment>\n". Suitable
// to concatenate into a server-side authorized_keys file or into
// RunPod's user-level pubKey blob.
func (k *KeyPair) MarshalAuthorizedKey() ([]byte, error) {
	sshPub, err := ssh.NewPublicKey(k.Public)
	if err != nil {
		return nil, fmt.Errorf("ssh.NewPublicKey: %w", err)
	}
	line := ssh.MarshalAuthorizedKey(sshPub)
	// MarshalAuthorizedKey already ends in '\n'; splice the comment
	// in before that. The library does not accept a comment arg.
	trimmed := strings.TrimRight(string(line), "\n")
	return []byte(trimmed + " " + k.Comment + "\n"), nil
}

// MarshalPrivatePEM returns the private key as a PEM-encoded
// OPENSSH PRIVATE KEY block. The block can be written to a file with
// 0600 perms and used directly as ssh -i.
func (k *KeyPair) MarshalPrivatePEM() ([]byte, error) {
	// ssh.MarshalPrivateKey handles the OPENSSH PRIVATE KEY framing.
	block, err := ssh.MarshalPrivateKey(k.Private, k.Comment)
	if err != nil {
		return nil, fmt.Errorf("ssh.MarshalPrivateKey: %w", err)
	}
	return pem.EncodeToMemory(block), nil
}

// UnmarshalPrivatePEM parses a PEM-encoded OPENSSH PRIVATE KEY block
// back into a KeyPair. The comment is passed in explicitly because
// `ssh.MarshalPrivateKey` bakes it into the OpenSSH binary blob
// rather than the PEM headers, and parsing that binary form here
// would couple the package to internal OpenSSH format details. The
// Store caches the comment in oneauth's KeyRecord.Kid alongside the
// PEM bytes.
//
// CreatedAt is parsed from the comment's trailing RFC3339 suffix
// (the "<ts>" in "iplane-<op>-<prov>-<ts>"); if the comment is empty
// or malformed, CreatedAt is the zero value -- callers care about
// the comment for identification, not the timestamp.
func UnmarshalPrivatePEM(operator, provider, comment string, pemBytes []byte) (*KeyPair, error) {
	raw, err := ssh.ParseRawPrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("ssh.ParseRawPrivateKey: %w", err)
	}
	priv, ok := raw.(*ed25519.PrivateKey)
	if !ok {
		// Some Go SSH versions return ed25519.PrivateKey by value
		// instead of by pointer; accept either.
		if direct, okDirect := raw.(ed25519.PrivateKey); okDirect {
			priv = &direct
		} else {
			return nil, fmt.Errorf("parsed key is not Ed25519 (got %T)", raw)
		}
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("Ed25519 private did not yield Ed25519 public (got %T)", priv.Public())
	}
	createdAt := time.Time{}
	if comment != "" {
		// Parse the RFC3339 suffix from "iplane-<op>-<prov>-<ts>".
		// time.RFC3339 has dashes inside the timestamp itself
		// (2026-05-20T...), so split on " " is wrong; the comment
		// uses '-' between parts but the timestamp dominates the
		// tail. Strip the known "iplane-<op>-<prov>-" prefix.
		prefix := fmt.Sprintf("iplane-%s-%s-", operator, provider)
		if strings.HasPrefix(comment, prefix) {
			if t, err := time.Parse(time.RFC3339, strings.TrimPrefix(comment, prefix)); err == nil {
				createdAt = t
			}
		}
	}
	return &KeyPair{
		Operator:  operator,
		Provider:  provider,
		CreatedAt: createdAt,
		Public:    pub,
		Private:   *priv,
		Comment:   comment,
	}, nil
}
