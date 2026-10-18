import Link from "next/link";
import { CopyCommand } from "@/components/copy-command";
import { RecordedPair } from "@/components/run-loader";
import { envAbout, envOrder, median, vm, vmSummary } from "@/lib/results";
import { fmtMiB, fmtMs, strategyLabel } from "@/lib/run";
import { installCmd } from "@/lib/site";

const steps = [
  {
    title: "Firecracker loads the device state, a few hundred kilobytes, and nothing else.",
    body: "Guest memory is registered with a userfaultfd that thaw serves, and the root disk is an NBD device that thaw serves. The VM resumes before any of its memory exists on the host.",
  },
  {
    title: "Every first touch becomes a block request.",
    body: "A page fault or a disk read asks thaw for one 64 KiB block. thaw takes it from blocks already prefetched, the host cache, a peer host, or the object store, in that order, and copies it into place.",
  },
  {
    title: "A plan says what the guest will want next.",
    body: "Each restore records the order in which the guest touched blocks. The next restore of that snapshot, or of any fork of it, merges those traces into a plan and prefetches in first-touch order, with demand requests on reserved streams so prefetch never sits in front of a fault.",
  },
  {
    title: "Hot blocks can be rewritten next to each other.",
    body: "Repack copies the planned blocks into new packs in plan order, so the plan reads as a few long ranges instead of scattered small ones. The snapshot's identity does not change.",
  },
  {
    title: "Forks upload only what changed.",
    body: "A fork pauses the VM, takes a Firecracker diff snapshot, and writes a child that points at its parent's blocks for everything the guest did not write. The child inherits the parent's traces, so its first restore already has a plan.",
  },
];

export default function Home() {
  const s = vmSummary("s3-express");
  const py = s["py-data"];
  const base = vm?.bases.find((b) => b.env === "py-data");
  const forks = vm?.forks ?? [];
  const std = vmSummary("s3-standard");
  const slower = envOrder.filter((e) => std[e]?.thaw && std[e]?.hydrate && std[e].thaw.answered > std[e].hydrate.answered);

  return (
    <>
      <section className="mx-auto max-w-6xl px-5 pt-16 sm:pt-24">
        <h1 className="max-w-[22ch] text-[2.3rem] font-semibold leading-[1.08] tracking-[-0.02em] sm:text-[3.4rem]">
          Start agent VMs from object storage without downloading them first.
        </h1>
        <p className="mt-6 max-w-[62ch] text-[1.08rem] leading-relaxed text-ink-soft">
          thaw restores Firecracker snapshots lazily, block by block, and prefetches what earlier restores of the same snapshot touched in the order they
          touched it. Forks of a running VM upload only the pages it wrote.
        </p>
        {py?.thaw && py?.full && (
          <p className="mt-4 max-w-[62ch] text-[1.08rem] leading-relaxed text-ink">
            On a 1 GiB VM holding a warm 3M-row DataFrame, the first answer came back in{" "}
            <strong className="mono font-medium">{fmtMs(py.thaw.answered)}</strong>. Downloading the snapshot first took{" "}
            <strong className="mono font-medium">{fmtMs(py.full.answered)}</strong>
            {base && (
              <>
                , and booting the same VM from scratch took <strong className="mono font-medium">{fmtMs(base.boot_ms + base.setup_ms)}</strong>
              </>
            )}
            .
          </p>
        )}
        <div className="mt-8 max-w-2xl">
          <CopyCommand cmd={installCmd} />
          <p className="mt-2 text-[0.84rem] text-ink-faint">
            Linux with KVM for VMs. The store, plan and replay commands also run on macOS.{" "}
            <Link href="/docs#install" className="underline decoration-rule underline-offset-2 hover:text-ink">
              Other ways to install
            </Link>
          </p>
        </div>
      </section>

      <section className="mx-auto mt-16 max-w-6xl px-5">
        <RecordedPair env="py-data" strategies={["full", "thaw"]} />
        <p className="mt-3 max-w-[80ch] text-[0.86rem] leading-relaxed text-ink-faint">
          The same restore twice, on one clock. Each cell is 64 KiB of the snapshot: guest memory first, then the disk blocks that hold data. This is a
          replay of a trace recorded on a real VM against a store shaped like S3 Express One Zone.{" "}
          <Link href="/console" className="underline decoration-rule underline-offset-2 hover:text-ink">
            Open the console
          </Link>{" "}
          to pick other environments and strategies.
        </p>
      </section>

      <section className="mx-auto mt-24 max-w-6xl px-5">
        <h2 className="text-[1.6rem] font-semibold tracking-[-0.01em]">How a restore works</h2>
        <ol className="mt-6 border-t border-ink">
          {steps.map((st, i) => (
            <li key={i} className="grid gap-2 border-b border-rule py-6 sm:grid-cols-[4rem_1fr_1.3fr] sm:gap-8">
              <span className="mono text-[0.9rem] text-ink-faint">{String(i + 1).padStart(2, "0")}</span>
              <h3 className="text-[1.05rem] font-medium leading-snug">{st.title}</h3>
              <p className="text-[0.96rem] leading-relaxed text-ink-soft">{st.body}</p>
            </li>
          ))}
        </ol>
      </section>

      {vm && Object.keys(s).length > 0 && (
        <section className="mx-auto mt-24 max-w-6xl px-5">
          <div className="flex flex-wrap items-baseline justify-between gap-4">
            <h2 className="text-[1.6rem] font-semibold tracking-[-0.01em]">Time to first answer, real VMs</h2>
            <Link href="/benchmarks" className="text-[0.92rem] text-ink-soft underline decoration-rule underline-offset-2 hover:text-ink">
              Every run, both store profiles, forks and replays
            </Link>
          </div>
          <p className="mt-3 max-w-[70ch] text-[0.96rem] leading-relaxed text-ink-soft">
            Medians over five tasks per environment, from the moment the restore starts until the guest&apos;s answer is back and checked. Store shaped
            like S3 Express One Zone. Cold boot is booting the VM and warming its workers from scratch, which is what an agent pays without snapshots.
          </p>
          <div className="mt-6 overflow-x-auto">
            <table className="w-full min-w-[640px] text-left text-[0.95rem]">
              <thead>
                <tr className="border-b border-ink text-[0.84rem] text-ink-faint">
                  <th className="py-2 pr-6 font-normal">Environment</th>
                  <th className="py-2 pr-6 text-right font-normal">Cold boot</th>
                  {["full", "lazy", "hydrate", "thaw"].map((st) => (
                    <th key={st} className={`py-2 pr-6 text-right font-normal ${st === "thaw" ? "text-ink" : ""}`}>
                      {strategyLabel[st]}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {envOrder
                  .filter((e) => s[e])
                  .map((e) => {
                    const b = vm?.bases.find((x) => x.env === e);
                    return (
                      <tr key={e} className="border-b border-rule">
                        <td className="py-3 pr-6">
                          <div className="mono text-[0.9rem]">{e}</div>
                          <div className="text-[0.82rem] text-ink-faint">{envAbout[e]}</div>
                        </td>
                        <td className="mono py-3 pr-6 text-right text-ink-faint">{b ? fmtMs(b.boot_ms + b.setup_ms) : "-"}</td>
                        {["full", "lazy", "hydrate", "thaw"].map((st) => (
                          <td key={st} className={`mono py-3 pr-6 text-right ${st === "thaw" ? "font-medium text-ink" : "text-ink-soft"}`}>
                            {s[e][st] ? fmtMs(s[e][st].answered) : "-"}
                          </td>
                        ))}
                      </tr>
                    );
                  })}
              </tbody>
            </table>
          </div>
        </section>
      )}

      {forks.length > 0 && (
        <section className="mx-auto mt-24 max-w-6xl px-5">
          <h2 className="text-[1.6rem] font-semibold tracking-[-0.01em]">Forks</h2>
          <p className="mt-3 max-w-[70ch] text-[0.96rem] leading-relaxed text-ink-soft">
            After its task, each VM was forked and the fork restored with the plan it inherited. Across {forks.length} forks the VM was paused for a median
            of <span className="mono text-ink">{fmtMs(median(forks.map((f) => f.fork!.pause_ms)))}</span>, the child uploaded a median of{" "}
            <span className="mono text-ink">{fmtMiB(median(forks.map((f) => f.fork!.uploaded)))}</span> after compression, and the fork answered its
            first task in <span className="mono text-ink">{fmtMs(median(forks.filter((f) => f.fork?.restore).map((f) => f.fork!.restore!.answered_ms)))}</span>.
          </p>
        </section>
      )}

      <section className="mx-auto mt-24 max-w-6xl px-5">
        <h2 className="text-[1.6rem] font-semibold tracking-[-0.01em]">What these numbers do not show</h2>
        <ul className="mt-5 max-w-[75ch] list-disc space-y-3 pl-5 text-[0.96rem] leading-relaxed text-ink-soft">
          <li>
            The VMs ran under nested virtualization, Firecracker inside a Linux VM on an Apple silicon laptop. Every page fault costs more there than on bare
            metal, which flatters strategies that fault less.
          </li>
          <li>
            The object store is a local directory slowed down to the first-byte latency and bandwidth of each S3 tier. Real S3 has a long tail this model
            only approximates.
          </li>
          {slower.length > 0 && (
            <li>
              The table above is S3 Express. With S3 Standard timing, thaw was slower than {strategyLabel.hydrate.toLowerCase()} for{" "}
              {slower.join(", ")}.{" "}
              <Link href="/benchmarks" className="underline decoration-rule underline-offset-2 hover:text-ink">
                Both stores are on the benchmarks page
              </Link>
              .
            </li>
          )}
          <li>Plans were trained on the same five tasks they were measured on. The replay sweeps hold each task out to show how a plan does on work it has not seen.</li>
        </ul>
        <p className="mt-4 text-[0.96rem]">
          <Link href="/docs#limits" className="underline decoration-rust underline-offset-[3px]">
            More on method and limits
          </Link>
        </p>
      </section>
    </>
  );
}
