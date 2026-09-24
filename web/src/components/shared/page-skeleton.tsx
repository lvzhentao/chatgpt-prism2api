import type { CSSProperties, ReactNode } from "react";
import { cn } from "@/lib/utils";

export function PageSkeleton({ kind = "page" }: { kind?: "page" | "cards" | "table" | "models" }) {
  if (kind === "cards") {
    return (
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2 lg:grid-cols-3">
        {Array.from({ length: 6 }).map((_, i) => (
            <div key={i} className="stagger-in rounded-md border border-border bg-bone p-3" style={{ "--stagger": i } as CSSProperties}>
            <div className="flex gap-2">
              <SkeletonBar className="size-8 shrink-0" />
              <div className="min-w-0 flex-1 space-y-2">
                <SkeletonBar className="h-4 w-20" />
                <SkeletonBar className="h-4 w-3/4" />
              </div>
            </div>
            <SkeletonBar className="mt-4 h-2 w-full" />
            <SkeletonBar className="mt-3 h-16 w-full" />
            <SkeletonBar className="mt-3 h-8 w-full" />
          </div>
        ))}
      </div>
    );
  }
  if (kind === "table") {
    return (
      <div className="page-enter overflow-hidden rounded-md border border-border bg-bone">
        {Array.from({ length: 8 }).map((_, i) => (
          <div key={i} className="flex gap-3 border-b border-border/70 px-4 py-3 last:border-0">
            <SkeletonBar className="h-4 w-1/5" />
            <SkeletonBar className="h-4 w-1/4" />
            <SkeletonBar className="h-4 flex-1" />
            <SkeletonBar className="h-4 w-16" />
          </div>
        ))}
      </div>
    );
  }
  if (kind === "models") {
    return (
      <div className="page-enter space-y-4">
        <div className="flex gap-2 overflow-hidden">
          {Array.from({ length: 6 }).map((_, i) => (
            <SkeletonBar key={i} className="h-9 w-28 shrink-0" />
          ))}
        </div>
        <div className="space-y-2">
          {Array.from({ length: 8 }).map((_, i) => (
            <SkeletonBar key={i} className="h-14 w-full" />
          ))}
        </div>
      </div>
    );
  }
  return (
    <div className="page-enter space-y-4">
      <SkeletonBar className="h-8 w-40" />
      <SkeletonBar className="h-4 w-80" />
      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <SkeletonBar key={i} className="h-28" />
        ))}
      </div>
      <SkeletonBar className="h-64 w-full" />
    </div>
  );
}

export function SkeletonBar({ className }: { className?: string }) {
  return <div className={cn("skeleton-shimmer rounded-md", className)} />;
}

export function Stagger({
  i,
  className,
  children,
}: {
  i: number;
  className?: string;
  children: ReactNode;
}) {
  return (
    <div className={cn("stagger-in", className)} style={{ "--stagger": i } as CSSProperties}>
      {children}
    </div>
  );
}
