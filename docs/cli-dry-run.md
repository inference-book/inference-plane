# CLI dry-run pattern

`--dry-run` lets an operator preview a state-changing command before
committing. With it set, the command:

- Validates the request (same checks the real path would do up-front).
- Reads any state the verb needs to make a decision (state file or
  remote service — both transports), **never writes**.
- Prints a `[dry-run] would ...` line describing the planned action,
  including projected cost when the provider has a catalog.
- Exits 0.

Zero provider API calls, zero state-file writes. This invariant is
load-bearing: an operator who is uncertain whether they want to spend
money should be able to type `--dry-run` and trust that nothing
changes.

## Where the flag lives

**Per-subcommand**, not on the `instance` group. The flag is registered
on each state-changing verb (`create`, `destroy`) so it cannot be
silently no-op'd on a read-only verb. Per the v0.1 design doc
(`docs/design/0001-provisioner.md` §"CLI: dry-run", line 225):

> `List` is not state-changing and is exempt; `iplane instance list`
> always reads live state. `--dry-run` on a read-only command would
> be a no-op and is rejected at flag parse.

When phases 2–5 add new state-changing verbs, register `--dry-run` on
those verbs directly. Don't promote it to a persistent flag on the
group.

## Where the logic lives

In the CLI layer, **not** the Provisioner interface. The design doc
calls this out explicitly:

> The Provisioner interface gains no dry-run method. Dry-run lives
> in the CLI layer, not the Provisioner.

Concretely, the dry-run helpers (`cmd/iplane/cmd/dryrun.go`) call:

1. `provisioners.ValidateID` and `provisioners.ValidateAndExpandRequirements`
   to catch user input errors before any I/O.
2. `client.DescribeInstance(ctx, ..., Source=LOCAL)` to read the
   existing record (or `NotFound`). Works in both transports — the
   in-process `*Service` and the remote gRPC client implement the
   same interface, so dry-run dispatches the same way.
3. For RunPod, `runpod.MatchSKUs` + `runpod.LookupSKU` to project
   cost from the static catalog.
4. Format the `[dry-run] would ...` block and return.

No `WithDryRun(bool)` option on the Service, no `dry_run` field on
the proto. The flag is purely a client-side affordance.

## What the output should say

Every dry-run line should be **unambiguous** about which path the real
run would take. The patterns we use today:

```
$ iplane instance create runpod my-pod --class small --dry-run
[dry-run] would create "my-pod" on runpod
[dry-run]   region:     (unpinned -- runpod schedules wherever capacity exists)
[dry-run]   constraints: vram>=24GB, ram>=16GB, disk>=20GB, gpus=1
[dry-run]   est cost:   $0.3600/hr (cheapest match: NVIDIA RTX A5000; runpod's live price at spawn may differ)
[dry-run] no provider calls made, no state file changes.
```

```
[dry-run] would no-op: "my-pod" already exists on runpod (state=ACTIVE, provider_id=qvmo3970most3d). idempotent re-create returns the existing record without a provider call.
```

```
[dry-run] would destroy "my-pod" on runpod
[dry-run]   provider id: qvmo3970most3d
[dry-run]   from state:  ACTIVE
[dry-run] no provider calls made, no state file changes.
```

Three invariants:

1. **Start every output line with `[dry-run]`** so log scrapes and
   pipes can filter on it.
2. **Name the operative verb in the present conditional**: "would
   create", "would destroy", "would no-op". Avoid "creating",
   "destroying" — past/present tense reads as "this happened."
3. **End with the no-op confirmation line**: `[dry-run] no provider
   calls made, no state file changes.` Reinforces the invariant for
   anyone skimming.

## Testing the invariant

When a future subcommand adds `--dry-run`, the test must assert both:

- Zero provider API calls (use a counted-mock provider).
- State file unchanged (verify by re-reading post-dry-run).

See `cmd/iplane/cmd/instance_test.go`'s `TestCreate_DryRun_FreshID`
and `TestDestroy_DryRun` for the pattern. The mocks count `Spawn` /
`Terminate`; the test fails loudly if a dry-run increments either.

## What dry-run is NOT

- **Not a deep-validate**. It does not contact the provider to check
  quota, capacity, or auth. Those failures only surface on a real
  run. (We could add a `--dry-run=deep` mode later; v0.1 keeps it
  read-local.)
- **Not a free preview of every error**. If RunPod would reject the
  request body for a schema reason that depends on live state, the
  dry-run won't catch it.
- **Not a substitute for tests**. Dry-run is an operator-facing
  affordance, not a CI gate.

## Future phases

Phases 2–5 add state-changing verbs (`iplane deploy`, `iplane fleet
drain`, etc.). When wiring `--dry-run` on a new verb:

1. Register the flag on that verb's cobra command, not the group.
2. Add a helper in `cmd/iplane/cmd/dryrun.go` (or a sibling file)
   following the `dryRunCreate` / `dryRunDestroy` shape.
3. Validate inputs first, read state second, format output third.
4. Add a test that counts provider calls (must be zero) AND verifies
   state file integrity post-run.
5. Update this document with the new verb's expected output shape.

If a verb's preview would require a deep provider call (e.g., "would
deploy" needs to confirm the target instance exists on the provider),
make the deep check explicit — `--dry-run` stays read-local; add
`--check` or similar for the round-trip variant.
