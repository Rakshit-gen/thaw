// Types and summaries for results/vm.json (real VMs) and results/sim.json
// (replays). scripts/sync-data.mjs copies both into src/data before a build.

import vmRaw from "@/data/vm.json";
import simRaw from "@/data/sim.json";

export type VmRun = {
  snapshot: string;
  env: string;
  task: string;
  strategy: string;
  profile: string;
  ok: boolean;
  err?: string;
  resumed_ms: number;
  connected_ms: number;
  answered_ms: number;
  download_ms?: number;
  guest_ms: number;
  touched: number;
  from_stage: number;
  demand: number;
  staged: number;
  unused: number;
  planned: number;
  bytes: number;
  requests: number;
  fork?: {
    snapshot: string;
    pause_ms: number;
    total_ms: number;
    dirty_mem: number;
    dirty_disk: number;
    uploaded: number;
    new: number;
    reused: number;
    restore?: VmRun;
  };
};

export type VmBase = { env: string; snapshot: string; boot_ms: number; setup_ms: number; snapshot_ms: number; upload_ms: number; stored: number; mem_mib: number };

export type VmResult = {
  when: string;
  machine: string;
  note: string;
  profiles: string[];
  bases: VmBase[];
  runs: VmRun[];
  forks: VmRun[];
  repacks?: { env: string; blocks: number; bytes: number; packs: number; took_ms: number }[];
  training_traces: number;
} | null;

export type SimRow = {
  sweep: string;
  env: string;
  task?: string;
  variant: string;
  profile: string;
  answered_ms: number;
  recorded_ms: number;
  stalled_ms: number;
  stalls: number;
  gets: number;
  bytes: number;
  touched: number;
  planned?: number;
  precision?: number;
  recall?: number;
  n?: number;
  p50_ms?: number;
  p95_ms?: number;
  cost_per_1000_usd?: number;
};

export type SimResult = { when: string; note: string; rows: SimRow[] } | null;

// Go writes empty slices as null.
const rawVm = vmRaw as unknown as VmResult;
export const vm: VmResult = rawVm && {
  ...rawVm,
  bases: rawVm.bases ?? [],
  runs: rawVm.runs ?? [],
  forks: rawVm.forks ?? [],
  repacks: rawVm.repacks ?? [],
  profiles: rawVm.profiles ?? [],
};
const rawSim = simRaw as unknown as SimResult;
export const sim: SimResult = rawSim && { ...rawSim, rows: rawSim.rows ?? [] };

export const envOrder = ["py-data", "repo", "node-ts"];
export const strategyOrder = ["full", "lazy", "hydrate", "thaw", "thaw+repack"];

export const envAbout: Record<string, string> = {
  "py-data": "Python kernel holding a 3M-row pandas DataFrame, 1 GiB VM",
  repo: "Django checkout with git and Python warmed up, 1 GiB VM",
  "node-ts": "TypeScript language service over the compiler's own source, 1.5 GiB VM",
};

export function median(xs: number[]) {
  if (!xs.length) return NaN;
  const s = [...xs].sort((a, b) => a - b);
  const m = Math.floor(s.length / 2);
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
}

export function pct(xs: number[], p: number) {
  if (!xs.length) return NaN;
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.min(s.length - 1, Math.floor((s.length - 1) * p + 0.5))];
}

export type Cellsum = { n: number; ok: number; answered: number; p90: number; demand: number; bytes: number; requests: number; unused: number };

// Medians of the real-VM runs, by env, profile and strategy.
export function vmSummary(profile: string) {
  const out: Record<string, Record<string, Cellsum>> = {};
  if (!vm) return out;
  for (const env of envOrder) {
    for (const st of strategyOrder) {
      const rs = vm.runs.filter((r) => r.env === env && r.strategy === st && r.profile === profile);
      if (!rs.length) continue;
      (out[env] ??= {})[st] = {
        n: rs.length,
        ok: rs.filter((r) => r.ok).length,
        answered: median(rs.map((r) => r.answered_ms)),
        p90: pct(rs.map((r) => r.answered_ms), 0.9),
        demand: median(rs.map((r) => r.demand)),
        bytes: median(rs.map((r) => r.bytes)),
        requests: median(rs.map((r) => r.requests)),
        unused: median(rs.map((r) => r.unused)),
      };
    }
  }
  return out;
}

export function simRows(sweep: string) {
  return sim?.rows.filter((r) => r.sweep === sweep) ?? [];
}

// Median of a sim sweep, grouped by env and variant (and profile).
export function simSummary(sweep: string, profile?: string) {
  const out: Record<string, Record<string, { answered: number; gets: number; bytes: number; precision: number; recall: number; n: number }>> = {};
  for (const r of simRows(sweep)) {
    if (profile && r.profile !== profile) continue;
    const g = ((out[r.env] ??= {})[r.variant] ??= { answered: 0, gets: 0, bytes: 0, precision: 0, recall: 0, n: 0 });
    g.n++;
  }
  for (const env of Object.keys(out)) {
    for (const v of Object.keys(out[env])) {
      const rs = simRows(sweep).filter((r) => r.env === env && r.variant === v && (!profile || r.profile === profile));
      out[env][v] = {
        n: rs.length,
        answered: median(rs.map((r) => r.answered_ms)),
        gets: median(rs.map((r) => r.gets)),
        bytes: median(rs.map((r) => r.bytes)),
        precision: median(rs.map((r) => r.precision ?? 0)),
        recall: median(rs.map((r) => r.recall ?? 0)),
      };
    }
  }
  return out;
}
