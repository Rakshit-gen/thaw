# thaw

Restores Firecracker agent sandboxes from an object store, fast enough to start one per task. thaw learns which memory and disk blocks a snapshot's restores actually touch, and fetches them ahead of the guest, in the order it will want them.

```
curl -fsSL https://raw.githubusercontent.com/Rakshit-gen/thaw/main/scripts/install.sh | sh
```

On real Firecracker VMs with S3 Express timing, a Python sandbox holding a warm 3M-row DataFrame answered its first task in 5.52 s with thaw. Downloading the snapshot first took 10.1 s, fully lazy took 19.3 s, and booting it from scratch took 134 s. thaw fetched 126 MiB of the 539 MiB snapshot.

It does not win everywhere. Resuming at once and streaming the whole snapshot behind the guest (`hydrate`) is close or ahead in several cases. With S3 Standard timing, plain thaw was slower than hydrate in all three environments; after a repack it was faster for py-data and node-ts and still slower for repo. The table below has all of it.

The site in `web/` plays back recorded restores block by block, has the full benchmark tables, and the docs.

## Why

An agent sandbox is a warm VM: a Python kernel with a dataset loaded, a repo with its tools warmed up, a TypeScript language service that has already parsed the project. Booting one from scratch takes minutes. A snapshot of it is a gigabyte or more, and it sits in S3 because the host restoring it is not the host that took it.

Restoring from S3 the usual ways:

- **Full download, then resume.** Pays for every byte up front, including the memory the task never reads.
- **Lazy, fetch each page on first touch.** Only moves what is needed, but each first touch is a round trip while a vCPU waits, thousands of times, mostly one block at a time.

thaw resumes the VM at once like lazy, but a prefetcher pulls the blocks past restores of this snapshot touched, in the order they touched them, with coalesced ranged GETs. Anything the plan missed is served on demand through reserved connections the prefetcher cannot take. The plan improves as restores happen, and a repack rewrites the hot blocks into plan order so they come down as a few long reads.

The benchmark measures time to a checked answer: restore, run a real task in the guest, verify the output. Time to "resumed" would make lazy look instant.

## Install

The one-liner above puts `thaw` (and on Linux the `thaw-guest` agent) in `~/.local/bin`, after checking the release's SHA256. `THAW_VERSION` pins a version and `THAW_BIN_DIR` changes the directory.

From source:

```
go install github.com/Rakshit-gen/thaw/cmd/thaw@latest
# or, in a checkout
make install
```

The restore commands (`base`, `run`, `bench`, `doctor`) need a Linux host:

- `/dev/kvm`, userfaultfd, and the `nbd` module (`modprobe nbd`). Tested on kernel 6.8.
- Firecracker and an uncompressed guest kernel with virtio-vsock. Tested with Firecracker 1.17 and the 6.1 guest kernel from Firecracker's CI.
- A root filesystem with the environments' packages. `scripts/build-rootfs.sh` builds the one the benchmarks use from Ubuntu 24.04.
- Root, for userfaultfd and NBD.

`sudo thaw doctor` checks all of that. The other commands (`ls`, `plan`, `repack`, `gc`, `sim`, `serve`, `export`, `report`) run anywhere, macOS included.

## Quickstart

```
# a store: a directory, or a bucket
export THAW_STORE=/var/lib/thaw/store
# export THAW_STORE='s3://bucket/thaw?endpoint=s3.us-east-1.amazonaws.com'

# boot an environment, warm it, snapshot it
sudo -E thaw base -env py-data

# restore and run a task; the first lazy restores record traces
sudo -E thaw run -snap <id> -strategy lazy -task revenue-by-region
sudo -E thaw run -snap <id> -strategy lazy -task top-customers

# now there is a plan
thaw plan -snap <id>
sudo -E thaw run -snap <id> -strategy thaw -profile s3-express

# fork after the task, and restore the fork
sudo -E thaw run -snap <id> -fork

# watch restores live in the site's console
thaw serve
```

`-profile` slows a local store down to the latency and bandwidth of `s3-standard`, `s3-express` or a `peer` host, which is how the benchmarks get repeatable store behaviour. Leave it at `none` against a real bucket.

## Measured vs claimed

Every number here comes from `results/vm.json` (real Firecracker VMs) or `results/sim.json` (trace replays), and `thaw report` regenerates [results/README.md](results/README.md) with the full tables from those files. Medians over every task of the environment.

Median time to a checked answer, 10 runs per cell (5 tasks, twice):

| store | env | boot from scratch | full | lazy | hydrate | thaw | thaw + repack | fetched, full / thaw |
|---|---|--:|--:|--:|--:|--:|--:|--:|
| S3 Express | py-data | 134.0 s | 10.1 s | 19.3 s | 8.97 s | **5.52 s** | 6.81 s | 539 / 126 MiB |
| S3 Express | repo | 17.2 s | 17.0 s | 20.7 s | 14.9 s | 15.0 s | **14.1 s** | 340 / 48 MiB |
| S3 Express | node-ts | 172.9 s | 15.9 s | 21.5 s | 13.6 s | **11.4 s** | 11.6 s | 375 / 76 MiB |
| S3 Standard | py-data | 134.0 s | 14.2 s | 99.4 s | 10.3 s | 22.5 s | **6.84 s** | 539 / 126 MiB |
| S3 Standard | repo | 17.2 s | 17.1 s | 68.3 s | **14.2 s** | 25.9 s | 21.1 s | 340 / 49 MiB |
| S3 Standard | node-ts | 172.9 s | 18.1 s | 93.7 s | 13.8 s | 22.2 s | **12.0 s** | 375 / 75 MiB |

Fastest in each row in bold. Forks paused the VM for 213 ms to 1.23 s and uploaded 4.7 to 36.7 MiB. In replays, 16 restores of one snapshot through a peer ring of 4 hosts made 2,533 to 4,050 GETs instead of 10,607 to 45,865 restoring independently, at a cost in latency for two of the three (p50 32.8 s against 24.7 s for py-data, 22.5 s against 17.5 s for node-ts).

Where these numbers come from:

- Firecracker ran inside a Lima VM on an Apple silicon Mac (nested virtualization). Every page fault and VM exit costs more nested, which hurts lazy and thaw more than full. Bare metal numbers will differ.
- The store is a local directory shaped to S3 latency and bandwidth (`objstore/shaped.go`), modelled on AWS's published figures. No live bucket was used.
- The bench ran in two parts, about 3 h 20 min of wall time in all. The first part stopped after 80 runs when one Firecracker process did not come up (`connection refused` on its API socket); `thaw bench -resume` kept those runs and did the rest.
- 4 of 300 runs did not end with a correct answer: two lazy restores of py-data's `monthly-trend` under S3 Standard hit the 5 minute limit, a lazy repo `git-status` failed in the guest (`waitid: no child processes`), and the guest process of a full repo `grep-slugify` was killed. They stay in the medians at the time they failed, against their own strategy.
- Ten runs per cell is few, and the tails are wide: several p90s in [results/README.md](results/README.md) are many times the median.
- Plans were trained on the same five tasks they were measured on. Replays that hold a task out keep recall at 88 to 91% for repo and node-ts but drop py-data to 67%.

## How it works

[DESIGN.md](DESIGN.md) has the details. In short:

- Snapshots are cut into 64 KiB content-addressed blocks, packed into objects, deduplicated across snapshots. All-zero blocks are never stored.
- Memory is served through userfaultfd, the disk through the kernel NBD client over a socketpair with a copy-on-write overlay.
- Every restore leaves a trace of which blocks it touched and when. Plans are built from recent traces, weighted toward newer ones.
- The fetcher coalesces neighbouring blocks into ranged GETs, keeps connections free for demand misses, and can fault around a miss.
- A host cache and a rendezvous-hashed peer layer let a fleet fetch each block from the store about once.
- Forks take a diff snapshot of a running VM and upload only its dirty pages and disk blocks.
- GC runs in two phases with tombstones, so it is safe while forks are reusing packs.

## Layout

```
cmd/thaw         the CLI
cmd/thaw-guest   the agent inside the VM (vsock)
host             base, restore and fork workflows on Firecracker
restore          the restore session and strategies
plan, trace      traces in, prefetch plans out
fetch            ranged GETs, coalescing, demand reservation
cache, peer      host cache and cross-host sharing
repo, image      snapshot storage: blocks, packs, manifests, GC, repack
objstore         S3, directory and shaped stores
uffd, nbd, vm    Linux and Firecracker plumbing
tasks            the benchmark tasks and their checked answers
web              the site (Next.js, static export)
results          benchmark output
```

## Reproducing

```
sudo -E thaw bench -store /var/lib/thaw/bench-store -out results/vm.json
sudo -E thaw sim -store /var/lib/thaw/bench-store -out results/sim.json
thaw export -store /var/lib/thaw/bench-store -out web/public/runs
thaw report
make web
```

The bench builds a base snapshot of each environment, trains plans with lazy restores, then runs every strategy on every task under each store profile, forks, repacks and runs thaw again on the repacked snapshots. It saves after every repetition, and `-bases env=id,...` reuses existing snapshots.

## Status

Works end to end on real VMs: base, all four restore strategies, repack, forks and fork restores, with every task's answer checked. `go test ./...` covers the storage, planning, fetching, caching, GC (including a concurrent chaos test) and the replay simulator. The uffd and NBD tests need root and skip without it.

Not done: a live S3 run, bare metal numbers, more than one vCPU count per environment, and multi-host fan-out on real hosts (the fan-out numbers are replays).

## License

MIT
