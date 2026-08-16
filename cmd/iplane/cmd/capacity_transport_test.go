package cmd

import (
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// #304. Pointing a capacity question at a remote control plane must reach it.
//
// The old code built an in-process service unconditionally and never looked at
// --service-url, so it answered from the CLI host: provider credentials live
// in the daemon's environment, and a host without keys reported "no capacity"
// for vendors the daemon could see. A confident wrong answer, in the one
// command whose whole purpose is telling "nobody looked" apart from "nothing
// available".
func TestCapacityClientIsRemoteWhenServiceURLIsSet(t *testing.T) {
	prev := instanceServiceURL
	instanceServiceURL = "http://remote.example"
	t.Cleanup(func() { instanceServiceURL = prev })

	client, err := buildCapacityClient()
	if err != nil {
		t.Fatalf("buildCapacityClient: %v", err)
	}

	if _, local := client.(*provisioners.Service); local {
		t.Error("built an in-process service while --service-url was set; the question would be answered on the wrong host")
	}
}

// The local path stays local, and stays lock-free. Taking the state lock here
// would make the command fail exactly while a daemon is running, which is when
// an operator most wants to ask whether there is capacity to scale onto.
func TestCapacityClientIsLocalWithoutAServiceURL(t *testing.T) {
	prev := instanceServiceURL
	instanceServiceURL = ""
	t.Cleanup(func() { instanceServiceURL = prev })

	client, err := buildCapacityClient()
	if err != nil {
		t.Fatalf("buildCapacityClient: %v", err)
	}

	if _, local := client.(*provisioners.Service); !local {
		t.Errorf("got %T, want the in-process service when no --service-url is set", client)
	}
}
