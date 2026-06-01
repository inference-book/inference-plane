package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"

	"connectrpc.com/connect"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// FlatMaxBodyBytes caps how much of the request body the flat handler
// will buffer before extracting the model field. Generously sized for
// long chat-completion conversations; protects against pathological
// inputs without truncating realistic ones. A body that exceeds this
// gets rejected with 413.
const FlatMaxBodyBytes = 8 * 1024 * 1024 // 8 MiB

// serveFlat handles the OpenAI-shaped URL family:
//
//	POST /v1/chat/completions
//	POST /v1/completions
//
// The request body's `model` field is the routing key. The handler
// reads the body, parses just enough to extract `model`, looks up
// deployments that serve it, picks one (newest RUNNING wins; see
// pickDeployment for the policy), and reverse-proxies the buffered
// body to the chosen deployment's engine.
//
// This is the primary operator-facing URL. Existing OpenAI SDKs work
// against iplane with `base_url=http://<iplane>/v1` and no further
// configuration -- the SDK appends /chat/completions and serializes
// model into the body, exactly the shape this handler expects.
func (r *Router) serveFlat(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, FlatMaxBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("read request body: %v", err), "invalid_request_error")
		return
	}
	if len(body) > FlatMaxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds the %d-byte limit", FlatMaxBodyBytes), "request_too_large")
		return
	}

	// Decode only the model field. Other fields stay opaque -- the
	// router does not interpret or rewrite them. The engine sees the
	// body byte-for-byte as the client sent it.
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("request body is not valid JSON: %v", err), "invalid_request_error")
		return
	}
	if probe.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "request body must include a non-empty `model` field", "invalid_request_error")
		return
	}

	dep, err := r.pickDeploymentForModel(req.Context(), probe.Model)
	if err != nil {
		var notFound errModelNotFound
		switch {
		case isErrAs(err, &notFound):
			writeOpenAIError(w, http.StatusNotFound, fmt.Sprintf("no RUNNING deployment serves model %q", probe.Model), "model_not_available")
		default:
			writeOpenAIError(w, http.StatusBadGateway, fmt.Sprintf("daemon lookup failed: %v", err), "daemon_error")
		}
		return
	}

	// Replay the buffered body for the reverse-proxy. ReverseProxy
	// reads req.Body to forward; we drained it for the model peek so
	// we re-seat it with a NopCloser-wrapped Reader. ContentLength
	// must match or the proxy may set chunked encoding incorrectly.
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))

	// stripDeployPrefix=false: the flat URL has no iplane-side prefix
	// to strip; /v1/chat/completions forwards as-is to the engine.
	r.handleWithMetrics(w, req, dep, false)
}

// pickDeploymentForModel returns the deployment that should serve a
// request for `model`. Policy (v0.2 ch7-beat1.3b):
//
//   - filter ListDeployments by Deployment.model == requested
//   - prefer RUNNING over other states
//   - among matches at equal precedence, prefer the most recently
//     created (newest first)
//
// Returns errModelNotFound when no deployment matches. Multi-replica
// fan-out within a single deployment is Beat 3's work; the policy
// here picks among DISTINCT deployments serving the same model
// (operator deliberately deployed two of them, e.g. A/B testing).
func (r *Router) pickDeploymentForModel(ctx context.Context, model string) (*provisionerv1.Deployment, error) {
	ctx, cancel := context.WithTimeout(ctx, DescribeTimeout)
	defer cancel()
	resp, err := r.client.ListDeployments(ctx, connect.NewRequest(&provisionerv1.ListDeploymentsRequest{}))
	if err != nil {
		return nil, err
	}
	var matches []*provisionerv1.Deployment
	for _, dep := range resp.Msg.GetDeployments() {
		if dep.GetModel() == model {
			matches = append(matches, dep)
		}
	}
	if len(matches) == 0 {
		return nil, errModelNotFound{model: model}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		// RUNNING ahead of everything else; within RUNNING, newest first.
		ri := matches[i].GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING
		rj := matches[j].GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING
		if ri != rj {
			return ri
		}
		ti := matches[i].GetCreatedAt().AsTime()
		tj := matches[j].GetCreatedAt().AsTime()
		return ti.After(tj)
	})
	return matches[0], nil
}

// errModelNotFound is the sentinel pickDeploymentForModel returns when
// no deployment in the daemon's state-of-record matches the requested
// model. serveFlat maps it to a 404 with an operator-friendly message.
type errModelNotFound struct {
	model string
}

func (e errModelNotFound) Error() string {
	return "no deployment serves model " + e.model
}

// isErrAs is a thin wrapper over errors.As for the fixed sentinel-type
// case (no target inference, no chain walk to a wrapped). The
// indirection keeps the import surface in this file narrow and
// expresses intent more clearly than the standard library form at
// each call site.
func isErrAs(err error, target *errModelNotFound) bool {
	if err == nil {
		return false
	}
	tgt, ok := err.(errModelNotFound)
	if !ok {
		return false
	}
	*target = tgt
	return true
}
