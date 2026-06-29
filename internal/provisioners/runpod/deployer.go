package runpod

import (
	"context"
	"errors"
	"fmt"
	"net"
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
			fmt.Errorf("runpod returned empty pod id (likely no capacity for any of skus %v)", podReq.GPUTypeIDs))
	}

	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
		Phase:           "runpod:scheduling",
		ProgressMessage: "pod created; waiting for engine /health via proxy URL",
		ContainerID:     created.ID,
	})

	// Wait for the engine to be reachable via RunPod's HTTPS proxy.
	// The proxy URL is deterministic from the pod id -- no Stage-1
	// publicIp poll needed, no NAT port lookup. The proxy 502s while
	// the container boots, then 200s once the engine answers.
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
//
// Pod ID lookup order:
//
//  1. dep.container_id -- the v0.1 1:1 shape (singular Instance ==
//     Deployment, pod id stamped on the Deployment record).
//  2. inst.provider_id -- the v0.2 multi-replica auto-provision shape
//     (each replica has its own Instance, pod id lives on the Instance;
//     dep.container_id is null per fanout.patchDeploymentSlot's
//     "reserved for singular" comment).
//
// Before the inst fallback existed, multi-replica destroys silently
// no-op'd because dep.container_id was always null and the pod stayed
// alive on RunPod -- a state-vs-reality leak the operator had no way
// to see without checking the RunPod console.
func (p *Provider) Destroy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	podID := dep.GetContainerId()
	if podID == "" && inst != nil {
		podID = inst.GetProviderId()
	}
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
		// Transient errors (network timeout, 5xx) leave the deployment
		// in TERMINATING with a failure_reason. The reaper sweeps
		// TERMINATING deployments whose updated_at is stale and retries
		// destroy -- a single network blip doesn't permanently strand
		// the pod. Permanent errors (auth, 4xx other than 404) go to
		// FAILED for operator action.
		if isTransientTerminateErr(err) {
			emit(provisioners.DeployStateUpdate{
				State:         provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING,
				Phase:         "runpod:terminate",
				FailureReason: fmt.Sprintf("runpod:terminate (transient, will retry): %v", err),
			})
			return fmt.Errorf("runpod:terminate (transient): %w", err)
		}
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
	// SKU resolution. RunPod's `gpuTypeIds` accepts a SET of acceptable
	// SKUs and lets the platform pick one with capacity, so we hand it
	// the full cheapest-first match list (with `gpuTypePriority=AVAILABILITY`
	// as a tie-breaker hint). Three cases:
	//   - explicit instance with a resolved SKU: pin to it (the
	//     operator picked this exact GPU at instance-create time).
	//   - auto-provisioned instance with reqs.Sku set: also pin
	//     (operator forced an exact SKU via --sku).
	//   - auto-provisioned instance with class / min-vram: pass the
	//     whole MatchSKUs list so RunPod can route to any SKU with
	//     free capacity, not just the cheapest. Mitigates the common
	//     "no capacity on A5000 right now" 500.
	var gpuSKUs []string
	if sku := inst.GetHardware().GetGpuSku(); sku != "" {
		gpuSKUs = []string{sku}
	} else {
		reqs := inst.GetSpec().GetRequirements()
		if reqs == nil {
			return createPodRequest{}, fmt.Errorf("instance has neither a resolved GPU SKU nor resource requirements to resolve one")
		}
		if sku := reqs.GetSku(); sku != "" {
			gpuSKUs = []string{sku}
		} else if matches := MatchSKUs(reqs); len(matches) > 0 {
			gpuSKUs = matches
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

	// Build the docker CMD. vLLM (and most OpenAI-compat engines) take
	// `--model X --host 0.0.0.0 --port N` plus the operator's custom
	// args. The image ENTRYPOINT is the engine binary; we REPLACE the
	// image CMD with this argv via RunPod's dockerStartCmd field. The
	// legacy single-string `dockerArgs` was removed from RunPod's REST
	// schema; argv tokens go on the wire as a JSON array.
	cmd := []string{
		"--model", dep.GetModel(),
		"--host", "0.0.0.0",
		"--port", fmt.Sprintf("%d", enginePort),
	}
	cmd = append(cmd, dep.GetEngineArgs()...)

	// GPU count: from the resolved instance if present, else the
	// requirements, else 1.
	gpuCount := int(inst.GetHardware().GetGpuCount())
	if gpuCount <= 0 {
		gpuCount = int(inst.GetSpec().GetRequirements().GetGpuCount())
	}
	if gpuCount <= 0 {
		gpuCount = 1
	}

	return createPodRequest{
		Name:              "iplane-engine-" + dep.GetId(),
		ImageName:         dep.GetImage(),
		GPUTypeIDs:        gpuSKUs,
		GPUTypePriority:   "availability",
		GPUCount:          gpuCount,
		ContainerDiskInGB: defaultContainerDiskGB,
		VolumeInGB:        defaultVolumeGB,
		ComputeType:       defaultComputeType,
		// RunPod port suffixes:
		//   /http -> reverse-proxied at
		//            https://<pod-id>-<port>.proxy.runpod.net.
		//            No publicIp needed; absent from portMappings.
		//            Free; works on community capacity.
		//   /tcp  -> NAT-mapped onto pod.publicIp; appears in
		//            pod.portMappings[<internal>] = <external>.
		//            Needs supportPublicIp; bills extra; restricts
		//            placement to publicIp-capable hosts.
		// Cost-aware default: engine port is /http (proxy URL). SSH is
		// /tcp but only added when the operator opts in via debug_shell
		// -- otherwise we'd allocate a publicIp the workload doesn't
		// need and lose the cheapest community capacity in the catalog.
		Ports:          enginePodPorts(enginePort, dep.GetDebugShell()),
		SupportPublicIP: dep.GetDebugShell(),
		Env:             env,
		DockerStartCmd:  cmd,
	}, nil
}

// enginePodPorts returns the Ports list for POST /pods. Engine port is
// always /http (proxy-routed); SSH is /tcp and only added when the
// operator asked for debug-shell access.
func enginePodPorts(enginePort int32, debugShell bool) []string {
	ports := []string{fmt.Sprintf("%d/http", enginePort)}
	if debugShell {
		ports = append(ports, "22/tcp")
	}
	return ports
}

// proxyEndpointForPod returns the deterministic RunPod proxy URL for
// a (pod, port) pair: https://<pod-id>-<port>.proxy.runpod.net. The
// proxy is wired up the moment the pod is scheduled (before the
// container is healthy) and 5xxs while the engine is still booting,
// 2xxs once /health answers. Exported via the waitForEngineReady
// constant so tests can override the base.
func proxyEndpointForPod(podID string, port int32) string {
	return fmt.Sprintf("https://%s-%d.proxy.runpod.net", podID, port)
}

// waitForEngineReady polls the engine's /health via the provider's
// HTTPS proxy URL (deterministic from the pod id -- no publicIp / NAT
// lookup needed). Returns the engine endpoint URL on success.
func (p *Provider) waitForEngineReady(ctx context.Context, podID string, enginePort int32, emit func(provisioners.DeployStateUpdate)) (string, error) {
	if enginePort == 0 {
		enginePort = 8000
	}
	// Engine-ready uses its own timeout: model load + image pull is
	// minutes, not the ~30s sshd allocation that sshReadyTimeout is
	// tuned for. The fallback applies only if the engine timeout
	// wasn't configured AND ssh timeout wasn't either (raw &Provider{}
	// construction); both production New() and WithSSHReadyWait set
	// engine-ready explicitly.
	timeout := p.engineReadyTimeout
	if timeout <= 0 {
		timeout = p.sshReadyTimeout // back-compat for raw constructions
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	interval := p.sshReadyInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)

	endpoint := proxyEndpointForPod(podID, enginePort)
	if p.proxyBaseURL != "" {
		// Test override -- inject a local httptest server in place of
		// proxy.runpod.net.
		endpoint = fmt.Sprintf("%s/%s-%d", strings.TrimRight(p.proxyBaseURL, "/"), podID, enginePort)
	}

	// HTTP-poll the engine's /health. Each tick re-emits a
	// progress_message that carries elapsed-time + last HTTP status (or
	// dial error) -- the only signal the operator gets during the cold
	// image pull + model load window. WatchDeployment fires on
	// progress_message changes so the CLI streams these to stdout.
	healthURL := endpoint + "/health"
	started := time.Now()
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
		elapsed := time.Since(started).Round(time.Second)
		ok, statusText, err := httpProbeHealth(ctx, healthURL)
		if ok {
			return endpoint, nil
		}
		var detail string
		switch {
		case err != nil:
			detail = fmt.Sprintf("dial error: %v", err)
		case statusText != "":
			detail = "HTTP " + statusText
		default:
			detail = "no response"
		}
		msg := fmt.Sprintf("waiting for engine /health (%s elapsed) -- %s", elapsed, detail)
		emit(provisioners.DeployStateUpdate{
			State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
			Phase:           "engine:waiting",
			ProgressMessage: msg,
			ContainerID:     podID,
		})
	}
}

// httpProbeHealth dials the endpoint with a tight timeout. Returns
// (true, "200 OK", nil) on 2xx,
// (false, "<status>", nil) on connect-but-not-2xx (e.g. "502 Bad Gateway"),
// (false, "", err) on real failures (DNS, refused, timeout).
func httpProbeHealth(ctx context.Context, url string) (bool, string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return false, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	return resp.StatusCode/100 == 2, resp.Status, nil
}

// isTransientTerminateErr returns true when a DELETE /pods/{id} error
// is the kind that's worth retrying (network timeout, 5xx, 408, 429).
// 4xx-other-than-404 -- and 404 is already swallowed upstream as
// "pod already gone" -- mean the operator's input is wrong, not the
// network: retrying would loop without making progress, so we mark
// FAILED and let the reaper give up.
//
// The reaper retries TERMINATING deployments (issue 165 / option B);
// the deployer signals "retry me" by leaving the deployment in
// TERMINATING and returning the error here.
func isTransientTerminateErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var pe *provisioners.ProviderError
	if errors.As(err, &pe) {
		if pe.HTTP == 0 {
			// No HTTP status = transport-level failure (DNS, dial
			// timeout, connection reset). Always transient.
			return true
		}
		// 5xx, plus 408 Request Timeout and 429 Too Many Requests
		// (RunPod returns 429 under bursts; backing off is the right
		// move, not giving up).
		return pe.HTTP >= 500 || pe.HTTP == 408 || pe.HTTP == 429
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
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
