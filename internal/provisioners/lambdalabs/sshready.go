package lambdalabs

import (
	"context"
	"fmt"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// WaitForSSHReady satisfies provisioners.SSHReadyWaiter: it blocks until
// Lambda has published the VM's public address and something is answering
// on tcp/22 there.
//
// Lambda's launch call returns instance ids and nothing else, and the
// describe that follows can come back with a booting instance and an empty
// `ip`. Without a wait the Service hands that address straight to the
// sshdocker executor, whose first connection then fails against an empty
// host on a machine that was about to be fine.
//
// Two stages under one budget, matching runpod's. Stage one polls describe
// until `ip` is non-empty; stage two probes the port, because an assigned
// address says the VM exists and says nothing about sshd being up yet. Both
// share sshReadyTimeout so the caller's wait is one deadline rather than
// two that compound.
//
// On timeout it returns the last target it managed to observe alongside the
// error, so a caller that wants to report where it got to can. A nil target
// means no address was ever published.
func (p *Provider) WaitForSSHReady(ctx context.Context, providerID string) (*provisionerv1.SshTarget, error) {
	if providerID == "" {
		return nil, provisioners.NewProviderError(p.Name(), "wait_ssh_ready",
			fmt.Errorf("provider id is required"), 0)
	}
	timeout := p.sshReadyTimeout
	if timeout <= 0 {
		// Always allow one lookup: a caller that disabled polling still
		// wants a best-effort answer rather than an instant timeout.
		timeout = time.Second
	}
	interval := p.sshReadyInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)

	var last *provisionerv1.SshTarget
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return last, ctx.Err()
			case <-time.After(interval):
			}
		}
		first = false
		if time.Now().After(deadline) {
			return last, provisioners.NewProviderError(p.Name(), "wait_ssh_ready",
				fmt.Errorf("public ip not assigned within %s", timeout), 0)
		}
		api, err := p.describeOne(ctx, providerID)
		if err != nil {
			return last, wrapErr("wait_ssh_ready", err)
		}
		if api.IP == "" {
			continue
		}
		last = sshTargetFor(api.IP)
		if err := p.waitForSSHPort(ctx, last, deadline, interval); err != nil {
			return last, err
		}
		return last, nil
	}
}

// waitForSSHPort retries the TCP probe until it answers or the shared
// deadline fires. A successful probe means sshd is listening; the executor's
// handshake is the next step and is not this function's business.
func (p *Provider) waitForSSHPort(ctx context.Context, target *provisionerv1.SshTarget, deadline time.Time, interval time.Duration) error {
	if p.sshProbe == nil {
		return nil
	}
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
		first = false
		if time.Now().After(deadline) {
			return provisioners.NewProviderError(p.Name(), "wait_ssh_ready",
				fmt.Errorf("tcp/%d on %s not reachable before deadline", target.GetPort(), target.GetHost()), 0)
		}
		if err := p.sshProbe(ctx, target.GetHost(), target.GetPort()); err == nil {
			return nil
		}
	}
}

// sshTargetFor builds the endpoint for a Lambda VM. Lambda gives every
// instance a real public address on the standard port, with no NAT and no
// per-instance port mapping to read back, and `ubuntu` is the user on every
// image it ships.
func sshTargetFor(ip string) *provisionerv1.SshTarget {
	return &provisionerv1.SshTarget{Host: ip, Port: 22, User: sshUser}
}
