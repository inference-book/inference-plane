package provisioners

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/vrambudget"
)

func TestEnginePlanReadsBothFlagForms(t *testing.T) {
	// vLLM accepts --flag=value and --flag value, and operators use
	// both. Reading only one would make the check silently skip half
	// the deploys it exists for.
	for _, args := range [][]string{
		{"--max-model-len=32768", "--max-num-seqs=16", "--quantization=fp8"},
		{"--max-model-len", "32768", "--max-num-seqs", "16", "--quantization", "fp8"},
	} {
		plan, usable := EnginePlan(args, nil)
		if !usable {
			t.Fatalf("%v: no context length found", args)
		}
		if plan.MaxModelLen != 32768 || plan.MaxBatch != 16 || plan.Weights != vrambudget.PrecisionFP8 {
			t.Errorf("%v parsed as %+v", args, plan)
		}
	}
}

func TestEnginePlanIsUnusableWithoutAContextLength(t *testing.T) {
	// The KV term is linear in context, and every other input has a
	// defensible default while this one does not. Substituting a number
	// would refuse deploys the operator never described.
	if _, usable := EnginePlan([]string{"--max-num-seqs=16", "--quantization=fp8"}, nil); usable {
		t.Error("a plan with no context length reported itself usable")
	}
}

func TestEnginePlanMapsKernelNamesOntoWidths(t *testing.T) {
	// An engine's quantization flag names a kernel rather than a width.
	// awq_marlin is awq weights through a faster kernel, and fp8_e4m3
	// differs from fp8_e5m2 in exponent bits and not in bytes.
	cases := map[string]vrambudget.Precision{
		"awq_marlin":  vrambudget.PrecisionAWQ,
		"gptq_marlin": vrambudget.PrecisionGPTQ,
		"fp8_e4m3":    vrambudget.PrecisionFP8,
		"awq":         vrambudget.PrecisionAWQ,
	}
	for in, want := range cases {
		plan, _ := EnginePlan([]string{"--max-model-len=4096", "--quantization=" + in}, nil)
		if plan.Weights != want {
			t.Errorf("--quantization %s parsed as %q, want %q", in, plan.Weights, want)
		}
	}
}

func TestEnginePlanKeepsTheDefaultForAQuantizationItCannotRead(t *testing.T) {
	// A method nobody models must not become a footprint. Leaving fp16
	// in place overstates the weights, which errs toward warning rather
	// than toward promising a fit that is not there.
	plan, _ := EnginePlan([]string{"--max-model-len=4096", "--quantization=some-new-thing"}, nil)
	if plan.Weights != vrambudget.PrecisionFP16 {
		t.Errorf("weights = %q, want the fp16 default left alone", plan.Weights)
	}
}

func TestEnginePlanTreatsAutoCacheDtypeAsTheWeightPrecision(t *testing.T) {
	// "auto" is the engine saying "same as the weights", which is what
	// an empty KVCache already means to the budget.
	plan, _ := EnginePlan([]string{"--max-model-len=4096", "--quantization=fp8", "--kv-cache-dtype=auto"}, nil)
	if plan.KVCache != "" {
		t.Errorf("KVCache = %q, want empty so it follows the weights", plan.KVCache)
	}
}

func TestEnginePlanTakesTheSplitFromParallelismNotTheArgs(t *testing.T) {
	// --tp is already typed and already validated, so the budget sizes
	// against the split the control plane knows rather than re-reading
	// the argument it generated.
	plan, _ := EnginePlan([]string{"--max-model-len=4096"},
		&provisionerv1.Parallelism{TensorParallelSize: 4})
	if plan.TPSize != 4 {
		t.Errorf("TPSize = %d, want 4", plan.TPSize)
	}
}
