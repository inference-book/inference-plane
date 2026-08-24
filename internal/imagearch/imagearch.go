// Package imagearch answers which CPU architectures a container image can
// run on, by asking the registry that holds it.
//
// The question exists because the operator is the wrong place to ask.
// ResourceRequirements.architecture and --arch let someone who already knows
// state it (#390), and the person who does not know rents Lambda's arm64
// GH200 for an x86 image and finds out when the container will not start on
// a machine that is already billing (#405).
//
// # Two shapes, and the older one is the common one here
//
// A modern tag is a manifest *index*: one document listing a manifest per
// platform, so the architectures are read straight off it. vLLM's v0.27.1 is
// this shape and carries linux/amd64 and linux/arm64.
//
// An older tag is a single manifest with no platform block anywhere. The
// architecture lives in the *config blob* the manifest points at, which is a
// second request. vLLM's v0.7.0 is this shape, and it is the tag this repo
// names in most of its examples, so the two-hop path is the normal path
// rather than a legacy branch.
//
// # Auth is read from the challenge, not hardcoded
//
// A registry answers an anonymous manifest request with 401 and a
// WWW-Authenticate header naming the realm, service and scope to get a token
// from. Following that rather than assuming auth.docker.io is what makes
// this work against ghcr, quay and nvcr without knowing anything about them.
//
// # Everything here fails open
//
// Every error path returns "no answer" rather than an error the caller must
// interpret. An unreadable registry, a rate limit, a private image, a shape
// nobody has seen: all mean the same thing to a caller, which is that it
// learned nothing and should behave as it did before this package existed.
// That matches provisioners.FilterArchitecture, which keeps a candidate that
// reports nothing, and the pre-rent budget check, which skips rather than
// refuses when an input is missing. A wrong refusal here is worse than the
// silence it replaces: it blocks a deploy that would have worked.
package imagearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultRegistry is where an image reference with no registry host lives.
// "vllm/vllm-openai" means Docker Hub, and Docker Hub's registry API is not
// served from docker.io.
const DefaultRegistry = "registry-1.docker.io"

// officialLibrary is the namespace Docker Hub puts single-segment names in,
// so "ubuntu" is really "library/ubuntu" as far as the API is concerned.
const officialLibrary = "library"

// acceptManifests is the Accept header that asks a registry for whichever
// shape it has. Order matters to some registries: index first, so a
// multi-arch image answers with the document that carries platforms rather
// than with one platform's manifest chosen for us.
var acceptManifests = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Resolver reads image architectures, caching what it learns.
//
// The cache is not an optimization detail. Docker Hub rate-limits anonymous
// requests and counts manifest reads against the limit, so a deploy loop or
// a CI run that resolves the same image repeatedly can exhaust an IP's
// budget and start getting 429s. One entry per image reference for the
// lifetime of the process is enough: an image tag's architectures do not
// change, and a mutable tag that is re-pushed is not something a running
// daemon needs to notice mid-flight.
type Resolver struct {
	client *http.Client

	mu    sync.Mutex
	cache map[string][]string
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithHTTPClient swaps the client. Tests point it at an httptest server.
func WithHTTPClient(c *http.Client) Option {
	return func(r *Resolver) { r.client = c }
}

// New builds a Resolver. The default client carries a timeout because this
// sits on the deploy path: a registry that hangs must not hold a rent open.
func New(opts ...Option) *Resolver {
	r := &Resolver{
		client: &http.Client{Timeout: 15 * time.Second},
		cache:  map[string][]string{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Architectures returns the CPU architectures the image can run on, in the
// vocabulary the registry uses ("amd64", "arm64"), which is already the
// vocabulary provisioners.NormalizeArch normalizes onto.
//
// Returns nil when it could not find out, which is not an error condition
// the caller has to distinguish: an empty answer means behave as before.
// The reason is returned alongside so a caller can log why it is proceeding
// blind, which is the difference between a silent fallback and one an
// operator can see.
func (r *Resolver) Architectures(ctx context.Context, image string) (arches []string, why string) {
	if image == "" {
		return nil, "no image was named"
	}
	r.mu.Lock()
	if cached, ok := r.cache[image]; ok {
		r.mu.Unlock()
		return cached, ""
	}
	r.mu.Unlock()

	got, reason := r.read(ctx, image)
	if len(got) > 0 {
		r.mu.Lock()
		r.cache[image] = got
		r.mu.Unlock()
	}
	return got, reason
}

func (r *Resolver) read(ctx context.Context, image string) ([]string, string) {
	registry, repo, ref, err := ParseRef(image)
	if err != nil {
		return nil, err.Error()
	}
	base := "https://" + registry

	body, mediaType, err := r.get(ctx, base+"/v2/"+repo+"/manifests/"+ref, repo, acceptManifests)
	if err != nil {
		return nil, "could not read the manifest: " + err.Error()
	}

	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, "the manifest did not parse"
	}

	// Index shape: the platforms are right here.
	if len(doc.Manifests) > 0 {
		var out []string
		for _, m := range doc.Manifests {
			a := m.Platform.Architecture
			// "unknown" is what an attestation manifest carries. Counting
			// it as a platform would make every modern image look as
			// though it runs anywhere.
			if a == "" || a == "unknown" {
				continue
			}
			if m.Platform.OS != "" && m.Platform.OS != "linux" {
				continue
			}
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
		if len(out) == 0 {
			return nil, "the manifest index named no linux platform"
		}
		slices.Sort(out)
		return out, ""
	}

	// Single-manifest shape: the architecture is in the config blob.
	if doc.Config.Digest == "" {
		return nil, "the manifest (" + mediaType + ") carries neither platforms nor a config digest"
	}
	cfgBody, _, err := r.get(ctx, base+"/v2/"+repo+"/blobs/"+doc.Config.Digest, repo, "*/*")
	if err != nil {
		return nil, "could not read the image config: " + err.Error()
	}
	var cfg configDoc
	if err := json.Unmarshal(cfgBody, &cfg); err != nil {
		return nil, "the image config did not parse"
	}
	if cfg.Architecture == "" {
		return nil, "the image config names no architecture"
	}
	return []string{cfg.Architecture}, ""
}

// get performs one registry request, acquiring a token if the registry asks
// for one. Follows the challenge rather than assuming a token endpoint,
// which is what makes this work on registries nobody here has tried.
func (r *Resolver) get(ctx context.Context, u, repo, accept string) ([]byte, string, error) {
	body, mediaType, challenge, err := r.attempt(ctx, u, accept, "")
	if err != nil {
		return nil, "", err
	}
	if challenge == "" {
		return body, mediaType, nil
	}
	token, err := r.token(ctx, challenge, repo)
	if err != nil {
		return nil, "", err
	}
	body, mediaType, again, err := r.attempt(ctx, u, accept, token)
	if err != nil {
		return nil, "", err
	}
	if again != "" {
		return nil, "", fmt.Errorf("registry still refused after a token (the image may be private)")
	}
	return body, mediaType, nil
}

// attempt makes one request. A 401 returns the WWW-Authenticate value rather
// than an error, so the caller can decide whether to go get a token.
func (r *Resolver) attempt(ctx context.Context, u, accept, token string) (body []byte, mediaType, challenge string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("Accept", accept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, "", resp.Header.Get("WWW-Authenticate"), nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("registry answered %d", resp.StatusCode)
	}
	b, err := readCapped(resp)
	if err != nil {
		return nil, "", "", err
	}
	return b, resp.Header.Get("Content-Type"), "", nil
}

// token exchanges a WWW-Authenticate challenge for a bearer token.
//
// The scope in the challenge is used when present and synthesized when not,
// because some registries answer with realm and service alone and expect the
// client to know what it is asking for.
func (r *Resolver) token(ctx context.Context, challenge, repo string) (string, error) {
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry asked for auth without naming a realm")
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint answered %d", resp.StatusCode)
	}
	b, err := readCapped(resp)
	if err != nil {
		return "", err
	}
	var tok tokenDoc
	if err := json.Unmarshal(b, &tok); err != nil {
		return "", fmt.Errorf("token response did not parse")
	}
	// Some registries return the same value under access_token.
	if tok.Token != "" {
		return tok.Token, nil
	}
	if tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	return "", fmt.Errorf("token response carried no token")
}

// ParseRef splits an image reference into registry host, repository and
// reference (tag or digest).
//
// The rule for deciding whether the first segment is a registry is the one
// the docker CLI uses: it is a host if it contains a dot or a colon, or is
// literally "localhost". That is why "vllm/vllm-openai" is a Docker Hub repo
// and "ghcr.io/foo/bar" is not.
func ParseRef(image string) (registry, repository, reference string, err error) {
	rest := image
	reference = "latest"

	// A digest reference wins over a tag, and its colon must not be read as
	// a registry port.
	if i := strings.Index(rest, "@"); i >= 0 {
		reference, rest = rest[i+1:], rest[:i]
	} else if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i+1:], "/") {
		reference, rest = rest[i+1:], rest[:i]
	}
	if rest == "" {
		return "", "", "", fmt.Errorf("image reference %q names no repository", image)
	}

	registry = DefaultRegistry
	parts := strings.Split(rest, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		registry, parts = parts[0], parts[1:]
	}
	repository = strings.Join(parts, "/")
	if registry == DefaultRegistry && !strings.Contains(repository, "/") {
		repository = officialLibrary + "/" + repository
	}
	if repository == "" {
		return "", "", "", fmt.Errorf("image reference %q names no repository", image)
	}
	return registry, repository, reference, nil
}

// parseChallenge pulls key="value" pairs out of a WWW-Authenticate header.
func parseChallenge(v string) map[string]string {
	out := map[string]string{}
	if i := strings.IndexByte(v, ' '); i >= 0 {
		v = v[i+1:] // drop the "Bearer" scheme
	}
	for _, field := range splitOutsideQuotes(v) {
		k, val, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(val), `"`)
	}
	return out
}

// splitOutsideQuotes splits on commas that are not inside a quoted value,
// because a scope legitimately contains commas when several are requested.
func splitOutsideQuotes(s string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			cur.WriteRune(r)
		case r == ',' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

type manifestDoc struct {
	Manifests []struct {
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
}

type configDoc struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type tokenDoc struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

// maxBody caps what this package will read from a registry. A manifest is
// kilobytes and a config blob tens of kilobytes; anything vastly larger is a
// registry misbehaving or answering with something else entirely, and this
// runs on the deploy path where a hostile or broken response must not become
// unbounded memory.
const maxBody = 4 << 20

func readCapped(resp *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}
