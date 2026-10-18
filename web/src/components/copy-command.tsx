"use client";

import { useState } from "react";

export function CopyCommand({ cmd }: { cmd: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="instrument flex items-center gap-3 px-4 py-3">
      <code className="mono min-w-0 flex-1 overflow-x-auto whitespace-nowrap text-[0.85rem] text-night-ink">
        <span className="select-none text-night-dim">$ </span>
        {cmd}
      </code>
      <button
        type="button"
        onClick={() => {
          navigator.clipboard.writeText(cmd).then(() => {
            setCopied(true);
            setTimeout(() => setCopied(false), 1500);
          });
        }}
        className="mono shrink-0 text-[0.78rem] text-night-dim hover:text-night-ink"
      >
        {copied ? "copied" : "copy"}
      </button>
    </div>
  );
}
