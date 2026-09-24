import type { InputHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className={cn(
        "flex h-9 w-full rounded-md border border-border bg-bone px-3 py-2 text-[14px] text-foreground placeholder:text-ash focus-visible:outline-none disabled:opacity-50",
        className,
      )}
      {...props}
    />
  );
}
