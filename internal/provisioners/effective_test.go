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
				InstanceId:  "primary",
				InstanceIds: []string{"a", "b", "c"},
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
				EngineEndpoint:  "http://primary:8000",
				EngineEndpoints: []string{"http://a:8000", "http://b:8000"},
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
