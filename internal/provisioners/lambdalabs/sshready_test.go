package lambdalabs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// The Provider carries WithSSHReadyWait and WithSSHProbe, and until this
// existed nothing read either: Lambda did not satisfy SSHReadyWaiter, so the
// Service's wait was a no-op and the deploy path dialled whatever address
// Spawn happened to return.
func TestProviderSatisfiesSSHReadyWaiter(t *testing.T) {
	var _ provisioners.SSHReadyWaiter = (*Provider)(nil)
}

// Lambda assigns the public IP a moment after launch, so the first describe
// answers with an instance and no address. Reporting that as the endpoint
// hands the executor an empty host.
func TestWaitForSSHReady_PollsUntilTheAddressAppears(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances/inst-1", func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		body := apiInstance{ID: "inst-1", Name: "iplane-my-pod", Status: "booting"}
		if n >= 3 {
			body.Status = "active"
			body.IP = "192.0.2.10"
		}
		writeJSON(w, instanceResponse{Data: body})
	})
	p, _ := newTestProvider(t, mux)

	target, err := p.WaitForSSHReady(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("WaitForSSHReady: %v", err)
	}
	if target.GetHost() != "192.0.2.10" || target.GetPort() != 22 || target.GetUser() != "ubuntu" {
		t.Errorf("target = %+v, want host=192.0.2.10 port=22 user=ubuntu", target)
	}
	if calls.Load() < 3 {
		t.Errorf("describe calls = %d, want at least 3 (the address was not there yet)", calls.Load())
	}
}

// A VM that never publishes an address has to fail rather than hang, and the
// error has to say which deadline it was.
func TestWaitForSSHReady_TimesOutWithoutAnAddress(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances/inst-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, instanceResponse{Data: apiInstance{ID: "inst-1", Status: "booting"}})
	})
	p, _ := newTestProvider(t, mux)

	_, err := p.WaitForSSHReady(context.Background(), "inst-1")
	if err == nil {
		t.Fatal("expected a timeout when the address never appears")
	}
}

// An address is not a listener. The port probe is what separates "Lambda
// told us where the box is" from "sshd is answering there", and skipping it
// is how a deploy fails its first SSH instead of waiting.
func TestWaitForSSHReady_WaitsForThePortToAnswer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances/inst-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, instanceResponse{Data: apiInstance{
			ID: "inst-1", Status: "active", IP: "192.0.2.10",
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	var probes atomic.Int32
	p := New(NewClient("test-api-key", WithBaseURL(srv.URL)),
		WithSSHReadyWait(2*time.Second, time.Millisecond),
		WithSSHProbe(func(context.Context, string, int32) error {
			if probes.Add(1) < 3 {
				return errors.New("connection refused")
			}
			return nil
		}))

	if _, err := p.WaitForSSHReady(context.Background(), "inst-1"); err != nil {
		t.Fatalf("WaitForSSHReady: %v", err)
	}
	if probes.Load() < 3 {
		t.Errorf("ssh probes = %d, want at least 3 (the port refused twice)", probes.Load())
	}
}
