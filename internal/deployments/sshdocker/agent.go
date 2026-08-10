package sshdocker

import (
	"context"
	"fmt"
	"strings"

	"github.com/inference-book/inference-plane/internal/engineagent"
)

// AgentContainerPrefix names the registration agent's container. Distinct
// from the engine's so `docker ps` on the box tells an operator which
// process is which, and so teardown can remove one without touching the
// other.
const AgentContainerPrefix = "iplane-agent-"

// AgentContainerName returns the agent sidecar's container name for a
// deployment id.
func AgentContainerName(deploymentID string) string {
	return AgentContainerPrefix + deploymentID
}

// AgentSpec is what RunAgent needs to start the sidecar.
type AgentSpec struct {
	// DeploymentID names both containers.
	DeploymentID string
	// Image is reused from the engine, deliberately. See RunAgent.
	Image string
	// Env carries the injected identity the deploy path stamped.
	Env map[string]string
}

// RunAgent starts the registration agent as a sidecar container beside the
// engine.
//
// # Why this path gets a sidecar and the image-native path does not
//
// A RunPod pod is one container, so the agent there has to be wrapped into
// the engine's own entrypoint, which means iplane has to be told what that
// entrypoint is (Deployment.engine_entrypoint). Here there is a real docker
// daemon on the host, so the agent gets its own container and **the engine
// container is not touched at all**. No entrypoint override on the engine,
// no wrapper, nothing for the operator to supply.
//
// Two details make it work:
//
//   - `--network container:<engine>` puts the agent in the engine's network
//     namespace, so the agent's health probe reaches the engine on
//     127.0.0.1 exactly as it would from inside. That is what preserves the
//     property the whole design rests on: readiness is observed locally, so
//     ASSEMBLING is visible before any endpoint is externally reachable.
//   - the **engine's own image** is reused rather than pulling a small one.
//     It is already on the host, so the sidecar costs no extra pull on a
//     box that just spent minutes fetching a multi-GB engine image, and
//     `--entrypoint` replaces whatever the image would have run. Nothing
//     about the image's contents matters beyond having a shell.
//
// Failure is the caller's to swallow. An engine that serves tokens must not
// be torn down because its agent would not start.
func (d *Docker) RunAgent(ctx context.Context, spec AgentSpec) (string, error) {
	if spec.DeploymentID == "" || spec.Image == "" {
		return "", fmt.Errorf("docker run agent: deployment id and image are required")
	}
	if spec.Env[engineagent.EnvEngineID] == "" || spec.Env[engineagent.EnvServiceURL] == "" {
		return "", fmt.Errorf("docker run agent: no agent identity stamped; nothing to register as")
	}
	url, ok := engineagent.BinaryURL("amd64")
	if !ok {
		return "", fmt.Errorf("docker run agent: no published agent binary for this build (set %s to override)",
			engineagent.EnvBinaryURL)
	}

	script, err := engineagent.AgentScript(url)
	if err != nil {
		return "", err
	}

	name := AgentContainerName(spec.DeploymentID)
	engine := ContainerName(spec.DeploymentID)

	var args []string
	args = append(args, "docker run -d")
	args = append(args, "--name", shellEscape(name))
	// Share the engine's network namespace so 127.0.0.1 is the engine.
	// This also means the sidecar publishes no ports of its own.
	args = append(args, "--network", shellEscape("container:"+engine))
	args = append(args, "--restart", "unless-stopped")
	args = append(args, "--label", shellEscape("iplane.deployment="+spec.DeploymentID))
	args = append(args, "--label", shellEscape("iplane.role=agent"))
	args = append(args, "--entrypoint", "/bin/sh")
	for k, v := range spec.Env {
		args = append(args, "-e", shellEscape(k+"="+v))
	}
	args = append(args, shellEscape(spec.Image))
	args = append(args, "-c", shellEscape(script))

	stdout, stderr, code, err := d.r.Run(ctx, strings.Join(args, " "))
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("docker run agent: exit %d: %s", code, strings.TrimSpace(string(stderr)))
	}
	return strings.TrimSpace(string(stdout)), nil
}

// StopAgent stops and removes the agent sidecar.
//
// Errors are returned but are safe for the caller to ignore: Stop and
// Remove already treat "no such container" as success, so a real error here
// means the box is unreachable, which the engine teardown running next will
// surface anyway. Leaving the sidecar behind while the engine goes away
// would leave a container renewing a lease for an engine that no longer
// exists, which is why this runs first.
func (d *Docker) StopAgent(ctx context.Context, deploymentID string) error {
	name := AgentContainerName(deploymentID)
	if err := d.Stop(ctx, name); err != nil {
		return err
	}
	return d.Remove(ctx, name)
}
