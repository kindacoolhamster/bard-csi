# Conformant CSI VolumeGroupSnapshot for ceph-rbd

Status: approved for implementation (Sonnet design + Opus adversarial review
converged 2026-09-02, live-validated against a real Ceph Tentacle v20.2.0
cluster). Supersedes the retired implementation withdrawn in `aebeb89` /
documented in `docs/upgrade-group-snapshots.md`.

## Why the old design failed and what changes

CSI v1.12 `CreateVolumeGroupSnapshot`: the group snapshot **MUST** give a
write-order consistency guarantee or fail. Individual member restorability is
only a MAY, but the MAY is a practical requirement in Kubernetes
(external-snapshotter materializes one `VolumeSnapshot` per member and the
ecosystem expects it to restore). The retired implementation gave up the MUST
to keep the MAY (sequential per-volume snapshots, even across backend
instances). That was withdrawn rather than patched, because the two halves
cannot be reconciled by patching a sequential-snapshot design — the mechanism
itself has to change.

This design gets **both halves** from a single Ceph primitive:

- **Write-order consistency**: `rbd group snap create` — an atomic,
  quiesced, cross-image snapshot cut. Confirmed live against krbd (Bard's
  production default mounter, not just idle/librbd images): two krbd-mapped
  images hammered with causally-dependent writes (write N to A, wait, write N
  to B, loop) both stalled for the full duration of the `group snap create`
  call and resumed the instant it returned; the captured snapshot values
  satisfied `A >= B >= A-1` on every trial, including a trial that landed
  mid-cycle (A=13, B=12). No trial showed an inverted or >1 gap.
- **Member restorability**: `rbd clone --pool P --image I --snap-id N
  --dest-pool P --dest X` — clones directly from a group-member (namespace
  `group`, non-"user"-type) snapshot. This became possible in **Ceph Squid
  v19.2.0** ("Support for cloning from non-user type snapshots... exposed via
  the new `--snap-id` option for `rbd clone`"); it's what the retired design's
  own contemporaneous note said didn't exist yet. Confirmed live: clone
  succeeds, `rbd info` shows the correct parent, the clone is an ordinary,
  independent image from that point on.

**Residual risk, carried forward rather than hidden**: all live validation
above used both group members mapped by krbd on **one** node (galileo). The
realistic Bard deployment maps each member on a **different** node (separate
PVCs, separate pods). The per-image exclusive-lock notify/quiesce protocol
`rbd group snap create` relies on is a RADOS-level, per-image mechanism
addressed to whichever client currently holds that image's lock — architecturally
node-agnostic — but this was not empirically checked cross-node (the obvious
test bed, the site2 dev1/dev3 k3s tier, was unreachable during this design
pass, and this machine's standing guidance is not to spin up a fresh multipass
VM to work around that).

Partially narrowed without a second node: CLAUDE.md documents that krbd
shares one rados client per (cluster,user) per node, so the single-node
trials above risked one shared client/watch-notify path serializing both
quiesces and masking a staggered cut. Remapped both members with `rbd map -o
noshare` (confirmed via `/sys/bus/rbd/devices/*/client_id`: two distinct
client ids) to force two independent rados clients on the same host, then
reran the causal test 4 more times — invariant held on every trial, including
another caught mid-cycle boundary (A=4, B=3). This converts the confirmed
claim from "one client, coordinated" to "multiple independent clients,
coordinated" — still same host/kernel/clock, so it does **not** retire the
residual (no network partition, no cross-node clock/latency variance), but it
rules out the cheapest alternative explanation.

**Action item, not optional**: re-run the causal write-order test (script
below, adapted to two nodes) on a real two-node krbd tier before this ships
as anything other than an experimental/beta capability. If it fails
cross-node, the fallback is scoping this feature to librbd-backed mounters
(rbd-nbd) only, or keeping it withdrawn.

## Scope

- **ceph-rbd only.** No other backend (cephfs, lvm, iscsi, nfs) implements
  the new interface; a group request touching a volume on a non-implementing
  backend is a hard error (see error codes below).
- **Single Ceph instance only.** No cross-instance/cross-cluster group — there
  is no external quiesce protocol to give one write-order cut across
  independent clusters. This was already decided in the withdrawal follow-up
  and is not revisited here.
- **One rbd group can hold one *active* set of members at a time** (Ceph
  constraint: an image belongs to at most one group). Two DIFFERENT group
  snapshot requests for the SAME member set share one rbd group and are both
  representable (see below). Two group snapshot requests for
  OVERLAPPING-but-different member sets cannot both be live at once — this is
  a real, documented limitation (see "Known limitation" below), not a bug to
  route around.

## Architecture

New optional interface on `internal/backend.Backend`, mirroring the five
existing optional capabilities (`NetworkFencer`, `VolumeReplicator`,
`EncryptionKeyRotator`, `VolumeGrouper`, and the pattern `driver.go` already
uses to conditionally register `FenceController`):

```go
type GroupSnapshotter interface {
    CreateVolumeGroupSnapshot(ctx context.Context, req *CreateGroupSnapshotRequest) (*GroupSnapshot, error)
    DeleteVolumeGroupSnapshot(ctx context.Context, groupSnapshotID string, memberSnapshotIDs []string, secrets map[string]string) error
    GetVolumeGroupSnapshot(ctx context.Context, groupSnapshotID string, memberSnapshotIDs []string, secrets map[string]string) (*GroupSnapshot, error)
}
```

`Capabilities.GroupSnapshot bool` gates it (field order must mirror
`bardplugin.Capabilities`, matching every other capability flag's convention).
`driver.go` registers CSI `GroupControllerServer` — and advertises
`GROUP_CONTROLLER_SERVICE` — only when a registered backend implements this
interface, exactly like the `FenceController`/`VolumeGroup` wiring today.

**Plugin-level capability caveat** (from review, worth documenting): like
every other plugin-advertised capability, this is read once at plugin sidecar
startup. Adding group-snapshot support to a backend via a live
`BackendCluster` CRD reload will not take effect until the sidecar restarts —
consistent with the existing "Plugin live config reload" known-follow-up, not
a new gap, but call it out in that section.

## RBD group lifecycle (the fix for the biggest design flaw caught in review)

**Do not key the rbd group on the CO's group-snapshot `Name`.** An image can
belong to only one group, and CSI's `CreateVolumeGroupSnapshotRequest.Name` is
a *per-call* idempotency key — a backup tool takes many differently-named
group snapshots of the *same* PVC set over time (hourly retention is the
normal case). Keying the group per-name would make the second such request
collide trying to re-add already-grouped images to a *new* group.

**Key the rbd group on a hash of the normalized, sorted, deduplicated member
handle set** (instance/pool included in the hash input, per review), using
the existing `shortName`-style deterministic naming already used for
csi-addons groups (different prefix, so the two features' groups never
collide in the pool namespace: e.g. `csi-group-` for csi-addons,
`csi-gsnap-` for this feature). One rbd group can then hold **many** rbd
group snapshots over its lifetime — the normal retention case.

- **CreateVolumeGroupSnapshot**: derive the group name from the member set,
  `rbd group create` (idempotent — ignore already-exists), add every member
  (idempotent — ignore already-exists), **then verify** via `groupImageList`
  that the group's actual membership now equals the requested set exactly
  (see "Membership verification" below — this is not optional), then `rbd
  group snap create <group>@<derived-snap-name>` (the snap name can be
  derived from the CO's `Name`, since it only needs to be unique *within*
  this one rbd group, not globally).
- **DeleteVolumeGroupSnapshot**: `rbd group snap rm <group>@<snap>` (safe
  unconditionally — live-confirmed: a member with no clone children is
  removed immediately, a member with a live clone is auto-trashed and
  auto-purged when its last clone disappears, using the *same* lazy-delete
  machinery this codebase already relies on for ordinary snapshot-with-clones
  deletion). Then check `rbd group snap list` on that group; if now empty,
  `rbd group rm` to disband it (confirmed live: succeeds even with member
  images still attached — no need to remove members first). If other group
  snapshots still exist in it, leave the group alone.

### Membership verification (closes a real bug found in review)

`isAlreadyExists()` matches any error containing the substring "exists" —
confirmed live that `rbd group image add` on an image already belonging to a
**different** group fails with exactly `rbd: add image error: (17) File
exists`, which that helper would misclassify as idempotent success. Reusing
`groupImageAdd` as-is would silently produce a group snapshot missing a
requested member. **Required fix**: after the add loop, call `groupImageList`
and assert the result set equals the requested set exactly; if not, fail the
whole request (see error codes) naming the specific volume that couldn't
join, and best-effort clean up the group (this satisfies the spec text: "If
an error occurs before all the individual snapshots are cut... SP MUST return
an error, and SP SHOULD also do clean up and make sure no snapshots are
leaked").

This same check is also the design's answer to "what if `rbd group snap
create` isn't as atomic as documented" (a review concern): we don't
independently re-verify every member's snapshot after the cut, because the
membership check *before* the cut plus Ceph's documented all-or-nothing
group-snapshot semantics together are the safety net — but if a future bug
report shows a partial cut in the wild, the fix is to add a post-cut
`groupImageList`-equivalent check (`rbd group snap list` reporting the new
snap's `state` as `complete`, not e.g. `incomplete` after a killed client),
not to distrust the mechanism wholesale.

## Member snapshot handle encoding

`internal/volumeid.Handle.Name` already carries `"image@snap"` for ordinary
snapshots. A group-member snapshot's real backend name is Ceph's synthetic,
**unstable** string (`.group.<poolid>_<groupid>_<groupsnapid>` — and `rbd
group snap rm` *renames* a still-referenced member into a `trash` namespace,
so a name-based handle would dangle). The **numeric snap id is stable**
across that rename. Encode it as `"image@#<id>"` — a `#` immediately after
`@` distinguishes it from an ordinary snapshot name with zero changes to
`internal/volumeid`'s parser (`Handle.Name` is opaque to it) and stays well
under the 128-byte CSI limit. Guard: reject any CO-supplied ordinary snapshot
name that itself starts with `#` (trivial, and Bard-generated ordinary names
never do).

Two call sites need an explicit branch on this sigil:

- **`provision()`** (`internal/cephplugin/cephplugin.go` ~line 1012, the
  `case req.SourceSnapshot != nil` branch): today it builds a single
  `pool/image@snap` positional arg for `rbd clone`. For the `@#N` form,
  split out the image name (strip the sigil) and issue `rbd clone --pool
  <pool> --image <image> --snap-id <N> --dest-pool <pool> --dest <dest>
  --rbd-default-clone-format 2` instead — flags, not a positional spec (this
  positional form doesn't support non-user snapshots at all, which is the
  whole reason `--snap-id` exists).
- **`DeleteSnapshot`** (~line 1383): CSI's CO SHALL NOT call ordinary
  `DeleteSnapshot` on a group member (`group_snapshot_id` is set), but
  defensively: if `req.Snapshot.Name` contains the `@#` sigil, return
  `FAILED_PRECONDITION` explaining the member must be deleted via
  `DeleteVolumeGroupSnapshot`, rather than attempting a nonsense `rbd snap rm
  pool/image@#N` (no such literal snapshot name exists).

Group-member snapshots do **not** participate in `b.snapIndex`/`b.snapNames`
(the ordinary-`CreateSnapshot` idempotency bookkeeping) — they're created via
a completely separate code path with their own idempotency (the group-name
hash), so this is correct by construction, not a gap to patch.

## Version gate: functional probe, not a version string

**Rejected approach** (my original proposal, correctly killed in review):
checking `rbd help clone` for `--snap-id` in the option list. The `rbd`
binary ships *inside Bard's own plugin image*, pinned at build time from the
upstream apt repo — so this tests a build-time constant of our own container,
never the operator's actual cluster. It would pass identically on every
deployment regardless of what Ceph version the operator runs, telling us
nothing.

**Adopted approach**: a real functional probe, run once per **instance**
(not once per process — different registered ceph-rbd instances can be on
different Ceph clusters/versions) at first group-snapshot use on that
instance, positive-cached for the process lifetime, negative-cached with a
short TTL (so an operator's cluster upgrade is picked up without a pod
restart). The probe runs the exact primitive chain end to end on a throwaway
tiny (4MiB) image in that instance's own pool, using deterministic
`shortName`-derived names so concurrent controllers/retries converge and any
leftover is reused rather than duplicated: create image → group create →
group image add → group snap create → `snap ls --all` (resolve the id) →
`clone --snap-id` → `rbd rm` clone → `group snap rm` → `group rm` → `rbd rm`
image. This is the only check that actually exercises the OSD-side path (the
missing piece on an old cluster is `cls_rbd` support for clone-from-non-user-
snapshot, invisible to any client-side version check) and incidentally also
catches pool-cap and image-feature problems a version number wouldn't.
**Implementation note**: `internal/fakerun` needs to model this chain, or
every unit test touching group-snapshot creation breaks.

**Do not** make `ceph versions` / `require_osd_release` the primary check —
it's a proxy for the feature (vendor backports create false negatives), it
needs mon read caps the documented least-privilege `profile rbd` user may not
have, and it reports daemon versions, not "is this specific operation
enabled." A version read is fine as a *supplementary* detail in the error
message, never as the gate itself.

## Error codes (pulled verbatim from CSI v1.12.0 `spec.md`, not inferred —
this codebase has a documented history of getting this wrong, see the targetd
conformance incident)

| Situation | RPC | Code | Spec citation |
|---|---|---|---|
| Cluster too old for member-restore (version-gate probe failed) | CreateVolumeGroupSnapshot | `FAILED_PRECONDITION` (9) | "Cannot snapshot multiple volumes together ... not configured properly based on requirements from the SP ... Caller MUST fix the configuration ... before retrying." |
| Source volumes span >1 backend instance/cluster | CreateVolumeGroupSnapshot | `FAILED_PRECONDITION` (9) | Same row — volumes not meeting the SP's requirements for being grouped together. |
| A requested member can't join the group (already in a different group) | CreateVolumeGroupSnapshot | `FAILED_PRECONDITION` (9) | Same row. |
| `snapshot_ids` doesn't match the group snapshot's actual members | DeleteVolumeGroupSnapshot | `INVALID_ARGUMENT` (3) | "Snapshot list mismatch ... SHOULD also be used to indicate ... a mismatch in the `snapshot_ids`." |
| `snapshot_ids` doesn't match, on Get | GetVolumeGroupSnapshot | `INVALID_ARGUMENT` (3) | Get's own table has the identical row/code — confirmed, not assumed, by reading both tables (they're not always aligned across RPCs, hence checking rather than reusing Delete's code by assumption). |
| Group snapshot doesn't exist | GetVolumeGroupSnapshot | `NOT_FOUND` (5) | "If the volume group snapshot does not exist any more, `GetVolumeGroupSnapshot` should return gRPC error code `NOT_FOUND`." |

## Delete/Get idempotency — required carve-outs (a real bug caught in review)

CSI requires `DeleteVolumeGroupSnapshot` to be idempotent, and it WILL be
retried with the same `snapshot_ids` whenever the CO doesn't see a response.
A naive "`snapshot_ids` must match current backend truth or INVALID_ARGUMENT"
check breaks this the moment the group snapshot is already gone: the second
(retried) call would see zero actual members and permanently mismatch,
wedging the `VolumeGroupSnapshotContent` undeletable. Required carve-outs,
in order:

1. Group snapshot **entirely absent** (never existed, or already fully
   deleted including group GC) → success, no-op. (Spec: "If a group snapshot
   ... does not exist or the artifacts ... do not exist anymore, the Plugin
   MUST reply `0 OK`.")
2. Group snapshot's `rbd group snap rm` already happened but the rbd group
   object itself still exists (other group snapshots pending, or the GC
   step didn't run yet) → proceed straight to the empty-group GC check and
   succeed.
3. Only when a group snapshot **currently exists** with a member set that
   differs from the CO's `snapshot_ids` → `INVALID_ARGUMENT` mismatch error.

Same three-way carve-out applies to `GetVolumeGroupSnapshot`'s mismatch check
(swap step 3's error for `NOT_FOUND` per that RPC's own table when the group
snapshot doesn't exist at all, matching the table above).

## Known limitation: overlapping member sets (document, don't route around)

Because the rbd group is keyed on the member set and an image can belong to
only one group, two group-snapshot requests whose member sets overlap but
differ (e.g. add a PVC to a StatefulSet, then request a group snapshot of the
new label set while older group snapshots of the old set are still retained)
cannot both be live — the new group can't claim an image already held by the
old group's still-existing group snapshot(s). **Failing cleanly is correct
behavior here, not a bug to solve**: return `FAILED_PRECONDITION` naming the
specific blocker ("volume X is held in group Y by group snapshot Z"), and
document this limitation in `docs/upgrade-group-snapshots.md` or a sibling
doc, next to the withdrawal rationale, so operators doing retention scheduling
understand the constraint up front.

## Chart / deploy

Re-add exactly what the withdrawal PR removed, gated the same way every other
optional capability is charted (only rendered when the ceph-rbd profile's
instance actually implements it):

- `hack/install-snapshotter.sh`: restore the group CRD install lines. This
  was **already correctly scoped there and not in the Helm chart** — verified
  directly against `git diff main...aebeb89 -- hack/install-snapshotter.sh` —
  so this is a pure revert of that block, consistent with the existing
  "cluster singleton, not chart-bundled" policy for CRDs, not a new
  exception to it.
- Chart: snapshotter sidecar's group feature gate, group RBAC, group
  `VolumeSnapshotClass` rendering — these DO belong in the chart (they were
  removed from `charts/bard-csi/templates/{rbac.yaml,storageclasses.yaml}` /
  `values.yaml` by the withdrawal), gated on capability like the
  `Replication`/`NetworkFence` profile flags already are.

## Testing plan

- **Unit** (fake runner): group name derivation (sort/dedupe/hash),
  membership-verification failure path, the version-gate probe chain, handle
  encode/decode round-trip for `@#N`, the Delete/Get idempotency carve-outs,
  cross-instance rejection, overlapping-member-set rejection.
- **Live, ceph-rbd control plane, no cluster** (mirrors the existing
  `TestRealCeph` pattern): full create → verify members → clone-restore one
  member → delete group snapshot → confirm pool returns to baseline.
- **Live, in-cluster krbd** (the tier that matters — this is the mounter the
  guarantee is actually for): provision N PVCs, CSI group-snapshot them,
  restore one member to a new PVC, confirm point-in-time data.
- **Required before calling this GA** (not optional, per the residual risk
  section above): re-run the causal write-order test with members mapped on
  **two different nodes** (site2 dev1/dev3 or equivalent), not just galileo
  single-node. Script shape: writer loop `write N to A; wait for completion;
  write N to B; repeat`, fire `rbd group snap create` mid-loop from a third
  location, clone-and-read-back both members, assert `A >= B >= A-1` across
  several trials including a mid-cycle catch. If this fails cross-node, do
  not ship the feature against krbd multi-node until resolved.
