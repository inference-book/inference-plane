package cmd

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// telemetryCmd is the verb group for telemetry-side ergonomics. v0.1
// has one subcommand (`url`) that discovers the cloudflared quick-
// tunnel URL when the docker-compose `tunnel` profile is active. The
// group exists so future telemetry helpers (e.g., `iplane telemetry
// status`, `iplane telemetry test`) have a natural home.
var telemetryCmd = &cobra.Command{
	Use:   "telemetry",
	Short: "Telemetry helpers (tunnel URL discovery, etc.)",
	Long: `Group for telemetry-pipeline ergonomics.

Subcommands:
  url   -- print the cloudflared quick-tunnel URL exposed by the
           docker-compose 'tunnel' profile. Use this URL as
           --otel-endpoint when deploying so the remote engine
           ships OTLP to the local stack.`,
}

var telemetryTunnelService string

var telemetryURLCmd = &cobra.Command{
	Use:   "url",
	Short: "Print the cloudflared tunnel URL discovered from docker logs",
	Args:  cobra.NoArgs,
	Long: `Read the cloudflared service's logs and extract the
trycloudflare.com URL it printed at startup. Use this URL as
the OTLP endpoint for engine deployments:

    export IPLANE_OTEL_ENDPOINT=$(iplane telemetry url)
    iplane deployment deploy my-llama ...

Errors with an actionable message when the docker-compose 'tunnel'
profile isn't active (no cloudflared container running).

Skip this entirely if you're using a hosted sink like Grafana Cloud
Free -- just export IPLANE_OTEL_ENDPOINT to your provider's OTLP URL
directly.`,
	RunE: runTelemetryURL,
}

func runTelemetryURL(cmd *cobra.Command, _ []string) error {
	url, err := discoverCloudflaredURL(telemetryTunnelService)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), url)
	return nil
}

// trycloudflareURLPattern matches the URL cloudflared prints to
// stderr at startup. Format is `https://<words>-<words>-<words>-<words>.trycloudflare.com`
// surrounded by the box-art banner; we anchor on the host suffix.
var trycloudflareURLPattern = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// discoverCloudflaredURL invokes `docker logs <service>` and greps
// out the trycloudflare.com URL. Returns an actionable error when:
//   - docker isn't on PATH (no docker = no tunnel possible).
//   - the container isn't running (profile not active).
//   - the URL isn't in the logs yet (container started but
//     cloudflared hasn't printed the banner -- retry in a moment).
func discoverCloudflaredURL(service string) (string, error) {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return "", fmt.Errorf("`docker` not found on PATH (the cloudflared tunnel runs as a docker-compose service): %w", err)
	}
	// The compose project name is `inference-plane-v01` (from the
	// `name:` field at the top of deploy/docker-compose.yaml), so the
	// container name is `<project>-<service>-1`. We try the service
	// name first (compose v2 supports it as a shorthand), then fall
	// back to the explicit container name.
	candidates := []string{
		service,
		"inference-plane-v01-" + service + "-1",
	}
	var lastErr error
	for _, name := range candidates {
		out, err := exec.Command(dockerPath, "logs", name).CombinedOutput()
		if err != nil {
			lastErr = fmt.Errorf("docker logs %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
			continue
		}
		if m := trycloudflareURLPattern.Find(out); m != nil {
			return string(m), nil
		}
		// Container found but URL not in logs yet.
		lastErr = fmt.Errorf("container %q running but no trycloudflare.com URL in its logs yet -- cloudflared may still be starting; retry in a few seconds", name)
	}
	return "", fmt.Errorf("%w\n(is the docker-compose tunnel profile active? run: COMPOSE_PROFILES=tunnel make up)", lastErr)
}

// discoverCloudflaredFromLogs is the testable core of
// discoverCloudflaredURL -- given a raw log buffer, returns the URL
// or an error. Used by unit tests so they don't need a real docker.
func discoverCloudflaredFromLogs(logs []byte) (string, error) {
	if m := trycloudflareURLPattern.Find(logs); m != nil {
		return string(m), nil
	}
	if bytes.Contains(logs, []byte("Your quick Tunnel has been created")) {
		// The banner is there but the regex didn't match -- means the
		// format drifted; surface verbatim for debugging.
		return "", fmt.Errorf("found cloudflared banner but no trycloudflare.com URL matched -- log format may have changed: %s", string(logs))
	}
	return "", fmt.Errorf("no trycloudflare.com URL in cloudflared logs (cloudflared may still be starting)")
}

func init() {
	rootCmd.AddCommand(telemetryCmd)
	telemetryCmd.AddCommand(telemetryURLCmd)
	telemetryURLCmd.Flags().StringVar(&telemetryTunnelService, "service", "cloudflared-otel",
		`docker-compose service name for the cloudflared tunnel`)
}
