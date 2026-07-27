package runpod

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners"
	skhttp "github.com/panyam/servicekit/http"
)

// Model staging: download a model onto a network volume so warm-cache
// deploys mount it instead of re-downloading. StageModel spins a
// throwaway CPU pod that mounts the volume, runs huggingface-cli
// download into the volume's HF cache, and exits; the pod is deleted
// once the download finishes. This backs the StageModel half of the
// VolumeManager capability.

const (
	// defaultStageImage is a small Python image the staging pod runs.
	// huggingface_hub[cli] is pip-installed at start (a few seconds)
	// rather than baked, so no custom image is needed.
	defaultStageImage = "python:3.11-slim"

	// stageComputeType asks RunPod for a CPU-only pod -- staging is a
	// pure download, so paying for a GPU would be waste.
	stageComputeType = "CPU"

	stagePollInterval = 10 * time.Second
	stageTimeout      = 30 * time.Minute
)

// StageModel satisfies provisioners.VolumeManager. It blocks until the
// download completes or stageTimeout elapses.
func (p *Provider) StageModel(ctx context.Context, spec provisioners.StageSpec) error {
	mountPath := spec.MountPath
	if mountPath == "" {
		mountPath = "/models"
	}
	env := map[string]string{
		// HF_HOME under the mount is exactly where volumecache points the
		// engine's HF_HOME at deploy time, so the layout lines up.
		"HF_HOME": mountPath + "/hf",
	}
	if spec.HFToken != "" {
		env["HF_TOKEN"] = spec.HFToken
	}

	// pip-install the CLI, then download. --exclude of the safetensors
	// index is avoided; a full snapshot is what the engine loads.
	cmd := []string{
		"bash", "-lc",
		fmt.Sprintf("set -euo pipefail; pip install -q -U 'huggingface_hub[cli]' && huggingface-cli download %s", spec.Model),
	}

	createBody := createPodRequest{
		Name:            "iplane-stage-" + volumeShortID(spec.VolumeID),
		ImageName:       defaultStageImage,
		ComputeType:     stageComputeType,
		NetworkVolumeID: spec.VolumeID,
		VolumeMountPath: mountPath,
		DataCenterIDs:   []string{spec.Region},
		Env:             env,
		DockerStartCmd:  cmd,
	}
	req, err := p.client.newReq("POST", "/pods", nil, createBody)
	if err != nil {
		return wrapErr("stage model", err)
	}
	created, err := skhttp.Call[createPodResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return wrapErr("stage model", err)
	}

	// Always tear the staging pod down, success or failure -- it is a
	// throwaway and would otherwise bill idle after the download exits.
	defer func() { _ = p.Terminate(context.WithoutCancel(ctx), created.ID) }()

	if err := p.waitForStageComplete(ctx, created.ID); err != nil {
		return err
	}
	return nil
}

// waitForStageComplete polls the staging pod until its container exits.
// A finished dockerStartCmd stops the pod, so a terminal desiredStatus
// signals the download is done; a "FAILED" status means the download
// errored. The exact terminal-status strings RunPod reports for a pod
// whose CMD exited are isolated in isStageComplete / isStageFailed.
//
// VALIDATE against a live RunPod run: confirm the desiredStatus values a
// completed / failed staging pod reports and adjust the predicates. The
// poll/terminate control flow is exercised by the fake harness
// independently of the exact strings.
func (p *Provider) waitForStageComplete(ctx context.Context, podID string) error {
	interval := p.stageInterval
	if interval <= 0 {
		interval = stagePollInterval
	}
	budget := p.stageBudget
	if budget <= 0 {
		budget = stageTimeout
	}
	deadline := time.Now().Add(budget)
	for {
		getReq, err := p.client.newReq("GET", "/pods/"+podID, nil, nil)
		if err != nil {
			return wrapErr("stage model: poll", err)
		}
		pod, err := skhttp.Call[podBody](ctx, getReq, p.client.callOpts()...)
		if err != nil {
			return wrapErr("stage model: poll", err)
		}
		switch {
		case isStageFailed(pod.DesiredStatus):
			return fmt.Errorf("stage model: staging pod %s failed (status %q)", podID, pod.DesiredStatus)
		case isStageComplete(pod.DesiredStatus):
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("stage model: timed out after %s waiting for download (last status %q)", budget, pod.DesiredStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// isStageComplete reports whether a staging pod's container has exited
// cleanly (the download finished). VALIDATE the exact strings live.
func isStageComplete(desiredStatus string) bool {
	switch strings.ToUpper(desiredStatus) {
	case "EXITED", "STOPPED", "TERMINATED", "COMPLETED":
		return true
	}
	return false
}

// isStageFailed reports whether a staging pod's container errored out.
// VALIDATE the exact strings live.
func isStageFailed(desiredStatus string) bool {
	return strings.EqualFold(desiredStatus, "FAILED")
}

// volumeShortID derives a pod-name-safe suffix from a volume id.
func volumeShortID(volumeID string) string {
	id := strings.ToLower(volumeID)
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}
