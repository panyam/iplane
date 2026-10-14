# RunPod adapter notes

Implementation lore for `internal/provisioners/runpod`. Mostly one theme:
**RunPod's status fields describe the rental, not the container**, and every
entry below is a version of someone believing otherwise.

## One API: REST v2 (since 2026-10)

The adapter speaks only `https://api.runpod.io/v2`. Before that it spoke three
surfaces: REST v1 (`rest.runpod.io/v1`) for pods and volumes, GraphQL
(`api.runpod.io/graphql`) for the GPU catalog and SSH keys, and v2 for pod logs
and the `runtime` block. RunPod retires REST v1 on 2026-11-15 and GraphQL in
early 2027, v2 covers everything, and the GraphQL client is deleted.
`WithBaseURL` takes the version segment, so tests point it at `<httptest>/v2`
and fakes match the real paths.

| Concern | v1 / GraphQL | v2 |
| --- | --- | --- |
| Pods | `/v1/pods`, list is a bare array, `?name=` filter | `/v2/pods`, `{"pods": [...], "pagination"}`, cursor pages, **no name filter** (client-side) |
| Create body | `imageName`, `gpuTypeIds[]`, `gpuCount`, `cloudType`, `containerDiskInGb`, `networkVolumeId`, `dockerStartCmd` | `image`, `gpu: {id, count, minRamPerGpu}`, `cloud`, `disk`, `mounts.network[{volumeId, path}]`, `cmd` / `entrypoint` |
| Create response | `{id, desiredStatus, costPerHr}`, then a GET | the full pod, no follow-up GET |
| Pod status | `desiredStatus`, `machineId`, `publicIp` + `portMappings` | `status`, host fields, `ssh.direct {host, port}` / `runtime.ports` |
| Catalog | GraphQL `gpuTypes` | `GET /v2/catalog/gpus`, `/catalog/cpus` |
| SSH keys | GraphQL `myself { pubKey }` / `updateUserSettings` | `GET` / `PUT /v2/account/ssh-keys`, `{"keys": [...]}` |
| Volumes | `/v1/networkvolumes`, `dataCenterId` | `/v2/network-volumes`, `dataCenter`, wrapped and paginated |
| Errors | `{"error"}` / `{"message"}` / arrays | problem+json `{title, status, detail, errors[]}`; `detail` and `errors` reach the operator |

What v2 lacks, and the workaround each forced:

- **One GPU type per create, no fallback.** v1 took `gpuTypeIds[]` and let
  RunPod pick. `createPod` now walks the cheapest-first SKU list (x clouds for
  a deploy) itself, the read-then-create loop RunPod documents for
  `POST /v2/pods`: 400 (no capacity, or a rule violation; only the `detail`
  tells them apart) and 403 (pool not available to the account) move on;
  401, 402, 422, 429, 5xx and transport errors stop. When every candidate
  refuses, the last `detail` is in the error.
- **No `supportPublicIp`.** SECURE pods get a public IP and COMMUNITY pods may
  not, so a pod that must be SSH-reachable (Spawn, `--debug-shell`) only tries
  SECURE; the proxy-only engine deploy tries SECURE then COMMUNITY.
- **Keys are opt-in per pod.** v1 injected the account keys into every pod; v2
  only with `startSsh: true`, which Spawn and `--debug-shell` set.
- **No machine id, no VRAM on the pod.** `runpod.machine_id` metadata is gone;
  VRAM comes from the SKU catalog (`gpu.memory` on a v2 pod is *system* RAM).
- **No name filter on `GET /pods`.** `List` reads every page and matches
  client-side. Fine at a thousand pods a page.
- **No bid price or interruptible create.** See the reclaimable section.
- **CPU pods need a flavor id.** See staging above.

`podAction` speaks `POST /v2/pods/{id}/action` (start / stop / restart /
terminate). No Provider verb uses stop/start today; `Terminate` uses
`DELETE /v2/pods/{id}`, the documented equivalent of `terminate`.

### Checking it against the live API

Nothing in the unit tests touches RunPod; their fixtures are written from
RunPod's OpenAPI document, which is a claim about the document. To check the
decoders against the service, read-only and free:

```
export RUNPOD_API_KEY=...
make runpod-v2-check
```

That runs `tests/smoke-runpod/readonly_test.go` (build tag `runpod_readonly`,
deliberately separate from `smoke_runpod`, which rents): `List` over
`GET /v2/pods`, `Candidates` at one and eight cards over `GET /v2/catalog/gpus`,
`FetchUserPubKey` over `GET /v2/account/ssh-keys`, and `ListVolumes` over
`GET /v2/network-volumes`. Its HTTP client refuses any method but GET. A
scoped key needs `pods:read`, `catalog:read`, `authentication:read` and
`volumes:read` for it.

Not yet verified live, because they need a rental: the create body and the
placement walk (capacity refusals really are 400), the host-field phase
signals, and whether COMMUNITY placements behave as assumed. The first paid
v2 deploy should run with `IPLANE_RUNPOD_DEBUG=1 IPLANE_RUNPOD_PHASE_TRACE=1`.

## Status fields that do not mean what they look like

- **Host assignment** (v1 lore, kept for the lesson): freshly-rented v1 pods returned `"machine": {}` from the follow-up GET, and traced live 2026-08-09 it stayed `{}` for a whole 2m14s deploy while the top-level `machineId` populated immediately. **v2 has neither field.** `machinePresent()` now reads fields only a host can supply: `dataCenterId`, `cudaVersion`, or a non-empty `ssh.proxy` endpoint ("null until the pod has a machine assignment"). Present-but-empty still does not count. v2's create places the pod synchronously (it answers 400 when it cannot), so these may already be set on the 201 and `runpod:scheduling` may be near-instant. Unmeasured; check with `IPLANE_RUNPOD_PHASE_TRACE=1` on the first live v2 deploy.
- **Cold-start phase boundaries must be state, not timestamps** (issue 208, fixed 2026-08-09). RunPod's `lastStartedAt` reads like "container started" and is not: it is stamped at pod-record creation, 5-7 ms *before* `createdAt`, on every pod observed. Keying the ladder on it made `classifyEnginePhase` short-circuit to `engine:init` on the first probe, so `runpod:scheduling` and `runpod:image-pull` were structurally unreachable and image-pull measured exactly 0s on every run (including both book A/B runs). The real container-start signal is the **`runtime` block on `GET /v2/pods/{id}`**, null until the container runs; REST v1 omitted `runtime` entirely, which is why the prober used to make two reads (v1 for the host, v2 for the runtime). On v2 one read carries both. v2's `startedAt` is the counterpart of `lastStartedAt` and is deliberately not used as a boundary either; nothing has shown it behaves differently. Two sub-traps: RunPod returns present-but-empty objects (`"machine": {}`, and `runtime` can arrive the same way) so presence checks must test for real content; and `runtime.uptimeInSeconds` is coarse/cached (read 37 three times across 24s) so it is evidence, not a clock. After the fix, a 1.5B cold start splits 109s image-pull / 43s engine-init — **the image pull is ~70% of a small-model cold start, and a warm deploy pays all of it**. Debug with `IPLANE_RUNPOD_PHASE_TRACE=1`.
- **RunPod staging completion is detected via pod logs, not pod status** (validated live 2026-07-28). RunPod gives NO signal that a one-shot pod's command finished: v1 `desiredStatus` stays `RUNNING`, and even v2 `status` stays `RUNNING` because the exited container is auto-restarted. So `internal/provisioners/runpod/stage.go`'s `StageModel` runs `hf download` (NOT the removed `huggingface-cli`), has the command print `__IPLANE_STAGE_DONE__` / `__IPLANE_STAGE_FAIL__` then `sleep infinity`, and `waitForStageComplete` polls `GET /v2/pods/{id}/logs` (SSE, on the same `WithBaseURL` host as everything else) for the marker via the pure `stageSignal` scanner; the `defer Terminate` still cleans the pod. v2 wants a CPU **flavor** for the staging pod where v1 took `computeType: CPU`, so `stageCPUFlavors` reads `GET /v2/catalog/cpus` at 4 vCPUs and tries the cheapest flavor in stock in the volume's datacenter first, rather than hardcoding an id RunPod can rename. Fake-harness tested against the logs endpoint. **Large models also need `hf download --max-workers 2`** (in the staging command): the CPU staging pod has only ~4 GB RAM, and the default 8 parallel workers streaming multi-GB shards through the FUSE volume mount OOM-kill the download (SIGKILL, rc 137) on a 70B+. A small model is unaffected; validated live on 72B FP8.

## What RunPod cannot tell you

**A container that exits is invisible.** Measured 2026-08-11 by renting a pod
whose entrypoint was `/bin/false`: `desiredStatus` stayed `RUNNING`,
`lastStatusChange` reported the rental rather than the container, and the v1
pod record carries no `status` or `runtime` field at all. RunPod restarts the
exited container, so from the API's view nothing is wrong.

This is why the adapter does **not** implement
`provisioners.FailureReporter`. It is a measured absence, not an oversight, and
the only surface carrying the failure is the v2 log stream. Read this before
"finishing" the implementation.

That measurement was on v1. v2's `status` adds `EXITED` and `ERROR` ("the
container is in an unrecoverable error state"), which reads like the missing
signal and may not be: the staging work found v2's `status` staying `RUNNING`
through an exit-and-restart too. Re-run the `/bin/false` experiment against v2
before building on either value.

**A missing image, by contrast, is caught at pod-create** and rejected with a
clear message, so no pod is created and nothing bills:

```
create pod: Container image "..." was not found on the registry.
```

That asymmetry with Vast, which rents the box first and discovers the bad image
afterwards, is a real architectural difference between the two providers.

## The catalog is `GET /v2/catalog/gpus`

Until 2026-10 the catalog was only on GraphQL (`gpuTypes`), because REST v1 had
none: `GET /v1/gpus` returned 400 with "that path does not exist in the
specification". v2 has one, and `Candidates` reads it with
`include=AVAILABILITY&product=POD&count=N&cloud=C` (`count` and `cloud` are
only honoured alongside `include`, which in turn requires `product`).

The `count` is the reason the call is worth making. Availability is a property
of a card **at a width**, not of a card: probing live on 2026-08-15, **35 of 48
types were obtainable as a single GPU and 11 of 48 as eight**. An availability
of `NONE`, or none reported, means the type cannot be had at that width at all,
and so does a zero price on the cloud in question; all three are dropped rather
than reported.

Two differences from GraphQL that change the arithmetic:

- **The price is per card.** v2 lists a single card's price per cloud and says
  a unit costs that times `gpu.count`; GraphQL's `lowestPrice` priced the whole
  shape. The candidate price is therefore price x count. Forgetting the
  multiply would quote an eight-card pod at one card's rate.
- **Availability is per cloud.** The read takes one `cloud` and defaults to
  SECURE, so `Candidates` makes it twice and reports each type once, on the
  first cloud (SECURE, then COMMUNITY) that has it in stock, at that cloud's
  price, because that is the order a create tries them in. The `cloud` attr
  says which.

`stock_status` keeps its old casing ("Low", not v2's "LOW") so capacity
samples recorded before and after the migration compare.

## RunPod does not expose a reclaimable tier

`minimumBidPrice` sits next to `uninterruptablePrice` and looks exactly like a
spot rate. It is not, or at least the catalog gives no way to tell: the two
were **equal on all 38 shapes that had any availability** when measured on
2026-08-15.

A bid price that is not below the on-demand price is the same rental
relabelled, so `Candidates` drops those for a `RECLAIM_POLICY_PREFERRED`
request rather than claiming a discount that does not exist and an
interruptibility this endpoint gives no way to verify.

On v2 the question is closed from the other side: the catalog carries no bid
price at all, and `CreatePodRequest` has no bid or interruptible field (the
schema is closed, so one cannot be sent). `Candidates` returns nothing for a
reclaimable request without making a call. If RunPod adds a spot tier to v2,
this is the section to revisit.

## System RAM comes with the pod shape

RunPod is the one provider that can neither let an operator select system RAM
nor report it per candidate. It arrives with the pod and scales with the card
count, so `DefaultSystemRAMGb` is a per-card estimate and the shared resolver
multiplies it by the requested width before comparing against `min_ram_gb`.

Getting that scaling wrong was #283: a four-card request asking for 200 GB was
judged against one card's 96, which rejected every A100 and H100 and landed on
a B200 whose single-card 256 was the only figure that cleared the bar. Asking
for the 384 that four cards actually carry matched nothing at all.

## The readiness loop lives elsewhere

`waitForEngineReady` is a wiring of `internal/provisioners/enginewait` (#268).
RunPod's half is the `Observe` callback plus `Config.Endpoint`, which is seeded
because the proxy URL is known the moment the pod id exists. That seeding is
load-bearing rather than cosmetic: it lets the loop probe `/health` before
asking the pods API anything, so a pod that is already serving costs zero status
reads. Three deployer tests assert exactly that by failing on an unexpected
`GET /pods/{id}`.

The phase ladder (`runpod:scheduling` -> `runpod:image-pull` -> `engine:init`)
and its derivation from pod status stay here, because they are claims about what
RunPod's API reports. Vast has its own, and the two deliberately share only the
`engine:init` rung so a dashboard can put them side by side.
