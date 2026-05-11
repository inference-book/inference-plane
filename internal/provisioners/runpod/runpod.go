// Package runpod implements the Provider interface against RunPod's
// GraphQL API. The adapter speaks the podFindAndDeployOnDemand /
// podTerminate / myself.pods surface and translates between
// provisioners.Spec / Instance and RunPod's pod shape.
//
// Tag stamping. RunPod has no native tag/label concept, so the adapter
// encodes iplane tags two ways: the iplane-id goes into the pod name
// (prefixed "iplane-") for cheap server-side List filtering, and the
// full Tags map (including iplane-id and iplane-operator) goes into
// the pod's env vars for client-side scans when the iplane-id is not
// part of the filter.
//
// Base image. v0.1's design split (docs/design/0001-provisioner.md
// "Provisioner / deploy split on RunPod") says phase 1 provisions a
// Docker-capable base image; phase 2's deploy primitive docker-runs
// the engine container on top. The default base image is RunPod's
// PyTorch image; the operator can override via Spec.base_image.
//
// SSH readiness. Spawn returns State=ACTIVE as soon as RunPod
// acknowledges the pod, but the SSH endpoint may take another 20-60s
// to become reachable. ssh fields stay empty until a later Describe
// call populates them. Phase 2's deploy primitive polls Describe
// before attempting docker-run.
package runpod

import (
	"context"
	"fmt"
	"strings"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Defaults for fields that v0.1 does not expose on Spec but RunPod
// requires on the create input. Phase 1.4's CLI may eventually expose
// some of these (--container-disk, --volume); for now they are
// hardcoded.
const (
	defaultBaseImage         = "runpod/pytorch:2.4.0-py3.11-cuda12.4.1-devel-ubuntu22.04"
	defaultContainerDiskGB   = 20
	defaultVolumeGB          = 0
	defaultPortsString       = "22/tcp,8000/http"
	defaultCloudType         = "SECURE"
	podNamePrefix            = "iplane-"
	startSshOnDeploy         = true
)

// Provider implements provisioners.Provider for RunPod.
type Provider struct {
	client *Client
	clock  func() time.Time
}

// New builds a RunPod Provider on top of a configured Client. The
// client owns auth (api key), endpoint (default or test-overridden),
// and HTTP transport.
func New(client *Client) *Provider {
	return &Provider{
		client: client,
		clock:  time.Now,
	}
}

// Name satisfies provisioners.Provider. Constant value used by the
// Service to dispatch by spec.provider.
func (p *Provider) Name() string { return provisioners.ProviderRunPod }

// IsActiveProviderState satisfies provisioners.ActiveStateChecker.
// The Service's idempotency-adoption code path calls this to decide
// whether a List result counts as "this pod is up and ready to be
// adopted" without the central Service having to learn RunPod's state
// vocabulary. See skus.go for the actual set.
func (p *Provider) IsActiveProviderState(state string) bool {
	return isActiveProviderState(state)
}

// Spawn calls podFindAndDeployOnDemand. The Service has already
// validated spec and run the idempotency lookup; the adapter trusts
// that it is being asked to create a fresh pod.
func (p *Provider) Spawn(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	if spec == nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn", fmt.Errorf("spec is nil"), 0)
	}

	// Resolve class -> SKU when the operator did not supply a SKU
	// directly. The Service's validation already enforced the class/SKU
	// mutex, so this branch is unambiguous.
	gpuTypeId := spec.GetGpu().GetSku()
	resolvedClass := spec.GetGpu().GetClass()
	if gpuTypeId == "" {
		var err error
		gpuTypeId, err = resolveSKU(resolvedClass)
		if err != nil {
			return nil, provisioners.NewProviderError(p.Name(), "spawn", err, 0)
		}
	} else if resolvedClass == "" {
		resolvedClass = classifySKU(gpuTypeId)
	}
	gpuCount := int(spec.GetGpu().GetCount())
	if gpuCount <= 0 {
		gpuCount = 1
	}

	image := spec.GetBaseImage()
	if image == "" {
		image = defaultBaseImage
	}

	input := map[string]any{
		"name":              podNamePrefix + spec.GetId(),
		"imageName":         image,
		"gpuTypeId":         gpuTypeId,
		"gpuCount":          gpuCount,
		"containerDiskInGb": defaultContainerDiskGB,
		"volumeInGb":        defaultVolumeGB,
		"cloudType":         defaultCloudType,
		"dataCenterId":      spec.GetRegion(),
		"env":               tagsToEnvInput(spec.GetTags()),
		"ports":             defaultPortsString,
		"startSsh":          startSshOnDeploy,
	}

	var resp struct {
		PodFindAndDeployOnDemand *podWire `json:"podFindAndDeployOnDemand"`
	}
	if err := p.client.do(ctx, "spawn", spawnMutation, map[string]any{"input": input}, &resp); err != nil {
		return nil, err
	}
	if resp.PodFindAndDeployOnDemand == nil || resp.PodFindAndDeployOnDemand.ID == "" {
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("runpod returned empty pod (likely no capacity in region %q for gpu %q)", spec.GetRegion(), gpuTypeId), 0)
	}

	return p.podToInstance(resp.PodFindAndDeployOnDemand, spec, resolvedClass, gpuTypeId, gpuCount), nil
}

// Terminate calls podTerminate. Idempotent: a "not found" response is
// treated as success (the pod is already gone, which is the desired
// end state).
func (p *Provider) Terminate(ctx context.Context, providerID string) error {
	if providerID == "" {
		return provisioners.NewProviderError(p.Name(), "terminate", fmt.Errorf("providerID is empty"), 0)
	}
	err := p.client.do(ctx, "terminate", terminateMutation, map[string]any{
		"input": map[string]any{"podId": providerID},
	}, nil)
	if err != nil {
		var pe *provisioners.ProviderError
		if isWrappedNotFound(err) {
			return nil
		}
		_ = pe
		return err
	}
	return nil
}

// Describe calls the pod query for one pod id. Returns ErrNotFound
// wrapped in ProviderError when RunPod reports the pod does not exist.
func (p *Provider) Describe(ctx context.Context, providerID string) (*provisionerv1.Instance, error) {
	if providerID == "" {
		return nil, provisioners.NewProviderError(p.Name(), "describe", fmt.Errorf("providerID is empty"), 0)
	}
	var resp struct {
		Pod *podWire `json:"pod"`
	}
	if err := p.client.do(ctx, "describe", podQuery, map[string]any{"input": map[string]any{"podId": providerID}}, &resp); err != nil {
		return nil, err
	}
	if resp.Pod == nil {
		return nil, provisioners.NewProviderError(p.Name(), "describe", provisioners.ErrNotFound, 0)
	}
	tags := envSliceToMap(resp.Pod.Env)
	return p.podToInstance(resp.Pod, specFromPod(resp.Pod, tags), classifySKU(firstGPUSKU(resp.Pod)), firstGPUSKU(resp.Pod), int(resp.Pod.GPUCount)), nil
}

// List calls myself.pods. Server-side prefiltering uses PodsFilter.name
// when the iplane-id tag is in the filter; everything else is
// client-side scan over the env vars.
func (p *Provider) List(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error) {
	podsFilter := map[string]any{}
	if id, ok := filter[provisioners.TagID]; ok && id != "" {
		podsFilter["name"] = podNamePrefix + id
	}

	var resp struct {
		Myself struct {
			Pods []*podWire `json:"pods"`
		} `json:"myself"`
	}
	vars := map[string]any{}
	if len(podsFilter) > 0 {
		vars["input"] = podsFilter
	}
	if err := p.client.do(ctx, "list", listQuery, vars, &resp); err != nil {
		return nil, err
	}

	refs := make([]*provisionerv1.InstanceRef, 0, len(resp.Myself.Pods))
	for _, pod := range resp.Myself.Pods {
		podTags := envSliceToMap(pod.Env)
		if !matchesAllTags(podTags, filter) {
			continue
		}
		refs = append(refs, &provisionerv1.InstanceRef{
			ProviderId:    pod.ID,
			ProviderState: pod.DesiredStatus,
			Tags:          podTags,
			HourlyRateUsd: pod.CostPerHr,
			CreatedAt:     parseRunPodTime(pod.CreatedAt),
		})
	}
	return refs, nil
}

// podToInstance assembles the iplane Instance from RunPod's pod shape.
// resolvedClass and resolvedSKU are passed separately because Spawn
// knows them up-front from the input (we want them on the Instance
// even when RunPod's response omits them, which it sometimes does
// pre-runtime-ready).
func (p *Provider) podToInstance(pod *podWire, spec *provisionerv1.Spec, resolvedClass, resolvedSKU string, gpuCount int) *provisionerv1.Instance {
	createdAt := parseRunPodTime(pod.CreatedAt)
	now := timestamppb.New(p.clock())

	ssh := &provisionerv1.SshTarget{}
	if pod.IPAddress != nil && pod.IPAddress.IPAddress != "" {
		ssh.Host = pod.IPAddress.IPAddress
		ssh.Port = 22
		ssh.User = "root"
	}

	return &provisionerv1.Instance{
		Id:         spec.GetId(),
		ProviderId: pod.ID,
		Provider:   p.Name(),
		Spec:       spec,
		Region:     spec.GetRegion(),
		Gpu: &provisionerv1.GpuInfo{
			Class:  resolvedClass,
			Sku:    resolvedSKU,
			Count:  int32(gpuCount),
			VramGb: int32(firstGPUVRAM(pod)),
		},
		HourlyRateUsd: pod.CostPerHr,
		State:         provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		CreatedAt:     createdAt,
		ActivatedAt:   now,
		Ssh:           ssh,
	}
}

// tagsToEnvInput converts iplane tags to RunPod's EnvironmentVariableInput
// shape ([{key, value}]). Empty input maps to an empty slice rather
// than nil so the GraphQL serializer does not omit the field.
func tagsToEnvInput(tags map[string]string) []map[string]string {
	out := make([]map[string]string, 0, len(tags))
	for k, v := range tags {
		out = append(out, map[string]string{"key": k, "value": v})
	}
	return out
}

// envSliceToMap parses RunPod's pod.env response shape (["KEY=VALUE", ...])
// back into a flat tag map. Malformed entries (no "=") are dropped
// silently; this is best-effort recovery, not validation.
func envSliceToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		out[kv[:i]] = kv[i+1:]
	}
	return out
}

// matchesAllTags is the client-side filter for tags that the
// PodsFilter cannot cover server-side (anything other than iplane-id).
func matchesAllTags(podTags, want map[string]string) bool {
	for k, v := range want {
		if podTags[k] != v {
			return false
		}
	}
	return true
}

// specFromPod reconstructs an approximate Spec for an externally-found
// pod (one Describe returned but we have no local record for). Used
// only by the Service's adopt path; the result is good enough to
// stamp on the Instance, not authoritative.
func specFromPod(pod *podWire, tags map[string]string) *provisionerv1.Spec {
	return &provisionerv1.Spec{
		Id:        tags[provisioners.TagID],
		Provider:  provisioners.ProviderRunPod,
		Region:    pod.MachineID, // best-effort, not always populated
		BaseImage: pod.ImageName,
		Tags:      tags,
		Gpu: &provisionerv1.GpuSpec{
			Sku:   firstGPUSKU(pod),
			Count: int32(pod.GPUCount),
		},
	}
}

func firstGPUSKU(pod *podWire) string {
	if len(pod.GPUs) > 0 {
		return pod.GPUs[0].ID
	}
	return ""
}

func firstGPUVRAM(pod *podWire) int {
	if len(pod.GPUs) > 0 {
		return pod.GPUs[0].MemoryInGB
	}
	return 0
}

func parseRunPodTime(s string) *timestamppb.Timestamp {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339, s); err2 == nil {
			return timestamppb.New(t2)
		}
		return nil
	}
	return timestamppb.New(t)
}

// isWrappedNotFound peeks through *ProviderError to detect ErrNotFound
// wrapped by the graphql layer's isNotFoundMessage check. Used by
// Terminate for idempotent return-nil-on-already-gone.
func isWrappedNotFound(err error) bool {
	for e := err; e != nil; {
		if e == provisioners.ErrNotFound {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := e.(unwrapper); ok {
			e = u.Unwrap()
			continue
		}
		return false
	}
	return false
}

// podWire is the subset of RunPod's Pod response fields the adapter
// actually consumes. We deliberately do NOT bind every field RunPod
// returns (there are ~50) -- only the ones that flow through to
// Instance or get round-tripped to env.
type podWire struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	ImageName     string         `json:"imageName"`
	MachineID     string         `json:"machineId"`
	CostPerHr     float64        `json:"costPerHr"`
	CreatedAt     string         `json:"createdAt"`
	DesiredStatus string         `json:"desiredStatus"`
	Env           []string       `json:"env"`
	GPUCount      int            `json:"gpuCount"`
	GPUs          []gpuWire      `json:"gpus"`
	IPAddress     *ipAddressWire `json:"ipAddress"`
	Ports         string         `json:"ports"`
}

type gpuWire struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	MemoryInGB  int    `json:"memoryInGb"`
}

type ipAddressWire struct {
	IPAddress string `json:"ipAddress"`
}

// GraphQL queries the adapter sends. Kept as plain string constants
// (no codegen) because there are only four of them and inlining them
// alongside the adapter keeps the wire-shape changes in one place.
const spawnMutation = `mutation Spawn($input: PodFindAndDeployOnDemandInput!) {
  podFindAndDeployOnDemand(input: $input) {
    id
    name
    imageName
    machineId
    costPerHr
    createdAt
    desiredStatus
    env
    gpuCount
    gpus { id displayName memoryInGb }
    ipAddress { ipAddress }
    ports
  }
}`

const terminateMutation = `mutation Terminate($input: PodTerminateInput!) {
  podTerminate(input: $input)
}`

const podQuery = `query Pod($input: PodFilter!) {
  pod(input: $input) {
    id
    name
    imageName
    machineId
    costPerHr
    createdAt
    desiredStatus
    env
    gpuCount
    gpus { id displayName memoryInGb }
    ipAddress { ipAddress }
    ports
  }
}`

const listQuery = `query List($input: PodsFilter) {
  myself {
    pods(input: $input) {
      id
      name
      imageName
      costPerHr
      createdAt
      desiredStatus
      env
      gpuCount
      gpus { id displayName memoryInGb }
    }
  }
}`

// Compile-time check.
var _ provisioners.Provider = (*Provider)(nil)
