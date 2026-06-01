# iplane instance CLI end-to-end

Walk through the v0.1 provisioner lifecycle from the operator's terminal -- the `iplane instance` command set. Zero cost (local provider; the laptop provisions itself, no API calls leave the machine).

## Walkthrough

### Setup

This walkthrough drives the iplane binary directly -- no separate `iplane serve` running. The CLI opens the state file under flock, instantiates provider adapters in-process, and prints the result.
Binary:    /var/folders/d9/05hxl2g557ddtw7bmg33_lndn40p3p/T/iplane-cli-example-3975075282/iplane
Provider:  local
State dir: /tmp/iplane-cli-example
Demo id:   cli-demo (class=small)
All commands are what you would type in a real shell. Defer-terminates on exit or Ctrl-C.

### 1. Check the CLI is wired

```bash
iplane instance list --state-dir /tmp/iplane-cli-example
```

```
  (no instances)
```

### 2. Create with --class small

The Service exposes three layers of resource specification (see docs/design/0001-provisioner.md). The walkthrough actually runs the class shorthand below; the variant block also shows the numeric-constraints form and the exact-SKU escape hatch so you can see what each layer expands to.

class=small expands server-side to min_vram_gb=24 / min_disk_gb=20 / min_ram_gb=16. The local resolver picks the cheapest SKU that satisfies those constraints.

#### Three ways to ask for the same shape

```bash
iplane instance create local cli-demo --class small
```

```
  Created instance "cli-demo"
    provider:     local
    provider id:  local:cli-demo
    state:        ACTIVE
    sku:          Apple M4 Pro
```

### 3. Describe (state-file source)

#### Describe sources

```bash
iplane instance describe cli-demo
```

```
  id:            cli-demo
  provider:      local
  provider id:   local:cli-demo
  state:         ACTIVE
  region:        -
  gpu class:     medium
  gpu sku:       Apple M4 Pro
  gpu count:     1
  vram (GB):     48
  hourly rate:   $0.0000/hr
  created at:    2026-05-19T17:36:19Z
  activated at:  2026-05-19T17:36:19Z
```

### 4. Idempotent re-create

Same id; the Service hits its state-file cache, finds an ACTIVE record, returns it. Zero provider calls. Safe to rerun a CLI command without leaking duplicates.

```bash
iplane instance create local cli-demo --class small
```

```
  Found existing instance "cli-demo"
    provider:     local
    provider id:  local:cli-demo
    state:        ACTIVE
    sku:          Apple M4 Pro
```

### 5. List (state-file source)

#### List sources

```bash
iplane instance list
```

```
  ID        PROVIDER  STATE   SKU           RATE        REGION
  cli-demo  local     ACTIVE  Apple M4 Pro  $0.0000/hr  -
```

### 6. Destroy

#### Destroy options

```bash
iplane instance destroy cli-demo
```

```
  Destroyed instance "cli-demo" (final state: TERMINATED)
```

### Done

Instance terminated. The state file at /tmp/iplane-cli-example/state.json carries the audit record (state=TERMINATED) -- rerunning the demo with the same id reuses the slot.
Two transports exercise the same Service contract: this walkthrough drives the CLI; 01-end-to-end drives the gRPC client. Operators pick the one that fits their workflow.

