package vast

import "testing"

// A provider that says nothing and a box that has used nothing are different
// facts, and only one of them is worth acting on. A scheduled contract
// reports zeros for everything, which must not read as a live box sitting
// idle.
func TestUsageFromReportsNothingWhenTheHostSaysNothing(t *testing.T) {
	if got := usageFrom(nil); got != nil {
		t.Fatalf("nil record produced %+v, want nil", got)
	}
	if got := usageFrom(&apiInstance{}); got != nil {
		t.Fatalf("all-zero record produced %+v, want nil", got)
	}
}

// The units are the point. Vast reports disk in GB and billed traffic in MB;
// a caller comparing against a model's download size works in bytes, and
// getting this wrong reports a 474 GB fetch as complete after 474 bytes.
func TestUsageFromNormalisesToBytes(t *testing.T) {
	got := usageFrom(&apiInstance{DiskUsage: 0.188, GPUUtil: 86.5, InetDownBilled: 5713.92})
	if got == nil {
		t.Fatal("a reporting host produced no usage")
	}
	if !got.GetAvailable() {
		t.Error("usage present but marked unavailable")
	}
	if want := int64(0.188 * 1e9); got.GetDiskUsedBytes() != want {
		t.Errorf("disk = %d bytes, want %d (0.188 GB)", got.GetDiskUsedBytes(), want)
	}
	if got.GetGpuUtilization() != 86.5 {
		t.Errorf("gpu = %v, want 86.5", got.GetGpuUtilization())
	}
	if want := int64(5713.92 * 1e6); got.GetNetworkRxBytes() != want {
		t.Errorf("rx = %d bytes, want %d (5713.92 MB)", got.GetNetworkRxBytes(), want)
	}
}

// One field alone is still a reading. A box burning GPU with an untouched
// disk is exactly the case this exists to surface, so it must not be
// discarded for looking mostly empty.
func TestUsageFromKeepsAPartialReading(t *testing.T) {
	got := usageFrom(&apiInstance{GPUUtil: 86.5})
	if got == nil {
		t.Fatal("busy GPUs with an idle disk were discarded; that is the hung-collective signature")
	}
	if got.GetDiskUsedBytes() != 0 {
		t.Errorf("disk = %d, want 0", got.GetDiskUsedBytes())
	}
}
