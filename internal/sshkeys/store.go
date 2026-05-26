package sshkeys

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/panyam/oneauth/keys"
	fsstore "github.com/panyam/oneauth/stores/fs"
)

// Store persists iplane-managed SSH key pairs through any
// oneauth keys.KeyStorage backend (FS, GORM, GAE, InMemory). The
// scoping convention (locked by docs/design/0002-deploy.md):
//
//	ClientID = "iplane:ssh:<operator>:<provider>"
//
// per-operator-per-provider, so rotation/revocation is scoped to one
// provider and a leaked key has narrow blast radius. EnsureKeyPair
// looks up by this ClientID; if missing it generates a new pair,
// stores the PEM-encoded private under the same ClientID, and
// returns the parsed KeyPair.
type Store struct {
	backend keys.KeyStorage
	clock   func() time.Time
}

// Option configures a Store at construction time.
type Option func(*Store)

// WithKeyStorage injects any oneauth keys.KeyStorage. Use this when
// the caller already has a GORM-backed store, an InMemoryKeyStore
// (tests), or a custom backend.
func WithKeyStorage(backend keys.KeyStorage) Option {
	return func(s *Store) { s.backend = backend }
}

// WithDir is sugar that constructs an FSKeyStore rooted at dir.
// Convenient for the common "single-binary CLI with files under
// ~/.iplane/keys" case so callers do not need to import oneauth's
// stores/fs package directly.
func WithDir(dir string) Option {
	return func(s *Store) {
		if dir == "" {
			return
		}
		s.backend = fsstore.NewFSKeyStore(filepath.Clean(dir))
	}
}

// WithClock injects a clock for tests so generated keys carry a
// known timestamp in their comment string.
func WithClock(c func() time.Time) Option {
	return func(s *Store) { s.clock = c }
}

// New constructs a Store. A backend is required -- pass either
// WithKeyStorage(...) or WithDir(...). The order of options is fine
// either way (later wins); callers who pass both are violating the
// API and get the last one.
func New(opts ...Option) (*Store, error) {
	s := &Store{clock: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	if s.backend == nil {
		return nil, errors.New("sshkeys.New: backend required (use WithKeyStorage or WithDir)")
	}
	return s, nil
}

// clientID is the canonical KeyStorage scoping string. Exposed
// (lowercase) for tests that want to inspect the on-disk layout.
func clientID(operator, provider string) string {
	return fmt.Sprintf("iplane:ssh:%s:%s", operator, provider)
}

// EnsureKeyPair returns the key for (operator, provider) -- either
// loading the existing one from disk or generating + storing a new
// one. Idempotent: subsequent calls with the same scope return the
// same key (same private bytes, same comment string).
func (s *Store) EnsureKeyPair(operator, provider string) (*KeyPair, error) {
	cid := clientID(operator, provider)

	if rec, err := s.backend.GetKey(cid); err == nil {
		pemBytes, ok := rec.Key.([]byte)
		if !ok {
			return nil, fmt.Errorf("sshkeys: stored key for %s is not []byte (got %T)", cid, rec.Key)
		}
		// The comment is cached in KeyRecord.Kid because
		// ssh.MarshalPrivateKey bakes its comment into the OpenSSH
		// binary blob, not the PEM headers -- we cannot recover it
		// from pemBytes alone without parsing the OpenSSH format.
		return UnmarshalPrivatePEM(operator, provider, rec.Kid, pemBytes)
	} else if !errors.Is(err, keys.ErrKeyNotFound) {
		return nil, fmt.Errorf("sshkeys: load %s: %w", cid, err)
	}

	// Not found -- generate + persist.
	kp, err := Generate(operator, provider, s.clock())
	if err != nil {
		return nil, err
	}
	pemBytes, err := kp.MarshalPrivatePEM()
	if err != nil {
		return nil, fmt.Errorf("sshkeys: marshal new key for %s: %w", cid, err)
	}
	if err := s.backend.PutKey(&keys.KeyRecord{
		ClientID:  cid,
		Key:       pemBytes,
		Algorithm: Algorithm,
		Kid:       kp.Comment, // cache the comment for round-trip recovery
	}); err != nil {
		return nil, fmt.Errorf("sshkeys: persist %s: %w", cid, err)
	}
	return kp, nil
}
