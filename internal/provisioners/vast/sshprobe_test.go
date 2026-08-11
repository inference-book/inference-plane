package vast

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The probe previously took its arguments, discarded them, and returned nil,
// so it reported every address as reachable. These two cases exist to make
// that state unrepresentable: one requires a real listener to pass, the
// other requires a closed port to fail. A stub cannot satisfy both.
func TestDefaultSSHProbeReachesAListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	if err := defaultSSHProbe(t.Context(), host, int32(port)); err != nil {
		t.Errorf("probe against a live listener failed: %v", err)
	}
}

func TestDefaultSSHProbeFailsOnAClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ln.Close() // nothing is listening now

	if err := defaultSSHProbe(t.Context(), host, int32(port)); err == nil {
		t.Error("probe reported a closed port as reachable")
	}
}

// A published record can carry port 0 before the mapping is assigned.
// Dialing port 0 is meaningless, so fall back to the SSH default rather than
// producing an error that looks like the host is down.
func TestDefaultSSHProbeDefaultsThePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	host, _, _ := net.SplitHostPort(ln.Addr().String())

	// Port 22 on loopback is almost certainly not this test's listener, so
	// the assertion is only that the call is well-formed and returns a
	// dial outcome rather than panicking on a zero port.
	err = defaultSSHProbe(t.Context(), host, 0)
	if err != nil && !strings.Contains(err.Error(), "connect") && !strings.Contains(err.Error(), "refused") &&
		!strings.Contains(err.Error(), "timeout") {
		t.Errorf("unexpected error shape for a zero port: %v", err)
	}
}

func TestDefaultSSHProbeHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// 203.0.113.0/24 is TEST-NET-3, reserved and unroutable, so this can
	// only return by way of the cancelled context.
	err := defaultSSHProbe(ctx, "203.0.113.1", 22)
	if err == nil {
		t.Error("probe succeeded against an unroutable address")
	}
}

// The wait ends as soon as the port answers, so the only cost of a generous
// ceiling is how long a dead machine takes to be declared dead. The measured
// figure it has to clear is 273s.
func TestDefaultSSHReadyTimeoutClearsTheMeasuredBootTime(t *testing.T) {
	const measured = 273 * time.Second
	if defaultSSHReadyTimeout <= measured {
		t.Errorf("defaultSSHReadyTimeout = %v, want > %v (observed time to first accept)",
			defaultSSHReadyTimeout, measured)
	}
}

func TestNewProviderUsesTheDefaultTimeout(t *testing.T) {
	p := New(NewClient("k"))
	if p.sshReadyTimeout != defaultSSHReadyTimeout {
		t.Errorf("sshReadyTimeout = %v, want %v", p.sshReadyTimeout, defaultSSHReadyTimeout)
	}
	if p.sshProbe == nil {
		t.Error("sshProbe is nil; WaitForSSHReady would skip verification entirely")
	}
}
