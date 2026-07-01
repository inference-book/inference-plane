package router

import (
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
