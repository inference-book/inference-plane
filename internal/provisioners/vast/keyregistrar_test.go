package vast

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// A real ed25519 authorized_keys line, and a different one, so the
// membership test is exercised against genuine wire bytes rather than
// strings that happen to differ.
const (
	keyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH1Zp0oS0gT9c1hnQzTz0K0dGyC7hFqSuLJ1cFO2rSjA iplane-default-vast-2026-08-10T00:00:00Z"
	keyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ9pQKDbkjqYsFhVv6uUqLTLPCLTiOaVaJGvHvBBnBnG someone-else@laptop"
)

// keyServer serves the account key list and records uploads.
type keyServer struct {
	listed  []sshKeyRecord
	uploads []string
	status  int
	errCode string
}

func (k *keyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != pathSSHKeys {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(k.listed)
	case http.MethodPost:
		var body struct {
			SSHKey string `json:"ssh_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		k.uploads = append(k.uploads, body.SSHKey)
		if k.status != 0 {
			w.WriteHeader(k.status)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"success":false,"error":%q,"msg":"nope"}`, k.errCode)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"id":7}`))
	}
}

func TestEnsurePublicKeyUploadsWhenAbsent(t *testing.T) {
	ks := &keyServer{}
	p, _ := newTestProvider(t, ks)

	if err := p.EnsurePublicKey(t.Context(), []byte(keyA), "iplane-default-vast-2026-08-10T00:00:00Z"); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}

	if len(ks.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(ks.uploads))
	}
	if !strings.Contains(ks.uploads[0], "AAAAIH1Zp0oS0gT9c1hnQzTz0K0dGyC7hFqSuLJ1cFO2rSjA") {
		t.Errorf("uploaded the wrong key material: %q", ks.uploads[0])
	}
	// The comment rides inside the key line so the account listing stays
	// attributable to iplane rather than being an anonymous blob.
	if !strings.Contains(ks.uploads[0], "iplane-default-vast-") {
		t.Errorf("uploaded key carries no iplane comment: %q", ks.uploads[0])
	}
}

// This runs on every instance create, so a non-idempotent version would
// litter the account with a duplicate key per deploy.
func TestEnsurePublicKeyIsIdempotent(t *testing.T) {
	ks := &keyServer{listed: []sshKeyRecord{{ID: 1, PublicKey: keyA}}}
	p, _ := newTestProvider(t, ks)

	if err := p.EnsurePublicKey(t.Context(), []byte(keyA), "iplane-default-vast-2026-08-10T00:00:00Z"); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}

	if len(ks.uploads) != 0 {
		t.Errorf("uploaded a key that was already registered: %v", ks.uploads)
	}
}

// Match on parsed wire bytes, not on the whole line, so a differing comment
// or trailing whitespace does not cause a duplicate upload of a key that is
// already on the account.
func TestEnsurePublicKeyMatchesOnKeyMaterialNotTheWholeLine(t *testing.T) {
	stored := strings.SplitN(keyA, " ", 3)
	ks := &keyServer{listed: []sshKeyRecord{
		{ID: 1, PublicKey: stored[0] + " " + stored[1] + " a-totally-different-comment\n"},
	}}
	p, _ := newTestProvider(t, ks)

	if err := p.EnsurePublicKey(t.Context(), []byte(keyA), "iplane-default-vast-2026-08-10T00:00:00Z"); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}

	if len(ks.uploads) != 0 {
		t.Errorf("re-uploaded a key whose material was already present: %v", ks.uploads)
	}
}

func TestEnsurePublicKeyUploadsWhenOnlyOtherKeysExist(t *testing.T) {
	ks := &keyServer{listed: []sshKeyRecord{{ID: 1, PublicKey: keyB}}}
	p, _ := newTestProvider(t, ks)

	if err := p.EnsurePublicKey(t.Context(), []byte(keyA), "iplane-default-vast-2026-08-10T00:00:00Z"); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}

	if len(ks.uploads) != 1 {
		t.Errorf("uploads = %d, want 1; someone else's key must not satisfy ours", len(ks.uploads))
	}
}

// A key on the account we cannot parse belongs to someone else and is not a
// reason to fail the deploy.
func TestUnparseableAccountKeyIsSkipped(t *testing.T) {
	ks := &keyServer{listed: []sshKeyRecord{
		{ID: 1, PublicKey: "this is not a key"},
		{ID: 2, PublicKey: keyA},
	}}
	p, _ := newTestProvider(t, ks)

	if err := p.EnsurePublicKey(t.Context(), []byte(keyA), "c"); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(ks.uploads) != 0 {
		t.Errorf("a garbage key on the account defeated the membership check: %v", ks.uploads)
	}
}

func TestEnsurePublicKeyRejectsMalformedInput(t *testing.T) {
	p, _ := newTestProvider(t, &keyServer{})

	if err := p.EnsurePublicKey(t.Context(), []byte("not a public key"), "c"); err == nil {
		t.Error("want an error for an unparseable public key")
	}
}

func TestEnsurePublicKeySurfacesUploadFailure(t *testing.T) {
	ks := &keyServer{status: http.StatusForbidden}
	p, _ := newTestProvider(t, ks)

	err := p.EnsurePublicKey(t.Context(), []byte(keyA), "c")
	if err == nil {
		t.Fatal("want an error when the upload is refused")
	}
	if !strings.Contains(err.Error(), "register ssh key") {
		t.Errorf("error = %v, want it to name the failing step", err)
	}
}

// The Service only wires key registration when the adapter satisfies the
// capability interface, so a signature drift would silently disable it and
// every deploy would go back to failing at the SSH dial with nothing
// pointing at the cause.
var _ provisioners.KeyRegistrar = (*Provider)(nil)

// Vast's API is asymmetric: a POST takes "ssh_key" and a GET returns
// "public_key". Reading only the write-side field yields a list of empty
// strings, which is indistinguishable from an account with no keys, so the
// membership test never matches and every create re-uploads.
func TestListReadsThePublicKeyField(t *testing.T) {
	ks := &keyServer{listed: []sshKeyRecord{{ID: 1, PublicKey: keyA}}}
	p, _ := newTestProvider(t, ks)

	if err := p.EnsurePublicKey(t.Context(), []byte(keyA), "c"); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(ks.uploads) != 0 {
		t.Error("did not recognise a key returned in public_key; idempotency is broken")
	}
}

// Some deployments may still return the write-side spelling; accept either
// rather than depending on which one a given Vast version emits.
func TestListAlsoAcceptsTheSshKeyField(t *testing.T) {
	ks := &keyServer{listed: []sshKeyRecord{{ID: 1, SSHKey: keyA}}}
	p, _ := newTestProvider(t, ks)

	if err := p.EnsurePublicKey(t.Context(), []byte(keyA), "c"); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(ks.uploads) != 0 {
		t.Error("did not recognise a key returned in ssh_key")
	}
}

// "Already exists" is the outcome we wanted. Failing the create over it
// turns a satisfied precondition into a refused instance, which is how the
// first version of this broke every repeat create.
func TestDuplicateUploadIsNotAnError(t *testing.T) {
	ks := &keyServer{status: http.StatusBadRequest, errCode: "duplicate"}
	p, _ := newTestProvider(t, ks)

	if err := p.EnsurePublicKey(t.Context(), []byte(keyA), "c"); err != nil {
		t.Errorf("duplicate key upload returned an error: %v", err)
	}
}

// A genuine refusal still has to surface.
func TestNonDuplicateUploadFailureStillErrors(t *testing.T) {
	ks := &keyServer{status: http.StatusForbidden, errCode: "forbidden"}
	p, _ := newTestProvider(t, ks)

	if err := p.EnsurePublicKey(t.Context(), []byte(keyA), "c"); err == nil {
		t.Error("want an error for a non-duplicate refusal")
	}
}
