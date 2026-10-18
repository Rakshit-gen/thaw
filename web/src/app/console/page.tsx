import type { Metadata } from "next";
import { Console } from "./console";

export const metadata: Metadata = {
  title: "Console",
  description: "Watch a snapshot restore block by block: recorded runs, or a local thaw serve.",
};

export default function ConsolePage() {
  return (
    <div className="mx-auto max-w-6xl px-5 pt-14">
      <h1 className="text-[2rem] font-semibold tracking-[-0.02em]">Console</h1>
      <p className="mt-3 max-w-[70ch] text-[1rem] leading-relaxed text-ink-soft">
        Watch a restore block by block. Recorded runs are replays exported with the site. With <code className="mono text-[0.9em]">thaw serve</code>{" "}
        running on your machine you can replay any snapshot&apos;s traces against any store profile, and on Linux with KVM restore a real VM and watch its
        faults arrive.
      </p>
      <div className="mt-8">
        <Console />
      </div>
    </div>
  );
}
