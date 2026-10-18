// A Run is what `thaw serve` streams and `thaw export` writes: the shape of
// each snapshot file, then every block event in time order.

export type RunFile = { name: string; blocks: number; block: number; zero_runs: number[] };

// [ms since the restore began, file index, block, kind]
export type RunEvent = [number, number, number, number];

export type Run = {
  snapshot: string;
  env: string;
  task: string;
  strategy: string;
  profile: string;
  source: "replay" | "vm";
  files: RunFile[];
  events: RunEvent[];
  marks: Record<string, number>;
  stats: Record<string, number>;
};

export const Kind = { Demand: 0, Staged: 1, Cached: 2, Zero: 3, Arrived: 4 } as const;

// What a cell shows. Order matters: a cell only moves forward.
export const Cell = { Zero: 0, Cold: 1, Arrived: 2, Hit: 3, Waited: 4 } as const;
export type CellState = (typeof Cell)[keyof typeof Cell];

export const cellColor: Record<number, string> = {
  [Cell.Cold]: "var(--frost-night)",
  [Cell.Arrived]: "var(--amber)",
  [Cell.Hit]: "var(--sage)",
  [Cell.Waited]: "var(--rust)",
};

// Resolved hex values for canvas, which can't read CSS variables cheaply.
export const cellHex: Record<number, string> = {
  [Cell.Zero]: "#1b1a18",
  [Cell.Cold]: "#2b3539",
  [Cell.Arrived]: "#d79a3e",
  [Cell.Hit]: "#6f9a72",
  [Cell.Waited]: "#c4512c",
};

export function zeroMap(f: RunFile): Uint8Array {
  const z = new Uint8Array(f.blocks);
  let i = 0;
  let zero = false;
  for (const n of f.zero_runs) {
    if (zero) z.fill(1, i, i + n);
    i += n;
    zero = !zero;
  }
  return z;
}

// The state of every block of every file at time t.
export class RunState {
  cells: Uint8Array[];
  next = 0; // index of the first event not yet applied
  waited = 0;
  hit = 0;
  arrived = 0;
  touched = 0;
  zeros: Uint8Array[];

  constructor(public run: Run) {
    this.zeros = run.files.map(zeroMap);
    this.cells = this.zeros.map((z) => z.map((v) => (v ? Cell.Zero : Cell.Cold)));
  }

  reset() {
    this.cells = this.zeros.map((z) => z.map((v) => (v ? Cell.Zero : Cell.Cold)));
    this.next = this.waited = this.hit = this.arrived = this.touched = 0;
  }

  // Applies events up to t and calls changed for every cell that moved.
  advance(t: number, changed?: (file: number, block: number, state: number) => void) {
    const ev = this.run.events;
    while (this.next < ev.length && ev[this.next][0] <= t) {
      const [, f, b, k] = ev[this.next++];
      const cells = this.cells[f];
      if (!cells || b >= cells.length) continue;
      const cur = cells[b];
      let to: number = cur;
      if (k === Kind.Arrived) {
        if (cur === Cell.Cold) to = Cell.Arrived;
        this.arrived++;
      } else if (k === Kind.Demand) {
        to = Cell.Waited;
        this.waited++;
        this.touched++;
      } else if (k === Kind.Staged || k === Kind.Cached) {
        to = Cell.Hit;
        this.hit++;
        this.touched++;
      } else if (k === Kind.Zero) {
        this.touched++;
      }
      if (to !== cur) {
        cells[b] = to;
        changed?.(f, b, to);
      }
    }
  }

  get done() {
    return this.next >= this.run.events.length;
  }
}

export function answeredAt(run: Run) {
  return run.marks.answered ?? (run.events.length ? run.events[run.events.length - 1][0] : 0);
}

export const fmtMs = (ms: number) => (ms >= 10000 ? `${(ms / 1000).toFixed(1)} s` : ms >= 1000 ? `${(ms / 1000).toFixed(2)} s` : `${Math.round(ms)} ms`);

export const fmtMiB = (bytes: number) => `${(bytes / 2 ** 20).toFixed(bytes < 10 * 2 ** 20 ? 1 : 0)} MiB`;

export const strategyLabel: Record<string, string> = {
  full: "Download first",
  lazy: "Lazy",
  hydrate: "Lazy + hydrate",
  thaw: "thaw",
  "thaw+repack": "thaw, repacked",
};
