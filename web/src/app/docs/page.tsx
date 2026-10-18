import type { Metadata } from "next";
import { CopyCommand } from "@/components/copy-command";
import { installCmd } from "@/lib/site";

export const metadata: Metadata = {
  title: "Docs",
  description: "Install thaw, snapshot an environment, restore and fork it, and rerun the benchmarks.",
};

const toc = [
  ["install", "Install"],
  ["host", "Preparing a host"],
  ["quickstart", "Quickstart"],
  ["commands", "Commands"],
  ["restore", "How a restore works"],
  ["store", "What is in the store"],
  ["plans", "Plans"],
  ["fetcher", "The fetcher"],
  ["forks", "Forks"],
  ["gc", "Garbage collection"],
  ["reproduce", "Reproducing the benchmarks"],
  ["limits", "Limits"],
];

export default function Docs() {
  return (
    <div className="mx-auto grid max-w-6xl gap-10 px-5 pt-14 lg:grid-cols-[13rem_1fr]">
      <aside className="hidden lg:block">
        <nav className="sticky top-8 flex flex-col gap-1.5 text-[0.88rem] text-ink-soft">
          {toc.map(([id, l]) => (
            <a key={id} href={`#${id}`} className="hover:text-ink">
              {l}
            </a>
          ))}
        </nav>
      </aside>
      <article className="prose-thaw min-w-0">
        <h1 className="text-[2rem] font-semibold tracking-[-0.02em]">Docs</h1>
        <p>
          thaw is one Go binary, <code>thaw</code>, plus a small agent, <code>thaw-guest</code>, that runs as PID 1 inside each VM. It keeps snapshots in
          any S3-compatible bucket or a local directory, restores them into Firecracker without downloading them first, and learns from each restore what
          to prefetch next time.
        </p>

        <h2 id="install">Install</h2>
        <div className="mt-4 not-prose">
          <CopyCommand cmd={installCmd} />
        </div>
        <p>
          The installer picks the release for your OS and architecture, checks its SHA256 against the release&apos;s <code>checksums.txt</code>, and
          puts <code>thaw</code> in <code>~/.local/bin</code>. On Linux it also installs <code>thaw-guest</code> next to it. Set{" "}
          <code>THAW_VERSION</code> to pin a version or <code>THAW_BIN_DIR</code> to install elsewhere.
        </p>
        <pre>
          <code>{`# with Go 1.25 or newer
go install github.com/Rakshit-gen/thaw/cmd/thaw@latest

# from source
git clone https://github.com/Rakshit-gen/thaw && cd thaw
make install`}</code>
        </pre>
        <p>
          On macOS, <code>thaw</code> works with stores, plans, traces and replays: <code>ls</code>, <code>plan</code>, <code>repack</code>,{" "}
          <code>gc</code>, <code>sim</code>, <code>serve</code> and <code>export</code>. Booting and restoring VMs needs Linux.
        </p>

        <h2 id="host">Preparing a host</h2>
        <p>A host that runs VMs needs:</p>
        <ul>
          <li>
            Linux with <code>/dev/kvm</code>, userfaultfd, and the <code>nbd</code> module (<code>modprobe nbd</code>). Tested on 6.8.
          </li>
          <li>
            Firecracker and an uncompressed guest kernel with virtio-vsock. Tested with Firecracker 1.17 and the 6.1 guest kernel from Firecracker&apos;s CI.
          </li>
          <li>
            A root filesystem image with the environments&apos; packages. <code>scripts/build-rootfs.sh</code> builds the one the benchmarks use from
            Ubuntu 24.04: Python with numpy and pandas, git, a Django checkout, Node and TypeScript.
          </li>
          <li>Root, for userfaultfd and NBD.</li>
        </ul>
        <pre>
          <code>{`sudo scripts/build-rootfs.sh            # writes /opt/thaw/rootfs.ext4
sudo cp vmlinux /opt/thaw/vmlinux
sudo thaw doctor                         # checks all of the above`}</code>
        </pre>
        <p>
          Paths can be changed with flags or with <code>THAW_FIRECRACKER</code>, <code>THAW_KERNEL</code>, <code>THAW_ROOTFS</code>,{" "}
          <code>THAW_AGENT</code>, <code>THAW_WORK</code> and <code>THAW_STORE</code>.
        </p>

        <h2 id="quickstart">Quickstart</h2>
        <pre>
          <code>{`# a store: a directory, or a bucket
export THAW_STORE=/var/lib/thaw/store
# export THAW_STORE='s3://bucket/thaw?endpoint=s3.us-east-1.amazonaws.com'

# boot an environment, warm it, snapshot it
sudo -E thaw base -env py-data
#   20261017-181903.444-2dc07f  py-data  boot 1.58s, setup 2m12s, ...

# restore it and run a task; the first lazy restores record traces
sudo -E thaw run -snap 20261017-181903.444-2dc07f -strategy lazy -task revenue-by-region
sudo -E thaw run -snap 20261017-181903.444-2dc07f -strategy lazy -task top-customers

# now thaw has a plan
thaw plan -snap 20261017-181903.444-2dc07f
sudo -E thaw run -snap 20261017-181903.444-2dc07f -strategy thaw -profile s3-express

# fork after the task, and restore the fork
sudo -E thaw run -snap 20261017-181903.444-2dc07f -fork`}</code>
        </pre>
        <p>
          <code>-profile</code> slows a local store down to the latency and bandwidth of <code>s3-standard</code>, <code>s3-express</code> or a{" "}
          <code>peer</code> host, which is how the benchmarks get repeatable store behaviour. Leave it at <code>none</code> against a real bucket.
        </p>

        <h2 id="commands">Commands</h2>
        <table>
          <thead>
            <tr>
              <th>Command</th>
              <th>What it does</th>
            </tr>
          </thead>
          <tbody>
            {[
              ["thaw base -env E", "Boots environment E from scratch, runs its setup and one round of its tasks, takes a full snapshot and uploads it."],
              ["thaw run -snap ID", "Restores a snapshot with a strategy (full, lazy, hydrate, thaw), runs a task, checks the answer and prints timings. -fork forks afterwards; -save=false keeps the trace out of future plans."],
              ["thaw bench", "Builds or reuses a base per environment, trains plans, then restores every strategy on every task under each store profile, with forks and a repack pass. Writes results/vm.json."],
              ["thaw sim", "Replays the recorded traces under other store tiers, fetcher settings, plan sizes, fleet sizes and block sizes. Writes results/sim.json."],
              ["thaw ls", "Lists snapshots with their parents, sizes and how many traces each has."],
              ["thaw plan -snap ID", "Shows the prefetch plan the stored traces give, including those inherited from ancestors."],
              ["thaw repack -snap ID", "Rewrites the planned blocks into new packs in plan order."],
              ["thaw gc", "Two-phase garbage collection of packs and manifests no snapshot needs."],
              ["thaw serve", "HTTP API for the console: snapshot list, results, and restores streamed as server-sent events."],
              ["thaw export", "Writes replayed runs as JSON for the site to play back without a server."],
              ["thaw report", "Writes results/README.md and an SVG chart per environment from results/vm.json and results/sim.json."],
              ["thaw doctor", "Checks that this host can run VMs."],
            ].map(([c, d]) => (
              <tr key={c}>
                <td>
                  <code>{c}</code>
                </td>
                <td>{d}</td>
              </tr>
            ))}
          </tbody>
        </table>

        <h2 id="restore">How a restore works</h2>
        <ol>
          <li>
            thaw reads the snapshot record and the manifests of its <code>mem</code> and <code>disk</code> files, and downloads the small{" "}
            <code>state</code> file whole.
          </li>
          <li>
            It attaches a free <code>/dev/nbdN</code> backed by the disk manifest and points the VM directory&apos;s <code>rootfs</code> at it, then
            listens on <code>mem.sock</code> and asks Firecracker to load the snapshot with a userfaultfd memory backend. Firecracker sends the
            userfaultfd and the layout of guest memory over that socket.
          </li>
          <li>
            The VM resumes. Each fault on a page thaw has not installed yet arrives as a message; thaw fetches the 64 KiB block holding it, copies the
            whole block in with <code>UFFDIO_COPY</code>, and the faulting vCPU continues. Disk reads arrive as NBD requests and are served the same way.
            Writes to the disk go to a sparse local overlay.
          </li>
          <li>
            Meanwhile the strategy prefetches: nothing for lazy, every block in file order for hydrate, the plan in first-touch order for thaw. Prefetched
            blocks wait in host memory until the guest touches them, so the recorded trace stays an honest picture of what the guest needed.
          </li>
          <li>
            The host connects to the guest agent over vsock, sends the task, and checks the answer. The time from the start of the restore to that
            checked answer is the number every benchmark reports.
          </li>
          <li>When the VM closes, its trace of first touches is stored next to the snapshot.</li>
        </ol>
        <p>
          Every VM runs in its own directory and the snapshot refers to its disk and vsock socket by relative names, so many restores of one snapshot can
          run side by side without patching device state.
        </p>

        <h2 id="store">What is in the store</h2>
        <table>
          <thead>
            <tr>
              <th>Key</th>
              <th>Contents</th>
            </tr>
          </thead>
          <tbody>
            {[
              ["packs/P", "Blocks, each deflated if that saves an eighth or more, packed into objects of about 8 MiB. Immutable."],
              ["index/P", "Which block hashes are in pack P and where. Lets a writer reuse blocks already stored."],
              ["manifests/S/F", "For file F of snapshot S: its size, block size, and per block a pack, offset, length and flags, or a zero mark."],
              ["snapshots/S.json", "The snapshot record: environment, parent, files and their manifest keys. Writing it commits the snapshot."],
              ["traces/S/T", "One recorded restore: the first touch of each block, when it happened and how long the guest waited."],
              ["tomb/P", "A pack garbage collection has condemned."],
            ].map(([k, d]) => (
              <tr key={k}>
                <td>
                  <code>{k}</code>
                </td>
                <td>{d}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <p>
          Blocks are addressed by content, so two snapshots of the same root filesystem share every block neither changed, and a fork stores only what
          its VM wrote. All-zero blocks are never stored; they are most of a fresh VM&apos;s memory.
        </p>

        <h2 id="plans">Plans</h2>
        <p>
          A plan is built from up to 20 recent traces of a snapshot and its ancestors, newest weighted most (each older trace counts 0.8 of the next).
          A block is planned if traces making up at least 30% of the weight touched it, and planned blocks are ordered by their weighted median time of
          first touch. Traces from install-mode restores, where prefetched blocks are copied into the guest before it asks, are left out: they only show
          what prefetch missed.
        </p>
        <p>
          Plans are cheap to rebuild, so none are stored. A snapshot with no traces restores lazily, records a trace, and has a plan from then on.
        </p>

        <h2 id="fetcher">The fetcher</h2>
        <ul>
          <li>16 requests in flight per restore, 4 of them reserved for blocks a guest is waiting on, so a long prefetch queue never delays a fault.</li>
          <li>Neighbouring wanted blocks in one pack are read as one range of up to 4 MiB, reading through holes of up to 256 KiB.</li>
          <li>Two faults on one block share one request. A fault on a block already being prefetched waits for that request instead of starting another.</li>
          <li>Blocks come from the host cache first, then the peer that owns their 4 MiB region, then the store. Every block is checked against its hash.</li>
          <li>
            In a peer ring each region has one owner by rendezvous hashing. The owner reads it from the store once and serves everyone else, so a fleet
            restoring one snapshot reads it about once.
          </li>
        </ul>

        <h2 id="forks">Forks</h2>
        <p>
          A fork pauses the VM and takes a Firecracker diff snapshot, which holds only the pages the guest wrote since it was restored, as a sparse file.
          thaw reads the dirty extents with <code>SEEK_DATA</code>, overlays them on the parent&apos;s blocks, does the same for the disk overlay, and
          writes a child snapshot whose manifests point at the parent&apos;s blocks everywhere else, without reading those. The VM resumes as soon as the
          diff and a copy of its dirty disk blocks are on local disk; the upload happens while it runs.
        </p>

        <h2 id="gc">Garbage collection</h2>
        <p>
          GC runs in two phases so it is safe while snapshots are being written. The first run tombstones every pack no committed manifest uses. A later
          run, once a tombstone is older than the grace period by the store&apos;s clock, checks again and deletes the pack only if nothing has started
          using it since. Writers skip tombstoned packs and must commit within the grace period of looking blocks up, so a writer can never commit a
          reference to a pack GC is about to delete.
        </p>

        <h2 id="reproduce">Reproducing the benchmarks</h2>
        <pre>
          <code>{`sudo -E thaw bench -store /var/lib/thaw/bench-store -out results/vm.json
sudo -E thaw sim   -store /var/lib/thaw/bench-store -out results/sim.json
thaw export -store /var/lib/thaw/bench-store -out web/public/runs
thaw report
cd web && pnpm install && pnpm build`}</code>
        </pre>
        <p>
          The run in the repository took about 3 h 20 min of wall time on a 6-core VM, the largest share of it in lazy restores under S3 Standard timing. It ran in two parts: one Firecracker failed to start, and <code>-resume</code> finished the rest. Results in the repository came from
          Firecracker 1.17 in a Lima VM on an Apple silicon laptop; <code>results/vm.json</code> records the machine.
        </p>

        <h2 id="limits">Limits</h2>
        <ul>
          <li>
            The published numbers come from nested virtualization. Faults and VM exits cost more there than on bare metal, which makes lazy restore look
            worse than it would on a real host. The ordering of strategies is the claim, not the absolute times.
          </li>
          <li>
            The store in the benchmarks is a local directory slowed to each tier&apos;s first-byte latency and per-stream and per-host bandwidth. That
            models the median well and the tail poorly.
          </li>
          <li>
            The real VM runs train and measure on the same five tasks per environment, with different runs. The replay sweep holds each task out, and
            shows how much a plan loses on work it has not seen.
          </li>
          <li>
            Replays keep the recorded compute time between touches, and lose the ordering between vCPUs. They are a model of a restore, labelled as one.
          </li>
          <li>
            One host restores one VM at a time in the benchmarks. The fan-out numbers for many restores at once are replays.
          </li>
          <li>Memory ballooning, huge pages and multiple disks are not supported.</li>
        </ul>
      </article>
    </div>
  );
}
