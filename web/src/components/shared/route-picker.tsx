import { useEffect, useMemo, useRef, useState } from "react";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

const PASSTHROUGH = "__default__";

export function RoutePicker({
  value,
  options,
  onChange,
}: {
  value: string;
  options: string[];
  onChange: (next: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const root = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  const filtered = useMemo(() => {
    const n = q.trim().toLowerCase();
    const list = n ? options.filter((id) => id.toLowerCase().includes(n)) : options;
    return list.slice(0, 40);
  }, [options, q]);

  const pick = (next: string) => {
    onChange(next === PASSTHROUGH ? "" : next);
    setOpen(false);
    setQ("");
  };

  return (
    <div ref={root} className="relative w-full">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex h-8 w-full items-center justify-between rounded-md border border-border bg-bone px-2.5 text-left font-mono text-[12px] text-ink"
      >
        <span className="truncate">{value || "默认透传"}</span>
        <span className="ml-2 text-ash">▾</span>
      </button>
      {open ? (
        <div className="absolute z-50 mt-1 w-full overflow-hidden rounded-md border border-border bg-bone shadow-[var(--shadow-flyout)]">
          <div className="p-1.5">
            <Input
              autoFocus
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="搜索或输入目标模型"
              className="h-8 font-mono text-[12px]"
            />
          </div>
          <div className="max-h-56 overflow-y-auto p-1">
            <button
              type="button"
              onClick={() => pick(PASSTHROUGH)}
              className={cn("block w-full rounded-sm px-2 py-1.5 text-left text-[12px] hover:bg-linen", !value && "bg-linen")}
            >
              默认透传
            </button>
            {filtered.map((id) => (
              <button
                key={id}
                type="button"
                onClick={() => pick(id)}
                className={cn("block w-full truncate rounded-sm px-2 py-1.5 text-left font-mono text-[12px] hover:bg-linen", value === id && "bg-linen")}
              >
                {id}
              </button>
            ))}
            {q.trim() && !options.includes(q.trim()) ? (
              <button
                type="button"
                onClick={() => pick(q.trim())}
                className="block w-full truncate rounded-sm px-2 py-1.5 text-left font-mono text-[12px] text-ember hover:bg-linen"
              >
                路由到 {q.trim()}
              </button>
            ) : null}
          </div>
        </div>
      ) : null}
    </div>
  );
}
