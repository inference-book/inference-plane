package provisioners

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

func TestEffectiveInstanceIDs(t *testing.T) {
	cases := []struct {
		name string
		dep  *provisionerv1.Deployment
		want []string
	}{
		{
			name: "nil deployment",
			dep:  nil,
			want: nil,
		},
		{
			name: "multi-instance list populated",
			dep: &provisionerv1.Deployment{
				InstanceId: "primary",
				Replicas:   []*provisionerv1.ReplicaBacking{{InstanceIds: []string{"a"}}, {InstanceIds: []string{"b"}}, {InstanceIds: []string{"c"}}},
			},
			want: []string{"a", "b", "c"},
		},
		{
			name: "empty list falls back to singular",
			dep: &provisionerv1.Deployment{
				InstanceId: "primary",
			},
			want: []string{"primary"},
		},
		{
			name: "everything empty",
			dep:  &provisionerv1.Deployment{},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveInstanceIDs(tc.dep)
			if len(got) != len(tc.want) {
				t.Fatalf("len=%d, want %d (got=%v want=%v)", len(got), len(tc.want), got, tc.want)
			}
			for i, v := range tc.want {
				if got[i] != v {
					t.Errorf("[%d] = %q, want %q", i, got[i], v)
				}
			}
		})
	}
}

func TestEffectiveEndpoints(t *testing.T) {
	cases := []struct {
		name string
		dep  *provisionerv1.Deployment
		want []string
	}{
		{
			name: "multi-endpoint list populated",
			dep: &provisionerv1.Deployment{
				EngineEndpoint: "http://primary:8000",
				Replicas:       []*provisionerv1.ReplicaBacking{{EngineEndpoint: "http://a:8000"}, {EngineEndpoint: "http://b:8000"}},
			},
			want: []string{"http://a:8000", "http://b:8000"},
		},
		{
			name: "empty list falls back to singular",
			dep: &provisionerv1.Deployment{
				EngineEndpoint: "http://primary:8000",
			},
			want: []string{"http://primary:8000"},
		},
		{
			name: "single-instance Beat 1+2 deployment shape",
			dep: &provisionerv1.Deployment{
				InstanceId:     "only",
				EngineEndpoint: "http://only:8000",
			},
			want: []string{"http://only:8000"},
		},
		{
			name: "everything empty",
			dep:  &provisionerv1.Deployment{},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveEndpoints(tc.dep)
			if len(got) != len(tc.want) {
				t.Fatalf("len=%d, want %d (got=%v want=%v)", len(got), len(tc.want), got, tc.want)
			}
			for i, v := range tc.want {
				if got[i] != v {
					t.Errorf("[%d] = %q, want %q", i, got[i], v)
				}
			}
		})
	}
}

// The distinction the whole model exists for. A deployment of two
// members, each one engine over four nodes, is eight machines billing
// and two endpoints to route to. Conflating them is how a bill gets
// read wrong in one direction and a load balancer in the other.
func TestHelpersSeparateMachinesFromMembers(t *testing.T) {
	dep := &provisionerv1.Deployment{
		Id: "k3",
		Replicas: []*provisionerv1.ReplicaBacking{
			{InstanceIds: []string{"n0", "n1", "n2", "n3"}, EngineEndpoint: "http://a:8000"},
			{InstanceIds: []string{"m0", "m1", "m2", "m3"}, EngineEndpoint: "http://b:8000"},
		},
	}

	if got := EffectiveInstanceIDs(dep); len(got) != 8 {
		t.Errorf("machines = %v, want all eight: every one of them is rented", got)
	}
	if got := EffectiveEndpoints(dep); len(got) != 2 {
		t.Errorf("endpoints = %v, want two: one engine serves on one address however many nodes it spans", got)
	}
	if got := EffectiveMemberInstanceIDs(dep); len(got) != 2 || got[0] != "n0" || got[1] != "m0" {
		t.Errorf("member primaries = %v, want [n0 m0]", got)
	}
	// The reverse lookup a slot patch needs: an update names a node,
	// and the record it belongs to is the member.
	if got := MemberOf(dep, "n2"); got != 0 {
		t.Errorf("MemberOf(n2) = %d, want 0", got)
	}
	if got := MemberOf(dep, "m1"); got != 1 {
		t.Errorf("MemberOf(m1) = %d, want 1", got)
	}
	if got := MemberOf(dep, "nobody"); got != -1 {
		t.Errorf("MemberOf(nobody) = %d, want -1", got)
	}
}

// A record written before members existed carries only the singular
// instance_id, and every helper resolves it to the one member it always
// was, so no caller branches on the absence.
func TestHelpersResolveALegacySingleInstanceRecord(t *testing.T) {
	dep := &provisionerv1.Deployment{Id: "old", InstanceId: "pod-1", EngineEndpoint: "http://old:8000"}

	if got := EffectiveInstanceIDs(dep); len(got) != 1 || got[0] != "pod-1" {
		t.Errorf("machines = %v, want [pod-1]", got)
	}
	if got := EffectiveEndpoints(dep); len(got) != 1 || got[0] != "http://old:8000" {
		t.Errorf("endpoints = %v, want the singular", got)
	}
	if got := EffectiveMemberInstanceIDs(dep); len(got) != 1 || got[0] != "pod-1" {
		t.Errorf("member primaries = %v, want [pod-1]", got)
	}
}
