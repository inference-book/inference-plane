package engineagent

import (
	"fmt"
	"os"
	"strings"

	"github.com/inference-book/inference-plane/internal/version"
)

// EnvBinaryURL overrides where the agent binary is fetched from. Set it to
// run an agent built from a working tree, or to serve the binary from
// somewhere other than the project's releases.
const EnvBinaryURL = "IPLANE_AGENT_BINARY_URL"

// releaseAssetURL is where a tagged build publishes its linux binaries. It
// mirrors .github/workflows/release.yml; the two are a pair, and a change to
// either without the other means pods fetch a 404.
const releaseAssetURL = "https://github.com/inference-book/inference-plane/releases/download/%s/iplane-linux-%s"

// BinaryURL returns where a container should fetch the agent from, and
// whether there is anywhere to fetch it from at all.
//
// A dev build has no published artifact. That is reported honestly rather
// than papered over with a "latest" URL, because a pod pulling an agent from
// a different version than the control plane it registers with is a class of
// bug nobody wants to debug from a rented box. An operator who wants an
// agent from an untagged build points EnvBinaryURL at their own copy.
func BinaryURL(arch string) (string, bool) {
	if u := os.Getenv(EnvBinaryURL); u != "" {
		return u, true
	}
	if !version.IsRelease() {
		return "", false
	}
	if arch == "" {
		arch = "amd64"
	}
	return fmt.Sprintf(releaseAssetURL, version.Version, arch), true
}

// WrapperScript returns a /bin/sh script that fetches the agent, starts it in
// the background, and then hands the container over to the engine.
//
// The shape is the important part. The engine is exec'd, not spawned, so it
// replaces the shell as the container's main process and the container's
// lifetime still tracks the engine exactly as it did before. Nothing about
// the engine's own failure or shutdown behaviour changes because a wrapper
// was added.
//
// Everything agent-related is best-effort and swallowed. An engine that can
// serve tokens must never fail to start because a download failed, a
// registry was unreachable, or the fetched file would not execute. The
// symptom of all of those is a member missing from the fleet view, which is
// the cost we accept; the alternative is a deployment that does not serve
// because its observability did not install, which is indefensible.
//
// engineCmd is the image's real entrypoint (see Deployment.engine_entrypoint
// on why iplane has to be told). Container args arrive as "$@" and are
// forwarded verbatim, so the engine sees exactly the arguments it would have
// seen without the wrapper.
func WrapperScript(binaryURL string, engineCmd []string) (string, error) {
	if binaryURL == "" {
		return "", fmt.Errorf("engineagent: binary url is required to build a wrapper")
	}
	if len(engineCmd) == 0 {
		return "", fmt.Errorf("engineagent: engine entrypoint is required to build a wrapper")
	}

	prelude, err := AgentPrelude(binaryURL)
	if err != nil {
		return "", err
	}
	return prelude + "\n" + "exec " + shJoin(engineCmd) + ` "$@"`, nil
}

// AgentPrelude returns the shell that fetches the agent and starts it in the
// background, without saying anything about how the engine is then launched.
//
// Split out because providers disagree about that last part and agree about
// everything before it. A provider that runs the workload as docker's
// ENTRYPOINT gets the engine argv plus "$@", because Docker splits
// ENTRYPOINT and CMD into one argv and the container's own arguments arrive
// separately. A provider that hands us a standalone startup script has no
// such split: the whole argv is already in the script and "$@" would be
// empty. Sharing the fetch and keeping the exec local is the same division
// the rest of the provider layer uses -- behaviour shared, provider
// specifics in the adapter.
//
// Everything here is best-effort and swallowed. An engine that can serve
// tokens must never fail to start because a download failed, a registry was
// unreachable, or the fetched file would not execute. The symptom of all of
// those is a member missing from the fleet view, which is the cost we
// accept; the alternative is a deployment that does not serve because its
// observability did not install.
func AgentPrelude(binaryURL string) (string, error) {
	if binaryURL == "" {
		return "", fmt.Errorf("engineagent: binary url is required to build an agent prelude")
	}
	// curl and wget are both plausible and neither is guaranteed, so try
	// each. `|| true` on the whole group is what makes the agent optional.
	return strings.Join([]string{
		"{",
		"  (",
		"    if command -v curl >/dev/null 2>&1; then",
		"      curl -fsSL " + shQuote(binaryURL) + " -o /tmp/iplane;",
		"    elif command -v wget >/dev/null 2>&1; then",
		"      wget -qO /tmp/iplane " + shQuote(binaryURL) + ";",
		"    fi;",
		"    chmod +x /tmp/iplane && /tmp/iplane engine-agent;",
		"  ) >/tmp/iplane-agent.log 2>&1 &",
		"} || true;",
	}, "\n"), nil
}

// AgentScript returns a /bin/sh script that fetches the agent and runs it in
// the foreground.
//
// The sidecar counterpart to WrapperScript. There is no engine to exec here
// and nothing to swallow: the agent IS this container's job, so a failed
// fetch should exit non-zero and let docker's restart policy retry rather
// than leaving a container alive doing nothing. That inverts the wrapper's
// rule, and the reason is that failing here costs no inference, while
// failing there would have.
func AgentScript(binaryURL string) (string, error) {
	if binaryURL == "" {
		return "", fmt.Errorf("engineagent: binary url is required to build an agent script")
	}
	return strings.Join([]string{
		"set -e;",
		"if command -v curl >/dev/null 2>&1; then",
		"  curl -fsSL " + shQuote(binaryURL) + " -o /tmp/iplane;",
		"else",
		"  wget -qO /tmp/iplane " + shQuote(binaryURL) + ";",
		"fi;",
		"chmod +x /tmp/iplane;",
		`exec /tmp/iplane engine-agent`,
	}, "\n"), nil
}

// shQuote wraps s in single quotes for safe use in a shell command,
// escaping any embedded single quote the standard way.
//
// The inputs here are operator-supplied (an image entrypoint, a binary URL)
// and land in a string that a shell will parse, so they are quoted rather
// than trusted. An unquoted URL containing a semicolon would be a command
// injection into the container's own entrypoint.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shJoin quotes and joins argv for use as a shell command.
func shJoin(argv []string) string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		out = append(out, shQuote(a))
	}
	return strings.Join(out, " ")
}
