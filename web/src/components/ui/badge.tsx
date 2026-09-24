import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

export function Badge({
  className,
  tone = "muted",
  children,
}: {
  className?: string;
  tone?: "muted" | "ok" | "warn" | "err" | "ember";
  children: ReactNode;
}) {
  const tones = {
    muted: "text-ash",
    ok: "text-verdant",
    warn: "text-amber",
    err: "text-crimson",
    ember: "text-ember",
  };
  return (
    <span className={cn("font-mono text-[12px] leading-none", tones[tone], className)}>{children}</span>
  );
}
