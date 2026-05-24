// Package runpod implements the Provider interface against RunPod's
// REST API (https://rest.runpod.io/v1). The adapter speaks five
// endpoints: POST /pods (create), GET /pods/{id} (describe), GET /pods
// (list with server-side filtering), DELETE /pods/{id} (terminate),
// and a follow-up GET /pods/{id} after every POST /pods to fetch the
// full pod record (POST returns only {id, desiredStatus, status}).
//
// Why REST and not GraphQL. RunPod has both, but the REST API is the
// pattern every other provider iplane will ship against (Lambda Labs
// in v0.2, Vast.ai / AWS / GCP in v0.3). Keeping all adapters on one
// transport means shared HTTP scaffolding, shared error mapping,
// shared OTel instrumentation later. The book also benefits: act-2's
// manual path is a one-line curl, not a GraphQL query string.
//
// Tag stamping in v0.1. RunPod's REST create body has no env field,
// so the adapter encodes iplane-id in the pod name (prefix "iplane-")
// and that is the only iplane tag visible on the pod itself. Server-side
// filtering by iplane-id works via the ?name= query param. The
// iplane-operator tag lives only in the iplane state file in v0.1 --
// good enough because v0.1 is single-operator. Multi-operator state
// (v1.0) revisits this with templates or a post-create update call.
//
// Base image and deploy split. v0.1's design (see
// docs/design/0001-provisioner.md "Provisioner / deploy split on
// RunPod") says phase 1 provisions a Docker-capable base image and
// phase 2's deploy primitive docker-runs the engine container on top.
// Default base is RunPod's PyTorch image; operator overrides via
// Spec.base_image.
//
// SSH readiness. Spawn returns State=ACTIVE as soon as RunPod
// acknowledges the pod and we have its full record, but the SSH
// endpoint may take another 20-60s to become reachable. ssh fields
// stay empty in the Spawn response unless RunPod has already assigned
// an IP. Phase 2's deploy primitive polls Describe before docker-run.
package runpod

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	skhttp "github.com/panyam/servicekit/http"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Defaults for fields that v0.1 does not expose on Spec. Phase 1.4's
// CLI may eventually expose some of these (--container-disk, etc.);
// for now they are hardcoded.
//
// cloudType is deliberately unset. RunPod's two cloud types are SECURE
// (T1/T2 datacenters; A100s, H100s, datacenter cards) and COMMUNITY
// (T3/T4 community hosts; cheap consumer cards like RTX 4090 and
// A5000). Pinning either side silently filters out half the catalog --
// an operator who asked for class=small under SECURE gets a "no pods
// available" because the cheap SKUs live in COMMUNITY. Leaving
// cloudType empty lets RunPod schedule on whichever has capacity for
// the requested gpuTypeIds.
//
// publicIp interaction: SECURE pods get publicIp by default; COMMUNITY
// pods do NOT. Because cloudType is unpinned, the same Spawn call can
// silently land on either cloud -- giving "sometimes I see a publicIp,
// sometimes I don't" behavior that hangs our waitForEngineReady /
// WaitForSSHReady probes (both poll for publicIp + NAT mappings). We
// set `supportPublicIp: true` on every POST /pods so the IP is
// allocated regardless of which cloud was picked. The flip side:
// community hosts that can't honor the request will reject the POST
// up front (clean 500), which is preferable to a silent 2m hang.
//
// Also relevant: gpuTypePriority=availability (defaultGPUPriority
// below) tells RunPod "land on any matching SKU on any host with
// capacity," which makes community placements more likely than the
// old "pin to cheapest matches[0]" path -- so we'd see the missing
// publicIp on a lot more pods without the supportPublicIp toggle.
const (
	defaultBaseImage       = "runpod/pytorch:2.4.0-py3.11-cuda12.4.1-devel-ubuntu22.04"
	defaultContainerDiskGB = 20
	defaultVolumeGB        = 0
	defaultComputeType     = "GPU"
	defaultGPUPriority     = "availability" // try SKUs in availability order; "custom" for strict priority
	podNamePrefix          = "iplane-"
)

// Default ports list. SSH (22/tcp) for phase 2's deploy primitive to
// docker-run on top; HTTP (8000/http) for vLLM's OpenAI-compat server
// once the engine container is running.
var defaultPortsList = []string{"22/tcp", "8000/http"}

// Provider implements provisioners.Provider for RunPod.
type Provider struct {
	client *Client
	clock  func() time.Time

	// sshReadyTimeout caps how long WaitForSSHReady waits for the pod's
	// PublicIP to be assigned AND for sshd inside the pod to be
	// reachable on tcp/22. Two sequential conditions; the timeout
	// covers both. Defaults: 120s timeout (covers the worst RunPod
	// scheduling tail PLUS container boot + sshd startup), 3s
	// polling interval. Tests inject short values via WithSSHReadyWait.
	sshReadyTimeout  time.Duration
	sshReadyInterval time.Duration

	// sshProbe is the function used to verify tcp/22 is actually
	// accepting connections (RunPod's publicIp can be assigned a few
	// seconds before the container's sshd is ready to handshake).
	// Default: net.DialTimeout. Tests inject a no-op via WithSSHProbe
	// so they don't need a real listener.
	sshProbe func(ctx context.Context, host string, port int32) error
}

// Option configures a Provider at construction.
type Option func(*Provider)

// WithSSHReadyWait overrides the default poll deadline + interval
// used by WaitForSSHReady. Tests inject short values to keep them
// fast.
func WithSSHReadyWait(timeout, interval time.Duration) Option {
	return func(p *Provider) {
		p.sshReadyTimeout = timeout
		p.sshReadyInterval = interval
	}
}

// WithSSHProbe overrides the tcp-port-reachability check that
// WaitForSSHReady runs after the publicIp is populated. Tests pass a
// no-op so they don't need a real listener; production uses the
// default net.DialTimeout-based probe.
func WithSSHProbe(probe func(ctx context.Context, host string, port int32) error) Option {
	return func(p *Provider) { p.sshProbe = probe }
}

// New builds a RunPod Provider on top of a configured Client.
func New(client *Client, opts ...Option) *Provider {
	p := &Provider{
		client:           client,
		clock:            time.Now,
		sshReadyTimeout:  120 * time.Second,
		sshReadyInterval: 5 * time.Second,
		sshProbe:         dialTCPProbe,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// dialTCPProbe is the production sshProbe: open a TCP connection
// with a tight timeout and close it. A successful dial means sshd
// (or whatever the pod has on port 22) accepted the SYN; the actual
// SSH handshake happens later in the deployment executor.
func dialTCPProbe(ctx context.Context, host string, port int32) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// Name satisfies provisioners.Provider.
func (p *Provider) Name() string { return provisioners.ProviderRunPod }

// IsActiveProviderState satisfies provisioners.ActiveStateChecker.
// Delegates to the adapter-local mapping in skus.go.
func (p *Provider) IsActiveProviderState(state string) bool {
	return isActiveProviderState(state)
}

// Spawn issues POST /pods, then immediately follows up with
// GET /pods/{id} to fetch the full pod record (POST returns only
// {id, desiredStatus, status}; we need costPerHr, createdAt, machine
// info, ipAddress to populate the iplane Instance).
func (p *Provider) Spawn(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	if spec == nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn", fmt.Errorf("spec is nil"), 0)
	}

	// Resolve requirements -> ordered SKU list.
	//
	//   - Operator-supplied --gpu-sku is the escape hatch: pass through.
	//   - Otherwise, the service already expanded any --gpu-class
	//     shorthand into min_vram_gb / min_disk_gb / min_ram_gb on the
	//     ResourceRequirements before dispatching here. We ask the
	//     resolver (skus.go MatchSKUs) for the gpuTypeIds satisfying
	//     those constraints, cheapest first.
	reqs := spec.GetRequirements()
	if reqs == nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("requirements is required"), 0)
	}
	var gpuTypeIDs []string
	resolvedSKU := reqs.GetSku()
	resolvedClass := reqs.GetClass()
	switch {
	case resolvedSKU != "":
		gpuTypeIDs = []string{resolvedSKU}
		if resolvedClass == "" {
			resolvedClass = classifySKU(resolvedSKU)
		}
	default:
		gpuTypeIDs = MatchSKUs(reqs)
		if len(gpuTypeIDs) == 0 {
			return nil, provisioners.NewProviderError(p.Name(), "spawn",
				fmt.Errorf("no SKU in the runpod catalog satisfies min_vram_gb=%d min_disk_gb=%d min_ram_gb=%d (try a different class or a lower constraint)",
					reqs.GetMinVramGb(), reqs.GetMinDiskGb(), reqs.GetMinRamGb()), 0)
		}
		resolvedSKU = gpuTypeIDs[0]
		if resolvedClass == "" {
			resolvedClass = classifySKU(resolvedSKU)
		}
	}
	gpuCount := int(reqs.GetGpuCount())
	if gpuCount <= 0 {
		gpuCount = 1
	}

	image := spec.GetBaseImage()
	if image == "" {
		image = defaultBaseImage
	}

	// Disk is per-instance in our model; RunPod's containerDiskInGb is
	// also per-instance, so direct mapping. Use the operator's
	// min_disk_gb if larger than our default.
	containerDisk := defaultContainerDiskGB
	if d := int(reqs.GetMinDiskGb()); d > containerDisk {
		containerDisk = d
	}

	// System RAM is per-instance in our model; RunPod's minRAMPerGPU is
	// per-GPU. Convert at the wire boundary: total/N (with a ceiling to
	// round up so we never under-request -- "I asked for 65 GB total on
	// a 2-GPU pod" should send minRAMPerGPU=33, not 32).
	minRAMPerGPU := 0
	if r := int(reqs.GetMinRamGb()); r > 0 {
		minRAMPerGPU = (r + gpuCount - 1) / gpuCount
	}

	// Region is a best-effort pin: send dataCenterIds only when the
	// operator actually asked for a region. The demo defaults region to
	// "US-WA-1" for runpod, but pinning a single datacenter when an
	// operator just wants "any cheap GPU" turns "no capacity in this DC"
	// into "no capacity at all". Empty region = no pin = RunPod
	// schedules wherever capacity exists.
	var dataCenterIDs []string
	if r := strings.TrimSpace(spec.GetRegion()); r != "" {
		dataCenterIDs = []string{r}
	}

	createBody := createPodRequest{
		Name:              podNamePrefix + spec.GetId(),
		ImageName:         image,
		GPUTypeIDs:        gpuTypeIDs,
		GPUTypePriority:   defaultGPUPriority,
		GPUCount:          gpuCount,
		MinRAMPerGPU:      minRAMPerGPU,
		ComputeType:       defaultComputeType,
		ContainerDiskInGB: containerDisk,
		VolumeInGB:        defaultVolumeGB,
		Ports:             defaultPortsList,
		DataCenterIDs:     dataCenterIDs,
		// Force publicIp allocation. See the cloudType comment block
		// above for why this is required when cloudType is unpinned.
		SupportPublicIP: true,
	}

	req, err := p.client.newReq("POST", "/pods", nil, createBody)
	if err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn", err, 0)
	}
	created, err := skhttp.Call[createPodResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, wrapErr("spawn", err)
	}
	if created.ID == "" {
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("runpod returned empty pod id (likely no capacity in %q for gpu %q)", spec.GetRegion(), resolvedSKU), 0)
	}

	// Follow-up GET to fetch the full record. We use the freshly-returned
	// pod id, so a 404 here is genuinely surprising; surface as-is.
	getReq, err := p.client.newReq("GET", "/pods/"+created.ID, nil, nil)
	if err != nil {
		return p.instanceFromCreate(spec, &created, resolvedClass, resolvedSKU, gpuCount), nil
	}
	pod, err := skhttp.Call[podBody](ctx, getReq, p.client.callOpts()...)
	if err != nil {
		// Spawn succeeded but follow-up failed. The pod exists; return a
		// best-effort Instance from what we know so the Service can
		// record it in state. Later Describe / List will fill the gaps.
		return p.instanceFromCreate(spec, &created, resolvedClass, resolvedSKU, gpuCount), nil
	}

	// Note: pod.publicIp is usually empty here -- RunPod assigns the
	// public IP a few seconds after the pod is scheduled, after this
	// immediate follow-up GET returns. Callers that need the SSH
	// endpoint populated (e.g. before deploying onto the pod) drive
	// the wait explicitly via WaitForSSHReady; Spawn returns ACTIVE
	// fast so callers that don't need SSH (e.g. operators who just
	// want a billed pod for an interactive jupyter / vscode session)
	// don't pay for it.
	return p.podToInstance(&pod, spec, resolvedClass, resolvedSKU, gpuCount), nil
}

// WaitForSSHReady waits until the pod's SSH endpoint is genuinely
// usable -- both (1) RunPod has assigned the public IP and (2) the
// pod's sshd is accepting TCP connections on port 22. Both conditions
// have an unbounded tail (RunPod's IP assignment ~5-10s, container
// boot + sshd startup another ~10-30s), and the whole window has to
// elapse before a Phase 2 deployment can SSH in. This is the single
// "Join" point for callers.
//
// Returns the resolved SshTarget on success, or an error on timeout
// / ctx cancellation / underlying GET failure. On timeout, the
// partial SshTarget is returned alongside the error so callers who
// want a "best effort" view (e.g. iplane instance describe after a
// failed wait) can still inspect what came back.
//
// Multiple callers can safely call this concurrently; each is a
// no-op-when-already-populated check followed by a polling loop.
func (p *Provider) WaitForSSHReady(ctx context.Context, providerID string) (*provisionerv1.SshTarget, error) {
	if providerID == "" {
		return nil, provisioners.NewProviderError(p.Name(), "wait_ssh_ready", fmt.Errorf("providerID is empty"), 0)
	}
	timeout := p.sshReadyTimeout
	if timeout <= 0 {
		// Always allow at least one GET; tests that disable polling
		// still want a single best-effort lookup.
		timeout = 1 * time.Second
	}
	interval := p.sshReadyInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)

	var last podBody
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return sshTargetFromPod(&last), ctx.Err()
			case <-time.After(interval):
			}
		}
		first = false
		if time.Now().After(deadline) {
			return sshTargetFromPod(&last), provisioners.NewProviderError(p.Name(), "wait_ssh_ready",
				fmt.Errorf("public ip not assigned within %s", timeout), 0)
		}
		getReq, err := p.client.newReq("GET", "/pods/"+providerID, nil, nil)
		if err != nil {
			return nil, provisioners.NewProviderError(p.Name(), "wait_ssh_ready", err, 0)
		}
		pod, err := skhttp.Call[podBody](ctx, getReq, p.client.callOpts()...)
		if err != nil {
			return sshTargetFromPod(&last), wrapErr("wait_ssh_ready", err)
		}
		last = pod
		if pod.PublicIP != "" {
			target := sshTargetFromPod(&pod)
			// Stage 2: probe tcp/22 until sshd inside the pod is
			// reachable. Reuses the same deadline + interval so the
			// overall budget covers both stages.
			if err := p.waitForSSHPort(ctx, target, deadline, interval); err != nil {
				return target, err
			}
			return target, nil
		}
	}
}

// waitForSSHPort retries the TCP probe until it succeeds or the
// deadline fires. A successful probe means the pod's sshd is up;
// the deployment executor's actual SSH handshake will be the next
// step.
func (p *Provider) waitForSSHPort(ctx context.Context, target *provisionerv1.SshTarget, deadline time.Time, interval time.Duration) error {
	if p.sshProbe == nil {
		return nil
	}
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
		first = false
		if time.Now().After(deadline) {
			return provisioners.NewProviderError(p.Name(), "wait_ssh_ready",
				fmt.Errorf("tcp/%d on %s not reachable before deadline", target.GetPort(), target.GetHost()), 0)
		}
		if err := p.sshProbe(ctx, target.GetHost(), target.GetPort()); err == nil {
			return nil
		}
	}
}

// sshTargetFromPod builds a SshTarget from a podBody. Honors RunPod's
// portMappings field -- a pod whose internal port 22 is NATed to a
// random external port (e.g. 22085) returns that external port, not
// 22, so the operator's ssh / the deployment executor's dial lands
// on the container's sshd and not the host's. Returns nil when
// PublicIP is empty so callers can distinguish "haven't observed an
// IP yet" from "observed an IP and it's literally blank."
func sshTargetFromPod(pod *podBody) *provisionerv1.SshTarget {
	if pod == nil || pod.PublicIP == "" {
		return nil
	}
	return &provisionerv1.SshTarget{
		Host: pod.PublicIP,
		Port: sshPortFromPod(pod),
		User: "root",
	}
}

// sshPortFromPod resolves the externally-dialable port for the
// container's sshd. RunPod sometimes maps the container's port 22
// to an arbitrary public-side port (random in the dynamic-port
// range); the operator dials that public port, RunPod's NAT
// forwards to container:22. Without the lookup we'd dial port 22
// directly and either hit the host's sshd (wrong keys) or time
// out (no service listening on that port).
//
// Falls back to 22 when no mapping is reported -- some RunPod
// configurations expose port 22 as-is on the public IP.
func sshPortFromPod(pod *podBody) int32 {
	if mapped, ok := pod.PortMappings["22"]; ok && mapped > 0 {
		return int32(mapped)
	}
	return 22
}

// Terminate calls DELETE /pods/{id}. Idempotent: a 404 (not found)
// is treated as success because the pod is already in the desired
// end state.
func (p *Provider) Terminate(ctx context.Context, providerID string) error {
	if providerID == "" {
		return provisioners.NewProviderError(p.Name(), "terminate", fmt.Errorf("providerID is empty"), 0)
	}
	req, err := p.client.newReq("DELETE", "/pods/"+providerID, nil, nil)
	if err != nil {
		return provisioners.NewProviderError(p.Name(), "terminate", err, 0)
	}
	if err := skhttp.CallVoid(ctx, req, p.client.callOpts()...); err != nil {
		wrapped := wrapErr("terminate", err)
		if isWrappedNotFound(wrapped) {
			return nil
		}
		return wrapped
	}
	return nil
}

// Describe calls GET /pods/{id}. Returns ErrNotFound wrapped in
// ProviderError when RunPod returns 404.
func (p *Provider) Describe(ctx context.Context, providerID string) (*provisionerv1.Instance, error) {
	if providerID == "" {
		return nil, provisioners.NewProviderError(p.Name(), "describe", fmt.Errorf("providerID is empty"), 0)
	}
	req, err := p.client.newReq("GET", "/pods/"+providerID, nil, nil)
	if err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "describe", err, 0)
	}
	pod, err := skhttp.Call[podBody](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, wrapErr("describe", err)
	}
	tags := map[string]string{}
	if name := strings.TrimPrefix(pod.Name, podNamePrefix); name != pod.Name && name != "" {
		tags[provisioners.TagID] = name
	}
	return p.podToInstance(&pod, specFromPod(&pod, tags), classifySKU(pod.gpuSKU()), pod.gpuSKU(), pod.gpuCountInt()), nil
}



// List calls GET /pods. When the filter includes iplane-id, we add
// ?name=iplane-<id> for server-side filtering. Other filter keys
// (e.g., iplane-operator) are applied client-side, but in v0.1 only
// iplane-id ends up encoded on the pod (via the name), so other tags
// would never match -- they silently filter out everything. Callers
// asking for operator-level filtering should consult the iplane state
// file instead, which IS the source of truth for that scope in v0.1.
func (p *Provider) List(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error) {
	q := url.Values{}
	if id, ok := filter[provisioners.TagID]; ok && id != "" {
		q.Set("name", podNamePrefix+id)
	}

	req, err := p.client.newReq("GET", "/pods", q, nil)
	if err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "list", err, 0)
	}
	pods, err := skhttp.Call[[]podBody](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, wrapErr("list", err)
	}

	refs := make([]*provisionerv1.InstanceRef, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		tags := map[string]string{}
		if name := strings.TrimPrefix(pod.Name, podNamePrefix); name != pod.Name && name != "" {
			tags[provisioners.TagID] = name
		}
		if !matchesFilter(tags, filter) {
			continue
		}
		refs = append(refs, &provisionerv1.InstanceRef{
			ProviderId:    pod.ID,
			ProviderState: pod.DesiredStatus,
			Tags:          tags,
			HourlyRateUsd: pod.CostPerHr,
			CreatedAt:     parseRunPodTime(pod.CreatedAt),
		})
	}
	return refs, nil
}

// podToInstance assembles the iplane Instance from a fully-populated
// pod record (the response of GET /pods/{id}). resolvedClass and
// resolvedSKU come from the caller because Spawn knows them up-front
// and RunPod's response sometimes omits them pre-runtime-ready.
func (p *Provider) podToInstance(pod *podBody, spec *provisionerv1.Spec, resolvedClass, resolvedSKU string, gpuCount int) *provisionerv1.Instance {
	createdAt := parseRunPodTime(pod.CreatedAt)
	now := timestamppb.New(p.clock())
	if resolvedSKU == "" {
		resolvedSKU = pod.gpuSKU()
	}
	if resolvedClass == "" {
		resolvedClass = classifySKU(resolvedSKU)
	}
	if gpuCount <= 0 {
		gpuCount = pod.gpuCountInt()
	}
	vramGB := pod.gpuVRAMGB()

	ssh := &provisionerv1.SshTarget{}
	if pod.PublicIP != "" {
		ssh.Host = pod.PublicIP
		ssh.Port = sshPortFromPod(pod)
		ssh.User = "root"
	}

	region := pod.dataCenter()
	if region == "" {
		region = spec.GetRegion()
	}

	return &provisionerv1.Instance{
		Id:         spec.GetId(),
		ProviderId: pod.ID,
		Provider:   p.Name(),
		Spec:       spec,
		Region:     region,
		Gpu: &provisionerv1.GpuInfo{
			Class:  resolvedClass,
			Sku:    resolvedSKU,
			Count:  int32(gpuCount),
			VramGb: int32(vramGB),
		},
		HourlyRateUsd: pod.CostPerHr,
		State:         provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		CreatedAt:     createdAt,
		ActivatedAt:   now,
		Ssh:           ssh,
	}
}

// instanceFromCreate builds a minimum-viable Instance when the
// post-create GET /pods/{id} fails. Used as a fallback so a temporary
// follow-up failure does not lose the operator's pod (it exists in
// RunPod even though we cannot read its full record right now). Later
// list / describe calls will populate the gaps.
func (p *Provider) instanceFromCreate(spec *provisionerv1.Spec, created *createPodResponse, resolvedClass, resolvedSKU string, gpuCount int) *provisionerv1.Instance {
	now := timestamppb.New(p.clock())
	return &provisionerv1.Instance{
		Id:         spec.GetId(),
		ProviderId: created.ID,
		Provider:   p.Name(),
		Spec:       spec,
		Region:     spec.GetRegion(),
		Gpu: &provisionerv1.GpuInfo{
			Class: resolvedClass,
			Sku:   resolvedSKU,
			Count: int32(gpuCount),
		},
		State:       provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		CreatedAt:   now,
		ActivatedAt: now,
	}
}

// specFromPod reconstructs an approximate Spec for an externally-found
// pod (one Describe returned but we have no local record for). Used
// only by the Service's adopt path. The result is good enough to stamp
// on the Instance, not authoritative.
func specFromPod(pod *podBody, tags map[string]string) *provisionerv1.Spec {
	return &provisionerv1.Spec{
		Id:        tags[provisioners.TagID],
		Provider:  provisioners.ProviderRunPod,
		Region:    pod.dataCenter(),
		BaseImage: pod.Image,
		Tags:      tags,
		Requirements: &provisionerv1.ResourceRequirements{
			Sku:      pod.gpuSKU(),
			GpuCount: int32(pod.gpuCountInt()),
			MinVramGb: int32(pod.gpuVRAMGB()),
		},
	}
}

// matchesFilter applies the post-fetch tag filter. v0.1 only encodes
// iplane-id on the pod (via name), so any filter key beyond iplane-id
// either matches because the value happens to be empty on both sides
// (rare) or filters out the pod entirely. Documented behavior.
func matchesFilter(podTags, want map[string]string) bool {
	for k, v := range want {
		if podTags[k] != v {
			return false
		}
	}
	return true
}

func parseRunPodTime(s string) *timestamppb.Timestamp {
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return timestamppb.New(t)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return timestamppb.New(t)
	}
	return nil
}

// isWrappedNotFound walks the error chain looking for ErrNotFound.
// Used by Terminate to return nil on already-gone (the contract is
// idempotent).
func isWrappedNotFound(err error) bool {
	for e := err; e != nil; {
		if e == provisioners.ErrNotFound {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// createPodRequest is the JSON body for POST /pods. Field names match
// RunPod's REST docs exactly so the wire shape is debuggable from the
// Go side. Omit-empty everywhere; the API treats missing fields as
// "use the documented default."
type createPodRequest struct {
	Name              string            `json:"name,omitempty"`
	ImageName         string            `json:"imageName,omitempty"`
	GPUTypeIDs        []string          `json:"gpuTypeIds,omitempty"`
	GPUTypePriority   string            `json:"gpuTypePriority,omitempty"`
	GPUCount          int               `json:"gpuCount,omitempty"`
	MinRAMPerGPU      int               `json:"minRAMPerGPU,omitempty"` // RunPod expresses RAM per-GPU; we convert from per-instance.
	CloudType         string            `json:"cloudType,omitempty"`
	ComputeType       string            `json:"computeType,omitempty"`
	ContainerDiskInGB int               `json:"containerDiskInGb,omitempty"`
	VolumeInGB        int               `json:"volumeInGb,omitempty"`
	NetworkVolumeID   string            `json:"networkVolumeId,omitempty"`
	Ports             []string          `json:"ports,omitempty"`
	Env               map[string]string `json:"env,omitempty"` // RunPod's REST uses a flat key/value map.
	// DockerStartCmd REPLACES the image's CMD with these argv tokens
	// (ENTRYPOINT — the engine binary — stays). RunPod's REST API
	// removed the legacy single-string `dockerArgs` in favor of these
	// two array fields; we pass args as an argv slice, not a shell-split
	// string.
	DockerStartCmd   []string `json:"dockerStartCmd,omitempty"`
	DockerEntrypoint []string `json:"dockerEntrypoint,omitempty"`
	Interruptible    bool     `json:"interruptible,omitempty"`
	TemplateID        string   `json:"templateId,omitempty"`
	SupportPublicIP   bool     `json:"supportPublicIp,omitempty"`
	DataCenterIDs     []string `json:"dataCenterIds,omitempty"` // best-effort; if rejected, RunPod schedules anywhere
}

// createPodResponse is the minimal response shape from POST /pods.
// We immediately follow up with GET /pods/{id} for the full record.
type createPodResponse struct {
	ID            string `json:"id"`
	DesiredStatus string `json:"desiredStatus"`
	Status        string `json:"status"`
}

// podBody mirrors the subset of RunPod's pod schema we consume. We
// deliberately do not bind every field RunPod returns -- only the
// ones that flow through to the iplane Instance.
type podBody struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Image         string         `json:"image"`
	MachineID     string         `json:"machineId"`
	CostPerHr     float64        `json:"costPerHr"`
	CreatedAt     string         `json:"createdAt"`
	DesiredStatus string         `json:"desiredStatus"`
	PublicIP      string         `json:"publicIp"`
	Ports         []string       `json:"ports"`
	// PortMappings encodes RunPod's NAT for ports declared in the
	// create-pod request. Keys are container-internal ports (as
	// strings, JSON-style), values are the public-IP-side ports.
	// Example: {"22": 22085} means an operator dialing publicIp:22085
	// reaches the container's port 22. SSH wires this through into
	// Ssh.Port; without the translation, every dial hits the host's
	// sshd or nothing at all.
	PortMappings  map[string]int `json:"portMappings"`
	Machine       *podMachine    `json:"machine"`
}

type podMachine struct {
	GPUTypeID    string  `json:"gpuTypeId"`
	GPUCount     int     `json:"gpuCount"`
	DataCenterID string  `json:"dataCenterId"`
	GPUType      *gpuType `json:"gpuType"`
}

type gpuType struct {
	ID         string `json:"id"`
	MemoryInGB int    `json:"memoryInGb"`
	Count      int    `json:"count"`
}

func (p *podBody) gpuSKU() string {
	if p.Machine != nil && p.Machine.GPUType != nil && p.Machine.GPUType.ID != "" {
		return p.Machine.GPUType.ID
	}
	if p.Machine != nil {
		return p.Machine.GPUTypeID
	}
	return ""
}

func (p *podBody) gpuVRAMGB() int {
	if p.Machine != nil && p.Machine.GPUType != nil {
		return p.Machine.GPUType.MemoryInGB
	}
	return 0
}

func (p *podBody) gpuCountInt() int {
	if p.Machine != nil {
		if p.Machine.GPUCount > 0 {
			return p.Machine.GPUCount
		}
		if p.Machine.GPUType != nil && p.Machine.GPUType.Count > 0 {
			return p.Machine.GPUType.Count
		}
	}
	return 0
}

func (p *podBody) dataCenter() string {
	if p.Machine != nil {
		return p.Machine.DataCenterID
	}
	return ""
}

// Compile-time check.
var _ provisioners.Provider = (*Provider)(nil)
