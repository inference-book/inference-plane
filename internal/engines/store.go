package engines

import (
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"google.golang.org/protobuf/proto"
)

// StateStore adapts the provisioner state store to the registry's narrow
// Store contract, so engine registrations land in the same state file as
// instances and deployments and survive a daemon restart.
type StateStore struct {
	store provisioners.Store
}

// NewStateStore wraps a provisioner store for the registry.
func NewStateStore(store provisioners.Store) *StateStore {
	return &StateStore{store: store}
}

// PutEngine writes e under its id, replacing any prior record.
//
// The whole read-modify-write happens inside one Update closure. That is not
// stylistic: under LockForLifetime the daemon holds the flock once and
// individual Updates skip re-acquiring it, so two agents renewing at the same
// instant would otherwise both read, both modify, and the second write would
// clobber the first. The same lost-update shape bit multi-replica deploys
// whose per-slot endpoint patches landed together. Never call store.Read or
// store.Update from inside this closure; the lock is not re-entrant.
func (s *StateStore) PutEngine(e *provisionerv1.Engine) error {
	return s.store.Update(func(st *provisioners.State) error {
		if st.Engines == nil {
			st.Engines = map[string]*provisionerv1.Engine{}
		}
		st.Engines[e.GetId()] = proto.Clone(e).(*provisionerv1.Engine)
		return nil
	})
}

// ListEngines returns every stored engine, LOST included. Order is
// unspecified; the registry sorts.
func (s *StateStore) ListEngines() ([]*provisionerv1.Engine, error) {
	st, err := s.store.Read()
	if err != nil {
		return nil, err
	}
	out := make([]*provisionerv1.Engine, 0, len(st.Engines))
	for _, e := range st.Engines {
		out = append(out, proto.Clone(e).(*provisionerv1.Engine))
	}
	return out, nil
}
