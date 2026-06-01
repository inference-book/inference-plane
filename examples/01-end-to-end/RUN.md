# Provisioner end-to-end

Walk through the v0.1 provisioner Service end-to-end. Zero cost (local provider; the laptop provisions itself, no API calls leave the machine).

## Walkthrough

### Setup

The provisioner Service is a connect-rpc handler. This demo connects via a generated ProvisionerServiceClient -- the same client the CLI (phase 1.4) will use.
Target URL:  http://localhost:9091
Provider:    local
Region:      laptop
Spawning id: demo-20260513t083318 with class=small (cheapest matching SKU)
All operations idempotent; defer-terminates on exit or Ctrl-C.

### 1. Check the service is reachable

```
  service reachable
```

### 2. Create with class=small shorthand

class=small expands to min_vram_gb=24, min_disk_gb=20, min_ram_gb=16 server-side. The local resolver picks the cheapest SKU satisfying those constraints.

```
  iplane id:       demo-20260513t083318
  provider id:     local:demo-20260513t083318
  state:           INSTANCE_STATE_ACTIVE
  resolved SKU:    Apple M4 Pro
  hourly rate:     $0.0000/hr
  already existed: false
```

### 3. Describe (local view)

```
  state file says: state=INSTANCE_STATE_ACTIVE, gpu=Apple M4 Pro (48GB), rate=$0.0000/hr
```

### 4. Idempotent re-create

Same spec.id; the service hits its local state cache, sees an active record, returns it. Zero provider calls. This is the safety the abstraction promised -- up-arrow + Enter cannot leak a duplicate instance.

```
  already_existed = true (no provider call)
```

### 5. List (local source)

```
  local state: 1 record(s)
    - demo-20260513t083318 @ local  state=INSTANCE_STATE_ACTIVE  $0.0000/hr
```

### 6. Destroy

```
  final state: INSTANCE_STATE_TERMINATED (terminated_at=08:33:18Z)
```

### Done

Instance terminated. State file at the server's --state-dir holds the audit record (state=terminated).
Re-running this demo with the same id reuses the terminated record's slot (id is reusable; idempotency adoption only fires for pending/active records).

