import { useState } from "react";
import { lobeIconFallback, lobeIconSrc, vendorOf } from "@/lib/model-meta";
import { cn } from "@/lib/utils";

export function ModelLogo({ id, className }: { id: string; className?: string }) {
  const v = vendorOf(id);
  const [step, setStep] = useState(0);
  const srcs = [lobeIconSrc(v.icon), lobeIconSrc(v.icon, false), lobeIconFallback(v.icon), lobeIconFallback(v.icon, false)];
  if (step >= srcs.length) {
    return (
      <span className={cn("grid size-7 place-items-center rounded-md bg-linen font-mono text-[10px] text-ash", className)}>
        {v.label.slice(0, 1)}
      </span>
    );
  }
  return (
    <img
      src={srcs[step]}
      alt={v.label}
      loading="lazy"
      decoding="async"
      className={cn("size-7 rounded-md bg-bone object-contain p-0.5", className)}
      onError={() => setStep((s) => s + 1)}
    />
  );
}
