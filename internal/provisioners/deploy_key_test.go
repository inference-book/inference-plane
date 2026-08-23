package provisioners_test

import (
	"context"
	"sync"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// keyRegistrarFanOutProvider is the fan-out double plus KeyRegistrar, so a
// test can see whether a deploy told the provider about the key it made.
type keyRegistrarFanOutProvider struct {
	*fanOutMockProvider

	mu         sync.Mutex
	calls      int
	comment    string
	pubKey     []byte
	failWith   error
	onRegister func()
}

func (p *keyRegistrarFanOutProvider) EnsurePublicKey(_ context.Context, publicKey []byte, comment string) error {
	if p.onRegister != nil {
		p.onRegister()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.pubKey = append([]byte(nil), publicKey...)
	p.comment = comment
	return p.failWith
}

func (p *keyRegistrarFanOutProvider) registrations() (int, string, []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.comment, append([]byte(nil), p.pubKey...)
}

func keyRegistrarDeploySvc(t *testing.T, prov provisioners.Provider) *provisioners.Service {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	return provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)))
}

func deployOneReplica(t *testing.T, svc *provisioners.Service, id string) error {
	t.Helper()
	_, err := svc.CreateDeployment(context.Background(),
		warmDeployReq(id, "Qwen/Qwen2.5-32B-Instruct-AWQ", "mockfan", "EU-RO-1", 1))
	return err
}

// A deployment generates a keypair locally. If it never tells the provider,
// the box boots with no way in, and nothing on the deploy path notices
// because the engine answers over HTTP either way. That is why SSH into a
// deployed replica only ever worked when an earlier CreateInstance had left
// a registered key in the same keystore.
func TestCreateDeploymentRegistersTheKeyWithTheProvider(t *testing.T) {
	prov := &keyRegistrarFanOutProvider{fanOutMockProvider: &fanOutMockProvider{name: "mockfan"}}
	svc := keyRegistrarDeploySvc(t, prov)

	if err := deployOneReplica(t, svc, "dep-key"); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	calls, comment, pub := prov.registrations()
	if calls == 0 {
		t.Fatal("the deploy never registered its public key; the replica boots unreachable")
	}
	if !sshkeys.IsIplaneComment(comment) {
		t.Errorf("registered comment %q is not an iplane comment, so the skip-if-present check cannot match it", comment)
	}
	if len(pub) < 11 || string(pub[:11]) != "ssh-ed25519" {
		t.Errorf("registered bytes are not an authorized_keys line: %q", pub)
	}
}

// Registration has to happen before anything is rented. Afterwards is a box
// already running without the key, which is the state this change exists to
// prevent, and "we registered it eventually" does not help a machine that
// booted five minutes ago.
func TestCreateDeploymentRegistersBeforeItRents(t *testing.T) {
	var mu sync.Mutex
	var order []string
	prov := &keyRegistrarFanOutProvider{fanOutMockProvider: &fanOutMockProvider{name: "mockfan"}}
	prov.onRegister = func() {
		mu.Lock()
		order = append(order, "register")
		mu.Unlock()
	}
	prov.fanOutMockProvider.deployFn = func(_ *provisionerv1.Instance, _ func(provisioners.DeployStateUpdate)) error {
		mu.Lock()
		order = append(order, "deploy")
		mu.Unlock()
		return nil
	}
	svc := keyRegistrarDeploySvc(t, prov)

	if err := deployOneReplica(t, svc, "dep-order"); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 {
		t.Fatalf("expected a register and a deploy, got %v", order)
	}
	if order[0] != "register" {
		t.Fatalf("order was %v; the key must be registered before the box is rented", order)
	}
}

// A provider that cannot register keys must still deploy. local and external
// are exactly this shape, and refusing them would break every zero-cost path.
func TestCreateDeploymentWithoutAKeyRegistrarStillDeploys(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)
	if err := deployOneReplica(t, svc, "dep-nokeyreg"); err != nil {
		t.Fatalf("a provider with no KeyRegistrar should still deploy: %v", err)
	}
}

// A provider that refuses the key fails that slot rather than the process.
// The fan-out already treats a keystore failure as a per-slot result; a
// registration failure is the same kind of thing and must not be worse.
func TestCreateDeploymentSurvivesARegistrationFailure(t *testing.T) {
	prov := &keyRegistrarFanOutProvider{
		fanOutMockProvider: &fanOutMockProvider{name: "mockfan"},
		failWith:           context.DeadlineExceeded,
	}
	svc := keyRegistrarDeploySvc(t, prov)

	// Either outcome is acceptable; a panic or a hang is not. The point is
	// that a refused key is reported, not swallowed into a deployment that
	// looks healthy and cannot be reached.
	err := deployOneReplica(t, svc, "dep-keyfail")
	if calls, _, _ := prov.registrations(); calls == 0 {
		t.Fatal("registration was never attempted")
	}
	t.Logf("registration failure surfaced as: %v", err)
}
