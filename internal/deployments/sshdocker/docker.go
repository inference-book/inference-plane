package sshdocker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Container name iplane uses for deployments. Stable across creates
// + idempotent re-deploys; `docker inspect iplane-deployment-<id>`
// is the canonical "what's running" lookup.
const ContainerNamePrefix = "iplane-deployment-"

// ContainerName returns the docker container name for a deployment id.
func ContainerName(deploymentID string) string {
	return ContainerNamePrefix + deploymentID
}

// ContainerState is the subset of `docker inspect` output the executor
// needs to make drift-detection decisions. Each field maps to a
// JSON path in inspect output (see decodeInspect for the mapping).
type ContainerState struct {
	Exists  bool
	Running bool
	Image   string // the image ref docker recorded (may differ from
	// what the operator typed if a digest was resolved)
	Model string // recovered from container labels (Phase 2 uses
	// docker run --label iplane.model=<value> to round-trip)
	ContainerID string
	ExitCode    int    // valid when Exists && !Running
	Status      string // raw State.Status string for debug
}

// Matches returns true if this container is already running the
// desired (image, model). Drift detection is exact-match per the
// design doc; engine args / env / ports are pass-through and do
// NOT count as drift in v0.1.
func (c *ContainerState) Matches(image, model string) bool {
	if !c.Exists || !c.Running {
		return false
	}
	return c.Image == image && c.Model == model
}

// Docker wraps a RemoteRunner with typed docker CLI helpers. One
// instance per executor; reuses the underlying RemoteRunner across
// every command for a single deployment.
type Docker struct {
	r RemoteRunner
}

// NewDocker constructs a Docker wrapper.
func NewDocker(r RemoteRunner) *Docker {
	return &Docker{r: r}
}

// Inspect runs `docker inspect <name> --format=...` and parses the
// state into a ContainerState. If the container does not exist,
// returns ContainerState{Exists: false} with no error -- "not found"
// is a legitimate inspect outcome, not a failure mode.
func (d *Docker) Inspect(ctx context.Context, name string) (*ContainerState, error) {
	// Use the structured JSON output so parsing is robust to docker
	// version differences in the human-readable formatter.
	cmd := fmt.Sprintf("docker inspect %s", shellEscape(name))
	stdout, stderr, code, err := d.r.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		// docker inspect returns non-zero when the container does
		// not exist. The error message is "No such object: <name>"
		// on stderr. Treat that as not-found rather than a real
		// error.
		if strings.Contains(string(stderr), "No such object") || strings.Contains(string(stderr), "No such container") {
			return &ContainerState{Exists: false}, nil
		}
		return nil, fmt.Errorf("docker inspect %s: exit %d: %s", name, code, strings.TrimSpace(string(stderr)))
	}
	return decodeInspect(stdout)
}

// decodeInspect parses `docker inspect`'s JSON array output. We pull
// out the bits that drive drift decisions; everything else stays
// unread.
func decodeInspect(raw []byte) (*ContainerState, error) {
	var arr []struct {
		Id     string `json:"Id"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Running  bool   `json:"Running"`
			Status   string `json:"Status"`
			ExitCode int    `json:"ExitCode"`
		} `json:"State"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("decode docker inspect output: %w", err)
	}
	if len(arr) == 0 {
		return &ContainerState{Exists: false}, nil
	}
	c := arr[0]
	return &ContainerState{
		Exists:      true,
		Running:     c.State.Running,
		Image:       c.Config.Image,
		Model:       c.Config.Labels["iplane.model"],
		ContainerID: c.Id,
		ExitCode:    c.State.ExitCode,
		Status:      c.State.Status,
	}, nil
}

// Pull runs `docker pull <image>`. The remote box may take a while
// to download large engine images; the caller's ctx bounds the
// wait.
func (d *Docker) Pull(ctx context.Context, image string) error {
	cmd := fmt.Sprintf("docker pull %s", shellEscape(image))
	stdout, stderr, code, err := d.r.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("docker pull %s: exit %d: %s\n%s", image, code, strings.TrimSpace(string(stderr)), strings.TrimSpace(string(stdout)))
	}
	return nil
}

// RunSpec carries the docker-run arguments the executor builds for
// each deployment. Maps cleanly onto Deployment proto fields.
type RunSpec struct {
	Name      string
	Image     string
	Model     string // round-tripped via --label iplane.model
	EngineArgs []string
	Env       map[string]string
	Port      int32 // engine listen port; 0 means "use the engine's default"
}

// Run launches the container in detached mode with --gpus all,
// --restart unless-stopped, and the iplane labels needed for
// idempotent inspect lookups.
//
// Returns the container id on success. Engine arg passing convention:
// vLLM (and most OpenAI-compat engines) take their model + flags as
// command-line args AFTER the image; RunSpec.EngineArgs is appended
// verbatim, with the model passed via "--model <Model>" so the
// happy-path case "iplane deployment deploy --image vllm --model X"
// works without operators having to know the engine's flag set.
func (d *Docker) Run(ctx context.Context, spec RunSpec) (string, error) {
	if spec.Name == "" || spec.Image == "" {
		return "", errors.New("docker run: name and image are required")
	}

	var args []string
	args = append(args, "docker run -d")
	args = append(args, "--name", shellEscape(spec.Name))
	args = append(args, "--gpus all")
	args = append(args, "--restart", "unless-stopped")
	args = append(args, "--label", shellEscape("iplane.deployment="+spec.Name))
	if spec.Model != "" {
		args = append(args, "--label", shellEscape("iplane.model="+spec.Model))
	}
	if spec.Port > 0 {
		args = append(args, "-p", fmt.Sprintf("%d:%d", spec.Port, spec.Port))
	}
	for k, v := range spec.Env {
		args = append(args, "-e", shellEscape(k+"="+v))
	}
	args = append(args, shellEscape(spec.Image))
	if spec.Model != "" {
		args = append(args, "--model", shellEscape(spec.Model))
	}
	for _, a := range spec.EngineArgs {
		args = append(args, shellEscape(a))
	}

	cmd := strings.Join(args, " ")
	stdout, stderr, code, err := d.r.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("docker run: exit %d: %s\n%s", code, strings.TrimSpace(string(stderr)), strings.TrimSpace(string(stdout)))
	}
	// docker run -d prints the container id on stdout.
	return strings.TrimSpace(string(stdout)), nil
}

// Stop runs `docker stop <name>`. No-op (returns nil) if the
// container does not exist -- the desired end state is "stopped"
// and we should not error when it's already true.
func (d *Docker) Stop(ctx context.Context, name string) error {
	cmd := fmt.Sprintf("docker stop %s", shellEscape(name))
	_, stderr, code, err := d.r.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if code != 0 {
		if strings.Contains(string(stderr), "No such container") {
			return nil
		}
		return fmt.Errorf("docker stop %s: exit %d: %s", name, code, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// Remove runs `docker rm <name>`. Same idempotent treatment as Stop.
func (d *Docker) Remove(ctx context.Context, name string) error {
	cmd := fmt.Sprintf("docker rm %s", shellEscape(name))
	_, stderr, code, err := d.r.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if code != 0 {
		if strings.Contains(string(stderr), "No such container") {
			return nil
		}
		return fmt.Errorf("docker rm %s: exit %d: %s", name, code, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// Health hits the engine's /health endpoint via curl inside the
// remote box -- keeps the executor's network surface to just SSH;
// no operator-side port-forwarding needed.
//
// Returns (true, nil) on 2xx, (false, nil) on non-2xx with a
// readable status, and (false, err) on real failures (curl missing
// or network broken).
func (d *Docker) Health(ctx context.Context, port int32) (bool, error) {
	if port <= 0 {
		port = 8000
	}
	// -s suppresses progress, -o /dev/null discards body, -w "%{http_code}" emits
	// the HTTP status as the only stdout content. --max-time bounds the wait.
	cmd := fmt.Sprintf(`curl -s -o /dev/null -w "%%{http_code}" --max-time 5 http://localhost:%d/health`, port)
	stdout, stderr, code, err := d.r.Run(ctx, cmd)
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, fmt.Errorf("curl /health exit %d: %s", code, strings.TrimSpace(string(stderr)))
	}
	status := strings.TrimSpace(string(stdout))
	return strings.HasPrefix(status, "2"), nil
}

// shellEscape wraps a string in single quotes so the remote sh sees
// it as a single argument, escaping any embedded single quotes via
// the standard '\'' dance. We never accept binary or control chars
// in any of the strings we pass through (ids are DNS-safe per
// provisioners.ValidateID; image / model refs are operator-supplied
// strings that should not contain control chars in practice), but
// the escape keeps us defended against accidental shell-meaningful
// characters like $ and `.
func shellEscape(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Reference: provisionerv1 import sanity (we don't use Deployment
// directly here, but executor.go does and pulls these types via
// this package's imports, so keep it lint-clean).
var _ = provisionerv1.DeploymentState_DEPLOYMENT_STATE_UNSPECIFIED
