package provisioners

import (
	"sort"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// intentDeploymentFields are the fields the OPERATOR sets and a fresh record
// must carry. Kept here rather than beside runtimeDeploymentFields because
// production reads only the runtime list; this one exists so the two together
// can be checked against the proto.
var intentDeploymentFields = map[string]bool{
	"id":                true,
	"instance_id":       true,
	"image":             true,
	"model":             true,
	"engine_args":       true,
	"engine_entrypoint": true,
	"engine_port":       true,
	"env":               true,
	"mounts":            true,
	"debug_shell":       true,
	"idle_ttl_seconds":  true,
	"no_idle_destroy":   true,
	"upstream_auth":     true,
	"parallelism":       true,
}

// Adding a field to Deployment forces a decision about whether a new record
// carries it or clears it, and this fails until that decision is written down.
//
// Without it the decision is made by omission. That is how outbound auth
// shipped: the credential was accepted, validated, and dropped, and nothing
// anywhere failed. The record simply did not have it, so the router presented
// nothing and every forwarded request went out unauthenticated (#182, #312).
func TestEveryDeploymentFieldIsClassified(t *testing.T) {
	runtime := map[string]bool{}
	for _, n := range runtimeDeploymentFields {
		runtime[string(n)] = true
	}

	fields := (&provisionerv1.Deployment{}).ProtoReflect().Descriptor().Fields()
	var unclassified, both []string
	for i := 0; i < fields.Len(); i++ {
		name := string(fields.Get(i).Name())
		switch {
		case runtime[name] && intentDeploymentFields[name]:
			both = append(both, name)
		case !runtime[name] && !intentDeploymentFields[name]:
			unclassified = append(unclassified, name)
		}
	}
	sort.Strings(unclassified)

	if len(unclassified) > 0 {
		t.Errorf("Deployment fields %v are classified as neither operator intent nor control-plane runtime.\n"+
			"Add each to intentDeploymentFields (a fresh record carries it) or to runtimeDeploymentFields "+
			"(a fresh record clears it). Leaving a field unclassified is how an operator-set value gets "+
			"silently dropped.", unclassified)
	}
	if len(both) > 0 {
		t.Errorf("Deployment fields %v are in both lists; a field is one or the other", both)
	}
}

// The runtime list has to name fields that exist. A typo would silently clear
// nothing, which is the failure this whole file is about, one level down.
func TestRuntimeFieldNamesExist(t *testing.T) {
	fields := (&provisionerv1.Deployment{}).ProtoReflect().Descriptor().Fields()
	for _, n := range runtimeDeploymentFields {
		if fields.ByName(n) == nil {
			t.Errorf("runtimeDeploymentFields names %q, which is not a field on Deployment", n)
		}
	}
}
