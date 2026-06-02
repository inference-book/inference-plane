// Package file is the file-backed impl of provisioners.Store. The
// state lives at <dir>/state.json (in production, ~/.iplane/state.json)
// and is the persistent intent log for every Spawn / Terminate the
// operator runs.
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
//
// file.Store satisfies provisioners.Store. The envelope type, schema
// version, lock-pid sidecar, and lifetime-flock semantics are all
// file-backend-specific concerns and live here, not on the Store
// interface -- other backends (GORM/SQLite, GAE) have different
// atomicity primitives.
package file

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
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
// gained the deployments map (purely additive). v0.2 ch7-beat1.1 bumped
// to "1.2" -- the Deployment message gained idle_ttl_seconds /
// last_activity_at / no_idle_destroy for the long-lived-daemon's idle
// reaper. v0.2 ch7-beat3.1 bumps to "1.3" -- the Deployment message
// gains replicas (default 1) for the multi-replica fan-out story.
// Still additive; old 1.2 records load with replicas=0 via protojson
// DiscardUnknown, and downstream code treats 0 as 1.
// v0.2 ch7-beat3.5 bumps to "1.4" -- the Deployment message gains
// unhealthy_instance_ids for the per-replica quarantine set written
// by the health-poll loop. Purely additive; old 1.3 records load
// with unhealthy_instance_ids=[] via protojson DiscardUnknown, which
// is exactly the "no replica is quarantined" initial state.
//
// Note: an interim "1.3" briefly existed in a draft of v0.2
// ch7-beat2.3 for Deployment.default_priority; that field was removed
// before merge and the version reverted to "1.2". This "1.3" bump
// is the first one operators see in shipped artifacts.
const SchemaVersion = "1.4"

// BackendLocalFile is the value written into the file's `backend` field.
// v1.0's remote backend will write its own value here and may fall back
// to local-file mode in degraded states -- the field records which
// writer last touched the file.
const BackendLocalFile = "local-file"

// envelope is the on-disk shape of state.json. SchemaVersion, Backend,
// and OperatorID are file-format metadata not part of the cross-backend
// contract (provisioners.Store / provisioners.State). The Service sees
// only the Instances + Deployments tables via provisioners.State; the
// envelope wraps them on disk along with the version stamp.
type envelope struct {
	SchemaVersion string
	Backend       string
	OperatorID    string
	Instances     map[string]*provisionerv1.Instance
	Deployments   map[string]*provisionerv1.Deployment
}

// Store owns the file path and the flock. One Store per CLI invocation
// is the expected usage. Two lifecycle patterns:
//
//   - One-shot: call Update(fn) directly. Each Update acquires the
//     flock, runs fn, writes, releases. Backward-compatible with the
//     pre-v0.2 caller.
//   - Lifetime: call LockForLifetime() at startup, defer the release.
//     Subsequent Update calls skip flock acquisition (the lock is
//     already held at the process level). This is the daemon pattern
//     iplane serve uses so the flock is held for the daemon's lifetime
//     and Update calls inside the daemon do not self-deadlock on a
//     second flock against the same already-held lock.
type Store struct {
	dir        string // directory holding state.json (and the flock file)
	path       string // <dir>/state.json
	operatorID string

	// heldLock is non-nil while LockForLifetime is active. Update
	// checks this field to skip re-acquiring the flock from the same
	// process (the syscall would deadlock against the already-held FD).
	heldLock *os.File
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

// Dir returns the directory that holds state.json and the flock file.
// Callers that need to place sibling artifacts (SSH key store, future
// caches) under the same root use this; passing `Path` + filepath.Dir
// would work but invites the path-string-manipulation footgun.
func (s *Store) Dir() string { return s.dir }

// Read returns the current state snapshot. Suitable for read-only
// callers; mutations through Read are not persisted (use Update for
// that). Returns an empty state with no instances/deployments if the
// state file does not yet exist.
//
// Satisfies provisioners.Store.Read.
func (s *Store) Read() (*provisioners.State, error) {
	env, err := s.readFromDisk()
	if err != nil {
		return nil, err
	}
	return envelopeToState(env), nil
}

// Update runs fn under the exclusive flock. fn receives the current
// state snapshot; any modification fn makes is persisted on return.
// fn must return quickly (it should NOT perform network IO -- the
// lock is held for its entire duration and blocks other CLI
// invocations).
//
// If fn returns a non-nil error, the file is NOT written; the error
// propagates to the caller and the on-disk state is unchanged.
//
// If LockForLifetime is currently active on this Store, Update reuses
// that lock instead of acquiring a second one (the same-process double
// flock would deadlock).
//
// Satisfies provisioners.Store.Update.
func (s *Store) Update(fn func(*provisioners.State) error) error {
	if s.heldLock == nil {
		lockFile, err := s.lock()
		if err != nil {
			return err
		}
		defer s.unlock(lockFile)
	}

	env, err := s.readFromDisk()
	if err != nil {
		return err
	}
	state := envelopeToState(env)
	if err := fn(state); err != nil {
		return err
	}
	// State and envelope share map references (envelopeToState
	// does not copy), so map mutations fn made are already visible
	// in env.Instances / env.Deployments. The defensive reassignment
	// here handles the edge case of fn replacing the maps entirely
	// (state.Instances = newMap).
	env.Instances = state.Instances
	env.Deployments = state.Deployments
	return s.writeToDisk(env)
}

// envelopeToState exposes the cross-backend slice of the envelope --
// just the two record tables. SchemaVersion / Backend / OperatorID
// stay in the envelope; they are file-format metadata and not part
// of the Store interface contract.
func envelopeToState(env *envelope) *provisioners.State {
	return &provisioners.State{
		Instances:   env.Instances,
		Deployments: env.Deployments,
	}
}

// ErrLockHeld is returned by LockForLifetime when another process
// already holds the state-dir flock. HolderPID carries the PID read
// from the lock-pid sidecar file (best-effort; zero if the sidecar
// was unreadable or stale). Callers can errors.As against it to
// surface an actionable message.
type ErrLockHeld struct {
	Path      string // the state directory whose flock is held
	HolderPID int    // 0 if the PID could not be determined
}

func (e *ErrLockHeld) Error() string {
	if e.HolderPID == 0 {
		return fmt.Sprintf("state directory %q is locked by another process", e.Path)
	}
	return fmt.Sprintf("state directory %q is locked by another process at PID %d", e.Path, e.HolderPID)
}

// LockForLifetime takes the exclusive flock and holds it until the
// returned release func runs. Non-blocking: if another process holds
// the lock, returns *ErrLockHeld immediately with the holder's PID
// (read from the lock-pid sidecar; best-effort).
//
// After successful acquisition, subsequent Update calls on this Store
// skip flock acquisition (the lock is already held). The release func
// is idempotent -- calling it twice is safe -- and clears the
// heldLock field, removes the lock-pid sidecar, and releases the
// flock. Always defer the release.
//
// Daemon pattern:
//
//	store, _ := file.Open(dir, operatorID)
//	release, err := store.LockForLifetime()
//	if err != nil { ... }
//	defer release()
//	// ... use store.Update freely; flock stays held the whole time ...
//
// One-shot CLI pattern: same shape, but the release runs as the CLI
// exits. The fail-fast non-blocking acquisition is what makes "iplane
// serve is running" surface as a clean error rather than the CLI
// hanging on the flock.
func (s *Store) LockForLifetime() (release func(), err error) {
	if s.heldLock != nil {
		return func() {}, fmt.Errorf("state: LockForLifetime called twice without release on %q", s.dir)
	}
	lockPath := filepath.Join(s.dir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &ErrLockHeld{Path: s.dir, HolderPID: readHolderPID(s.dir)}
		}
		return nil, fmt.Errorf("flock %q: %w", lockPath, err)
	}
	// Write our PID into the sidecar so the next contender can name
	// us in their error. Best-effort: a write failure doesn't undo
	// the lock acquisition (the flock is the truth; the sidecar is
	// informational).
	_ = writeHolderPID(s.dir, os.Getpid())
	s.heldLock = f

	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			_ = removeHolderPID(s.dir)
			s.heldLock = nil
			s.unlock(f)
		})
	}, nil
}

// holderPIDPath returns the path to the lock-pid sidecar.
func holderPIDPath(dir string) string {
	return filepath.Join(dir, ".lock-pid")
}

// writeHolderPID stamps the current PID into the lock-pid sidecar.
// Atomic write via temp + rename so a contender either sees the old
// PID or the new one, never a torn write.
func writeHolderPID(dir string, pid int) error {
	tmp, err := os.CreateTemp(dir, ".lock-pid-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := fmt.Fprintf(tmp, "%d\n", pid); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, holderPIDPath(dir))
}

// readHolderPID returns the PID recorded in the lock-pid sidecar, or
// zero if the file is missing, empty, malformed, or unreadable. The
// flock itself is the source of truth for "is the lock held"; this
// is only for error-message attribution.
func readHolderPID(dir string) int {
	raw, err := os.ReadFile(holderPIDPath(dir))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return pid
}

func removeHolderPID(dir string) error {
	err := os.Remove(holderPIDPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
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

func (s *Store) readFromDisk() (*envelope, error) {
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
	file := &envelope{
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

func (s *Store) writeToDisk(file *envelope) error {
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

func (s *Store) emptyFile() *envelope {
	return &envelope{
		SchemaVersion: SchemaVersion,
		Backend:       BackendLocalFile,
		OperatorID:    s.operatorID,
		Instances:     map[string]*provisionerv1.Instance{},
		Deployments:   map[string]*provisionerv1.Deployment{},
	}
}
