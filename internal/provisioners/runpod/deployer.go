package runpod

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	skhttp "github.com/panyam/servicekit/http"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// Provider implements the Deployer capability via the image-as-pod
// model: the deployment.image is launched directly as a new RunPod
// pod (no docker-in-docker, no SSH config-push, no OS-layer
// orchestration). The pod IS the engine container.
//
// This is the v0.1 default path for RunPod and the recommended shape
// for any image-native provider (Vast.ai, Modal, Replicate, Vertex
// AI, SageMaker). VM-style providers (Lambda Labs, raw AWS GPU
// instances) fall back to the sshdocker.Executor via the Service's
// capability dispatch.
//
// Why not docker-in-pod: every base image becomes an OS-compat
// surface (apt vs apk, systemd vs sysvinit, privileged vs not).
// Auto-installing docker on a random base image fails for any number
// of reasons (no apt, no privileged container, no cgroups access).
// Image-as-pod sidesteps the whole matrix -- the operator picks an
// engine image that runs as-is on the provider's container runtime.

// Deploy spawns a new RunPod pod whose container IS the engine image,
// then watches the pod's lifecycle + the engine's HTTP /health until
// it reaches a terminal state.
//
// emit fires on every transition. Caller (the Service) patches the
// state file from the updates.
func (p *Provider) Deploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	if dep == nil || inst == nil {
		return fmt.Errorf("runpod.Deploy: deployment and instance are required")
	}
	if dep.GetImage() == "" {
		return failedf(emit, "validate", fmt.Errorf("deployment.image is required for the image-as-pod model"))
	}

	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING,
		Phase:           "runpod:create-pod",
		ProgressMessage: fmt.Sprintf("spawning pod with image %s", dep.GetImage()),
	})

	podReq, err := buildEnginePodRequest(dep, inst)
	if err != nil {
		return failedf(emit, "runpod:create-pod", err)
	}

	req, err := p.client.newReq("POST", "/pods", nil, podReq)
	if err != nil {
		return failedf(emit, "runpod:create-pod", fmt.Errorf("build request: %w", err))
	}
	created, err := skhttp.Call[createPodResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return failedf(emit, "runpod:create-pod", wrapErr("deploy", err))
	}
	if created.ID == "" {
		return failedf(emit, "runpod:create-pod",
			fmt.Errorf("runpod returned empty pod id (likely no capacity for sku %q)", inst.GetGpu().GetSku()))
	}

	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
		Phase:           "runpod:scheduling",
		ProgressMessage: "pod created; waiting for public IP + engine /health",
		ContainerID:     created.ID,
	})

	// Wait for the engine to be reachable. publicIp + port-mapped 22
	// (for debug ssh) AND the engine port reachable via HTTP /health.
	endpoint, err := p.waitForEngineReady(ctx, created.ID, dep.GetEnginePort(), emit)
	if err != nil {
		return failedf(emit, "runpod:engine-ready", err)
	}

	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		Phase:           "engine:serving",
		ProgressMessage: "engine /health is 2xx",
		ContainerID:     created.ID,
		EngineEndpoint:  endpoint,
	})
	return nil
}

// Destroy terminates the engine pod via DELETE /pods/{id}.
// Idempotent: 404 from RunPod is treated as success (already gone).
func (p *Provider) Destroy(ctx context.Context, dep *provisionerv1.Deployment, _ *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	podID := dep.GetContainerId()
	if podID == "" {
		// Nothing to do server-side; just transition.
		emit(provisioners.DeployStateUpdate{
			State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
			Phase:           "runpod:terminate",
			ProgressMessage: "no pod id on record; nothing to terminate",
		})
		return nil
	}

	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING,
		Phase:           "runpod:terminate",
		ProgressMessage: fmt.Sprintf("terminating pod %s", podID),
	})

	if err := p.Terminate(ctx, podID); err != nil {
		return failedf(emit, "runpod:terminate", err)
	}

	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
		Phase:           "runpod:terminate",
		ProgressMessage: "pod terminated",
	})
	return nil
}

// buildEnginePodRequest assembles the createPodRequest for the engine
// pod. The instance's GPU SKU is inherited (the operator already
// picked the class when provisioning the instance record); the
// engine image + model + env come from the Deployment.
func buildEnginePodRequest(dep *provisionerv1.Deployment, inst *provisionerv1.Instance) (createPodRequest, error) {
	// SKU resolution, two cases:
	//   - explicit instance: the GPU was already resolved at instance
	//     create; reuse inst.Gpu.Sku.
	//   - auto-provisioned instance: the instance is a PENDING shell
	//     carrying Spec.Requirements; resolve the cheapest matching SKU
	//     here (same MatchSKUs path Spawn uses).
	gpuSKU := inst.GetGpu().GetSku()
	if gpuSKU == "" {
		reqs := inst.GetSpec().GetRequirements()
		if reqs == nil {
			return createPodRequest{}, fmt.Errorf("instance has neither a resolved GPU SKU nor resource requirements to resolve one")
		}
		if sku := reqs.GetSku(); sku != "" {
			gpuSKU = sku
		} else if matches := MatchSKUs(reqs); len(matches) > 0 {
			gpuSKU = matches[0]
		} else {
			return createPodRequest{}, fmt.Errorf("no runpod SKU satisfies the deployment's requirements (min_vram_gb=%d)", reqs.GetMinVramGb())
		}
	}

	enginePort := dep.GetEnginePort()
	if enginePort == 0 {
		enginePort = 8000
	}

	// Build the env block. v0.1 just passes the operator-supplied env
	// through. The engine image is responsible for honoring whatever
	// convention it has (MODEL=..., HF_TOKEN=..., etc).
	env := make(map[string]string, len(dep.GetEnv()))
	for k, v := range dep.GetEnv() {
		env[k] = v
	}

	// Build the docker args. vLLM (and most OpenAI-compat engines)
	// take `--model X --host 0.0.0.0 --port N` plus the operator's
	// custom args. The image's ENTRYPOINT is the engine binary;
	// dockerArgs is appended after it.
	args := []string{
		"--model", dep.GetModel(),
		"--host", "0.0.0.0",
		"--port", fmt.Sprintf("%d", enginePort),
	}
	args = append(args, dep.GetEngineArgs()...)

	// GPU count: from the resolved instance if present, else the
	// requirements, else 1.
	gpuCount := int(inst.GetGpu().GetCount())
	if gpuCount <= 0 {
		gpuCount = int(inst.GetSpec().GetRequirements().GetGpuCount())
	}
	if gpuCount <= 0 {
		gpuCount = 1
	}

	return createPodRequest{
		Name:              "iplane-engine-" + dep.GetId(),
		ImageName:         dep.GetImage(),
		GPUTypeIDs:        []string{gpuSKU},
		GPUCount:          gpuCount,
		ContainerDiskInGB: defaultContainerDiskGB,
		VolumeInGB:        defaultVolumeGB,
		ComputeType:       defaultComputeType,
		Ports: []string{
			fmt.Sprintf("%d/http", enginePort),
			"22/tcp",
		},
		Env:        env,
		DockerArgs: strings.Join(args, " "),
	}, nil
}

// waitForEngineReady polls GET /pods/{id} for publicIp + portMapping,
// then HTTP-polls the engine's /health endpoint until 2xx. Returns
// the engine endpoint URL on success.
func (p *Provider) waitForEngineReady(ctx context.Context, podID string, enginePort int32, emit func(provisioners.DeployStateUpdate)) (string, error) {
	if enginePort == 0 {
		enginePort = 8000
	}
	timeout := p.sshReadyTimeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	interval := p.sshReadyInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	deadline := time.Now().Add(timeout)

	// Stage 1: wait for publicIp.
	var pod podBody
	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("publicIp not assigned within %s", timeout)
		}
		getReq, err := p.client.newReq("GET", "/pods/"+podID, nil, nil)
		if err != nil {
			return "", fmt.Errorf("build get request: %w", err)
		}
		fresh, err := skhttp.Call[podBody](ctx, getReq, p.client.callOpts()...)
		if err == nil && fresh.PublicIP != "" {
			pod = fresh
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}

	// Resolve the externally-dialable engine port via the NAT mapping
	// (RunPod often NATs container:8000 to a random external port).
	externalPort := enginePort
	if mapped, ok := pod.PortMappings[fmt.Sprintf("%d", enginePort)]; ok && mapped > 0 {
		externalPort = int32(mapped)
	}
	endpoint := fmt.Sprintf("http://%s:%d", pod.PublicIP, externalPort)

	// Stage 2: HTTP-poll the engine's /health.
	healthURL := endpoint + "/health"
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return endpoint, ctx.Err()
			case <-time.After(interval):
			}
		}
		first = false
		if time.Now().After(deadline) {
			return endpoint, fmt.Errorf("engine /health not reachable within %s", timeout)
		}
		ok, err := httpProbeHealth(ctx, healthURL)
		if ok {
			return endpoint, nil
		}
		msg := "polling engine /health (not 2xx yet)"
		if err != nil {
			msg = fmt.Sprintf("polling engine /health: %v", err)
		}
		emit(provisioners.DeployStateUpdate{
			State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
			Phase:           "engine:waiting",
			ProgressMessage: msg,
			ContainerID:     podID,
		})
	}
}

// httpProbeHealth dials the endpoint with a tight timeout. Returns
// (true, nil) on 2xx, (false, nil) on connect-but-not-2xx, (false,
// err) on real failures.
func httpProbeHealth(ctx context.Context, url string) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode/100 == 2, nil
}

// failedf emits a FAILED state update with the wrapped error reason
// and returns the wrapped error for the caller's return. Mirrors the
// shape of sshdocker.Executor's failed helper so the two paths look
// the same from the Service's perspective.
func failedf(emit func(provisioners.DeployStateUpdate), phase string, err error) error {
	emit(provisioners.DeployStateUpdate{
		State:         provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
		Phase:         phase,
		FailureReason: fmt.Sprintf("%s: %v", phase, err),
	})
	return fmt.Errorf("%s: %w", phase, err)
}

// Compile-time check: *Provider satisfies the Deployer capability.
var _ provisioners.Deployer = (*Provider)(nil)
