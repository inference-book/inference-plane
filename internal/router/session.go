package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// SessionHeader carries the client's conversation/session identity. The
// prefix-affinity routing policy (Ch 8) pins every request bearing the
// same value to one replica, so a multi-turn conversation's later turns
// reuse a warm prefix cache instead of re-prefilling on a fresh replica.
// Absent or empty means "no affinity": the policy falls back to
// round-robin. Mirrors PriorityHeader / TenantHeader; like those, it is
// a router-only signal and does not cross to the engine.
const SessionHeader = "X-IPlane-Session"

// sessionFromHeader decodes X-IPlane-Session, trimmed. Empty when the
// header is absent.
func sessionFromHeader(req *http.Request) string {
	return strings.TrimSpace(req.Header.Get(SessionHeader))
}

// sessionKey resolves a request's affinity key: the explicit
// X-IPlane-Session header when present, else the flat handler's
// body-derived key (stashed on the context by serveFlat), else "". The
// explicit header always wins -- a client that knows its own
// conversation id gives a better key than the router can infer.
//
// Header-less clients only get a derived key on the flat URL, where the
// body is already parsed for the model field (see deriveSessionKey). The
// deploy-id URL streams the body unparsed and is used by iplane-aware
// clients who can send the header, so it stays header-only.
func sessionKey(req *http.Request) string {
	if s := sessionFromHeader(req); s != "" {
		return s
	}
	return derivedSessionFromContext(req.Context())
}

type derivedSessionCtxKey struct{}

// withDerivedSession stashes the body-derived affinity key on the
// context. serveFlat computes it once (from the already-parsed body) and
// sessionKey reads it downstream at dispatch.
func withDerivedSession(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, derivedSessionCtxKey{}, key)
}

func derivedSessionFromContext(ctx context.Context) string {
	s, _ := ctx.Value(derivedSessionCtxKey{}).(string)
	return s
}

// flatMessage is the minimal chat-message shape the router parses from a
// flat-URL body to derive a session key. Content is RawMessage so both
// plain-string and multimodal-array content parse without failing.
type flatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// deriveSessionKey computes a stable affinity key from a chat request's
// opening -- the first system message plus the first user message. The
// opening never changes as a conversation grows, so every turn hashes to
// the same key; different conversations differ in their first user turn,
// so they get different keys. Returns "" when there is no user message to
// key on (e.g. a /v1/completions request with `prompt` instead of
// `messages`), so such traffic gets no affinity rather than a bogus
// shared key.
//
// This is the header-less fallback for plain OpenAI clients. It is a
// coarse proxy for the engine's prefix cache: two distinct conversations
// that share a system prompt still get distinct keys (their first user
// turns differ), which the longest-prefix variant would instead
// co-locate. That refinement is a follow-up; the opening-hash covers the
// per-conversation multi-turn case.
func deriveSessionKey(messages []flatMessage) string {
	var system, firstUser []byte
	for _, m := range messages {
		switch m.Role {
		case "system":
			if system == nil {
				system = m.Content
			}
		case "user":
			if firstUser == nil {
				firstUser = m.Content
			}
		}
		if firstUser != nil {
			break
		}
	}
	if firstUser == nil {
		return ""
	}
	h := sha256.New()
	h.Write(system)
	h.Write([]byte{0})
	h.Write(firstUser)
	return "d-" + hex.EncodeToString(h.Sum(nil)[:8])
}
