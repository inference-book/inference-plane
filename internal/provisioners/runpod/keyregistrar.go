package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// graphqlEndpoint is RunPod's GraphQL URL. SSH key management lives
// only here -- the REST API at rest.runpod.io/v1 has no SSH-key
// endpoints. updateUserSettings is the one mutation we need; the
// rest of the runpod adapter is REST.
//
// If RunPod adds per-key REST endpoints later, this file plus the
// inline graphql client below can be deleted.
const graphqlEndpoint = "https://api.runpod.io/graphql"

// EnsurePublicKey satisfies provisioners.KeyRegistrar. The Service
// calls it before Spawn so the operator's iplane-generated SSH key
// is registered with RunPod's account and gets auto-injected into
// newly-created pods via /root/.ssh/authorized_keys.
//
// RunPod's pubKey is a single newline-concatenated authorized_keys
// blob (read-modify-write). To stay idempotent: parse the existing
// blob, skip-if-an-iplane-line-with-our-comment-is-present, else
// append our line and PUT the full blob back.
//
// Comment-based identification: each iplane key carries a stable
// comment string like "iplane-<operator>-<provider>-<rfc3339>". If
// a previous iplane invocation already left a key for this
// operator+provider scope, sshkeys.IsIplaneComment + an exact
// public-bytes match will recognize it and skip the upload.
//
// Scoped-key gotcha: RunPod's rpa_ scoped keys cannot mutate
// user-settings (user-settings is not a scope category in their
// model). A 403 here gets wrapped with a clear message pointing
// at the Full-access bootstrap requirement.
func (p *Provider) EnsurePublicKey(ctx context.Context, publicKey []byte, comment string) error {
	current, err := p.fetchPubKeyBlob(ctx)
	if err != nil {
		return err
	}

	// Parse the desired line so we can compare structurally rather
	// than as text (catches whitespace + comment variations).
	wantParsed, _, _, _, err := ssh.ParseAuthorizedKey(publicKey)
	if err != nil {
		return fmt.Errorf("parse own public key: %w", err)
	}
	wantMarshaled := ssh.MarshalAuthorizedKey(wantParsed)

	// Walk the existing blob:
	//   - non-iplane lines are preserved verbatim (operator's own
	//     keys, runpodctl entries, anything the RunPod UI added).
	//   - iplane-tagged lines that match our exact bytes are KEPT
	//     and flagged haveMatch; we can short-circuit the write.
	//   - iplane-tagged lines with bytes that DON'T match (stale
	//     keys from prior runs with a wiped keystore) are dropped.
	//     A long-running operator account would otherwise accumulate
	//     iplane-* entries forever, eventually risking RunPod's
	//     pod-init not honoring whatever's past whatever line limit
	//     it imposes.
	var kept [][]byte
	haveMatch := false
	prunedStale := false
	rest := []byte(current)
	for len(rest) > 0 {
		lineStart := rest
		parsed, gotComment, _, next, parseErr := ssh.ParseAuthorizedKey(rest)
		if parseErr != nil {
			// Malformed line in the existing blob -- preserve it
			// verbatim and skip past.
			if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
				kept = append(kept, append([]byte(nil), rest[:idx]...))
				rest = rest[idx+1:]
				continue
			}
			kept = append(kept, append([]byte(nil), rest...))
			break
		}
		consumed := len(lineStart) - len(next)
		lineBytes := lineStart[:consumed]
		rest = next
		if !sshkeys.IsIplaneComment(gotComment) {
			kept = append(kept, append([]byte(nil), bytes.TrimRight(lineBytes, "\n")...))
			continue
		}
		if bytes.Equal(ssh.MarshalAuthorizedKey(parsed), wantMarshaled) {
			// Our exact key already in the blob -- preserve.
			kept = append(kept, append([]byte(nil), bytes.TrimRight(lineBytes, "\n")...))
			haveMatch = true
			continue
		}
		// Stale iplane key from a prior keystore -- drop.
		prunedStale = true
	}

	// Fast path: our key was already in the blob AND nothing else
	// needed pruning. No write needed -- idempotent no-op.
	if haveMatch && !prunedStale {
		return nil
	}

	// Rebuild. Append our key if it wasn't already present after
	// pruning stale entries.
	var sb strings.Builder
	for _, line := range kept {
		sb.Write(line)
		sb.WriteByte('\n')
	}
	if !haveMatch {
		sb.Write(bytes.TrimRight(publicKey, "\n"))
		sb.WriteByte('\n')
	}

	return p.updatePubKeyBlob(ctx, sb.String())
}

// graphqlResponse is the envelope every GraphQL call returns:
// `{"data": {...}, "errors": [...]}`. We only need the field shape
// at the data level, which differs per query/mutation -- so this
// envelope uses json.RawMessage and individual calls unmarshal again.
type graphqlResponse struct {
	Data   json.RawMessage `json:"data,omitempty"`
	Errors []graphqlError  `json:"errors,omitempty"`
}

type graphqlError struct {
	Message string `json:"message"`
}

// gqlPost is the one-mutation GraphQL client. RunPod accepts
// "Authorization: Bearer <api-key>" on the same key that drives the
// REST API. Body shape: `{"query": "..."}`.
//
// If the user's RUNPOD_API_KEY is a scoped `rpa_` key, this returns
// a wrapped 403 with a clear message; the standard RunPod blog post
// confirms user-settings is not a scope category, so the upload
// requires a Full-access key.
func (p *Provider) gqlPost(ctx context.Context, query string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, fmt.Errorf("encode graphql body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build graphql request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.client.apiKey)
	req.Header.Set("Accept", "application/json")

	httpClient := http.DefaultClient
	if p.client.httpClient != nil {
		httpClient = p.client.httpClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graphql request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read graphql response: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("RUNPOD_API_KEY needs Full scope for SSH key registration (user-settings is not covered by scoped rpa_ keys): %s", strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("graphql HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var env graphqlResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode graphql envelope: %w: %s", err, raw)
	}
	if len(env.Errors) > 0 {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		return nil, fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}
	return env.Data, nil
}

// FetchUserPubKey exposes the GraphQL fetch of the user-level
// pubKey blob so diagnostic commands (iplane runpod debug-keys) can
// surface it without re-implementing the GraphQL transport.
func (p *Provider) FetchUserPubKey(ctx context.Context) (string, error) {
	return p.fetchPubKeyBlob(ctx)
}

// fetchPubKeyBlob reads the operator's current authorized_keys-style
// pubKey string. Returns "" if RunPod has nothing yet (cleaner than
// surfacing a null for the empty case).
func (p *Provider) fetchPubKeyBlob(ctx context.Context) (string, error) {
	data, err := p.gqlPost(ctx, `query { myself { pubKey } }`)
	if err != nil {
		return "", fmt.Errorf("fetch pubKey: %w", err)
	}
	var resp struct {
		Myself struct {
			PubKey string `json:"pubKey"`
		} `json:"myself"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("decode myself { pubKey }: %w: %s", err, data)
	}
	return resp.Myself.PubKey, nil
}

// updatePubKeyBlob writes the full pubKey blob back via
// updateUserSettings. RunPod's mutation has no per-key endpoint;
// this is the entire blob. Caller is responsible for read-modify-
// write semantics.
func (p *Provider) updatePubKeyBlob(ctx context.Context, blob string) error {
	// Embed via fmt.Sprintf with %q so newlines + quotes get escaped
	// into a JSON-safe GraphQL string literal.
	mutation := fmt.Sprintf(`mutation { updateUserSettings(input: { pubKey: %q }) { id } }`, blob)
	_, err := p.gqlPost(ctx, mutation)
	if err != nil {
		return fmt.Errorf("updateUserSettings: %w", err)
	}
	return nil
}
