"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { Player } from "@/components/player";
import { loadIndex, loadRun, RunIndexEntry } from "@/components/run-loader";
import { fmtMiB, fmtMs, Run, RunEvent, strategyLabel } from "@/lib/run";

const strategies = ["full", "lazy", "hydrate", "thaw"];
const profiles = ["s3-express", "s3-standard", "peer"];

type Snap = { id: string; env: string; parent?: string; created: string; meta?: Record<string, string>; tasks: string[] };

export function Console() {
  const [mode, setMode] = useState<"recorded" | "server">("recorded");
  return (
    <div>
      <div role="tablist" aria-label="Where runs come from" className="flex gap-6 border-b border-rule">
        {(
          [
            ["recorded", "Recorded runs"],
            ["server", "Local thaw serve"],
          ] as const
        ).map(([m, label]) => (
          <button
            key={m}
            type="button"
            role="tab"
            aria-selected={mode === m}
            onClick={() => setMode(m)}
            className={`-mb-px border-b-2 py-3 text-[0.94rem] ${mode === m ? "border-rust text-ink" : "border-transparent text-ink-faint hover:text-ink-soft"}`}
          >
            {label}
          </button>
        ))}
      </div>
      <div className="mt-6">{mode === "recorded" ? <Recorded /> : <Server />}</div>
    </div>
  );
}

function Select<T extends string>({ label, value, options, onChange, show }: { label: string; value: T; options: T[]; onChange: (v: T) => void; show?: (v: T) => string }) {
  return (
    <label className="flex flex-col gap-1 text-[0.8rem] text-ink-faint">
      {label}
      <select
        value={value}
        onChange={(e) => onChange(e.target.value as T)}
        className="mono rounded-[3px] border border-rule bg-paper px-2 py-1.5 text-[0.86rem] text-ink"
      >
        {options.map((o) => (
          <option key={o} value={o}>
            {show ? show(o) : o}
          </option>
        ))}
      </select>
    </label>
  );
}

function Recorded() {
  const [index, setIndex] = useState<RunIndexEntry[] | null>(null);
  const [env, setEnv] = useState("py-data");
  const [a, setA] = useState("lazy");
  const [b, setB] = useState("thaw");
  const [runs, setRuns] = useState<Run[] | null>(null);

  useEffect(() => {
    loadIndex().then(setIndex).catch(() => setIndex([]));
  }, []);
  useEffect(() => {
    if (!index?.length) return;
    let off = false;
    const pick = [a, b].filter((s, i, xs) => xs.indexOf(s) === i);
    Promise.all(pick.map((s) => loadRun(`${env}-${s}.json`)))
      .then((rs) => !off && setRuns(rs))
      .catch(() => !off && setRuns(null));
    return () => {
      off = true;
    };
  }, [index, env, a, b]);

  if (index === null) return <p className="text-ink-faint">Loading recorded runs.</p>;
  if (!index.length) return <p className="text-ink-soft">This build has no recorded runs. Run thaw export after a benchmark to add them.</p>;
  const envs = [...new Set(index.map((e) => e.env))];
  const task = index.find((e) => e.env === env)?.task;
  return (
    <div>
      <div className="flex flex-wrap items-end gap-4">
        <Select label="Environment" value={env} options={envs} onChange={setEnv} />
        <Select label="Left" value={a} options={strategies} onChange={setA} show={(s) => strategyLabel[s]} />
        <Select label="Right" value={b} options={strategies} onChange={setB} show={(s) => strategyLabel[s]} />
        {task && (
          <p className="pb-1.5 text-[0.86rem] text-ink-faint">
            task <span className="mono text-ink-soft">{task}</span>, replayed against S3 Express One Zone timing
          </p>
        )}
      </div>
      <div className="mt-5">{runs ? <Player runs={runs} autoplay /> : <div className="instrument h-[480px]" aria-busy />}</div>
      {runs && <RunStats runs={runs} />}
    </div>
  );
}

function RunStats({ runs }: { runs: Run[] }) {
  return (
    <table className="mt-6 w-full max-w-3xl text-left text-[0.9rem]">
      <thead>
        <tr className="border-b border-ink text-[0.8rem] text-ink-faint">
          <th className="py-2 pr-4 font-normal" />
          {runs.map((r) => (
            <th key={r.strategy} className="py-2 pr-4 text-right font-normal">
              {strategyLabel[r.strategy] ?? r.strategy}
            </th>
          ))}
        </tr>
      </thead>
      <tbody className="mono">
        {(
          [
            ["answered", (r: Run) => fmtMs(r.marks.answered ?? 0)],
            ["recorded on the VM", (r: Run) => (r.marks.recorded ? fmtMs(r.marks.recorded) : "-")],
            ["blocks touched", (r: Run) => (r.stats.touched ?? 0).toLocaleString("en-US")],
            ["guest waited on", (r: Run) => (r.stats.demand ?? 0).toLocaleString("en-US")],
            ["fetched but unused", (r: Run) => (r.stats.unused ?? 0).toLocaleString("en-US")],
            ["bytes from the store", (r: Run) => fmtMiB(r.stats.bytes ?? 0)],
            ["store requests", (r: Run) => (r.stats.gets ?? 0).toLocaleString("en-US")],
          ] as [string, (r: Run) => string][]
        ).map(([label, f]) => (
          <tr key={label} className="border-b border-rule">
            <td className="py-2 pr-4 font-sans text-ink-soft">{label}</td>
            {runs.map((r) => (
              <td key={r.strategy} className="py-2 pr-4 text-right">
                {f(r)}
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function Server() {
  const [url, setUrl] = useState("http://127.0.0.1:7070");
  const [snaps, setSnaps] = useState<Snap[] | null>(null);
  const [err, setErr] = useState("");
  const [snap, setSnap] = useState("");
  const [task, setTask] = useState("");
  const [strategy, setStrategy] = useState("thaw");
  const [profile, setProfile] = useState("s3-express");
  const [live, setLive] = useState(false);
  const [run, setRun] = useState<Run | null>(null);
  const [running, setRunning] = useState(false);
  const [, bump] = useState(0);
  const es = useRef<EventSource | null>(null);

  const connect = () => {
    setErr("");
    fetch(`${url}/api/snapshots`)
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`${r.status}`))))
      .then((s: Snap[]) => {
        const withTasks = (s ?? []).filter((x) => x.tasks?.length);
        setSnaps(withTasks);
        if (withTasks[0]) {
          setSnap(withTasks[0].id);
          setTask(withTasks[0].tasks[0]);
        }
      })
      .catch((e) => setErr(`Could not reach ${url}: ${e.message}. Start one with: thaw serve -store <your store>`));
  };

  useEffect(() => () => es.current?.close(), []);

  const start = () => {
    es.current?.close();
    setRun(null);
    setErr("");
    setRunning(true);
    const q = new URLSearchParams({ snapshot: snap, task, strategy, profile, live: live ? "1" : "0" });
    const src = new EventSource(`${url}/api/run?${q}`);
    es.current = src;
    let cur: Run | null = null;
    src.addEventListener("head", (e) => {
      cur = { ...(JSON.parse((e as MessageEvent).data) as Run), events: [] };
      setRun(cur);
    });
    src.addEventListener("events", (e) => {
      if (!cur) return;
      cur.events.push(...(JSON.parse((e as MessageEvent).data) as RunEvent[]));
      bump((n) => n + 1);
    });
    src.addEventListener("done", (e) => {
      const d = JSON.parse((e as MessageEvent).data) as Run;
      if (cur) {
        cur.marks = d.marks;
        cur.stats = d.stats;
        cur.events.sort((x, y) => x[0] - y[0]);
        setRun({ ...cur });
      }
      setRunning(false);
      src.close();
    });
    src.addEventListener("error", (e) => {
      const data = (e as MessageEvent).data;
      setErr(data ? JSON.parse(data).error : "The stream closed.");
      setRunning(false);
      src.close();
    });
  };

  const cur = snaps?.find((s) => s.id === snap);
  const shown = useMemo(() => (run ? [run] : []), [run]);
  return (
    <div>
      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-[0.8rem] text-ink-faint">
          Server
          <input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            className="mono w-64 rounded-[3px] border border-rule bg-paper px-2 py-1.5 text-[0.86rem] text-ink"
          />
        </label>
        <button type="button" onClick={connect} className="rounded-[3px] border border-ink px-3 py-1.5 text-[0.88rem] hover:bg-paper-deep">
          Connect
        </button>
      </div>
      {err && <p className="mt-4 max-w-[75ch] text-[0.9rem] text-rust">{err}</p>}
      {snaps && !snaps.length && <p className="mt-4 text-ink-soft">The server has no snapshots with recorded traces yet.</p>}
      {snaps && snaps.length > 0 && (
        <div className="mt-5 flex flex-wrap items-end gap-4">
          <Select
            label="Snapshot"
            value={snap}
            options={snaps.map((s) => s.id)}
            onChange={(v) => {
              setSnap(v);
              setTask(snaps.find((s) => s.id === v)?.tasks[0] ?? "");
            }}
            show={(v) => `${v} (${snaps.find((s) => s.id === v)?.env})`}
          />
          <Select label="Task" value={task} options={cur?.tasks ?? []} onChange={setTask} />
          <Select label="Strategy" value={strategy} options={strategies} onChange={setStrategy} show={(s) => strategyLabel[s]} />
          <Select label="Store" value={profile} options={profiles} onChange={setProfile} />
          <label className="flex items-center gap-2 pb-1.5 text-[0.86rem] text-ink-soft">
            <input type="checkbox" checked={live} onChange={(e) => setLive(e.target.checked)} />
            restore a real VM
          </label>
          <button
            type="button"
            disabled={running}
            onClick={start}
            className="rounded-[3px] bg-ink px-4 py-1.5 text-[0.88rem] text-paper disabled:opacity-50"
          >
            {running ? "Running" : "Run"}
          </button>
        </div>
      )}
      {run && (
        <div className="mt-5">
          <Player runs={shown} live={running} autoplay />
          {!running && <RunStats runs={[run]} />}
        </div>
      )}
    </div>
  );
}
