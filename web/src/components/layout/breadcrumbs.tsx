import { ChevronRight } from "lucide-react";
import { Link, useMatches } from "react-router-dom";

export type Crumb = { label: string; to?: string };

type Handle = {
  crumb?: Crumb | ((p: Record<string, string | undefined>) => Crumb);
  crumbs?: Crumb[];
};

export function Breadcrumbs() {
  const matches = useMatches();
  const crumbs: Crumb[] = [];
  for (const m of matches) {
    const handle = m.handle as Handle | undefined;
    if (!handle) continue;
    if (handle.crumbs) {
      crumbs.push(...handle.crumbs);
    } else if (handle.crumb) {
      crumbs.push(typeof handle.crumb === "function" ? handle.crumb(m.params) : handle.crumb);
    }
  }
  const seen = new Set<string>();
  const unique = crumbs.filter((c) => {
    const key = `${c.to ?? ""}:${c.label}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
  if (unique.length === 0) return null;
  return (
    <nav aria-label="面包屑" className="mb-4 flex flex-wrap items-center gap-1 text-[13px] text-ash">
      {unique.map((c, i) => (
        <span key={`${c.label}-${i}`} className="flex items-center gap-1">
          {i > 0 ? <ChevronRight className="size-3.5" /> : null}
          {c.to && i < unique.length - 1 ? (
            <Link to={c.to} className="text-ink/70 transition-colors duration-150 hover:text-ember hover:underline">
              {c.label}
            </Link>
          ) : (
            <span className="text-ink">{c.label}</span>
          )}
        </span>
      ))}
    </nav>
  );
}
