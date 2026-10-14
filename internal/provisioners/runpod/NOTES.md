# RunPod adapter notes

Implementation lore for `internal/provisioners/runpod`. Mostly one theme:
**RunPod's status fields describe the rental, not the container**, and every
entry below is a version of someone believing otherwise.

## One API: REST v2 (since 2026-10)

The adapter speaks only `https://api.runpod.io/v2`. Before that it spoke three
surfaces: REST v1 (`rest.runpod.io/v1`) for pods and volumes, GraphQL
(`api.runpod.io/graphql`) for SSH keys, and v2 for pod logs
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
| CPU flavors (staging) | `computeType: CPU` on the create | `GET /v2/catalog/cpus` |
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
- **No bid price or interruptible create.** v1's `interruptible` field was
  bound but never set by this adapter, so nothing is lost.
- **CPU pods need a flavor id.** See the staging note below.

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
`GET /v2/pods`, `FetchUserPubKey` over `GET /v2/account/ssh-keys`, and `ListVolumes` over
`GET /v2/network-volumes`. Its HTTP client refuses any method but GET. A
scoped key needs `pods:read`, `authentication:read` and
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
