package engineagent

// Env vars carrying the agent's injected identity.
//
// These names cross a network boundary and a container image: the deploy
// path stamps them onto the engine container and the agent reads them there.
// A rename on one side alone produces an engine that registers with no
// identity, which is the exact failure issue 214's attribution cannot
// survive.
//
// They live in this package rather than beside either user because both
// sides are users. `internal/provisioners` writes them and `iplane
// engine-agent` reads them; the reader owns the vocabulary and the writer
// imports it, so there is one definition instead of two that agree until
// they do not.
const (
	// EnvEngineID is the correlation key. It is the replica instance id, so
	// the control plane joins a registration back to the slot that produced
	// it with a lookup rather than a parse.
	EnvEngineID = "IPLANE_ENGINE_ID"

	EnvDeploymentID = "IPLANE_DEPLOYMENT_ID"
	EnvModel        = "IPLANE_ENGINE_MODEL"
	EnvProvider     = "IPLANE_PROVIDER"
	EnvNodeIndex    = "IPLANE_NODE_INDEX"

	// EnvServiceURL is where to register. The same spelling the other
	// remote-transport verbs use, so an operator who already exported it
	// does not learn a second name.
	EnvServiceURL = "IPLANE_SERVICE_URL"

	// EnvHealthURL is the engine's own health endpoint, probed over
	// loopback to separate ASSEMBLING from SERVING.
	EnvHealthURL = "IPLANE_ENGINE_HEALTH_URL"

	// EnvEndpoint is the externally reachable engine URL. Normally left
	// unstamped: on RunPod it derives from a pod id that does not exist
	// until the create call returns, so the control plane fills it on
	// registration instead. Honoured when something does know it.
	EnvEndpoint = "IPLANE_ENGINE_ENDPOINT"

	// EnvHostID is the provider's machine id. Also normally unstamped, for
	// the same timing reason, and unreadable from inside the container in
	// any case (docs/design/0007 finding 4).
	EnvHostID = "IPLANE_HOST_ID"
)
