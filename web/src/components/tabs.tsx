"use client";

import { ReactNode, useState } from "react";

// Tabs over panels rendered on the server.
export function Tabs({ labels, children, label }: { labels: string[]; children: ReactNode[]; label: string }) {
  const [i, setI] = useState(0);
  return (
    <div>
      <div role="tablist" aria-label={label} className="flex flex-wrap gap-x-6 border-b border-rule">
        {labels.map((l, j) => (
          <button
            key={l}
            type="button"
            role="tab"
            aria-selected={i === j}
            onClick={() => setI(j)}
            className={`-mb-px border-b-2 py-2.5 text-[0.92rem] ${i === j ? "border-rust text-ink" : "border-transparent text-ink-faint hover:text-ink-soft"}`}
          >
            {l}
          </button>
        ))}
      </div>
      <div className="mt-6">{children[i]}</div>
    </div>
  );
}
