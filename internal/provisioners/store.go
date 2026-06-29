package provisioners

import (
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// State is the operator-visible snapshot of provisioner state-of-record.
// Two tables, keyed by tenant-global id: Instances (the GPU side) and
// Deployments (the model-placement side). Both ids are independent
// namespaces -- a Deployment.id and an Instance.id can match without
// collision because they live in different maps. Deployment.InstanceId
// is the foreign key into Instances.
//
// Backends marshal this in/out of their own persistence; the Service
// reads and mutates it through Store.Update. Backend-specific metadata
// (file envelope versions, GORM table schemas, GAE entity kinds) is
// internal to each backend and not part of this contract.
type State struct {
	Instances   map[string]*provisionerv1.Instance
	Deployments map[string]*provisionerv1.Deployment
}

// NewState returns an empty State with both maps initialized. Backends
// use this as their starting point when no prior persistence exists.
func NewState() *State {
	return &State{
		Instances:   map[string]*provisionerv1.Instance{},
		Deployments: map[string]*provisionerv1.Deployment{},
	}
}

// Store is the persistence contract the Service consumes. v0.2 ships
// one impl at internal/provisioners/stores/file/ (atomic write +
// rename + flock). Future backends -- GORM/SQLite for v0.2's later
// query-heavy work, GAE for hosted control plane -- drop in as
// sibling subpackages of stores/ and satisfy this interface.
//
// Atomicity guarantees are backend-specific in implementation but
// equivalent in contract: Update's fn runs against a consistent
// snapshot, and any mutation fn makes is persisted on return.
// Concurrent Updates against the same Store are serialized.
type Store interface {
	// Read returns the current state. Callers should treat the
	// returned *State as a snapshot; mutations through Read are
	// not persisted (use Update for that).
	Read() (*State, error)

	// Update runs fn under the backend's atomicity primitive
	// (flock for file, transaction for SQL, datastore transaction
	// for GAE). The *State fn receives is the live snapshot; any
	// modification fn makes is persisted on return. If fn returns
	// a non-nil error, persistence is skipped and the error
	// propagates.
	//
	// fn should be quick and side-effect-free outside the *State
	// -- the backend may hold a lock for fn's duration.
	Update(fn func(*State) error) error
}
