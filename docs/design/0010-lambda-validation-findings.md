# 0010 — Live Lambda Labs validation findings

**Status:** Findings (no design in this doc)
**Phase:** v0.2, issue #427
**Depends on:** PR 429 (the adapter fixes this run was meant to confirm), #431, #432
**Settles:** #161 on hardware; the deploy and teardown criteria on #427

## What this settles

The Lambda adapter had never been pointed at hardware. It has now.
**`gpu_1x_a10` in `us-east-1`, $1.29/hr. Six rentals across two sessions,
$1.07 total.** Zero instances left running, verified against the vendor API
afterwards. `hack/lambda-watchdog.sh` was armed before the first rental.

The run confirmed three fixes that had only ever been exercised against
stubs, and found four defects that no unit test could have found. Three of
the four are on the shared VM-style deploy path rather than in the Lambda
adapter, so they apply to raw AWS and GCP the moment those land (#428).

**The headline of the first rental is that `deployment destroy` did not work at
all on this path.** Not the way #161 described, where the container stops and
the machine survives. It failed before doing anything, on every attempt, and
the machine could only be released by a different verb. #161's fix was correct
and unreachable.

A second rental, after that was fixed, settled #161 itself: confirmed, and the
fix confirmed with it.

## Confirmed on hardware

**iplane's own SSH key reaches the box.** The first live record, seconds
after `instance create`:

```json
"ssh_key_names": ["iplane-default-lambdalabs"],
"tags": [{"key": "iplane-id", "value": "lambda-probe"},
         {"key": "iplane-operator", "value": "default"}]
```

Before PR 429 the adapter had no `KeyRegistrar`, so the box booted with
whichever key the account listed first while the deploy presented iplane's
own. The pair could never have matched. It matches now, and SSH worked.

**The launch tag stamping works** against the real API, which PR 433 had only
been able to assert against a stub. **`iplane instance list` self-healed** a
PENDING record to ACTIVE through the filtered `List` from the same PR.

**Cold start, end to end: 4m30s.** Deploy at 18:52:34, engine answering
`/health` at 18:57:04, on a box that had to boot, pull a 10.5 GB vLLM image
and start the engine. `deployment query` then returned 32 tokens in 338 ms.

**The SSH-readiness gap is real and it is wide.** Lambda published the public
IP 71 seconds after launch and did not report `active` for another 2m53s:

```
18:31:34  launch returned, status booting, no ip
18:32:45  ip 141.148.47.179 assigned
18:35:38  status active
```

`WaitForSSHReady` was configured and unimplemented before PR 429. A deploy
that read the address from Spawn's describe would have had an address almost
three minutes before the machine would answer.

## Found on hardware

### 1. The sshdocker executor assumed a root SSH user

```
failure: inspect failed: docker inspect iplane-deployment-lambda-probe-dep:
  exit 1: permission denied ... unix:///var/run/docker.sock
```

On the box:

```
uid=1000(ubuntu) gid=1000(ubuntu) groups=1000(ubuntu),100(users),118(admin)
/usr/bin/docker      Docker version 28.3.1
docker ps         -> permission denied
sudo -n docker ps -> works
```

Lambda ships docker and leaves the login user out of the `docker` group. Every
provider the executor was written against logs in as root, so the question had
never come up. This is the stock cloud-image shape and AWS and GCP will land
in exactly the same place.

The executor now probes `docker info`, then `sudo -n docker info`, and
prefixes accordingly. **Probing rather than inferring from the username**,
because group membership, socket permissions and rootless docker can each make
the answer differ from what `id` suggests.

**The probe is lazy rather than part of `EnsureInstalled`.** That was the
first attempt, and the teardown failed identically twenty minutes later:
`Destroy` builds its own `Docker` and never calls `EnsureInstalled`. Resolving
on first use means a path that skips a setup step cannot be wrong.

### 2. The idempotency lookup adopted a different deployment's machine

After a failed auto-provision, the state file held:

```
instance lambda-auto  -> provider_id 539e76ce...  ssh 141.148.47.179
instance lambda-probe -> provider_id 539e76ce...  ssh 141.148.47.179
```

against a vendor that had:

```
iplane-lambda-probe -> 539e76ce15274eddbb2962a1d1ba7dc4  141.148.47.179
iplane-lambda-auto  -> ee18741497ee41af92f96163493b4cb6  129.153.159.179
```

Two iplane ids on one machine, and a second machine with no local record.
`iplane instance destroy lambda-auto` would have terminated lambda-probe's box
and left the other billing. It was terminated by hand through the vendor API,
because iplane's own record could not be trusted to name the right box.

`CreateInstance` asks "do you already have an instance under this id?" and
**adopts the first ACTIVE ref without re-checking the tag**. The pre-#431
Lambda adapter dropped the filter and answered with the whole account. #431
fixed the adapters; this run says that was not sufficient on its own, and the
adoption site now re-filters locally the way the self-heal already does.

The asymmetry is the argument for the second belt. A bad self-heal marks a
record wrongly and a later verb can correct it. A bad adoption binds a
deployment to somebody else's machine and reports `AlreadyExisted`, so nothing
is rented, nothing is corrected, and the operator gets no signal.

### 3. A deployed VM-style deployment could not be destroyed

This is the one worth the rental on its own.

```
$ iplane deployment destroy lambda-auto
Error: destroy "lambda-auto": ssh:connecting: instance "lambda-auto" has no
  SSH endpoint (deployment requires an SSH-reachable instance)
```

The engine was serving. The endpoint was in the deployment record. The
instance record had no `ssh` field at all, and `sshdocker.Executor` refuses an
instance it has no address for, so the container was never stopped,
`releaseRental` was never reached, and the machine went on billing. Both the
pre-fix and post-fix binaries failed identically: **#161's fix was correct and
unreachable on the path it was written for.**

`provisionReplica` does stamp the endpoint after `WaitForSSHReady`.
`finalizeInstanceFromDeploy` then cleared it, deliberately:

```go
// Leave Ssh unset: Describe's publicIp+portMapping is unverified
finalized.Ssh = nil
```

That reasoning is sound for an image-native provider, where Describe's address
is a guess and iplane never SSHes. On the VM path the record already holds an
endpoint that `WaitForSSHReady` dialled and the executor then used to start
the container, and `patchRecord` replaces the whole record, so unset means
erased. It now preserves what is already there and still refuses to adopt
Describe's, which leaves RunPod unchanged.

### 4. The auto-provision path has no idempotency lookup

Re-running a failed deploy under the same id rented a second machine rather
than adopting the first, and both carried `iplane-id: lambda-auto`:

```
18:52:44Z  3 instance(s)
  iplane-lambda-probe 539e76ce... active
  iplane-lambda-auto  996db8af... active
  iplane-lambda-auto  7106df27... booting
```

`CreateInstance` has such a lookup and `provisionReplica` does not: it calls
`registerKeyAndSpawn`, which is `ensureProviderKey` then `Spawn`. The slot
record is keyed on the deployment id, so the retry overwrites the
`provider_id` that named the first machine.

**Not fixed here, and the evidence is weaker than the rest.** I had cleared
the `lambda-auto` records by hand before the retry, so the observed orphaning
is not purely iplane's doing. The conclusion rests on the code path rather
than the sequence. Filed rather than fixed, and it wants reproducing without a
human editing state.

## What the vendor returns that the schema does not declare

The live instance record carries `is_reserved` and `workspace_id`, and neither
appears in the OpenAPI document recorded at v1.10.0. The drift test in
`internal/provisioners/lambdalabs` checks decoded-against-declared, which is
the direction that matters, and this is the other direction: the vendor's own
published schema is incomplete relative to its responses. Worth knowing before
trusting the document as exhaustive.

## Cost

| box | from | to | minutes | cost |
| --- | --- | --- | --- | --- |
| 539e76ce lambda-probe | 18:31:34 | 18:58:45 | 27.2 | $0.58 |
| ee18741 lambda-auto, try 1 | 18:43:41 | 18:44:30 | 0.8 | $0.02 |
| 996db8a lambda-auto, try 2 | 18:47:50 | 18:53:30 | 5.7 | $0.12 |
| 7106df2 lambda-auto, try 3 | 18:52:34 | 18:58:58 | 6.4 | $0.14 |
| **first session** | | | **40.1** | **$0.86** |
| dc2c020e lam-leak, second session | 19:15:44 | 19:20:42 | 5.0 | $0.11 |
| 65e59de5 lam-fixed, second session | 19:20:51 | 19:25:30 | 4.7 | $0.10 |
| **all in** | | | **49.8** | **$1.07** |

Three of the four rentals were the run finding its own defects rather than the
plan. That is what the money bought.

## #161 on hardware, second rental

The first rental could not reach the coupled teardown, because the teardown
failed earlier. With that fixed, a second rental on 2026-08-24 settled it.

An auto-provisioned 1:1 deployment (`ownsInstance` true), destroyed with a
binary identical to merged main except that the `releaseRental` call is
removed, so nothing else differs:

```
=== 19:20:08Z deployment destroy, binary WITHOUT releaseRental ===
Destroyed deployment "lam-leak" (final state: TERMINATED)

  iplane:  deployment lam-leak -> DEPLOYMENT_STATE_TERMINATED
           instance   lam-leak -> INSTANCE_STATE_TERMINATED
  Lambda:  status booting
           STILL BILLING AT $1.29/hr: True
```

**#161 is confirmed.** iplane reported a clean teardown of both records while
the machine was still there and still charging. The operator's only recourse
at that point is the vendor API, because every iplane verb now believes the
instance is gone.

Then the same deployment shape on a fresh machine, destroyed with merged main,
where the only difference is that the `releaseRental` call is present:

```
=== 19:25:24Z deployment destroy, merged main (releaseRental present) ===
Destroyed deployment "lam-fixed" (final state: TERMINATED)

  iplane:  deployment lam-fixed -> DEPLOYMENT_STATE_TERMINATED
           instance   lam-fixed -> INSTANCE_STATE_TERMINATED
  Lambda:  status terminating
           STILL BILLING AT $1.29/hr: False
```

Same command, same account, same 1:1 auto-provisioned shape, two binaries that
differ in one call. Reported state is identical in both; the machine is not.

Worth noting what this cost to observe rather than to assert. The unit tests
have proved the missing `Terminate` call since PR 429, and they proved it
against a double. What they could not show is that the reported state is
*indistinguishable* between the two cases, which is the property that makes
the bug expensive: an operator reading iplane has no signal at all, and finds
out from an invoice.

**A third defect surfaced on this rental**, from running the two boxes in
parallel out of separate state directories. `EnsurePublicKey` named the stored
key per operator and deleted a key already under that name, and Lambda refuses
that while a running instance references it:

```
ensure-key:delete-stale failed (http 400): Key is currently in use, cannot delete
```

The second keystore could not register, so it could not rent at all. This was
named as the open risk when the registrar landed and it paid out on the second
rental. The stored name now carries a digest of the key and nothing is ever
deleted (#442).

## What is still not settled

**Whether a Lambda filesystem mounts inside the engine container.** #432
settled that filesystems are API-creatable and that `file_system_names` is
validated at launch. Nobody has looked at `/lambda/nfs/<name>` from inside a
running container, which is what #436 needs.

**The multi-replica and multi-node shapes.** One replica, one card. Nothing
here says anything about a Lambda deployment spanning machines.

**Whether a Lambda deployment survives being scaled, migrated or drained.**
None of those verbs were driven here.
