package engineagent

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Weight staging: the reading that makes the download visible while it is
// happening rather than after it has already cost an hour.
//
// The engine says nothing during this window. vLLM hands snapshot_download a
// disabled tqdm, so a container pulling a 474 GB checkpoint prints not one
// line until the fetch completes, and then prints how long it took. Two
// GLM-5.2 deploys sat in that silence for an hour each and hit the
// engine-ready timeout, roughly $22 apiece, with no record of whether they
// were slow at the network or slow at the load.
//
// Nothing else can see it either. The engine has no endpoint yet, so no probe
// answers; the provider reports only that the container is running. The agent
// is the one thing already inside that window: AgentPrelude starts it
// backgrounded before the engine execs, and it registers as ASSEMBLING on a
// cadence throughout.
//
// This is the sensor. What consumes it, and specifically whether a deploy
// should give up early when a measured rate projects past its deadline, is
// deliberately elsewhere (#413): abandoning a rental is a decision with a bill
// attached, and it belongs on the deploy path rather than in a reporter.

// DefaultCacheDirs are where a Hugging Face cache lands when nobody said
// otherwise. Checked in order; the first that exists is measured.
//
// HF_HOME wins when set, and vLLM's --download-dir wins over that, which is
// why NewStagingSensor takes the directory rather than deciding for itself.
var DefaultCacheDirs = []string{
	"/root/.cache/huggingface",
	"/data/.cache/huggingface",
}

// StagingSensor measures how fast model weights are landing on this node.
//
// Stateful on purpose: a rate needs two observations, and the agent is the
// only thing that persists across ticks. Safe for concurrent use, though the
// agent calls it from one goroutine.
type StagingSensor struct {
	dir string
	now func() time.Time

	mu       sync.Mutex
	lastAt   time.Time
	lastSize int64
	seeded   bool
}

// NewStagingSensor returns a sensor watching dir, or nil when dir is empty
// and no default cache directory exists.
//
// A nil sensor is a supported state and reads as "no reading", so a caller
// need not branch on it beyond passing it along. That matters because the
// same agent binary runs next to engines that stage weights and engines that
// were handed a warm volume and stage nothing.
func NewStagingSensor(dir string) *StagingSensor {
	if dir == "" {
		for _, d := range DefaultCacheDirs {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				dir = d
				break
			}
		}
	}
	if dir == "" {
		return nil
	}
	return &StagingSensor{dir: dir, now: time.Now}
}

// Read reports the bytes on disk and the rate since the previous call.
//
// Never returns nil and never reports a rate it did not measure. An
// unreadable directory is available=false, the same answer ReadInterconnect
// gives for a missing nvidia-smi, and for the same reason: a legible gap
// beats a fabricated zero, and a node whose cache cannot be read must not be
// mistaken for one whose download has stalled.
//
// The first call establishes the baseline and reports available=true with a
// zero rate over a zero interval, because one observation genuinely cannot
// support a rate. Callers distinguish that from a measured stall by looking
// at interval_seconds rather than at the rate.
//
// A shrinking directory reports a zero rate rather than a negative one.
// Weights only accumulate during staging, so a decrease means files were
// evicted or the cache was cleared, and calling that negative throughput
// would be arithmetic dressed up as a measurement.
func (s *StagingSensor) Read() *provisionerv1.StagingProgress {
	if s == nil {
		return &provisionerv1.StagingProgress{Available: false}
	}
	size, err := dirSize(s.dir)
	if err != nil {
		return &provisionerv1.StagingProgress{Available: false}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	out := &provisionerv1.StagingProgress{Available: true, BytesLocal: size}
	if s.seeded {
		elapsed := now.Sub(s.lastAt).Seconds()
		if elapsed > 0 {
			out.IntervalSeconds = elapsed
			if delta := size - s.lastSize; delta > 0 {
				out.BytesPerSecond = float64(delta) / elapsed
			}
		}
	}
	s.lastAt, s.lastSize, s.seeded = now, size, true
	return out
}

// dirSize sums the apparent size of every regular file under root.
//
// Walk errors on individual entries are skipped rather than failing the whole
// reading. A cache being written into races the walk by construction: a blob
// can be renamed out from under it mid-download, and letting that turn the
// whole measurement into "no reading" would blind us precisely when the
// download is most active. Only a root that cannot be walked at all is a
// failure.
//
// Apparent size rather than blocks allocated, because the number is compared
// against the model's size as the hub reports it, and sparse or
// partially-allocated files would otherwise read as further along than they
// are.
func dirSize(root string) (int64, error) {
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		if err != nil {
			return 0, err
		}
		return 0, fs.ErrInvalid
	}
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// Dir reports the directory being measured, for the log line that tells an
// operator which cache the numbers describe. Empty for a nil sensor.
func (s *StagingSensor) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// ReadOrNil returns Read as a bound function, or nil for a nil sensor, so a
// caller can hand it to WithStaging without branching.
//
// Read already handles a nil receiver, but handing WithStaging a non-nil
// function that always reports unavailable would make the agent claim its
// sensor failed on every renewal. There was nothing to sense, which is a
// different thing, and the fleet view should say so by staying silent.
func (s *StagingSensor) ReadOrNil() func() *provisionerv1.StagingProgress {
	if s == nil {
		return nil
	}
	return s.Read
}
