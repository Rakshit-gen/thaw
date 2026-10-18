# Design

This is how thaw works and why it is built the way it is. The README covers what it does and what it measured; this file is for someone about to read or change the code.

## The problem

An agent sandbox is a VM with a warm environment: a Python kernel with data loaded, a repo with its toolchain warmed up, a language server that has already indexed the code. Booting one from scratch takes minutes. Restoring a Firecracker snapshot takes far less, but the snapshot is guest memory plus a disk image, often a gigabyte or more, and it lives in an object store because the host that restores it is rarely the host that took it.

There are two usual ways to restore from a remote store:

- **Full.** Download everything, then resume. Simple, and the guest never waits once it runs. But it pays for every byte up front, including memory the task never reads.
- **Lazy.** Resume at once and fetch each block the first time the guest touches it. Only the needed bytes move. But each first touch is a round trip to the store while a vCPU sits stalled, and a task touches thousands of blocks, mostly one at a time.

thaw learns which blocks a snapshot's restores touch, and in what order. On the next restore it fetches those blocks ahead of the guest, in that order, with batched requests, while still serving anything it missed on demand. The metric is time to a checked answer: restore, run a task inside the guest, and verify the output. Time to "VM resumed" flatters lazy restores, since lazy resumes almost instantly and then crawls.

## Pieces

```
            object store (S3, MinIO, or a directory)
   snapshots/  manifests/  packs/  index/  traces/  tomb/
                         |
                       fetch  <- host cache <- peer hosts
                         |
                      restore.Session  <- plan (from traces)
                    /                \
         uffd handler (memory)    NBD server (disk)
                    \                /
                      Firecracker VM
                           |
                    thaw-guest agent (vsock)
```

| Package | Job |
|---|---|
| `image` | Splits a file into fixed-size blocks, addressed by content hash. All-zero blocks are recorded as zero and never stored. |
| `repo` | Snapshot layout in the bucket: manifests, packs, indexes, traces. Writes with dedup, repacks, collects garbage. |
| `objstore` | The small slice of an object store thaw uses, for S3 (SigV4, no SDK), a local directory, and a "shaped" wrapper that adds latency and bandwidth limits for repeatable benchmarks. |
| `fetch` | Ranged GETs with coalescing, reserved demand slots and fault-around. |
| `cache`, `peer` | A per-host block cache and a rendezvous-hashed cache shared across hosts. |
| `plan` | Turns traces into a prefetch plan. |
| `restore` | The session that serves blocks to the VM and runs a prefetch strategy. |
| `uffd`, `nbd` | Linux plumbing: userfaultfd for guest memory, the kernel NBD client for the disk. |
| `vm`, `host` | The Firecracker API, the agent protocol, and the base / restore / fork workflow. |
| `trace` | The record of one restore: which blocks were touched, when, and how they were served. |
| `tasks` | The benchmark tasks and the answers they must produce. |

## Storage

A snapshot is three files: guest memory, the root disk, and Firecracker's VM state. Each file is cut into 64 KiB blocks. A block's ID is the hash of its contents, so a block shared by two snapshots is stored once. That matters most for forks: a fork of a snapshot shares nearly all of its parent's blocks, and only its dirty pages and disk blocks are new.

Blocks are written into packs, objects of many blocks each, because one object per block would mean a request per block on every restore and millions of objects in the bucket. A manifest maps each block of a file to (pack, offset, length), or to "zero". Writing `snapshots/<id>.json` is the commit point. Nothing reads a snapshot before that record exists, so a crashed upload leaves garbage but never a broken snapshot.

A block is stored deflated when that makes it at least an eighth smaller, and raw otherwise, so a restore never spends CPU inflating a block that barely shrank.

### Garbage collection

GC has to work while other hosts are writing snapshots, and every fork is a writer that reuses its parent's packs. The race: a writer sees block B in pack P and decides to reuse it; GC decides P is unreferenced and deletes it; the writer commits a manifest pointing into a deleted pack.

So GC runs in two phases, described in full in `repo/gc.go`:

1. Write a tombstone for every unreferenced pack. Delete nothing.
2. On a later run, for each tombstone older than the grace period, list the committed manifests again. Referenced now means it was rescued, so drop the tombstone. Otherwise delete the index, then the pack, then the tombstone.

Writers check tombstones before reusing anything and must commit within the grace period. `repo/chaos_test.go` runs writers and GC concurrently against a store that injects failures and checks that every committed snapshot can still be read in full.

## Restore

`restore.Session` owns one restore. Firecracker resumes with its memory registered to a userfaultfd that thaw serves, and its root drive pointed at an NBD device that thaw also serves. When the guest touches a page or reads a disk sector for the first time, the handler calls `Session.Get`:

- if the block is zero, serve zeros;
- if it is staged (already prefetched into host memory), copy it out;
- otherwise fetch it now, while the guest waits. This is a demand fetch.

The strategy decides what gets staged ahead of the guest:

| Strategy | Staging |
|---|---|
| `full` | Every block, before resume. Firecracker then loads memory from a local file, so this is the plain download-then-resume baseline rather than lazy machinery with everything prefetched. |
| `lazy` | Nothing. |
| `hydrate` | Every block in file order, starting at resume. This is the background-fill approach used by systems that hydrate snapshots after starting the guest. |
| `thaw` | The plan's blocks in plan order, starting at resume. |

Staged blocks are not installed into guest memory, so the guest still faults on each one and the trace records every block the restore touched. Without that, the trace would show only what prefetch missed, and the next plan would be built from misses. There is an install mode that copies staged pages directly with `UFFDIO_COPY`. It saves a fault per page, but its traces are flagged and never used for planning.

Disk writes go to a sparse copy-on-write overlay beside the VM, so restores of one snapshot never see each other's writes.

## Plans

A trace lists the blocks one restore touched, each with a timestamp. The plan for a snapshot is built from its recent traces:

- A block is planned if enough traces touched it (weighted share at least 0.3).
- Its position in the plan is the weighted median of when those traces touched it.
- Newer traces weigh more (decay 0.8 per trace, newest 20 kept), so the plan follows a workload that drifts.
- Blocks a trace saw as zero are dropped, since they cost nothing to serve.

Traces from different tasks on one snapshot are pooled. Most of a restore is the same whatever the task: resuming the kernel, waking the agent, touching the interpreter's heap. The task-specific tail is what the frequency threshold decides about: with traces from five tasks, a block only one task touches is in about a fifth of them, under the default 0.3, so it is left to demand fetching. The `minfreq` replay sweep measures what lowering the threshold buys that task and costs the others. The benchmarks report "thaw, unseen task" separately: a plan built only from traces of other tasks, to show how much of the gain survives a task the plan has never seen.

## Fetching

Requests are ranged GETs on packs. On S3 the first byte of a request costs tens of milliseconds and the next few hundred kilobytes cost almost nothing, so the fetcher works to make fewer, larger requests:

- **Coalescing.** Blocks next to each other in a pack are fetched in one request, up to 4 MiB, reading through holes up to 256 KiB.
- **Reserved demand slots.** 16 requests in flight by default, 4 of them reserved for demand fetches. A prefetcher that takes every connection while the guest waits on a miss makes restores slower than lazy. Prefetch never takes the reserved slots.
- **Fault-around.** A demand fetch can also bring along the planned blocks that follow it in the same pack.
- **Repack.** `thaw repack` copies a snapshot's planned blocks into new packs in plan order and points the manifests at the copies, so the plan reads as a few long sequential requests instead of many short scattered ones. The old copies stay until GC finds them unreferenced.

Under the store sits the host cache, a log of segment files with clock eviction a segment at a time, shared by every VM on the host. Beside it sits the peer layer: each 4 MiB region of a pack has an owner host chosen by rendezvous hashing, and a host that misses its own cache asks the owner, which reads through from the store once. The aim is that a fleet fetches each block from the store about once, however many hosts start forks of the same snapshot.

## Forks

`thaw run -fork` forks a running VM after its task:

1. Sync the guest's filesystem, pause the VM, and take a Firecracker diff snapshot. Its memory file holds only the pages written since the restore.
2. Copy the overlay's dirty disk blocks to a private file, then resume the parent. The pause covers only steps 1 and 2.
3. Upload the fork as a new snapshot whose parent is the original. Its manifests reuse the parent's blocks for everything the diff did not change, so only dirty blocks are written.

A fork can itself be forked, since memory reads go through the fork's own overlay first.

## Measuring

`thaw bench` runs the whole matrix on real Firecracker VMs: every environment, every task, every strategy, on each store profile, after a few training restores to give thaw its traces. The store is a local directory with latency and bandwidth shaped to match S3 Standard or S3 Express One Zone, so runs are repeatable and cost nothing. Every number it writes comes from a real guest computing a real answer that was checked.

`thaw sim` replays the recorded traces through the real `restore` and `fetch` code against shaped stores, with the guest's think time between touches taken from the trace. That makes sweeps cheap that would take days on VMs: stream counts, coalescing off, block sizes, how many training traces a plan needs, fan-out across hosts. Each replay row carries the time the real VM took for the same trace, so the gap between replay and reality is visible.

## Limits

- The benchmarks ran inside a Lima VM on Apple silicon, so Firecracker was nested. Nesting makes every page fault and VM exit slower, which hurts lazy and thaw more than full. Bare metal numbers would differ.
- The S3 profiles are shaped from published latency figures, not measured against a live bucket for each run.
- One vCPU count and one memory size per environment.
- A plan learned for one task generalizes only partly to others. The "unseen task" rows measure how much.
- The host must run as root for userfaultfd and NBD.
