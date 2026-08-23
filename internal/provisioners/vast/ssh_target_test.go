package vast

import "testing"

// Absent and empty must not be the same answer. Vast assigns ssh_host
// seconds to minutes after the contract is created, so an early tick sees
// nothing; reporting a zero-valued target then would record an address of
// "" on the instance and make a machine that is about to be reachable look
// permanently unreachable.
func TestSSHTargetFromReportsNilUntilTheAddressExists(t *testing.T) {
	if got := sshTargetFrom(nil); got != nil {
		t.Fatalf("nil record produced %+v, want nil", got)
	}
	if got := sshTargetFrom(&apiInstance{SSHPort: 20996}); got != nil {
		t.Fatalf("record with no host produced %+v, want nil", got)
	}
}

func TestSSHTargetFromCarriesHostAndPort(t *testing.T) {
	got := sshTargetFrom(&apiInstance{SSHHost: "ssh2.vast.ai", SSHPort: 20996})
	if got == nil {
		t.Fatal("an assigned address produced no target")
	}
	if got.GetHost() != "ssh2.vast.ai" || got.GetPort() != 20996 {
		t.Fatalf("got %s:%d, want ssh2.vast.ai:20996", got.GetHost(), got.GetPort())
	}
	if got.GetUser() != "root" {
		t.Fatalf("user = %q, want root", got.GetUser())
	}
}
