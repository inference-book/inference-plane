package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// TestPickReplica_RoundRobin: with three healthy endpoints and one
// deployment, consecutive pickReplica calls cycle through all three
// in order. Atomic counter wraps modulo n.
func TestPickReplica_RoundRobin(t *testing.T) {
	r := New(&fakeDeploymentClient{}, nil)
	dep := &provisionerv1.Deployment{
		Id:       "d",
		Replicas: []*provisionerv1.ReplicaBacking{{InstanceIds: []string{"a"}, EngineEndpoint: "http://a"}, {InstanceIds: []string{"b"}, EngineEndpoint: "http://b"}, {InstanceIds: []string{"c"}, EngineEndpoint: "http://c"}},
	}
	got := make([]string, 6)
	for i := range got {
		id, _, ok := r.pickReplica(context.Background(), dep)
		if !ok {
			t.Fatalf("pickReplica returned !ok on iter %d", i)
		}
		got[i] = id
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q (round-robin order broken)", i, got[i], want[i])
		}
	}
}

// TestPickReplica_SkipsEmptyEndpoints: a deployment with one empty
// endpoint slot (e.g., an instance still provisioning, or a future
// quarantined replica) is skipped in the rotation. Counter still
// advances so the rotation stays deterministic across many calls.
func TestPickReplica_SkipsEmptyEndpoints(t *testing.T) {
	r := New(&fakeDeploymentClient{}, nil)
	dep := &provisionerv1.Deployment{
		Id:       "d",
		Replicas: []*provisionerv1.ReplicaBacking{{InstanceIds: []string{"a"}, EngineEndpoint: "http://a"}, {InstanceIds: []string{"b"}}, {InstanceIds: []string{"c"}, EngineEndpoint: "http://c"}},
	}
	seen := map[string]int{}
	for range 30 {
		id, ep, ok := r.pickReplica(context.Background(), dep)
		if !ok {
			t.Fatalf("pickReplica !ok despite 2 healthy slots")
		}
		if ep == "" {
			t.Fatalf("picked an empty endpoint")
		}
		seen[id]++
	}
	if seen["b"] != 0 {
		t.Errorf("b was picked %d times; should be skipped (empty endpoint)", seen["b"])
	}
	if seen["a"] == 0 || seen["c"] == 0 {
		t.Errorf("healthy replicas should each get hits: got a=%d c=%d", seen["a"], seen["c"])
	}
}

// TestPickReplica_AllEmpty: all endpoint slots empty -> returns
// ok=false; the caller maps this to 503 replica_unavailable.
func TestPickReplica_AllEmpty(t *testing.T) {
	r := New(&fakeDeploymentClient{}, nil)
	dep := &provisionerv1.Deployment{
		Id:       "d",
		Replicas: []*provisionerv1.ReplicaBacking{{InstanceIds: []string{"a"}}, {InstanceIds: []string{"b"}}},
	}
	if _, _, ok := r.pickReplica(context.Background(), dep); ok {
		t.Fatalf("pickReplica returned ok=true when all endpoints are empty")
	}
}

// TestPickReplica_SkipsQuarantined: a deployment whose
// unhealthy_instance_ids set contains one of three replicas keeps
// routing exclusively to the other two. v0.2 ch7-beat3.5 acceptance
// per #87 -- the router stops routing to the unhealthy replica even
// though its engine_endpoints[i] URL is still populated (quarantine
// is non-destructive).
func TestPickReplica_SkipsQuarantined(t *testing.T) {
	r := New(&fakeDeploymentClient{}, nil)
	dep := &provisionerv1.Deployment{
		Id:                   "d",
		Replicas:             []*provisionerv1.ReplicaBacking{{InstanceIds: []string{"a"}, EngineEndpoint: "http://a"}, {InstanceIds: []string{"b"}, EngineEndpoint: "http://b"}, {InstanceIds: []string{"c"}, EngineEndpoint: "http://c"}},
		UnhealthyInstanceIds: []string{"b"},
	}
	seen := map[string]int{}
	for range 60 {
		id, ep, ok := r.pickReplica(context.Background(), dep)
		if !ok {
			t.Fatalf("pickReplica !ok despite 2 healthy replicas")
		}
		if ep == "http://b" || id == "b" {
			t.Fatalf("picked the quarantined replica: id=%q ep=%q", id, ep)
		}
		seen[id]++
	}
	if seen["b"] != 0 {
		t.Errorf("quarantined replica b was picked %d times", seen["b"])
	}
	if seen["a"] == 0 || seen["c"] == 0 {
		t.Errorf("healthy replicas a,c should share traffic: got a=%d c=%d", seen["a"], seen["c"])
	}
}

// TestPickReplica_AllQuarantined: every replica in the unhealthy
// set returns ok=false; pairs with the AllEmpty case from #85 --
// router callers map this to 503 replica_unavailable.
func TestPickReplica_AllQuarantined(t *testing.T) {
	r := New(&fakeDeploymentClient{}, nil)
	dep := &provisionerv1.Deployment{
		Id:                   "d",
		Replicas:             []*provisionerv1.ReplicaBacking{{InstanceIds: []string{"a"}, EngineEndpoint: "http://a"}, {InstanceIds: []string{"b"}, EngineEndpoint: "http://b"}},
		UnhealthyInstanceIds: []string{"a", "b"},
	}
	if _, _, ok := r.pickReplica(context.Background(), dep); ok {
		t.Fatalf("pickReplica returned ok=true with every replica quarantined")
	}
}

// TestPickReplica_QuarantinedDoesNotSkipEmptyInstanceID: a replica
// padded with an empty instance_id (test fixture or pre-Beat-3
// record) cannot be matched by the unhealthy_instance_ids set --
// there is no key to put it under. The router falls back to
// "route if endpoint is non-empty," matching the #85 pad-with-
// empty-IDs invariant.
func TestPickReplica_QuarantinedDoesNotSkipEmptyInstanceID(t *testing.T) {
	r := New(&fakeDeploymentClient{}, nil)
	dep := &provisionerv1.Deployment{
		Id:                   "d",
		Replicas:             []*provisionerv1.ReplicaBacking{{EngineEndpoint: "http://only"}},
		UnhealthyInstanceIds: []string{""}, // shouldn't match the empty padded id
	}
	id, ep, ok := r.pickReplica(context.Background(), dep)
	if !ok || ep != "http://only" || id != "" {
		t.Fatalf("expected router to forward to the unkeyed endpoint, got id=%q ep=%q ok=%v", id, ep, ok)
	}
}

// TestPickReplica_SingleInstance_Beat1Compat: a Beat 1+2 deployment
// shape (singular instance_id + engine_endpoint, no list fields)
// works unchanged. Beat 1 tests should keep passing through the
// router's effective-list fallback.
func TestPickReplica_SingleInstance_Beat1Compat(t *testing.T) {
	r := New(&fakeDeploymentClient{}, nil)
	dep := &provisionerv1.Deployment{
		Id:             "single",
		InstanceId:     "the-pod",
		EngineEndpoint: "http://engine:8000",
	}
	for range 3 {
		id, ep, ok := r.pickReplica(context.Background(), dep)
		if !ok || id != "the-pod" || ep != "http://engine:8000" {
			t.Errorf("Beat 1 compat broken: id=%q ep=%q ok=%v", id, ep, ok)
		}
	}
}

// TestRouter_MultiReplica_DistributesEvenly: the v0.2 ch7-beat3.3
// acceptance criterion. 3-replica deployment + 30 requests -> each
// replica gets ~10 hits. The hit count is exactly 10 each because
// 30 % 3 == 0 and the lottery is deterministic; if 30 doesn't
// divide evenly we'd have a ±1 tolerance instead.
func TestRouter_MultiReplica_DistributesEvenly(t *testing.T) {
	const replicas = 3
	const requests = 30

	var perEngineHits [replicas]int64
	engines := make([]*httptest.Server, replicas)
	endpoints := make([]string, replicas)
	for i := range engines {
		i := i
		engines[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&perEngineHits[i], 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		}))
		defer engines[i].Close()
		endpoints[i] = engines[i].URL
	}

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:             "multi",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: endpoints[0],
				Replicas:       membersFrom([]string{"a", "b", "c"}, endpoints),
			}}, nil
		},
	}, nil)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	for range requests {
		resp, err := http.Post(srv.URL+"/v1/multi/v1/chat/completions", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		resp.Body.Close()
	}

	const wantPer = requests / replicas
	for i, hits := range perEngineHits {
		if hits != int64(wantPer) {
			t.Errorf("replica %d got %d hits, want %d (round-robin distribution broken)", i, hits, wantPer)
		}
	}
}

// TestRouter_AllReplicasUnhealthy_503: a deployment with non-empty
// instance_ids but all empty engine_endpoints (every replica still
// provisioning, or all quarantined post-#87) returns 503
// replica_unavailable with a Retry-After header.
//
// The deployment is intentionally constructed with a non-empty
// SINGULAR engine_endpoint so the v0.1 forwardable() precondition
// passes; the all-empty state is on the multi-instance list. This
// is the post-quarantine shape #87 will produce: singular stays
// populated for backward-compat, but the per-replica list is all
// empty because every replica is sidelined.
func TestRouter_AllReplicasUnhealthy_503(t *testing.T) {
	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:             "all-down",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: "http://stale-primary:8000", // singular survives quarantine
				// Every member's endpoint empty: all slots quarantined.
				Replicas: []*provisionerv1.ReplicaBacking{{InstanceIds: []string{"a"}}, {InstanceIds: []string{"b"}}},
			}}, nil
		},
	}, nil)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/all-down/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 503; body = %s", resp.StatusCode, body)
	}
	if h := resp.Header.Get("Retry-After"); h == "" {
		t.Errorf("missing Retry-After header on replica_unavailable 503")
	}
}

// TestRouter_ReplicaID_OnSpanAndMetric: a request that picks
// replica "a" sees iplane.router.replica_id="a" on the span AND
// instance_id="a" on the metric. Chapter narrative: per-request
// trace + metric pair lets operators debug "this slow request
// hit replica X."
func TestRouter_ReplicaID_OnSpanAndMetric(t *testing.T) {
	exp := setupTracingCapture(t)
	reader, recorder := setupMetricsCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:       "tagged",
				Model:    "m",
				State:    provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				Replicas: []*provisionerv1.ReplicaBacking{{InstanceIds: []string{"a"}, EngineEndpoint: engine.URL}},
			}}, nil
		},
	}, recorder)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/tagged/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if got := spanAttrFromStub(spans[0], AttrRouterReplicaID); got != "a" {
		t.Errorf("span replica_id = %q, want a", got)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	reqs := findCounter(t, rm, "inference.requests.total")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request observation, got %d", len(reqs))
	}
	if got := attrValue(reqs[0].Attributes, "instance_id"); got != "a" {
		t.Errorf("metric instance_id = %q, want a", got)
	}
}

// membersFrom builds one member per (instance, endpoint) pair, which is
// the ordinary fan-out shape. A test wanting one engine over several
// nodes builds the ReplicaBacking directly.
func membersFrom(ids, endpoints []string) []*provisionerv1.ReplicaBacking {
	n := max(len(ids), len(endpoints))
	out := make([]*provisionerv1.ReplicaBacking, n)
	for i := range out {
		out[i] = &provisionerv1.ReplicaBacking{}
		if i < len(ids) && ids[i] != "" {
			out[i].InstanceIds = []string{ids[i]}
		}
		if i < len(endpoints) {
			out[i].EngineEndpoint = endpoints[i]
		}
	}
	return out
}

// A member spanning four nodes is one thing to route to, not four. The
// router picks members, and a member is one engine on one endpoint
// however many machines it was assembled from.
func TestRouterTreatsAMultiNodeMemberAsOneReplica(t *testing.T) {
	dep := &provisionerv1.Deployment{
		Id:    "k3",
		State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		Replicas: []*provisionerv1.ReplicaBacking{
			{InstanceIds: []string{"n0", "n1", "n2", "n3"}, EngineEndpoint: "http://a:8000"},
			{InstanceIds: []string{"m0", "m1", "m2", "m3"}, EngineEndpoint: "http://b:8000"},
		},
	}

	got := (&Router{}).eligibleReplicas(dep)

	if len(got) != 2 {
		t.Fatalf("eligible replicas = %d, want 2 (eight machines, two engines): %+v", len(got), got)
	}
	// The member is named by its rank-0 node, which is what metrics and
	// affinity key on.
	if got[0].InstanceID != "n0" || got[1].InstanceID != "m0" {
		t.Errorf("replica ids = %q/%q, want the members' primaries n0/m0", got[0].InstanceID, got[1].InstanceID)
	}
}

// One sick node ends the engine it belongs to. Routing to the survivors
// of a multi-node engine is not degraded service; there is no engine at
// the other end of that endpoint any more.
func TestRouterDropsAMemberWhenAnyOfItsNodesIsQuarantined(t *testing.T) {
	dep := &provisionerv1.Deployment{
		Id:    "k3",
		State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		Replicas: []*provisionerv1.ReplicaBacking{
			{InstanceIds: []string{"n0", "n1", "n2", "n3"}, EngineEndpoint: "http://a:8000"},
			{InstanceIds: []string{"m0", "m1", "m2", "m3"}, EngineEndpoint: "http://b:8000"},
		},
		// Not the primary: a non-rank-0 node is exactly the case a
		// primary-only check would miss.
		UnhealthyInstanceIds: []string{"n2"},
	}

	got := (&Router{}).eligibleReplicas(dep)

	if len(got) != 1 || got[0].InstanceID != "m0" {
		t.Errorf("eligible = %+v, want only the healthy member m0", got)
	}
}
