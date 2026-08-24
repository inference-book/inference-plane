package imagearch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeRegistry serves the two manifest shapes and the token dance, and
// records what it was asked for. Bodies are transcribed from real Docker Hub
// responses for vllm/vllm-openai (2026-08-24), because the point of this
// package is speaking a protocol somebody else defined.
type fakeRegistry struct {
	t *testing.T
	// manifest is served for any manifest request; blob for any blob.
	manifest, blob string
	// requireToken makes the registry answer 401 with a challenge first,
	// which is what Docker Hub does for an anonymous read.
	requireToken bool
	tokenIssued  atomic.Int32
	manifestGets atomic.Int32
	lastAccept   string
	// scopeSeen records what the token request asked for.
	scopeSeen string
	// realm is filled in once the test server has an address.
	realm string
}

func (f *fakeRegistry) start(t *testing.T) (*Resolver, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenIssued.Add(1)
		f.scopeSeen = r.URL.Query().Get("scope")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok-123"})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if f.requireToken && r.Header.Get("Authorization") != "Bearer tok-123" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+f.realm+`",service="registry.example",scope="repository:vllm/vllm-openai:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.Path, "/blobs/") {
			_, _ = w.Write([]byte(f.blob))
			return
		}
		f.manifestGets.Add(1)
		f.lastAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		_, _ = w.Write([]byte(f.manifest))
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	f.realm = srv.URL + "/token"
	return New(WithHTTPClient(srv.Client())), strings.TrimPrefix(srv.URL, "https://")
}

const indexBody = `{"schemaVersion":2,
 "mediaType":"application/vnd.oci.image.index.v1+json",
 "manifests":[
  {"platform":{"architecture":"amd64","os":"linux"}},
  {"platform":{"architecture":"arm64","os":"linux"}},
  {"platform":{"architecture":"unknown","os":"unknown"}}
 ]}`

const singleManifestBody = `{"schemaVersion":2,
 "mediaType":"application/vnd.docker.distribution.manifest.v2+json",
 "config":{"mediaType":"application/vnd.docker.container.image.v1+json",
           "digest":"sha256:644d31f210b8e4d005fcbc6bc3ad13221993c0cd5ec627e9503d74dd53509b52"}}`

const configBody = `{"architecture":"amd64","os":"linux"}`

// A modern tag is an index and the platforms are on it. vLLM v0.27.1 is this
// shape and carries both architectures, which is why the trap #405 describes
// does not fire on a current tag.
func TestArchitectures_ReadsAManifestIndex(t *testing.T) {
	f := &fakeRegistry{manifest: indexBody}
	r, host := f.start(t)

	got, why := r.Architectures(t.Context(), host+"/vllm/vllm-openai:v0.27.1")
	if why != "" {
		t.Fatalf("could not read: %s", why)
	}
	if !slices.Equal(got, []string{"amd64", "arm64"}) {
		t.Errorf("architectures = %v, want [amd64 arm64]", got)
	}
}

// The attestation entry a modern build attaches carries architecture
// "unknown". Counting it would make every such image look as though it runs
// anywhere, which is the opposite of the answer this package exists to give.
func TestArchitectures_IgnoresTheUnknownAttestationEntry(t *testing.T) {
	f := &fakeRegistry{manifest: indexBody}
	r, host := f.start(t)

	got, _ := r.Architectures(t.Context(), host+"/vllm/vllm-openai:v0.27.1")
	if slices.Contains(got, "unknown") {
		t.Errorf("architectures = %v, want the attestation entry dropped", got)
	}
}

// An older tag is a single manifest with no platform block anywhere, and the
// architecture is in the config blob. vLLM v0.7.0 is this shape, and it is
// the tag this repo names in most of its examples, so the second hop is the
// normal path rather than a legacy branch.
func TestArchitectures_FollowsTheConfigBlobOnASingleManifest(t *testing.T) {
	f := &fakeRegistry{manifest: singleManifestBody, blob: configBody}
	r, host := f.start(t)

	got, why := r.Architectures(t.Context(), host+"/vllm/vllm-openai:v0.7.0")
	if why != "" {
		t.Fatalf("could not read: %s", why)
	}
	if !slices.Equal(got, []string{"amd64"}) {
		t.Errorf("architectures = %v, want [amd64]", got)
	}
}

// The token endpoint comes from the challenge, not from a constant, which is
// what makes this work against ghcr and quay without knowing anything about
// them.
func TestArchitectures_FollowsTheAuthChallenge(t *testing.T) {
	f := &fakeRegistry{manifest: indexBody, requireToken: true}
	r, host := f.start(t)

	got, why := r.Architectures(t.Context(), host+"/vllm/vllm-openai:v0.27.1")
	if why != "" {
		t.Fatalf("could not read: %s", why)
	}
	if len(got) == 0 {
		t.Fatal("read nothing after authenticating")
	}
	if f.tokenIssued.Load() != 1 {
		t.Errorf("token requests = %d, want 1", f.tokenIssued.Load())
	}
	if f.scopeSeen != "repository:vllm/vllm-openai:pull" {
		t.Errorf("token scope = %q, want the one the challenge named", f.scopeSeen)
	}
}

// Docker Hub counts manifest reads against an anonymous rate limit, so a
// daemon that resolves the same image on every deploy can exhaust an IP's
// budget and start getting 429s on the path it least wants them.
func TestArchitectures_ReadsARegistryOncePerImage(t *testing.T) {
	f := &fakeRegistry{manifest: indexBody}
	r, host := f.start(t)
	ref := host + "/vllm/vllm-openai:v0.27.1"

	for range 5 {
		if _, why := r.Architectures(t.Context(), ref); why != "" {
			t.Fatalf("could not read: %s", why)
		}
	}
	if got := f.manifestGets.Load(); got != 1 {
		t.Errorf("manifest reads = %d across 5 lookups, want 1", got)
	}
}

// The Accept header has to offer the index types first, or a registry is
// entitled to pick one platform's manifest for us and the answer silently
// narrows to whatever it chose.
func TestArchitectures_AsksForTheIndexTypes(t *testing.T) {
	f := &fakeRegistry{manifest: indexBody}
	r, host := f.start(t)

	_, _ = r.Architectures(t.Context(), host+"/vllm/vllm-openai:v0.27.1")
	for _, want := range []string{"oci.image.index", "manifest.list"} {
		if !strings.Contains(f.lastAccept, want) {
			t.Errorf("Accept %q does not offer %q", f.lastAccept, want)
		}
	}
}

// Every failure is the same failure to a caller: it learned nothing. The
// reason comes back so a caller can say why it is proceeding blind, which is
// what separates a visible fallback from a silent one.
func TestArchitectures_FailsOpenWithAReason(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	r := New(WithHTTPClient(srv.Client()))

	got, why := r.Architectures(t.Context(), strings.TrimPrefix(srv.URL, "https://")+"/vllm/vllm-openai:v1")
	if len(got) != 0 {
		t.Errorf("architectures = %v, want none from a registry that errored", got)
	}
	if why == "" {
		t.Error("no reason given; a caller proceeding blind should be able to say why")
	}
}

// A private image answers 401 even after a token. That is indistinguishable
// from a registry outage as far as the caller is concerned, and must not
// become a refusal.
func TestArchitectures_FailsOpenOnAnImageItCannotSee(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok-123"})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://127.0.0.1:1/token",service="s"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	r := New(WithHTTPClient(srv.Client()))
	got, why := r.Architectures(t.Context(), strings.TrimPrefix(srv.URL, "https://")+"/private/thing:v1")
	if len(got) != 0 {
		t.Errorf("architectures = %v, want none for an image it cannot see", got)
	}
	if why == "" {
		t.Error("no reason given")
	}
}

func TestParseRef(t *testing.T) {
	tests := []struct{ in, registry, repo, ref string }{
		// No dot and no colon in the first segment, so it is a Docker Hub repo.
		{"vllm/vllm-openai:v0.7.0", DefaultRegistry, "vllm/vllm-openai", "v0.7.0"},
		// A single segment is an official image and lives under library/.
		{"ubuntu:24.04", DefaultRegistry, "library/ubuntu", "24.04"},
		{"ubuntu", DefaultRegistry, "library/ubuntu", "latest"},
		// A dot makes the first segment a host.
		{"ghcr.io/foo/bar:v1", "ghcr.io", "foo/bar", "v1"},
		{"nvcr.io/nvidia/tritonserver:24.01-py3", "nvcr.io", "nvidia/tritonserver", "24.01-py3"},
		// A port colon must not be read as a tag.
		{"localhost:5000/foo:v1", "localhost:5000", "foo", "v1"},
		// A digest wins over a tag, and its colon is not a registry port.
		{"vllm/vllm-openai@sha256:abc", DefaultRegistry, "vllm/vllm-openai", "sha256:abc"},
		// Deep paths stay whole.
		{"ghcr.io/a/b/c:v1", "ghcr.io", "a/b/c", "v1"},
	}
	for _, tt := range tests {
		reg, repo, ref, err := ParseRef(tt.in)
		if err != nil {
			t.Errorf("ParseRef(%q): %v", tt.in, err)
			continue
		}
		if reg != tt.registry || repo != tt.repo || ref != tt.ref {
			t.Errorf("ParseRef(%q) = (%q, %q, %q), want (%q, %q, %q)",
				tt.in, reg, repo, ref, tt.registry, tt.repo, tt.ref)
		}
	}
}

// A scope legitimately contains commas when several are requested, so
// splitting the challenge on every comma corrupts it.
func TestParseChallenge(t *testing.T) {
	got := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:vllm/vllm-openai:pull"`)
	if got["realm"] != "https://auth.docker.io/token" {
		t.Errorf("realm = %q", got["realm"])
	}
	if got["scope"] != "repository:vllm/vllm-openai:pull" {
		t.Errorf("scope = %q", got["scope"])
	}

	multi := parseChallenge(`Bearer realm="https://r/token",scope="repository:a:pull,repository:b:pull"`)
	if multi["scope"] != "repository:a:pull,repository:b:pull" {
		t.Errorf("a multi-scope challenge was split on its own comma: %q", multi["scope"])
	}
}
