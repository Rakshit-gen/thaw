"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { BlockGrid, GridHandle, Legend } from "./block-grid";
import { answeredAt, fmtMs, Run, RunState, strategyLabel } from "@/lib/run";

type Props = {
  runs: Run[];
  autoplay?: boolean;
  loop?: boolean;
  compact?: boolean;
  // Live runs grow while playing; the clock follows the newest event.
  live?: boolean;
};

const speeds = [0.25, 0.5, 1, 2];

export function Player({ runs, autoplay = true, loop = false, compact = false, live = false }: Props) {
  const states = useMemo(() => runs.map((r) => new RunState(r)), [runs]);
  const grids = useRef<(GridHandle | null)[]>([]);
  const [t, setT] = useState(0);
  const [playing, setPlaying] = useState(autoplay);
  const [speed, setSpeed] = useState(1);
  const tRef = useRef(0);
  const end = Math.max(...runs.map(answeredAt), 1) + 400;

  const seek = useCallback(
    (to: number) => {
      const back = to < tRef.current;
      tRef.current = to;
      states.forEach((s, i) => {
        if (back) {
          s.reset();
          s.advance(to);
          grids.current[i]?.repaint();
        } else {
          s.advance(to, (f, b, st) => grids.current[i]?.paint(f, b, st));
        }
      });
      setT(to);
    },
    [states],
  );

  // New runs start at the beginning.
  const [shown, setShown] = useState(states);
  if (shown !== states) {
    setShown(states);
    setT(0);
    setPlaying(autoplay);
  }
  useEffect(() => {
    tRef.current = 0;
    grids.current.forEach((g) => g?.repaint());
  }, [states]);

  useEffect(() => {
    if (!playing) return;
    let raf = 0;
    let last = performance.now();
    const tick = (now: number) => {
      const dt = (now - last) * speed;
      last = now;
      const to = tRef.current + dt;
      if (!live && to >= end) {
        seek(end);
        setPlaying(false);
        return;
      }
      seek(to);
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => {
      cancelAnimationFrame(raf);
    };
  }, [playing, speed, end, live, seek]);

  // A looping player rests on the finished picture, then starts over.
  useEffect(() => {
    if (!loop || playing || t < end) return;
    const id = setTimeout(() => {
      seek(0);
      setPlaying(true);
    }, 2500);
    return () => clearTimeout(id);
  }, [loop, playing, t, end, seek]);

  const toggle = () => {
    if (!playing && tRef.current >= end) seek(0);
    setPlaying((p) => !p);
  };

  return (
    <div className="instrument p-4 sm:p-5">
      <div className={`grid gap-6 ${runs.length > 1 ? "md:grid-cols-2" : ""}`}>
        {runs.map((r, i) => {
          const s = states[i];
          const ans = answeredAt(r);
          const answered = t >= ans;
          return (
            <div key={`${r.snapshot}-${r.strategy}-${i}`} className="min-w-0">
              <div className="flex items-baseline justify-between gap-3 border-b border-night-rule pb-2">
                <div className="text-[0.95rem] font-medium">{strategyLabel[r.strategy] ?? r.strategy}</div>
                <div className={`mono text-[0.95rem] ${answered ? "text-night-ink" : "text-night-dim"}`}>
                  {answered ? `answered at ${fmtMs(ans)}` : fmtMs(Math.min(t, ans))}
                </div>
              </div>
              <Readout state={s} t={t} />
              <BlockGrid
                ref={(g) => {
                  grids.current[i] = g;
                }}
                state={s}
                className={`mt-3 ${compact ? "max-h-[360px] overflow-hidden" : ""}`}
              />
            </div>
          );
        })}
      </div>
      <div className="mt-4 flex flex-wrap items-center gap-4 border-t border-night-rule pt-4">
        <button type="button" onClick={toggle} className="mono w-20 rounded-[3px] border border-night-rule px-3 py-1.5 text-[0.82rem] hover:bg-night-3">
          {playing ? "pause" : t >= end ? "replay" : "play"}
        </button>
        <input
          type="range"
          min={0}
          max={end}
          step={1}
          value={Math.min(t, end)}
          onChange={(e) => {
            setPlaying(false);
            seek(Number(e.target.value));
          }}
          aria-label="Time since the restore began"
          className="min-w-40 flex-1 accent-[var(--amber)]"
        />
        <div className="mono flex gap-1 text-[0.78rem]">
          {speeds.map((v) => (
            <button
              key={v}
              type="button"
              onClick={() => setSpeed(v)}
              className={`rounded-[3px] px-2 py-1 ${speed === v ? "bg-night-3 text-night-ink" : "text-night-dim hover:text-night-ink"}`}
            >
              {v}x
            </button>
          ))}
        </div>
      </div>
      <div className="mt-3">
        <Legend />
      </div>
    </div>
  );
}

function Readout({ state, t }: { state: RunState; t: number }) {
  void t; // re-render with the clock
  const mib = (n: number) => `${((n * 65536) / 2 ** 20).toFixed(0)} MiB`;
  return (
    <dl className="mono mt-2 grid grid-cols-3 gap-2 text-[0.78rem]">
      <div>
        <dt className="text-night-dim">guest waited on</dt>
        <dd className="text-rust">{state.waited.toLocaleString("en-US")} blocks</dd>
      </div>
      <div>
        <dt className="text-night-dim">already there</dt>
        <dd className="text-sage">{state.hit.toLocaleString("en-US")} blocks</dd>
      </div>
      <div>
        <dt className="text-night-dim">fetched, raw</dt>
        <dd className="text-amber">{mib(state.arrived)}</dd>
      </div>
    </dl>
  );
}
