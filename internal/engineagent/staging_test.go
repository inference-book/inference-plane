package engineagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A sensor with nothing to watch must report absence, not zero. A caller that
// cannot tell those apart concludes every volume-mounted engine has stalled.
func TestStagingSensorNilReportsUnavailable(t *testing.T) {
	var s *StagingSensor
	got := s.Read()
	if got.GetAvailable() {
		t.Fatalf("nil sensor reported a reading: %+v", got)
	}
	if s.ReadOrNil() != nil {
		t.Fatal("nil sensor handed back a non-nil read func")
	}
	if s.Dir() != "" {
		t.Fatalf("nil sensor named a directory: %q", s.Dir())
	}
}

func TestNewStagingSensorNoDefaultDirReturnsNil(t *testing.T) {
	saved := DefaultCacheDirs
	DefaultCacheDirs = []string{filepath.Join(t.TempDir(), "absent")}
	t.Cleanup(func() { DefaultCacheDirs = saved })

	if got := NewStagingSensor(""); got != nil {
		t.Fatalf("sensor built with no cache present: %+v", got)
	}
}

// An unreadable directory is not an empty one. This is the same distinction
// InterconnectHealth draws, and getting it wrong makes a broken sensor look
// like a download that never started.
func TestStagingSensorUnreadableDirReportsUnavailable(t *testing.T) {
	s := &StagingSensor{dir: filepath.Join(t.TempDir(), "gone"), now: time.Now}
	if got := s.Read(); got.GetAvailable() {
		t.Fatalf("missing directory reported a reading: %+v", got)
	}
}

// The first observation cannot support a rate. It must still report the bytes
// it can see, and must mark the interval as zero so a consumer can tell "no
// rate yet" from "measured zero".
func TestStagingSensorFirstReadHasNoRate(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blobs", "a"), 1024)

	s := &StagingSensor{dir: dir, now: time.Now}
	got := s.Read()
	if !got.GetAvailable() {
		t.Fatal("readable directory reported no reading")
	}
	if got.GetBytesLocal() != 1024 {
		t.Fatalf("bytes_local = %d, want 1024", got.GetBytesLocal())
	}
	if got.GetIntervalSeconds() != 0 {
		t.Fatalf("first read claimed an interval of %v", got.GetIntervalSeconds())
	}
	if got.GetBytesPerSecond() != 0 {
		t.Fatalf("first read invented a rate of %v", got.GetBytesPerSecond())
	}
}

func TestStagingSensorMeasuresRateAcrossReads(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blobs", "a"), 1000)

	clock := time.Unix(1000, 0)
	s := &StagingSensor{dir: dir, now: func() time.Time { return clock }}
	s.Read()

	writeFile(t, filepath.Join(dir, "blobs", "b"), 3000)
	clock = clock.Add(2 * time.Second)

	got := s.Read()
	if got.GetBytesLocal() != 4000 {
		t.Fatalf("bytes_local = %d, want 4000", got.GetBytesLocal())
	}
	if got.GetIntervalSeconds() != 2 {
		t.Fatalf("interval = %v, want 2", got.GetIntervalSeconds())
	}
	if got.GetBytesPerSecond() != 1500 {
		t.Fatalf("rate = %v, want 1500 (3000 bytes over 2s)", got.GetBytesPerSecond())
	}
}

// A download that is genuinely stuck must read as a measured zero, because
// that is the reading an early-abort decision depends on being able to trust.
func TestStagingSensorStallReportsMeasuredZero(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blobs", "a"), 1000)

	clock := time.Unix(1000, 0)
	s := &StagingSensor{dir: dir, now: func() time.Time { return clock }}
	s.Read()
	clock = clock.Add(30 * time.Second)

	got := s.Read()
	if !got.GetAvailable() {
		t.Fatal("stalled download reported no reading")
	}
	if got.GetBytesPerSecond() != 0 {
		t.Fatalf("rate = %v, want 0", got.GetBytesPerSecond())
	}
	if got.GetIntervalSeconds() != 30 {
		t.Fatalf("interval = %v, want 30; a stall must be distinguishable from an unseeded read",
			got.GetIntervalSeconds())
	}
}

// Weights only accumulate while staging. A cache that shrank was evicted or
// cleared, and reporting that as negative throughput would be arithmetic
// presented as a measurement.
func TestStagingSensorShrinkingCacheReportsZeroNotNegative(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blobs", "a"), 5000)

	clock := time.Unix(1000, 0)
	s := &StagingSensor{dir: dir, now: func() time.Time { return clock }}
	s.Read()

	if err := os.Remove(filepath.Join(dir, "blobs", "a")); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)

	got := s.Read()
	if got.GetBytesPerSecond() < 0 {
		t.Fatalf("rate = %v, want no negative throughput", got.GetBytesPerSecond())
	}
	if got.GetBytesLocal() != 0 {
		t.Fatalf("bytes_local = %d, want 0", got.GetBytesLocal())
	}
}

// Directories and symlinks must not be counted. The HF cache is a tree of
// symlinks from snapshots/ into blobs/, so counting them would roughly double
// every reading and report a download as further along than it is.
func TestDirSizeCountsRegularFilesOnce(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blobs", "a"), 2048)
	if err := os.MkdirAll(filepath.Join(dir, "snapshots", "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "snapshots", "main", "a.safetensors")
	if err := os.Symlink(filepath.Join(dir, "blobs", "a"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := dirSize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2048 {
		t.Fatalf("dirSize = %d, want 2048 (the symlink must not be counted again)", got)
	}
}
