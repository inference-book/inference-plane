package provisioners

import (
	"fmt"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// vLLM's names for the two splits.
//
// Hardcoded to one engine on purpose. vLLM says tensor-parallel-size, TGI says
// num-shard, SGLang says tp-size, and a neutral vocabulary mapped onto each is
// the multi-engine adapter registry this project has already decided not to
// hand-roll ahead of a second engine existing. Every deploy path today builds
// `--model / --host / --port` the same way, on the same assumption that the
// image is OpenAI-compatible, so this adds no assumption that was not already
// there.
//
// When a second engine lands, the friction is right here and the mapping earns
// itself then.
const (
	vllmTensorParallelFlag   = "--tensor-parallel-size"
	vllmPipelineParallelFlag = "--pipeline-parallel-size"
)

// ValidateParallelism checks that a requested split fits the cards the
// deployment will actually get, and returns the engine arguments that express
// it.
//
// gpuCount is the cards on one instance. Multi-node members do not exist yet
// (issue 212), so a split has to fit inside one machine, and the error says so
// rather than leaving an operator to infer it from an arithmetic failure.
//
// cardsKnown is false for an engine iplane did not provision. The operator runs
// it, we never saw its hardware, and refusing a four-way split because we
// cannot count the cards would be inventing a limit on somebody else's machine.
// The arguments still go through, because passing them on is the whole job for
// an attached engine. Same boundary that stops `fleet drain` releasing hardware
// iplane did not rent.
//
// The check earns its place by being one the engine cannot make in time.
// vLLM discovers a world size larger than the visible device count when it
// starts, which is after the rental began billing and several minutes after
// the operator typed the command. The control plane knows the card count
// before it rents anything.
//
// What is deliberately NOT checked: whether the tensor-parallel size divides
// the model's attention-head count. That is a fact about the weights, the
// control plane never reads them, and guessing would reject valid deployments
// on a rule it cannot actually evaluate.
func ValidateParallelism(p *provisionerv1.Parallelism, gpuCount int32, cardsKnown bool) ([]string, error) {
	tp, pp := p.GetTensorParallelSize(), p.GetPipelineParallelSize()
	if tp == 0 && pp == 0 {
		return nil, nil
	}
	if tp < 0 || pp < 0 {
		return nil, fmt.Errorf("parallelism: sizes cannot be negative (tp=%d, pp=%d)", tp, pp)
	}
	// An unset dimension is one way, not zero ways.
	if tp == 0 {
		tp = 1
	}
	if pp == 0 {
		pp = 1
	}

	if !cardsKnown {
		return parallelismArgs(tp, pp), nil
	}
	cards := gpuCount
	if cards <= 0 {
		cards = 1
	}
	if want := tp * pp; want > cards {
		return nil, fmt.Errorf(
			"parallelism: tensor_parallel_size %d x pipeline_parallel_size %d needs %d cards and the deployment asks for %d; "+
				"raise --gpu-count or lower the split (a split cannot span instances yet)",
			tp, pp, want, cards)
	}

	return parallelismArgs(tp, pp), nil
}

// parallelismArgs renders the split as engine flags, omitting a one-way
// dimension because that is what the engine does unasked.
func parallelismArgs(tp, pp int32) []string {
	var args []string
	if tp > 1 {
		args = append(args, fmt.Sprintf("%s=%d", vllmTensorParallelFlag, tp))
	}
	if pp > 1 {
		args = append(args, fmt.Sprintf("%s=%d", vllmPipelineParallelFlag, pp))
	}
	return args
}

// deploymentGPUCount reports how many cards one replica of this deployment
// gets, which is what a split has to fit inside.
//
// Reads the first replica spec rather than summing them. Replicas are
// independent engines and a split lives inside one of them, so five replicas
// of a four-card shape is five separate four-way splits and never a
// twenty-way one.
func deploymentGPUCount(specs []*provisionerv1.ReplicaSpec) (cards int32, known bool) {
	for _, s := range specs {
		// An attached engine carries an endpoint and no requirements: the
		// operator's hardware, which we never chose and cannot count.
		if s.GetEngineEndpoint() != "" {
			return 0, false
		}
		if s.GetRequirements() != nil {
			return s.GetRequirements().GetGpuCount(), true
		}
	}
	// No specs at all is the pinned-instance form, where the instance was
	// chosen elsewhere and its shape is not on this request.
	return 0, len(specs) > 0
}
