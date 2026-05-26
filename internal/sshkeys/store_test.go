package sshkeys

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/panyam/oneauth/keys"
)

func newInMemStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(
		WithKeyStorage(keys.NewInMemoryKeyStore()),
		WithClock(func() time.Time { return fixedTime() }),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestNew_RequiresBackend(t *testing.T) {
	_, err := New()
	if err == nil {
		t.Fatal("New with no backend should error")
	}
}

func TestEnsureKeyPair_GeneratesFirstCall(t *testing.T) {
	s := newInMemStore(t)
	kp, err := s.EnsureKeyPair("default", "runpod")
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	if kp.Operator != "default" || kp.Provider != "runpod" {
		t.Errorf("scope wrong: %+v", kp)
	}
	if !IsIplaneComment(kp.Comment) {
		t.Errorf("comment %q is not an iplane comment", kp.Comment)
	}
}

func TestEnsureKeyPair_IdempotentSecondCall(t *testing.T) {
	s := newInMemStore(t)
	first, err := s.EnsureKeyPair("default", "runpod")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := s.EnsureKeyPair("default", "runpod")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if string(first.Private) != string(second.Private) {
		t.Error("second call regenerated the private key instead of reusing it")
	}
	if string(first.Public) != string(second.Public) {
		t.Error("second call regenerated the public key instead of reusing it")
	}
	if first.Comment != second.Comment {
		t.Errorf("comment changed across calls: %q vs %q", first.Comment, second.Comment)
	}
}

func TestEnsureKeyPair_PerScopeIsolation(t *testing.T) {
	s := newInMemStore(t)
	a, err := s.EnsureKeyPair("default", "runpod")
	if err != nil {
		t.Fatalf("scope a: %v", err)
	}
	b, err := s.EnsureKeyPair("default", "lambda")
	if err != nil {
		t.Fatalf("scope b: %v", err)
	}
	if string(a.Private) == string(b.Private) {
		t.Error("different providers should get different keys (per-operator-per-provider scoping)")
	}
	c, err := s.EnsureKeyPair("other-op", "runpod")
	if err != nil {
		t.Fatalf("scope c: %v", err)
	}
	if string(a.Private) == string(c.Private) {
		t.Error("different operators should get different keys")
	}
}

func TestWithDir_PersistsToFilesystem(t *testing.T) {
	dir := t.TempDir()
	s, err := New(WithDir(dir), WithClock(func() time.Time { return fixedTime() }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	kp, err := s.EnsureKeyPair("default", "runpod")
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}

	// Verify the on-disk file exists (FSKeyStore lays out as
	// <dir>/signing_keys/<safeClientID>.json) and that subsequent
	// reads from a fresh Store on the same dir return the same key.
	entries, _ := os.ReadDir(filepath.Join(dir, "signing_keys"))
	if len(entries) != 1 {
		t.Errorf("signing_keys/ contains %d files, want 1", len(entries))
	}

	s2, err := New(WithDir(dir))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	kp2, err := s2.EnsureKeyPair("default", "runpod")
	if err != nil {
		t.Fatalf("EnsureKeyPair after reopen: %v", err)
	}
	if string(kp.Private) != string(kp2.Private) {
		t.Error("reopened store generated a fresh key instead of loading the existing one")
	}
}
