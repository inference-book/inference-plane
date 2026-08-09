package runpod

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners"
	skhttp "github.com/panyam/servicekit/http"
)

// Model staging: download a model onto a network volume so warm-cache
// deploys mount it instead of re-downloading. StageModel spins a
// throwaway CPU pod that mounts the volume and runs `hf download`
// into the volume's HF cache. The download command prints a
// sentinel line on completion, which the poller reads back out of the
// pod's log stream (RunPod exposes no pod-status signal that a one-shot
// command finished); the pod is deleted once the marker appears. This
// backs the StageModel half of the VolumeManager capability.

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

	// logsReadTimeout bounds one poll's read of the (streaming) logs
	// endpoint: the tail backfill arrives at once, then the SSE stream
	// stays open, so we read for this long and cut it off. Only bites
	// against a real stream; the test fake closes the body at EOF.
	logsReadTimeout = 6 * time.Second

	// Staging-completion markers the download command prints to stdout,
	// read back out of the pod logs. RunPod gives no pod-status signal
	// that a one-shot command finished (the pod stays RUNNING and the
	// container restarts), so the command reports its own outcome and we
	// scan the log stream for it. Deliberately unlikely to collide with
	// pip / hf output.
	stageSentinelDone = "__IPLANE_STAGE_DONE__"
	stageSentinelFail = "__IPLANE_STAGE_FAIL__"
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

	// pip-install the CLI, then download a full snapshot (the engine
	// loads the whole thing). The command reports its own outcome as a
	// sentinel line -- there is no pod-status signal for "one-shot
	// command finished" -- then sleeps so RunPod does not restart the
	// exited container and re-run the download; the poller terminates
	// the pod once it reads the marker.
	// `hf download`, not the old `huggingface-cli download`: current
	// huggingface_hub removed the huggingface-cli entrypoint (it prints a
	// deprecation notice and exits non-zero), and the CLI is now `hf`.
	//
	// --max-workers 2: the CPU staging pod has little RAM (~4 GB), and the
	// default 8 parallel workers streaming multi-GB shards through the FUSE
	// volume mount OOM-kill the download (SIGKILL / rc 137) on a large
	// model. Throttling concurrency keeps it under the RAM ceiling. A small
	// model is unaffected; a 70B+ needs it.
	cmd := []string{
		"bash", "-lc",
		fmt.Sprintf(
			"if pip install -q -U 'huggingface_hub[cli]' && hf download %s --max-workers 2; then echo %s; else echo %s; fi; sleep infinity",
			spec.Model, stageSentinelDone, stageSentinelFail,
		),
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

// waitForStageComplete polls the staging pod's logs until the download
// command prints its completion marker. RunPod exposes no pod-status
// signal that a one-shot command finished -- v1 keeps desiredStatus at
// RUNNING, and even v2's status stays RUNNING because the exited
// container is restarted (confirmed live 2026-07-28). So the command
// echoes stageSentinelDone / stageSentinelFail and we read it back out
// of the v2 pod-logs stream.
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
		logs, err := p.fetchStageLogs(ctx, podID)
		// A transient logs-fetch error (stream hiccup, pod not yet
		// emitting) is not fatal: keep polling until the deadline.
		if err == nil {
			switch done, failed := stageSignal(logs); {
			case failed:
				return fmt.Errorf("stage model: staging pod %s reported a download failure (%s in logs)", podID, stageSentinelFail)
			case done:
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("stage model: timed out after %s waiting for the %s marker in staging logs", budget, stageSentinelDone)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// fetchStageLogs reads a bounded chunk of the staging pod's v2 log
// stream. The endpoint is Server-Sent Events: the tail backfill arrives
// immediately and the stream then stays open, so we read under a short
// deadline and cut it off, scanning whatever arrived. A deadline while
// reading the still-open stream is expected, not an error.
func (p *Provider) fetchStageLogs(ctx context.Context, podID string) ([]byte, error) {
	readCtx, cancel := context.WithTimeout(ctx, logsReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(readCtx, http.MethodGet, p.client.v2LogsURL(podID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.client.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	httpClient := http.DefaultClient
	if p.client.httpClient != nil {
		httpClient = p.client.httpClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Logs not available yet (pod still scheduling); treat as empty.
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("stage model: logs HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// io.ReadAll returns whatever was read before the deadline cancels
	// the stream, plus the deadline error -- keep the bytes, drop the err.
	body, _ := io.ReadAll(resp.Body)
	return body, nil
}

// stageSignal scans staging-pod log bytes for the completion markers the
// download command prints. Failure is checked first so a failed run is
// never mistaken for success.
func stageSignal(logs []byte) (done, failed bool) {
	if bytes.Contains(logs, []byte(stageSentinelFail)) {
		return false, true
	}
	if bytes.Contains(logs, []byte(stageSentinelDone)) {
		return true, false
	}
	return false, false
}

// volumeShortID derives a pod-name-safe suffix from a volume id.
func volumeShortID(volumeID string) string {
	id := strings.ToLower(volumeID)
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}
