import type { TextareaHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export function Textarea({ className, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return (
    <textarea
      className={cn(
        "flex min-h-24 w-full rounded-md border border-border bg-bone px-3 py-2 text-[14px] text-foreground placeholder:text-ash focus-visible:outline-none",
        className,
      )}
      {...props}
    />
  );
}
