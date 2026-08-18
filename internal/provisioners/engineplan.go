package provisioners

import (
	"strconv"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/vrambudget"
)

// Engine flags that name a budget input. vLLM's spellings, for the same
// reason ValidateParallelism hardcodes its two: one engine exists, and a
// neutral vocabulary mapped onto each is the multi-engine adapter
// registry this project has decided not to hand-roll ahead of a second
// engine.
const (
	vllmQuantizationFlag = "--quantization"
	vllmKVCacheDtypeFlag = "--kv-cache-dtype"
	vllmMaxModelLenFlag  = "--max-model-len"
	vllmMaxNumSeqsFlag   = "--max-num-seqs"
)

// EnginePlan reads the deploy plan back out of the arguments already
// being forwarded to the engine, and reports whether enough of it is
// present to compute a budget.
//
// Reading what is passed rather than adding typed deploy flags for the
// same facts. Promoting them would fork the vocabulary for every engine
// that spells them differently and put the control plane in the business
// of knowing engine CLIs, which is the line the engine-stays-opaque rule
// draws and which capabilities.yaml records as deliberate for the
// engine-args pass-through. Parsing catches the same mistake without
// crossing it.
//
// usable is false when the context length is absent, because the KV term
// is linear in it and every other input has a defensible default while
// this one does not. A caller that gets false must skip the check rather
// than substitute a number, since inventing a context is how a budget
// starts refusing deploys the operator never described.
//
// Arguments nobody models here are ignored and travel on untouched.
func EnginePlan(args []string, par *provisionerv1.Parallelism) (plan vrambudget.Plan, usable bool) {
	plan = vrambudget.Plan{
		Weights:  vrambudget.PrecisionFP16,
		MaxBatch: 1,
		TPSize:   par.GetTensorParallelSize(),
		EPSize:   par.GetExpertParallelSize(),
	}

	for i := 0; i < len(args); i++ {
		flag, value := args[i], ""
		if eq := strings.IndexByte(flag, '='); eq >= 0 {
			flag, value = flag[:eq], flag[eq+1:]
		} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			value = args[i+1]
			i++
		}
		if value == "" {
			continue
		}

		switch flag {
		case vllmQuantizationFlag:
			if p, err := vrambudget.ParsePrecision(normalizeEngineQuant(value)); err == nil {
				plan.Weights = p
			}
		case vllmKVCacheDtypeFlag:
			// "auto" is the engine's way of saying "same as the weights",
			// which is what an empty KVCache already means here.
			if value != "auto" {
				if p, err := vrambudget.ParsePrecision(value); err == nil {
					plan.KVCache = p
				}
			}
		case vllmMaxModelLenFlag:
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				plan.MaxModelLen = int32(n)
				usable = true
			}
		case vllmMaxNumSeqsFlag:
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				plan.MaxBatch = int32(n)
			}
		}
	}
	return plan, usable
}

// normalizeEngineQuant maps the method names an engine takes onto the
// precision ladder the budget reasons about.
//
// An engine's quantization flag names a kernel, not a width: awq_marlin
// and gptq_marlin are the same weights as awq and gptq loaded through a
// faster kernel, and fp8_e4m3 and fp8_e5m2 differ in exponent bits and
// not in bytes. A budget cares only about the byte count, so mapping
// these is a translation rather than a guess. Anything unrecognised is
// returned unchanged and fails ParsePrecision, which leaves the default
// in place rather than inventing a footprint.
func normalizeEngineQuant(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch {
	case strings.HasPrefix(v, "awq"):
		return "awq"
	case strings.HasPrefix(v, "gptq"):
		return "gptq"
	case strings.HasPrefix(v, "fp8"):
		return "fp8"
	}
	return v
}
