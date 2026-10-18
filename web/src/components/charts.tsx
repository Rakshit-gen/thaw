// Small SVG charts, drawn on the server from the results files.

type Bar = { label: string; value: number; tone?: "thaw" | "base" | "warn"; note?: string };

const tone = { thaw: "var(--sage)", base: "var(--ink-faint)", warn: "var(--rust)" };

// Horizontal bars on a shared linear scale, values in ms.
export function HBars({ bars, max, fmt, width = 640 }: { bars: Bar[]; max?: number; fmt: (v: number) => string; width?: number }) {
  const m = max ?? Math.max(...bars.map((b) => b.value).filter(Number.isFinite), 1);
  const labelW = 150;
  const rowH = 26;
  const plotW = width - labelW - 90;
  return (
    <svg viewBox={`0 0 ${width} ${bars.length * rowH + 6}`} className="w-full max-w-[640px]" role="img" aria-label="Bar chart">
      {bars.map((b, i) => {
        const w = Number.isFinite(b.value) ? Math.max(1, (Math.min(b.value, m) / m) * plotW) : 0;
        const y = i * rowH + 4;
        const over = b.value > m;
        return (
          <g key={b.label + i}>
            <text x={0} y={y + 14} fontSize={12.5} fill="var(--ink-soft)">
              {b.label}
            </text>
            <rect x={labelW} y={y + 3} width={w} height={rowH - 10} fill={tone[b.tone ?? "base"]} opacity={b.tone ? 1 : 0.55} />
            {over && <path d={`M${labelW + w - 6} ${y + 3} l6 ${(rowH - 10) / 2} l-6 ${(rowH - 10) / 2}`} fill="var(--paper)" />}
            <text x={labelW + w + 6} y={y + 14} fontSize={12} fill="var(--ink)" className="mono">
              {Number.isFinite(b.value) ? fmt(b.value) : "-"}
              {b.note ? ` ${b.note}` : ""}
            </text>
          </g>
        );
      })}
    </svg>
  );
}

// One line per series over shared x positions.
export function Lines({
  xs,
  series,
  fmtY,
  xLabel,
  height = 220,
}: {
  xs: string[];
  series: { name: string; values: number[]; color: string }[];
  fmtY: (v: number) => string;
  xLabel: string;
  height?: number;
}) {
  const W = 640;
  const pad = { l: 64, r: 110, t: 12, b: 34 };
  const all = series.flatMap((s) => s.values).filter(Number.isFinite);
  const maxY = Math.max(...all, 1) * 1.08;
  const x = (i: number) => pad.l + (xs.length === 1 ? 0 : (i / (xs.length - 1)) * (W - pad.l - pad.r));
  const y = (v: number) => pad.t + (1 - v / maxY) * (height - pad.t - pad.b);
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => f * maxY);
  return (
    <svg viewBox={`0 0 ${W} ${height}`} className="w-full max-w-[640px]" role="img" aria-label="Line chart">
      {ticks.map((t) => (
        <g key={t}>
          <line x1={pad.l} x2={W - pad.r} y1={y(t)} y2={y(t)} stroke="var(--rule)" />
          <text x={pad.l - 8} y={y(t) + 4} fontSize={11} textAnchor="end" fill="var(--ink-faint)" className="mono">
            {fmtY(t)}
          </text>
        </g>
      ))}
      {xs.map((l, i) => (
        <text key={l} x={x(i)} y={height - pad.b + 18} fontSize={11} textAnchor="middle" fill="var(--ink-faint)" className="mono">
          {l}
        </text>
      ))}
      <text x={(pad.l + W - pad.r) / 2} y={height - 2} fontSize={11} textAnchor="middle" fill="var(--ink-faint)">
        {xLabel}
      </text>
      {series.map((s) => {
        const pts = s.values.map((v, i) => [x(i), y(v)] as const).filter(([, py]) => Number.isFinite(py));
        if (!pts.length) return null;
        const last = pts[pts.length - 1];
        return (
          <g key={s.name}>
            <polyline points={pts.map((p) => p.join(",")).join(" ")} fill="none" stroke={s.color} strokeWidth={2} />
            {pts.map(([px, py], i) => (
              <circle key={i} cx={px} cy={py} r={3} fill={s.color} />
            ))}
            <text x={last[0] + 8} y={last[1] + 4} fontSize={11.5} fill="var(--ink-soft)">
              {s.name}
            </text>
          </g>
        );
      })}
    </svg>
  );
}
