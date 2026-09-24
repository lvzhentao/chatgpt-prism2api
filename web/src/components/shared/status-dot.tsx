import { cn } from "@/lib/utils";

export function StatusDot({ tone }: { tone: "ok" | "warn" | "err" | "muted" }) {
  return (
    <span
      className={cn("inline-block size-1.5 rounded-full", {
        "bg-verdant": tone === "ok",
        "bg-amber": tone === "warn",
        "bg-crimson": tone === "err",
        "bg-mist": tone === "muted",
      })}
    />
  );
}
