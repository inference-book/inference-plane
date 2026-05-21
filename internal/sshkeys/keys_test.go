package sshkeys

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func fixedTime() time.Time {
	return time.Date(2026, 5, 20, 15, 30, 0, 0, time.UTC)
}

func TestGenerate_ProducesValidEd25519(t *testing.T) {
	kp, err := Generate("default", "runpod", fixedTime())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(kp.Public) != ed25519.PublicKeySize {
		t.Errorf("public key length = %d, want %d", len(kp.Public), ed25519.PublicKeySize)
	}
	if len(kp.Private) != ed25519.PrivateKeySize {
		t.Errorf("private key length = %d, want %d", len(kp.Private), ed25519.PrivateKeySize)
	}
	if kp.Operator != "default" || kp.Provider != "runpod" {
		t.Errorf("scope wrong: operator=%q provider=%q", kp.Operator, kp.Provider)
	}
	// Sign/verify round-trip proves the pair is internally consistent.
	msg := []byte("hello")
	sig := ed25519.Sign(kp.Private, msg)
	if !ed25519.Verify(kp.Public, msg, sig) {
		t.Error("public key does not verify a signature made with the private")
	}
}

func TestFormatComment_StableShape(t *testing.T) {
	c := FormatComment("default", "runpod", fixedTime())
	want := "iplane-default-runpod-2026-05-20T15:30:00Z"
	if c != want {
		t.Errorf("comment = %q, want %q", c, want)
	}
	if !IsIplaneComment(c) {
		t.Error("IsIplaneComment did not recognize an iplane comment")
	}
	if IsIplaneComment("user@host") {
		t.Error("IsIplaneComment matched a non-iplane comment")
	}
}

func TestMarshalAuthorizedKey_OpenSSHFormat(t *testing.T) {
	kp, err := Generate("default", "runpod", fixedTime())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	line, err := kp.MarshalAuthorizedKey()
	if err != nil {
		t.Fatalf("MarshalAuthorizedKey: %v", err)
	}
	// Must parse back through ssh.ParseAuthorizedKey -- the spec
	// check that matters for interop with RunPod's authorized_keys
	// blob handling.
	parsed, parsedComment, _, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil {
		t.Fatalf("ParseAuthorizedKey(round-trip): %v\n%s", err, line)
	}
	if parsed.Type() != "ssh-ed25519" {
		t.Errorf("type = %q, want ssh-ed25519", parsed.Type())
	}
	if parsedComment != kp.Comment {
		t.Errorf("round-tripped comment = %q, want %q", parsedComment, kp.Comment)
	}
	if !strings.HasSuffix(string(line), "\n") {
		t.Error("authorized_keys line should end with newline")
	}
}

func TestMarshalUnmarshalPrivatePEM_RoundTrip(t *testing.T) {
	kp, err := Generate("default", "runpod", fixedTime())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	pemBytes, err := kp.MarshalPrivatePEM()
	if err != nil {
		t.Fatalf("MarshalPrivatePEM: %v", err)
	}
	// Must be parseable by the standard library + go-ssh.
	if !strings.Contains(string(pemBytes), "OPENSSH PRIVATE KEY") {
		t.Errorf("PEM block type should be OPENSSH PRIVATE KEY; got:\n%s", pemBytes)
	}
	got, err := UnmarshalPrivatePEM("default", "runpod", pemBytes)
	if err != nil {
		t.Fatalf("UnmarshalPrivatePEM: %v", err)
	}
	if string(got.Private) != string(kp.Private) {
		t.Error("round-tripped private key bytes differ")
	}
	if string(got.Public) != string(kp.Public) {
		t.Error("round-tripped public key bytes differ")
	}
}
