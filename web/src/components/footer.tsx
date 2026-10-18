import Link from "next/link";
import { repoUrl } from "@/lib/site";

export function Footer() {
  return (
    <footer className="mt-24 border-t border-rule">
      <div className="mx-auto flex max-w-6xl flex-wrap items-baseline gap-x-8 gap-y-2 px-5 py-8 text-[0.86rem] text-ink-faint">
        <span className="mono text-ink-soft">thaw</span>
        <span>MIT licensed. Written in Go.</span>
        <Link href="/docs#limits" className="hover:text-ink">
          What the numbers do not show
        </Link>
        <a href={repoUrl} className="ml-auto hover:text-ink">
          Source
        </a>
      </div>
    </footer>
  );
}
