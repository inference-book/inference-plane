# 0007 — Live GPU validation findings

**Status:** Findings (no code in this doc)
**Phase:** v0.2 Ch 10, epic #211
**Depends on:** [0006-ch10-provider-reality-and-control-channel.md](0006-ch10-provider-reality-and-control-channel.md) (the experiments this settles)
**Blocks:** unblocks #213; constrains 204b and #214

## What this settles

Two of `0006`'s three open experiments settled on one rented box, plus the first real-hardware check of anything shipped for #203. The third (Vast renter-side co-located rental, for #212) is untouched and still open. **2x RTX A6000 on Vast, $0.639/hr, ~$0.10 total.** Zero instances left running; verified against both provider APIs afterward.

The A6000 was chosen deliberately over a cheaper single-GPU box: it is `CapabilityOptional` in our catalog with a measured bridge, which is the exact case 203b's measured tier was built to rescue.

### 1. NVML answers inside a provider container. #213 is unblocked.

This was the load-bearing unknown. `#213` is built entirely on it and nobody had checked.

```
### nvidia-smi nvlink -s
GPU 0: NVIDIA RTX A6000 (UUID: GPU-f3aa01f8-...)
	 Link 0: 14.062 GB/s
	 Link 1: 14.062 GB/s
	 Link 2: 14.062 GB/s
	 Link 3: 14.062 GB/s
GPU 1: NVIDIA RTX A6000 (UUID: GPU-6c620167-...)
	 Link 0: 14.062 GB/s
	 ...

### nvidia-smi nvlink -e
	 Link 0: Replay Errors: 0
	 Link 0: Recovery Errors: 0
	 Link 0: CRC Errors: 0
```

Both per-link **state** and the **error counters** #213 names are readable from inside the container with no special privileges. The sensor is buildable.

### 2. The #203 fabric catalog matches real hardware.

First real-hardware check of anything shipped in #219/#221. iplane stamped:

```
fabric_scope:      FABRIC_SCOPE_INTRA_NODE
fabric_source:     FABRIC_SOURCE_MEASURED
fabric_gbps:       449
fabric_technology: nvlink
```

Ground truth on the box:

```
        GPU0  GPU1
GPU0     X    NV4
GPU1    NV4    X
```

`NV4` is four NVLink connections. 4 links x 14.062 GB/s = **56.25 GB/s**, which is exactly Vast's `bw_nvlink` of 56.248, and 56.25 x 8 = **450 Gbps** against our stamped 449. The measured tier, the GB/s-to-gigabit conversion, and the server-side `bw_nvlink` filter are all correct end to end on real hardware.

Note what this validated: a card whose SKU name says nothing about NVLink, correctly identified as NVLinked **because the provider measured it**. On RunPod the same card would have resolved to `UNKNOWN` and been refused, which is the intended asymmetry.

### 3. `bw_nvlink` is per-machine, effectively.

Across 800 rentable multi-GPU offers spanning 624 machines, exactly **one** machine reported differing `bw_nvlink` across its own offers (318.7 vs 478.1, machine 143870). So the reading is a machine property and caching or reasoning per-machine is safe, with the caveat that a machine partitioned into differently-sized rentals can report differently per slice.

### 4. A box cannot see its own provider identity. Matters for 204b and #214.

```
hostname -> 90d38788883e        (a docker container id)
env      -> no VAST_*, MACHINE_*, or HOST_* variables
```

An agent running on the box has **no way to discover the provider's machine id from inside**. #214's failure attribution depends on that identity, so it must be **injected at deploy time** from what the control plane already knows (`Deployment.Env` already carries `HF_TOKEN` and the OTLP settings by the same mechanism).

Consequence for 204b: the agent's span is partly *told to it* rather than discovered. Card count and link health it can read; who and where it is, it cannot.

### 5. Two incidental findings

**Vast deprecated `/api/v0/instances/`** (the collection endpoint) with `410 Gone`. iplane is unaffected: `List` already uses v1, and the per-instance v0 GET and DELETE that `Describe` and `Terminate` use still work. Worth knowing before someone "helpfully" migrates the working paths.

**`iplane instance ssh` cannot run a remote command.** `buildSSHArgv` puts `user@host` last, so anything after `--` is parsed as ssh options and a command string is read as a hostname. Fine for the interactive use it was built for; it blocks scripted use, which is what a demokit walkthrough or a CI check would want.

**Vast's create path does not populate iplane's key store.** Rented instances authenticate against SSH keys registered on the Vast *account*, so `instance create` never calls `EnsureKeyPair` and `--state-dir/keys/` stays empty until something else forces it. Not wrong, but it means the RunPod and Vast key stories differ more than the shared seam suggests.
