package runpod

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// stageWithFastPoll runs StageModel with a sub-millisecond poll cadence
// so tests exercise the poll/terminate loop without sleeping between
// ticks.
func stageWithFastPoll(t *testing.T, p *Provider, spec provisioners.StageSpec) error {
	t.Helper()
	p.stageInterval = time.Millisecond
	p.stageBudget = 5 * time.Second
	return p.StageModel(context.Background(), spec)
}

func TestEnsureVolume_CreatesWhenAbsent(t *testing.T) {
	var createBody networkVolumeCreate
	f, p := newFake(t, func(method, path string, body []byte) (int, string) {
		switch {
		case method == "GET" && path == "/networkvolumes":
			return 200, `[]`
		case method == "POST" && path == "/networkvolumes":
			_ = json.Unmarshal(body, &createBody)
			return 201, `{"id":"vol-new","name":"iplane-cache-EU-RO-1","size":100,"dataCenterId":"EU-RO-1"}`
		}
		t.Errorf("unexpected %s %s", method, path)
		return 500, "{}"
	})
	ref, err := p.EnsureVolume(context.Background(), provisioners.VolumeSpec{
		Name: "iplane-cache-EU-RO-1", Region: "EU-RO-1", SizeGB: 100,
	})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if ref.ID != "vol-new" || ref.Region != "EU-RO-1" {
		t.Errorf("ref = %+v, want id vol-new / region EU-RO-1", ref)
	}
	if createBody.Name != "iplane-cache-EU-RO-1" || createBody.Size != 100 || createBody.DataCenterID != "EU-RO-1" {
		t.Errorf("create body = %+v, want name/size/dc set", createBody)
	}
	_ = f
}

func TestEnsureVolume_ReusesExistingByNameAndRegion(t *testing.T) {
	posted := false
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		switch {
		case method == "GET" && path == "/networkvolumes":
			return 200, `[{"id":"vol-old","name":"iplane-cache-EU-RO-1","size":100,"dataCenterId":"EU-RO-1"},
			              {"id":"vol-other","name":"iplane-cache-EU-RO-1","size":50,"dataCenterId":"US-CA-1"}]`
		case method == "POST" && path == "/networkvolumes":
			posted = true
			return 201, `{"id":"vol-dup"}`
		}
		return 500, "{}"
	})
	ref, err := p.EnsureVolume(context.Background(), provisioners.VolumeSpec{
		Name: "iplane-cache-EU-RO-1", Region: "EU-RO-1", SizeGB: 100,
	})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if ref.ID != "vol-old" {
		t.Errorf("ref.ID = %q, want vol-old (region match); vol-other is the wrong region", ref.ID)
	}
	if posted {
		t.Error("EnsureVolume created a duplicate volume instead of reusing the existing one")
	}
}

func TestStageModel_PollsLogsForDoneThenDeletesPod(t *testing.T) {
	var stageCmd string
	var deleted string
	polls := 0
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		switch {
		case method == "POST" && path == "/pods":
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			stageCmd = strings.Join(req.DockerStartCmd, " ")
			if req.NetworkVolumeID != "vol-1" || req.ComputeType != stageComputeType {
				t.Errorf("stage pod req = %+v, want vol-1 + CPU", req)
			}
			return 201, `{"id":"stage-pod","desiredStatus":"RUNNING"}`
		case method == "GET" && path == "/v2/pods/stage-pod/logs":
			polls++
			if polls < 2 {
				// container still downloading -- marker not printed yet.
				return 200, `data: {"source":"container","line":"downloading shards..."}`
			}
			return 200, `data: {"source":"container","line":"` + stageSentinelDone + `"}`
		case method == "DELETE" && path == "/pods/stage-pod":
			deleted = "stage-pod"
			return 200, "{}"
		}
		t.Errorf("unexpected %s %s", method, path)
		return 500, "{}"
	})
	// Zero the poll interval so the test doesn't sleep 10s between polls.
	p2 := p
	err := stageWithFastPoll(t, p2, provisioners.StageSpec{
		VolumeID: "vol-1", Region: "EU-RO-1", Model: "Qwen/Qwen2.5-32B-Instruct-AWQ", MountPath: "/models",
	})
	if err != nil {
		t.Fatalf("StageModel: %v", err)
	}
	if !strings.Contains(stageCmd, "hf download Qwen/Qwen2.5-32B-Instruct-AWQ") {
		t.Errorf("stage cmd missing download: %q", stageCmd)
	}
	if !strings.Contains(stageCmd, stageSentinelDone) || !strings.Contains(stageCmd, stageSentinelFail) {
		t.Errorf("stage cmd missing completion markers: %q", stageCmd)
	}
	if deleted != "stage-pod" {
		t.Error("staging pod was not torn down after completion")
	}
}

func TestStageModel_FailMarkerErrors(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		switch {
		case method == "POST" && path == "/pods":
			return 201, `{"id":"stage-pod","desiredStatus":"RUNNING"}`
		case method == "GET" && path == "/v2/pods/stage-pod/logs":
			return 200, `data: {"source":"container","line":"` + stageSentinelFail + `"}`
		case method == "DELETE" && path == "/pods/stage-pod":
			return 200, "{}"
		}
		return 500, "{}"
	})
	err := stageWithFastPoll(t, p, provisioners.StageSpec{VolumeID: "vol-1", Region: "EU-RO-1", Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "failure") {
		t.Errorf("StageModel err = %v, want a staging-failure error", err)
	}
}

func TestStageSignal(t *testing.T) {
	done, failed := stageSignal([]byte("pip install ...\n" + stageSentinelDone + "\n"))
	if !done || failed {
		t.Errorf("done marker: done=%v failed=%v, want true/false", done, failed)
	}
	done, failed = stageSignal([]byte("Traceback ...\n" + stageSentinelFail + "\n"))
	if done || !failed {
		t.Errorf("fail marker: done=%v failed=%v, want false/true", done, failed)
	}
	// Failure wins if somehow both appear, so a failed run is never
	// mistaken for success.
	done, failed = stageSignal([]byte(stageSentinelDone + " " + stageSentinelFail))
	if done || !failed {
		t.Errorf("both markers: done=%v failed=%v, want false/true", done, failed)
	}
	if done, failed := stageSignal([]byte("still downloading shards")); done || failed {
		t.Errorf("no marker: done=%v failed=%v, want false/false", done, failed)
	}
}
