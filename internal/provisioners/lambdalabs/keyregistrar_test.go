package lambdalabs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// A real ed25519 public key, so the registrar's structural comparison has
// something parseable to work with. The bytes are arbitrary.
const testPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICN+lJwsONkwrdsSnQsu1ydUkIuIg5oOC+Eslvmtt60T iplane-default-lambdalabs-2026-08-24T10:00:00Z"

const testComment = "iplane-default-lambdalabs-2026-08-24T10:00:00Z"

// iplaneKeyName is what sshKeyName derives from testComment and testPubKey:
// the comment with its RFC3339 tail dropped, plus a digest of the key.
var iplaneKeyName = sshKeyName(testComment, []byte(testPubKey))

// sshKeyMux serves GET /ssh-keys from the supplied slice, records what
// POST /ssh-keys was asked to create, and records every DELETE.
func sshKeyMux(t *testing.T, existing []apiSSHKey, posted *map[string]string, deleted *[]string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, sshKeysResponse{Data: existing})
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("POST /ssh-keys body did not parse: %v", err)
			}
			*posted = req
			writeJSON(w, map[string]any{"data": apiSSHKey{
				ID: "new", Name: req["name"], PublicKey: req["public_key"],
			}})
		default:
			t.Errorf("unexpected method %s on /ssh-keys", r.Method)
		}
	})
	mux.HandleFunc("/api/v1/ssh-keys/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected method %s on /ssh-keys/{id}", r.Method)
		}
		*deleted = append(*deleted, strings.TrimPrefix(r.URL.Path, "/api/v1/ssh-keys/"))
		writeJSON(w, map[string]any{"data": map[string]any{}})
	})
	return mux
}

func testSpec(id string) *provisionerv1.Spec {
	return &provisionerv1.Spec{
		Id:           id,
		Requirements: &provisionerv1.ResourceRequirements{Sku: "gpu_1x_a10"},
	}
}

// Without this the adapter never puts iplane's key on the account, Spawn
// attaches whatever key happened to be there first, and the deploy path
// then SSHes in holding a private key the VM has never heard of.
func TestEnsurePublicKey_RegistersWhenAbsent(t *testing.T) {
	var posted map[string]string
	var deleted []string
	p, _ := newTestProvider(t, sshKeyMux(t, []apiSSHKey{
		{ID: "k1", Name: "GMac", PublicKey: "ssh-rsa AAAAsomeoneelse"},
	}, &posted, &deleted))

	if err := p.EnsurePublicKey(context.Background(), []byte(testPubKey), testComment); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if posted == nil {
		t.Fatal("no key was POSTed to Lambda")
	}
	if posted["name"] != iplaneKeyName {
		t.Errorf("registered name = %q, want %q", posted["name"], iplaneKeyName)
	}
	if !strings.Contains(posted["public_key"], "AAAAC3NzaC1lZDI1NTE5") {
		t.Errorf("registered public_key = %q, want our key", posted["public_key"])
	}
	if len(deleted) != 0 {
		t.Errorf("deleted %v; registering must not touch the operator's own keys", deleted)
	}
}

// Called before every rent, so re-registering an unchanged key has to cost
// nothing.
func TestEnsurePublicKey_IsIdempotent(t *testing.T) {
	var posted map[string]string
	var deleted []string
	p, _ := newTestProvider(t, sshKeyMux(t, []apiSSHKey{
		{ID: "k1", Name: "GMac", PublicKey: "ssh-rsa AAAAsomeoneelse"},
		{ID: "k2", Name: iplaneKeyName, PublicKey: testPubKey},
	}, &posted, &deleted))

	if err := p.EnsurePublicKey(context.Background(), []byte(testPubKey), testComment); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if posted != nil {
		t.Errorf("POSTed %v; the key was already on file", posted)
	}
	if len(deleted) != 0 {
		t.Errorf("deleted %v; nothing needed replacing", deleted)
	}
}

// A whitespace or comment difference is not a different key, so the
// comparison is on the key material rather than on the line.
func TestEnsurePublicKey_MatchesOnKeyMaterialNotOnText(t *testing.T) {
	var posted map[string]string
	var deleted []string
	sameKeyOtherComment := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICN+lJwsONkwrdsSnQsu1ydUkIuIg5oOC+Eslvmtt60T somebody-elses-label"
	p, _ := newTestProvider(t, sshKeyMux(t, []apiSSHKey{
		{ID: "k2", Name: "whatever", PublicKey: sameKeyOtherComment},
	}, &posted, &deleted))

	if err := p.EnsurePublicKey(context.Background(), []byte(testPubKey), testComment); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if posted != nil {
		t.Errorf("POSTed %v; the same key material was already on file under another label", posted)
	}
}

// Lambda caps a key name at 64 characters, and the name has to survive two
// things at once: re-registering the same key must reuse a name, and two
// different keys must never share one.
func TestSSHKeyName(t *testing.T) {
	other := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherOtherOtherOtherOtherOtherOther other"

	got := sshKeyName(testComment, []byte(testPubKey))
	if !strings.HasPrefix(got, "iplane-default-lambdalabs-") {
		t.Errorf("sshKeyName = %q, want the readable part kept", got)
	}
	if got != sshKeyName(testComment, []byte(testPubKey)) {
		t.Error("the same key produced two names; re-registration would duplicate it")
	}
	// The timestamp still goes, so re-registering one key after a clock tick
	// reuses its name rather than adding a second copy.
	later := "iplane-default-lambdalabs-2027-01-01T00:00:00Z"
	if sshKeyName(later, []byte(testPubKey)) != got {
		t.Error("the comment's timestamp reached the name; one key would accumulate entries")
	}
	// Two keys under one operator must not collide, which is the whole of
	// #442: resolving a collision meant deleting, and Lambda refuses to
	// delete a key a running instance is using.
	if sshKeyName(testComment, []byte(other)) == got {
		t.Error("two different keys derived one name")
	}

	for _, tt := range []struct{ comment, wantPrefix string }{
		{"iplane-ops-lambdalabs-2026-08-24T10:00:00+05:30", "iplane-ops-lambdalabs-"},
		{"", "iplane-key-"},
		{"no timestamp here", "no-timestamp-here-"},
		{"iplane-" + strings.Repeat("x", 100), "iplane-"},
	} {
		got := sshKeyName(tt.comment, []byte(testPubKey))
		if !strings.HasPrefix(got, tt.wantPrefix) {
			t.Errorf("sshKeyName(%q) = %q, want prefix %q", tt.comment, got, tt.wantPrefix)
		}
		if len(got) == 0 || len(got) > 64 {
			t.Errorf("sshKeyName(%q) = %q, length %d outside Lambda's 1..64", tt.comment, got, len(got))
		}
	}

	// A long operator id is truncated to make room for the digest rather than
	// the digest being shortened, so two long ids still differ.
	longA := sshKeyName("iplane-"+strings.Repeat("x", 100)+"a", []byte(testPubKey))
	longB := sshKeyName("iplane-"+strings.Repeat("x", 100)+"b", []byte(other))
	if longA == longB {
		t.Error("two truncated names with different keys collided")
	}
}

// The launch call has to name iplane's key. Attaching the account's first
// key instead is what made the deploy path unreachable: the VM boots with
// somebody else's public key while sshdocker holds iplane's private one.
func TestSpawn_AttachesTheKeyIplaneRegistered(t *testing.T) {
	var posted map[string]string
	var deleted []string
	var launched map[string]any
	mux := sshKeyMux(t, []apiSSHKey{
		{ID: "k1", Name: "GMac", PublicKey: "ssh-rsa AAAAsomeoneelse"},
		{ID: "k2", Name: iplaneKeyName, PublicKey: testPubKey},
	}, &posted, &deleted)
	mux.HandleFunc("/api/v1/instance-operations/launch", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &launched)
		writeJSON(w, launchResponse{Data: struct {
			InstanceIDs []string `json:"instance_ids"`
		}{InstanceIDs: []string{"inst-1"}}})
	})
	mux.HandleFunc("/api/v1/instances/inst-1", func(w http.ResponseWriter, _ *http.Request) {
		body := apiInstance{ID: "inst-1", Name: "iplane-my-pod", Status: "booting"}
		body.InstanceType.Name = "gpu_1x_a10"
		writeJSON(w, instanceResponse{Data: body})
	})
	p, _ := newTestProvider(t, mux)

	if _, err := p.Spawn(context.Background(), testSpec("my-pod")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	names, _ := launched["ssh_key_names"].([]any)
	if len(names) != 1 || names[0] != iplaneKeyName {
		t.Errorf("ssh_key_names = %v, want iplane's own key", names)
	}
}

// An operator driving the adapter by hand has never run EnsurePublicKey, so
// the account's own key stays the fallback rather than becoming an error.
func TestSpawn_FallsBackToTheAccountsOwnKey(t *testing.T) {
	var posted map[string]string
	var deleted []string
	var launched map[string]any
	mux := sshKeyMux(t, []apiSSHKey{
		{ID: "k1", Name: "GMac", PublicKey: "ssh-rsa AAAAsomeoneelse"},
	}, &posted, &deleted)
	mux.HandleFunc("/api/v1/instance-operations/launch", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &launched)
		writeJSON(w, launchResponse{Data: struct {
			InstanceIDs []string `json:"instance_ids"`
		}{InstanceIDs: []string{"inst-1"}}})
	})
	mux.HandleFunc("/api/v1/instances/inst-1", func(w http.ResponseWriter, _ *http.Request) {
		body := apiInstance{ID: "inst-1", Name: "iplane-my-pod", Status: "booting"}
		body.InstanceType.Name = "gpu_1x_a10"
		writeJSON(w, instanceResponse{Data: body})
	})
	p, _ := newTestProvider(t, mux)

	if _, err := p.Spawn(context.Background(), testSpec("my-pod")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	names, _ := launched["ssh_key_names"].([]any)
	if len(names) != 1 || names[0] != "GMac" {
		t.Errorf("ssh_key_names = %v, want the account's own key", names)
	}
}

// The defect this replaced. Registering a second keypair used to delete the
// key stored under the shared derived name, and Lambda refuses that while any
// running instance references it:
//
//	ensure-key:delete-stale failed (http 400): Key is currently in use, cannot delete
//
// Registration failed, so Spawn never ran and nothing could be rented at all
// for as long as the other machine was up. Hit live on the second #427
// rental, with two state directories against one account (#442).
func TestEnsurePublicKey_DoesNotDeleteAnythingWhenRegisteringASecondKey(t *testing.T) {
	var posted map[string]string
	var deleted []string
	otherKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherOtherOtherOtherOtherOtherOther somebody-else"
	p, _ := newTestProvider(t, sshKeyMux(t, []apiSSHKey{
		{ID: "k-inuse", Name: sshKeyName(testComment, []byte(otherKey)), PublicKey: otherKey},
	}, &posted, &deleted))

	if err := p.EnsurePublicKey(t.Context(), []byte(testPubKey), testComment); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(deleted) != 0 {
		t.Errorf("deleted %v; Lambda refuses to delete a key in use and the rent fails with it", deleted)
	}
	if posted == nil {
		t.Fatal("the second key was never registered")
	}
	if posted["name"] == sshKeyName(testComment, []byte(otherKey)) {
		t.Error("the second key reused the first key's name")
	}
}

// With several iplane keys on one account, a prefix scan can no longer tell
// which private half the caller holds. Spawn attaches the one this Provider
// registered.
func TestSpawn_AttachesTheKeyThisProviderRegistered(t *testing.T) {
	var posted map[string]string
	var deleted []string
	otherKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherOtherOtherOtherOtherOtherOther somebody-else"
	oursName := sshKeyName(testComment, []byte(testPubKey))
	theirsName := sshKeyName(testComment, []byte(otherKey))

	var launched map[string]any
	mux := sshKeyMux(t, []apiSSHKey{
		// Listed first, so a prefix scan would pick the wrong one.
		{ID: "k-theirs", Name: theirsName, PublicKey: otherKey},
		{ID: "k-ours", Name: oursName, PublicKey: testPubKey},
	}, &posted, &deleted)
	mux.HandleFunc("/api/v1/instance-operations/launch", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &launched)
		writeJSON(w, launchResponse{Data: struct {
			InstanceIDs []string `json:"instance_ids"`
		}{InstanceIDs: []string{"inst-1"}}})
	})
	mux.HandleFunc("/api/v1/instances/inst-1", func(w http.ResponseWriter, _ *http.Request) {
		body := apiInstance{ID: "inst-1", Name: "iplane-my-pod", Status: "booting"}
		body.InstanceType.Name = "gpu_1x_a10"
		writeJSON(w, instanceResponse{Data: body})
	})
	p, _ := newTestProvider(t, mux)

	if err := p.EnsurePublicKey(t.Context(), []byte(testPubKey), testComment); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if _, err := p.Spawn(t.Context(), testSpec("my-pod")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	names, _ := launched["ssh_key_names"].([]any)
	if len(names) != 1 || names[0] != oursName {
		t.Errorf("ssh_key_names = %v, want %q; the VM would boot with a key we cannot present", names, oursName)
	}
}
