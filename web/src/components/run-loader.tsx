"use client";

import { useEffect, useState } from "react";
import { Player } from "./player";
import type { Run } from "@/lib/run";

export type RunIndexEntry = { file: string; env: string; task: string; strategy: string; answered_ms: number };

export async function loadIndex(): Promise<RunIndexEntry[]> {
  const r = await fetch("/runs/index.json");
  if (!r.ok) return [];
  return r.json();
}

export async function loadRun(file: string): Promise<Run> {
  const r = await fetch(`/runs/${file}`);
  if (!r.ok) throw new Error(`${file}: ${r.status}`);
  return r.json();
}

// Plays recorded runs of one environment side by side.
export function RecordedPair({ env, strategies, loop = true }: { env: string; strategies: string[]; loop?: boolean }) {
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [missing, setMissing] = useState(false);
  const key = strategies.join(",");
  useEffect(() => {
    let off = false;
    Promise.all(key.split(",").map((s) => loadRun(`${env}-${s}.json`)))
      .then((rs) => !off && setRuns(rs))
      .catch(() => !off && setMissing(true));
    return () => {
      off = true;
    };
  }, [env, key]);
  if (missing) {
    return (
      <div className="instrument p-5 text-[0.9rem] text-night-dim">
        No recorded runs in this build. Run <code className="mono text-night-ink">thaw export</code> after a benchmark to add them.
      </div>
    );
  }
  if (!runs) return <div className="instrument h-[420px]" aria-busy />;
  return <Player runs={runs} loop={loop} compact />;
}
