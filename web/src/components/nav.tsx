import Link from "next/link";
import { repoUrl } from "@/lib/site";

const links = [
  { href: "/console", label: "Console" },
  { href: "/benchmarks", label: "Benchmarks" },
  { href: "/docs", label: "Docs" },
];

export function Nav() {
  return (
    <header className="border-b border-rule">
      <nav className="mx-auto flex h-14 max-w-6xl items-center gap-8 px-5">
        <Link href="/" className="mono flex items-center gap-2 text-[0.98rem] font-medium text-ink">
          <Mark />
          thaw
        </Link>
        <div className="flex gap-6 text-[0.92rem] text-ink-soft">
          {links.map((l) => (
            <Link key={l.href} href={l.href} className="hover:text-ink">
              {l.label}
            </Link>
          ))}
        </div>
        <a href={repoUrl} className="ml-auto text-[0.92rem] text-ink-soft hover:text-ink">
          GitHub
        </a>
      </nav>
    </header>
  );
}

// Four blocks thawing left to right: frost, amber, sage, sage.
export function Mark({ size = 14 }: { size?: number }) {
  const c = ["var(--frost)", "var(--amber)", "var(--sage)", "var(--sage)"];
  const s = size / 2 - 0.5;
  return (
    <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} aria-hidden>
      {c.map((fill, i) => (
        <rect key={i} x={(i % 2) * (s + 1)} y={Math.floor(i / 2) * (s + 1)} width={s} height={s} fill={fill} />
      ))}
    </svg>
  );
}
