// Package state holds the iplane state file and the read-modify-write
// loop the Service runs under. The file lives at <dir>/state.json (in
// production, ~/.iplane/state.json) and is the persistent intent log
// for every Spawn / Terminate the operator runs.
//
// Two contracts the rest of the codebase relies on:
//
//   - Writes are atomic. Update writes to a temp file in the same
//     directory then renames it onto state.json. A crash mid-write
//     leaves either the old file or the new file -- never a torn one.
//
//   - Writes are serialized across processes by a flock on the
//     directory. Two `iplane instance create` invocations on the same
//     laptop cannot race: the second one waits for the first to
//     finish before it sees the file. (Cross-laptop dedup waits for
//     v1.0's multi-operator backend; that's documented in the design
//     doc.)
//
// Instance values inside the file are serialized via protojson, with
// UseProtoNames so the on-disk field names (provider_id, etc.) match
// what the design doc's state-file example shows. The envelope itself
// is plain stdlib JSON.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// SchemaVersion is what new state files are stamped with. Uses
// semver-shaped minor/patch bumps so the string communicates intent
// of the change even though backward compatibility is not a project
// invariant (no shipping customers):
//
//	MAJOR bump (X.0)   -- breaking change to an existing field
//	                      (renamed, retyped, removed). Phase 1's
//	                      shipped "1" is implicitly "1.0".
//	MINOR bump (X.Y)   -- additive change. New top-level field, new
//	                      message, new enum value. Old readers ignore
//	                      what they don't know about.
//	PATCH bump (X.Y.Z) -- cosmetic / comment / on-disk-formatting
//	                      change. Field set unchanged.
//
// v0.1 Phase 1 shipped "1". v0.1 Phase 2 bumped to "1.1" -- the envelope
// gained the deployments map (purely additive). v0.2 ch7-beat1.1 bumps
// to "1.2" -- the Deployment message gains idle_ttl_seconds /
// last_activity_at / no_idle_destroy for the long-lived-daemon's idle
// reaper. Still additive; old records load with zero values via
// protojson DiscardUnknown.
const SchemaVersion = "1.2"

// BackendLocalFile is the value written into the file's `backend` field.
// v1.0's remote backend will write its own value here and may fall back
// to local-file mode in degraded states -- the field records which
// writer last touched the file.
const BackendLocalFile = "local-file"

// File is the in-memory representation of state.json. Two top-level
// tables: instances (Phase 1) and deployments (Phase 2). Both are
// keyed by tenant-global id; the two id namespaces are independent
// (a Deployment.id and an Instance.id can match, they're looked up in
// different maps). Deployments cross-reference instances by
// Deployment.instance_id -- not embedded as sub-records, so v0.2's
// multi-deployment-per-instance scales without restructuring.
type File struct {
	SchemaVersion string
	Backend       string
	OperatorID    string
	Instances     map[string]*provisionerv1.Instance
	Deployments   map[string]*provisionerv1.Deployment
}

// Store owns the file path and the flock. One Store per CLI invocation
// is the expected usage; Update is safe to call multiple times on the
// same Store from one process.
type Store struct {
	dir        string // directory holding state.json (and the flock file)
	path       string // <dir>/state.json
	operatorID string
}

// Open prepares a Store rooted at dir. Creates dir if it does not exist.
// dir is typically os.UserHomeDir() + "/.iplane" in production and a
// t.TempDir() in tests.
//
// operatorID is stamped into newly-created state files and used as the
// default tag value when callers do not supply one. v0.1 always passes
// "default"; multi-operator backends in v1.0 will populate this from
// auth state.
func Open(dir, operatorID string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %q: %w", dir, err)
	}
	return &Store{
		dir:        dir,
		path:       filepath.Join(dir, "state.json"),
		operatorID: operatorID,
	}, nil
}

// Path returns the absolute path to state.json. Useful for error
// messages and tests.
func (s *Store) Path() string { return s.path }

// Read returns the current contents of the state file without taking
// the write lock. Suitable for read-only callers (the `list` command
// without --remote). Returns an empty file with the right header if
// state.json does not yet exist.
func (s *Store) Read() (*File, error) {
	return s.readFromDisk()
}

// Update runs fn under the exclusive flock. fn receives the loaded
// File; any modification fn makes to it is persisted on return. fn
// must return quickly (it should NOT perform network IO -- the lock
// is held for its entire duration and blocks other CLI invocations).
//
// If fn returns a non-nil error, the file is NOT written; the error
// propagates to the caller and the on-disk state is unchanged.
func (s *Store) Update(fn func(*File) error) error {
	lockFile, err := s.lock()
	if err != nil {
		return err
	}
	defer s.unlock(lockFile)

	file, err := s.readFromDisk()
	if err != nil {
		return err
	}
	if err := fn(file); err != nil {
		return err
	}
	return s.writeToDisk(file)
}

// lock acquires the exclusive directory flock. Returns the *os.File so
// the caller can keep it referenced for the duration of the locked
// section -- the runtime's *os.File finalizer would otherwise close
// the underlying FD as soon as the value becomes unreachable, and
// closing the FD releases the flock. (Worse: the FD slot can then be
// reused by the OS for an unrelated socket, and unlock's Close()
// would tear down whatever now lives there.) The flock is held against
// a sentinel file (.lock) inside the dir rather than against state.json
// itself because state.json is rewritten on every Update via temp +
// rename, which would drop any lock held on it.
func (s *Store) lock() (*os.File, error) {
	lockPath := filepath.Join(s.dir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %q: %w", lockPath, err)
	}
	return f, nil
}

func (s *Store) unlock(f *os.File) {
	if f == nil {
		return
	}
	// LOCK_UN on close is automatic, but explicit is clearer.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func (s *Store) readFromDisk() (*File, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.emptyFile(), nil
		}
		return nil, fmt.Errorf("read state file %q: %w", s.path, err)
	}
	if len(raw) == 0 {
		return s.emptyFile(), nil
	}
	var env struct {
		SchemaVersion string                     `json:"schema_version"`
		Backend       string                     `json:"backend"`
		OperatorID    string                     `json:"operator_id"`
		Instances     map[string]json.RawMessage `json:"instances"`
		Deployments   map[string]json.RawMessage `json:"deployments"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse state file %q: %w", s.path, err)
	}
	file := &File{
		SchemaVersion: env.SchemaVersion,
		Backend:       env.Backend,
		OperatorID:    env.OperatorID,
		Instances:     make(map[string]*provisionerv1.Instance, len(env.Instances)),
		Deployments:   make(map[string]*provisionerv1.Deployment, len(env.Deployments)),
	}
	if file.OperatorID == "" {
		file.OperatorID = s.operatorID
	}
	unmarshal := protojson.UnmarshalOptions{DiscardUnknown: true}
	for id, instRaw := range env.Instances {
		inst := &provisionerv1.Instance{}
		if err := unmarshal.Unmarshal(instRaw, inst); err != nil {
			return nil, fmt.Errorf("parse instance %q in state file: %w", id, err)
		}
		file.Instances[id] = inst
	}
	for id, depRaw := range env.Deployments {
		dep := &provisionerv1.Deployment{}
		if err := unmarshal.Unmarshal(depRaw, dep); err != nil {
			return nil, fmt.Errorf("parse deployment %q in state file: %w", id, err)
		}
		file.Deployments[id] = dep
	}
	return file, nil
}

func (s *Store) writeToDisk(file *File) error {
	if file.SchemaVersion == "" {
		file.SchemaVersion = SchemaVersion
	}
	if file.Backend == "" {
		file.Backend = BackendLocalFile
	}
	if file.OperatorID == "" {
		file.OperatorID = s.operatorID
	}
	marshal := protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: false,
	}
	instancesJSON := make(map[string]json.RawMessage, len(file.Instances))
	for id, inst := range file.Instances {
		b, err := marshal.Marshal(inst)
		if err != nil {
			return fmt.Errorf("marshal instance %q: %w", id, err)
		}
		instancesJSON[id] = b
	}
	deploymentsJSON := make(map[string]json.RawMessage, len(file.Deployments))
	for id, dep := range file.Deployments {
		b, err := marshal.Marshal(dep)
		if err != nil {
			return fmt.Errorf("marshal deployment %q: %w", id, err)
		}
		deploymentsJSON[id] = b
	}
	envelope := struct {
		SchemaVersion string                     `json:"schema_version"`
		Backend       string                     `json:"backend"`
		OperatorID    string                     `json:"operator_id"`
		Instances     map[string]json.RawMessage `json:"instances"`
		Deployments   map[string]json.RawMessage `json:"deployments"`
	}{
		SchemaVersion: file.SchemaVersion,
		Backend:       file.Backend,
		OperatorID:    file.OperatorID,
		Instances:     instancesJSON,
		Deployments:   deploymentsJSON,
	}
	out, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state envelope: %w", err)
	}
	// Atomic write: temp file in same dir, then rename. Rename within
	// one filesystem is atomic on Linux and macOS; a crash mid-write
	// leaves either the old file or the new file, never a torn one.
	tmp, err := os.CreateTemp(s.dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename %q to %q: %w", tmpPath, s.path, err)
	}
	return nil
}

func (s *Store) emptyFile() *File {
	return &File{
		SchemaVersion: SchemaVersion,
		Backend:       BackendLocalFile,
		OperatorID:    s.operatorID,
		Instances:     map[string]*provisionerv1.Instance{},
		Deployments:   map[string]*provisionerv1.Deployment{},
	}
}
