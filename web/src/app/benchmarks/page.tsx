import type { Metadata } from "next";
import Link from "next/link";
import { HBars, Lines } from "@/components/charts";
import { Tabs } from "@/components/tabs";
import { envAbout, envOrder, median, sim, simRows, simSummary, strategyOrder, vm, vmSummary } from "@/lib/results";
import { fmtMiB, fmtMs, strategyLabel } from "@/lib/run";

export const metadata: Metadata = {
  title: "Benchmarks",
  description: "Every restore thaw's benchmarks ran on real Firecracker VMs, and the replay sweeps built from their traces.",
};

const profileLabel: Record<string, string> = {
  "s3-express": "S3 Express One Zone",
  "s3-standard": "S3 Standard",
  peer: "Peer host on the LAN",
};

function H2({ id, children }: { id: string; children: React.ReactNode }) {
  return (
    <h2 id={id} className="mt-20 scroll-mt-20 border-t border-ink pt-5 text-[1.5rem] font-semibold tracking-[-0.01em]">
      {children}
    </h2>
  );
}

function P({ children }: { children: React.ReactNode }) {
  return <p className="mt-3 max-w-[72ch] text-[0.96rem] leading-relaxed text-ink-soft">{children}</p>;
}

function Th({ children, right }: { children?: React.ReactNode; right?: boolean }) {
  return <th className={`py-2 pr-5 text-[0.8rem] font-normal text-ink-faint ${right ? "text-right" : ""}`}>{children}</th>;
}

function Td({ children, right, strong }: { children: React.ReactNode; right?: boolean; strong?: boolean }) {
  return <td className={`py-2 pr-5 ${right ? "mono text-right" : ""} ${strong ? "font-medium text-ink" : "text-ink-soft"}`}>{children}</td>;
}

function Table({ head, children, min = 640 }: { head: React.ReactNode; children: React.ReactNode; min?: number }) {
  return (
    <div className="mt-5 overflow-x-auto">
      <table className="w-full text-left text-[0.92rem]" style={{ minWidth: min }}>
        <thead>
          <tr className="border-b border-ink">{head}</tr>
        </thead>
        <tbody className="[&>tr]:border-b [&>tr]:border-rule">{children}</tbody>
      </table>
    </div>
  );
}

function VmProfile({ profile }: { profile: string }) {
  const s = vmSummary(profile);
  return (
    <div className="space-y-12">
      {envOrder
        .filter((e) => s[e])
        .map((e) => {
          const base = vm!.bases.find((b) => b.env === e);
          const rows = strategyOrder.filter((st) => s[e][st]);
          const cold = base ? base.boot_ms + base.setup_ms : NaN;
          const max = Math.max(...rows.map((st) => s[e][st].answered));
          return (
            <div key={e}>
              <h3 className="mono text-[1rem] font-medium">{e}</h3>
              <p className="text-[0.86rem] text-ink-faint">{envAbout[e]}</p>
              <div className="mt-4">
                <HBars
                  max={max}
                  fmt={fmtMs}
                  bars={[
                    ...rows.map((st) => ({ label: strategyLabel[st], value: s[e][st].answered, tone: st.startsWith("thaw") ? ("thaw" as const) : undefined })),
                    ...(base ? [{ label: "Cold boot", value: cold, tone: "warn" as const, note: cold > max ? "(off the scale)" : "" }] : []),
                  ]}
                />
              </div>
              <Table
                head={
                  <>
                    <Th>Strategy</Th>
                    <Th right>Median</Th>
                    <Th right>p90</Th>
                    <Th right>Waited on</Th>
                    <Th right>Fetched</Th>
                    <Th right>Requests</Th>
                    <Th right>Unused</Th>
                    <Th right>Correct</Th>
                  </>
                }
              >
                {rows.map((st) => {
                  const c = s[e][st];
                  return (
                    <tr key={st}>
                      <Td strong={st.startsWith("thaw")}>{strategyLabel[st]}</Td>
                      <Td right strong={st.startsWith("thaw")}>
                        {fmtMs(c.answered)}
                      </Td>
                      <Td right>{fmtMs(c.p90)}</Td>
                      <Td right>{Math.round(c.demand).toLocaleString("en-US")}</Td>
                      <Td right>{fmtMiB(c.bytes)}</Td>
                      <Td right>{Math.round(c.requests).toLocaleString("en-US")}</Td>
                      <Td right>{Math.round(c.unused).toLocaleString("en-US")}</Td>
                      <Td right>
                        {c.ok}/{c.n}
                      </Td>
                    </tr>
                  );
                })}
              </Table>
              <PerTask env={e} profile={profile} />
            </div>
          );
        })}
    </div>
  );
}

function PerTask({ env, profile }: { env: string; profile: string }) {
  const runs = vm!.runs.filter((r) => r.env === env && r.profile === profile);
  const tasks = [...new Set(runs.map((r) => r.task))];
  const sts = strategyOrder.filter((st) => runs.some((r) => r.strategy === st));
  return (
    <details className="mt-4">
      <summary className="cursor-pointer text-[0.88rem] text-ink-soft hover:text-ink">By task</summary>
      <Table
        min={560}
        head={
          <>
            <Th>Task</Th>
            {sts.map((st) => (
              <Th key={st} right>
                {strategyLabel[st]}
              </Th>
            ))}
          </>
        }
      >
        {tasks.map((t) => (
          <tr key={t}>
            <Td>
              <span className="mono text-[0.86rem]">{t}</span>
            </Td>
            {sts.map((st) => {
              const xs = runs.filter((r) => r.task === t && r.strategy === st).map((r) => r.answered_ms);
              return (
                <Td key={st} right strong={st === "thaw"}>
                  {xs.length ? fmtMs(median(xs)) : "-"}
                </Td>
              );
            })}
          </tr>
        ))}
      </Table>
    </details>
  );
}

function Forks() {
  const fs = vm?.forks ?? [];
  if (!fs.length) return <P>No forks in this run.</P>;
  return (
    <Table
      head={
        <>
          <Th>Env</Th>
          <Th>Task</Th>
          <Th right>Paused</Th>
          <Th right>Dirty memory</Th>
          <Th right>Dirty disk</Th>
          <Th right>Uploaded</Th>
          <Th right>Fork total</Th>
          <Th right>Fork answered</Th>
          <Th right>Parent answered</Th>
        </>
      }
      min={820}
    >
      {fs.map((r) => (
        <tr key={r.fork!.snapshot}>
          <Td>
            <span className="mono text-[0.86rem]">{r.env}</span>
          </Td>
          <Td>
            <span className="mono text-[0.86rem]">{r.task}</span>
          </Td>
          <Td right>{fmtMs(r.fork!.pause_ms)}</Td>
          <Td right>{fmtMiB(r.fork!.dirty_mem * 65536)}</Td>
          <Td right>{fmtMiB(r.fork!.dirty_disk * 65536)}</Td>
          <Td right>{fmtMiB(r.fork!.uploaded)}</Td>
          <Td right>{fmtMs(r.fork!.total_ms)}</Td>
          <Td right strong>
            {r.fork!.restore ? fmtMs(r.fork!.restore.answered_ms) : "-"}
          </Td>
          <Td right>{fmtMs(r.answered_ms)}</Td>
        </tr>
      ))}
    </Table>
  );
}

function Repack() {
  if (!vm?.repacks?.length) return <P>Repack was not part of this run.</P>;
  return (
    <Table
      head={
        <>
          <Th>Env</Th>
          <Th>Store</Th>
          <Th right>Hot blocks</Th>
          <Th right>thaw</Th>
          <Th right>Requests</Th>
          <Th right>thaw, repacked</Th>
          <Th right>Requests</Th>
        </>
      }
    >
      {vm.repacks.flatMap((rp) =>
        vm!.profiles.map((p) => {
          const s = vmSummary(p)[rp.env] ?? {};
          return (
            <tr key={rp.env + p}>
              <Td>
                <span className="mono text-[0.86rem]">{rp.env}</span>
              </Td>
              <Td>{profileLabel[p] ?? p}</Td>
              <Td right>{rp.blocks.toLocaleString("en-US")}</Td>
              <Td right>{s.thaw ? fmtMs(s.thaw.answered) : "-"}</Td>
              <Td right>{s.thaw ? Math.round(s.thaw.requests).toLocaleString("en-US") : "-"}</Td>
              <Td right strong>
                {s["thaw+repack"] ? fmtMs(s["thaw+repack"].answered) : "-"}
              </Td>
              <Td right>{s["thaw+repack"] ? Math.round(s["thaw+repack"].requests).toLocaleString("en-US") : "-"}</Td>
            </tr>
          );
        }),
      )}
    </Table>
  );
}

function SimStrategies() {
  const variants = ["full", "lazy", "hydrate", "thaw", "thaw, unseen task"];
  const label: Record<string, string> = { ...strategyLabel, "thaw, unseen task": "thaw, task held out" };
  const profiles = ["s3-standard", "s3-express", "peer"];
  return (
    <Tabs label="Store profile" labels={profiles.map((p) => profileLabel[p])}>
      {profiles.map((p) => {
        const s = simSummary("strategy", p);
        return (
          <div key={p} className="space-y-10">
            {envOrder
              .filter((e) => s[e])
              .map((e) => (
                <div key={e}>
                  <h3 className="mono text-[1rem] font-medium">{e}</h3>
                  <div className="mt-3">
                    <HBars
                      fmt={fmtMs}
                      bars={variants
                        .filter((v) => s[e][v])
                        .map((v) => ({ label: label[v] ?? v, value: s[e][v].answered, tone: v.startsWith("thaw") ? ("thaw" as const) : undefined }))}
                    />
                  </div>
                </div>
              ))}
          </div>
        );
      })}
    </Tabs>
  );
}

function SimFetch() {
  const s = simSummary("fetch", "s3-express");
  const envs = envOrder.filter((e) => s[e]);
  if (!envs.length) return <P>Not run.</P>;
  const variants = Object.keys(s[envs[0]]);
  return (
    <Table
      head={
        <>
          <Th>Fetcher</Th>
          {envs.map((e) => (
            <Th key={e} right>
              {e}
            </Th>
          ))}
          {envs.map((e) => (
            <Th key={e + "g"} right>
              requests, {e}
            </Th>
          ))}
        </>
      }
      min={820}
    >
      {variants.map((v) => (
        <tr key={v}>
          <Td>{v}</Td>
          {envs.map((e) => (
            <Td key={e} right strong={v === "thaw, 16 streams"}>
              {s[e][v] ? fmtMs(s[e][v].answered) : "-"}
            </Td>
          ))}
          {envs.map((e) => (
            <Td key={e + "g"} right>
              {s[e][v] ? Math.round(s[e][v].gets).toLocaleString("en-US") : "-"}
            </Td>
          ))}
        </tr>
      ))}
    </Table>
  );
}

function SimTraining() {
  const s = simSummary("training", "s3-express");
  const envs = envOrder.filter((e) => s[e]);
  if (!envs.length) return <P>Not run.</P>;
  const ks = ["0 traces", "1 traces", "2 traces", "3 traces", "5 traces", "9 traces"].filter((k) => envs.some((e) => s[e][k]));
  const colors = ["var(--rust)", "var(--amber)", "var(--sage)"];
  return (
    <div className="grid gap-10 lg:grid-cols-2">
      <div>
        <h3 className="text-[0.95rem] font-medium">Time to answer</h3>
        <Lines
          xs={ks.map((k) => k.split(" ")[0])}
          xLabel="traces the plan learned from"
          fmtY={fmtMs}
          series={envs.map((e, i) => ({ name: e, color: colors[i % 3], values: ks.map((k) => s[e][k]?.answered ?? NaN) }))}
        />
      </div>
      <div>
        <h3 className="text-[0.95rem] font-medium">Share of touched blocks the plan had</h3>
        <Lines
          xs={ks.map((k) => k.split(" ")[0])}
          xLabel="traces the plan learned from"
          fmtY={(v) => `${Math.round(v * 100)}%`}
          series={envs.map((e, i) => ({ name: e, color: colors[i % 3], values: ks.map((k) => s[e][k]?.recall ?? NaN) }))}
        />
      </div>
    </div>
  );
}

function SimMinFreq() {
  const s = simSummary("minfreq", "s3-express");
  const envs = envOrder.filter((e) => s[e]);
  if (!envs.length) return <P>Not run.</P>;
  const vs = ["min share 0.1", "min share 0.2", "min share 0.3", "min share 0.5", "min share 0.8"].filter((v) => envs.some((e) => s[e][v]));
  return (
    <Table
      head={
        <>
          <Th>Planned if in at least</Th>
          {envs.map((e) => (
            <Th key={e} right>
              {e}
            </Th>
          ))}
          {envs.map((e) => (
            <Th key={e + "r"} right>
              {e} read
            </Th>
          ))}
        </>
      }
    >
      {vs.map((v) => (
        <tr key={v}>
          <Td>{`${Math.round(Number(v.split(" ")[2]) * 100)}% of traces`}</Td>
          {envs.map((e) => (
            <Td key={e} right strong={v === "min share 0.3"}>
              {s[e][v] ? fmtMs(s[e][v].answered) : "-"}
            </Td>
          ))}
          {envs.map((e) => (
            <Td key={e + "r"} right>
              {s[e][v] ? fmtMiB(s[e][v].bytes) : "-"}
            </Td>
          ))}
        </tr>
      ))}
    </Table>
  );
}

function SimFanout() {
  const rows = simRows("fanout");
  if (!rows.length) return <P>Not run.</P>;
  return (
    <Table
      head={
        <>
          <Th>Env</Th>
          <Th>Setup</Th>
          <Th right>Restores at once</Th>
          <Th right>p50</Th>
          <Th right>p95</Th>
          <Th right>Store GETs</Th>
          <Th right>Read from store</Th>
          <Th right>Request cost per 1000</Th>
        </>
      }
      min={820}
    >
      {rows.map((r, i) => (
        <tr key={i}>
          <Td>
            <span className="mono text-[0.86rem]">{r.env}</span>
          </Td>
          <Td>{r.variant}</Td>
          <Td right>{r.n}</Td>
          <Td right>{fmtMs(r.p50_ms ?? NaN)}</Td>
          <Td right>{fmtMs(r.p95_ms ?? NaN)}</Td>
          <Td right>{r.gets.toLocaleString("en-US")}</Td>
          <Td right>{fmtMiB(r.bytes)}</Td>
          <Td right strong={r.variant !== "independent"}>
            ${(r.cost_per_1000_usd ?? 0).toFixed(3)}
          </Td>
        </tr>
      ))}
    </Table>
  );
}

function SimBlocksize() {
  const s = simSummary("blocksize", "s3-express");
  const envs = envOrder.filter((e) => s[e]);
  if (!envs.length) return <P>Not run.</P>;
  const sizes = ["64 KiB", "256 KiB", "1024 KiB"];
  return (
    <Table
      head={
        <>
          <Th>Env</Th>
          <Th>Strategy</Th>
          {sizes.map((z) => (
            <Th key={z} right>
              {z}
            </Th>
          ))}
          {sizes.map((z) => (
            <Th key={z + "b"} right>
              read, {z}
            </Th>
          ))}
        </>
      }
      min={820}
    >
      {envs.flatMap((e) =>
        ["thaw", "lazy"].map((st) => (
          <tr key={e + st}>
            <Td>
              <span className="mono text-[0.86rem]">{e}</span>
            </Td>
            <Td>{strategyLabel[st]}</Td>
            {sizes.map((z) => (
              <Td key={z} right strong={st === "thaw"}>
                {s[e][`${st}, ${z}`] ? fmtMs(s[e][`${st}, ${z}`].answered) : "-"}
              </Td>
            ))}
            {sizes.map((z) => (
              <Td key={z + "b"} right>
                {s[e][`${st}, ${z}`] ? fmtMiB(s[e][`${st}, ${z}`].bytes) : "-"}
              </Td>
            ))}
          </tr>
        )),
      )}
    </Table>
  );
}

export default function Benchmarks() {
  return (
    <div className="mx-auto max-w-6xl px-5 pt-14">
      <h1 className="text-[2rem] font-semibold tracking-[-0.02em]">Benchmarks</h1>
      <P>
        Two kinds of numbers, kept apart. Real VM runs boot, restore and fork Firecracker VMs and check every answer. Replays take the block traces those
        VMs recorded and play them back against conditions the test machine cannot produce: other store tiers, other fetcher settings, fleets of hosts.
        Every number here comes from <code className="mono text-[0.9em]">results/vm.json</code> or <code className="mono text-[0.9em]">results/sim.json</code>{" "}
        in the repository, and <Link href="/docs#reproduce" className="underline decoration-rust underline-offset-[3px]">the docs say how to rerun them</Link>.
      </P>
      <nav className="mt-6 flex flex-wrap gap-x-6 gap-y-2 text-[0.9rem] text-ink-soft">
        {[
          ["vm", "Real VMs"],
          ["forks", "Forks"],
          ["repack", "Repack"],
          ["strategies", "Replay: store tiers"],
          ["fetch", "Replay: fetcher"],
          ["training", "Replay: plan training"],
          ["minfreq", "Replay: plan threshold"],
          ["fanout", "Replay: fan-out and cost"],
          ["blocksize", "Replay: block size"],
        ].map(([id, l]) => (
          <a key={id} href={`#${id}`} className="underline decoration-rule underline-offset-2 hover:text-ink">
            {l}
          </a>
        ))}
      </nav>

      <H2 id="vm">Real VMs</H2>
      {vm ? (
        <>
          <P>
            {vm.machine}. {vm.note} Each environment ran {vm.training_traces > 0 ? `after ${vm.training_traces} training restores in all, ` : ""}with
            every strategy on each of its five tasks, twice. Times run from the start of the restore to a checked answer. &quot;Waited on&quot; counts
            blocks the guest stopped for; &quot;unused&quot; counts blocks fetched that the guest never touched.
          </P>
          <div className="mt-8">
            <Tabs label="Store profile" labels={vm.profiles.map((p) => profileLabel[p] ?? p)}>
              {vm.profiles.map((p) => (
                <VmProfile key={p} profile={p} />
              ))}
            </Tabs>
          </div>
        </>
      ) : (
        <P>No real VM results in this build.</P>
      )}

      <H2 id="forks">Forks</H2>
      <P>
        After the first thaw run of each task the VM was forked: paused, diff-snapshotted, and written as a child of its snapshot that stores only what
        the guest wrote. The child was then restored with the plan it inherited and given the same task.
      </P>
      <Forks />

      <H2 id="repack">Repack</H2>
      <P>
        Once the training traces were in, each base snapshot was repacked so its planned blocks sit together in plan order, and thaw ran again. Fewer,
        longer range reads matter most where every request pays a long first byte.
      </P>
      <Repack />

      {sim ? (
        <>
          <H2 id="strategies">Replay: store tiers</H2>
          <P>
            {sim.note} For each task the newest trace is replayed; the plan learns from every other trace, or, in &quot;task held out&quot;, only from
            traces of the environment&apos;s other tasks.
          </P>
          <div className="mt-6">
            <SimStrategies />
          </div>

          <H2 id="fetch">Replay: fetcher</H2>
          <P>
            thaw with its plan under S3 Express timing, changing one knob at a time from the default of 16 streams with 4 reserved for demand, 4 MiB
            coalesced ranges, and no fault-around. Lazy rows show what fault-around buys a restore with no plan.
          </P>
          <SimFetch />

          <H2 id="training">Replay: plan training</H2>
          <P>How many recorded restores a plan needs before it stops improving. Zero traces is lazy restore.</P>
          <div className="mt-6">
            <SimTraining />
          </div>

          <H2 id="minfreq">Replay: plan threshold</H2>
          <P>
            A block is planned when at least a set share of the traces touched it, 30% by default. The traces pool all five tasks of an environment,
            so a block only one task touches shows up in about a fifth of them. Lower thresholds keep those blocks and make every other task fetch them
            too. Time to answer is the median over tasks, each replayed with a plan built from the other traces.
          </P>
          <SimMinFreq />

          <H2 id="fanout">Replay: fan-out and cost</H2>
          <P>
            Many agents restoring the same snapshot at once, as when one environment fans out into parallel attempts. Independent hosts each read the
            hot set from the store; one host with a block cache reads it once; four hosts in a peer ring read each 4 MiB region once between them.
            Cost counts S3 Standard request prices only, since same-region transfer is free.
          </P>
          <SimFanout />

          <H2 id="blocksize">Replay: block size</H2>
          <P>
            The same snapshots rewritten at bigger blocks and the same traces mapped onto them. Bigger blocks mean fewer requests and more bytes nobody
            asked for. Sizes under the recorded 64 KiB cannot be replayed.
          </P>
          <SimBlocksize />
        </>
      ) : (
        <>
          <H2 id="strategies">Replays</H2>
          <P>No replay results in this build.</P>
        </>
      )}
    </div>
  );
}
