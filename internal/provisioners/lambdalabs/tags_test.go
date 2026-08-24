package lambdalabs

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/providertest"
)

// launchTagMux serves the calls Spawn makes and captures the launch body.
func launchTagMux(t *testing.T, launched *map[string]any) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ssh-keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, sshKeysResponse{Data: []apiSSHKey{{ID: "k1", Name: "GMac", PublicKey: "ssh-rsa AAAA"}}})
	})
	mux.HandleFunc("/api/v1/instance-operations/launch", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, launched); err != nil {
			t.Errorf("launch body did not parse: %v", err)
		}
		writeJSON(w, launchResponse{Data: struct {
			InstanceIDs []string `json:"instance_ids"`
		}{InstanceIDs: []string{"inst-1"}}})
	})
	mux.HandleFunc("/api/v1/instances/inst-1", func(w http.ResponseWriter, _ *http.Request) {
		body := apiInstance{ID: "inst-1", Name: "iplane-my-pod", Status: "booting"}
		body.InstanceType.Name = "gpu_1x_a10"
		writeJSON(w, instanceResponse{Data: body})
	})
	return mux
}

// The Service stamps iplane-id and iplane-operator onto every Spec before
// Spawn, and this adapter used to drop them and write a display name
// instead. Lambda's launch call takes tags directly, so there was never a
// second call to justify it.
func TestSpawn_StampsOwnershipTags(t *testing.T) {
	var launched map[string]any
	p, _ := newTestProvider(t, launchTagMux(t, &launched))

	if _, err := p.Spawn(t.Context(), &provisionerv1.Spec{
		Id:           "my-pod",
		Requirements: &provisionerv1.ResourceRequirements{Sku: "gpu_1x_a10"},
		Tags: map[string]string{
			provisioners.TagID:       "my-pod",
			provisioners.TagOperator: "default",
		},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	got := map[string]string{}
	for _, entry := range launched["tags"].([]any) {
		e := entry.(map[string]any)
		got[e["key"].(string)] = e["value"].(string)
	}
	if got[provisioners.TagID] != "my-pod" {
		t.Errorf("launch tags = %v, want %s=my-pod", got, provisioners.TagID)
	}
	if got[provisioners.TagOperator] != "default" {
		t.Errorf("launch tags = %v, want %s=default", got, provisioners.TagOperator)
	}
}

// Lambda bounds a tag key to `^[a-z][a-z0-9-:]+$`, and a key outside it
// fails the whole launch with a 400. Refusing to rent over a cosmetic tag is
// the wrong trade, so an unusable key is dropped and the rent proceeds.
func TestSpawn_DropsATagKeyLambdaWouldReject(t *testing.T) {
	var launched map[string]any
	p, _ := newTestProvider(t, launchTagMux(t, &launched))

	if _, err := p.Spawn(t.Context(), &provisionerv1.Spec{
		Id:           "my-pod",
		Requirements: &provisionerv1.ResourceRequirements{Sku: "gpu_1x_a10"},
		Tags: map[string]string{
			provisioners.TagID: "my-pod",
			"Team_Name":        "platform",
		},
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	for _, entry := range launched["tags"].([]any) {
		if key := entry.(map[string]any)["key"].(string); key == "Team_Name" {
			t.Error("sent a tag key Lambda's pattern rejects; the launch would have 400ed")
		}
	}
}

// The name is a display field an operator can change from the console.
// Ownership has to survive that, which is the whole reason for reading tags.
func TestList_RecoversOwnershipFromTagsWhenTheNameWasChanged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, instanceListResponse{Data: []apiInstance{{
			ID: "1", Name: "renamed-by-hand", Status: "active",
			Tags: []apiTag{
				{Key: provisioners.TagID, Value: "my-pod"},
				{Key: provisioners.TagOperator, Value: "default"},
			},
		}}})
	})
	p, _ := newTestProvider(t, mux)

	refs, err := p.List(t.Context(), map[string]string{provisioners.TagID: "my-pod"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d, want 1; the rename hid an instance iplane owns", len(refs))
	}
	if got := refs[0].GetTags()[provisioners.TagOperator]; got != "default" {
		t.Errorf("recovered %s=%q, want default", provisioners.TagOperator, got)
	}
}

// With tags stamped, Lambda recovers the operator too, so the shared suite
// holds it to the contract over both keys rather than over the one the name
// could carry.
func TestListFilterConformanceWithTags(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, instanceListResponse{Data: []apiInstance{
			{ID: "1", Name: "iplane-foo", Status: "active", Tags: []apiTag{
				{Key: provisioners.TagID, Value: "foo"},
				{Key: provisioners.TagOperator, Value: "alice"},
			}},
			{ID: "2", Name: "iplane-bar", Status: "booting", Tags: []apiTag{
				{Key: provisioners.TagID, Value: "bar"},
				{Key: provisioners.TagOperator, Value: "bob"},
			}},
		}})
	})
	p, _ := newTestProvider(t, mux)

	providertest.RunListFilterConformance(t, p, []providertest.SeededInstance{
		{ProviderID: "1", Tags: map[string]string{
			provisioners.TagID: "foo", provisioners.TagOperator: "alice",
		}},
		{ProviderID: "2", Tags: map[string]string{
			provisioners.TagID: "bar", provisioners.TagOperator: "bob",
		}},
	})
}
