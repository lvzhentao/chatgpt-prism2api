import { cn } from "@/lib/utils";

export function ProgressBar({
  current,
  total,
  className,
  tone = "ink",
}: {
  current: number;
  total: number;
  className?: string;
  tone?: "ink" | "ok";
}) {
  const pct = total > 0 ? Math.min(100, Math.round((current / total) * 100)) : 0;
  return (
    <div className={cn("h-1.5 w-full overflow-hidden rounded-sm bg-linen", className)} role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
      <div
        className={cn("h-full transition-[width] duration-300 ease-out", tone === "ok" ? "bg-verdant" : "bg-ink")}
        style={{ width: `${pct}%` }}
      />
    </div>
  );
}
