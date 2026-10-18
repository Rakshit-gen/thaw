"use client";

import { forwardRef, useCallback, useEffect, useImperativeHandle, useRef } from "react";
import { Cell, cellHex, RunState } from "@/lib/run";

export type GridHandle = {
  // Redraw everything, after a reset or a jump backwards.
  repaint: () => void;
  // Draw one cell that changed.
  paint: (file: number, block: number, state: number) => void;
};

type Layout = { file: number; x: number; y: number; cols: number; cell: number; gap: number; order: Int32Array | null; label: string; rows: number };

// Memory is drawn whole, a row of 128 blocks per 8 MiB. The disk is mostly
// empty space, so only its non-zero blocks are drawn, packed in order.
function layout(state: RunState, width: number) {
  const out: Layout[] = [];
  let y = 0;
  state.run.files.forEach((f, i) => {
    const isMem = f.name === "mem";
    let order: Int32Array | null = null;
    let n = f.blocks;
    if (!isMem) {
      const z = state.zeros[i];
      const idx: number[] = [];
      for (let b = 0; b < z.length; b++) if (!z[b]) idx.push(b);
      order = new Int32Array(f.blocks).fill(-1);
      idx.forEach((b, j) => (order![b] = j));
      n = idx.length;
    }
    const cols = 128;
    const gap = 1;
    const cell = Math.max(1, Math.floor((width - (cols - 1) * gap) / cols));
    const rows = Math.ceil(n / cols);
    const label = isMem ? `memory, ${f.blocks.toLocaleString("en-US")} blocks` : `disk, ${n.toLocaleString("en-US")} non-zero of ${f.blocks.toLocaleString("en-US")} blocks`;
    y += 22;
    out.push({ file: i, x: 0, y, cols, cell, gap, order, label, rows });
    y += rows * (cell + gap) + 14;
  });
  return { parts: out, height: y };
}

export const BlockGrid = forwardRef<GridHandle, { state: RunState; className?: string }>(function BlockGrid({ state, className }, ref) {
  const canvas = useRef<HTMLCanvasElement>(null);
  const lay = useRef<ReturnType<typeof layout> | null>(null);

  const cellAt = (p: Layout, block: number) => {
    const j = p.order ? p.order[block] : block;
    if (j < 0) return null;
    return { x: (j % p.cols) * (p.cell + p.gap), y: p.y + Math.floor(j / p.cols) * (p.cell + p.gap) };
  };

  const repaint = useCallback(() => {
    const c = canvas.current;
    if (!c) return;
    const width = c.parentElement?.clientWidth ?? 512;
    const dpr = window.devicePixelRatio || 1;
    const l = layout(state, width);
    lay.current = l;
    const w = l.parts[0] ? l.parts[0].cols * (l.parts[0].cell + 1) - 1 : width;
    c.width = Math.round(w * dpr);
    c.height = Math.round(l.height * dpr);
    c.style.width = `${w}px`;
    c.style.height = `${l.height}px`;
    const g = c.getContext("2d")!;
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, w, l.height);
    g.font = "11px var(--font-mono), ui-monospace, monospace";
    for (const p of l.parts) {
      g.fillStyle = "#8f897d";
      g.fillText(p.label, 0, p.y - 8);
      const cells = state.cells[p.file];
      for (let b = 0; b < cells.length; b++) {
        const at = cellAt(p, b);
        if (!at) continue;
        g.fillStyle = cellHex[cells[b]];
        g.fillRect(at.x, at.y, p.cell, p.cell);
      }
    }
  }, [state]);

  const paint = useCallback((file: number, block: number, s: number) => {
    const c = canvas.current;
    const l = lay.current;
    if (!c || !l) return;
    const p = l.parts[file];
    if (!p) return;
    const at = cellAt(p, block);
    if (!at) return;
    const g = c.getContext("2d")!;
    g.fillStyle = cellHex[s] ?? cellHex[Cell.Cold];
    g.fillRect(at.x, at.y, p.cell, p.cell);
  }, []);

  useImperativeHandle(ref, () => ({ repaint, paint }), [repaint, paint]);

  useEffect(() => {
    repaint();
    const ro = new ResizeObserver(() => repaint());
    if (canvas.current?.parentElement) ro.observe(canvas.current.parentElement);
    return () => ro.disconnect();
  }, [repaint]);

  return (
    <div className={className}>
      <canvas ref={canvas} role="img" aria-label="Map of snapshot blocks and their state" />
    </div>
  );
});

export function Legend({ dark = true }: { dark?: boolean }) {
  const items = [
    { c: Cell.Cold, label: "not fetched" },
    { c: Cell.Arrived, label: "arrived, not used yet" },
    { c: Cell.Hit, label: "used, already here" },
    { c: Cell.Waited, label: "used, guest waited" },
  ];
  return (
    <div className={`flex flex-wrap gap-x-5 gap-y-1.5 text-[0.78rem] ${dark ? "text-night-dim" : "text-ink-soft"}`}>
      {items.map((i) => (
        <span key={i.c} className="flex items-center gap-1.5">
          <span className="inline-block h-2.5 w-2.5" style={{ background: cellHex[i.c] }} />
          {i.label}
        </span>
      ))}
    </div>
  );
}
