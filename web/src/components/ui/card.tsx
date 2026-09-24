import type { HTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export function Card({ className, interactive, ...props }: HTMLAttributes<HTMLDivElement> & { interactive?: boolean }) {
  return (
    <div
      className={cn(
        "rounded-md border border-ink/10 bg-bone p-6",
        interactive ? "card-press" : "transition-colors duration-150",
        className,
      )}
      {...props}
    />
  );
}

export function CardTitle({ className, ...props }: HTMLAttributes<HTMLHeadingElement>) {
  return <h3 className={cn("text-[22px] leading-[1.3] tracking-[-0.11px] text-ink", className)} {...props} />;
}

export function CardDesc({ className, ...props }: HTMLAttributes<HTMLParagraphElement>) {
  return <p className={cn("mt-1 text-[13px] text-ash", className)} {...props} />;
}
